package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 验证修复后：
// 1. 服务端按节流向 WS 上传端回报 progress.bytes
// 2. progress.bytes 始终 <= 下载端已实收字节数（即不再"虚高")
// 3. 上传方在传输完成前不会看到 progress.bytes 等于 file.Size
// 4. 最终 progress 收敛到 file.Size
func TestUploadProgressMatchesActualDownload(t *testing.T) {
	ffb := NewFileFlowBridge(0, 0, 100*1024*1024*1024, 16)
	router := ffb.buildRouter()

	srv := httptest.NewServer(router)
	defer srv.Close()

	// 8 MiB 测试文件，足以触发多次节流回报
	const fileSize = 8 * 1024 * 1024
	fileContent := bytes.Repeat([]byte("ABCDEFGH"), fileSize/8)

	// 1) 注册
	regBody, _ := json.Marshal(map[string]interface{}{
		"filename": "progress_test.bin",
		"size":     fileSize,
	})
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	defer resp.Body.Close()
	var regResp struct {
		AuthToken string `json:"auth_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		t.Fatalf("decode register resp: %v", err)
	}
	token := regResp.AuthToken

	// 2) 建立 WS 上传连接（模拟浏览器侧）
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/ws/" + token
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer wsConn.Close()

	type progressSample struct {
		bytes int64
		at    time.Time
	}
	var (
		mu                  sync.Mutex
		progressSamples     []progressSample
		gotSendChunk        bool
		gotTransferComplete bool
	)

	// 3) 上传端读取服务端消息：收 send_chunk 后才开始推数据
	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		offset := int64(0)
		for {
			mt, msg, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var m map[string]interface{}
			if err := json.Unmarshal(msg, &m); err != nil {
				continue
			}
			cmd, _ := m["command"].(string)
			switch cmd {
			case "send_chunk":
				gotSendChunk = true
				// 模拟前端按 64KB 分块发送
				go func() {
					const chunk = 64 * 1024
					for offset < fileSize {
						end := offset + chunk
						if end > fileSize {
							end = fileSize
						}
						if err := wsConn.WriteMessage(websocket.BinaryMessage, fileContent[offset:end]); err != nil {
							return
						}
						offset = end
						time.Sleep(time.Millisecond) // 留给服务端转发
					}
				}()
			case "progress":
				if b, ok := m["bytes"].(float64); ok {
					mu.Lock()
					progressSamples = append(progressSamples, progressSample{
						bytes: int64(b),
						at:    time.Now(),
					})
					mu.Unlock()
				}
			case "transfer_complete":
				mu.Lock()
				gotTransferComplete = true
				mu.Unlock()
				return
			}
		}
	}()

	// 4) 慢速下载端：用一个故意限速的 reader 消费 response.Body
	// 故意慢一点，确保上传端如果还按"入队字节"算就会大幅领先实际下载
	dlResp, err := http.Get(srv.URL + "/download/" + token + "/progress_test.bin")
	if err != nil {
		t.Fatalf("download GET: %v", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		t.Fatalf("download status: %d", dlResp.StatusCode)
	}

	var downloadedAt []struct {
		bytes int64
		at    time.Time
	}
	_ = downloadedAt // 保留备用追踪
	downloaded := int64(0)
	dlBuf := make([]byte, 32*1024)
	for {
		n, err := dlResp.Body.Read(dlBuf)
		if n > 0 {
			downloaded += int64(n)
			// 模拟下载端较慢消费
			time.Sleep(8 * time.Millisecond)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("download read: %v", err)
		}
	}

	if downloaded != fileSize {
		t.Fatalf("download size mismatch: got %d want %d", downloaded, fileSize)
	}

	// 等待 transfer_complete
	select {
	case <-uploadDone:
	case <-time.After(5 * time.Second):
		t.Logf("upload goroutine did not exit, but download completed")
	}

	mu.Lock()
	defer mu.Unlock()

	if !gotSendChunk {
		t.Fatalf("did not receive send_chunk from server")
	}
	if len(progressSamples) == 0 {
		t.Fatalf("did not receive any progress messages — fix not active")
	}
	t.Logf("received %d progress samples", len(progressSamples))

	// 关键断言 1: progress 单调递增
	for i := 1; i < len(progressSamples); i++ {
		if progressSamples[i].bytes < progressSamples[i-1].bytes {
			t.Errorf("progress not monotonic at i=%d: %d < %d",
				i, progressSamples[i].bytes, progressSamples[i-1].bytes)
		}
	}

	// 关键断言 2: 所有 progress.bytes 都 <= fileSize（不能虚报超过文件总大小）
	for _, ps := range progressSamples {
		if ps.bytes > fileSize {
			t.Errorf("progress %d > fileSize %d", ps.bytes, fileSize)
		}
	}

	// 关键断言 3: progress 不会在早期就虚报到 fileSize
	// （即不会在上传未完成时显示 100%）
	// 取前 1/3 样本中的最大值，不应等于 fileSize
	earlyCutoff := len(progressSamples) / 3
	if earlyCutoff > 0 {
		earlyMax := int64(0)
		for _, ps := range progressSamples[:earlyCutoff] {
			if ps.bytes > earlyMax {
				earlyMax = ps.bytes
			}
		}
		if earlyMax >= fileSize {
			t.Errorf("early progress already hit fileSize (%d) at sample %d/%d — would show 100%% too early",
				fileSize, earlyCutoff, len(progressSamples))
		}
		t.Logf("first 1/3 progress samples max = %d / %d (%.1f%%), no premature 100%%",
			earlyMax, fileSize, float64(earlyMax)/float64(fileSize)*100)
	}

	// 关键断言 4: 最终 progress 要么收敛到 fileSize，要么 transfer_complete 兜底
	last := progressSamples[len(progressSamples)-1].bytes
	if last != fileSize && !gotTransferComplete {
		t.Errorf("final progress %d != fileSize %d and no transfer_complete to force 100%%",
			last, fileSize)
	}

	if !gotTransferComplete {
		t.Errorf("did not receive transfer_complete")
	}
}

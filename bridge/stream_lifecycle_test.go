package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ==================== WebSocket 上传端模拟 ====================

// wsTestProvider 模拟浏览器上传端（static/index.html）的行为：
//   - 连上 /ws/{token} 后等待命令
//   - 收到 send_chunk 就从头把文件推一遍
//   - 收到 stop_upload 只作废当前发送循环，保持连接等待重试（不关闭连接）
type wsTestProvider struct {
	conn *websocket.Conn
	data []byte

	mu  sync.Mutex
	gen int
}

func startTestWSProvider(t *testing.T, baseURL, token string, data []byte) *wsTestProvider {
	t.Helper()

	wsURL := strings.Replace(baseURL, "http", "ws", 1) + "/ws/" + token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 连接失败: %v", err)
	}

	p := &wsTestProvider{conn: conn, data: data}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读取 READY 失败: %v", err)
	}
	var ready map[string]interface{}
	if err := json.Unmarshal(msg, &ready); err != nil || ready["command"] != "READY" {
		t.Fatalf("期望 READY，实际: %s", string(msg))
	}

	go p.loop()
	return p
}

func (p *wsTestProvider) close() { _ = p.conn.Close() }

func (p *wsTestProvider) loop() {
	for {
		_, msg, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]interface{}
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch m["command"] {
		case "send_chunk":
			p.mu.Lock()
			p.gen++
			gen := p.gen
			p.mu.Unlock()
			go p.send(gen)
		case "stop_upload":
			p.mu.Lock()
			p.gen++
			p.mu.Unlock()
		}
	}
}

// send 按代次推流；代次失配立即退出，模拟浏览器端的行为
func (p *wsTestProvider) send(gen int) {
	const chunk = 64 * 1024
	for off := 0; off < len(p.data); off += chunk {
		p.mu.Lock()
		cur := p.gen
		p.mu.Unlock()
		if cur != gen {
			return
		}
		end := off + chunk
		if end > len(p.data) {
			end = len(p.data)
		}
		if err := p.conn.WriteMessage(websocket.BinaryMessage, p.data[off:end]); err != nil {
			return
		}
	}
}

// registerTestFile 直接写入注册表，避免依赖 HTTP 注册流程
func registerTestFile(ffb *FileFlowBridge, token string, size int64) {
	ffb.mu.Lock()
	ffb.fileRegistry[token] = &FileMetadata{
		Filename:         token + ".bin",
		OriginalFilename: token + ".bin",
		Size:             size,
		Status:           "registered",
		AuthToken:        token,
		RegisteredAt:     time.Now(),
		ExpiresAt:        time.Now().Add(time.Hour),
	}
	ffb.getOrCreateDoneChLocked(token)
	ffb.mu.Unlock()
}

// waitForTokenStatus 轮询 /status，直到 status 变成 want 或超时
func waitForTokenStatus(t *testing.T, baseURL, token, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/status/" + token)
		if err == nil {
			var body struct {
				Status string `json:"status"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil {
				last = body.Status
				if body.Status == want {
					resp.Body.Close()
					return
				}
			}
			resp.Body.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 token 状态 %q 超时，最后状态 %q", want, last)
}

// ==================== 中断后可重试（不得废链，也不得拼出错位数据） ====================

func TestWebSocketDownloadInterruptKeepsTokenAndRetries(t *testing.T) {
	ffb := NewFileFlowBridge(0, 0, 1<<30, 8)
	srv := httptest.NewServer(ffb.buildRouter())
	defer srv.Close()

	payload := bytes.Repeat([]byte("0123456789abcdef"), 256*1024) // 4 MiB
	const token = "wsretryt"
	registerTestFile(ffb, token, int64(len(payload)))

	provider := startTestWSProvider(t, srv.URL, token, payload)
	defer provider.close()

	// WS provider 接入应计入上传指标（旧实现里流式上传完全不计数）
	if got := ffb.metrics.uploadTotal.Load(); got != 1 {
		t.Fatalf("uploads_total = %d, want 1", got)
	}

	request := func() *http.Response {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/download/"+token, nil)
		if err != nil {
			t.Fatal(err)
		}
		// 非浏览器客户端才会走流式转发分支
		req.Header.Set("User-Agent", "curl/8.18.0")
		req.Header.Set("Accept", "*/*")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// 1) 首次下载读一小段后主动断开
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/download/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "curl/8.18.0")
	req.Header.Set("Accept", "*/*")
	first, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.StatusCode != http.StatusOK {
		t.Fatalf("首次下载状态码 = %d, want 200", first.StatusCode)
	}
	if _, err := io.ReadFull(first.Body, make([]byte, 256*1024)); err != nil {
		t.Fatalf("读取前 256KiB 失败: %v", err)
	}
	cancel()
	first.Body.Close()

	// 2) 中断后 token 必须还在，且状态从 downloading 回滚到 streaming
	waitForTokenStatus(t, srv.URL, token, "streaming", 5*time.Second)

	if got := ffb.metrics.bytesUploaded.Load(); got <= 0 {
		t.Fatalf("bytes_uploaded = %d, 应大于 0", got)
	}

	// 3) 重试必须从 0 重新下发，且不能混入上次残留的字节
	retry := request()
	defer retry.Body.Close()
	got, err := io.ReadAll(retry.Body)
	if err != nil {
		t.Fatalf("重试读取失败: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("重试内容不一致: len=%d (want %d), 首 16 字节=%q (want %q)",
			len(got), len(payload), string(got[:min(16, len(got))]), string(payload[:16]))
	}
}

// ==================== 上传槽必须覆盖「等下载端」阶段 ====================

func TestUploadSlotHeldWhileProviderWaitsForDownloader(t *testing.T) {
	ffb := NewFileFlowBridge(0, 0, 1<<30, 8)
	ffb.MaxParallelUploads = 1
	ffb.initUploadSem()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go ffb.handleStreamConnection(conn)
		}
	}()

	registerTestFile(ffb, "slotA", 100)
	registerTestFile(ffb, "slotB", 100)

	handshake := func(token string) string {
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatalf("TCP 连接失败: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		meta, _ := json.Marshal(map[string]string{"auth_token": token})
		if _, err := conn.Write(append(meta, '\n')); err != nil {
			t.Fatalf("发送元数据失败: %v", err)
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatalf("读取握手响应失败: %v", err)
		}
		return strings.TrimSpace(line)
	}

	// provider A 握手后停在「等下载端」，应持续占用唯一的上传槽
	connA, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	_ = connA.SetDeadline(time.Now().Add(5 * time.Second))
	metaA, _ := json.Marshal(map[string]string{"auth_token": "slotA"})
	if _, err := connA.Write(append(metaA, '\n')); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(connA).ReadString('\n'); err != nil || strings.TrimSpace(line) != "STREAM_READY" {
		t.Fatalf("provider A 握手失败: line=%q err=%v", strings.TrimSpace(line), err)
	}

	if got := handshake("slotB"); got != "TOO_MANY_UPLOADS" {
		t.Fatalf("provider B 握手响应 = %q, want TOO_MANY_UPLOADS", got)
	}
}

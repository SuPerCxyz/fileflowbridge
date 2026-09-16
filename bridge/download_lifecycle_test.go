package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestResumableProbeDoesNotConsumeToken 验证 resumable 下载的 HEAD / Range 探测
// 不会销毁暂存文件（旧实现会让链接永久失效），只有完整下发才回收资源。
func TestResumableProbeDoesNotConsumeToken(t *testing.T) {
	ffb := NewFileFlowBridge(0, 0, 1<<30, 8)
	ffb.TempDir = t.TempDir()
	srv := httptest.NewServer(ffb.buildRouter())
	defer srv.Close()

	path, err := ffb.allocResumableTempFile("rsmtok", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}

	ffb.mu.Lock()
	meta := &FileMetadata{
		Filename: "r.bin", OriginalFilename: "r.bin", Size: 1000,
		Status: "registered", AuthToken: "rsmtok",
		RegisteredAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		Resumable: true, ChunkSize: 64 * 1024, TempPath: path,
		UploadReadyAt: time.Now(),
	}
	meta.InitChunkBitmap(1)
	meta.uploadReady.Store(true)
	ffb.fileRegistry["rsmtok"] = meta
	ffb.mu.Unlock()

	exists := func() bool { _, err := os.Stat(path); return err == nil }

	send := func(method, rangeHdr string) int {
		req, _ := http.NewRequest(method, srv.URL+"/download/rsmtok/r.bin", nil)
		req.Header.Set("User-Agent", "curl/8.18.0")
		req.Header.Set("Accept", "*/*")
		if rangeHdr != "" {
			req.Header.Set("Range", rangeHdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// 读干 body，确保服务端 handler 已经写完并执行完 defer 清理
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	if code := send(http.MethodHead, ""); code != http.StatusOK {
		t.Fatalf("HEAD: got %d want 200", code)
	}
	if !exists() {
		t.Fatal("HEAD 之后暂存文件被删除了")
	}
	if code := send(http.MethodGet, "bytes=0-99"); code != http.StatusPartialContent {
		t.Fatalf("Range: got %d want 206", code)
	}
	if !exists() {
		t.Fatal("Range 之后暂存文件被删除了")
	}
	if code := send(http.MethodGet, ""); code != http.StatusOK {
		t.Fatalf("完整下载: got %d want 200", code)
	}
	// 客户端读完 body 与服务端 handler 返回之间存在微小时间差，这里做有限等待
	deadline := time.Now().Add(2 * time.Second)
	for exists() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if exists() {
		t.Fatal("完整下载后暂存文件应被回收")
	}
}

// TestExpiredTokenRejectedByTransferEndpoints 过期 token 应在下载 / 续传入口立即被拒（410），
// 不等 cleanupExpiredFiles 的 5 分钟清理周期兜底。
func TestExpiredTokenRejectedByTransferEndpoints(t *testing.T) {
	ffb := NewFileFlowBridge(0, 0, 1<<30, 8)
	srv := httptest.NewServer(ffb.buildRouter())
	defer srv.Close()

	ffb.mu.Lock()
	ffb.fileRegistry["exptok"] = &FileMetadata{
		Filename:         "x.bin",
		OriginalFilename: "x.bin",
		Size:             100,
		Status:           "registered",
		AuthToken:        "exptok",
		RegisteredAt:     time.Now().Add(-3 * time.Hour),
		ExpiresAt:        time.Now().Add(-time.Hour),
		Resumable:        true,
		ChunkSize:        64 * 1024,
	}
	ffb.mu.Unlock()

	do := func(method, url string, body []byte) int {
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", "curl/8.18.0")
		req.Header.Set("Accept", "*/*")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := do(http.MethodGet, srv.URL+"/download/exptok", nil); code != http.StatusGone {
		t.Errorf("过期下载: got %d want 410", code)
	}
	if code := do(http.MethodPut, srv.URL+"/upload/exptok/chunk?index=0", make([]byte, 64*1024)); code != http.StatusGone {
		t.Errorf("过期续传: got %d want 410", code)
	}
	if code := do(http.MethodGet, srv.URL+"/upload/exptok/status", nil); code != http.StatusGone {
		t.Errorf("过期状态查询: got %d want 410", code)
	}
}

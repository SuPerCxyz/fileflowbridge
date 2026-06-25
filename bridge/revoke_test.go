package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestRevokeStreamMode: 注册 stream token → DELETE → fileRegistry 立即清空，
// .part 不存在（stream 模式本来就没 .part）。
func TestRevokeStreamMode(t *testing.T) {
	ffb, h := newResumableTestBridge(t, 10)

	// 用 stream 模式注册（resumable=false）
	body, _ := json.Marshal(map[string]any{
		"filename": "a.bin",
		"size":     1024,
	})
	req := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var reg map[string]any
	_ = json.NewDecoder(w.Body).Decode(&reg)
	token := reg["auth_token"].(string)

	// 撤销
	req = httptest.NewRequest("DELETE", "/register/"+token, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}

	// 注册表应该已经清掉
	ffb.mu.RLock()
	_, exists := ffb.fileRegistry[token]
	ffb.mu.RUnlock()
	if exists {
		t.Fatalf("fileRegistry should be empty after revoke")
	}

	// 重复撤销 → 404 不存在 / 或 204
	req = httptest.NewRequest("DELETE", "/register/"+token, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound && w.Code != http.StatusNoContent {
		t.Fatalf("repeat revoke: %d", w.Code)
	}
}

// TestRevokeResumableMode: 注册 resumable → 上传 1 个 chunk → DELETE → .part 被删除
func TestRevokeResumableMode(t *testing.T) {
	ffb, h := newResumableTestBridge(t, 10)

	const size = 128 * 1024
	const chunkSize = 64 * 1024
	reg := registerResumable(t, h, size, chunkSize)
	token := reg["auth_token"].(string)

	// 上传 chunk 0
	wResp := putChunk(h, token, 0, bytes.Repeat([]byte{0xAA}, chunkSize), chunkSize)
	if wResp.Code != 200 {
		t.Fatalf("chunk 0: %d", wResp.Code)
	}

	ffb.mu.RLock()
	meta := ffb.fileRegistry[token]
	ffb.mu.RUnlock()
	if meta == nil || meta.TempPath == "" {
		t.Fatalf("expected meta + tempPath after chunk upload")
	}
	tempPath := meta.TempPath
	if _, err := os.Stat(tempPath); err != nil {
		t.Fatalf(".part should exist before revoke: %v", err)
	}

	// 撤销
	req := httptest.NewRequest("DELETE", "/register/"+token, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("revoke status=%d body=%s", w.Code, w.Body.String())
	}

	// .part 必须被删
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf(".part should be removed after revoke, got err=%v", err)
	}

	// fileRegistry 必须清空
	ffb.mu.RLock()
	_, exists := ffb.fileRegistry[token]
	ffb.mu.RUnlock()
	if exists {
		t.Fatalf("fileRegistry should be empty after revoke")
	}
}

// TestRevokeRequiresAPIKey: 启用 API Key 后未带头 → 401
func TestRevokeRequiresAPIKey(t *testing.T) {
	ffb, h := newResumableTestBridge(t, 10)
	ffb.APIKey = "topsecret"
	// 注册（要带 header）
	body, _ := json.Marshal(map[string]any{"filename": "x", "size": 100})
	req := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "topsecret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("register: %d", w.Code)
	}
	var reg map[string]any
	_ = json.NewDecoder(w.Body).Decode(&reg)
	token := reg["auth_token"].(string)

	// 不带 key → 401
	req = httptest.NewRequest("DELETE", "/register/"+token, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// 带正确 key → 200
	req = httptest.NewRequest("DELETE", "/register/"+token, nil)
	req.Header.Set("X-API-Key", "topsecret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("with key: %d", w.Code)
	}
}

// TestTCPProviderDisconnectCleansUp: TCP provider 握手成功后立即断开，
// 不到 TTL 也不需要等下载端，bridge 必须在 health 间隔内主动清理。
func TestTCPProviderDisconnectCleansUp(t *testing.T) {
	ffb, h := newResumableTestBridge(t, 10)

	// 启 TCP listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go ffb.handleStreamConnection(c)
		}
	}()
	tcpAddr := ln.Addr().(*net.TCPAddr)
	ffb.TCPPort = tcpAddr.Port

	// 注册（stream 模式）
	body, _ := json.Marshal(map[string]any{
		"filename": "b.bin",
		"size":     1024,
	})
	req := httptest.NewRequest("POST", "/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("register: %d", w.Code)
	}
	var reg map[string]any
	_ = json.NewDecoder(w.Body).Decode(&reg)
	token := reg["auth_token"].(string)

	// provider 握手
	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	meta := fmt.Sprintf(`{"auth_token":"%s","filename":"b.bin"}`+"\n", token)
	if _, err := conn.Write([]byte(meta)); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil || line != "STREAM_READY\n" {
		t.Fatalf("expected STREAM_READY, got %q err=%v", line, err)
	}

	// 等 bridge 把 activeStreams 写入（异步）
	time.Sleep(100 * time.Millisecond)
	ffb.mu.RLock()
	_, hasStream := ffb.activeStreams[token]
	ffb.mu.RUnlock()
	if !hasStream {
		t.Fatalf("expected activeStream after handshake")
	}

	// provider 立刻断开（模拟 Ctrl+C 后 conn close）
	_ = conn.Close()

	// 等 health 探测发现 EOF + 清理；
	// connectionHealthInterval = 2s + peek timeout 200ms
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		ffb.mu.RLock()
		_, stillRegistered := ffb.fileRegistry[token]
		_, stillActive := ffb.activeStreams[token]
		ffb.mu.RUnlock()
		if !stillRegistered && !stillActive {
			return // ✅ 清掉了
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("expected cleanup after TCP provider disconnect, still has registry/stream")
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// newResumableTestBridge 构造一个独立的 bridge 实例 + temp dir，
// 便于 resumable 系列测试隔离运行。
func newResumableTestBridge(t *testing.T, maxParallel int) (*FileFlowBridge, http.Handler) {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "ffb-resumable-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tempDir) })

	ffb := NewFileFlowBridge(0, 0, int64(1)<<30, 8) // 1 GiB 上限
	ffb.MaxParallelUploads = maxParallel
	ffb.TempDir = tempDir
	ffb.initUploadSem()
	return ffb, ffb.buildRouter()
}

// registerResumable 构造一个 resumable 注册请求。
func registerResumable(t *testing.T, h http.Handler, size, chunkSize int64) map[string]any {
	t.Helper()
	body := map[string]any{
		"filename":   "x.bin",
		"size":       size,
		"resumable":  true,
		"chunk_size": chunkSize,
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/register", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("register status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode register resp: %v", err)
	}
	if out["resumable"] != true {
		t.Fatalf("expected resumable=true, got %#v", out)
	}
	return out
}

// putChunk 用指定 index / body 发起 PUT chunk 请求。
func putChunk(h http.Handler, token string, index int, body []byte, contentLength int64) *httptest.ResponseRecorder {
	url := fmt.Sprintf("/upload/%s/chunk?index=%d", token, index)
	req := httptest.NewRequest("PUT", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = contentLength
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// statusJSON 解析 /status 响应。
func statusJSON(t *testing.T, h http.Handler, token string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/upload/"+token+"/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return out
}

// TestResumableHappyPath 上传 3 个 chunk → status upload_ready=true → 全文下载校验内容。
func TestResumableHappyPath(t *testing.T) {
	_, h := newResumableTestBridge(t, 10)

	const size = 192 * 1024
	const chunkSize = 64 * 1024
	reg := registerResumable(t, h, size, chunkSize)
	token := reg["auth_token"].(string)
	totalChunks := int(reg["total_chunks"].(float64))
	if totalChunks != 3 {
		t.Fatalf("expected 3 chunks, got %d", totalChunks)
	}

	payload := bytes.Repeat([]byte{0xAB}, size)
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > size {
			end = size
		}
		w := putChunk(h, token, i, payload[start:end], int64(end-start))
		if w.Code != 200 {
			t.Fatalf("chunk %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}

	status := statusJSON(t, h, token)
	if status["upload_ready"] != true {
		t.Fatalf("upload_ready should be true: %#v", status)
	}
	if cnt, _ := status["missing_count"].(float64); cnt != 0 {
		t.Fatalf("missing_count should be 0, got %v", cnt)
	}

	req := httptest.NewRequest("GET", "/download/"+token, nil)
	req.Header.Set("User-Agent", "curl/8.0")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("download status=%d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatalf("downloaded body mismatch")
	}
}

// TestResumableRange resumable 上传完成后下载端应支持 Range 请求。
func TestResumableRange(t *testing.T) {
	_, h := newResumableTestBridge(t, 10)

	const size = 256 * 1024
	const chunkSize = 64 * 1024
	reg := registerResumable(t, h, size, chunkSize)
	token := reg["auth_token"].(string)
	totalChunks := int(reg["total_chunks"].(float64))

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > size {
			end = size
		}
		w := putChunk(h, token, i, payload[start:end], int64(end-start))
		if w.Code != 200 {
			t.Fatalf("chunk %d status=%d", i, w.Code)
		}
	}

	req := httptest.NewRequest("GET", "/download/"+token, nil)
	req.Header.Set("User-Agent", "curl/8.0")
	req.Header.Set("Range", "bytes=128-255")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), payload[128:256]) {
		t.Fatalf("range body mismatch")
	}
}

// TestChunkIdempotent 同一 index 重复上传不会重复计入 received_bytes。
func TestChunkIdempotent(t *testing.T) {
	_, h := newResumableTestBridge(t, 10)

	const size = 64 * 1024
	const chunkSize = 64 * 1024
	reg := registerResumable(t, h, size, chunkSize)
	token := reg["auth_token"].(string)

	body := bytes.Repeat([]byte{1}, size)
	for i := 0; i < 3; i++ {
		w := putChunk(h, token, 0, body, size)
		if w.Code != 200 {
			t.Fatalf("chunk attempt %d status=%d", i, w.Code)
		}
	}
	status := statusJSON(t, h, token)
	if rb, _ := status["received_bytes"].(float64); int64(rb) != size {
		t.Fatalf("received_bytes should equal size after dup upload, got %v", rb)
	}
}

// TestChunkLengthChecks ContentLength != expectedLen 必须 400；未声明 ContentLength 必须 411。
func TestChunkLengthChecks(t *testing.T) {
	_, h := newResumableTestBridge(t, 10)
	const size = 64 * 1024
	reg := registerResumable(t, h, size, size)
	token := reg["auth_token"].(string)

	w := putChunk(h, token, 0, bytes.Repeat([]byte{1}, size), int64(size+10))
	if w.Code != 400 {
		t.Fatalf("expected 400 on length mismatch, got %d", w.Code)
	}

	url := "/upload/" + token + "/chunk?index=0"
	req := httptest.NewRequest("PUT", url, bytes.NewReader(bytes.Repeat([]byte{1}, size)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/octet-stream")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusLengthRequired {
		t.Fatalf("expected 411 on missing Content-Length, got %d", w.Code)
	}
}

// TestRegisterChunkSizeClamping computeChunkLayout 把过小 chunk_size 夹到 minChunk。
func TestRegisterChunkSizeClamping(t *testing.T) {
	_, h := newResumableTestBridge(t, 10)
	// 请求 1 KiB chunk → 应被夹到 64 KiB
	reg := registerResumable(t, h, 256*1024, 1024)
	gotChunk, _ := reg["chunk_size"].(float64)
	if int64(gotChunk) != 64*1024 {
		t.Fatalf("expected chunk_size clamped to 64KiB, got %v", gotChunk)
	}
	gotTotal, _ := reg["total_chunks"].(float64)
	if int(gotTotal) != 4 {
		t.Fatalf("expected total_chunks=4, got %v", gotTotal)
	}
}

// TestUploadReadyAfterAllChunks all chunks 到齐时 uploadReady.Load() 必须为 true。
func TestUploadReadyAfterAllChunks(t *testing.T) {
	ffb, h := newResumableTestBridge(t, 10)
	const size = 64 * 1024
	reg := registerResumable(t, h, size, size)
	token := reg["auth_token"].(string)

	w := putChunk(h, token, 0, bytes.Repeat([]byte{2}, size), size)
	if w.Code != 200 {
		t.Fatalf("chunk status=%d", w.Code)
	}
	ffb.mu.RLock()
	meta := ffb.fileRegistry[token]
	ffb.mu.RUnlock()
	if meta == nil {
		t.Fatalf("metadata missing after chunk")
	}
	if !meta.uploadReady.Load() {
		t.Fatalf("uploadReady should be true after all chunks received")
	}
}

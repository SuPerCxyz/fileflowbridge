package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// ==================== Resumable 上传：chunked PUT ====================
//
// 当 /register 时声明 resumable=true，bridge 会为该 token 创建：
//   - <TempDir>/<token>.part 临时文件（pre-allocated 到 size）
//   - 内存中的 ReceivedChunks 位图，按 chunk index 索引
//
// provider 通过 `PUT /upload/{token}/chunk?index=N` 上传单个 chunk。
// 服务器幂等：重复上传同一 index 不会出错（只是覆盖位图位）。
//
// 当所有 chunk 到齐后，meta.UploadReady=true；
// 此时下载端访问 /download/{token} 会从临时文件以 Range-friendly 方式服务。

// handleFileUploadChunk PUT /upload/{auth_token}/chunk?index=N
func (ffb *FileFlowBridge) handleFileUploadChunk(w http.ResponseWriter, r *http.Request) {
	if !ffb.acquireUploadSlot(r) {
		ffb.metrics.incUploadRejected()
		http.Error(w, "too many concurrent uploads", http.StatusTooManyRequests)
		return
	}
	defer ffb.releaseUploadSlot()

	authToken := chi.URLParam(r, "auth_token")

	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	ffb.mu.RUnlock()
	if !exists {
		http.Error(w, "无效的认证令牌", http.StatusUnauthorized)
		return
	}
	if !metadata.Resumable {
		http.Error(w, "该 token 未启用 resumable 上传，请使用 POST /upload/{token}", http.StatusBadRequest)
		return
	}
	if metadata.UploadReady {
		// 已经全部接收，幂等返回 200
		writeChunkStatus(w, metadata, http.StatusOK)
		return
	}

	indexStr := r.URL.Query().Get("index")
	if indexStr == "" {
		http.Error(w, "缺少 index query 参数", http.StatusBadRequest)
		return
	}
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 || index >= metadata.TotalChunks {
		http.Error(w, fmt.Sprintf("index 越界 (有效范围 0..%d)", metadata.TotalChunks-1), http.StatusBadRequest)
		return
	}

	offset := int64(index) * metadata.ChunkSize
	expectedLen := metadata.ChunkSize
	if index == metadata.TotalChunks-1 {
		expectedLen = metadata.Size - offset
	}

	if r.ContentLength > 0 && r.ContentLength != expectedLen {
		http.Error(w, fmt.Sprintf("chunk 大小不正确：期望 %d, 收到 Content-Length %d", expectedLen, r.ContentLength), http.StatusBadRequest)
		return
	}

	// 打开临时文件，按 offset 写入
	f, err := os.OpenFile(metadata.TempPath, os.O_WRONLY, 0o644)
	if err != nil {
		logError("打开临时文件失败: %v", err)
		http.Error(w, "服务器无法写入临时文件", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		logError("临时文件 seek 失败: %v", err)
		http.Error(w, "服务器写入失败", http.StatusInternalServerError)
		return
	}

	// 注入限速
	var bodyReader io.Reader = http.MaxBytesReader(w, r.Body, expectedLen+1024)
	if ffb.UploadBytesPerSec > 0 {
		bodyReader = newThrottledReader(bodyReader, newTokenBucket(ffb.UploadBytesPerSec))
	}

	n, err := io.CopyN(f, bodyReader, expectedLen)
	if err != nil && err != io.EOF {
		logWarn("chunk 写入失败 (token=%s index=%d): %v", authToken, index, err)
		http.Error(w, "chunk 写入失败", http.StatusBadRequest)
		return
	}
	if n != expectedLen {
		http.Error(w, fmt.Sprintf("chunk 字节数不足: 期望 %d 实际 %d", expectedLen, n), http.StatusBadRequest)
		return
	}

	// 更新位图
	metadata.chunkMu.Lock()
	alreadyHad := metadata.ReceivedChunks[index]
	metadata.ReceivedChunks[index] = true
	if !alreadyHad {
		metadata.ReceivedBytes += n
		ffb.metrics.addUploadBytes(n)
	}
	allDone := true
	for _, got := range metadata.ReceivedChunks {
		if !got {
			allDone = false
			break
		}
	}
	if allDone && !metadata.UploadReady {
		metadata.UploadReady = true
		metadata.UploadReadyAt = time.Now()
	}
	metadata.chunkMu.Unlock()

	if allDone {
		ffb.mu.Lock()
		if meta, ok := ffb.fileRegistry[authToken]; ok {
			meta.Status = "streaming"
			meta.StreamStarted = time.Now()
		}
		ffb.mu.Unlock()
		ffb.metrics.incUpload()
		logInfo("✅ Resumable 上传完成: %s (token_id: %s)", metadata.OriginalFilename, authToken)
	} else {
		logDebug("📦 收到 chunk index=%d/%d (token=%s)", index, metadata.TotalChunks-1, authToken)
	}

	writeChunkStatus(w, metadata, http.StatusOK)
}

// handleUploadStatus GET /upload/{auth_token}/status
//
// 返回 resumable 上传当前进度，供 provider 续传时查询已有的 chunk。
func (ffb *FileFlowBridge) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	authToken := chi.URLParam(r, "auth_token")

	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	ffb.mu.RUnlock()
	if !exists {
		http.Error(w, "无效的认证令牌", http.StatusUnauthorized)
		return
	}
	if !metadata.Resumable {
		http.Error(w, "该 token 未启用 resumable 上传", http.StatusBadRequest)
		return
	}

	writeChunkStatus(w, metadata, http.StatusOK)
}

// writeChunkStatus 输出 resumable 上传当前状态
func writeChunkStatus(w http.ResponseWriter, metadata *FileMetadata, code int) {
	metadata.chunkMu.Lock()
	missing := make([]int, 0)
	for i, got := range metadata.ReceivedChunks {
		if !got {
			missing = append(missing, i)
		}
	}
	received := metadata.ReceivedBytes
	ready := metadata.UploadReady
	metadata.chunkMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"resumable":      true,
		"total_chunks":   metadata.TotalChunks,
		"chunk_size":     metadata.ChunkSize,
		"size":           metadata.Size,
		"received_bytes": received,
		"missing_chunks": missing,
		"upload_ready":   ready,
	})
}

// computeChunkLayout 根据 size + 期望 chunkSize 计算实际 chunkSize / totalChunks
//
// 规则：
//   - chunkSize <= 0 → 使用默认 8 MiB
//   - chunkSize 强制 4 KiB 上限 1 GiB 之间
//   - totalChunks = ceil(size / chunkSize)，至少 1
func computeChunkLayout(size int64, chunkSize int64) (int64, int) {
	const (
		defaultChunk = 8 * 1024 * 1024
		minChunk     = 4 * 1024
		maxChunk     = 1024 * 1024 * 1024
	)
	if chunkSize <= 0 {
		chunkSize = defaultChunk
	}
	if chunkSize < minChunk {
		chunkSize = minChunk
	}
	if chunkSize > maxChunk {
		chunkSize = maxChunk
	}
	if size <= 0 {
		return chunkSize, 1
	}
	total := int((size + chunkSize - 1) / chunkSize)
	if total < 1 {
		total = 1
	}
	return chunkSize, total
}

// allocResumableTempFile 创建固定大小的临时文件
func (ffb *FileFlowBridge) allocResumableTempFile(authToken string, size int64) (string, error) {
	dir, err := ffb.ensureTempDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, authToken+".part")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			os.Remove(path)
			return "", err
		}
	}
	return path, nil
}

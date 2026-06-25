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
//   - 内存中的 receivedChunks 位集（bitset），按 chunk index 索引
//
// provider 通过 `PUT /upload/{token}/chunk?index=N` 上传单个 chunk。
// 服务器幂等：重复上传同一 index 不会出错（只是覆盖位图位）。
//
// 当所有 chunk 到齐后，meta.uploadReady=true；
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
	if metadata.uploadReady.Load() {
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

	// 严格 Content-Length 校验：不允许 chunked transfer，必须显式声明长度。
	if r.ContentLength < 0 {
		http.Error(w, "必须显式提供 Content-Length（不支持 chunked transfer）", http.StatusLengthRequired)
		return
	}
	if r.ContentLength != expectedLen {
		http.Error(w, fmt.Sprintf("chunk 大小不正确：期望 %d, 收到 Content-Length %d", expectedLen, r.ContentLength), http.StatusBadRequest)
		return
	}

	// 打开临时文件，按 offset 写入。0o600：仅 owner 可读写。
	f, err := os.OpenFile(metadata.TempPath, os.O_WRONLY, 0o600)
	if err != nil {
		// 如果在 RUnlock 到这里之间 cleanup 把 token 删了，返回 410 Gone 更精准。
		ffb.mu.RLock()
		_, stillExists := ffb.fileRegistry[authToken]
		ffb.mu.RUnlock()
		if !stillExists {
			http.Error(w, "token 已过期或被清理", http.StatusGone)
			return
		}
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

	// ContentLength 已严格等于 expectedLen，MaxBytesReader 严格夹到 expectedLen。
	var bodyReader io.Reader = http.MaxBytesReader(w, r.Body, expectedLen)
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

	// 更新位图（幂等：重复 index 不重复计入 ReceivedBytes / 不重复扣减 missingCount）
	newlyReceived, allDone := metadata.MarkChunkReceived(index, n)
	if newlyReceived {
		ffb.metrics.addUploadBytes(n)
	}

	if allDone && metadata.uploadReady.CompareAndSwap(false, true) {
		metadata.UploadReadyAt = time.Now()
		ffb.mu.Lock()
		meta, ok := ffb.fileRegistry[authToken]
		if ok {
			meta.Status = "streaming"
			meta.StreamStarted = time.Now()
		}
		ffb.mu.Unlock()
		ffb.metrics.incUpload()
		if !ok {
			// token 已被 cleanup 抢先清掉；记 WARN 而非静默跳过。
			logWarn("⚠️ chunk 全部到齐但 token 已被清理: %s", authToken)
		} else {
			logInfo("✅ Resumable 上传完成: %s (token_id: %s)", metadata.OriginalFilename, authToken)
		}
	} else if !allDone {
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

// maxMissingChunksInStatus 单次 status 响应中 missing_chunks 列表的硬上限。
// 防止小 chunk size 触发 O(N) 序列化 + 大 JSON 输出。
// 超过该上限时 missing_chunks 会被截断，missing_count 字段始终反映真实剩余数。
const maxMissingChunksInStatus = 10000

// writeChunkStatus 输出 resumable 上传当前状态
func writeChunkStatus(w http.ResponseWriter, metadata *FileMetadata, code int) {
	missing, missingCount, received := metadata.SnapshotChunkStatus(maxMissingChunksInStatus)
	ready := metadata.uploadReady.Load()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := map[string]interface{}{
		"resumable":      true,
		"total_chunks":   metadata.TotalChunks,
		"chunk_size":     metadata.ChunkSize,
		"size":           metadata.Size,
		"received_bytes": received,
		"missing_chunks": missing,
		"missing_count":  missingCount,
		"upload_ready":   ready,
	}
	if missingCount > len(missing) {
		// 提示客户端：missing 列表被截断，应先续传本批前缀再次查询。
		resp["missing_truncated"] = true
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// computeChunkLayout 根据 size + 期望 chunkSize 计算实际 chunkSize / totalChunks
//
// 规则：
//   - chunkSize <= 0 → 使用默认 8 MiB
//   - chunkSize 强制夹到 [64 KiB, 1 GiB]
//     (最小值提到 64 KiB 防止小 chunk 导致位图 / missing_chunks 爆炸)
//   - totalChunks = ceil(size / chunkSize)，至少 1；
//     用 int64 计算避免 32 位平台溢出。
func computeChunkLayout(size int64, chunkSize int64) (int64, int) {
	const (
		defaultChunk int64 = 8 * 1024 * 1024
		minChunk     int64 = 64 * 1024
		maxChunk     int64 = 1024 * 1024 * 1024
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
	total64 := (size + chunkSize - 1) / chunkSize
	if total64 < 1 {
		total64 = 1
	}
	// 64 KiB / 100 GiB ≈ 160 万 chunk，int 安全；
	// 上层路径里 size <= MaxFileSize（默认 100 GiB）由 /register 保证。
	return chunkSize, int(total64)
}

// allocResumableTempFile 创建固定大小的临时文件。
// 0o600：仅 bridge 进程 owner 可读写，防止多用户机器上他人读取文件内容。
func (ffb *FileFlowBridge) allocResumableTempFile(authToken string, size int64) (string, error) {
	dir, err := ffb.ensureTempDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, authToken+".part")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
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

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// ==================== Resumable 模式下载 ====================
//
// 与 stream 模式不同，resumable 模式的文件实际落盘在 metadata.TempPath。
// 全部 chunk 到齐后，下载端访问 /download 会用 http.ServeContent 服务该文件，
// 因此原生支持 Range / If-Range / HEAD / If-Modified-Since 等。

func (ffb *FileFlowBridge) serveResumableDownload(
	w http.ResponseWriter, r *http.Request, authToken string, metadata *FileMetadata,
) {
	if !metadata.UploadReady {
		// 425 Too Early：客户端可以稍后重试
		w.Header().Set("Retry-After", "5")
		http.Error(w, "上传尚未完成，请稍后再试", http.StatusTooEarly)
		ffb.metrics.incDownloadError()
		return
	}

	// 状态机：streaming/ready/downloading → downloading
	ffb.mu.Lock()
	if metadata.Status == "registered" {
		metadata.Status = "streaming"
	}
	previousStatus := metadata.Status
	metadata.Status = "downloading"
	ffb.mu.Unlock()

	defer func() {
		// resumable 模式同样是 single-shot：下载完成（或失败）后清理
		ffb.removeFileResources(authToken)
	}()

	// 打开临时文件
	f, err := os.Open(metadata.TempPath)
	if err != nil {
		ffb.mu.Lock()
		if meta, ok := ffb.fileRegistry[authToken]; ok && !ffb.downloadCompleted[authToken] {
			meta.Status = previousStatus
		}
		ffb.mu.Unlock()
		logError("打开 resumable 临时文件失败: %v", err)
		http.Error(w, "服务器内部错误", http.StatusInternalServerError)
		ffb.metrics.incDownloadError()
		return
	}
	defer f.Close()

	// 必要响应头：filename / SHA256 / FileID
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, metadata.OriginalFilename))
	w.Header().Set("X-FileFlow-FileID", authToken)
	w.Header().Set("X-FileFlow-Original-Filename", metadata.OriginalFilename)
	if metadata.ExpectedSHA256 != "" {
		w.Header().Set("X-FileFlow-SHA256", metadata.ExpectedSHA256)
	}
	w.Header().Set("Content-Type", "application/octet-stream")

	startTime := time.Now()
	logInfo("⬇️ 开始下载 (resumable): %s (token_id: %s)", metadata.OriginalFilename, authToken)

	// 用 http.ServeContent 自动处理 Range / HEAD / ETag
	// 注：包装 ResponseWriter 统计字节 / 注入下行限速
	rw := newCountingResponseWriter(w, ffb.DownloadBytesPerSec, ffb.metrics.addDownloadBytes)
	http.ServeContent(rw, r, metadata.OriginalFilename, metadata.UploadReadyAt, f)

	// 仅在完整发完整文件大小时才做 SHA256 校验（Range 请求不校验）
	rangeReq := r.Header.Get("Range") != ""
	transferCompleted := !rangeReq && rw.written >= metadata.Size

	if transferCompleted && metadata.ExpectedSHA256 != "" {
		// 重新读临时文件做 hash；resumable 落盘比 stream 模式宽裕，可以离线校验
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				got := hex.EncodeToString(h.Sum(nil))
				if got != metadata.ExpectedSHA256 {
					ffb.metrics.incHashMismatch()
					logError("❌ resumable SHA256 校验失败: %s 期望=%s 实际=%s",
						authToken, metadata.ExpectedSHA256, got)
				} else {
					logInfo("🔐 resumable SHA256 校验通过: %s", authToken)
				}
			}
		}
	}

	elapsed := time.Since(startTime)
	if transferCompleted {
		ffb.mu.Lock()
		ffb.serverStats.FilesTransferred++
		ffb.serverStats.BytesTransferred += rw.written
		ffb.downloadCompleted[authToken] = true
		ffb.downloadCompletedAt[authToken] = time.Now()
		ffb.closeDoneChLocked(authToken)
		ffb.mu.Unlock()
		ffb.metrics.incDownloadComplete()

		speed := float64(rw.written) / elapsed.Seconds() / 1024 / 1024
		logInfo("✅ resumable 传输完成: %s, %.2f MiB, %.2f MiB/s",
			authToken, float64(rw.written)/(1024*1024), speed)
	}
}

// ==================== 字节计数 + 限速 ResponseWriter ====================

type countingResponseWriter struct {
	http.ResponseWriter
	written  int64
	rateBPS  int64
	bucket   *tokenBucket
	addBytes func(int64)
}

func newCountingResponseWriter(w http.ResponseWriter, rateBPS int64, addBytes func(int64)) *countingResponseWriter {
	cw := &countingResponseWriter{
		ResponseWriter: w,
		rateBPS:        rateBPS,
		addBytes:       addBytes,
	}
	if rateBPS > 0 {
		cw.bucket = newTokenBucket(rateBPS)
	}
	return cw
}

func (cw *countingResponseWriter) Write(p []byte) (int, error) {
	if cw.bucket != nil {
		// 大块切片限速，避免单次等待太久
		const chunk = 64 * 1024
		i := 0
		for i < len(p) {
			end := i + chunk
			if end > len(p) {
				end = len(p)
			}
			cw.bucket.wait(int64(end - i))
			n, err := cw.ResponseWriter.Write(p[i:end])
			cw.written += int64(n)
			if cw.addBytes != nil {
				cw.addBytes(int64(n))
			}
			if err != nil {
				return i + n, err
			}
			i = end
		}
		return len(p), nil
	}

	n, err := cw.ResponseWriter.Write(p)
	cw.written += int64(n)
	if cw.addBytes != nil {
		cw.addBytes(int64(n))
	}
	return n, err
}

// 让 http.ResponseController 能找到 Flusher / ReaderFrom 等接口
func (cw *countingResponseWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// 显式提供 Header / WriteHeader 是为了让外部能拿到内层 ResponseWriter 的能力
var _ http.ResponseWriter = (*countingResponseWriter)(nil)

// 防止编译器报 strconv 未使用（HEAD 路径中可能用到）
var _ = strconv.Itoa

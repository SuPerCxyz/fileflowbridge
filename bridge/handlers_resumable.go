package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ==================== Resumable 模式下载 ====================
//
// 与 stream 模式不同，resumable 模式的文件实际落盘在 metadata.TempPath。
// 全部 chunk 到齐后，下载端访问 /download 会用 http.ServeContent 服务该文件，
// 因此原生支持 Range / If-Range / HEAD / If-Modified-Since 等。
//
// 状态机说明：状态转换由外层 handleDownloadRequest 统一负责，本函数仅做：
//   1) 上传完成性校验
//   2) 文件 IO + ServeContent
//   3) 完成时校验 SHA256 + 更新统计
// 不再独立设置 Status，避免与未来多下载端语义冲突。

func (ffb *FileFlowBridge) serveResumableDownload(
	w http.ResponseWriter, r *http.Request, authToken string, metadata *FileMetadata,
) {
	if !metadata.uploadReady.Load() {
		// 425 Too Early：客户端可以稍后重试
		w.Header().Set("Retry-After", "5")
		http.Error(w, "上传尚未完成，请稍后再试", http.StatusTooEarly)
		ffb.metrics.incDownloadError()
		return
	}

	defer func() {
		// resumable 模式同样是 single-shot：下载完成（或失败）后清理
		ffb.removeFileResources(authToken)
	}()

	// 打开临时文件
	f, err := os.Open(metadata.TempPath)
	if err != nil {
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

	// 仅在完整发完整文件大小时才做 SHA256 校验：
	// - Range 请求只发了部分文件
	// - HEAD 请求不传输 body
	rangeReq := r.Header.Get("Range") != ""
	isHead := r.Method == http.MethodHead
	transferCompleted := !rangeReq && !isHead && rw.written >= metadata.Size

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

// ReadFrom 实现 io.ReaderFrom，让 http.ServeContent 内部的 io.Copy
// 可以走 sendfile(2) 零拷贝路径（无限速时直接委托底层 ResponseWriter）。
// 有限速时回落到带令牌桶的分段拷贝。
func (cw *countingResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	if cw.bucket != nil {
		// 限速路径：分段读 + 令牌桶等待，无法走 sendfile
		const chunk = 64 * 1024
		buf := make([]byte, chunk)
		var total int64
		for {
			nr, er := src.Read(buf)
			if nr > 0 {
				cw.bucket.wait(int64(nr))
				nw, ew := cw.ResponseWriter.Write(buf[:nr])
				total += int64(nw)
				cw.written += int64(nw)
				if cw.addBytes != nil {
					cw.addBytes(int64(nw))
				}
				if ew != nil {
					return total, ew
				}
				if nr != nw {
					return total, io.ErrShortWrite
				}
			}
			if er == io.EOF {
				return total, nil
			}
			if er != nil {
				return total, er
			}
		}
	}

	// 无限速：委托底层 ResponseWriter 的 ReadFrom（标准 http 响应支持 sendfile(2)）。
	if rf, ok := cw.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		cw.written += n
		if cw.addBytes != nil {
			cw.addBytes(n)
		}
		return n, err
	}

	// 兜底：底层不支持 ReaderFrom 时用 io.Copy（仍走 cw.Write，统计正确）。
	return io.Copy(struct{ io.Writer }{cw}, src)
}

// 编译期保证接口
var (
	_ http.ResponseWriter = (*countingResponseWriter)(nil)
	_ io.ReaderFrom       = (*countingResponseWriter)(nil)
)

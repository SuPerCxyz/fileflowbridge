package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// ==================== 下载入口 ====================

// handleFileDownload GET /download/{auth_token}
func (ffb *FileFlowBridge) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	authToken := chi.URLParam(r, "auth_token")
	ffb.handleDownloadRequest(w, r, authToken)
}

// handleFileDownloadWithName GET /download/{auth_token}/{filename}
func (ffb *FileFlowBridge) handleFileDownloadWithName(w http.ResponseWriter, r *http.Request) {
	authToken := chi.URLParam(r, "auth_token")
	ffb.handleDownloadRequest(w, r, authToken)
}

// handleDownloadRequest 下载核心逻辑：
//   - 浏览器请求 → 中间页（serveDownloadPage）
//   - 其他客户端 → 等待 provider 流上线，流式转发
//
// 当前实现仅支持 single-shot 下载（max_downloads <= 1）。
// 多接收端被 /register 直接拒绝；这里不再处理 N > 1 的分支。
func (ffb *FileFlowBridge) handleDownloadRequest(w http.ResponseWriter, r *http.Request, authToken string) {
	// HEAD 仅用于探测元数据，不计入 download 指标（避免偏差）。
	if r.Method != http.MethodHead {
		ffb.metrics.incDownload()
	}

	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	isCompleted := ffb.downloadCompleted[authToken]
	ffb.mu.RUnlock()

	if isCompleted {
		http.Error(w, "文件下载已完成，资源已释放", http.StatusGone)
		ffb.metrics.incDownloadError()
		return
	}
	if !exists {
		http.Error(w, "文件不存在", http.StatusNotFound)
		ffb.metrics.incDownloadError()
		return
	}

	// 浏览器返回下载中间页
	if isBrowserRequest(r) {
		ffb.serveDownloadPage(w, r, authToken, metadata)
		return
	}

	// === Resumable 模式 ===
	//
	// resumable 文件落盘到 TempPath，全部 chunk 到齐前返回 425 Too Early；
	// 到齐后用 http.ServeFile 服务，原生支持 Range / If-Range / HEAD。
	if metadata.Resumable {
		ffb.serveResumableDownload(w, r, authToken, metadata)
		return
	}

	// HEAD：仅返回头
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeContentDispositionFilename(metadata.OriginalFilename)))
		if metadata.Size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
		}
		if metadata.ExpectedSHA256 != "" {
			w.Header().Set("X-FileFlow-SHA256", metadata.ExpectedSHA256)
		}
		w.Header().Set("Accept-Ranges", "none")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Range 请求：在边传边下的模型下无法 seek，直接拒绝
	if r.Header.Get("Range") != "" {
		w.Header().Set("Accept-Ranges", "none")
		http.Error(w, "Range requests not supported for in-flight streams", http.StatusRequestedRangeNotSatisfiable)
		ffb.metrics.incDownloadError()
		return
	}

	// 状态机：streaming/registered → downloading
	ffb.mu.Lock()
	metadata, exists = ffb.fileRegistry[authToken]
	isCompleted = ffb.downloadCompleted[authToken]
	if !exists {
		ffb.mu.Unlock()
		http.Error(w, "文件不存在", http.StatusNotFound)
		ffb.metrics.incDownloadError()
		return
	}
	if isCompleted {
		ffb.mu.Unlock()
		http.Error(w, "文件下载已完成，资源已释放", http.StatusGone)
		ffb.metrics.incDownloadError()
		return
	}
	if metadata.Status == "downloading" {
		ffb.mu.Unlock()
		http.Error(w, "文件正在下载中", http.StatusConflict)
		ffb.metrics.incDownloadError()
		return
	}
	if metadata.Status != "streaming" && metadata.Status != "registered" {
		ffb.mu.Unlock()
		http.Error(w, "文件尚未准备好下载", http.StatusServiceUnavailable)
		ffb.metrics.incDownloadError()
		return
	}
	previousStatus := metadata.Status
	metadata.Status = "downloading"
	ffb.mu.Unlock()

	// 等待 provider 流上线（最多 ~12 秒：前 6 次 100ms，之后 200ms）
	var streamConn interface{}
	var streamReady bool
	waitDuration := 100 * time.Millisecond
	maxRetries := 60
	for i := 0; i < maxRetries; i++ {
		ffb.mu.RLock()
		streamConn, streamReady = ffb.activeStreams[authToken]
		ffb.mu.RUnlock()
		if streamReady {
			break
		}
		time.Sleep(waitDuration)
		if i > 5 {
			waitDuration = 200 * time.Millisecond
		}
	}

	if !streamReady {
		ffb.mu.Lock()
		// 仅在状态仍是 "downloading"（我们设置的）时才回滚，
		// 避免覆盖并发清理或上传端更新导致的其他状态。
		if meta, ok := ffb.fileRegistry[authToken]; ok && !ffb.downloadCompleted[authToken] && meta.Status == "downloading" {
			meta.Status = previousStatus
		}
		ffb.mu.Unlock()
		logWarn("⚠️ 文件源不可用: %s", authToken)
		http.Error(w, "文件源不可用", http.StatusServiceUnavailable)
		ffb.metrics.incDownloadError()
		return
	}

	defer ffb.removeFileResources(authToken)

	// 响应头
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeContentDispositionFilename(metadata.OriginalFilename)))
	w.Header().Set("X-FileFlow-FileID", authToken)
	w.Header().Set("X-FileFlow-Original-Filename", sanitizeContentDispositionFilename(metadata.OriginalFilename))
	if metadata.ExpectedSHA256 != "" {
		w.Header().Set("X-FileFlow-SHA256", metadata.ExpectedSHA256)
	}
	if metadata.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
	}
	w.Header().Set("Accept-Ranges", "none")

	logInfo("⬇️ 开始下载: %s (token_id: %s)", metadata.OriginalFilename, authToken)

	startTime := time.Now()

	// 选择 reader / conn
	var reader io.Reader
	var conn net.Conn
	wsStream, isWS := streamConn.(*WebSocketStreamConnection)

	if tcpConn, ok := streamConn.(*StreamConnection); ok {
		reader = tcpConn.Reader
		conn = tcpConn.Conn
		if conn != nil {
			conn.SetReadDeadline(time.Now().Add(tcpStreamReadTimeout))
		}
	} else if isWS {
		reader = wsStream
		_ = wsStream.writeJSON(map[string]interface{}{
			"command": "download_started",
			"offset":  0,
			"size":    metadata.Size,
		})
		if err := wsStream.writeJSON(map[string]interface{}{
			"command": "send_chunk",
			"offset":  0,
			"size":    metadata.Size,
		}); err != nil {
			ffb.mu.Lock()
			if meta, ok := ffb.fileRegistry[authToken]; ok && !ffb.downloadCompleted[authToken] && meta.Status == "downloading" {
				meta.Status = previousStatus
			}
			ffb.mu.Unlock()
			logWarn("发送数据请求失败: %v", err)
			http.Error(w, "无法从上传端请求数据", http.StatusInternalServerError)
			ffb.metrics.incDownloadError()
			return
		}
		conn = nil
	} else {
		http.Error(w, "未知的连接类型", http.StatusInternalServerError)
		ffb.metrics.incDownloadError()
		return
	}

	// 下行限速封装：per-connection 独立桶
	if ffb.DownloadBytesPerSec > 0 {
		reader = newThrottledReader(reader, newTokenBucket(ffb.DownloadBytesPerSec))
	}

	// SHA256 增量计算
	var hasher hash.Hash
	if metadata.ExpectedSHA256 != "" {
		hasher = sha256.New()
	}

	totalTransferred, transferCompleted := ffb.pumpDownload(w, r, reader, conn, wsStream, hasher, metadata, authToken, startTime)

	// SHA256 校验：发现不匹配时 panic(http.ErrAbortHandler) 截断响应。
	// 由于 Content-Length 已经声明，下载端收到 short body 即可判定文件损坏。
	if transferCompleted && hasher != nil {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != metadata.ExpectedSHA256 {
			ffb.metrics.incHashMismatch()
			logError("❌ SHA256 校验失败: %s 期望=%s 实际=%s, 截断响应",
				authToken, metadata.ExpectedSHA256, got)
			ffb.finalizeDownload(authToken, metadata, totalTransferred, false, time.Since(startTime), wsStream)
			// 触发 http.Server 截断响应；不向客户端写错误体（Content-Length 已声明）
			panic(http.ErrAbortHandler)
		}
		logInfo("🔐 SHA256 校验通过: %s", authToken)
	}

	ffb.finalizeDownload(authToken, metadata, totalTransferred, transferCompleted, time.Since(startTime), wsStream)
}

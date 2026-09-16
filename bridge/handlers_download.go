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
	if !metadata.ExpiresAt.IsZero() && metadata.ExpiresAt.Before(time.Now()) {
		// 到期即拒绝，不等 cleanupExpiredFiles（5 分钟一轮）来兜底
		http.Error(w, "下载链接已过期", http.StatusGone)
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

	// 选择 reader / conn
	var reader io.Reader
	var conn net.Conn
	wsStream, isWS := streamConn.(*WebSocketStreamConnection)

	// 资源回收语义：single-shot 只在「完整下发整个文件」后被消耗。
	//   - WS 流可重发：中断（客户端断开/读超时）时保留 token，下载端可重试
	//   - TCP 流不可回卷：一旦读过一部分就无法从头重放，中断后必须回收
	// 统一收尾：无论正常返回还是 panic 截断，都走同一套资源处置逻辑
	completedNormally := false
	defer func() {
		if completedNormally || !isWS {
			ffb.removeFileResources(authToken)
			return
		}
		// WS 且未完整下发：保留 token，丢弃残留并回滚状态，供下载端重试
		ffb.markDownloadInterrupted(authToken, previousStatus, wsStream)
	}()

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

	if tcpConn, ok := streamConn.(*StreamConnection); ok {
		reader = tcpConn.Reader
		conn = tcpConn.Conn
		if conn != nil {
			conn.SetReadDeadline(time.Now().Add(tcpStreamReadTimeout))
		}
	} else if isWS {
		reader = wsStream

		// 上一次下载被中断过：先让 provider 作废其发送循环并清掉残留缓冲，
		// 否则这次会从上次断点继续吐数据，客户端拿到的是错位的字节流。
		ffb.mu.RLock()
		needReset := metadata.DownloadInterrupted
		ffb.mu.RUnlock()
		if needReset {
			if !wsStream.resetForNewDownload() {
				logWarn("⚠️ 无法确认上传端已停止旧循环，拒绝本次下载: %s", authToken)
				http.Error(w, "上一次传输中断且上传端未能复位，请重新发起传输", http.StatusServiceUnavailable)
				ffb.metrics.incDownloadError()
				return
			}
			ffb.mu.Lock()
			if meta, ok := ffb.fileRegistry[authToken]; ok {
				meta.DownloadInterrupted = false
			}
			ffb.mu.Unlock()
		}

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
	completedNormally = transferCompleted

	if !transferCompleted {
		// 具体处置（丢弃残留 / 回滚状态 / 回收资源）在 defer 中统一完成
		logWarn("⚠️ 下载未完成: %s (token_id: %s), 已传输 %d / %d",
			metadata.OriginalFilename, authToken, totalTransferred, metadata.Size)
	}
}

// markDownloadInterrupted 下载中断后的收尾（仅用于可重发的 WebSocket 流）：
//  1. 立刻丢弃残留数据——否则 WS 读协程写满 DataChan 后会超时退出并关闭连接，
//     token 会被 handleWebSocketConnection 的清理逻辑一并删掉，重试无从谈起；
//  2. 把状态从 downloading 回滚，否则后续重试会被 409「文件正在下载中」挡住；
//  3. 标记待复位，下次下载前再清一次，确保从 offset 0 的干净数据开始。
func (ffb *FileFlowBridge) markDownloadInterrupted(authToken, previousStatus string, wsStream *WebSocketStreamConnection) {
	if wsStream != nil {
		wsStream.drainDataQuiet(wsResetQuiet, wsResetBudget)
	}

	ffb.mu.Lock()
	defer ffb.mu.Unlock()
	meta, ok := ffb.fileRegistry[authToken]
	if !ok || ffb.downloadCompleted[authToken] || meta.Status != "downloading" {
		return
	}
	meta.Status = previousStatus
	meta.DownloadInterrupted = true
}

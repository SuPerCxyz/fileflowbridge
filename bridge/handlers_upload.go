package main

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ==================== HTTP multipart 上传 ====================
//
// 与 handleStreamConnection（TCP 路径）类似，本接口让 provider 端通过 HTTP
// multipart 上传文件，bridge 边接边转发给等待的下载端。
//
// 关键约束：
//   - 不使用 r.ParseMultipartForm（会将全部 body 缓冲到内存/磁盘，违背"边传边下"）
//   - 使用 r.MultipartReader() 拿到流式 part，再用 io.Pipe 把数据交给下载端
//   - 等待下载完成 / 资源回收 / 客户端断开 / 服务器关闭，全部走 channel select
func (ffb *FileFlowBridge) handleFileUpload(w http.ResponseWriter, r *http.Request) {
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
	if metadata.Resumable {
		http.Error(w, "该 token 启用了 resumable 上传，请使用 PUT /upload/{token}/chunk", http.StatusBadRequest)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		http.Error(w, "请求必须是multipart/form-data格式", http.StatusBadRequest)
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		logWarn("解析 multipart 请求失败: %v", err)
		http.Error(w, "无效的 multipart 请求", http.StatusBadRequest)
		return
	}

	var part *multipart.Part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			logWarn("读取 multipart part 失败: %v", err)
			http.Error(w, "读取上传数据失败", http.StatusBadRequest)
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		}
		_, _ = io.Copy(io.Discard, p)
		p.Close()
	}
	if part == nil {
		http.Error(w, "缺少 file 字段", http.StatusBadRequest)
		return
	}
	defer part.Close()

	ffb.metrics.incUpload()

	ffb.mu.Lock()
	if ffb.fileRegistry[authToken] != nil {
		ffb.fileRegistry[authToken].Status = "streaming"
		ffb.fileRegistry[authToken].StreamStarted = time.Now()
	}
	doneCh := ffb.getOrCreateDoneChLocked(authToken)
	if old, oldExists := ffb.activeStreams[authToken]; oldExists {
		if oldTCP, ok := old.(*StreamConnection); ok && oldTCP.Conn != nil {
			oldTCP.Conn.Close()
			logInfo("♻️ 关闭同 token 的旧 TCP 流连接: %s", authToken)
		} else if oldWS, ok := old.(*WebSocketStreamConnection); ok && oldWS.Conn != nil {
			oldWS.Conn.Close()
			logInfo("♻️ 关闭同 token 的旧 WebSocket 流连接: %s", authToken)
		}
	}
	ffb.mu.Unlock()

	// io.Pipe 把上传方 part 直接桥接给下载端 reader
	pr, pw := io.Pipe()

	// 注入上行限速
	var partReader io.Reader = part
	if ffb.UploadBytesPerSec > 0 {
		partReader = newThrottledReader(part, newTokenBucket(ffb.UploadBytesPerSec))
	}

	streamConn := &StreamConnection{
		Reader: pr,
		Writer: nil,
		Conn:   nil,
	}
	ffb.mu.Lock()
	ffb.activeStreams[authToken] = streamConn
	ffb.mu.Unlock()

	copyDone := make(chan error, 1)
	go func() {
		n, err := io.Copy(pw, partReader)
		ffb.metrics.addUploadBytes(n)
		_ = pw.CloseWithError(err)
		copyDone <- err
	}()

	select {
	case <-doneCh:
		_ = pw.CloseWithError(io.EOF)
	case <-r.Context().Done():
		logWarn("⚠️ 上传端断开: %s (token_id: %s)", metadata.OriginalFilename, authToken)
		_ = pw.CloseWithError(r.Context().Err())
		ffb.removeFileResources(authToken)
	case <-ffb.ShutdownEvent:
		_ = pw.CloseWithError(errServerShutdown)
		ffb.removeFileResources(authToken)
	case <-time.After(10 * time.Minute):
		logWarn("⚠️ 上传等待下载超时: %s (token_id: %s)", metadata.OriginalFilename, authToken)
		_ = pw.CloseWithError(errUploadTimeout)
		ffb.removeFileResources(authToken)
	}

	// 等待 io.Copy goroutine 退出；加超时防止 shutdown 时客户端空闲导致永久阻塞
	select {
	case <-copyDone:
	case <-time.After(10 * time.Second):
		logWarn("⚠️ 上传 copy goroutine 未在超时内退出: %s (token_id: %s)", metadata.OriginalFilename, authToken)
	}

	logInfo("✅ 文件上传处理完成: %s (token_id: %s)", metadata.OriginalFilename, authToken)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"success": true, "message": "文件上传处理完成"}`)
}

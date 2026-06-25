package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ==================== 注册 ====================

// handleRootPage 返回 index.html
func (ffb *FileFlowBridge) handleRootPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

// isHexString 校验全部字符是小写十六进制
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// handleFileRegistration POST /register
//
// Body: { filename, size, sha256?, max_downloads? }
// Resp: { auth_token, tcp_endpoint, download_url, expires_at, original_filename, sha256?, max_downloads }
func (ffb *FileFlowBridge) handleFileRegistration(w http.ResponseWriter, r *http.Request) {
	if !ffb.requireAPIKey(w, r) {
		ffb.metrics.incRegisterError()
		return
	}
	if r.Body == nil {
		ffb.metrics.incRegisterError()
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}

	var data struct {
		Filename     string `json:"filename"`
		Size         int64  `json:"size"`
		SHA256       string `json:"sha256,omitempty"`
		MaxDownloads int    `json:"max_downloads,omitempty"`
		Resumable    bool   `json:"resumable,omitempty"`
		ChunkSize    int64  `json:"chunk_size,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		ffb.metrics.incRegisterError()
		http.Error(w, "无效的JSON数据", http.StatusBadRequest)
		return
	}

	if data.Filename == "" {
		ffb.metrics.incRegisterError()
		http.Error(w, "文件名是必需的", http.StatusBadRequest)
		return
	}

	if data.Size > ffb.MaxFileSize {
		ffb.metrics.incRegisterError()
		http.Error(w, "文件大小超过限制", http.StatusRequestEntityTooLarge)
		return
	}

	expectedSHA := strings.ToLower(strings.TrimSpace(data.SHA256))
	if expectedSHA != "" {
		if len(expectedSHA) != 64 || !isHexString(expectedSHA) {
			ffb.metrics.incRegisterError()
			http.Error(w, "sha256 字段必须是 64 位十六进制字符串", http.StatusBadRequest)
			return
		}
	}

	// 多接收端 (max_downloads > 1) 暂未实现：
	// 当前架构是 provider 顺序流，不存在 fan-out / 缓存层，无法真正服务多个下载者。
	// 协议上保留字段以备未来扩展，目前直接拒绝避免悄默坏行为。
	if data.MaxDownloads > 1 {
		ffb.metrics.incRegisterError()
		http.Error(w, "max_downloads > 1 is not implemented in this version", http.StatusNotImplemented)
		return
	}
	if data.MaxDownloads < 0 {
		data.MaxDownloads = 0
	}

	// resumable 模式必须知道 size 才能预分配临时文件
	if data.Resumable && data.Size <= 0 {
		ffb.metrics.incRegisterError()
		http.Error(w, "resumable=true 时必须提交 size > 0", http.StatusBadRequest)
		return
	}

	authToken := ffb.createNewID()
	metadata := &FileMetadata{
		Filename:         data.Filename,
		OriginalFilename: data.Filename,
		Size:             data.Size,
		Status:           "registered",
		ClientIP:         r.RemoteAddr,
		AuthToken:        authToken,
		RegisteredAt:     time.Now(),
		ExpiresAt:        time.Now().Add(2 * time.Hour),
		ExpectedSHA256:   expectedSHA,
		MaxDownloads:     data.MaxDownloads,
		Resumable:        data.Resumable,
	}

	// resumable 模式下分配临时文件 + chunk 位图
	if data.Resumable {
		chunkSize, totalChunks := computeChunkLayout(data.Size, data.ChunkSize)
		path, err := ffb.allocResumableTempFile(authToken, data.Size)
		if err != nil {
			ffb.metrics.incRegisterError()
			logError("分配 resumable 临时文件失败: %v", err)
			http.Error(w, "服务器无法准备临时文件", http.StatusInternalServerError)
			return
		}
		metadata.ChunkSize = chunkSize
		metadata.TempPath = path
		metadata.InitChunkBitmap(totalChunks)
	}

	ffb.mu.Lock()
	ffb.fileRegistry[authToken] = metadata
	ffb.getOrCreateDoneChLocked(authToken)
	ffb.serverStats.FilesRegistered++
	ffb.mu.Unlock()
	ffb.metrics.incRegister()

	scheme := getScheme(r)
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var portStr string
	if scheme == "https" || r.Header.Get("X-Forwarded-Proto") == "https" {
		portStr = ""
	} else {
		portStr = fmt.Sprintf(":%d", ffb.HTTPPort)
	}
	safeFilename := url.PathEscape(data.Filename)

	responseData := map[string]interface{}{
		"auth_token": authToken,
		"tcp_endpoint": map[string]interface{}{
			"host": host,
			"port": ffb.TCPPort,
		},
		"download_url":      fmt.Sprintf("%s://%s%s/download/%s/%s", scheme, host, portStr, authToken, safeFilename),
		"expires_at":        metadata.ExpiresAt.Format(time.RFC3339),
		"original_filename": data.Filename,
		"max_downloads":     data.MaxDownloads,
	}
	if expectedSHA != "" {
		responseData["sha256"] = expectedSHA
	}
	if metadata.Resumable {
		responseData["resumable"] = true
		responseData["chunk_size"] = metadata.ChunkSize
		responseData["total_chunks"] = metadata.TotalChunks
		responseData["chunk_upload_url"] = fmt.Sprintf("%s://%s%s/upload/%s/chunk", scheme, host, portStr, authToken)
		responseData["chunk_status_url"] = fmt.Sprintf("%s://%s%s/upload/%s/status", scheme, host, portStr, authToken)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responseData)

	logInfo("📝 文件注册成功: %s (token_id: %s)", data.Filename, authToken)
}

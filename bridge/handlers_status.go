package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// ==================== 状态 / 统计 / 健康 ====================

// handleStatusCheck GET /status/{auth_token}
func (ffb *FileFlowBridge) handleStatusCheck(w http.ResponseWriter, r *http.Request) {
	authToken := chi.URLParam(r, "auth_token")

	// 在锁内拷贝所有需要的字段，避免锁外读取可变字段（如 Status）造成数据竞争
	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	completed := ffb.downloadCompleted[authToken]
	if !exists {
		ffb.mu.RUnlock()
		http.Error(w, "文件未找到", http.StatusNotFound)
		return
	}
	// 拷贝快照
	snapshot := struct {
		Filename           string
		OriginalFilename   string
		Size               int64
		Status             string
		ClientIP           string
		RegisteredAt       time.Time
		ExpiresAt          time.Time
		ExpectedSHA256     string
		MaxDownloads       int
		CompletedDownloads int
		StreamStarted      time.Time
		ClientAddress      string
	}{
		Filename:           metadata.Filename,
		OriginalFilename:   metadata.OriginalFilename,
		Size:               metadata.Size,
		Status:             metadata.Status,
		ClientIP:           metadata.ClientIP,
		RegisteredAt:       metadata.RegisteredAt,
		ExpiresAt:          metadata.ExpiresAt,
		ExpectedSHA256:     metadata.ExpectedSHA256,
		MaxDownloads:       metadata.MaxDownloads,
		CompletedDownloads: metadata.CompletedDownloads,
		StreamStarted:      metadata.StreamStarted,
		ClientAddress:      metadata.ClientAddress,
	}
	ffb.mu.RUnlock()

	responseData := map[string]interface{}{
		"filename":            snapshot.Filename,
		"original_filename":   snapshot.OriginalFilename,
		"size":                snapshot.Size,
		"status":              snapshot.Status,
		"client_ip":           snapshot.ClientIP,
		"registered_at":       snapshot.RegisteredAt.Format(time.RFC3339),
		"expires_at":          snapshot.ExpiresAt.Format(time.RFC3339),
		"download_completed":  completed,
		"max_downloads":       snapshot.MaxDownloads,
		"completed_downloads": snapshot.CompletedDownloads,
	}
	if snapshot.ExpectedSHA256 != "" {
		responseData["sha256"] = snapshot.ExpectedSHA256
	}
	if !snapshot.StreamStarted.IsZero() {
		responseData["stream_started"] = snapshot.StreamStarted.Format(time.RFC3339)
	}
	if snapshot.ClientAddress != "" {
		responseData["client_address"] = snapshot.ClientAddress
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responseData)
}

// handleServerStats GET /stats
func (ffb *FileFlowBridge) handleServerStats(w http.ResponseWriter, r *http.Request) {
	ffb.mu.RLock()
	stats := map[string]interface{}{
		"status":              "running",
		"uptime":              time.Since(ffb.serverStats.StartTime).Seconds(),
		"files_registered":    ffb.serverStats.FilesRegistered,
		"files_transferred":   ffb.serverStats.FilesTransferred,
		"bytes_transferred":   ffb.serverStats.BytesTransferred,
		"active_connections":  ffb.serverStats.ActiveConnections,
		"peak_connections":    ffb.serverStats.PeakConnections,
		"registered_files":    len(ffb.fileRegistry),
		"active_streams":      len(ffb.activeStreams),
		"completed_downloads": len(ffb.downloadCompleted),
	}
	ffb.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// handleHealthCheck GET /health
func (ffb *FileFlowBridge) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().Format(time.RFC3339),
		"version":   "2.0.0",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleClientConfig GET /config
//
// 返回前端需要知道的服务端开关，主要给浏览器上传页用：
//   - requires_api_key：服务端是否启用 --api-key（前端需要提示用户输入）
//   - max_file_size：注册的字节上限
//   - max_parallel_uploads：服务端并发上限，前端可据此提示用户
//
// 不返回任何敏感字段；本接口本身**不需要鉴权**，因为它就是用来告诉客户端
// "你是否需要鉴权"的元接口。
func (ffb *FileFlowBridge) handleClientConfig(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"requires_api_key":     ffb.APIKey != "",
		"max_file_size":        ffb.MaxFileSize,
		"max_parallel_uploads": ffb.MaxParallelUploads,
		"resumable_supported":  true,
		"revoke_supported":     true,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

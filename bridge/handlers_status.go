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

	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	completed := ffb.downloadCompleted[authToken]
	ffb.mu.RUnlock()

	if !exists {
		http.Error(w, "文件未找到", http.StatusNotFound)
		return
	}

	responseData := map[string]interface{}{
		"filename":            metadata.Filename,
		"original_filename":   metadata.OriginalFilename,
		"size":                metadata.Size,
		"status":              metadata.Status,
		"client_ip":           metadata.ClientIP,
		"registered_at":       metadata.RegisteredAt.Format(time.RFC3339),
		"expires_at":          metadata.ExpiresAt.Format(time.RFC3339),
		"download_completed":  completed,
		"max_downloads":       metadata.MaxDownloads,
		"completed_downloads": metadata.CompletedDownloads,
	}
	if metadata.ExpectedSHA256 != "" {
		responseData["sha256"] = metadata.ExpectedSHA256
	}
	if !metadata.StreamStarted.IsZero() {
		responseData["stream_started"] = metadata.StreamStarted.Format(time.RFC3339)
	}
	if metadata.ClientAddress != "" {
		responseData["client_address"] = metadata.ClientAddress
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

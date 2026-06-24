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

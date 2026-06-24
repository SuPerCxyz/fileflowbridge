package main

import (
	"os"
	"time"
)

// ==================== 清理逻辑 ====================

// cleanupResources 定时清理过期 / 已完成的资源
func (ffb *FileFlowBridge) cleanupResources() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if ffb.isShuttingDown.Load() {
				return
			}
			ffb.cleanupExpiredFiles(time.Now())
		case <-ffb.ShutdownEvent:
			return
		}
	}
}

// cleanupExpiredFiles 单次清理已过期文件，便于测试与定时任务复用
func (ffb *FileFlowBridge) cleanupExpiredFiles(currentTime time.Time) []string {
	var expiredFiles []string
	var completedTokens []string

	ffb.mu.RLock()
	for authToken, metadata := range ffb.fileRegistry {
		if metadata.ExpiresAt.Before(currentTime) {
			expiredFiles = append(expiredFiles, authToken)
		}
	}
	for authToken, completedAt := range ffb.downloadCompletedAt {
		if currentTime.Sub(completedAt) >= completedDownloadTTL {
			completedTokens = append(completedTokens, authToken)
		}
	}
	ffb.mu.RUnlock()

	for _, authToken := range expiredFiles {
		ffb.removeFileResources(authToken)
		logInfo("🧹 清理过期文件: %s", authToken)
	}

	if len(completedTokens) > 0 {
		ffb.mu.Lock()
		for _, authToken := range completedTokens {
			delete(ffb.downloadCompleted, authToken)
			delete(ffb.downloadCompletedAt, authToken)
			logInfo("🧹 清理下载完成标记: %s", authToken)
		}
		ffb.mu.Unlock()
	}

	return expiredFiles
}

// removeFileResources 清理"正在进行中"的资源（registry + 活动连接 + done channel + 临时文件）
//
// 注意：不会删除 downloadCompleted / downloadCompletedAt 标记，
// 这些由 cleanupExpiredFiles 按 completedDownloadTTL 统一清理。
//
// 实现要点：在锁内取出 streamConn / TempPath 并从 map 删除，
// 然后**在锁外**关闭物理连接 / 删临时文件，避免 IO 在持锁下意外 block。
func (ffb *FileFlowBridge) removeFileResources(authToken string) {
	ffb.mu.Lock()

	var tempPath string
	if meta, ok := ffb.fileRegistry[authToken]; ok {
		tempPath = meta.TempPath
	}
	delete(ffb.fileRegistry, authToken)

	streamConn, hadStream := ffb.activeStreams[authToken]
	if hadStream {
		delete(ffb.activeStreams, authToken)
	}

	ffb.closeDoneChLocked(authToken)
	ffb.mu.Unlock()

	if hadStream {
		if tcpConn, ok := streamConn.(*StreamConnection); ok && tcpConn.Conn != nil {
			tcpConn.Conn.Close()
		} else if wsConn, ok := streamConn.(*WebSocketStreamConnection); ok && wsConn.Conn != nil {
			wsConn.Conn.Close()
		}
	}

	if tempPath != "" {
		if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
			logWarn("删除 resumable 临时文件失败: %s: %v", tempPath, err)
		}
	}

	logInfo("🗑️ 文件资源已清理: %s", authToken)
}

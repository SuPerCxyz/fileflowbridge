package main

import (
	"time"
)

// finalizeDownload 收尾：更新计数 / 通知上传端 / 关闭连接
//
// 当前仅支持 single-shot（max_downloads <= 1）；多接收端在 /register 阶段已被拒绝。
// 完成时序：
//  1. 写最终 progress + transfer_complete 到 WS（doneCh 仍未关，WS 仍存活）
//  2. 锁内更新 metrics / downloadCompleted / 关闭 doneCh
//  3. 主调用方稍后会通过 defer removeFileResources 关物理连接；这里不再重复关
func (ffb *FileFlowBridge) finalizeDownload(
	authToken string,
	metadata *FileMetadata,
	totalTransferred int64,
	transferCompleted bool,
	elapsed time.Duration,
	wsStream *WebSocketStreamConnection,
) {
	transferTime := elapsed.Seconds()

	// 1) 先发最终进度 + transfer_complete；此时 doneCh 尚未关闭，WS 仍存活
	if transferCompleted && wsStream != nil && wsStream.Conn != nil {
		_ = wsStream.writeJSON(map[string]interface{}{
			"command": "progress",
			"bytes":   totalTransferred,
		})
		_ = wsStream.writeJSON(map[string]interface{}{
			"command": "transfer_complete",
			"message": "文件传输已完成",
		})
		logInfo("✅ 已通知上传端传输完成: %s", authToken)
	}

	// 2) 锁内更新状态
	ffb.mu.Lock()
	if _, ok := ffb.fileRegistry[authToken]; ok && transferCompleted {
		ffb.serverStats.FilesTransferred++
		ffb.metrics.incDownloadComplete()
		ffb.downloadCompleted[authToken] = true
		ffb.downloadCompletedAt[authToken] = time.Now()
		ffb.closeDoneChLocked(authToken)
	}
	ffb.mu.Unlock()

	if transferCompleted && transferTime > 0 {
		sizeMiB := float64(totalTransferred) / (1024 * 1024)
		speedValue := float64(totalTransferred) / transferTime / 1024
		speedUnit := "KiB/s"
		if speedValue >= 1024 {
			speedValue /= 1024
			speedUnit = "MiB/s"
		}
		logInfo("✅ 传输完成: %s (token_id: %s), 大小: %.2f MiB, 耗时: %.2fs, 速度: %.2f %s",
			metadata.OriginalFilename, authToken, sizeMiB, transferTime, speedValue, speedUnit)
	} else if !transferCompleted {
		logWarn("⚠️ 传输未完成: %s (token_id: %s), 已传输: %d / %d",
			metadata.OriginalFilename, authToken, totalTransferred, metadata.Size)
	}

	// 3) 物理连接由调用方的 `defer removeFileResources(authToken)` 负责关闭。
	//    此处不再 explicit Close，避免与 WS 读循环、心跳协程产生写竞争。
	if transferCompleted {
		logInfo("🏁 文件标记为已完成: %s (token_id: %s)", metadata.OriginalFilename, authToken)
	}
}

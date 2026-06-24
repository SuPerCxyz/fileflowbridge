package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"time"
)

// ==================== TCP 流连接处理 ====================

// handleStreamError 处理流连接错误并清理 activeStreams 表
func (ffb *FileFlowBridge) handleStreamError(authToken string, err error, conn net.Conn) {
	if err == io.EOF {
		logInfo("连接正常关闭: %s", authToken)
		return
	}

	if netErr, ok := err.(net.Error); ok {
		if netErr.Timeout() {
			logWarn("连接超时: %s - %v", authToken, netErr)
			if conn != nil {
				conn.SetReadDeadline(time.Time{})
			}
		} else {
			logWarn("网络错误: %s - %v", authToken, netErr)
		}
	} else {
		logWarn("流错误: %s - %v", authToken, err)
	}

	ffb.mu.Lock()
	defer ffb.mu.Unlock()
	delete(ffb.activeStreams, authToken)
}

// handleStreamConnection 处理来自 provider 的 TCP 流握手与转交
func (ffb *FileFlowBridge) handleStreamConnection(conn net.Conn) {
	// TCP 路径也参与上传并发限流；2 秒拿不到槽就拒绝
	if !ffb.acquireUploadSlotBlocking(2 * time.Second) {
		ffb.metrics.incUploadRejected()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Write([]byte("TOO_MANY_UPLOADS\n"))
		conn.Close()
		return
	}
	slotReleased := false
	defer func() {
		if !slotReleased {
			ffb.releaseUploadSlot()
		}
	}()

	isHandover := false
	defer func() {
		if !isHandover {
			conn.Close()
			logInfo("🔌 未完成握手的连接已释放: %s", conn.RemoteAddr().String())
		}
	}()

	ffb.mu.Lock()
	ffb.serverStats.ActiveConnections++
	if ffb.serverStats.ActiveConnections > ffb.serverStats.PeakConnections {
		ffb.serverStats.PeakConnections = ffb.serverStats.ActiveConnections
	}
	ffb.mu.Unlock()

	defer func() {
		ffb.mu.Lock()
		ffb.serverStats.ActiveConnections--
		ffb.mu.Unlock()
	}()

	logInfo("🔗 新的流连接来自 %s", conn.RemoteAddr().String())

	// 设置 TCP KeepAlive
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	// 仅用于元数据读取的超时
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	reader := bufio.NewReader(conn)
	metadataRaw, err := reader.ReadString('\n')
	if err != nil {
		logWarn("无效的连接元数据: %v", err)
		return
	}

	var metadata map[string]string
	if err := json.Unmarshal([]byte(metadataRaw), &metadata); err != nil {
		logWarn("元数据解析错误: %v", err)
		return
	}

	authToken := metadata["auth_token"]

	if !ffb.validateStreamConnection(authToken) {
		logWarn("⛔ 无效的连接尝试: %s", authToken)
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte("INVALID_CONNECTION\n")); err != nil {
			logWarn("⛔ 发送 INVALID_CONNECTION 失败: %v", err)
		}
		conn.Close()
		return
	}

	// 拒绝 resumable token 走 TCP（必须用 PUT /upload/{token}/chunk）
	ffb.mu.RLock()
	resumable := ffb.fileRegistry[authToken] != nil && ffb.fileRegistry[authToken].Resumable
	ffb.mu.RUnlock()
	if resumable {
		logWarn("⛔ resumable token 不允许 TCP 推送: %s", authToken)
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Write([]byte("RESUMABLE_REQUIRED\n"))
		conn.Close()
		return
	}

	// 更新文件状态
	ffb.mu.Lock()
	ffb.fileRegistry[authToken].Status = "streaming"
	ffb.fileRegistry[authToken].StreamStarted = time.Now()
	ffb.fileRegistry[authToken].ClientAddress = conn.RemoteAddr().String()
	fileName := ffb.fileRegistry[authToken].OriginalFilename
	ffb.mu.Unlock()

	conn.SetReadDeadline(time.Time{})

	streamConn := &StreamConnection{
		Reader: reader,
		Writer: conn,
		Conn:   conn,
	}

	// 若同一 token 已有活动连接，先关闭旧连接避免资源泄漏
	ffb.mu.Lock()
	if old, exists := ffb.activeStreams[authToken]; exists {
		if oldTCP, ok := old.(*StreamConnection); ok && oldTCP.Conn != nil {
			oldTCP.Conn.Close()
			logInfo("♻️ 关闭同 token 的旧 TCP 流连接: %s", authToken)
		} else if oldWS, ok := old.(*WebSocketStreamConnection); ok && oldWS.Conn != nil {
			oldWS.Conn.Close()
			logInfo("♻️ 关闭同 token 的旧 WebSocket 流连接: %s", authToken)
		}
	}
	ffb.activeStreams[authToken] = streamConn
	ffb.mu.Unlock()

	logInfo("✅ 流隧道已建立: %s (token_id: %s)", fileName, authToken)

	// 发送准备确认（带写超时）
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("STREAM_READY\n")); err != nil {
		logWarn("⛔ 发送 STREAM_READY 失败: %v", err)
		ffb.removeFileResources(authToken)
		return
	}
	conn.SetWriteDeadline(time.Time{})

	isHandover = true
	// 握手完成，监控协程接管；槽在该协程退出时释放
	go func() {
		ffb.monitorConnectionHealth(streamConn, authToken)
		ffb.releaseUploadSlot()
	}()
	slotReleased = true
}

// validateStreamConnection 校验 TCP 握手时携带的 token
func (ffb *FileFlowBridge) validateStreamConnection(authToken string) bool {
	ffb.mu.RLock()
	defer ffb.mu.RUnlock()

	metadata, exists := ffb.fileRegistry[authToken]
	if !exists {
		return false
	}
	if metadata.AuthToken != authToken {
		return false
	}
	if metadata.Status != "registered" {
		return false
	}
	if metadata.ExpiresAt.Before(time.Now()) {
		return false
	}
	if ffb.downloadCompleted[authToken] {
		return false
	}
	return true
}

// monitorConnectionHealth 仅用作 housekeeping，不再做底层 TCP_INFO 探测
func (ffb *FileFlowBridge) monitorConnectionHealth(conn *StreamConnection, authToken string) {
	ticker := time.NewTicker(connectionHealthInterval)
	defer ticker.Stop()

	ffb.mu.RLock()
	filename := "未知文件"
	if meta, ok := ffb.fileRegistry[authToken]; ok {
		filename = meta.OriginalFilename
	}
	ffb.mu.RUnlock()

	for {
		select {
		case <-ticker.C:
			ffb.mu.RLock()
			isCompleted := ffb.downloadCompleted[authToken]
			_, isActive := ffb.activeStreams[authToken]
			ffb.mu.RUnlock()

			if isCompleted || !isActive {
				logInfo("📭 文件 %s (token_id: %s) 传输结束或资源已释放，停止监控", filename, authToken)
				return
			}
		case <-ffb.ShutdownEvent:
			logInfo("🛑 服务器关闭，停止监控: %s (token_id: %s)", filename, authToken)
			return
		}
	}
}

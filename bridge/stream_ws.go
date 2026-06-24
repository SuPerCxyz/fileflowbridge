package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// ==================== WebSocket 流连接处理 ====================

// requestFileData 在锁外向 WS provider 发送 send_chunk 命令
func (ffb *FileFlowBridge) requestFileData(authToken string, offset, size int64) {
	ffb.mu.RLock()
	conn, exists := ffb.activeStreams[authToken]
	ffb.mu.RUnlock()
	if !exists {
		logWarn("找不到连接: %s", authToken)
		return
	}

	if wsConn, ok := conn.(*WebSocketStreamConnection); ok {
		request := map[string]interface{}{
			"command": "send_chunk",
			"offset":  offset,
			"size":    size,
		}
		if err := wsConn.writeJSON(request); err != nil {
			logWarn("发送数据请求失败: %v", err)
		}
	}
}

// handleWebSocketConnection 浏览器或 ws-provider 通过 /ws/{auth_token} 推送数据
func (ffb *FileFlowBridge) handleWebSocketConnection(w http.ResponseWriter, r *http.Request) {
	if !ffb.acquireUploadSlot(r) {
		ffb.metrics.incUploadRejected()
		http.Error(w, "too many concurrent uploads", http.StatusTooManyRequests)
		return
	}
	// 注意：WS 是长连接，释放放到 defer 末尾
	slotReleased := false
	defer func() {
		if !slotReleased {
			ffb.releaseUploadSlot()
		}
	}()

	authToken := chi.URLParam(r, "auth_token")

	ffb.mu.RLock()
	meta, exists := ffb.fileRegistry[authToken]
	ffb.mu.RUnlock()
	if !exists {
		http.Error(w, "无效的认证令牌", http.StatusUnauthorized)
		return
	}
	if meta != nil && meta.Resumable {
		http.Error(w, "该 token 启用了 resumable 上传，请使用 PUT /upload/{token}/chunk", http.StatusBadRequest)
		return
	}

	// 当 FileFlowBridge 通过裸结构体字面量初始化（多见于测试）时 upgrader 可能为 nil，
	// 这里做一次惰性初始化，保持安全策略与 isOriginAllowed 一致。
	if ffb.upgrader == nil {
		ffb.mu.Lock()
		if ffb.upgrader == nil {
			ffb.upgrader = &websocket.Upgrader{
				CheckOrigin: func(r *http.Request) bool {
					return ffb.isOriginAllowed(r.Header.Get("Origin"))
				},
			}
		}
		ffb.mu.Unlock()
	}

	conn, err := ffb.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logWarn("WebSocket升级失败: %v", err)
		return
	}

	logInfo("🔗 WebSocket连接已建立: %s", authToken)

	wsStreamConn := &WebSocketStreamConnection{
		Conn:      conn,
		DataChan:  make(chan []byte, 50),
		CloseChan: make(chan struct{}),
	}

	const (
		wsPongWait   = 90 * time.Second
		wsPingPeriod = 30 * time.Second
	)
	conn.SetReadLimit(64 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	// 更新状态 + 取 done channel + 替换旧连接
	ffb.mu.Lock()
	if ffb.fileRegistry[authToken] != nil {
		ffb.fileRegistry[authToken].Status = "streaming"
		ffb.fileRegistry[authToken].StreamStarted = time.Now()
	}
	if old, oldExists := ffb.activeStreams[authToken]; oldExists {
		if oldTCP, ok := old.(*StreamConnection); ok && oldTCP.Conn != nil {
			oldTCP.Conn.Close()
			logInfo("♻️ 关闭同 token 的旧 TCP 流连接: %s", authToken)
		} else if oldWS, ok := old.(*WebSocketStreamConnection); ok && oldWS.Conn != nil {
			oldWS.Conn.Close()
			logInfo("♻️ 关闭同 token 的旧 WebSocket 流连接: %s", authToken)
		}
	}
	ffb.activeStreams[authToken] = wsStreamConn
	doneCh := ffb.getOrCreateDoneChLocked(authToken)
	ffb.mu.Unlock()

	if err := wsStreamConn.writeMessage(websocket.TextMessage, []byte(`{"command":"READY"}`)); err != nil {
		logWarn("发送READY消息失败: %v", err)
		conn.Close()
		return
	}

	// 心跳协程
	go func() {
		ticker := time.NewTicker(wsPingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				wsStreamConn.Mutex.Lock()
				if wsStreamConn.Conn == nil {
					wsStreamConn.Mutex.Unlock()
					return
				}
				_ = wsStreamConn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				perr := wsStreamConn.Conn.WriteMessage(websocket.PingMessage, nil)
				wsStreamConn.Mutex.Unlock()
				if perr != nil {
					_ = conn.Close()
					return
				}
			case <-doneCh:
				_ = conn.Close()
				return
			case <-ffb.ShutdownEvent:
				_ = conn.Close()
				return
			case <-wsStreamConn.CloseChan:
				return
			}
		}
	}()

	// 数据读取协程
	go func() {
		defer wsStreamConn.close()
		defer conn.Close()
		// WS 退出时归还信号量
		defer func() {
			if !slotReleased {
				slotReleased = true
				ffb.releaseUploadSlot()
			}
		}()

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					logWarn("WebSocket意外关闭: %v", err)
				} else {
					logInfo("WebSocket连接关闭: %v", err)
				}
				break
			}

			switch messageType {
			case websocket.BinaryMessage:
				ffb.mu.RLock()
				isDownloadCompleted := ffb.downloadCompleted[authToken]
				ffb.mu.RUnlock()
				if isDownloadCompleted {
					logInfo("⚠️ 下载已完成，忽略上传数据: %s", authToken)
					continue
				}

				data := make([]byte, len(message))
				copy(data, message)

				select {
				case wsStreamConn.DataChan <- data:
				case <-doneCh:
					logInfo("📭 WebSocket 上传：检测到下载已完成/资源回收，停止接收: %s", authToken)
					return
				case <-time.After(10 * time.Second):
					logWarn("WebSocket数据通道阻塞，可能下载端已断开: %s", authToken)
					return
				}
			case websocket.TextMessage:
				var msg map[string]interface{}
				if err := json.Unmarshal(message, &msg); err == nil {
					if cmd, ok := msg["command"]; ok {
						switch cmd {
						case "request_data":
							offset, _ := msg["offset"].(float64)
							size, _ := msg["size"].(float64)
							ffb.requestFileData(authToken, int64(offset), int64(size))
						case "download_started":
							logInfo("下载已开始: %s", authToken)
						case "stop_upload":
							logInfo("客户端请求停止上传: %s", authToken)
							ffb.removeFileResources(authToken)
							return
						}
					}
				}
			}
		}
	}()

	// 连接关闭时清理资源
	defer func() {
		ffb.mu.RLock()
		metadata, ok := ffb.fileRegistry[authToken]
		completed := ffb.downloadCompleted[authToken]
		shouldCleanup := ok && !completed && metadata.Status == "streaming"
		ffb.mu.RUnlock()

		if shouldCleanup {
			ffb.removeFileResources(authToken)
			logInfo("🧹 已清理放弃的WebSocket上传: %s", authToken)
			return
		}

		ffb.mu.Lock()
		delete(ffb.activeStreams, authToken)
		ffb.mu.Unlock()
		logInfo("🔗 WebSocket连接已关闭: %s", authToken)
	}()

	<-wsStreamConn.CloseChan
}

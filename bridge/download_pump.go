package main

import (
	"hash"
	"io"
	"net"
	"net/http"
	"time"
)

// pumpDownload 从 reader 流式拷贝到 HTTP ResponseWriter，
// 期间同步更新 hash / 进度统计 / 向 ws 上报进度。
//
// 返回：总转发字节数、是否完成（按文件大小判定，0 表示流式不定长）
func (ffb *FileFlowBridge) pumpDownload(
	w http.ResponseWriter,
	r *http.Request,
	reader io.Reader,
	conn net.Conn,
	wsStream *WebSocketStreamConnection,
	hasher hash.Hash,
	metadata *FileMetadata,
	authToken string,
	startTime time.Time,
) (int64, bool) {
	var totalTransferred int64
	var localChunk int64
	buf := make([]byte, 256*1024)

	const progressReportBytes = 256 * 1024
	const progressReportInterval = 200 * time.Millisecond
	var lastProgressReported int64
	lastProgressTime := startTime
	// 复用同一个 map，避免每次进度上报都分配新对象
	progressMsg := map[string]interface{}{
		"command": "progress",
		"bytes":   int64(0),
	}

	clientClosed := func() bool {
		select {
		case <-r.Context().Done():
			return true
		default:
			return false
		}
	}

	notifyStopUpload := func() {
		if wsStream != nil && wsStream.Conn != nil {
			_ = wsStream.writeJSON(map[string]interface{}{
				"command": "stop_upload",
			})
		}
	}

	for {
		if clientClosed() {
			logInfo("❌ 客户端连接断开: %s (token_id: %s)", metadata.OriginalFilename, authToken)
			notifyStopUpload()
			break
		}

		n, err := reader.Read(buf)
		if err != nil {
			if err == io.EOF {
				if n > 0 {
					if hasher != nil {
						hasher.Write(buf[:n])
					}
					if _, werr := w.Write(buf[:n]); werr != nil {
						logWarn("❌ 客户端写入失败: %v", werr)
					}
					totalTransferred += int64(n)
				}
				break
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logWarn("⚠️ 读取超时，判定传输中断: %v", err)
				break
			}
			ffb.handleStreamError(authToken, err, conn)
			break
		}
		if n == 0 {
			break
		}

		if clientClosed() {
			notifyStopUpload()
			break
		}

		if hasher != nil {
			hasher.Write(buf[:n])
		}

		if _, err := w.Write(buf[:n]); err != nil {
			logWarn("❌ 客户端断开连接: %v", err)
			notifyStopUpload()
			break
		}

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		totalTransferred += int64(n)
		localChunk += int64(n)
		ffb.metrics.addDownloadBytes(int64(n))

		// 节流向 WebSocket 上传端回报真实已转发字节
		if wsStream != nil && wsStream.Conn != nil {
			if totalTransferred-lastProgressReported >= progressReportBytes ||
				time.Since(lastProgressTime) >= progressReportInterval {
				progressMsg["bytes"] = totalTransferred
				_ = wsStream.writeJSON(progressMsg)
				lastProgressReported = totalTransferred
				lastProgressTime = time.Now()
			}
		}

		if metadata.Size > 0 && totalTransferred >= metadata.Size {
			logInfo("✅ 文件数据已全部传输: %s (token_id: %s)", metadata.OriginalFilename, authToken)
			break
		}

		if localChunk >= 10*1024*1024 {
			ffb.mu.Lock()
			ffb.serverStats.BytesTransferred += localChunk
			ffb.mu.Unlock()
			localChunk = 0
		}

		if conn != nil {
			conn.SetReadDeadline(time.Now().Add(tcpStreamReadTimeout))
		}
	}

	ffb.mu.Lock()
	ffb.serverStats.BytesTransferred += localChunk
	ffb.mu.Unlock()

	completed := metadata.Size == 0 || totalTransferred >= metadata.Size
	return totalTransferred, completed
}

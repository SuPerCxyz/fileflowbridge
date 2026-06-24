package main

import (
	"net/http"
	"time"
)

// ==================== 上传并发限流 ====================
//
// 共一个进程级信号量；所有上传入口（TCP / WS / multipart / chunk PUT）共享。
// 容量由 FileFlowBridge.MaxParallelUploads 控制（默认 10）。
//
// 行为：
//   - acquireUploadSlot 在最多 acquireWait 时间内尝试取一个槽；超时返回 false
//   - 调用方持有期间不能 nest 调用，避免死锁
//   - 取到槽必须确保 releaseUploadSlot 被调用

const acquireWait = 2 * time.Second

// acquireUploadSlot 尝试取得一个上传槽。
// 当未配置上限（uploadSem == nil）时立即返回 true。
// 当 HTTP 客户端断开（r.Context().Done()）时也会立即返回 false。
func (ffb *FileFlowBridge) acquireUploadSlot(r *http.Request) bool {
	if ffb.uploadSem == nil {
		return true
	}
	timer := time.NewTimer(acquireWait)
	defer timer.Stop()
	select {
	case ffb.uploadSem <- struct{}{}:
		return true
	case <-r.Context().Done():
		return false
	case <-timer.C:
		return false
	case <-ffb.ShutdownEvent:
		return false
	}
}

// acquireUploadSlotBlocking 用于无 HTTP 上下文的 TCP 路径。
// 在 ShutdownEvent 触发或 timeout 之前阻塞。
func (ffb *FileFlowBridge) acquireUploadSlotBlocking(timeout time.Duration) bool {
	if ffb.uploadSem == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ffb.uploadSem <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ffb.ShutdownEvent:
		return false
	}
}

// releaseUploadSlot 释放一个上传槽。调用方应确保仅在 acquire 成功时调用。
func (ffb *FileFlowBridge) releaseUploadSlot() {
	if ffb.uploadSem == nil {
		return
	}
	select {
	case <-ffb.uploadSem:
	default:
		// 不应发生；如果 release 多于 acquire 就静默吞掉
	}
}

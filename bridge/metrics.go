package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// ==================== Prometheus 兼容指标（最小实现） ====================
//
// 为避免引入 prometheus/client_golang 体积依赖，这里手写一个最简实现，
// 输出 Prometheus text exposition format 0.0.4，
// 可被 Prometheus / VictoriaMetrics / Grafana Agent 直接 scrape。

// serverMetrics 嵌入到 FileFlowBridge 内
type serverMetrics struct {
	registerTotal     atomic.Int64
	registerErrors    atomic.Int64
	uploadTotal       atomic.Int64
	uploadRejected    atomic.Int64
	downloadTotal     atomic.Int64
	downloadErrors    atomic.Int64
	downloadsComplete atomic.Int64
	hashMismatches    atomic.Int64
	bytesUploaded     atomic.Int64
	bytesDownloaded   atomic.Int64
}

func (m *serverMetrics) incRegister()         { m.registerTotal.Add(1) }
func (m *serverMetrics) incRegisterError()    { m.registerErrors.Add(1) }
func (m *serverMetrics) incUpload()           { m.uploadTotal.Add(1) }
func (m *serverMetrics) incUploadRejected()   { m.uploadRejected.Add(1) }
func (m *serverMetrics) incDownload()         { m.downloadTotal.Add(1) }
func (m *serverMetrics) incDownloadError()    { m.downloadErrors.Add(1) }
func (m *serverMetrics) incDownloadComplete() { m.downloadsComplete.Add(1) }
func (m *serverMetrics) incHashMismatch()     { m.hashMismatches.Add(1) }
func (m *serverMetrics) addUploadBytes(n int64) {
	if n > 0 {
		m.bytesUploaded.Add(n)
	}
}
func (m *serverMetrics) addDownloadBytes(n int64) {
	if n > 0 {
		m.bytesDownloaded.Add(n)
	}
}

// handleMetrics 实现 /metrics 端点
//
// 若设置了 --metrics-key / FFB_METRICS_KEY，则要求请求携带 X-Metrics-Key 头
// （也支持 Authorization: Bearer）。与业务用的 X-API-Key 隔离，避免抓取凭证泄漏后
// 可冒充注册者。
func (ffb *FileFlowBridge) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !ffb.requireMetricsKey(w, r) {
		return
	}

	ffb.mu.RLock()
	registered := len(ffb.fileRegistry)
	activeStreams := len(ffb.activeStreams)
	completedTracked := len(ffb.downloadCompleted)
	uptime := time.Since(ffb.serverStats.StartTime).Seconds()
	stats := ffb.serverStats
	ffb.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Helper：写一行指标
	write := func(name, help, mtype string, value any) {
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
		fmt.Fprintf(w, "# TYPE %s %s\n", name, mtype)
		fmt.Fprintf(w, "%s %v\n", name, value)
	}

	write("ffb_uptime_seconds", "Server uptime in seconds.", "gauge", uptime)
	write("ffb_files_registered_total", "Total number of files registered.", "counter", ffb.metrics.registerTotal.Load())
	write("ffb_register_errors_total", "Total number of failed register requests.", "counter", ffb.metrics.registerErrors.Load())
	write("ffb_uploads_total", "Total number of upload sessions started.", "counter", ffb.metrics.uploadTotal.Load())
	write("ffb_uploads_rejected_total", "Uploads rejected due to MaxParallelUploads limit.", "counter", ffb.metrics.uploadRejected.Load())
	write("ffb_downloads_total", "Total number of download sessions started.", "counter", ffb.metrics.downloadTotal.Load())
	write("ffb_downloads_complete_total", "Total number of downloads that completed successfully.", "counter", ffb.metrics.downloadsComplete.Load())
	write("ffb_downloads_errors_total", "Total number of downloads that ended in error.", "counter", ffb.metrics.downloadErrors.Load())
	write("ffb_hash_mismatch_total", "Total number of SHA256 hash mismatches detected on completion.", "counter", ffb.metrics.hashMismatches.Load())
	write("ffb_bytes_uploaded_total", "Total bytes received from providers.", "counter", ffb.metrics.bytesUploaded.Load())
	write("ffb_bytes_downloaded_total", "Total bytes sent to downloaders.", "counter", ffb.metrics.bytesDownloaded.Load())

	// 与原 /stats 重叠但用 gauge / counter 暴露给 Prometheus
	write("ffb_files_in_registry", "Currently registered (in-flight) files.", "gauge", registered)
	write("ffb_active_streams", "Currently active streaming connections.", "gauge", activeStreams)
	write("ffb_completed_downloads_tracked", "Completed download markers still kept (within TTL).", "gauge", completedTracked)
	write("ffb_peak_connections", "Peak active connections seen since start.", "gauge", stats.PeakConnections)
}

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// 主函数 —— 入口；具体功能见 server.go / registry.go / stream_*.go / handlers_*.go
func main() {
	fmt.Println("🌊 FileFlow Bridge - 文件流桥接服务器")
	fmt.Println("==================================================")

	setupLogging()

	// 环境变量默认值
	defaultHTTPPort := getEnvInt("FFB_HTTP_PORT", 8000)
	defaultTCPPort := getEnvInt("FFB_TCP_PORT", 8888)
	defaultMaxFileSize := getEnvInt64("FFB_MAX_FILE_SIZE", 100)
	defaultTokenLength := getEnvInt("FFB_TOKEN_LEN", 16)
	defaultAllowedOrigins := os.Getenv("FFB_ALLOWED_ORIGINS")
	defaultAPIKey := os.Getenv("FFB_API_KEY")
	defaultMetricsKey := os.Getenv("FFB_METRICS_KEY")
	defaultTLSCert := os.Getenv("FFB_TLS_CERT")
	defaultTLSKey := os.Getenv("FFB_TLS_KEY")
	defaultUploadBPS := getEnvInt64("FFB_UPLOAD_BPS", 0)
	defaultDownloadBPS := getEnvInt64("FFB_DOWNLOAD_BPS", 0)
	defaultMaxParallel := getEnvInt("FFB_MAX_PARALLEL_UPLOADS", 10)
	defaultTempDir := os.Getenv("FFB_TEMP_DIR")

	httpPort := flag.Int("http-port", defaultHTTPPort, "HTTP 服务器端口")
	tcpPort := flag.Int("tcp-port", defaultTCPPort, "TCP 流服务器端口")
	maxFileSize := flag.Int64("max-file-size", defaultMaxFileSize, "最大允许文件大小 (GiB)")
	tokenLength := flag.Int("token-len", defaultTokenLength, "随机 token 长度 (6-32)；默认 16，建议 ≥ 12")
	allowedOrigins := flag.String("allowed-origins", defaultAllowedOrigins, "CORS 允许的 origin 列表，逗号分隔；空或 * 表示放行全部")
	apiKey := flag.String("api-key", defaultAPIKey, "可选：要求 /register 携带 X-API-Key 头")
	metricsKey := flag.String("metrics-key", defaultMetricsKey, "可选：要求 /metrics 携带 X-Metrics-Key 头（独立于 API Key）")
	tlsCert := flag.String("tls-cert", defaultTLSCert, "TLS 证书路径（与 --tls-key 同时设置时启用 HTTPS）")
	tlsKey := flag.String("tls-key", defaultTLSKey, "TLS 私钥路径")
	uploadBPS := flag.Int64("upload-bps", defaultUploadBPS, "上行 per-connection 限速 bytes/sec，0=不限速")
	downloadBPS := flag.Int64("download-bps", defaultDownloadBPS, "下行 per-connection 限速 bytes/sec，0=不限速")
	maxParallel := flag.Int("max-parallel-uploads", defaultMaxParallel, "并行上传数上限（共享 TCP/WS/multipart/chunk）；0/负数=不限")
	tempDir := flag.String("temp-dir", defaultTempDir, "resumable 模式的临时文件目录（默认 OSTempDir/fileflow-bridge）")

	flag.Parse()

	if *maxFileSize <= 0 {
		log.Printf("⚠️ 警告: max-file-size %d 无效，回落默认值 100 GiB", *maxFileSize)
		*maxFileSize = 100
	}
	maxFileSizeBytes := (*maxFileSize) * 1024 * 1024 * 1024

	if *tokenLength < 6 || *tokenLength > 32 {
		log.Printf("⚠️ 警告: ID 长度 %d 不在有效范围 (6-32)，回落 16", *tokenLength)
		*tokenLength = 16
	}

	server := NewFileFlowBridge(*httpPort, *tcpPort, maxFileSizeBytes, *tokenLength)

	if origins := strings.TrimSpace(*allowedOrigins); origins != "" {
		parts := strings.Split(origins, ",")
		cleaned := make([]string, 0, len(parts))
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				cleaned = append(cleaned, v)
			}
		}
		server.AllowedOrigins = cleaned
	}
	server.APIKey = strings.TrimSpace(*apiKey)
	server.MetricsKey = strings.TrimSpace(*metricsKey)
	server.TLSCertFile = strings.TrimSpace(*tlsCert)
	server.TLSKeyFile = strings.TrimSpace(*tlsKey)
	if *uploadBPS > 0 {
		server.UploadBytesPerSec = *uploadBPS
	}
	if *downloadBPS > 0 {
		server.DownloadBytesPerSec = *downloadBPS
	}
	server.MaxParallelUploads = *maxParallel
	server.initUploadSem()
	server.TempDir = strings.TrimSpace(*tempDir)
	if server.APIKey != "" {
		log.Printf("🔐 已启用 API Key 鉴权（X-API-Key 头）")
	}
	if server.MetricsKey != "" {
		log.Printf("🔐 已启用 Metrics Key 鉴权（X-Metrics-Key 头）")
	}
	if server.MaxParallelUploads > 0 {
		log.Printf("🚦 上传并发上限: %d", server.MaxParallelUploads)
	} else {
		log.Printf("🚦 上传并发上限: 不限")
	}
	if server.TLSCertFile != "" && server.TLSKeyFile != "" {
		log.Printf("🔒 已启用 HTTPS：cert=%s key=%s", server.TLSCertFile, server.TLSKeyFile)
	}
	if server.UploadBytesPerSec > 0 {
		log.Printf("🚧 上行限速: %d B/s (~%.2f MiB/s)", server.UploadBytesPerSec, float64(server.UploadBytesPerSec)/(1024*1024))
	}
	if server.DownloadBytesPerSec > 0 {
		log.Printf("🚧 下行限速: %d B/s (~%.2f MiB/s)", server.DownloadBytesPerSec, float64(server.DownloadBytesPerSec)/(1024*1024))
	}

	// 信号处理 → ShutdownEvent
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("🛑 收到信号 %v，开始关闭...", sig)
		server.TriggerShutdown()
	}()

	if err := server.StartServer(); err != nil {
		log.Fatalf("💥 服务器启动失败: %v", err)
	}

	fmt.Println("👋 服务器已停止")
}

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// NewFileFlowBridge 初始化服务器
func NewFileFlowBridge(httpPort, tcpPort int, maxFileSize int64, tokenLength int) *FileFlowBridge {
	ffb := &FileFlowBridge{
		HTTPPort:            httpPort,
		TCPPort:             tcpPort,
		MaxFileSize:         maxFileSize,
		TokenLength:         tokenLength,
		MaxParallelUploads:  10, // 默认 10 并发
		TempDir:             "", // empty → 在 StartServer/buildRouter 阶段初始化
		ShutdownEvent:       make(chan struct{}),
		fileRegistry:        make(map[string]*FileMetadata),
		activeStreams:       make(map[string]interface{}),
		downloadCompleted:   make(map[string]bool),
		downloadCompletedAt: make(map[string]time.Time),
		downloadDone:        make(map[string]chan struct{}),
		serverStats: ServerStats{
			StartTime: time.Now(),
		},
	}
	ffb.upgrader = &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return ffb.isOriginAllowed(r.Header.Get("Origin"))
		},
	}
	ffb.initUploadSem()
	return ffb
}

// initUploadSem 在 MaxParallelUploads > 0 时构造信号量；否则保持 nil（不限）
func (ffb *FileFlowBridge) initUploadSem() {
	if ffb.MaxParallelUploads > 0 {
		ffb.uploadSem = make(chan struct{}, ffb.MaxParallelUploads)
	} else {
		ffb.uploadSem = nil
	}
}

// ensureTempDir 返回 resumable 临时文件根目录；首次调用时创建。
// 权限 0o700：仅 bridge 进程 owner 可访问，防止多用户机器上其他用户读取 .part。
func (ffb *FileFlowBridge) ensureTempDir() (string, error) {
	dir := ffb.TempDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "fileflow-bridge")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll 在目录已存在时不会调整权限，显式 Chmod 一次。
	// 失败仅记 WARN：可能是非 owner 持有该目录或不支持 Unix 权限位的 FS。
	if err := os.Chmod(dir, 0o700); err != nil {
		logWarn("temp-dir Chmod 0o700 失败: %s: %v", dir, err)
	}
	return dir, nil
}

// buildRouter 构造完整路由（独立函数，便于测试复用）
func (ffb *FileFlowBridge) buildRouter() http.Handler {
	router := chi.NewRouter()

	router.Post("/register", ffb.handleFileRegistration)
	router.Delete("/register/{auth_token}", ffb.handleFileRevocation)
	router.Post("/upload/{auth_token}", ffb.handleFileUpload)
	router.Put("/upload/{auth_token}/chunk", ffb.handleFileUploadChunk)
	router.Get("/upload/{auth_token}/status", ffb.handleUploadStatus)
	router.Get("/ws/{auth_token}", ffb.handleWebSocketConnection)
	router.Get("/download/{auth_token}", ffb.handleFileDownload)
	router.Get("/download/{auth_token}/{filename}", ffb.handleFileDownloadWithName)
	router.Head("/download/{auth_token}", ffb.handleFileDownload)
	router.Head("/download/{auth_token}/{filename}", ffb.handleFileDownloadWithName)
	router.Get("/status/{auth_token}", ffb.handleStatusCheck)
	router.Get("/stats", ffb.handleServerStats)
	router.Get("/health", ffb.handleHealthCheck)
	router.Get("/metrics", ffb.handleMetrics)
	router.Get("/config", ffb.handleClientConfig)

	// 静态文件：仅在 ./static 目录存在时启用
	staticDir := "./static"
	if _, err := os.Stat(staticDir); err == nil {
		router.Get("/", ffb.handleRootPage)
		fs := http.FileServer(http.Dir(staticDir))
		router.Handle("/static/*", http.StripPrefix("/static/", fs))
	}

	return ffb.corsMiddleware(router)
}

// StartServer 启动 HTTP / WebSocket / TCP 三个监听并阻塞，直到收到 ShutdownEvent
func (ffb *FileFlowBridge) StartServer() error {
	handler := ffb.buildRouter()

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", ffb.HTTPPort),
		Handler: handler,
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", ffb.TCPPort))
	if err != nil {
		return fmt.Errorf("TCP服务器启动失败: %v", err)
	}

	go ffb.cleanupResources()

	useTLS := ffb.TLSCertFile != "" && ffb.TLSKeyFile != ""
	go func() {
		if useTLS {
			log.Printf("🔒 HTTPS服务器运行在端口 %d", ffb.HTTPPort)
		} else {
			log.Printf("🌐 HTTP服务器运行在端口 %d", ffb.HTTPPort)
		}
		log.Printf("📦 最大文件大小限制: %.1f GiB", float64(ffb.MaxFileSize)/(1024*1024*1024))

		var err error
		if useTLS {
			err = httpServer.ListenAndServeTLS(ffb.TLSCertFile, ffb.TLSKeyFile)
		} else {
			err = httpServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP服务器错误: %v", err)
		}
	}()

	go func() {
		log.Printf("🔌 TCP服务器运行在端口 %d", ffb.TCPPort)
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ffb.isShuttingDown.Load() {
					break
				}
				log.Printf("TCP连接接受错误: %v", err)
				continue
			}
			go ffb.handleStreamConnection(conn)
		}
	}()

	<-ffb.ShutdownEvent
	ffb.isShuttingDown.Store(true)

	ffb.gracefulShutdown(httpServer, listener)
	return nil
}

// gracefulShutdown 关闭 HTTP / TCP 监听器并释放所有 in-flight 资源
func (ffb *FileFlowBridge) gracefulShutdown(httpServer *http.Server, listener net.Listener) {
	log.Println("🛑 开始优雅关闭...")
	ffb.isShuttingDown.Store(true)

	// 先收集 token 再在锁外清理，避免与 removeFileResources 内部加锁死锁
	ffb.mu.RLock()
	tokens := make([]string, 0, len(ffb.activeStreams))
	for authToken := range ffb.activeStreams {
		tokens = append(tokens, authToken)
	}
	ffb.mu.RUnlock()

	for _, authToken := range tokens {
		ffb.removeFileResources(authToken)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP服务器关闭错误: %v", err)
	}
	if listener != nil {
		listener.Close()
	}
	log.Println("✅ 服务器关闭完成")
}

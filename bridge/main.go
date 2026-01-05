package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"flag"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// 文件元数据结构
type FileMetadata struct {
	Filename		 string	`json:"filename"`
	OriginalFilename string	`json:"original_filename"`
	Size			 int64	 `json:"size"`
	Status		   string	`json:"status"`
	ClientIP		 string	`json:"client_ip"`
	AuthToken		string	`json:"auth_token"`
	RegisteredAt	 time.Time `json:"registered_at"`
	ExpiresAt		time.Time `json:"expires_at"`
	StreamStarted	time.Time `json:"stream_started,omitempty"`
	ClientAddress	string	`json:"client_address,omitempty"`
}

// 服务器统计信息
type ServerStats struct {
	StartTime		 time.Time `json:"start_time"`
	FilesRegistered   int	   `json:"files_registered"`
	FilesTransferred  int	   `json:"files_transferred"`
	BytesTransferred  int64	 `json:"bytes_transferred"`
	ActiveConnections int	   `json:"active_connections"`
	PeakConnections   int	   `json:"peak_connections"`
}

// TCP连接信息
type StreamConnection struct {
	Reader io.Reader
	Writer io.Writer
	Conn   net.Conn
}

// 文件流桥服务器
type FileFlowBridge struct {
	HTTPPort	  	int
	TCPPort	   	int
	MaxFileSize   	int64
	TokenLength		int
	ShutdownEvent 	chan struct{}

	fileRegistry	  map[string]*FileMetadata
	activeStreams	 map[string]*StreamConnection
	downloadCompleted map[string]bool
	serverStats	   ServerStats
	isShuttingDown	bool

	// 用于同步访问共享资源
	mu sync.RWMutex
}


// 处理流错误
func (ffb *FileFlowBridge) handleStreamError(authToken string, err error, conn net.Conn) {
	if err == io.EOF {
		log.Printf("连接正常关闭: %s", authToken)
		return
	}

	if netErr, ok := err.(net.Error); ok {
		if netErr.Timeout() {
			log.Printf("连接超时: %s - %v", authToken, netErr)
			// 尝试重置连接
			if conn != nil {
				conn.SetReadDeadline(time.Time{})
			}
		} else {
			log.Printf("网络错误: %s - %v", authToken, netErr)
		}
	} else {
		log.Printf("流错误: %s - %v", authToken, err)
	}

	// 清理资源
	ffb.mu.Lock()
	defer ffb.mu.Unlock()

	if _, exists := ffb.activeStreams[authToken]; exists {
		delete(ffb.activeStreams, authToken)
	}
}


// 检查连接状态
func (ffb *FileFlowBridge) checkConnectionHealth(conn *StreamConnection) bool {
	if conn == nil || conn.Conn == nil {
		return false
	}

	// // 尝试发送一个小数据包测试连接
	// _, err := conn.Conn.Write([]byte{0})
	// if err != nil {
	//	 return false
	// }

	return true
}

// 初始化服务器
func NewFileFlowBridge(httpPort, tcpPort int, maxFileSize int64, tokenLength int) *FileFlowBridge {
	return &FileFlowBridge{
		HTTPPort:	  httpPort,
		TCPPort:	   tcpPort,
		MaxFileSize:   maxFileSize,
		TokenLength:	  tokenLength,
		ShutdownEvent: make(chan struct{}),
		fileRegistry:  make(map[string]*FileMetadata),
		activeStreams: make(map[string]*StreamConnection),
		downloadCompleted: make(map[string]bool),
		serverStats: ServerStats{
			StartTime: time.Now(),
		},
	}
}

// 生成指定长度的随机字符串
func (ffb *FileFlowBridge) createNewID() string {
	if ffb.TokenLength < 6 || ffb.TokenLength > 32 {
		return uuid.New().String()
	}
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	ret := make([]byte, ffb.TokenLength)
	for i := 0; i < ffb.TokenLength; i++ {
		num, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		ret[i] = charset[num.Int64()]
	}
	return string(ret)
}

// 启动服务器
func (ffb *FileFlowBridge) StartServer() error {
	// 启动HTTP服务器
	router := mux.NewRouter()
	router.HandleFunc("/register", ffb.handleFileRegistration).Methods("POST")
	router.HandleFunc("/download/{auth_token}", ffb.handleFileDownload)
	router.HandleFunc("/download/{auth_token}/{filename}", ffb.handleFileDownloadWithName)
	router.HandleFunc("/status/{auth_token}", ffb.handleStatusCheck)
	router.HandleFunc("/stats", ffb.handleServerStats)
	router.HandleFunc("/health", ffb.handleHealthCheck)

	// 配置CORS
	corsMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}

	httpServer := &http.Server{
		Addr:	fmt.Sprintf(":%d", ffb.HTTPPort),
		Handler: corsMiddleware(router),
	}

	// 启动TCP服务器
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", ffb.TCPPort))
	if err != nil {
		return fmt.Errorf("TCP服务器启动失败: %v", err)
	}

	// 启动清理任务
	go ffb.cleanupResources()

	// 启动HTTP服务器
	go func() {
		log.Printf("🌐 HTTP服务器运行在端口 %d", ffb.HTTPPort)
		log.Printf("📦 最大文件大小限制: %.1f GiB", float64(ffb.MaxFileSize)/(1024*1024*1024))

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP服务器错误: %v", err)
		}
	}()

	// 处理TCP连接
	go func() {
		log.Printf("🔌 TCP服务器运行在端口 %d", ffb.TCPPort)
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ffb.isShuttingDown {
					break
				}
				log.Printf("TCP连接接受错误: %v", err)
				continue
			}

			go ffb.handleStreamConnection(conn)
		}
	}()

	// 等待关闭信号
	<-ffb.ShutdownEvent
	ffb.isShuttingDown = true

	// 优雅关闭
	ffb.gracefulShutdown(httpServer, listener)
	return nil
}

// 处理流连接
func (ffb *FileFlowBridge) handleStreamConnection(conn net.Conn) {
	isHandover := false
	defer func() {
		if !isHandover {
			conn.Close()
			log.Printf("🔌 未完成握手的连接已释放: %s", conn.RemoteAddr().String())
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

	log.Printf("🔗 新的流连接来自 %s", conn.RemoteAddr().String())

	// 设置TCP KeepAlive
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	// 设置读取超时（仅用于元数据读取）
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	// 读取元数据
	reader := bufio.NewReader(conn)
	metadataRaw, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("无效的连接元数据: %v", err)
		return
	}

	// 解析元数据
	var metadata map[string]string
	if err := json.Unmarshal([]byte(metadataRaw), &metadata); err != nil {
		log.Printf("元数据解析错误: %v", err)
		return
	}

	authToken := metadata["auth_token"]

	// 验证连接 - 修复重复声明问题
	valid := ffb.validateStreamConnection(authToken)
	if !valid {
		log.Printf("⛔ 无效的连接尝试: %s", authToken)
		conn.Write([]byte("INVALID_CONNECTION\n"))
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

	// 取消读取超时（重要修改）
	conn.SetReadDeadline(time.Time{})

	// 存储流连接
	streamConn := &StreamConnection{
		Reader: reader,
		Writer: conn,
		Conn:   conn,
	}

	ffb.mu.Lock()
	ffb.activeStreams[authToken] = streamConn
	ffb.mu.Unlock()

	log.Printf("✅ 流隧道已建立: %s (token_id: %s)", fileName, authToken)

	// 发送准备确认
	conn.Write([]byte("STREAM_READY\n"))

	// 保持连接活跃（使用TCP KeepAlive替代应用层心跳）
	isHandover = true
	go ffb.monitorConnectionHealth(streamConn, authToken)
}

// 验证流连接
func (ffb *FileFlowBridge) validateStreamConnection(authToken string) bool {
	ffb.mu.RLock()
	defer ffb.mu.RUnlock()

	metadata, exists := ffb.fileRegistry[authToken]
	if !exists {
		return false
	}

	// 检查认证令牌
	if metadata.AuthToken != authToken {
		return false
	}

	// 检查文件状态
	if metadata.Status != "registered" {
		return false
	}

	// 检查过期时间
	if metadata.ExpiresAt.Before(time.Now()) {
		return false
	}

	// 检查是否已经下载完成
	if ffb.downloadCompleted[authToken] {
		return false
	}

	return true
}


// 监控连接健康状态
func (ffb *FileFlowBridge) monitorConnectionHealth(conn *StreamConnection, authToken string) {
	ticker := time.NewTicker(30 * time.Second) 
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
				log.Printf("📭 文件 %s (token_id: %s) 传输结束或资源已释放，停止监控", filename, authToken)
				return
			}

			isBroken := false
			if tcpConn, ok := conn.Conn.(*net.TCPConn); ok {
				rawConn, err := tcpConn.SyscallConn()
				if err == nil {
					rawConn.Control(func(fd uintptr) {
						// 1. 底层探测：尝试窥视缓冲区 (Peek)
						// MSG_PEEK: 不取走数据; MSG_DONTWAIT: 非阻塞
						var buf [1]byte
						n, _, recvErr := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)

						// 2. 获取 TCP 状态
						var info syscall.TCPInfo
						size := uint32(unsafe.Sizeof(info))
						ptr := uintptr(unsafe.Pointer(&info))
						_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, 
							syscall.IPPROTO_TCP, syscall.TCP_INFO, ptr, uintptr(unsafe.Pointer(&size)), 0)

						if n == 0 && recvErr == nil {
							isBroken = true
							return
						}


						if errno == 0 && info.State != 1 {
							isBroken = true
							return
						}

						if recvErr != nil && recvErr != syscall.EAGAIN && recvErr != syscall.EWOULDBLOCK {
							isBroken = true
							return
						}
					})
				}
			}

			if isBroken {
				log.Printf("🔌 检测到物理连接已断开，正在清理: %s (token_id: %s)", filename, authToken)
				ffb.removeFileResources(authToken)
				return
			}

			log.Printf("📡 连接健康检查: %s (token_id: %s) - 活跃中", filename, authToken)

		case <-ffb.ShutdownEvent:
			log.Printf("🛑 服务器关闭，停止监控: %s (token_id: %s)", filename, authToken)
			return
		}
	}
}


func getScheme(r *http.Request) string {
	// 检查反向代理头
	if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
		return scheme
	}
	if scheme := r.Header.Get("X-Forwarded-Scheme"); scheme != "" {
		return scheme
	}
	// 默认基于TLS判断
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// 获取正确的主机名（去除端口号）
func getHost(r *http.Request) string {
	host := r.Host
	// 移除端口号部分
	if strings.Contains(host, ":") {
		return strings.Split(host, ":")[0]
	}
	return host
}

// 处理文件注册
func (ffb *FileFlowBridge) handleFileRegistration(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		http.Error(w, "无效的请求体", http.StatusBadRequest)
		return
	}

	var data struct {
		Filename string `json:"filename"`
		Size	 int64  `json:"size"`
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "无效的JSON数据", http.StatusBadRequest)
		return
	}

	// 验证输入
	if data.Filename == "" {
		http.Error(w, "文件名是必需的", http.StatusBadRequest)
		return
	}

	if data.Size > ffb.MaxFileSize {
		http.Error(w, "文件大小超过限制", http.StatusRequestEntityTooLarge)
		return
	}

	// 生成文件ID和认证令牌
	authToken := ffb.createNewID()
	clientIP := r.RemoteAddr

	// 存储文件元数据
	metadata := &FileMetadata{
		Filename:		 data.Filename,
		OriginalFilename: data.Filename,
		Size:			 data.Size,
		Status:		   "registered",
		ClientIP:		 clientIP,
		AuthToken:		authToken,
		RegisteredAt:	 time.Now(),
		ExpiresAt:		time.Now().Add(2 * time.Hour),
	}

	ffb.mu.Lock()
	ffb.fileRegistry[authToken] = metadata
	ffb.serverStats.FilesRegistered++
	ffb.mu.Unlock()

	scheme := getScheme(r)
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var portStr string
	if scheme == "https" || r.Header.Get("X-Forwarded-Proto") == "https" {
		// 隐藏端口，因为 Caddy 已经处理了 443 -> 8000 的映射
		portStr = "" 
	} else {
		// 本地测试或非加密访问，显示程序真实的监听端口
		portStr = fmt.Sprintf(":%d", ffb.HTTPPort)
	}
	safeFilename := url.PathEscape(data.Filename)

	// 生成响应
	responseData := map[string]interface{}{
		"auth_token": authToken,
		"tcp_endpoint": map[string]interface{}{
			"host": host, 
			"port": ffb.TCPPort,
		},
		"download_url": 		fmt.Sprintf("%s://%s%s/download/%s/%s", scheme, host, portStr, authToken, safeFilename),
		// "direct_download_url": fmt.Sprintf("%s://%s%d/download/%s", scheme, host, ffb.HTTPPort, authToken),
		// "status_url":		  fmt.Sprintf("%s://%s%d/status/%s", scheme, host, ffb.HTTPPort, authToken),
		"expires_at":		  	metadata.ExpiresAt.Format(time.RFC3339),
		"original_filename":   	data.Filename,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responseData)

	log.Printf("📝 文件注册成功: %s (token_id: %s)", data.Filename, authToken)
}

// 处理文件下载
func (ffb *FileFlowBridge) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	authToken := vars["auth_token"]
	ffb.handleDownloadRequest(w, r, authToken)

}

// 处理带文件名的下载
func (ffb *FileFlowBridge) handleFileDownloadWithName(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	authToken := vars["auth_token"]
	ffb.handleDownloadRequest(w, r, authToken)
}

// 处理下载请求的核心逻辑
func (ffb *FileFlowBridge) handleDownloadRequest(w http.ResponseWriter, r *http.Request, authToken string) {
	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	completed := ffb.downloadCompleted[authToken]
	ffb.mu.RUnlock()

	if !exists || completed {
		http.Error(w, "文件不存在或已下载", http.StatusNotFound)
		return
	}

	if completed {
		http.Error(w, "文件下载已完成，资源已释放", http.StatusGone)
		return
	}

	defer ffb.removeFileResources(authToken)

	// 检查文件状态 - 允许"registered"状态的文件开始下载
	if metadata.Status != "streaming" && metadata.Status != "registered" {
		http.Error(w, "文件尚未准备好下载", http.StatusServiceUnavailable)
		return
	}

	// 检查流是否可用，如果不可用则等待一段时间
	var streamConn *StreamConnection
	var exists1 bool

	// 等待最多10秒让流连接建立
	for i := 0; i < 20; i++ {
		ffb.mu.RLock()
		streamConn, exists1 = ffb.activeStreams[authToken]
		ffb.mu.RUnlock()

		if exists1 {
			break
		}

		time.Sleep(500 * time.Millisecond)
	}

	if !exists1 {
		log.Printf("⚠️ 文件源不可用，可能流连接尚未建立: %s", authToken)
		http.Error(w, "文件源不可用", http.StatusServiceUnavailable)
		return
	}

	// 准备响应头
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, metadata.OriginalFilename))
	w.Header().Set("X-FileFlow-FileID", authToken)
	w.Header().Set("X-FileFlow-Original-Filename", metadata.OriginalFilename)

	if metadata.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(metadata.Size, 10))
	}

	// 开始传输
	log.Printf("⬇️ 开始下载: %s (token_id: %s)", metadata.OriginalFilename, authToken)

	startTime := time.Now()
	var totalTransferred int64
	var localChunk int64
	buf := make([]byte, 256*1024)

	// 设置合理的读取超时（5分钟）
	if conn := streamConn.Conn; conn != nil {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	}

	for {
		n, err := streamConn.Reader.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}

			// 检查是否是超时错误
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("⚠️ 读取超时，但继续尝试: %v", err)

				// 重置超时并继续尝试
				if conn := streamConn.Conn; conn != nil {
					conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
				}
				continue
			}

			ffb.handleStreamError(authToken, err, streamConn.Conn)
			break
		}

		if n == 0 {
			break
		}

		// 写入响应
		if _, err := w.Write(buf[:n]); err != nil {
			log.Printf("❌ 客户端断开连接: %v", err)
			break
		}

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		totalTransferred += int64(n)
		localChunk += int64(n)

		if localChunk >= 10*1024*1024 {
			ffb.mu.Lock()
			ffb.serverStats.BytesTransferred += localChunk
			ffb.mu.Unlock()
			localChunk = 0
		}

		// 每次成功读取后重置超时
		if conn := streamConn.Conn; conn != nil {
			conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		}
	}

	// 传输完成
	transferTime := time.Since(startTime).Seconds()
	ffb.mu.Lock()
	ffb.serverStats.FilesTransferred++
	ffb.serverStats.BytesTransferred += localChunk
	ffb.downloadCompleted[authToken] = true
	ffb.mu.Unlock()
	if transferTime > 0 {
		sizeMiB := float64(totalTransferred) / (1024 * 1024)
		speedValue := float64(totalTransferred) / transferTime / 1024
		speedUnit := "KiB/s"
		if speedValue >= 1024 {
			speedValue /= 1024
			speedUnit = "MiB/s"
		}

		log.Printf("✅ 传输完成: %s (token_id: %s), 大小: %.2f MiB, 耗时: %.2fs, 速度: %.2f %s",
			metadata.OriginalFilename, 
			authToken, 
			sizeMiB, 
			transferTime, 
			speedValue, 
			speedUnit,
		)

		if conn, exists := ffb.activeStreams[authToken]; exists {
			if conn.Conn != nil {
				conn.Conn.Close()
				log.Printf("🔌 关闭已完成文件的TCP连接: %s (token_id: %s)", metadata.OriginalFilename, authToken)
			}
			delete(ffb.activeStreams, authToken)
		}

		log.Printf("🏁 文件标记为已完成: %s (token_id: %s)", metadata.OriginalFilename, authToken)
	}
}

// 检查文件状态
func (ffb *FileFlowBridge) handleStatusCheck(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	authToken := vars["auth_token"]

	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	completed := ffb.downloadCompleted[authToken]
	ffb.mu.RUnlock()

	if !exists {
		http.Error(w, "文件未找到", http.StatusNotFound)
		return
	}

	// 创建响应数据
	responseData := map[string]interface{}{
		"filename":		  metadata.Filename,
		"original_filename": metadata.OriginalFilename,
		"size":			  metadata.Size,
		"status":			metadata.Status,
		"client_ip":		 metadata.ClientIP,
		"registered_at":	 metadata.RegisteredAt.Format(time.RFC3339),
		"expires_at":		metadata.ExpiresAt.Format(time.RFC3339),
		"download_completed": completed,
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

// 获取服务器统计信息
func (ffb *FileFlowBridge) handleServerStats(w http.ResponseWriter, r *http.Request) {
	ffb.mu.RLock()
	stats := map[string]interface{}{
		"status":			 	"running",
		"uptime":				time.Since(ffb.serverStats.StartTime).Seconds(),
		"files_registered":  	ffb.serverStats.FilesRegistered,
		"files_transferred": 	ffb.serverStats.FilesTransferred,
		"bytes_transferred": 	ffb.serverStats.BytesTransferred,
		"active_connections":	ffb.serverStats.ActiveConnections,
		"peak_connections":  	ffb.serverStats.PeakConnections,
		"registered_files": 	len(ffb.fileRegistry),
		"active_streams":   	len(ffb.activeStreams),
		"completed_downloads": 	len(ffb.downloadCompleted),
	}
	ffb.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// 健康检查
func (ffb *FileFlowBridge) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"status":	"healthy",
		"timestamp": time.Now().Format(time.RFC3339),
		"version":   "1.0.0",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// 清理资源
func (ffb *FileFlowBridge) cleanupResources() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if ffb.isShuttingDown {
				return
			}

			currentTime := time.Now()
			var expiredFiles []string

			ffb.mu.RLock()
			for authToken, metadata := range ffb.fileRegistry {
				if metadata.ExpiresAt.Before(currentTime) {
					expiredFiles = append(expiredFiles, authToken)
				}
			}
			ffb.mu.RUnlock()

			for _, authToken := range expiredFiles {
				ffb.removeFileResources(authToken)
				log.Printf("🧹 清理过期文件: %s", authToken)
			}

		case <-ffb.ShutdownEvent:
			return
		}
	}
}

// 移除文件资源
func (ffb *FileFlowBridge) removeFileResources(authToken string) {
	ffb.mu.Lock()
	defer ffb.mu.Unlock()

	// 移除注册信息
	delete(ffb.fileRegistry, authToken)

	// 关闭TCP连接
	if streamConn, exists := ffb.activeStreams[authToken]; exists {
		if streamConn.Conn != nil {
			streamConn.Conn.Close()
		}
		delete(ffb.activeStreams, authToken)
	}

	// 移除下载完成标记
	delete(ffb.downloadCompleted, authToken)
}

// 优雅关闭
func (ffb *FileFlowBridge) gracefulShutdown(httpServer *http.Server, listener net.Listener) {
	log.Println("🛑 开始优雅关闭...")
	ffb.isShuttingDown = true

	// 关闭所有TCP连接
	ffb.mu.Lock()
	for authToken := range ffb.activeStreams {
		ffb.removeFileResources(authToken)
	}
	ffb.mu.Unlock()

	// 关闭HTTP服务器
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP服务器关闭错误: %v", err)
	}

	// 关闭TCP监听器
	if listener != nil {
		listener.Close()
	}

	log.Println("✅ 服务器关闭完成")
}

// 检测是否在容器中运行
func isRunningInContainer() bool {
	// 检查常见的容器指示文件和环境变量
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	if _, err := os.Stat("/proc/1/cgroup"); err == nil {
		if content, err := os.ReadFile("/proc/1/cgroup"); err == nil {
			if contains(string(content), "docker") || contains(string(content), "kubepods") {
				return true
			}
		}
	}

	containerVars := []string{"KUBERNETES_SERVICE_HOST", "CONTAINER", "DOCKER_CONTAINER"}
	for _, envVar := range containerVars {
		if os.Getenv(envVar) != "" {
			return true
		}
	}

	return false
}

// 配置日志
func setupLogging() {
	logLevel := os.Getenv("FFB_LOG_LEVEL")
	if logLevel == "" {
		logLevel = "INFO"
	}

	logPath := os.Getenv("FFB_LOG_PATH")
	if logPath == "" {
		logPath = "fileflow_bridge.log"
	}

	// 如果在容器中运行，只输出到控制台
	if isRunningInContainer() {
		fmt.Println("🐳 检测到容器环境，日志仅输出到控制台")
	} else {
		// 确保日志目录存在
		logDir := filepath.Dir(logPath)
		if logDir != "" {
			os.MkdirAll(logDir, 0755)
		}

		// 创建日志文件
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			log.SetOutput(io.MultiWriter(os.Stdout, logFile))
			fmt.Printf("📝 日志文件: %s\n", logPath)
		} else {
			log.SetOutput(os.Stdout)
		}
	}
}

// 辅助函数：检查字符串是否包含子串
func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[:len(substr)] == substr
}

// 辅助函数：获取整数环境变量，不存在则返回默认值
func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

// 辅助函数：获取 int64 环境变量
func getEnvInt64(key string, defaultVal int64) int64 {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return defaultVal
}

// 主函数
func main() {
	fmt.Println("🌊 FileFlow Bridge - 文件流桥接服务器")
	fmt.Println("==================================================")

	// 设置日志
	setupLogging()

	// 获取环境变量配置
	defaultHTTPPort := getEnvInt("FFB_HTTP_PORT", 8000)
	defaultTCPPort := getEnvInt("FFB_TCP_PORT", 8888)
	defaultMaxFileSize := getEnvInt64("FFB_MAX_FILE_SIZE", 100)
	defaultTokenLength := getEnvInt("FFB_TOKEN_LEN", 8)

	httpPort := flag.Int("http-port", defaultHTTPPort, "HTTP 服务器端口")
	tcpPort := flag.Int("tcp-port", defaultTCPPort, "TCP 流服务器端口")
	maxFileSize := flag.Int64("max-file-size", defaultMaxFileSize, "最大允许文件大小 (GiB)")
	tokenLength := flag.Int("token-len", defaultTokenLength, "随机token长度，默认8位")

	flag.Parse()

	finalTokenLen := tokenLength
	calcBytes := (*maxFileSize) * 1024 * 1024 * 1024
	maxFileSizeBytes := &calcBytes
	if *finalTokenLen < 6 || *finalTokenLen > 32 {
		log.Printf("⚠️ 警告: ID 长度 %d 不在有效范围 (6-32)，将恢复默认值 8", *finalTokenLen)
		defaultVal := 8
		finalTokenLen = &defaultVal
	}

	// 创建服务器实例
	server := NewFileFlowBridge(*httpPort, *tcpPort, *maxFileSizeBytes, *finalTokenLen)

	// 启动服务器
	if err := server.StartServer(); err != nil {
		log.Fatalf("💥 服务器启动失败: %v", err)
	}

	fmt.Println("👋 服务器已停止")
}

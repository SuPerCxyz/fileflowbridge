package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"html"
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
	"github.com/gorilla/websocket"
)

// 文件元数据结构
type FileMetadata struct {
	Filename         string    `json:"filename"`
	OriginalFilename string    `json:"original_filename"`
	Size             int64     `json:"size"`
	Status           string    `json:"status"`
	ClientIP         string    `json:"client_ip"`
	AuthToken        string    `json:"auth_token"`
	RegisteredAt     time.Time `json:"registered_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	StreamStarted    time.Time `json:"stream_started,omitempty"`
	ClientAddress    string    `json:"client_address,omitempty"`
}

// 服务器统计信息
type ServerStats struct {
	StartTime         time.Time `json:"start_time"`
	FilesRegistered   int       `json:"files_registered"`
	FilesTransferred  int       `json:"files_transferred"`
	BytesTransferred  int64     `json:"bytes_transferred"`
	ActiveConnections int       `json:"active_connections"`
	PeakConnections   int       `json:"peak_connections"`
}

// TCP连接信息
type StreamConnection struct {
	Reader io.Reader
	Writer io.Writer
	Conn   net.Conn
}

// 用于从channel读取数据的Reader
type ChannelReader struct {
	dataChan <-chan []byte
	buffer   []byte
	index    int
	done     chan bool
}

// 实现io.Reader接口的Read方法
func (cr *ChannelReader) Read(p []byte) (n int, err error) {
	for {
		// 如果有缓冲数据，先使用缓冲数据
		if cr.buffer != nil && cr.index < len(cr.buffer) {
			// 计算可以复制的字节数
			remaining := len(cr.buffer) - cr.index
			toCopy := len(p)
			if toCopy > remaining {
				toCopy = remaining
			}

			copy(p, cr.buffer[cr.index:cr.index+toCopy])
			cr.index += toCopy
			return toCopy, nil
		}

		// 从channel获取新数据
		data, ok := <-cr.dataChan
		if !ok {
			// channel已关闭，表示没有更多数据
			return 0, io.EOF
		}

		// 更新缓冲区
		cr.buffer = data
		cr.index = 0
	}
}

// 全局WebSocket升级器
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// 允许来自相同主机的连接
		return true
	},
}

const (
	tcpStreamReadTimeout     = 2 * time.Second
	connectionHealthInterval = 2 * time.Second
	completedDownloadTTL     = 1 * time.Minute
)

// 文件流桥服务器
type FileFlowBridge struct {
	HTTPPort      int
	TCPPort       int
	MaxFileSize   int64
	TokenLength   int
	ShutdownEvent chan struct{}

	fileRegistry        map[string]*FileMetadata
	activeStreams       map[string]interface{} // 使用interface{}以支持多种连接类型
	downloadCompleted   map[string]bool
	downloadCompletedAt map[string]time.Time
	serverStats         ServerStats
	isShuttingDown      bool

	// 用于同步访问共享资源
	mu sync.RWMutex
}

// 流连接接口
type StreamConnectionInterface interface {
	io.Reader
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
		HTTPPort:            httpPort,
		TCPPort:             tcpPort,
		MaxFileSize:         maxFileSize,
		TokenLength:         tokenLength,
		ShutdownEvent:       make(chan struct{}),
		fileRegistry:        make(map[string]*FileMetadata),
		activeStreams:       make(map[string]interface{}),
		downloadCompleted:   make(map[string]bool),
		downloadCompletedAt: make(map[string]time.Time),
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

	// API路由
	router.HandleFunc("/register", ffb.handleFileRegistration).Methods("POST")
	router.HandleFunc("/upload/{auth_token}", ffb.handleFileUpload).Methods("POST")
	router.HandleFunc("/ws/{auth_token}", ffb.handleWebSocketConnection).Methods("GET")
	router.HandleFunc("/download/{auth_token}", ffb.handleFileDownload)
	router.HandleFunc("/download/{auth_token}/{filename}", ffb.handleFileDownloadWithName)
	router.HandleFunc("/status/{auth_token}", ffb.handleStatusCheck)
	router.HandleFunc("/stats", ffb.handleServerStats)
	router.HandleFunc("/health", ffb.handleHealthCheck)

	// WebSocket路由
	router.HandleFunc("/ws/{auth_token}", ffb.handleWebSocketConnection).Methods("GET")

	// 配置WebSocket升级器
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			// 允许来自相同主机的连接
			return true
		},
	}

	// 添加静态文件服务 - 放在最后以避免覆盖API路由
	staticDir := "./static"
	if _, err := os.Stat(staticDir); err == nil {
		// 如果static目录存在，则提供静态文件服务
		staticFS := http.FileServer(http.Dir(staticDir))

		// 特殊处理根路径，返回index.html
		router.HandleFunc("/", ffb.handleRootPage)

		// 提供其他静态文件服务，但不覆盖API路由
		router.PathPrefix("/").Handler(staticFS).Methods("GET")
	}

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
		Addr:    fmt.Sprintf(":%d", ffb.HTTPPort),
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

// 处理根页面
func (ffb *FileFlowBridge) handleRootPage(w http.ResponseWriter, r *http.Request) {
	// 返回index.html
	http.ServeFile(w, r, "./static/index.html")
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
		Size     int64  `json:"size"`
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
		Filename:         data.Filename,
		OriginalFilename: data.Filename,
		Size:             data.Size,
		Status:           "registered",
		ClientIP:         clientIP,
		AuthToken:        authToken,
		RegisteredAt:     time.Now(),
		ExpiresAt:        time.Now().Add(2 * time.Hour),
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
		"download_url": fmt.Sprintf("%s://%s%s/download/%s/%s", scheme, host, portStr, authToken, safeFilename),
		// "direct_download_url": fmt.Sprintf("%s://%s%d/download/%s", scheme, host, ffb.HTTPPort, authToken),
		// "status_url":		  fmt.Sprintf("%s://%s%d/status/%s", scheme, host, ffb.HTTPPort, authToken),
		"expires_at":        metadata.ExpiresAt.Format(time.RFC3339),
		"original_filename": data.Filename,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responseData)

	log.Printf("📝 文件注册成功: %s (token_id: %s)", data.Filename, authToken)
}

// 处理文件上传
func (ffb *FileFlowBridge) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	authToken := vars["auth_token"]

	// 验证文件令牌
	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	ffb.mu.RUnlock()

	if !exists {
		http.Error(w, "无效的认证令牌", http.StatusUnauthorized)
		return
	}

	// 验证请求内容类型
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		http.Error(w, "请求必须是multipart/form-data格式", http.StatusBadRequest)
		return
	}

	// 限制上传大小
	r.ParseMultipartForm(32 << 20) // 32MB

	// 获取上传的文件
	file, _, err := r.FormFile("file")
	if err != nil {
		log.Printf("获取上传文件失败: %v", err)
		http.Error(w, "获取上传文件失败", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 更新文件状态
	ffb.mu.Lock()
	if ffb.fileRegistry[authToken] != nil {
		ffb.fileRegistry[authToken].Status = "streaming"
		ffb.fileRegistry[authToken].StreamStarted = time.Now()
	}
	ffb.mu.Unlock()

	// 创建一个通道来处理数据流
	dataChan := make(chan []byte, 10)

	// 启动goroutine读取上传的文件数据
	go func() {
		defer close(dataChan)
		buffer := make([]byte, 32*1024) // 32KB buffer
		for {
			// 检查下载是否已完成
			ffb.mu.RLock()
			completed := ffb.downloadCompleted[authToken]
			ffb.mu.RUnlock()

			if completed {
				log.Printf("⚠️ 下载已完成，停止上传: %s", authToken)
				return
			}

			n, err := file.Read(buffer)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buffer[:n])
				select {
				case dataChan <- data:
				case <-time.After(5 * time.Second): // 减少超时时间以快速响应
					log.Printf("数据通道超时，可能下载端已断开: %s", authToken)
					return
				}
			}
			if err != nil {
				break
			}
		}
	}()

	// 创建一个reader来从channel读取数据
	reader := &ChannelReader{
		dataChan: dataChan,
		buffer:   nil,
		index:    0,
		done:     nil,
	}

	// 将reader包装为StreamConnection
	streamConn := &StreamConnection{
		Reader: reader,
		Writer: nil,
		Conn:   nil,
	}

	ffb.mu.Lock()
	ffb.activeStreams[authToken] = streamConn
	ffb.mu.Unlock()

	// 等待下载完成
	downloadWaitStart := time.Now()
	for {
		ffb.mu.RLock()
		completed := ffb.downloadCompleted[authToken]
		_, exists := ffb.activeStreams[authToken]
		ffb.mu.RUnlock()

		if completed || !exists {
			break
		}

		if time.Since(downloadWaitStart) > 10*time.Minute { // 增加超时时间
			break // 下载超时
		}

		time.Sleep(500 * time.Millisecond)
	}

	// 不要在这里删除流连接，让handleDownloadRequest完成后删除
	log.Printf("✅ 文件上传处理完成: %s (token_id: %s)", metadata.OriginalFilename, authToken)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success": true, "message": "文件上传处理完成"}`)
}

// WebSocket流连接
type WebSocketStreamConnection struct {
	Conn      *websocket.Conn
	Buffer    []byte
	Index     int
	Mutex     sync.Mutex
	DataChan  chan []byte
	CloseChan chan struct{}
}

func (wsConn *WebSocketStreamConnection) writeJSON(v interface{}) error {
	wsConn.Mutex.Lock()
	defer wsConn.Mutex.Unlock()

	if wsConn.Conn == nil {
		return io.EOF
	}

	return wsConn.Conn.WriteJSON(v)
}

func (wsConn *WebSocketStreamConnection) writeMessage(messageType int, data []byte) error {
	wsConn.Mutex.Lock()
	defer wsConn.Mutex.Unlock()

	if wsConn.Conn == nil {
		return io.EOF
	}

	return wsConn.Conn.WriteMessage(messageType, data)
}

// 实现io.Reader接口，从WebSocket读取数据
func (wsConn *WebSocketStreamConnection) Read(p []byte) (n int, err error) {
	// 如果有缓冲数据，先使用缓冲数据
	if wsConn.Buffer != nil && wsConn.Index < len(wsConn.Buffer) {
		remaining := len(wsConn.Buffer) - wsConn.Index
		toCopy := len(p)
		if toCopy > remaining {
			toCopy = remaining
		}

		copy(p, wsConn.Buffer[wsConn.Index:wsConn.Index+toCopy])
		wsConn.Index += toCopy
		return toCopy, nil
	}

	// 从WebSocket连接读取新数据
	select {
	case data, ok := <-wsConn.DataChan:
		if !ok {
			// 通道已关闭，表示没有更多数据
			return 0, io.EOF
		}

		// 使用新数据作为缓冲
		wsConn.Buffer = data
		wsConn.Index = 0

		// 返回一部分数据
		remaining := len(wsConn.Buffer) - wsConn.Index
		toCopy := len(p)
		if toCopy > remaining {
			toCopy = remaining
		}

		copy(p, wsConn.Buffer[wsConn.Index:wsConn.Index+toCopy])
		wsConn.Index += toCopy
		return toCopy, nil
	case <-wsConn.CloseChan:
		return 0, io.EOF
	}
}

// 请求文件数据
func (ffb *FileFlowBridge) requestFileData(authToken string, offset, size int64) {
	// 向上传端请求特定偏移量和大小的数据块
	conn, exists := ffb.activeStreams[authToken]
	if !exists {
		log.Printf("找不到连接: %s", authToken)
		return
	}

	if wsConn, ok := conn.(*WebSocketStreamConnection); ok {
		request := map[string]interface{}{
			"command": "send_chunk",
			"offset":  offset,
			"size":    size,
		}

		err := wsConn.writeJSON(request)
		if err != nil {
			log.Printf("发送数据请求失败: %v", err)
		}
	}
}

// 处理WebSocket连接
func (ffb *FileFlowBridge) handleWebSocketConnection(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	authToken := vars["auth_token"]

	// 验证认证令牌
	ffb.mu.RLock()
	_, exists := ffb.fileRegistry[authToken]
	ffb.mu.RUnlock()

	if !exists {
		http.Error(w, "无效的认证令牌", http.StatusUnauthorized)
		return
	}

	// 升级到WebSocket连接
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		return
	}

	log.Printf("🔗 WebSocket连接已建立: %s", authToken)

	// 创建WebSocket流连接
	wsStreamConn := &WebSocketStreamConnection{
		Conn:      conn,
		Buffer:    nil,
		Index:     0,
		DataChan:  make(chan []byte, 50), // 增加缓冲区大小 to handle browser uploads
		CloseChan: make(chan struct{}),
	}

	// 更新文件状态
	ffb.mu.Lock()
	if ffb.fileRegistry[authToken] != nil {
		ffb.fileRegistry[authToken].Status = "streaming"
		ffb.fileRegistry[authToken].StreamStarted = time.Now()
	}
	ffb.activeStreams[authToken] = wsStreamConn
	ffb.mu.Unlock()

	// Send READY message to indicate connection is established
	err = wsStreamConn.writeMessage(websocket.TextMessage, []byte(`{"command":"READY"}`))
	if err != nil {
		log.Printf("发送READY消息失败: %v", err)
		conn.Close()
		return
	}

	// 启动数据读取协程
	go func() {
		defer close(wsStreamConn.DataChan)
		defer close(wsStreamConn.CloseChan)
		defer conn.Close()

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("WebSocket意外关闭: %v", err)
				} else {
					log.Printf("WebSocket连接关闭: %v", err)
				}
				break
			}

			if messageType == websocket.BinaryMessage {
				// 检查是否有活跃的下载连接
				ffb.mu.RLock()
				isDownloadCompleted := ffb.downloadCompleted[authToken]
				ffb.mu.RUnlock()

				if isDownloadCompleted {
					log.Printf("⚠️ 下载已完成，忽略上传数据: %s", authToken)
					continue
				}

				// 接收到文件数据，发送到数据通道
				data := make([]byte, len(message))
				copy(data, message)

				select {
				case wsStreamConn.DataChan <- data:
				case <-time.After(10 * time.Second): // 增加超时时间 to handle slower downloads
					log.Printf("WebSocket数据通道阻塞，可能下载端已断开: %s", authToken)
					return
				}
			} else if messageType == websocket.TextMessage {
				// 处理文本消息
				var msg map[string]interface{}
				if err := json.Unmarshal(message, &msg); err == nil {
					if cmd, ok := msg["command"]; ok {
						switch cmd {
						case "request_data":
							// 客户端请求数据块
							offset, _ := msg["offset"].(float64)
							size, _ := msg["size"].(float64)
							ffb.requestFileData(authToken, int64(offset), int64(size))
						case "download_started":
							// 下载端已开始下载
							log.Printf("下载已开始: %s", authToken)
						case "stop_upload":
							// 客户端请求停止上传 (when download is cancelled)
							log.Printf("客户端请求停止上传: %s", authToken)
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
		metadata, exists := ffb.fileRegistry[authToken]
		completed := ffb.downloadCompleted[authToken]
		shouldCleanup := exists && !completed && metadata.Status == "streaming"
		ffb.mu.RUnlock()

		if shouldCleanup {
			ffb.removeFileResources(authToken)
			log.Printf("🧹 已清理放弃的WebSocket上传: %s", authToken)
			return
		}

		ffb.mu.Lock()
		delete(ffb.activeStreams, authToken)
		ffb.mu.Unlock()
		log.Printf("🔗 WebSocket连接已关闭: %s", authToken)
	}()

	// 保持连接活跃
	<-wsStreamConn.CloseChan
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

// 检测请求是否来自浏览器
func isBrowserRequest(r *http.Request) bool {
	userAgent := strings.ToLower(r.UserAgent())
	acceptHeader := r.Header.Get("Accept")

	// 检查是否来自常见浏览器
	browserIndicators := []string{
		"mozilla/",
		"chrome/",
		"safari/",
		"firefox/",
		"edge/",
		"opera/",
	}

	isBrowser := false
	for _, indicator := range browserIndicators {
		if strings.Contains(userAgent, indicator) {
			isBrowser = true
			break
		}
	}

	// 检查Accept头是否为*/* (通常浏览器发送此值)
	acceptContainsStar := strings.Contains(acceptHeader, "*/*")

	// 检查是否来自命令行工具
	commandLineIndicators := []string{
		"wget",
		"curl",
		"lwp-request",
		"libwww-perl",
		"python-urllib",
		"java",
		"okhttp",
	}

	isCommandLine := false
	for _, indicator := range commandLineIndicators {
		if strings.Contains(userAgent, indicator) {
			isCommandLine = true
			break
		}
	}

	// 如果是浏览器请求并且不是命令行工具，则显示下载页面
	return isBrowser && !isCommandLine && acceptContainsStar
}

// 处理下载请求的核心逻辑
func (ffb *FileFlowBridge) handleDownloadRequest(w http.ResponseWriter, r *http.Request, authToken string) {
	ffb.mu.RLock()
	metadata, exists := ffb.fileRegistry[authToken]
	isCompleted := ffb.downloadCompleted[authToken]
	ffb.mu.RUnlock()

	if isCompleted {
		http.Error(w, "文件下载已完成，资源已释放", http.StatusGone)
		return
	}

	if !exists {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}

	// 如果是浏览器请求，返回下载页面而不是直接下载
	if isBrowserRequest(r) {
		ffb.serveDownloadPage(w, r, authToken, metadata)
		return
	}

	ffb.mu.Lock()
	metadata, exists = ffb.fileRegistry[authToken]
	isCompleted = ffb.downloadCompleted[authToken]
	if !exists {
		ffb.mu.Unlock()
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	if isCompleted {
		ffb.mu.Unlock()
		http.Error(w, "文件下载已完成，资源已释放", http.StatusGone)
		return
	}
	if metadata.Status == "downloading" {
		ffb.mu.Unlock()
		http.Error(w, "文件正在下载中", http.StatusConflict)
		return
	}
	if metadata.Status != "streaming" && metadata.Status != "registered" {
		ffb.mu.Unlock()
		http.Error(w, "文件尚未准备好下载", http.StatusServiceUnavailable)
		return
	}
	previousStatus := metadata.Status
	metadata.Status = "downloading"
	ffb.mu.Unlock()

	// 检查流是否可用，如果不可用则等待一段时间
	var streamConn interface{}
	var exists1 bool

	// 等待最多30秒让流连接建立 (增加等待时间以适应高并发场景)
	// 使用指数退避策略来减少锁竞争
	waitDuration := 100 * time.Millisecond
	maxRetries := 60 // 60 * 100ms = 6秒; 或者调整为 300 * 100ms = 30秒
	for i := 0; i < maxRetries; i++ {
		ffb.mu.RLock()
		streamConn, exists1 = ffb.activeStreams[authToken]
		ffb.mu.RUnlock()

		if exists1 {
			break
		}

		time.Sleep(waitDuration)
		// 可选：使用轻微的指数退避
		if i > 5 { // 前几次快速检查，之后稍微减慢
			waitDuration = 200 * time.Millisecond
		}
	}

	if !exists1 {
		ffb.mu.Lock()
		if meta, ok := ffb.fileRegistry[authToken]; ok && !ffb.downloadCompleted[authToken] {
			meta.Status = previousStatus
		}
		ffb.mu.Unlock()
		log.Printf("⚠️ 文件源不可用，可能流连接尚未建立: %s", authToken)
		http.Error(w, "文件源不可用", http.StatusServiceUnavailable)
		return
	}

	defer ffb.removeFileResources(authToken)

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

	// 根据连接类型进行处理
	var reader io.Reader
	var conn net.Conn

	if tcpConn, ok := streamConn.(*StreamConnection); ok {
		reader = tcpConn.Reader
		conn = tcpConn.Conn
		// 发送端异常断开时尽快回收资源，同时不影响正常连续传输
		if conn != nil {
			conn.SetReadDeadline(time.Now().Add(tcpStreamReadTimeout))
		}
	} else if wsConn, ok := streamConn.(*WebSocketStreamConnection); ok {
		reader = wsConn

		// 对于WebSocket连接，发送请求数据的命令
		// 这将触发上传端开始发送数据
		request := map[string]interface{}{
			"command": "download_started", // 通知上传端下载已开始
			"offset":  0,                  // 从开头开始
			"size":    metadata.Size,      // 请求整个文件
		}
		err := wsConn.writeJSON(request)
		if err != nil {
			log.Printf("发送下载开始通知失败: %v", err)
		} else {
			log.Printf("✅ 已通知上传端下载已开始: %s", authToken)
		}

		// 然后发送实际的数据请求
		request = map[string]interface{}{
			"command": "send_chunk",
			"offset":  0,             // 从开头开始
			"size":    metadata.Size, // 请求整个文件
		}
		err = wsConn.writeJSON(request)
		if err != nil {
			ffb.mu.Lock()
			if meta, ok := ffb.fileRegistry[authToken]; ok && !ffb.downloadCompleted[authToken] {
				meta.Status = previousStatus
			}
			ffb.mu.Unlock()
			log.Printf("发送数据请求失败: %v", err)
			http.Error(w, "无法从上传端请求数据", http.StatusInternalServerError)
			return
		}

		conn = nil // WebSocket连接不需要设置超时
	} else {
		http.Error(w, "未知的连接类型", http.StatusInternalServerError)
		return
	}

	// 检查客户端连接是否断开的函数
	clientClosed := func() bool {
		select {
		case <-r.Context().Done():
			return true
		default:
			return false
		}
	}

	for {
		// 检查客户端是否已断开连接
		if clientClosed() {
			log.Printf("❌ 客户端连接断开，停止传输: %s (token_id: %s)", metadata.OriginalFilename, authToken)
			// 通知上传端停止上传
			if wsConn, ok := streamConn.(*WebSocketStreamConnection); ok {
				stopRequest := map[string]interface{}{
					"command": "stop_upload",
				}
				// Attempt to send stop command but don't fail if connection is closed
				if wsConn.Conn != nil {
					err := wsConn.writeJSON(stopRequest)
					if err != nil {
						log.Printf("无法发送停止上传命令: %v", err)
					}
				}
			}
			break
		}

		n, err := reader.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}

			// 检查是否是超时错误
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("⚠️ 读取超时，判定传输中断: %v", err)
				break
			}

			ffb.handleStreamError(authToken, err, conn)
			break
		}

		if n == 0 {
			break
		}

		// 再次检查客户端是否已断开连接
		if clientClosed() {
			log.Printf("❌ 客户端连接断开，停止传输: %s (token_id: %s)", metadata.OriginalFilename, authToken)
			// 通知上传端停止上传
			if wsConn, ok := streamConn.(*WebSocketStreamConnection); ok {
				stopRequest := map[string]interface{}{
					"command": "stop_upload",
				}
				// Attempt to send stop command but don't fail if connection is closed
				if wsConn.Conn != nil {
					err := wsConn.writeJSON(stopRequest)
					if err != nil {
						log.Printf("无法发送停止上传命令: %v", err)
					}
				}
			}
			break
		}

		// 写入响应
		if _, err := w.Write(buf[:n]); err != nil {
			log.Printf("❌ 客户端断开连接: %v", err)
			// 通知上传端停止上传
			if wsConn, ok := streamConn.(*WebSocketStreamConnection); ok {
				stopRequest := map[string]interface{}{
					"command": "stop_upload",
				}
				// Attempt to send stop command but don't fail if connection is closed
				if wsConn.Conn != nil {
					err := wsConn.writeJSON(stopRequest)
					if err != nil {
						log.Printf("无法发送停止上传命令: %v", err)
					}
				}
			}
			break
		}

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		totalTransferred += int64(n)
		localChunk += int64(n)

		// 检查是否已传输完整个文件
		if totalTransferred >= metadata.Size {
			log.Printf("✅ 文件数据已全部传输: %s (token_id: %s)", metadata.OriginalFilename, authToken)
			break
		}

		if localChunk >= 10*1024*1024 {
			ffb.mu.Lock()
			ffb.serverStats.BytesTransferred += localChunk
			ffb.mu.Unlock()
			localChunk = 0
		}

		// 每次成功读取后重置超时
		if conn != nil {
			conn.SetReadDeadline(time.Now().Add(tcpStreamReadTimeout))
		}
	}

	transferCompleted := metadata.Size == 0 || totalTransferred >= metadata.Size
	transferTime := time.Since(startTime).Seconds()
	ffb.mu.Lock()
	ffb.serverStats.BytesTransferred += localChunk
	if transferCompleted {
		ffb.serverStats.FilesTransferred++
		ffb.downloadCompleted[authToken] = true
		ffb.downloadCompletedAt[authToken] = time.Now()
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

		log.Printf("✅ 传输完成: %s (token_id: %s), 大小: %.2f MiB, 耗时: %.2fs, 速度: %.2f %s",
			metadata.OriginalFilename,
			authToken,
			sizeMiB,
			transferTime,
			speedValue,
			speedUnit,
		)
	}

	if !transferCompleted {
		log.Printf("⚠️ 传输未完成: %s (token_id: %s), 已传输: %d / %d",
			metadata.OriginalFilename,
			authToken,
			totalTransferred,
			metadata.Size,
		)
	}

	// 通知上传端传输已完成
	if conn, exists := ffb.activeStreams[authToken]; exists {
		if tcpConn, ok := conn.(*StreamConnection); ok && tcpConn.Conn != nil {
			tcpConn.Conn.Close()
			log.Printf("🔌 关闭已完成文件的TCP连接: %s (token_id: %s)", metadata.OriginalFilename, authToken)
		} else if wsConn, ok := conn.(*WebSocketStreamConnection); ok {
			if transferCompleted {
				notification := map[string]interface{}{
					"command": "transfer_complete",
					"message": "文件传输已完成",
				}
				if wsConn.Conn != nil {
					err := wsConn.writeJSON(notification)
					if err != nil {
						log.Printf("发送传输完成通知失败: %v", err)
					} else {
						log.Printf("✅ 已通知上传端传输完成: %s", authToken)
					}
				} else {
					log.Printf("WebSocket连接已关闭，无法发送传输完成通知: %s", authToken)
				}
			}

			if wsConn.Conn != nil {
				wsConn.Conn.Close()
			}
			log.Printf("🔌 关闭已完成文件的WebSocket连接: %s (token_id: %s)", metadata.OriginalFilename, authToken)
		}
		delete(ffb.activeStreams, authToken)
	} else {
		log.Printf("⚠️ 传输完成时未找到活动连接: %s", authToken)
	}

	if transferCompleted {
		log.Printf("🏁 文件标记为已完成: %s (token_id: %s)", metadata.OriginalFilename, authToken)
	}
}

// 服务下载页面
func (ffb *FileFlowBridge) serveDownloadPage(w http.ResponseWriter, r *http.Request, authToken string, metadata *FileMetadata) {
	// 读取下载页面模板
	templatePath := "./static/download.html"
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		// 如果模板不存在，返回简单的提示页面
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head>
	<title>文件下载 - FileFlow Bridge</title>
	<meta charset="utf-8">
	<style>
		body { font-family: Arial, sans-serif; text-align: center; padding: 50px; background-color: #f5f5f5; }
		.container { background: white; padding: 30px; border-radius: 10px; box-shadow: 0 2px 10px rgba(0,0,0,0.1); display: inline-block; }
		button { background: #4CAF50; color: white; padding: 15px 32px; text-align: center; text-decoration: none; display: inline-block; font-size: 16px; margin: 10px; cursor: pointer; border: none; border-radius: 5px; }
		button:hover { background: #45a049; }
		.info { margin: 20px 0; }
	</style>
</head>
<body>
	<div class="container">
		<h1>📥 文件下载</h1>
		<div class="info">
			<p><strong>文件名:</strong> %s</p>
			<p><strong>文件大小:</strong> %.2f MB</p>
			<p><strong>文件ID:</strong> %s</p>
		</div>
		<p>点击下方按钮开始下载:</p>
		<a href="/download/%s/%s" download><button>点击下载</button></a>
		<br>
		<a href="/download/%s/%s">直接下载链接</a>
	</div>
</body>
</html>`,
			html.EscapeString(metadata.OriginalFilename),
			float64(metadata.Size)/(1024*1024),
			authToken,
			authToken,
			url.PathEscape(metadata.OriginalFilename),
			authToken,
			url.PathEscape(metadata.OriginalFilename))
		return
	}

	// 读取模板文件
	content, err := os.ReadFile(templatePath)
	if err != nil {
		log.Printf("读取下载页面模板失败: %v", err)
		http.Error(w, "内部服务器错误", http.StatusInternalServerError)
		return
	}

	// Format file size in human-readable format
	formattedSize := formatFileSize(metadata.Size)

	// 替换模板中的占位符
	templateContent := string(content)
	templateContent = strings.ReplaceAll(templateContent, "{{FILENAME}}", url.PathEscape(metadata.OriginalFilename))
	templateContent = strings.ReplaceAll(templateContent, "{{FILESIZE_HUMAN}}", formattedSize)
	templateContent = strings.ReplaceAll(templateContent, "{{FILESIZE_RAW}}", strconv.FormatInt(metadata.Size, 10))
	templateContent = strings.ReplaceAll(templateContent, "{{ORIGINAL_FILENAME}}", html.EscapeString(metadata.OriginalFilename))
	templateContent = strings.ReplaceAll(templateContent, "{{TOKEN}}", authToken)

	// 设置响应头
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// 输出替换后的页面内容
	fmt.Fprint(w, templateContent)
}

// Helper function to format file size in human-readable format
func formatFileSize(size int64) string {
	if size <= 0 {
		return "0 B"
	}

	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}

	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.2f %cB", float64(size)/float64(div), "KMGTPE"[exp])
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
		"filename":           metadata.Filename,
		"original_filename":  metadata.OriginalFilename,
		"size":               metadata.Size,
		"status":             metadata.Status,
		"client_ip":          metadata.ClientIP,
		"registered_at":      metadata.RegisteredAt.Format(time.RFC3339),
		"expires_at":         metadata.ExpiresAt.Format(time.RFC3339),
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
		"status":              "running",
		"uptime":              time.Since(ffb.serverStats.StartTime).Seconds(),
		"files_registered":    ffb.serverStats.FilesRegistered,
		"files_transferred":   ffb.serverStats.FilesTransferred,
		"bytes_transferred":   ffb.serverStats.BytesTransferred,
		"active_connections":  ffb.serverStats.ActiveConnections,
		"peak_connections":    ffb.serverStats.PeakConnections,
		"registered_files":    len(ffb.fileRegistry),
		"active_streams":      len(ffb.activeStreams),
		"completed_downloads": len(ffb.downloadCompleted),
	}
	ffb.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// 健康检查
func (ffb *FileFlowBridge) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	response := map[string]interface{}{
		"status":    "healthy",
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
			ffb.cleanupExpiredFiles(time.Now())

		case <-ffb.ShutdownEvent:
			return
		}
	}
}

// 单次清理已过期文件，便于测试与定时任务复用
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
		log.Printf("🧹 清理过期文件: %s", authToken)
	}

	if len(completedTokens) > 0 {
		ffb.mu.Lock()
		for _, authToken := range completedTokens {
			delete(ffb.downloadCompleted, authToken)
			delete(ffb.downloadCompletedAt, authToken)
			log.Printf("🧹 清理下载完成标记: %s", authToken)
		}
		ffb.mu.Unlock()
	}

	return expiredFiles
}

// 移除文件资源
func (ffb *FileFlowBridge) removeFileResources(authToken string) {
	ffb.mu.Lock()
	defer ffb.mu.Unlock()

	// 移除注册信息
	delete(ffb.fileRegistry, authToken)

	// 关闭TCP连接
	if streamConn, exists := ffb.activeStreams[authToken]; exists {
		if tcpConn, ok := streamConn.(*StreamConnection); ok && tcpConn.Conn != nil {
			tcpConn.Conn.Close()
		} else if wsConn, ok := streamConn.(*WebSocketStreamConnection); ok && wsConn.Conn != nil {
			wsConn.Conn.Close()
		}
		delete(ffb.activeStreams, authToken)
	}

	// 移除下载完成标记
	if !ffb.downloadCompleted[authToken] {
		delete(ffb.downloadCompleted, authToken)
		delete(ffb.downloadCompletedAt, authToken)
	}

	log.Printf("🗑️ 文件资源已清理: %s", authToken)
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

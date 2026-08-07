package main

import (
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ==================== 基础常量 ====================

const (
	tcpStreamReadTimeout     = 2 * time.Second
	connectionHealthInterval = 2 * time.Second
	completedDownloadTTL     = 1 * time.Minute
)

// ==================== 元数据 / 统计 ====================

// FileMetadata 文件元数据结构
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

	// SHA256 hash 校验（v2 新增；为空表示 provider 未提交，跳过校验）
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`

	// 多接收端 (1→N) 分发；MaxDownloads <= 1 表示单次下载（默认行为）
	MaxDownloads       int `json:"max_downloads,omitempty"`
	CompletedDownloads int `json:"completed_downloads,omitempty"`

	// === v3 新增：断点续传 (resumable) ===
	//
	// Resumable=true 时启用 chunked 上传：
	//   - 注册时 bridge 创建 TempPath 临时文件 + chunk 位集
	//   - provider 通过 PUT /upload/{token}/chunk?index=N 上传单个块
	//   - GET /upload/{token}/status 查询已收块（用于客户端续传）
	//   - 全部块到齐后文件标记为 ready，下载端 GET /download 可用 Range 支持
	// Resumable=false（默认）保留 v1/v2 的零落盘行为。
	Resumable     bool      `json:"resumable,omitempty"`
	ChunkSize     int64     `json:"chunk_size,omitempty"`
	TotalChunks   int       `json:"total_chunks,omitempty"`
	TempPath      string    `json:"-"` // 临时文件路径，不外暴露
	ReceivedBytes int64     `json:"received_bytes,omitempty"`
	UploadReadyAt time.Time `json:"upload_ready_at,omitempty"`

	// lastChunkAt 记录最后一次收到 chunk 的时间。
	// 用于 cleanupExpiredFiles 判断 resumable 上传是否已停滞：
	// 若距 lastChunkAt 超过 resumableStaleTTL 仍未完成，视为 provider 已放弃，
	// 提前清理临时文件，避免大文件 .part 长时间占用磁盘。
	// 受 chunkMu 保护。
	lastChunkAt time.Time

	// receivedChunks 用位集存储已收 chunk index：第 i 位为 1 表示 index=i 已收。
	// 相比 []bool 内存降为 1/8（4 KiB chunk × 100 GiB 文件 ≈ 3.2 MB vs 26 MB）。
	// 受 chunkMu 保护。
	receivedChunks []uint64

	// missingCount 当前缺失的 chunk 数，便于 O(1) 判断 upload 是否完成、
	// 避免 status 端点每次 O(N) 全扫位图。受 chunkMu 保护。
	missingCount int

	// uploadReady 所有 chunk 到齐后置 true。原子读写以允许锁外快速预检。
	uploadReady atomic.Bool

	chunkMu sync.Mutex // 保护 receivedChunks / ReceivedBytes / missingCount
}

// MarkChunkReceived 在 chunkMu 内幂等标记 chunk index 已收。
// 返回 (新收, 全部收齐)。新收=true 表示首次记录此 index。
func (m *FileMetadata) MarkChunkReceived(index int, n int64) (newlyReceived, allDone bool) {
	m.chunkMu.Lock()
	defer m.chunkMu.Unlock()

	word := index / 64
	bit := uint64(1) << uint(index%64)
	if word < 0 || word >= len(m.receivedChunks) {
		return false, m.missingCount == 0
	}
	if m.receivedChunks[word]&bit != 0 {
		// 已收过，幂等返回
		return false, m.missingCount == 0
	}
	m.receivedChunks[word] |= bit
	m.ReceivedBytes += n
	m.lastChunkAt = time.Now()
	if m.missingCount > 0 {
		m.missingCount--
	}
	allDone = m.missingCount == 0
	return true, allDone
}

// HasChunk 在锁内查询某 index 是否已收。
func (m *FileMetadata) HasChunk(index int) bool {
	m.chunkMu.Lock()
	defer m.chunkMu.Unlock()
	word := index / 64
	bit := uint64(1) << uint(index%64)
	if word < 0 || word >= len(m.receivedChunks) {
		return false
	}
	return m.receivedChunks[word]&bit != 0
}

// LastChunkAt 返回最后一次收到 chunk 的时间（用于 cleanup 判断停滞）。
func (m *FileMetadata) LastChunkAt() time.Time {
	m.chunkMu.Lock()
	defer m.chunkMu.Unlock()
	return m.lastChunkAt
}

// SnapshotChunkStatus 返回上传状态快照供 /status 接口使用。
// missingLimit > 0 时 missing 列表最多返回这么多项（截断防 DoS）；0 表示不限。
func (m *FileMetadata) SnapshotChunkStatus(missingLimit int) (missing []int, missingCount int, receivedBytes int64) {
	m.chunkMu.Lock()
	defer m.chunkMu.Unlock()

	missingCount = m.missingCount
	receivedBytes = m.ReceivedBytes

	if missingCount == 0 {
		return nil, 0, receivedBytes
	}

	cap := missingCount
	if missingLimit > 0 && missingLimit < cap {
		cap = missingLimit
	}
	missing = make([]int, 0, cap)
	for i := 0; i < m.TotalChunks; i++ {
		word := i / 64
		bit := uint64(1) << uint(i%64)
		if m.receivedChunks[word]&bit == 0 {
			missing = append(missing, i)
			if missingLimit > 0 && len(missing) >= missingLimit {
				break
			}
		}
	}
	return missing, missingCount, receivedBytes
}

// InitChunkBitmap 初始化位图与 missingCount，应在注册时调用。
func (m *FileMetadata) InitChunkBitmap(totalChunks int) {
	m.chunkMu.Lock()
	defer m.chunkMu.Unlock()
	m.TotalChunks = totalChunks
	words := (totalChunks + 63) / 64
	m.receivedChunks = make([]uint64, words)
	m.missingCount = totalChunks
}

// ServerStats 服务器统计信息
type ServerStats struct {
	StartTime         time.Time `json:"start_time"`
	FilesRegistered   int       `json:"files_registered"`
	FilesTransferred  int       `json:"files_transferred"`
	BytesTransferred  int64     `json:"bytes_transferred"`
	ActiveConnections int       `json:"active_connections"`
	PeakConnections   int       `json:"peak_connections"`
}

// ==================== 流连接抽象 ====================

// StreamConnection 一条由 provider 推送数据的流（TCP 或 io.Pipe 包装的 HTTP 上传）
type StreamConnection struct {
	Reader io.Reader
	Writer io.Writer
	Conn   net.Conn
}

// WebSocketStreamConnection 由浏览器 WebSocket 推送数据的流
type WebSocketStreamConnection struct {
	Conn      *websocket.Conn
	Buffer    []byte
	Index     int
	Mutex     sync.Mutex
	DataChan  chan []byte
	CloseChan chan struct{}
	closeOnce sync.Once
}

// close 安全地关闭 DataChan / CloseChan，可多次调用而不会 panic。
func (wsConn *WebSocketStreamConnection) close() {
	wsConn.closeOnce.Do(func() {
		close(wsConn.DataChan)
		close(wsConn.CloseChan)
	})
}

func (wsConn *WebSocketStreamConnection) writeJSON(v interface{}) error {
	wsConn.Mutex.Lock()
	defer wsConn.Mutex.Unlock()

	if wsConn.Conn == nil {
		return io.EOF
	}
	// 防止对端卡住时 WriteJSON 无限阻塞
	_ = wsConn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return wsConn.Conn.WriteJSON(v)
}

func (wsConn *WebSocketStreamConnection) writeMessage(messageType int, data []byte) error {
	wsConn.Mutex.Lock()
	defer wsConn.Mutex.Unlock()

	if wsConn.Conn == nil {
		return io.EOF
	}
	// 防止对端卡住时 WriteMessage 无限阻塞
	_ = wsConn.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return wsConn.Conn.WriteMessage(messageType, data)
}

// Read 实现 io.Reader：从 DataChan 读取数据，CloseChan 关闭时返回 EOF
func (wsConn *WebSocketStreamConnection) Read(p []byte) (n int, err error) {
	// 缓冲数据优先消费
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

	select {
	case data, ok := <-wsConn.DataChan:
		if !ok {
			return 0, io.EOF
		}
		wsConn.Buffer = data
		wsConn.Index = 0

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

// ==================== Bridge 主结构 ====================

// FileFlowBridge 文件流桥服务器
type FileFlowBridge struct {
	HTTPPort       int
	TCPPort        int
	MaxFileSize    int64
	TokenLength    int
	AllowedOrigins []string // CORS 允许的 origin 列表；空 / ["*"] 表示允许全部
	APIKey         string   // 可选：要求 /register 携带 X-API-Key 头
	MetricsKey     string   // 可选：独立鉴权 /metrics（与 APIKey 隔离）
	TLSCertFile    string   // 可选：TLS 证书路径
	TLSKeyFile     string   // 可选：TLS 私钥路径

	// GitHub 仓库（owner/repo），用于 /cli 下载页查询最新 release
	GitHubRepo string
	// GitHub Token（可选）：提升 /cli/releases/latest 代理的 API 配额
	GitHubToken string

	// 限速：单位 bytes/sec，0 = 不限速
	//
	// 语义：每个 upload / download 连接 *独立* 申请一个新的 token bucket，
	// 不在多个连接间共享。N 并发连接的实际带宽上限约为 N * limit。
	UploadBytesPerSec   int64
	DownloadBytesPerSec int64

	// === v3 新增 ===

	// 最大并行上传数（默认 10；0/负数 = 不限）。覆盖所有上传入口：
	// TCP / WebSocket / multipart / chunk PUT。超过时返回 429。
	MaxParallelUploads int

	// 临时文件根目录（默认 os.TempDir()/fileflow-bridge）。仅 resumable 模式使用。
	TempDir string

	// uploadSem 限流信号量；nil 表示不限。NewFileFlowBridge 中初始化。
	uploadSem chan struct{}

	// WebSocket upgrader，按实例配置 CheckOrigin
	upgrader *websocket.Upgrader

	ShutdownEvent chan struct{}

	fileRegistry        map[string]*FileMetadata
	activeStreams       map[string]interface{} // 支持多种连接类型
	downloadCompleted   map[string]bool
	downloadCompletedAt map[string]time.Time
	downloadDone        map[string]chan struct{} // 注册时创建；完成 / 资源回收时关闭，供等待方 select

	// Prometheus 指标：内置最小实现（无外部依赖）
	metrics serverMetrics

	serverStats    ServerStats
	isShuttingDown atomic.Bool
	shutdownOnce   sync.Once

	mu sync.RWMutex
}

// TriggerShutdown 安全地触发 ShutdownEvent，可多次调用而不会 panic。
func (ffb *FileFlowBridge) TriggerShutdown() {
	ffb.shutdownOnce.Do(func() {
		close(ffb.ShutdownEvent)
	})
}

// ==================== 内部错误哨兵 ====================

var (
	errServerShutdown = fmt.Errorf("server shutting down")
	errUploadTimeout  = fmt.Errorf("upload timed out waiting for download")
	errTooManyUploads = fmt.Errorf("too many concurrent uploads")
)

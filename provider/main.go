package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	// "log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ==================== 全局配置与日志 ====================
// var logger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

// ==================== 数据结构定义 ====================

// FileInfo 文件信息结构体
type FileInfo struct {
	Path    string
	Name    string
	Size    int64
	ModTime int64
}

// RegisterResponse 注册文件响应结构体
type RegisterResponse struct {
	AuthToken        string `json:"auth_token"`
	DownloadURL      string `json:"download_url"`
	OriginalFilename string `json:"original_filename"`
	TcpEndpoint      struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"tcp_endpoint"`

	// v3 resumable 模式响应字段
	Resumable      bool   `json:"resumable,omitempty"`
	ChunkSize      int64  `json:"chunk_size,omitempty"`
	TotalChunks    int    `json:"total_chunks,omitempty"`
	ChunkUploadURL string `json:"chunk_upload_url,omitempty"`
	ChunkStatusURL string `json:"chunk_status_url,omitempty"`
}

// FlowProvider 主客户端结构体
type FlowProvider struct {
	BridgeURL    string
	APIKey       string        // 可选，注册时携带 X-API-Key 头
	SHA256       string        // 可选，注册时提交给桥（小写十六进制）
	MaxDownloads int           // 当前 bridge 仅支持 0/1（single-shot）；> 1 会被拒绝
	WaitTimeout  time.Duration // 上传完成后等下载者多久；0 = 默认 30 分钟

	// resumable 模式
	Resumable  bool  // 是否启用断点续传
	ChunkSize  int64 // 想用的 chunk size，bridge 可能调整；0 = 8 MiB
	UploadConc int   // resumable 上传时本端的并发；默认 4
	MaxRetries int   // 单 chunk 失败后重试次数；默认 5

	AuthToken       string
	TcpHost         string
	TcpPort         int
	FileInfo        FileInfo
	DownloadURL     string
	chunkUploadURL  string
	chunkStatusURL  string
	actualChunkSize int64
	totalChunks     int
}

// ==================== 核心功能实现 ====================

// RevokeFile 主动撤销已注册的 token（DELETE /register/{token}）。
// 用于 Ctrl+C / Ctrl+\ 等场景，让 bridge 立即清理 .part 文件、注册表项
// 并截断正在进行的下载，避免错误内容继续分发。
//
// 多次调用是幂等的：bridge 返回 200 / 204 都视为成功。
func (f *FlowProvider) RevokeFile() error {
	if f.AuthToken == "" {
		return nil // 还没注册成功，没什么要撤销的
	}
	url := fmt.Sprintf("%s/register/%s", f.BridgeURL, f.AuthToken)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	if f.APIKey != "" {
		req.Header.Set("X-API-Key", f.APIKey)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		// 已被 cleanup 清掉，视作成功
		return nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("撤销失败: status=%d body=%s", resp.StatusCode, string(body))
	}
}

// NewFlowProvider 创建新的FlowProvider实例
func NewFlowProvider(bridgeURL string) *FlowProvider {
	return &FlowProvider{
		BridgeURL: strings.TrimSuffix(bridgeURL, "/"),
	}
}

// RegisterFile 注册文件到桥接服务器
func (f *FlowProvider) RegisterFile(filePath string) (*RegisterResponse, error) {
	// 获取文件信息
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("文件不存在: %v", err)
	}

	f.FileInfo = FileInfo{
		Path:    filePath,
		Name:    filepath.Base(filePath),
		Size:    fileInfo.Size(),
		ModTime: fileInfo.ModTime().Unix(),
	}

	// 准备注册请求
	registerURL := fmt.Sprintf("%s/register", f.BridgeURL)
	payload := map[string]interface{}{
		"filename": f.FileInfo.Name,
		"size":     f.FileInfo.Size,
	}
	if f.SHA256 != "" {
		payload["sha256"] = f.SHA256
	}
	if f.MaxDownloads > 0 {
		payload["max_downloads"] = f.MaxDownloads
	}
	if f.Resumable {
		payload["resumable"] = true
		if f.ChunkSize > 0 {
			payload["chunk_size"] = f.ChunkSize
		}
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("JSON序列化失败: %v", err)
	}

	// 发送HTTP POST请求
	req, err := http.NewRequest("POST", registerURL, strings.NewReader(string(jsonPayload)))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if f.APIKey != "" {
		req.Header.Set("X-API-Key", f.APIKey)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("网络错误: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("注册失败: %s (状态码: %d)", string(body), resp.StatusCode)
	}

	// 解析响应
	var result RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %v", err)
	}

	// 更新实例状态
	f.AuthToken = result.AuthToken
	f.TcpHost = result.TcpEndpoint.Host
	f.TcpPort = result.TcpEndpoint.Port
	f.DownloadURL = result.DownloadURL
	if result.Resumable {
		f.chunkUploadURL = result.ChunkUploadURL
		f.chunkStatusURL = result.ChunkStatusURL
		f.actualChunkSize = result.ChunkSize
		f.totalChunks = result.TotalChunks
		if f.actualChunkSize == 0 {
			return nil, fmt.Errorf("服务器返回 resumable=true 但 chunk_size=0")
		}
	}

	fmt.Println("📁 原始文件名:", result.OriginalFilename)
	fmt.Println("🔗 点击或双击复制下载地址:")
	fmt.Println(result.DownloadURL)
	if result.Resumable {
		fmt.Printf("🧩 Resumable: chunk_size=%d, total_chunks=%d\n", result.ChunkSize, result.TotalChunks)
	}

	return &result, nil
}

func waitForTransferCompletion(conn net.Conn, timeout time.Duration) error {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("设置等待超时失败: %v", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 1)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return fmt.Errorf("等待下载完成超时: %w", err)
		}
		return fmt.Errorf("等待下载完成失败: %v", err)
	}
}

// EstablishStreamConnection 建立TCP流连接并传输文件
func (f *FlowProvider) EstablishStreamConnection() error {
	if f.AuthToken == "" || f.TcpHost == "" || f.TcpPort == 0 {
		return errors.New("文件未正确注册")
	}

	// fmt.Println("🔗 连接到TCP服务器 %s:%d...", f.TcpHost, f.TcpPort)

	// 建立TCP连接
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", f.TcpHost, f.TcpPort), 30*time.Second)
	if err != nil {
		return fmt.Errorf("TCP连接失败: %v", err)
	}
	defer conn.Close()

	// 发送连接元数据
	meta := map[string]string{
		"auth_token": f.AuthToken,
		"filename":   f.FileInfo.Name,
	}
	metaJSON, _ := json.Marshal(meta)
	if _, err := conn.Write(append(metaJSON, '\n')); err != nil {
		return fmt.Errorf("发送元数据失败: %v", err)
	}

	// 等待服务器确认
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取服务器响应失败: %v", err)
	}
	if strings.TrimSpace(response) != "STREAM_READY" {
		return fmt.Errorf("服务器响应错误: %s", response)
	}

	fmt.Println("✅ 流连接已建立，开始传输文件...")

	// 传输文件内容
	if err := f.streamFileContent(conn); err != nil {
		return err
	}

	fmt.Println("⏳ 文件已上传，等待下载端完成...")
	waitTimeout := f.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 30 * time.Minute
	}
	if err := waitForTransferCompletion(conn, waitTimeout); err != nil {
		return err
	}

	fmt.Println("🎉 文件传输完成!")
	return nil
}

// FormatSpeed 格式化速度输出
func FormatSpeed(bytesPerSecond float64) string {
	units := []string{"B/s", "KiB/s", "MiB/s", "GiB/s"}
	unitIndex := 0
	for bytesPerSecond >= 1024 && unitIndex < len(units)-1 {
		bytesPerSecond /= 1024
		unitIndex++
	}
	return fmt.Sprintf("%.2f %s", bytesPerSecond, units[unitIndex])
}

// FormatSize 格式化大小输出
func FormatSize(bytes int64) string {
	size := float64(bytes)
	units := []string{"B", "KiB", "MiB", "GiB"}
	unitIndex := 0
	for size >= 1024 && unitIndex < len(units)-1 {
		size /= 1024
		unitIndex++
	}
	return fmt.Sprintf("%.2f %s", size, units[unitIndex])
}

// streamFileContent 流式传输文件内容
func (f *FlowProvider) streamFileContent(conn net.Conn) error {
	file, err := os.Open(f.FileInfo.Path)
	if err != nil {
		return fmt.Errorf("打开文件失败: %v", err)
	}
	defer file.Close()

	// 进度条：使用构造函数初始化退出信号 channel
	progress := NewProgressBar(f.FileInfo.Size, "📤 上传中", []string{"B", "KiB", "MiB", "GiB"})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		progress.Print()
	}()
	// 无论后续是否出错都通知进度条退出并等待 goroutine 结束，避免泄漏
	defer wg.Wait()
	defer progress.Finish()

	// 传输文件
	buffer := make([]byte, 65536)
	var transferred int64
	startTime := time.Now()

	for {
		n, err := file.Read(buffer)
		if n > 0 {
			if _, writeErr := conn.Write(buffer[:n]); writeErr != nil {
				return fmt.Errorf("写入数据失败: %v", writeErr)
			}
			transferred += int64(n)
			progress.Set(transferred)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取文件失败: %v", err)
		}
	}

	// 计算传输统计
	duration := time.Since(startTime)
	var bps float64
	if duration.Seconds() > 0 {
		bps = float64(transferred) / duration.Seconds()
	}

	fmt.Printf(
		"📊 传输统计: %s, 耗时 %.2f 秒, 平均速度: %s\n",
		FormatSize(transferred),
		duration.Seconds(),
		FormatSpeed(bps),
	)

	return nil
}

// GenerateDownloadInfo 生成下载信息
func (f *FlowProvider) GenerateDownloadInfo() string {
	if f.AuthToken == "" || f.DownloadURL == "" {
		return "文件未注册或下载URL不可用"
	}

	size := float64(f.FileInfo.Size)
	unit := "Bytes"
	units := []string{"Bytes", "KiB", "MiB", "GiB", "TiB"}

	i := 0
	for size >= 1024 && i < len(units)-1 {
		size /= 1024
		i++
	}
	unit = units[i]

	var sizeStr string
	if unit == "Bytes" {
		sizeStr = fmt.Sprintf("%d %s", f.FileInfo.Size, unit)
	} else {
		sizeStr = fmt.Sprintf("%.2f %s", size, unit)
	}

	return fmt.Sprintf(`
📥 下载信息:

• 文件名称: %s
• 文件大小: %s
• 下载URL: %s
• 有效时间: 下载完成后自动失效

💡 提示: 请确保发送端保持运行，直到下载完成。
`, f.FileInfo.Name, sizeStr, f.DownloadURL)
}

// ==================== 进度条实现 ====================

// ProgressBar 简单的进度条实现
//
// 使用方式：
//
//	pb := NewProgressBar(total, desc, units)
//	go pb.Print()
//	... pb.Set(n) ...
//	pb.Finish() // 必须调用，否则 Print 协程不会退出
type ProgressBar struct {
	Total     int64
	Current   int64
	Desc      string
	Units     []string
	lastPrint time.Time
	mu        sync.Mutex
	done      chan struct{}
	doneOnce  sync.Once
}

// NewProgressBar 构造一个 ProgressBar，并初始化退出信号
func NewProgressBar(total int64, desc string, units []string) *ProgressBar {
	return &ProgressBar{
		Total: total,
		Desc:  desc,
		Units: units,
		done:  make(chan struct{}),
	}
}

// Set 更新当前进度
func (p *ProgressBar) Set(current int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Current = current
}

// Print 打印进度条，直到 Finish 被调用或进度满
func (p *ProgressBar) Print() {
	ticker := time.NewTicker(500 * time.Millisecond) // 每500ms更新一次
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.mu.Lock()
			if p.Total > 0 && p.Current >= p.Total {
				p.mu.Unlock()
				return
			}

			if p.Total <= 0 {
				p.mu.Unlock()
				continue
			}

			// 计算百分比和单位
			percent := float64(p.Current) / float64(p.Total) * 100
			size, unit := p.getHumanSize(p.Current)
			totalSize, totalUnit := p.getHumanSize(p.Total)

			// 打印进度条
			fmt.Printf("\r%s [%-50s] %.1f%% (%.2f %s / %.2f %s)",
				p.Desc,
				strings.Repeat("=", int(percent/2))+">",
				percent,
				size, unit,
				totalSize, totalUnit,
			)
			p.mu.Unlock()
		}
	}
}

// Finish 完成进度条，可安全多次调用
func (p *ProgressBar) Finish() {
	p.doneOnce.Do(func() {
		if p.done != nil {
			close(p.done)
		}
	})

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.Total <= 0 {
		return
	}

	// 获取当前大小（完成时 Current == Total）和单位（与 Total 单位一致）
	currentSize, currentUnit := p.getHumanSize(p.Current)
	totalSize, totalUnit := p.getHumanSize(p.Total)

	fmt.Printf("\r%s [%-50s] 100.0%% (%.2f %s / %.2f %s)\n",
		p.Desc,
		strings.Repeat("=", 50),
		currentSize,
		currentUnit,
		totalSize,
		totalUnit,
	)
}

// getHumanSize 转换为人类可读的大小单位
func (p *ProgressBar) getHumanSize(bytes int64) (float64, string) {
	size := float64(bytes)
	unitIndex := 0
	for size >= 1024 && unitIndex < len(p.Units)-1 {
		size /= 1024
		unitIndex++
	}
	return size, p.Units[unitIndex]
}

// ==================== 主函数 ====================

const usageText = `🌊 FileFlow Bridge - 文件提供客户端

用法:
  fileflowprovider [flags] <桥接服务器URL> <文件路径>

示例:
  fileflowprovider http://localhost:8000 ./large_file.zip
  fileflowprovider --hash --max-downloads=3 https://ffb.soocoo.xyz ./file.zip
  FFB_API_KEY=xxx fileflowprovider --hash https://ffb.soocoo.xyz ./file.zip

Flags:
`

// computeSHA256 流式计算文件 SHA256（小写十六进制）
func computeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// signalContext 返回一个会在 SIGINT/SIGTERM/SIGHUP 时取消的 Context。
// 这是 provider 主动撤销的触发源——CLI 主流程在 ctx.Done() 时立即
// 调用 RevokeFile()，让 bridge 立即释放资源而不是等到 TTL 过期。
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

func main() {
	var (
		flagHash         = flag.Bool("hash", false, "上传前在本地计算 SHA256 并提交给桥用于校验")
		flagSHA256       = flag.String("sha256", "", "直接指定文件的 SHA256（64 位十六进制），优先级高于 --hash")
		flagMaxDownloads = flag.Int("max-downloads", 0, "允许的最大下载次数；当前 bridge 仅支持 0/1（>1 会被服务器拒绝）")
		flagAPIKey       = flag.String("api-key", os.Getenv("FFB_API_KEY"), "可选：桥接服务器的 API Key（也可通过 FFB_API_KEY 环境变量设置）")
		flagWaitTimeout  = flag.Duration("wait-timeout", 30*time.Minute, "上传完成后等待下载者完成的最长时间")
		flagResumable    = flag.Bool("resumable", false, "启用断点续传：使用 chunked PUT 上传；支持中断后恢复，下载端原生支持 Range")
		flagChunkSize    = flag.Int64("chunk-size", 0, "resumable 模式下的 chunk 大小（字节），0=用服务器默认（8MiB）")
		flagUploadConc   = flag.Int("upload-conc", 4, "resumable 上传并发数")
		flagMaxRetries   = flag.Int("retries", 5, "单个 chunk 失败后的重试次数")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usageText)
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		flag.Usage()
		os.Exit(1)
	}
	bridgeURL := args[0]
	filePath := args[1]

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		fmt.Println("❌ 错误: 文件", filePath, "不存在")
		os.Exit(1)
	}

	provider := NewFlowProvider(bridgeURL)
	provider.APIKey = strings.TrimSpace(*flagAPIKey)
	if *flagMaxDownloads > 1 {
		fmt.Println("⚠️ 注意: bridge 当前不支持 max_downloads > 1，会返回 501；建议改为 0/1")
	}
	if *flagMaxDownloads > 0 {
		provider.MaxDownloads = *flagMaxDownloads
	}
	if *flagWaitTimeout > 0 {
		provider.WaitTimeout = *flagWaitTimeout
	}
	provider.Resumable = *flagResumable
	provider.ChunkSize = *flagChunkSize
	provider.UploadConc = *flagUploadConc
	provider.MaxRetries = *flagMaxRetries

	// 解析 SHA256：优先用显式给的，再回退到本地计算
	if v := strings.TrimSpace(*flagSHA256); v != "" {
		provider.SHA256 = strings.ToLower(v)
	} else if *flagHash {
		fmt.Println("🔐 正在计算 SHA256...")
		sum, err := computeSHA256(filePath)
		if err != nil {
			fmt.Println("❌ 计算 SHA256 失败:", err)
			os.Exit(1)
		}
		provider.SHA256 = sum
		fmt.Println("🔐 SHA256:", sum)
	}

	fmt.Println("📝 注册文件中...")
	if _, err := provider.RegisterFile(filePath); err != nil {
		fmt.Println("❌ 注册失败:", err)
		os.Exit(1)
	}

	// 安装信号处理：注册成功后到上传结束之间，Ctrl+C / SIGTERM 必须
	// 主动撤销 token 让 bridge 立即释放资源，而不是让对端等 TTL。
	sigCtx, sigStop := signalContext()
	defer sigStop()
	var (
		revoked   sync.Once
		uploadErr error
	)
	revokeNow := func(reason string) {
		revoked.Do(func() {
			fmt.Fprintf(os.Stderr, "\n🛑 %s，撤销 token...\n", reason)
			if err := provider.RevokeFile(); err != nil {
				fmt.Fprintln(os.Stderr, "⚠️ 撤销失败:", err)
			} else {
				fmt.Fprintln(os.Stderr, "✅ 已撤销，bridge 资源已释放")
			}
		})
	}
	go func() {
		<-sigCtx.Done()
		revokeNow("收到中断信号")
		os.Exit(130) // 标准 SIGINT 退出码
	}()
	// 上传过程中任何错误也一并撤销，避免半成品文件留在 bridge 端
	defer func() {
		if uploadErr != nil {
			revokeNow("上传异常")
		}
	}()

	if provider.Resumable {
		fmt.Println("📤 启动 resumable 上传...")
		if err := provider.UploadChunked(); err != nil {
			uploadErr = err
			fmt.Println("❌ 上传失败:", err)
			os.Exit(1)
		}
	} else {
		fmt.Println("🔗 建立流连接...")
		if err := provider.EstablishStreamConnection(); err != nil {
			uploadErr = err
			fmt.Println("❌ 传输失败:", err)
			os.Exit(1)
		}
	}

	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println(provider.GenerateDownloadInfo())
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("✅ 操作完成! 文件已准备好下载")
	fmt.Println("💡 注意: 文件下载完成后，下载链接将自动失效")
}

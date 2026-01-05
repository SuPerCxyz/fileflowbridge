package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	// "log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ==================== 全局配置与日志 ====================
// var logger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

// ==================== 数据结构定义 ====================

// FileInfo 文件信息结构体
type FileInfo struct {
	Path     string
	Name     string
	Size     int64
	ModTime  int64
}

// RegisterResponse 注册文件响应结构体
type RegisterResponse struct {
	AuthToken       string `json:"auth_token"`
	DownloadURL     string `json:"download_url"`
	OriginalFilename string `json:"original_filename"`
	TcpEndpoint     struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"tcp_endpoint"`
}

// FlowProvider 主客户端结构体
type FlowProvider struct {
	BridgeURL    string
	AuthToken    string
	TcpHost      string
	TcpPort      int
	FileInfo     FileInfo
	DownloadURL  string
}

// ==================== 核心功能实现 ====================

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

	// 修复可能的多余端口号
	if strings.Contains(f.TcpHost, ":") {
		parts := strings.Split(f.TcpHost, ":")
		if len(parts) > 1 {
			f.TcpHost = parts[0]  // 只取主机名部分
			// 如果端口被错误地放在了host字段，可以尝试提取
			if port, err := strconv.Atoi(parts[1]); err == nil && f.TcpPort == 0 {
				f.TcpPort = port
			}
		}
	}

	// 日志输出
	// logger.Printf("✅ 文件注册成功")
	// logger.Printf("📋 文件Token: %s", f.AuthToken)
	// logger.Printf("🔑 认证令牌: %s", f.AuthToken)
	// logger.Printf("🔌 TCP端点: %s:%d", f.TcpHost, f.TcpPort)
	fmt.Println("📁 原始文件名:", result.OriginalFilename)
	fmt.Println("🔗 点击或双击复制下载地址:")
	fmt.Println(result.DownloadURL)

	return &result, nil
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
		"filename":  f.FileInfo.Name,
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

	fmt.Println("🎉 文件传输完成!")
	return nil
}

// streamFileContent 流式传输文件内容
func (f *FlowProvider) streamFileContent(conn net.Conn) error {
	file, err := os.Open(f.FileInfo.Path)
	if err != nil {
		return fmt.Errorf("打开文件失败: %v", err)
	}
	defer file.Close()

	// 进度条实现
	progress := &ProgressBar{
		Total: f.FileInfo.Size,
		Desc:  "📤 上传中",
		Units: []string{"B", "KB", "MB", "GB"},
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		progress.Print()
	}()
	defer wg.Wait()

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
	speed := float64(transferred) / duration.Seconds() / 1024 // KB/s

	progress.Finish()
	fmt.Printf(
		"📊 传输统计: %d 字节, %.2f 秒, %.2f KB/s",
		transferred, duration.Seconds(), speed,
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
type ProgressBar struct {
	Total     int64
	Current   int64
	Desc      string
	Units     []string
	lastPrint time.Time
	mu        sync.Mutex
}

// Set 更新当前进度
func (p *ProgressBar) Set(current int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Current = current
}

// Print 打印进度条
func (p *ProgressBar) Print() {
	ticker := time.NewTicker(500 * time.Millisecond) // 每500ms更新一次
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		if p.Current >= p.Total {
			p.mu.Unlock()
			break
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

// Finish 完成进度条
func (p *ProgressBar) Finish() {
    p.mu.Lock()
    defer p.mu.Unlock()

    // 获取当前大小（完成时 Current == Total）和单位（与 Total 单位一致）
    currentSize, currentUnit := p.getHumanSize(p.Current)
    totalSize, totalUnit := p.getHumanSize(p.Total)

    // 格式化字符串：5个占位符对应5个参数
    fmt.Printf("\r%s [%-50s] 100.0%% (%.2f %s / %.2f %s)\n",
        p.Desc,                  // %s：描述文字（如 "上传中"）
        strings.Repeat("=", 50), // %-50s：50个等号填满进度条
        currentSize,             // %.2f：当前大小数值（完成时=总大小）
        currentUnit,             // %s：当前单位（如 MB/GB）
        totalSize,               // %.2f：总大小数值
        totalUnit,                // %s：总单位（如 MB/GB）
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

func main() {
	if len(os.Args) < 3 {
		fmt.Println("🌊 FileFlow Bridge - 文件提供客户端")
		fmt.Println("=" + strings.Repeat("=", 49))
		fmt.Println("用法: flow_provider <桥接服务器URL> <文件路径>")
		fmt.Println("示例: flow_provider http://localhost:8000 ./large_file.zip")
		os.Exit(1)
	}

	bridgeURL := os.Args[1]
	filePath := os.Args[2]

	// 检查文件是否存在
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		fmt.Println("❌ 错误: 文件", filePath, "不存在")
		os.Exit(1)
	}

	provider := NewFlowProvider(bridgeURL)

	// 执行注册和传输
	var err error
	fmt.Println("📝 注册文件中...")
	if _, err = provider.RegisterFile(filePath); err != nil {
		fmt.Println("❌ 注册失败:", err)
	}

	fmt.Println("🔗 建立流连接...")
	if err = provider.EstablishStreamConnection(); err != nil {
		fmt.Println("❌ 传输失败:", err)
	}

	// 显示下载信息
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println(provider.GenerateDownloadInfo())
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("✅ 操作完成! 文件已准备好下载")
	fmt.Println("💡 注意: 文件下载完成后，下载链接将自动失效")
}
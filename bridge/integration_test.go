package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// 集成测试套件
type IntegrationTestSuite struct {
	bridge      *FileFlowBridge
	server      *httptest.Server
	bridgeURL   string
	tempDir     string
	testFiles   []string
	cleanupOnce sync.Once
}

// 创建集成测试环境
func createIntegrationTestSuite(t *testing.T) *IntegrationTestSuite {
	// 创建临时目录
	tempDir, err := os.MkdirTemp("", "fileflow_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}

	// 创建测试桥接服务器
	ffb := &FileFlowBridge{
		HTTPPort:          0, // 使用随机端口
		TCPPort:           0, // 使用随机端口
		MaxFileSize:       100,
		TokenLength:       8,
		ShutdownEvent:     make(chan struct{}),
		fileRegistry:      make(map[string]*FileMetadata),
		activeStreams:     make(map[string]interface{}),
		downloadCompleted: make(map[string]bool),
		serverStats: ServerStats{
			StartTime: time.Now(),
		},
	}

	// 创建HTTP路由器
	router := mux.NewRouter()
	router.HandleFunc("/register", ffb.handleFileRegistration).Methods("POST")
	router.HandleFunc("/status/{auth_token}", ffb.handleStatusCheck).Methods("GET")
	router.HandleFunc("/stats", ffb.handleServerStats).Methods("GET")
	router.HandleFunc("/health", ffb.handleHealthCheck).Methods("GET")
	router.HandleFunc("/download/{auth_token}", ffb.handleFileDownload).Methods("GET")
	router.HandleFunc("/upload/{auth_token}", ffb.handleFileUpload).Methods("POST")
	router.HandleFunc("/ws/{auth_token}", ffb.handleWebSocketConnection).Methods("GET")

	// 创建测试服务器
	server := httptest.NewServer(router)

	return &IntegrationTestSuite{
		bridge:    ffb,
		server:    server,
		bridgeURL: server.URL,
		tempDir:   tempDir,
		testFiles: []string{},
	}
}

// 清理测试环境
func (suite *IntegrationTestSuite) cleanup() {
	suite.cleanupOnce.Do(func() {
		if suite.server != nil {
			suite.server.Close()
		}
		// 清理临时文件
		for _, file := range suite.testFiles {
			os.Remove(file)
		}
		os.RemoveAll(suite.tempDir)
	})
}

// 创建测试文件
func (suite *IntegrationTestSuite) createTestFile(name string, content string) string {
	filePath := filepath.Join(suite.tempDir, name)
	err := os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		panic(fmt.Sprintf("创建测试文件失败: %v", err))
	}
	suite.testFiles = append(suite.testFiles, filePath)
	return filePath
}

// 测试完整的文件注册流程
func TestCompleteFileRegistration(t *testing.T) {
	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	// 创建测试文件
	testFile := suite.createTestFile("test.txt", "这是测试文件内容")
	fileInfo, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("获取文件信息失败: %v", err)
	}

	// 准备注册请求
	payload := map[string]interface{}{
		"filename": filepath.Base(testFile),
		"size":     fileInfo.Size(),
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("JSON序列化失败: %v", err)
	}

	// 发送注册请求
	resp, err := http.Post(
		suite.bridgeURL+"/register",
		"application/json",
		bytes.NewReader(jsonPayload),
	)
	if err != nil {
		t.Fatalf("注册请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("注册失败，状态码: %d, 响应: %s", resp.StatusCode, string(body))
	}

	// 解析响应
	var registerResp struct {
		AuthToken        string `json:"auth_token"`
		DownloadURL      string `json:"download_url"`
		OriginalFilename string `json:"original_filename"`
		TcpEndpoint      struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"tcp_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&registerResp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// 验证响应
	if registerResp.AuthToken == "" {
		t.Error("认证令牌为空")
	}
	if registerResp.DownloadURL == "" {
		t.Error("下载URL为空")
	}

	t.Logf("文件注册成功，令牌: %s", registerResp.AuthToken)

	// 测试状态查询
	statusResp, err := http.Get(suite.bridgeURL + "/status/" + registerResp.AuthToken)
	if err != nil {
		t.Fatalf("状态查询失败: %v", err)
	}
	defer statusResp.Body.Close()

	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("状态查询失败，状态码: %d", statusResp.StatusCode)
	}

	var status map[string]interface{}
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("解析状态响应失败: %v", err)
	}

	if status["filename"] != filepath.Base(testFile) {
		t.Errorf("文件名不匹配，期望: %s, 实际: %v", filepath.Base(testFile), status["filename"])
	}

	t.Log("状态查询测试通过")
}

// 测试WebSocket连接和文件传输
func TestWebSocketFileTransfer(t *testing.T) {
	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	// 创建测试文件
	testFile := suite.createTestFile("websocket_test.txt", "WebSocket测试文件内容\n第二行\n第三行")
	fileInfo, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("获取文件信息失败: %v", err)
	}

	// 注册文件
	payload := map[string]interface{}{
		"filename": filepath.Base(testFile),
		"size":     fileInfo.Size(),
	}

	jsonPayload, _ := json.Marshal(payload)
	resp, err := http.Post(suite.bridgeURL+"/register", "application/json", bytes.NewReader(jsonPayload))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	defer resp.Body.Close()

	var registerResp struct {
		AuthToken        string `json:"auth_token"`
		DownloadURL      string `json:"download_url"`
		OriginalFilename string `json:"original_filename"`
		TcpEndpoint      struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"tcp_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&registerResp); err != nil {
		t.Fatalf("解析注册响应失败: %v", err)
	}

	// 建立WebSocket连接
	wsURL := strings.Replace(suite.bridgeURL, "http", "ws", 1) + "/ws/" + registerResp.AuthToken
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket连接失败: %v", err)
	}
	defer wsConn.Close()

	// 等待READY消息
	_, message, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("读取READY消息失败: %v", err)
	}

	var readyMsg map[string]interface{}
	if err := json.Unmarshal(message, &readyMsg); err != nil {
		t.Fatalf("解析READY消息失败: %v", err)
	}

	if readyMsg["command"] != "READY" {
		t.Fatalf("期望READY命令，实际: %v", readyMsg["command"])
	}

	t.Log("WebSocket连接建立成功")

	// 模拟文件上传（发送二进制数据）
	fileContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("读取测试文件失败: %v", err)
	}

	// 发送文件数据
	err = wsConn.WriteMessage(websocket.BinaryMessage, fileContent)
	if err != nil {
		t.Fatalf("发送文件数据失败: %v", err)
	}

	t.Log("文件数据发送成功")

	// 等待一段时间让服务器处理
	time.Sleep(100 * time.Millisecond)

	// 测试下载
	downloadResp, err := http.Get(suite.bridgeURL + "/download/" + registerResp.AuthToken)
	if err != nil {
		t.Fatalf("下载请求失败: %v", err)
	}
	defer downloadResp.Body.Close()

	if downloadResp.StatusCode != http.StatusOK {
		t.Fatalf("下载失败，状态码: %d", downloadResp.StatusCode)
	}

	// 读取下载内容
	downloadedContent, err := io.ReadAll(downloadResp.Body)
	if err != nil {
		t.Fatalf("读取下载内容失败: %v", err)
	}

	// 验证内容一致性
	if string(downloadedContent) != string(fileContent) {
		t.Error("下载内容与原始内容不匹配")
	}

	t.Log("WebSocket文件传输测试通过")
}

// 测试并发文件传输
func TestConcurrentFileTransfers(t *testing.T) {
	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	concurrency := 10
	var wg sync.WaitGroup
	errors := make(chan error, concurrency)

	// 并发注册和传输文件
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// 创建测试文件
			filename := fmt.Sprintf("concurrent_%d.txt", id)
			content := fmt.Sprintf("并发测试文件 %d 内容\n第二行 %d", id, id)
			testFile := suite.createTestFile(filename, content)
			fileInfo, _ := os.Stat(testFile)

			// 注册文件
			payload := map[string]interface{}{
				"filename": filename,
				"size":     fileInfo.Size(),
			}

			jsonPayload, _ := json.Marshal(payload)
			resp, err := http.Post(suite.bridgeURL+"/register", "application/json", bytes.NewReader(jsonPayload))
			if err != nil {
				errors <- fmt.Errorf("注册失败 ID %d: %v", id, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				errors <- fmt.Errorf("注册失败 ID %d, 状态码: %d", id, resp.StatusCode)
				return
			}

			var registerResp struct {
				AuthToken        string `json:"auth_token"`
				DownloadURL      string `json:"download_url"`
				OriginalFilename string `json:"original_filename"`
				TcpEndpoint      struct {
					Host string `json:"host"`
					Port int    `json:"port"`
				} `json:"tcp_endpoint"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&registerResp); err != nil {
				errors <- fmt.Errorf("解析响应失败 ID %d: %v", id, err)
				return
			}

			// 测试状态查询
			statusResp, err := http.Get(suite.bridgeURL + "/status/" + registerResp.AuthToken)
			if err != nil {
				errors <- fmt.Errorf("状态查询失败 ID %d: %v", id, err)
				return
			}
			defer statusResp.Body.Close()

			if statusResp.StatusCode != http.StatusOK {
				errors <- fmt.Errorf("状态查询失败 ID %d, 状态码: %d", id, statusResp.StatusCode)
				return
			}

		}(i)
	}

	// 等待所有goroutine完成
	wg.Wait()
	close(errors)

	// 检查是否有错误
	for err := range errors {
		t.Error(err)
	}

	if len(errors) == 0 {
		t.Logf("并发文件传输测试通过，成功处理 %d 个文件", concurrency)
	}
}

// 测试错误处理
func TestErrorHandling(t *testing.T) {
	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	// 测试无效的文件注册
	invalidPayload := map[string]interface{}{
		"filename": "", // 空文件名
		"size":     100,
	}

	jsonPayload, _ := json.Marshal(invalidPayload)
	resp, err := http.Post(suite.bridgeURL+"/register", "application/json", bytes.NewReader(jsonPayload))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("期望状态码 %d, 实际 %d", http.StatusBadRequest, resp.StatusCode)
	}

	// 测试无效的状态查询
	statusResp, err := http.Get(suite.bridgeURL + "/status/invalid_token")
	if err != nil {
		t.Fatalf("状态查询请求失败: %v", err)
	}
	defer statusResp.Body.Close()

	if statusResp.StatusCode != http.StatusNotFound {
		t.Errorf("期望状态码 %d, 实际 %d", http.StatusNotFound, statusResp.StatusCode)
	}

	// 测试无效的下载请求
	downloadResp, err := http.Get(suite.bridgeURL + "/download/invalid_token")
	if err != nil {
		t.Fatalf("下载请求失败: %v", err)
	}
	defer downloadResp.Body.Close()

	if downloadResp.StatusCode != http.StatusNotFound {
		t.Errorf("期望状态码 %d, 实际 %d", http.StatusNotFound, downloadResp.StatusCode)
	}

	t.Log("错误处理测试通过")
}

// 测试服务器统计信息
func TestServerStats(t *testing.T) {
	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	// 获取初始统计信息
	resp, err := http.Get(suite.bridgeURL + "/stats")
	if err != nil {
		t.Fatalf("获取统计信息失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("统计信息请求失败，状态码: %d", resp.StatusCode)
	}

	var stats map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("解析统计信息失败: %v", err)
	}

	// 验证统计信息字段
	expectedFields := []string{"status", "uptime", "files_registered", "files_transferred", "active_connections"}
	for _, field := range expectedFields {
		if _, ok := stats[field]; !ok {
			t.Errorf("统计信息缺少字段: %s", field)
		}
	}

	if stats["status"] != "running" {
		t.Errorf("期望状态 'running', 实际: %v", stats["status"])
	}

	t.Log("服务器统计信息测试通过")
}

// 测试健康检查
func TestHealthCheck(t *testing.T) {
	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	resp, err := http.Get(suite.bridgeURL + "/health")
	if err != nil {
		t.Fatalf("健康检查失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查失败，状态码: %d", resp.StatusCode)
	}

	var health map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("解析健康检查响应失败: %v", err)
	}

	if health["status"] != "healthy" {
		t.Errorf("期望状态 'healthy', 实际: %v", health["status"])
	}

	t.Log("健康检查测试通过")
}

// 测试文件过期机制
func TestFileExpiration(t *testing.T) {
	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	// 手动创建一个即将过期的文件
	expiredToken := "expired_test_token"
	suite.bridge.fileRegistry[expiredToken] = &FileMetadata{
		Filename:     "expired.txt",
		Size:         1024,
		Status:       "registered",
		AuthToken:    expiredToken,
		RegisteredAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // 1小时前过期
	}

	// 执行清理
	suite.bridge.cleanupResources()

	// 验证过期文件被清理
	if _, exists := suite.bridge.fileRegistry[expiredToken]; exists {
		t.Error("过期文件未被清理")
	}

	t.Log("文件过期机制测试通过")
}

// 性能基准测试
func BenchmarkFileRegistration(b *testing.B) {
	suite := createIntegrationTestSuite(&testing.T{})
	defer suite.cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload := map[string]interface{}{
			"filename": fmt.Sprintf("bench_%d.txt", i),
			"size":     1024,
		}

		jsonPayload, _ := json.Marshal(payload)
		resp, err := http.Post(suite.bridgeURL+"/register", "application/json", bytes.NewReader(jsonPayload))
		if err != nil {
			b.Fatalf("注册失败: %v", err)
		}
		resp.Body.Close()
	}
}

// 压力测试：大量并发连接
func TestStressConcurrentConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过压力测试")
	}

	suite := createIntegrationTestSuite(t)
	defer suite.cleanup()

	concurrency := 100
	duration := 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup
	errors := make(chan error, concurrency)
	successCount := 0

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				default:
					// 注册文件
					payload := map[string]interface{}{
						"filename": fmt.Sprintf("stress_%d_%d.txt", id, time.Now().UnixNano()),
						"size":     1024,
					}

					jsonPayload, _ := json.Marshal(payload)
					resp, err := http.Post(suite.bridgeURL+"/register", "application/json", bytes.NewReader(jsonPayload))
					if err != nil {
						errors <- fmt.Errorf("注册失败 ID %d: %v", id, err)
						return
					}
					resp.Body.Close()

					if resp.StatusCode == http.StatusOK {
						successCount++
					}

					time.Sleep(100 * time.Millisecond)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// 检查错误
	for err := range errors {
		t.Error(err)
	}

	t.Logf("压力测试完成，成功处理 %d 个请求", successCount)
}

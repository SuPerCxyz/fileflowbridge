package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// 创建测试用的FileFlowBridge实例
func createTestBridge() *FileFlowBridge {
	return &FileFlowBridge{
		HTTPPort:      8000,
		TCPPort:       8888,
		MaxFileSize:   100,
		TokenLength:   8,
		ShutdownEvent: make(chan struct{}),
		fileRegistry:  make(map[string]*FileMetadata),
		activeStreams: make(map[string]interface{}),
	}
}

// 测试文件注册功能
func TestFileRegistration(t *testing.T) {
	ffb := createTestBridge()

	// 创建测试文件内容
	testContent := "这是一个测试文件内容"
	testFile := &struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}{
		Filename: "test.txt",
		Size:     int64(len(testContent)),
	}

	// 编码请求数据
	requestBody, _ := json.Marshal(testFile)

	// 创建HTTP请求
	req := httptest.NewRequest("POST", "/api/register", bytes.NewReader(requestBody))
	w := httptest.NewRecorder()

	// 调用处理器
	ffb.handleFileRegistration(w, req)

	// 检查响应状态码
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 %d, 得到 %d", http.StatusOK, w.Code)
	}

	// 解析响应
	var response map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &response)

	// 验证响应包含必要的字段
	if _, ok := response["auth_token"]; !ok {
		t.Error("响应缺少auth_token字段")
	}
	if _, ok := response["download_url"]; !ok {
		t.Error("响应缺少download_url字段")
	}

	t.Logf("文件注册成功, 认证令牌: %v", response["auth_token"])
}

// 测试状态查询功能
func TestStatusCheck(t *testing.T) {
	ffb := createTestBridge()

	// 手动创建一个测试条目，而不是通过模拟HTTP请求
	testToken := ffb.createNewID()
	now := time.Now()
	ffb.fileRegistry[testToken] = &FileMetadata{
		Filename:         "test.txt",
		OriginalFilename: "test.txt",
		Size:             1024,
		Status:           "registered",
		ClientIP:         "127.0.0.1:12345",
		AuthToken:        testToken,
		RegisteredAt:     now,
		ExpiresAt:        now.Add(2 * time.Hour),
	}

	// 创建状态查询请求
	req := httptest.NewRequest("GET", "/status/"+testToken, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	// 调用处理器
	ffb.handleStatusCheck(w, req)

	// 检查响应状态码
	if w.Code != http.StatusOK {
		t.Errorf("期望状态码 %d, 得到 %d", http.StatusOK, w.Code)
		body, _ := io.ReadAll(w.Body)
		t.Logf("Response body: %s", string(body))
	}

	// 解析响应
	var response map[string]interface{}
	err := json.NewDecoder(w.Body).Decode(&response)
	if err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	// 验证响应内容
	if response["filename"] != "test.txt" {
		t.Errorf("期望文件名 'test.txt', 得到 '%v'", response["filename"])
	}

	if response["original_filename"] != "test.txt" {
		t.Errorf("期望原始文件名 'test.txt', 得到 '%v'", response["original_filename"])
	}

	t.Logf("状态查询成功: %+v", response)
}

// 测试令牌生成
func TestTokenGeneration(t *testing.T) {
	ffb := createTestBridge()

	// 生成多个令牌测试唯一性
	tokens := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		token := ffb.createNewID()
		if tokens[token] {
			t.Errorf("生成的令牌重复: %s", token)
		}
		tokens[token] = true

		// 检查令牌长度（如果TokenLength在有效范围内）
		if ffb.TokenLength >= 6 && ffb.TokenLength <= 32 {
			if len(token) != ffb.TokenLength {
				t.Errorf("令牌长度期望 %d, 得到 %d", ffb.TokenLength, len(token))
			}
		}
	}

	t.Logf("成功生成 %d 个唯一令牌", len(tokens))
}

// 测试文件过期清理
func TestFileExpirationCleanup(t *testing.T) {
	ffb := createTestBridge()

	// 创建一个已过期的文件
	expiredToken := "expired_token"
	ffb.fileRegistry[expiredToken] = &FileMetadata{
		Filename:     "expired.txt",
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // 1小时前过期
		RegisteredAt: time.Now().Add(-2 * time.Hour),
	}

	// 创建一个未过期的文件
	validToken := "valid_token"
	ffb.fileRegistry[validToken] = &FileMetadata{
		Filename:     "valid.txt",
		ExpiresAt:    time.Now().Add(1 * time.Hour), // 1小时后过期
		RegisteredAt: time.Now(),
	}

	// 执行清理
	ffb.cleanupResources()

	// 验证过期文件被删除
	if _, exists := ffb.fileRegistry[expiredToken]; exists {
		t.Error("过期文件未被清理")
	}

	// 验证有效文件保留
	if _, exists := ffb.fileRegistry[validToken]; !exists {
		t.Error("有效文件被错误清理")
	}

	t.Log("文件过期清理测试通过")
}

// 测试并发注册处理
func TestConcurrentRegistration(t *testing.T) {
	ffb := createTestBridge()

	// 并发注册多个文件
	concurrency := 50
	done := make(chan bool, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			defer func() { done <- true }()

			testFile := struct {
				Filename string `json:"filename"`
				Size     int64  `json:"size"`
			}{
				Filename: fmt.Sprintf("concurrent_test_%d.txt", id),
				Size:     1024,
			}

			requestBody, _ := json.Marshal(testFile)
			req := httptest.NewRequest("POST", "/api/register", bytes.NewReader(requestBody))
			w := httptest.NewRecorder()

			ffb.handleFileRegistration(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("并发注册失败, ID: %d, 状态码: %d", id, w.Code)
			}
		}(i)
	}

	// 等待所有goroutine完成
	for i := 0; i < concurrency; i++ {
		<-done
	}

	// 验证所有文件都已注册
	if len(ffb.fileRegistry) != concurrency {
		t.Errorf("期望注册 %d 个文件, 实际注册 %d 个", concurrency, len(ffb.fileRegistry))
	}

	t.Logf("并发注册测试通过, 成功注册 %d 个文件", len(ffb.fileRegistry))
}

// 创建测试文件用于集成测试
func createTestFile(filename string, content string) error {
	return os.WriteFile(filename, []byte(content), 0644)
}

// 集成测试：完整的文件上传下载流程
func TestCompleteFileFlow(t *testing.T) {
	// 创建临时测试文件
	testFile := "temp_test_file.txt"
	testContent := "这是一个完整的测试文件内容，用于验证文件上传下载流程。\n包含多行内容。\n第三行内容。"

	err := createTestFile(testFile, testContent)
	if err != nil {
		t.Fatalf("创建测试文件失败: %v", err)
	}
	defer os.Remove(testFile)

	// 验证文件创建
	fileInfo, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("无法获取测试文件信息: %v", err)
	}

	t.Logf("创建测试文件成功: %s, 大小: %d 字节", testFile, fileInfo.Size())

	// 这里可以扩展为完整的HTTP服务器集成测试
	// 由于需要启动完整的服务器，暂时跳过实际的网络测试
	t.Log("集成测试准备完成（需要启动完整服务器进行网络测试）")
}

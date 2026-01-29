# AGENTS.md - FileFlow Bridge 开发指南

## 项目概述
FileFlow Bridge 是一个高性能文件流桥接工具，使用"流式桥接"技术实现零等待即时文件分发。

## 构建和运行命令

### 构建
```bash
# 构建桥接服务器
go build -o fileflowbridge bridge/main.go

# 构建文件提供者
go build -o fileflowprovider provider/main.go

# 多平台构建
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/fileflowbridge-linux-amd64 bridge/main.go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/fileflowbridge-linux-arm64 bridge/main.go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/fileflowprovider-linux-amd64 provider/main.go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/fileflowprovider-linux-arm64 provider/main.go

# Docker
docker build -t fileflowbridge .
docker-compose up -d
```

### 运行
```bash
# 桥接服务器
./fileflowbridge --http-port=8000 --tcp-port=8888 --max-file-size=100 --token-len=8

# 使用环境变量运行
FFB_HTTP_PORT=8000 FFB_TCP_PORT=8888 FFB_MAX_FILE_SIZE=100 FFB_TOKEN_LEN=8 ./fileflowbridge

# 文件提供者
./fileflowprovider http://localhost:8000 ./your_file.txt
```

### 开发工具
```bash
# 格式化和检查
go fmt ./...
go vet ./...

# 依赖管理
go mod tidy
go mod verify

# 竞态检测器
go run -race bridge/main.go --http-port=8000 --tcp-port=8888
```

## 环境变量
- `FFB_HTTP_PORT`: HTTP服务器端口（默认：8000）
- `FFB_TCP_PORT`: TCP流端口（默认：8888）
- `FFB_MAX_FILE_SIZE`: 最大文件大小，单位GiB（默认：100）
- `FFB_TOKEN_LEN`: 认证令牌长度（默认：8，范围：6-32）
- `FFB_LOG_LEVEL`: 日志级别（默认：INFO）
- `FFB_LOG_PATH`: 日志文件路径（默认：fileflow_bridge.log）

## 代码风格指南

### 语言和运行时
- **语言**: Go 1.24.6（兼容 Go 1.25+）
- **模块**: `fileflowbridge`
- **依赖**: `github.com/google/uuid`, `github.com/gorilla/mux`

### 导入组织
```go
import (
    // 标准库优先
    "fmt"
    "net/http"
    "time"
    
    // 第三方库空行后导入
    "github.com/google/uuid"
    "github.com/gorilla/mux"
)
```

### 命名约定
- **变量**: 驼峰式（`fileMetadata`, `streamConnection`）
- **函数**: 导出函数使用帕斯卡命名法，私有函数使用驼峰式
- **常量**: 大写蛇形命名法（`MAX_BUFFER_SIZE`）
- **结构体**: 帕斯卡命名法（`FileMetadata`, `FlowProvider`）
- **接口**: 帕斯卡命名法，使用描述性名称

### 错误处理
- 始终显式处理错误
- 使用 `fmt.Errorf` 包装错误上下文
- 从函数返回错误，不要 panic

```go
if err != nil {
    return nil, fmt.Errorf("注册文件失败: %v", err)
}
```

### 结构体和JSON标签
- API结构体使用JSON标签
- JSON字段名使用蛇形命名法
- 可选字段包含 `omitempty`

```go
type FileMetadata struct {
    Filename    string    `json:"filename"`
    Size        int64     `json:"size"`
    AuthToken   string    `json:"auth_token"`
    RegisteredAt time.Time `json:"registered_at"`
    ExpiresAt   time.Time `json:"expires_at"`
    StreamStarted time.Time `json:"stream_started,omitempty"`
}
```

### 并发处理
- 使用 `sync.RWMutex` 保护共享状态
- 锁获取应最小化且短暂
- 使用goroutine进行并发操作

```go
ffb.mu.Lock()
defer ffb.mu.Unlock()
// 关键部分
```

### 日志记录
- 使用 `log.Printf` 进行结构化日志记录
- 包含上下文如认证令牌和文件名
- 使用表情符号前缀增强视觉清晰度：
  - 📝 注册
  - 🔗 连接
  - ✅ 成功
  - ❌ 错误
  - ⚠️ 警告

### HTTP处理器
- 遵循验证 → 处理 → 响应模式
- 设置适当的内容类型头
- 使用正确的HTTP状态码
- 始终检查请求体是否为nil

```go
func (ffb *FileFlowBridge) handleFileRegistration(w http.ResponseWriter, r *http.Request) {
    if r.Body == nil {
        http.Error(w, "无效的请求体", http.StatusBadRequest)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(responseData)
}
```

## 项目结构
```
fileflowbridge/
├── bridge/           # 桥接服务器
│   ├── main.go
│   └── static/      # 静态文件目录
│       └── index.html
├── provider/         # 文件提供者客户端
│   └── main.go
├── go.mod
├── Dockerfile
├── docker-compose.yaml
└── .github/workflows/release.yml
```

## 测试
没有正式的测试框架。手动测试方法：
1. 运行桥接服务器
2. 使用提供者上传文件
3. 通过HTTP端点下载
4. 监控日志中的错误

## 给代理的注意事项
- 流式传输应用 - 优先考虑内存效率
- 网络可靠性是关键 - 实现正确的错误处理
- 安全令牌应使用加密安全随机数生成
- 修改代码时保留中文注释
- 性能监控和统计是重要功能
- 日志管理很重要 - 在生产环境中需要配置日志轮转以避免占用过多磁盘空间

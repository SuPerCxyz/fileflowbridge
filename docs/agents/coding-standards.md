# Coding Standards

本文件集中说明 Go 代码约定，避免代理在实现时反复翻长文档。

## 语言与依赖

- Go 版本：`1.24.6`，兼容 `1.25+`
- 模块名：`fileflowbridge`
- 当前第三方依赖：`github.com/google/uuid`、`github.com/gorilla/mux`

## 导入组织

标准库在前，第三方库空行后导入：

```go
import (
    "fmt"
    "net/http"
    "time"

    "github.com/google/uuid"
    "github.com/gorilla/mux"
)
```

## 命名与结构

- 变量使用驼峰式，如 `fileMetadata`
- 导出函数与结构体使用帕斯卡命名法
- 私有函数使用驼峰式
- 常量使用大写蛇形命名法
- 接口名称保持语义清晰，避免过度抽象

API 结构体保留 JSON 标签，字段名使用蛇形命名法；可选字段使用 `omitempty`。

## 错误处理

- 始终显式处理错误
- 优先返回错误，不要 `panic`
- 使用 `fmt.Errorf` 包装上下文

```go
if err != nil {
    return nil, fmt.Errorf("注册文件失败: %v", err)
}
```

## 并发与性能

- 共享状态使用 `sync.RWMutex` 保护
- 锁范围尽量小，避免把 I/O 放进锁内
- 这是流式传输应用，任何改动都要优先考虑内存占用和长连接稳定性

## 日志与 HTTP 处理

- 使用 `log.Printf` 输出带上下文的日志
- 日志保留现有表情前缀风格：`📝`、`🔗`、`✅`、`❌`、`⚠️`
- HTTP 处理器遵循“验证 -> 处理 -> 响应”
- 设置正确的 `Content-Type` 和 HTTP 状态码
- 始终检查请求体是否为空

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

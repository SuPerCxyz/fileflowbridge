# FileFlow Bridge Architecture

```
                   ┌───────────────┐
                   │   Provider    │  (CLI / Browser)
                   │  - has file   │
                   └──────┬────────┘
                          │ 1. POST /register
                          ▼
                   ┌───────────────┐
                   │    Bridge     │  (this repo)
                   │  - no disk    │
                   │  - in-flight  │
                   └──────┬────────┘
                          │ 4. byte stream
                          ▼
                   ┌───────────────┐
                   │  Downloader   │  (curl / browser)
                   └───────────────┘

  2. provider 用 auth_token 走 TCP / HTTP multipart / WebSocket 接入 bridge
  3. downloader GET /download/{token} 拉流
  bridge 把 provider 的字节流"边收边转"给 downloader，全程不落盘
```

---

## 1. 关键设计取舍

### 1.1 零落盘 / 零等待

bridge **不持有任何文件副本**：每次 `Read(provider) → Write(downloader)` 同步在一个 goroutine 内完成。
带来两个直接结果：

- **下载者必须在场**：provider 推送的字节没人收就会阻塞回压到 provider。
- **HTTP Range 不可实现**：bridge 没法 seek，provider 也不重发字节。
  对应实现：`/download` 在收到 `Range:` 时直接返回 `416 + Accept-Ranges: none`。

### 1.2 边传边下的连接拓扑

| 通道           | 适用场景          | 限速点    | seek 能力 |
| -------------- | ----------------- | --------- | --------- |
| TCP            | CLI provider      | 无（应用层 token bucket） | 无 |
| HTTP multipart | 不能开 TCP 的环境 | 上行 bucket | 无 |
| WebSocket      | 浏览器 provider   | 上行 bucket | 无 |

`activeStreams[auth_token]` 在三种通道下分别保存 `*StreamConnection` / `io.Pipe Reader` / `*WebSocketStreamConnection`，都通过 `io.Reader` 抽象给下载侧。

### 1.3 三种"完成"信号源

下载流程结束有 3 个可能原因：

1. 真正读完（`reader.Read → io.EOF` 或 `totalTransferred >= metadata.Size`）。
2. 客户端断开（`r.Context().Done()`）。
3. provider 主动停止（WS `stop_upload` / TCP 关闭）。

只有第 1 种会触发 `transferCompleted=true`，并在 hash 校验通过时记入 `metrics.downloadsComplete`。

---

## 2. 包结构

代码全部在 `bridge` / `provider` 两个 `main` 包内，按职责切到独立文件：

```
bridge/
  main.go                # 入口：flag 解析 + 信号
  server.go              # NewFileFlowBridge / StartServer / buildRouter / gracefulShutdown
  types.go               # FileMetadata / FileFlowBridge / WebSocketStreamConnection / 共享常量
  log.go                 # leveled logger（FFB_LOG_LEVEL）
  util.go                # env / host / scheme / formatFileSize / 浏览器检测 / token 生成
  cors.go                # CORS + APIKey + done channel helper
  ratelimit.go           # 令牌桶（throttledReader/Writer）
  metrics.go             # 内置 Prometheus exposition

  registry_cleanup.go    # 过期清理 + removeFileResources

  stream_tcp.go          # TCP 流接入
  stream_ws.go           # WebSocket 流接入（含心跳 + doneCh 监听）

  handlers_register.go   # POST /register
  handlers_status.go     # GET /status, /stats, /health
  handlers_upload.go     # POST /upload/{token} —— multipart 流式
  handlers_download.go   # GET /download/{token} —— 入口与状态机
  download_pump.go       # reader → ResponseWriter 拷贝循环 + hash + progress
  download_finalize.go   # 收尾：通知 provider + 关连接
  download_page.go       # 浏览器中间页模板

provider/
  main.go                # 单文件：注册 + 推流 + 进度条
```

---

## 3. 并发模型

### 3.1 全局锁

`FileFlowBridge.mu` 是唯一的 `sync.RWMutex`：

- 保护 `fileRegistry / activeStreams / downloadCompleted / downloadCompletedAt / downloadDone / serverStats`。
- 持锁内 **不做网络 IO / 不写 channel**，仅做 map 增删查。
- `closeDoneChLocked` / `getOrCreateDoneChLocked` 显式 "Locked" 后缀，要求调用方提前加锁。

### 3.2 Done channel

`downloadDone[token]` 在注册时创建，下载完成或资源回收时关闭。
所有等待方（`handleFileUpload` / WS 心跳 / WS 读循环）都通过 `select { case <-doneCh: ... }` 退出，无轮询。

### 3.3 优雅关闭

```
SIGINT/SIGTERM
   ↓
TriggerShutdown()  // sync.Once 防重复 close
   ↓
close(ShutdownEvent)
   ↓
StartServer 从 <-ShutdownEvent 返回
   ↓
gracefulShutdown:
   - 收集 activeStreams 的 token（在锁内）
   - 锁外逐个 removeFileResources（避免再次加锁死锁）
   - httpServer.Shutdown(ctx, 5s)
   - tcpListener.Close()
```

---

## 4. 安全模型

| 风险                          | 缓解                                          |
| ----------------------------- | --------------------------------------------- |
| 任意第三方占用 token / 带宽    | `FFB_API_KEY`（注册时强制 X-API-Key）         |
| 跨站 WS 劫持                  | `FFB_ALLOWED_ORIGINS` 收窄                    |
| 计时侧信道                    | `crypto/subtle.ConstantTimeCompare`           |
| token 可被猜测                | `crypto/rand` 生成；可调长 16-32              |
| 明文传输                      | 内置 `--tls-cert/--tls-key` 或反代 TLS        |
| 路径穿越                      | 文件名仅用于 Content-Disposition，不参与路径解析 |
| DDoS（注册风暴）              | 当前**未实现**注册风暴限流；建议反代层 + APIKey |
| 文件被篡改                    | SHA256 校验（注册时提交 → 下载完成时比对）    |

---

## 5. 指标 (Prometheus)

`/metrics` 输出 0.0.4 text exposition：

```
# HELP ffb_uptime_seconds Server uptime in seconds.
# TYPE ffb_uptime_seconds gauge
ffb_uptime_seconds 123.45
# HELP ffb_files_registered_total Total number of files registered.
# TYPE ffb_files_registered_total counter
ffb_files_registered_total 42
... (downloads / bytes / hash_mismatch / active_streams / peak_connections)
```

为避免引入 `prometheus/client_golang` 体积依赖，桥内部使用 `atomic.Int64` 自实现。

---

## 6. 限速

`ratelimit.go` 提供进程级 token bucket：

- 容量 = 1 秒带宽（允许 1s 突发）
- `wait(n)` 阻塞直到能取 n 个令牌
- `throttledReader` / `throttledWriter` 分别封装上下行 IO

**Per-connection 独立桶**：每个 upload / download handler 会**单独 `newTokenBucket`**。
含义：`--upload-bps=10MB/s` 在 N 个并发连接下总带宽上限大约是 `N * 10MB/s`。
原因：在边传边下的模型下，单个连接才是有意义的限速单位；做全局限速会让所有
慢连接互相阻塞。若需要总带宽天花板，请在反向代理层（Caddy / Nginx）做。

---

## 7. 与 v1 的兼容性

| 变化                                | 影响 |
| ----------------------------------- | ---- |
| /register 接受新字段 `sha256`/`max_downloads` | 兼容：旧 provider 不传也工作 |
| /download 加 `Accept-Ranges: none` + 416 | 兼容：旧客户端不发 Range |
| /metrics 端点新增                     | 兼容：旧客户端无感 |
| 路由从 gorilla/mux 迁移到 chi v5      | 仅内部实现，URL/method 一致 |
| 默认 Allowed Origins 仍是 `*`         | 兼容：建议生产显式收窄 |
| `removeFileResources` 不再清完成标记 | 行为修正：完成标记由 cleanupExpiredFiles 单独按 TTL 清理 |

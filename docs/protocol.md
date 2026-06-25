# FileFlow Bridge Protocol (v3)

本文档描述 FileFlow Bridge 桥接服务的对外 HTTP / WebSocket / TCP 协议。
适用于 v3.x 实现（含 SHA256 校验、多接收端预留、Prometheus、限速、并发上传上限、resumable chunked 上传）。

---

## 1. 角色

- **Provider（提供者）**：拥有文件本体，主动连接桥进行推送。
  CLI 或浏览器都可作为 provider。
- **Bridge（桥）**：协调连接、转发字节流；不落盘。
- **Downloader（下载者）**：通过桥提供的 download URL 直接下载文件。

---

## 2. 端口与端点

默认监听：

| 端口 | 协议   | 用途                                     |
| ---- | ------ | ---------------------------------------- |
| 8000 | HTTP/WS| `/register`、`/upload/{token}`、`/ws/{token}`、`/download/{token}[/{filename}]`、`/status/{token}`、`/stats`、`/health`、`/metrics` |
| 8888 | TCP    | CLI provider 的字节流推送通道            |

可通过 `--http-port` / `--tcp-port` 或环境变量 `FFB_HTTP_PORT` / `FFB_TCP_PORT` 修改。

---

## 3. 注册 `POST /register`

### 请求体

```json
{
  "filename": "example.zip",
  "size": 1048576,
  "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "max_downloads": 1,
  "resumable": false,
  "chunk_size": 0
}
```

| 字段           | 类型    | 是否必填 | 说明 |
| -------------- | ------- | -------- | ---- |
| filename       | string  | 是       | 显示给下载者的文件名。仅用于 Content-Disposition，不参与路径解析。 |
| size           | int64   | 是       | 字节数；必须 `<= FFB_MAX_FILE_SIZE`（默认 100 GiB）。 |
| sha256         | string  | 否       | 文件 SHA256，小写 64 位十六进制；提交后桥在每次下载完成时校验。 |
| max_downloads  | int     | 否       | `<=1`：单次下载（默认，兼容 v1）。`N>1`：多接收端模式（当前实现尚未启用，见 §6）。 |
| resumable      | bool    | 否       | `true` 时启用 chunked 上传模式。bridge 会预分配临时文件并允许 provider 通过 `PUT /upload/{token}/chunk?index=N` 分块续传。 |
| chunk_size     | int64   | 否       | resumable 模式下期望的单块字节数；缺省或非法时 bridge 强制夹到 `[4 KiB, 1 GiB]` 并使用默认 8 MiB。 |

### 请求头

| 头             | 说明 |
| -------------- | ---- |
| `Content-Type` | 必须为 `application/json` |
| `X-API-Key`    | 桥启用了 `FFB_API_KEY` 时必填；也支持 `Authorization: Bearer <key>` |

### 响应

```json
{
  "auth_token": "abc12345",
  "tcp_endpoint": { "host": "bridge.example.com", "port": 8888 },
  "download_url": "https://bridge.example.com/download/abc12345/example.zip",
  "expires_at": "2026-06-24T18:00:00Z",
  "original_filename": "example.zip",
  "max_downloads": 1,
  "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "resumable": true,
  "chunk_size": 8388608,
  "total_chunks": 13,
  "chunk_upload_url": "https://bridge.example.com/upload/abc12345/chunk",
  "chunk_status_url": "https://bridge.example.com/upload/abc12345/status"
}
```

`auth_token` 长度由 `--token-len` 决定（默认 8，范围 6-32，超出回退到 UUID）。
注册有效期固定 2 小时；过期后所有相关资源被清理。

启用 `resumable` 时响应额外返回 `chunk_size`、`total_chunks`、`chunk_upload_url`、`chunk_status_url`；
非 resumable 注册保持兼容，不返回这些字段。

### 错误

| 状态 | 含义 |
| ---- | ---- |
| 400  | JSON 无效 / filename 为空 / sha256 不合法 |
| 401  | API Key 缺失或错误 |
| 413  | 文件大小超过限制 |

---

## 4. 推送数据（Provider → Bridge）

注册成功后，provider 必须通过下面四条通道之一推送字节流。前三条 stream 通道在协议上互斥；
重复使用同一 token 建立第二条 stream 连接时，旧连接会被桥**主动关闭**（防资源泄漏）。
resumable 模式（注册时 `resumable=true`）只能使用第 4 条 chunk 通道，与前 3 条互斥。

> **并发上限**：所有上传通道共享一个进程级信号量，容量由 `--max-parallel-uploads`
> （默认 10）控制。槽位耗尽时新上传在 2 秒内未拿到槽返回 `429 Too Many Requests`。
> `0` 或负数表示不限。
>
> **公平性提示**：resumable 模式下单个 chunk PUT 在上传完成前持有槽位，
> chunk 越大持有时间越长。`chunk_size` 上限为 1 GiB，因此单个 chunk 上传理论上
> 可独占一个槽位数十秒到数分钟。若部署在公共环境，建议保持默认 8 MiB 左右的
> chunk size，并适当上调 `--max-parallel-uploads`。

### 4.1 TCP 通道（推荐 CLI provider）

1. provider 连接 `tcp_endpoint.host:port`。
2. 写入一行 JSON 元数据（`\n` 结尾）：

   ```json
   {"auth_token":"abc12345","filename":"example.zip"}
   ```

3. bridge 校验通过后返回单行 `STREAM_READY\n`，否则返回 `INVALID_CONNECTION\n` 并断开。
4. provider 直接将文件字节流写入连接；EOF 表示发送结束。
5. bridge 会保持连接直到下载完成 / 客户端断开 / 超时；provider 可以读到一个 EOF 作为完成信号。
6. resumable token 走 TCP 会被立即拒绝，必须改用 §4.4。

### 4.2 HTTP multipart 通道

`POST /upload/{auth_token}`，Content-Type 必须以 `multipart/form-data` 开头，文件字段必须命名为 `file`。

- 桥使用 `MultipartReader` 流式读取，**不落盘**。
- 响应仅在传输结束后回写，等待下载端完成或服务器关闭。
- resumable token 命中此入口返回 `400`，提示改用 chunked PUT。

### 4.3 WebSocket 通道（浏览器 provider）

`GET /ws/{auth_token}`：

- 升级成功后桥发送 `{"command":"READY"}`。
- provider 将文件以**二进制帧**发送；text 帧用于控制消息。
- 桥每 30 秒发 Ping；90 秒没收到 Pong 则关连接。
- 控制消息（text）：

  ```jsonc
  // provider → bridge
  { "command": "stop_upload" }            // provider 主动放弃
  { "command": "request_data", "offset": 0, "size": 1048576 }  // 浏览器拉模式

  // bridge → provider
  { "command": "READY" }                  // 握手完成
  { "command": "send_chunk", "offset": 0, "size": 1048576 }    // 触发推送
  { "command": "download_started" }       // 下载端已上线
  { "command": "progress", "bytes": 524288 }                   // 真实已转发字节
  { "command": "transfer_complete", "message": "..." }         // 收尾
  ```

- resumable token 命中此入口返回 `400`。

### 4.4 Chunked PUT 通道（resumable 模式）

仅当注册请求声明 `resumable=true` 时启用。bridge 在注册时为该 token 创建临时文件
`<temp-dir>/<token>.part` 并按 `size` 预分配，同时初始化长度为 `total_chunks` 的位图。

#### 4.4.1 上传单个 chunk

```
PUT /upload/{auth_token}/chunk?index={N}
Content-Type: application/octet-stream
Content-Length: <expected_chunk_bytes>
<二进制 chunk body>
```

- `index` 取值范围 `0..total_chunks-1`，越界返回 `400`。
- 非末块的字节数必须等于 `chunk_size`；末块字节数等于 `size - (total_chunks-1)*chunk_size`。
- 必须显式声明 `Content-Length` 且等于期望长度；缺失返回 `411 Length Required`，
  不匹配返回 `400`（不支持 chunked transfer encoding）。
- 重复上传同一 `index` 是**幂等**的（覆盖位图位与同段字节，但 `received_bytes` 不重复计入）。
- 已 `upload_ready` 后再次提交立即返回 `200` 并附最新状态。
- 上传槽位被全局信号量约束，槽满返回 `429 Too Many Requests`。
- token 已被清理（过期 / 下载完成）时返回 `410 Gone`。

成功响应（`200`）始终是 §4.4.3 描述的状态 JSON。

#### 4.4.2 查询上传状态 / 续传探测

```
GET /upload/{auth_token}/status
```

返回当前 chunk 位图，供 provider 在重连/续传时知道哪些 chunk 还需要发送。
Provider 也可以直接选择重发任意 chunk，bridge 幂等处理。

#### 4.4.3 状态 JSON

```json
{
  "resumable": true,
  "total_chunks": 13,
  "chunk_size": 8388608,
  "size": 104857600,
  "received_bytes": 33554432,
  "missing_chunks": [4, 5, 6, 7, 8, 9, 10, 11, 12],
  "missing_count": 9,
  "upload_ready": false
}
```

| 字段              | 含义 |
| ----------------- | ---- |
| received_bytes    | 已被 bridge 写入临时文件、且首次计入的字节数（不重复计入幂等重传） |
| missing_chunks    | 仍未收到的 chunk index 列表；为防 DoS 单次响应最多返回前 10000 项 |
| missing_count     | 实际剩余的 chunk 数（即使 `missing_chunks` 被截断，本字段仍为真实值） |
| missing_truncated | 仅当 `missing_chunks` 被截断时出现并为 `true`；客户端应先补这一批前缀再次查询 |
| upload_ready      | 所有 chunk 到齐后置 `true`；此后下载端访问 `/download/{token}` 直接由临时文件服务（支持 `Range`） |

> 注：`chunk_size` 范围被夹到 `[64 KiB, 1 GiB]`；过小或过大会被服务端调整后返回真实值。
> resumable 续传的语义是「同一 token 内的 chunk 续传」。注册时 bridge 即根据 `size`
> 在 `--temp-dir` 预留 `.part` 文件（权限 `0o600`，目录 `0o700`），TTL 内随时可以续传。
> Token 过期或注册被清理后必须重新 `/register`。

---

## 5. 下载 `GET /download/{auth_token}[/{filename}]`

- 浏览器 User-Agent（Mozilla/Chrome/...）：返回 HTML 中间页（`/static/download.html` 或内置模板）。
- 命令行（curl/wget/python-urllib/...）：直接流式响应字节。
- `HEAD` 返回响应头但不传输 body。
- **Range 行为按 token 模式区分**：
  - **stream 模式（非 resumable）**：实时透传，无法重放，请求带 `Range` 时返回
    `416 Requested Range Not Satisfiable` + `Accept-Ranges: none`。
  - **resumable 模式**：所有 chunk 到齐（`upload_ready=true`）后，bridge 用 `http.ServeContent`
    服务落盘的临时文件，**原生支持** `Range`、`If-Range`、`HEAD`、`If-Modified-Since`。
    若仍在收 chunk，返回 `425 Too Early` + `Retry-After: 5`。
- 响应头（stream 模式）：

  | 头                         | 含义 |
  | -------------------------- | ---- |
  | Content-Type               | `application/octet-stream` |
  | Content-Length             | 当注册时提供了 size 时设置 |
  | Content-Disposition        | `attachment; filename="..."` |
  | Accept-Ranges              | `none` |
  | X-FileFlow-FileID          | `auth_token` |
  | X-FileFlow-Original-Filename | 原始文件名 |
  | X-FileFlow-SHA256          | provider 提交的 SHA256（如有） |

- 响应头（resumable 模式）：由 `http.ServeContent` 自动负责 `Content-Length`、`Content-Range`、
  `Accept-Ranges: bytes`、`Last-Modified` 等；同时附加上表中的 `X-FileFlow-*` 头。

### 状态码

| 状态 | 含义 |
| ---- | ---- |
| 200  | 正常流式响应（或 resumable 模式完整体） |
| 206  | resumable 模式部分内容（Range） |
| 404  | token 不存在或已过期 |
| 409  | 单次下载模式下已有 in-flight 下载 |
| 410  | 单次模式下载完成 / 多次模式次数耗尽 / 完成后 1 分钟内 |
| 416  | stream 模式的 Range 请求（不支持） |
| 425  | resumable 模式所有 chunk 尚未到齐 |
| 503  | provider 还没建立流连接（30s 内 retry-after 风格） |

---

## 6. 多接收端 (1→N)

> ⚠️ **当前版本不支持**。`/register` 收到 `max_downloads > 1` 时返回 `501 Not Implemented`。
>
> 在零落盘 / 边收边转的架构下，provider 推送的是不可重发的顺序流，
> 单文件无法天然 fan-out 给多个下载者。后续如需支持，需要引入：
>
> - provider 协议层面的 chunked-by-offset（每段可独立重发）
> - 或在 bridge 维护中间缓存（与"零落盘"卖点冲突）
>
> 字段保留以备未来扩展；当前实现仅响应 0 / 1。

---

## 7. SHA256 校验

- 注册时提交 `sha256` → 桥在下载完成时按字节流计算 SHA256 并对比。
- **不匹配时**：bridge 调用 `panic(http.ErrAbortHandler)` 截断响应。
  下载者收到的 body 长度 < `Content-Length`，标准 HTTP 客户端（curl、wget、浏览器）
  会判定为下载失败。同时 metrics `ffb_hash_mismatch_total` +1。
- 不提交 `sha256` 等同于不校验。

---

## 8. 状态 / 健康 / 指标

| Path              | 方法 | 用途 |
| ----------------- | ---- | ---- |
| `/status/{token}` | GET  | 单文件元数据（含 status、download_completed、completed_downloads） |
| `/stats`          | GET  | 全局粗粒度统计 |
| `/health`         | GET  | liveness：返回 `status: healthy` |
| `/metrics`        | GET  | Prometheus 0.0.4 text exposition；启用 `--metrics-key` 时需 `X-Metrics-Key` 头 |

---

## 9. 限速与并发

- `--upload-bps` / `--download-bps`（或环境变量）配置全局令牌桶；`0` 表示不限速。
  详见 architecture.md。
- `--max-parallel-uploads` / `FFB_MAX_PARALLEL_UPLOADS`：所有上传通道共享的活跃任务上限，
  默认 `10`，`0/负数=不限`。槽位耗尽时新上传在 2 秒内未拿到槽返回
  `429 Too Many Requests`（计入 `ffb_uploads_rejected_total`）。
- `--temp-dir` / `FFB_TEMP_DIR`：resumable 模式下 chunk 落盘的根目录，默认 OS 临时目录下
  `fileflow-bridge`。token 过期 / 下载完成 / 服务关闭时由清理流程移除 `.part` 文件。

---

## 10. 安全建议

1. 部署时 **设置 `FFB_API_KEY`** + **收窄 `FFB_ALLOWED_ORIGINS`**，避免被任意第三方占用 token / 占带宽。
2. 公网部署优先用 `--tls-cert/--tls-key` 启用内置 HTTPS；
   反代场景由 Caddy/Nginx 转发并保留 `X-Forwarded-Proto`。
3. download URL 即权限：拿到 URL 的人可以下载文件本体。
4. 文件名仅用于 Content-Disposition，不参与路径解析，但 provider 应自行 sanitize（避免奇怪字符）。

# 🌊 FileFlow Bridge 使用指南

`FileFlow Bridge` 是一款高性能的文件流转桥接工具。它采用 **“边传边下” (Streaming Bridge)** 技术，打破了传统中转服务器“先完整上传、再分发下载”的模式，实现零等待的即时文件分发。

---

## 🚀 核心特性

* **零时延分发**：文件注册后立即获取下载链接，无需等待上传完成。
* **内存友好**：默认 stream 模式下数据在 TCP 隧道与 HTTP 响应间实时透传，不占用服务器磁盘。
* **极简配置**：支持命令行参数与环境变量，配置灵活。
* **生产可观测**：内置 `/metrics` (Prometheus)、`/stats`、`/health` 三套端点。
* **可选 SHA256 校验**：provider 在 `/register` 提交 hash，bridge 在下载结束时按字节校验。
* **多接收端 (1→N)**：`max_downloads=N` 模式允许同一文件被多次下载。
* **可选 API Key 鉴权**：`FFB_API_KEY` 启用后 `/register` 强制校验。
* **内置 HTTPS / WSS**：不再强依赖反向代理。
* **全局限速**：上下行各自的令牌桶，按需限速。
* **并行上传上限**：所有上传通道共享的进程级信号量，默认 10，可通过 `--max-parallel-uploads` / `FFB_MAX_PARALLEL_UPLOADS` 调整。
* **断点续传 (resumable)**：可选 chunked 上传模式，provider 中断后可从已收到的 chunk 续传；下载端原生支持 HTTP `Range`。
* **优雅关闭**：SIGINT/SIGTERM 安全收尾，并按 TTL 清理过期资源。

---

## 🌐 公共节点 (Quick Start)
为了方便快速测试，你可以直接使用我们维护的公共演示节点，无需自行部署服务端：

* 公共`Bridge`地址: https://ffb.soocoo.xyz

* 用法示例:
    ```
    ./fileflowprovider https://ffb.soocoo.xyz ./你的文件.zip
    ```

---

## 🛠️ 服务端部署 (Bridge Server)

服务端负责协调连接并提供 HTTP 访问入口。

构建命令、开发验证命令和完整环境变量说明已拆分到 [docs/agents/runtime-and-commands.md](docs/agents/runtime-and-commands.md)。本 README 仅保留用户部署和使用时最常用的信息。

### 1. Docker 部署 (推荐)

最简单的部署方式是使用Docker，官方提供了预构建镜像：

```bash
# 使用预构建镜像运行（推荐）
docker run -d --name fileflowbridge -p 8000:8000 -p 8888:8888 superc/ffbridge

# 或使用自定义配置运行
docker run -d --name fileflowbridge \
  -p 8080:8080 \
  -p 9999:9999 \
  -e FFB_HTTP_PORT=8080 \
  -e FFB_TCP_PORT=9999 \
  -e FFB_MAX_FILE_SIZE=50 \
  -e FFB_TOKEN_LEN=16 \
  -e FFB_LOG_LEVEL=DEBUG \
  superc/ffbridge
```

或者使用Docker Compose：

```bash
# 使用docker-compose（自动拉取预构建镜像）
docker-compose up -d
```

对于开发用途，也可以从源码构建：

```bash
# 首先确保bin目录中有预构建的二进制文件
mkdir -p bin
GOOS=linux GOARCH=amd64 go build -o bin/fileflowbridge-linux-amd64 bridge/main.go

# 然后构建Docker镜像
docker build -t fileflowbridge .
```

### 2. 直接运行二进制文件

你也可以直接运行预编译的二进制文件：

```bash
./fileflowbridge --http-port=8000 --tcp-port=8888 --max-file-size=100 --token-len=16
```

### 3. 常用配置参数

程序按以下优先级读取配置：**命令行参数 > 环境变量 > 默认值**。

| 配置项 | 命令行参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| **HTTP 端口** | `--http-port` | `FFB_HTTP_PORT` | `8000` | 对外提供访问与下载的 **HTTP** 端口 |
| **TCP 端口** | `--tcp-port` | `FFB_TCP_PORT` | `8888` | 接收文件流推送的内网/外网 TCP 端口 |
| **最大文件限制** | `--max-file-size` | `FFB_MAX_FILE_SIZE` | `100` | 允许注册的最大文件大小 (**单位: GiB**) |
| **AuthToken 长度** | `--token-len` | `FFB_TOKEN_LEN` | `8` | 注册时生成的 **AuthToken** 长度（6-32 位） |
| **API Key** | `--api-key` | `FFB_API_KEY` | _空_ | 启用后 `/register` 必须携带 `X-API-Key` 头 |
| **Metrics Key** | `--metrics-key` | `FFB_METRICS_KEY` | _空_ | 启用后 `/metrics` 必须携带 `X-Metrics-Key` 头（与 API Key 独立） |
| **并行上传上限** | `--max-parallel-uploads` | `FFB_MAX_PARALLEL_UPLOADS` | `10` | 所有上传通道（TCP / WS / multipart / chunk PUT）共享的活跃任务上限，`0`/负数=不限。超过时新上传 2 秒内未拿到槽返回 429 |
| **Resumable 临时目录** | `--temp-dir` | `FFB_TEMP_DIR` | OS 临时目录下 `fileflow-bridge` | resumable 模式下 chunk 落盘的根目录 |
| **允许 Origin** | `--allowed-origins` | `FFB_ALLOWED_ORIGINS` | `*` | CORS / WebSocket 白名单，逗号分隔 |
| **HTTPS 证书** | `--tls-cert` | `FFB_TLS_CERT` | _空_ | 与 `--tls-key` 一起设置时启用内置 HTTPS |
| **HTTPS 私钥** | `--tls-key` | `FFB_TLS_KEY` | _空_ | TLS 私钥路径 |
| **上行限速** | `--upload-bps` | `FFB_UPLOAD_BPS` | `0` | **per-connection** 上行限速（B/s），0 = 不限 |
| **下行限速** | `--download-bps` | `FFB_DOWNLOAD_BPS` | `0` | **per-connection** 下行限速（B/s），0 = 不限 |
| **日志级别** | 无 | `FFB_LOG_LEVEL` | `INFO` | `DEBUG/INFO/WARN/ERROR` |
| **日志路径** | 无 | `FFB_LOG_PATH` | `fileflow_bridge.log` | 日志文件保存路径 |

#### 监控

- `GET /health`：liveness 健康检查
- `GET /stats`：JSON 粗粒度统计
- `GET /metrics`：Prometheus 0.0.4 text exposition（无需 client_golang，内置实现）

更完整的协议与架构说明：

- [docs/protocol.md](docs/protocol.md)
- [docs/architecture.md](docs/architecture.md)
- [docs/agents/runtime-and-commands.md](docs/agents/runtime-and-commands.md)

---

## 📤 提供端使用 (File Provider)

提供端用于将本地文件"映射"到云端，并通过流方式推送数据。

### 使用方法

```bash
./fileflowprovider [flags] <桥接服务器URL> <文件路径>
```

可用 flags：

| flag             | 说明 |
| ---------------- | ---- |
| `--hash`         | 上传前在本地计算 SHA256，并提交给桥用于校验 |
| `--sha256=<hex>` | 直接指定 64 位十六进制 SHA256，优先级高于 `--hash` |
| `--max-downloads=N` | 允许的最大下载次数；`0/1` 为单次（默认） |
| `--api-key=<key>` | 桥启用 API Key 鉴权时必填（也可用 `FFB_API_KEY` 环境变量） |
| `--resumable`    | 启用断点续传：使用 chunked PUT 上传；中断后再次以相同源文件运行可续传，下载端原生支持 HTTP `Range` |
| `--chunk-size=<bytes>` | resumable 模式下的单块大小，`0` 使用服务器默认（8 MiB） |
| `--upload-conc=<N>` | resumable 上传并发数，默认 `4`；受 bridge 端 `--max-parallel-uploads` 节流 |
| `--retries=<N>`  | 单个 chunk 失败后的重试次数，默认 `5` |
| `--wait-timeout=<duration>` | 上传完成后等待下载完成的最长时间，默认 `30m` |

> 注意：服务端地址必须包含 `http://` 或 `https://` 前缀。

**示例：**

```bash
# 基础用法
./fileflowprovider http://1.2.3.4:8000 /home/data/large_video.mp4

# 带 SHA256 校验 + 允许 3 人下载
./fileflowprovider --hash --max-downloads=3 https://ffb.soocoo.xyz ./file.zip

# 携带 API Key
FFB_API_KEY=secret ./fileflowprovider --hash https://ffb.soocoo.xyz ./file.zip

# 启用断点续传（适合大文件、不稳定网络）
./fileflowprovider --resumable --hash https://ffb.soocoo.xyz ./big.iso

# 续传：原命令重新执行同样的源文件即可，bridge 会跳过已收到的 chunk
./fileflowprovider --resumable --hash https://ffb.soocoo.xyz ./big.iso
```
```

### 执行流程

1. **注册**：向服务端申请文件认证令牌。
2. **生成链接**：终端输出唯一的 HTTP 下载地址。
3. **流式传输**：当有人访问下载地址时，提供端会立即通过 TCP 隧道向服务端推送数据。

---

## 🔧 API 接口

FileFlow Bridge 提供以下 REST API 接口：

* `/register` - 注册新文件
* `/upload/{auth_token}` - 上传文件（支持multipart表单）
* `/download/{auth_token}` - 下载文件
* `/download/{auth_token}/{filename}` - 按文件名下载
* `/ws/{auth_token}` - WebSocket连接（用于浏览器上传）
* `/status/{auth_token}` - 查询文件状态
* `/stats` - 获取服务器统计信息
* `/health` - 健康检查接口

---

---

## 📖 运行示例 (Demo)
当你运行`fileflowprovider`后，程序会立即返回一个下载链接。你只需将该链接发送给接收者，对方点击即可开始下载。

```
[root@test ~]# ./fileflowprovider https://ffb.soocoo.xyz test_file
📝 注册文件中...
📁 原始文件名: test_file
🔗 点击或双击复制下载地址:
https://ffb.soocoo.xyz/download/hU50yWYu/test_file

# --- 此时，接收者在浏览器打开上述链接，传输会自动开始 ---

🔗 建立流连接...
✅ 流连接已建立，开始传输文件...
📤 上传中 [==================================================] 100.0% (100.00 MiB / 100.00 MiB)
📊 传输统计: 100.00 MiB, 耗时 5.00 秒, 平均速度: 19.99 MiB/s
🎉 文件传输完成!

============================================================

📥 下载信息:

• 文件名称: test_file
• 文件大小: 100.00 MiB
• 下载URL: https://ffb.soocoo.xyz/download/hU50yWYu/test_file
• 有效时间: 下载完成后自动失效

💡 提示: 请确保发送端保持运行，直到下载完成。

============================================================
✅ 操作完成! 文件已准备好下载
💡 注意: 文件下载完成后，下载链接将自动失效
```

---

## ⚠️ 注意事项

* **单次有效**：默认 stream 模式下，下载地址在完成后立即失效，资源自动释放。
* **断点续传**：默认（stream）模式仍是实时透传，**不支持** Range 与中断续传；如需断点续传请在 provider 使用 `--resumable`，bridge 会将 chunk 落到 `--temp-dir`，下载端原生支持 HTTP `Range`。
* **防火墙策略**：请确保服务端定义的 `HTTP 端口` 和 `TCP 端口` 在防火墙或安全组中已开放。
* **安全性**：`AuthToken` 是 File Provider 连接 Bridge Server 进行流传输的唯一凭证。增加 `--token-len` 可以有效防止暴力破解
* **服务端资源**：请确保服务端有足够的网络带宽和内存资源以支持高并发传输
* **日志管理**：在生产环境中，建议配置日志轮转以避免占用过多磁盘空间。Docker部署方案已内置日志大小限制。
* **静态文件**：服务器支持静态文件服务，会自动提供 `bridge/static` 目录下的文件。

---

## 📚 补充文档

* 用户部署与日常使用：当前 [README.md](README.md)
* 开发容器与调试流程：[DEVELOPMENT.md](DEVELOPMENT.md)
* 构建、运行、环境变量全集：[docs/agents/runtime-and-commands.md](docs/agents/runtime-and-commands.md)
* 测试执行用项目用户手册：[docs/project-user-manual.md](docs/project-user-manual.md)
* 浏览器异常测试脚本：[docs/browser-anomaly-tests.md](docs/browser-anomaly-tests.md)
* 统一异常测试入口：[docs/all-anomaly-tests.md](docs/all-anomaly-tests.md)

### 快速测试入口

```bash
# Go 基础验证
npm test
npm run vet

# 仅浏览器异常场景
npm run browser:anomaly

# 统一运行 Go + 正式异常测试
npm run anomaly:all

# 发布前检查
npm run release:check
```

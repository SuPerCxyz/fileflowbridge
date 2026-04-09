# FileFlow Bridge 项目用户手册

本手册面向后续人工测试执行。内容基于当前代码实现、静态页面和现有测试用例整理，不以旧版 README 描述为准。

## 1. 手册目标

- 说明项目当前真实具备的功能边界
- 给出可直接执行的测试准备和测试步骤
- 标明哪些路径是主路径，哪些路径实现存在但测试覆盖较弱

## 2. 项目功能总览

FileFlow Bridge 是一个“注册文件 -> 建立流连接 -> 下载端触发传输 -> 传输完成即释放资源”的文件流桥接系统。

当前项目由三个主要部分组成：

1. `bridge/main.go`
   作用：桥接服务端，提供 HTTP API、下载入口、WebSocket 上传入口、TCP 流入口、状态和统计接口。
2. `provider/main.go`
   作用：命令行发送端客户端。先调用 `/register` 注册文件，再通过 TCP 连接把文件内容流式推送给桥接端。
3. `bridge/static/index.html` 与 `bridge/static/download.html`
   作用：浏览器上传页和浏览器下载页。浏览器上传主路径依赖 WebSocket，不走 CLI Provider。

## 3. 基于代码确认的核心行为

### 3.1 文件生命周期

1. 客户端先调用 `/register`
2. 服务端生成 `auth_token`
3. 服务端返回下载链接、TCP 目标地址和过期时间
4. 发送端通过 TCP 或 WebSocket 提供文件流
5. 下载端访问 `/download/{auth_token}` 或 `/download/{auth_token}/{filename}`
6. 文件传输完成后，服务端立即标记完成并清理资源

结论：

- 下载链接默认是一次性资源
- 注册后的默认过期时间是 2 小时
- 传输完成后再次访问同一链接，预期会得到 `404` 或 `410`

### 3.2 当前支持的上传方式

#### A. CLI Provider + TCP 流

这是最成熟、最明确的主路径。

- 入口程序：`provider/main.go`
- 注册方式：HTTP `POST /register`
- 传输方式：TCP，发送一行 JSON 元数据后等待 `STREAM_READY`
- 特点：下载链接在注册成功后立即可见，随后发送端建立 TCP 流连接并开始等待下载

#### B. 浏览器上传页 + WebSocket

这是浏览器主路径。

- 入口页面：`/`
- 上传方式：浏览器先注册文件，再连接 `/ws/{auth_token}`
- 真实传输时机：下载端开始下载后，桥接端通过 WebSocket 发 `send_chunk`
- 特点：浏览器页面会先展示下载链接，真正文件数据在下载触发后才开始发送

#### C. HTTP Multipart 上传

接口存在，但不是当前默认主路径。

- 接口：`POST /upload/{auth_token}`
- 要求：`multipart/form-data`，字段名为 `file`
- 现状：代码实现存在，但现有测试对这条路径覆盖较弱，浏览器首页也不走这条路径

建议：

- 人工测试优先覆盖 A 和 B
- C 作为补充测试项，不建议把它作为首个验收路径

### 3.3 当前支持的下载方式

#### A. 命令行/程序化下载

- 使用 `curl`、`wget` 或其他 HTTP 客户端访问下载地址
- 服务端直接返回二进制流

#### B. 浏览器下载

- 浏览器访问下载地址时，服务端会先返回下载页
- 下载页中点击按钮后，再访问真实下载链接

注意：

- 浏览器请求和命令行请求的行为不同
- 浏览器访问 `/download/...` 不一定直接开始下载，通常会先看到下载页

### 3.4 当前可用接口

| 接口 | 方法 | 作用 | 建议测试等级 |
| --- | --- | --- | --- |
| `/register` | `POST` | 注册文件，返回 token、下载地址、TCP 端点 | 必测 |
| `/upload/{auth_token}` | `POST` | HTTP multipart 上传 | 选测 |
| `/ws/{auth_token}` | `GET` | 浏览器上传 WebSocket | 必测 |
| `/download/{auth_token}` | `GET` | 下载入口 | 必测 |
| `/download/{auth_token}/{filename}` | `GET` | 带文件名的下载入口 | 必测 |
| `/status/{auth_token}` | `GET` | 查询文件状态 | 必测 |
| `/stats` | `GET` | 查询服务端统计信息 | 必测 |
| `/health` | `GET` | 健康检查 | 必测 |

## 4. 现有测试覆盖结论

仓库已有测试文件：

- `bridge/bridge_test.go`
- `bridge/integration_test.go`
- `bridge/enhanced_test.go`

这些测试已经覆盖了以下方向：

- 文件注册
- 状态查询
- token 生成
- WebSocket 传输
- 错误处理
- 健康检查
- 统计接口
- 并发注册/并发操作
- 过期资源清理
- 带文件名下载地址
- 连接中断处理

覆盖相对较弱的方向：

- HTTP multipart 上传路径
- 本地运行目录与静态资源路径组合
- 真正跨进程的 CLI Provider 到 Bridge 再到浏览器/`curl` 的全链路人工测试

## 5. 测试前准备

### 5.1 推荐测试环境

推荐准备以下两种环境中的一种：

### 方案 A：Docker 环境

适合验证浏览器页面、下载页、容器化部署和公网近似行为。

```bash
docker-compose up -d
```

默认端口：

- HTTP：`8000`
- TCP：`8888`

### 方案 B：本地源码运行

适合快速调试，但要注意静态目录依赖。

重要说明：

- 浏览器首页和下载页依赖当前工作目录下存在 `./static`
- 如果从仓库根目录直接运行 `go run bridge/main.go`，静态页面可能不可用
- 本地做浏览器页面测试时，优先在 `bridge/` 目录内运行服务端

示例：

终端 1：

```bash
cd bridge
go run main.go --http-port=8000 --tcp-port=8888 --max-file-size=1 --token-len=8
```

终端 2：

```bash
go build -o fileflowprovider provider/main.go
```

### 5.2 测试文件准备

建议至少准备以下文件：

1. `small.txt`
   内容：几十到几百字节，便于快速验证
2. `medium.bin`
   内容：1MB 到 10MB，便于观察进度和统计
3. `name with space.txt`
   作用：验证 URL 编码和带文件名下载

### 5.3 测试工具准备

建议本机具备以下工具：

- 浏览器
- `curl`
- `jq`（可选，用于查看 JSON）
- `sha256sum` 或 `shasum -a 256`（用于校验下载结果）

## 6. 标准测试流程

以下流程按“先低风险、后全链路”的顺序排列。

### 6.1 TC-01 健康检查

目的：确认桥接服务端已启动。

命令：

```bash
curl http://127.0.0.1:8000/health
```

期望结果：

- HTTP `200`
- 返回 JSON
- `status` 为 `healthy`

### 6.2 TC-02 初始统计接口

目的：确认统计接口可用。

命令：

```bash
curl http://127.0.0.1:8000/stats
```

期望结果：

- HTTP `200`
- JSON 中至少包含以下字段：
  - `status`
  - `uptime`
  - `files_registered`
  - `files_transferred`
  - `bytes_transferred`
  - `active_connections`

### 6.3 TC-03 CLI Provider 主链路

目的：验证最核心的“注册 -> TCP 流 -> 下载”流程。

前置条件：

- Bridge 已启动
- 测试文件存在，例如 `small.txt`

步骤：

1. 构建 Provider

```bash
go build -o fileflowprovider provider/main.go
```

2. 启动 Provider

```bash
./fileflowprovider http://127.0.0.1:8000 ./small.txt
```

3. 记录终端输出中的下载链接，例如：

```text
http://127.0.0.1:8000/download/<token>/small.txt
```

4. 用另一终端下载

```bash
curl -L -o downloaded-small.txt "http://127.0.0.1:8000/download/<token>/small.txt"
```

5. 比较源文件和下载文件

```bash
sha256sum small.txt downloaded-small.txt
```

期望结果：

- Provider 能注册成功
- Provider 输出下载链接
- 下载命令返回成功
- 两个文件哈希一致
- Provider 终端输出传输完成和统计信息

### 6.4 TC-04 状态查询生命周期

目的：观察文件状态从注册到传输的变化。

步骤：

1. 在 Provider 注册成功后、下载前，查询：

```bash
curl http://127.0.0.1:8000/status/<token>
```

2. 下载进行中再次查询：

```bash
curl http://127.0.0.1:8000/status/<token>
```

3. 下载结束后再次查询：

```bash
curl http://127.0.0.1:8000/status/<token>
```

期望结果：

- 注册后通常为 `registered`
- 流连接建立后通常为 `streaming`
- 下载完成后，资源会被清理，因此最终更可能返回 `404`

说明：

- 虽然代码中有 `download_completed` 标记，但下载结束后资源紧接着会被清理，不应把“完成后还能长期查状态”作为预期

### 6.5 TC-05 单次下载语义

目的：验证资源下载完成后即失效。

步骤：

1. 完成一次成功下载
2. 再次访问相同链接

```bash
curl -i "http://127.0.0.1:8000/download/<token>/small.txt"
```

期望结果：

- 第二次访问返回 `404` 或 `410`
- 不应再次成功下载完整文件

### 6.6 TC-06 浏览器上传主链路

目的：验证首页上传页、WebSocket 上传和浏览器下载页。

前置条件：

- 服务端必须能提供 `./static`
- 推荐使用 Docker，或在 `bridge/` 目录下本地运行服务端

步骤：

1. 浏览器打开：

```text
http://127.0.0.1:8000/
```

2. 选择文件并点击上传
3. 页面应很快展示一个下载链接
4. 用另一个浏览器标签页打开该下载链接
5. 浏览器应先显示下载页
6. 点击“点击下载文件”
7. 观察上传页状态变化和下载结果

期望结果：

- 首页可正常打开
- 可选择或拖拽文件
- 上传页出现下载链接
- 下载页显示文件名、大小、token
- 点击下载后文件成功落地
- 上传页在传输完成后显示完成状态

### 6.7 TC-07 带文件名下载地址

目的：验证 `/download/{auth_token}/{filename}` 形式。

步骤：

```bash
curl -L -o downloaded-named.txt "http://127.0.0.1:8000/download/<token>/name%20with%20space.txt"
```

期望结果：

- 请求成功
- 文件名路径中的空格需要 URL 编码
- 下载内容与原文件一致

### 6.8 TC-08 错误输入测试

目的：验证基础错误处理。

### 空文件名注册

```bash
curl -i \
  -H 'Content-Type: application/json' \
  -d '{"filename":"","size":123}' \
  http://127.0.0.1:8000/register
```

期望结果：

- HTTP `400`

### 无效 token 状态查询

```bash
curl -i http://127.0.0.1:8000/status/invalid_token
```

期望结果：

- HTTP `404`

### 无效 token 下载

```bash
curl -i http://127.0.0.1:8000/download/invalid_token
```

期望结果：

- HTTP `404`

### 6.9 TC-09 文件大小限制

目的：验证服务端大小限制。

说明：

- 启动参数 `--max-file-size` 单位是 GiB
- `/register` 中比较的是字节数
- 因此测试时要让客户端上报的 `size` 明确超过限制

示例：

如果服务端以 `--max-file-size=1` 启动，则下面请求应失败：

```bash
curl -i \
  -H 'Content-Type: application/json' \
  -d '{"filename":"too-large.bin","size":2147483648}' \
  http://127.0.0.1:8000/register
```

期望结果：

- HTTP `413`

### 6.10 TC-10 浏览器与命令行下载差异

目的：确认下载入口的双重行为。

步骤：

1. 用浏览器访问下载链接
2. 用 `curl` 访问同一类下载链接

期望结果：

- 浏览器通常先看到下载页
- `curl` 通常直接得到文件流

## 7. 补充测试流程

### 7.1 HTTP Multipart 上传接口

这条路径适合作为补充项。

步骤：

1. 先注册文件
2. 用返回的 token 调用 multipart 上传
3. 另一端发起下载

示例：

```bash
curl -H 'Content-Type: application/json' \
  -d '{"filename":"small.txt","size":123}' \
  http://127.0.0.1:8000/register
```

拿到 token 后：

```bash
curl -i -F "file=@small.txt" http://127.0.0.1:8000/upload/<token>
```

期望结果：

- 接口接受请求
- 只有当下载端真正开始消费数据后，这次上传才算完整走通

注意：

- 当前项目对这条路径的自动化覆盖明显弱于 CLI Provider 和 WebSocket 上传
- 若首次测试就要验收项目，不建议先走这条路径

## 8. 人工测试记录建议

每条测试至少记录以下内容：

1. 测试时间
2. Bridge 启动方式
3. Provider 启动方式
4. 测试文件名与大小
5. 下载方式
6. 实际 HTTP 状态码
7. 实际终端输出或页面行为
8. 文件哈希是否一致
9. 是否符合预期

## 9. 当前已知实现注意事项

以下内容来自代码分析，执行测试时要特别留意：

1. 浏览器页面依赖 `./static`
   如果运行目录下没有 `static`，接口仍可能可用，但首页和下载页会不可用或退回简化页面。
2. Provider usage 文案落后于实际文件名
   代码里的 usage 还写着 `flow_provider`，但仓库实际构建产物是 `fileflowprovider`。
3. 首页提示“无大小限制”并不准确
   前端文案如此显示，但服务端实际有大小限制。
4. 下载完成后状态不可长期查询
   服务端完成传输后会清理注册信息，因此完成态更像瞬时状态。
5. HTTP Multipart 上传不是首页主路径
   首页默认走的是 WebSocket 上传。

## 10. 推荐验收顺序

建议按下面顺序执行：

1. `TC-01` 健康检查
2. `TC-02` 统计接口
3. `TC-03` CLI Provider 主链路
4. `TC-04` 状态生命周期
5. `TC-05` 单次下载语义
6. `TC-06` 浏览器上传主链路
7. `TC-07` 带文件名下载地址
8. `TC-08` 错误输入测试
9. `TC-09` 文件大小限制
10. `TC-10` 浏览器与命令行差异
11. `7.1` HTTP Multipart 上传补充测试

## 11. 结论

从当前代码和测试覆盖看，项目最值得优先验证的能力是：

- CLI Provider + TCP 流下载
- 浏览器 WebSocket 上传
- 浏览器/命令行双下载入口
- 状态、健康检查、统计接口
- 一次性下载和资源清理机制

如果要把这份手册作为后续测试依据，建议优先按第 10 节的顺序执行，不要先用 HTTP multipart 上传路径作为项目主验收口径。

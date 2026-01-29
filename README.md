# 🌊 FileFlow Bridge 使用指南

`FileFlow Bridge` 是一款高性能的文件流转桥接工具。它采用 **“边传边下” (Streaming Bridge)** 技术，打破了传统中转服务器“先完整上传、再分发下载”的模式，实现零等待的即时文件分发。

---

## 🚀 核心特性

* **零时延分发**：文件注册后立即获取下载链接，无需等待上传完成。
* **内存友好**：数据在 TCP 隧道与 HTTP 响应间实时透传，不占用服务器磁盘。
* **极简配置**：支持命令行参数与环境变量，配置灵活。

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

### 3. 配置参数

程序按以下优先级读取配置：**命令行参数 > 环境变量 > 默认值**。

#### 3.1 所有可用配置选项

| 配置项 | 命令行参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- | --- |
| **HTTP 端口** | `--http-port` | `FFB_HTTP_PORT` | `8000` | 对外提供访问与下载的 **HTTP** 端口 |
| **TCP 端口** | `--tcp-port` | `FFB_TCP_PORT` | `8888` | 接收文件流推送的内网/外网 TCP 端口 |
| **最大文件限制** | `--max-file-size` | `FFB_MAX_FILE_SIZE` | `100` | 允许注册的最大文件大小 (**单位: GiB**) |
| **AuthToken 长度** | `--token-len` | `FFB_TOKEN_LEN` | `8` | 注册时生成的 **AuthToken** 长度，长度越长安全性越高，长度范围6-32位，超出限制将改成默认8位 |
| **日志级别** | 无 | `FFB_LOG_LEVEL` | `INFO` | 控制日志输出级别 |
| **日志路径** | 无 | `FFB_LOG_PATH` | `fileflow_bridge.log` | 日志文件保存路径 |

#### 3.2 配置说明

- **FFB_HTTP_PORT**: HTTP服务器监听端口，用于提供API接口和文件下载服务
- **FFB_TCP_PORT**: TCP流服务器监听端口，用于接收文件流数据
- **FFB_MAX_FILE_SIZE**: 限制单个文件的最大大小（单位：GiB），例如设置为100表示最大支持100GiB文件
- **FFB_TOKEN_LEN**: 认证令牌长度（6-32字符），更长的令牌更安全但会增加URL长度
- **FFB_LOG_LEVEL**: 日志级别（INFO、DEBUG等），控制控制台输出的详细程度
- **FFB_LOG_PATH**: 日志文件存储路径（在容器中运行时此设置会被忽略，只输出到控制台）

---

## 📤 提供端使用 (File Provider)

提供端用于将本地文件“映射”到云端，并通过流方式推送数据。

### 使用方法

在命令行中依次指定 **服务端完整 HTTP 地址** 和 **本地文件路径**：

```bash
./fileflowprovider http://<IP或域名>:<端口> <文件路径>
```

> **注意**：服务端地址必须包含 `http://` 前缀及明确的端口号。

**示例：**

```bash
# 假设服务端运行在 1.2.3.4 的 8000 端口
./fileflowprovide http://1.2.3.4:8000 /home/data/large_video.mp4
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

## 📖 运行示例 (Demo)

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

* **单次有效**：为保证传输性能与安全，下载地址在完成后立即失效，资源自动释放。
* **不支持断点续传**：由于采用实时流物理透传，下载过程中断需重新发起注册。
* **防火墙策略**：请确保服务端定义的 `HTTP 端口` 和 `TCP 端口` 在防火墙或安全组中已开放。
* **安全性**：`AuthToken` 是 File Provider 连接 Bridge Server 进行流传输的唯一凭证。增加 `--token-len` 可以有效防止暴力破解
* **服务端资源**：请确保服务端有足够的网络带宽和内存资源以支持高并发传输
* **日志管理**：在生产环境中，建议配置日志轮转以避免占用过多磁盘空间。Docker部署方案已内置日志大小限制。
* **静态文件**：服务器支持静态文件服务，会自动提供 `bridge/static` 目录下的文件。

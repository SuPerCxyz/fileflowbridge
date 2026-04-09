# Runtime And Commands

本文件集中说明代理最常用的构建、运行和环境变量信息。

## 构建

```bash
go build -o fileflowbridge bridge/main.go
go build -o fileflowprovider provider/main.go
```

多平台构建：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/fileflowbridge-linux-amd64 bridge/main.go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/fileflowbridge-linux-arm64 bridge/main.go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/fileflowprovider-linux-amd64 provider/main.go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/fileflowprovider-linux-arm64 provider/main.go
```

Docker：

```bash
docker build -t fileflowbridge .
docker-compose up -d
```

## 运行

桥接服务器：

```bash
./fileflowbridge --http-port=8000 --tcp-port=8888 --max-file-size=100 --token-len=8
```

使用环境变量：

```bash
FFB_HTTP_PORT=8000 FFB_TCP_PORT=8888 FFB_MAX_FILE_SIZE=100 FFB_TOKEN_LEN=8 ./fileflowbridge
```

文件提供者：

```bash
./fileflowprovider http://localhost:8000 ./your_file.txt
```

## 开发命令

```bash
go fmt ./...
go vet ./...
go mod tidy
go mod verify
go run -race bridge/main.go --http-port=8000 --tcp-port=8888
```

开发容器和调试流程见 [DEVELOPMENT.md](../../DEVELOPMENT.md)。

## 环境变量

- `FFB_HTTP_PORT`：HTTP 服务器端口，默认 `8000`
- `FFB_TCP_PORT`：TCP 流端口，默认 `8888`
- `FFB_MAX_FILE_SIZE`：最大文件大小，单位 GiB，默认 `100`
- `FFB_TOKEN_LEN`：认证令牌长度，默认 `8`，允许范围 `6-32`
- `FFB_LOG_LEVEL`：日志级别，默认 `INFO`
- `FFB_LOG_PATH`：日志文件路径，默认 `fileflow_bridge.log`

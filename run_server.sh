#!/bin/bash
# 启动FileFlow Bridge服务器的脚本

echo "🌊 Starting FileFlow Bridge Server..."
echo "=================================================="

# 编译服务器
go build -o fileflowbridge ./bridge/main.go

if [ $? -ne 0 ]; then
    echo "❌ 编译失败"
    exit 1
fi

echo "✅ 编译成功"

# 检查端口是否已被占用
if lsof -Pi :8000 -sTCP:LISTEN -t >/dev/null ; then
    echo "⚠️  端口8000已被占用，请停止占用此端口的进程"
else
    echo "✅ 端口8000空闲"
fi

if lsof -Pi :8888 -sTCP:LISTEN -t >/dev/null ; then
    echo "⚠️  端口8888已被占用，请停止占用此端口的进程"
else
    echo "✅ 端口8888空闲"
fi

# 启动服务器，使用全部支持的参数
./fileflowbridge -http-port 8000 -tcp-port 8888 -max-file-size 100 -token-len 8

echo "👋 服务器已停止"
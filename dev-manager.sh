#!/bin/bash

# 开发环境管理脚本
set -e

echo "FileFlow Bridge 开发环境管理脚本"
echo "=================================="

start_dev() {
    echo "启动 FileFlow Bridge 开发环境..."
    docker-compose -f docker-compose.dev.yaml up -d
    echo "开发环境已启动!"
    echo "HTTP 端口: ${FFB_HTTP_PORT:-8000}"
    echo "TCP 端口: ${FFB_TCP_PORT:-8888}"
    echo "请访问 http://localhost:${FFB_HTTP_PORT:-8000} 查看应用"
}

stop_dev() {
    echo "停止 FileFlow Bridge 开发环境..."
    docker-compose -f docker-compose.dev.yaml down
    echo "开发环境已停止!"
}

restart_dev() {
    echo "重启 FileFlow Bridge 开发环境..."
    docker-compose -f docker-compose.dev.yaml restart
    echo "开发环境已重启!"
}

view_logs() {
    echo "查看 FileFlow Bridge 开发环境日志..."
    docker-compose -f docker-compose.dev.yaml logs -f
}

enter_container() {
    echo "进入 FileFlow Bridge 开发容器..."
    docker-compose -f docker-compose.dev.yaml exec fileflow-bridge-dev sh
}

build_image() {
    echo "构建开发镜像..."
    docker-compose -f docker-compose.dev.yaml build --no-cache
    echo "开发镜像构建完成!"
}

case "${1:-menu}" in
    menu)
        echo ""
        echo "请选择操作:"
        echo "1) 启动开发环境"
        echo "2) 停止开发环境"
        echo "3) 重启开发环境"
        echo "4) 查看日志"
        echo "5) 进入容器调试"
        echo "6) 构建开发镜像"
        echo "q) 退出"
        echo ""

        read -p "请输入选项 (1-6 或 q): " choice
        case $choice in
            1) start_dev ;;
            2) stop_dev ;;
            3) restart_dev ;;
            4) view_logs ;;
            5) enter_container ;;
            6) build_image ;;
            q|Q) exit 0 ;;
            *) echo "无效选项" ;;
        esac
        ;;
    start)
        start_dev
        ;;
    stop)
        stop_dev
        ;;
    restart)
        restart_dev
        ;;
    logs)
        view_logs
        ;;
    shell|exec)
        enter_container
        ;;
    build)
        build_image
        ;;
    *)
        echo "用法: $0 [start|stop|restart|logs|shell|build|menu]"
        exit 1
        ;;
esac
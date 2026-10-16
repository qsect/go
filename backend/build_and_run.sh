#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "============================================================"
echo " 装置车间综合协作平台 - Go 后端一键编译与启动脚本"
echo "============================================================"

# 1. 自动检测并复用现有 PHP 系统的 .env 配置
if [ ! -f ".env" ]; then
    if [ -f "../backend/.env" ]; then
        echo "[INFO] 检测到 ../backend/.env，正在复制并追加 Go 端口配置..."
        cp ../backend/.env .env
        echo "" >> .env
        echo "# Go 服务监听端口" >> .env
        echo "GO_LISTEN_ADDR=127.0.0.1:8080" >> .env
        echo "PUBLIC_ROOT=../backend/public" >> .env
        echo "STORAGE_ROOT=./storage" >> .env
    elif [ -f "../新系统 - 1/backend/.env" ]; then
        echo "[INFO] 检测到 ../新系统 - 1/backend/.env，正在复制并追加 Go 端口配置..."
        cp "../新系统 - 1/backend/.env" .env
        echo "" >> .env
        echo "# Go 服务监听端口" >> .env
        echo "GO_LISTEN_ADDR=127.0.0.1:8080" >> .env
        echo "PUBLIC_ROOT=../新系统 - 1/backend/public" >> .env
        echo "STORAGE_ROOT=./storage" >> .env
    else
        echo "[WARN] 未找到现成 .env，已从 .env.example 生成默认 .env，请先填写真实 DB_PASS！"
        cp .env.example .env
    fi
fi

mkdir -p bin storage/sessions storage/schema

# 2. 拉取 Go 依赖并编译二进制
echo "[INFO] 正在设置 GOPROXY 并拉取依赖..."
export GOPROXY=${GOPROXY:-https://goproxy.cn,direct}
go mod tidy

echo "[INFO] 正在编译生产环境二进制 -> ./bin/vite-server ..."
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ./bin/vite-server ./cmd/server

echo "[SUCCESS] 编译完成：$(pwd)/bin/vite-server"

if [ "${1:-}" = "--build-only" ]; then
    exit 0
fi

echo "[INFO] 正在启动 Go 后端服务..."
exec ./bin/vite-server

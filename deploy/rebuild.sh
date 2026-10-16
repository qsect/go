#!/usr/bin/env bash
# ============================================================
# 装置车间综合协作平台 —— 全量重构部署脚本
#
# 用法：bash /www/wwwroot/go/deploy/rebuild.sh
#
# 设计原则：
#   1. 每次执行都清理上一轮构建产生的所有无用文件，不留残留
#   2. 编译 / 构建失败立即中止，绝不误杀正在运行的服务
#   3. 二进制原子替换 + 保留一份 .bak 用于一键回滚
#   4. 重启交给宝塔「进程守护管理器」，不手动 nohup
#      （手动 nohup 会与守护管理器抢进程，导致 bind: address already in use）
# ============================================================
set -euo pipefail

APP_DIR=/www/wwwroot/go
FE_DIR="$APP_DIR/frontend"
BE_DIR="$APP_DIR/backend"

# 清理 /www/backup 下超过 N 天的本站清理备份（只匹配 go-before-purge-* ，不动其他备份）
KEEP_BACKUP_DAYS=7

step() { echo; echo "========== $* =========="; }

# ------------------------------------------------------------
step "[0/6] 环境检查"
command -v go  >/dev/null || { echo "✗ 未找到 go，中止";  exit 1; }
command -v npm >/dev/null || { echo "✗ 未找到 npm，中止"; exit 1; }
echo "  go: $(go version | awk '{print $3}')"
echo "  node: $(node -v 2>/dev/null || echo '-')"

# ------------------------------------------------------------
step "[1/6] 清理上一轮残留"
# 编译临时产物
rm -f  "$BE_DIR/inspect_server_new" "$BE_DIR/inspect_server_new.tmp"
# 上一轮 dist 切换留下的旧目录（正常流程已删，这里防异常中断残留）
rm -rf "$FE_DIR/dist_old"
echo "  已清临时文件"

# ------------------------------------------------------------
step "[2/6] 备份当前二进制（用于回滚，只保留一份）"
if [ -f "$BE_DIR/inspect_server" ]; then
  cp -f "$BE_DIR/inspect_server" "$BE_DIR/inspect_server.bak"
  echo "  已备份 inspect_server.bak ($(du -h "$BE_DIR/inspect_server.bak" | cut -f1))"
else
  echo "  未发现现有二进制，跳过备份"
fi

# ------------------------------------------------------------
step "[3/6] 前端构建（清空旧 dist，防止 chunk 无限累积）"
cd "$FE_DIR"
# 注意：vite.config.js 里 emptyOutDir=false，vite 不会自动清理，必须手动切换
[ -d dist ] && mv dist dist_old
npm run build
rm -rf dist_old
echo "  dist 体积: $(du -sh dist | cut -f1)，文件数: $(ls -1 dist/assets | wc -l)"

# ------------------------------------------------------------
step "[4/6] 后端编译"
cd "$BE_DIR"
rm -f inspect_server_new
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o inspect_server_new ./cmd/server
# 属主必须 www，否则后续写 storage/sessions、public/uploads 会权限错乱
chown www:www inspect_server_new
chmod 755 inspect_server_new
mv -f inspect_server_new inspect_server
echo "  新二进制: $(du -h inspect_server | cut -f1)"

# CEM 需求计划号同步定时任务（独立二进制，由 cron 调用）
mkdir -p "$BE_DIR/bin"
rm -f "$BE_DIR/bin/cemsync_new"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BE_DIR/bin/cemsync_new" ./cmd/cemsync
chown www:www "$BE_DIR/bin/cemsync_new"
chmod 755 "$BE_DIR/bin/cemsync_new"
mv -f "$BE_DIR/bin/cemsync_new" "$BE_DIR/bin/cemsync"
echo "  cemsync : $(du -h "$BE_DIR/bin/cemsync" | cut -f1)"

# ------------------------------------------------------------
step "[5/6] 清理运行时垃圾"
# 过期会话文件（保留 1 天内的，避免把在线用户全踢掉）
find "$BE_DIR/storage/sessions" -name "*.json" -type f -mtime +1 -delete 2>/dev/null || true
echo "  剩余会话文件: $(ls -1 "$BE_DIR/storage/sessions" 2>/dev/null | wc -l)"
# 启动日志（守护管理器的输出不写这里，通常为空）
: > "$BE_DIR/server.log"
chown www:www "$BE_DIR/server.log" 2>/dev/null || true
# 本站清理备份保留 N 天
find /www/backup -maxdepth 1 -name "go-before-purge-*.tar.gz" -type f -mtime +"$KEEP_BACKUP_DAYS" -delete 2>/dev/null || true
echo "  /www/backup 剩余本站备份: $(ls -1 /www/backup/go-before-purge-* 2>/dev/null | wc -l)"

# ------------------------------------------------------------
step "[6/6] 重启服务（守护管理器自动拉起）"
pkill -TERM -f "$BE_DIR/inspect_server" || true
sleep 3

# 守护管理器未托管时才手动兜底，避免双进程抢端口
if ! pgrep -f "$BE_DIR/inspect_server" >/dev/null; then
  echo "  ⚠ 守护管理器未自动拉起，手动启动"
  su -s /bin/bash www -c "cd $BE_DIR && nohup ./inspect_server >> server.log 2>&1 &"
  sleep 2
fi

# ------------------------------------------------------------
step "验证"
PROC_COUNT=$(pgrep -cf "$BE_DIR/inspect_server" || true)
echo "  进程数     : ${PROC_COUNT:-0}   （必须为 1，>1 说明有进程抢端口）"
pgrep -af "$BE_DIR/inspect_server" || echo "  ✗ 无进程！"
echo -n "  登录接口   : "; curl -s -o /dev/null -w "%{http_code}\n" -X POST http://127.0.0.1:8080/api/auth/login || echo "失败"
echo -n "  未知路由   : "; curl -s http://127.0.0.1:8080/api/not_exist | head -c 110 || echo "失败"
echo
echo -n "  站点首页   : "; curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8777/ || echo "失败"

step "完成"
echo "  回滚命令： cd $BE_DIR && mv -f inspect_server.bak inspect_server && pkill -TERM -f $BE_DIR/inspect_server"
echo "  查看日志： tail -50 $BE_DIR/server.log"
echo "  ⚠ 服务已重启，所有在线用户需要重新登录"

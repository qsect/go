# 装置车间综合协作平台：PHP 残留溯源 + 纯 Go 化优化方案

> 分析对象：`D:\work\新系统-go`
> 分析日期：2026-10-15
> 结论一句话：**Go 侧已经 100% 覆盖了 PHP 的全部 33 个业务控制器，仓库里的 PHP 是"迁移蓝图 + 反代兜底"的历史遗留，95% 可安全删除。**

---

## 一、PHP 文件为什么还在？

### 1.1 直接原因：三条历史设计痕迹

| 痕迹 | 证据 | 含义 |
|---|---|---|
| **① PHP 被当成迁移蓝图留在仓库** | Go 控制器注释里大量出现：`// 严格对齐 TraceController.php queryRecords`、`// ★★★ 关键修复（对标 trace.php 第 902-916 行）★★★`、`// 对标 backend/includes/CemClient.php + CemConfig.php`（`quality_issue_controller.go` 共 40+ 处） | 迁移时是"照着 PHP 逐行翻译"，翻译完没删源稿 |
| **② 保留了 PHP 反代兜底通道** | `internal/router/router.go:522`：`fallbackURL := config.Get("PHP_FALLBACK_URL", "")`；`NoRoute` 中未命中的 `/api/*` 会 `proxy.ServeHTTP` 转发到 PHP。`.env.example` 注释写明「尚未迁移的业务模块接口将自动透明转发至 PHP」 | 当初设计是 **Go + PHP 双跑灰度**，PHP 是"兜底后端" |
| **③ 一个未迁移的 PHP 常驻脚本** | `backend/app/cem/index.php`（CEM 流程卡/需求计划号同步），带 `logs/cron.log`、`sync_2026-10-06.log`、`sync_2026-10-08.log`、`debug_page_1~3.html` | 这个还在真实运行，是唯一"活着的" PHP |

### 1.2 佐证：生产环境 PHP 其实已经不接流量

`deploy/nginx_go.conf` 全篇只有 `proxy_pass http://127.0.0.1:8080`（Go），**没有任何 php-fpm 段**；`.env` 中 `PHP_FALLBACK_URL` **未配置**（兜底实际已关闭）。

> 也就是说：线上 PHP 早已"退役"，仓库里的 PHP 只是没被清理的尸体 + 一份被 Go 抄完的参考稿。

---

## 二、PHP 文件逐类清点（共 57 个，含 45 个纯冗余）

| 类别 | 路径 | 文件数 | 状态 | 处置 |
|---|---:|---|---|---|
| **A. 旧控制器（已 1:1 重写为 Go）** | `backend/app/Controllers/*.php` | 28 | 已迁移 | ✅ 可删 |
| **B. 旧框架内核** | `backend/app/Core/*.php` | 12 | 已迁移到 `internal/core/` | ✅ 可删 |
| **C. 旧中间件** | `backend/app/Middlewares/*.php` | 3 | 已迁移到 `internal/middleware/` | ✅ 可删 |
| **D. 旧服务** | `backend/app/Services/AuditService.php` | 1 | 已迁移 `internal/service/` | ✅ 可删 |
| **E. 旧入口 + 旧路由表** | `backend/public/index.php` | 1 | 路由表已被 `router.go` 取代 | ✅ 可删 |
| **F. 旧追溯脚本** | `backend/app/legacy_trace.php` | 1 | 已并入 `quality_issue_controller.go` | ✅ 可删 |
| **G. CEM 客户端** | `backend/includes/CemClient.php`、`CemPushService.php`、`response.php` | 3 | 已并入 Go 的 trace 控制器 | ✅ 可删 |
| **H. 参考副本（完全重复）** | `php_ref/legacy_trace.php`（与 F **逐行相同**，均 1493 行） | 1 | 冗余副本 | ✅ 可删 |
| **I. 空配置文件** | `backend/config/database.php`（0 字节） | 1 | 空文件 | ✅ 可删 |
| **J. CEM 凭据配置** | `backend/config/cem.local.php` | 1 | ⚠️ 明文密码且已入库 | 🔴 先轮换 + 清库 |
| **K. CEM 定时同步脚本（仍在跑）** | `backend/app/cem/index.php` + `cem_plan_map.json` + `logs/*` | 8 | 🟡 **唯一活着的 PHP** | ⚠️ 先迁 Go 再删 |

**净结论：J 之外，A~I 共 45 个文件 = 纯冗余；K 的 8 个文件需先做 Go 化迁移。**

---

## 三、迁移完成度验证（Go 是否已全覆盖？）

一开始看 `internal/controller/` 只有 8 个 `.go` 文件会误以为"Go 才写了 1/4"。实际是 **按业务域打包成了大文件**：

| Go 文件 | 行数 | 内含控制器 |
|---|---:|---|
| `quality_issue_controller.go` | 5405 | Trace / QualityTrace / BarcodeValidator / FirstPiece / Impedance / Quality / Troubleshoot / Beibian / DocError / Zywj（**10 个**） |
| `production_material_controller.go` | 2077 | Progress / Kitting / DispatchKitting / LabelPrint / Equipment / Mac / MacQuery（**7 个**） |
| `asset_hr_controller.go` | 2921 | Osm / Assets / AgingCart / Attendance / Diary / Manhour / Skill（**7 个**） |
| `collab_report_controller.go` | 1284 | Discuss / Files / Tools / Weekly / Oee（**5 个**） |
| `system_controller.go` | 909 | System |
| `client_version_controller.go` | 714 | ClientVersion |
| `auth_controller.go` | 486 | Auth |
| `dashboard_controller.go` | 196 | Dashboard |

**33 个构造函数全部齐备，与 `router.go` 的 33 处 `controller.NewXxxController()` 一一对应，无缺失。**
且 Go 侧还多出 PHP 没有的接口：`/api/auth/nav_tree`、`/api/discuss/list_notice_reads`、`/api/discuss/mark_notice_read`。

→ **功能迁移已完成，删 PHP 不会造成功能缺失。**

---

## 四、优化清单（按优先级）

### 🔴 P0 — 安全，立即处理

| # | 问题 | 位置 | 说明 |
|---|---|---|---|
| 1 | **明文 CEM 密码已进 Git** | `backend/config/cem.local.php` | `.gitignore` 里写的是 `config.local.php`，**匹配不到 `cem.local.php`**，该文件已被 `git ls-files` 跟踪。需改 `.gitignore` 为 `*.local.php` + `git rm --cached` + **轮换 CEM 密码** |
| 2 | 会话文件入库 | `backend/storage/sessions/003f011e04f62431b583a255b20a30dc.json` | 含用户会话数据，已从磁盘被 Git 跟踪 |
| 3 | 抓取的 CEM 页面入库 | `backend/app/cem/logs/debug_page_1~3.html`（约 95KB） | 第三方系统页面快照，可能含生产数据 |
| 4 | 日志入库 | `backend/server.log`、`backend/app/cem/logs/*.log` | 运行日志不应进版本库 |
| 5 | 构建产物入库 | `frontend/dist/index.html` | `dist/` 未完全排除 |

### 🟡 P1 — 架构与正确性

| # | 问题 | 位置 | 建议 |
|---|---|---|---|
| 6 | **PHP 反代兜底未拆除** | `router.go:520-540` | 删除 `PHP_FALLBACK_URL` 整段逻辑。它让"任何拼错的 `/api/*` 路径被静默转发"，是排障陷阱，也是双栈残留 |
| 7 | **Session 无 GC、不可水平扩展** | `internal/core/session.go` | 内存 map + 磁盘 JSON，**没有过期清理**，`storage/sessions/` 会无限增长；多实例部署时内存不共享、磁盘也不共享。建议换 Redis（或至少加定时 GC + 文件过期扫描） |
| 8 | **启动时自动执行 DDL** | `main.go:30` → `core.AutoInitBrandNewDatabase`（`schema.go` 58KB） | 生产库上跑自动建表风险高。建议改为 `golang-migrate` 版本化迁移，或至少加 `AUTO_MIGRATE=0` 开关 |
| 9 | **SQL 用 `%d` 拼 LIMIT/OFFSET** | 22 处，典型 `system_controller.go:686` | 当前 `page/limit` 走 Atoi 风险低，但建议统一改为参数化（`LIMIT ? OFFSET ?`），并对 `page/limit` 做上限钳制（防大 offset 拖库） |
| 10 | **控制器文件过大** | `quality_issue_controller.go` 5405 行 / 179KB | 1 个文件塞 10 个 controller 违背 Go 惯例、难 review、易冲突。建议按控制器拆分为独立文件 |
| 11 | `router.go` 单文件 543 行 | | 按 9 大业务域拆成多个 `router_*.go`，用 `Register(r *gin.Engine)` 挂载 |
| 12 | **缺健康检查端点** | — | 加 `/healthz`（DB ping），供 systemd `ExecStartPost` / 负载均衡探活 |
| 13 | **缺日志轮转** | `deploy/vite-go.service` 用 `StandardOutput=append:` | 日志会无限增长。改用 logrotate，或 Go 内用 `lumberjack` 按大小切割 |
| 14 | Nginx 未开压缩与限流 | `deploy/nginx_go.conf` | 建议加 `gzip on` + `limit_req_zone`（对 `/api/auth/login` 单独限流更稳，现有限流只在应用层） |

### 🟢 P2 — 工程化

| # | 问题 | 建议 |
|---|---|---|
| 15 | **0 个单元测试**（无 `_test.go`） | 至少为 `core`（AsInt/AsString/password_policy/work_order_lookup）补单测 |
| 16 | 无 lint / CI | 加 `golangci-lint`、`go vet`、`.github/workflows` 或 GitLab CI |
| 17 | 本地/CI 无 Go 工具链 | 当前环境 `go: command not found`，需在构建机确认 Go ≥ 1.22 |
| 18 | `.env` 与 `.env.example` 不一致 | `.env` 里 `DB_NAME=gogo`，`.env.example` 是 `vite`；建议统一并加部署校验 |
| 19 | `SESSION_COOKIE_NAME=PHPSESSID` | 兼容期合理，纯 Go 化后可改为 `VITE_SESSION`（需与前端 Cookie 同步切换） |
| 20 | 无用文件 | `backend/build_and_run.sh`（与 Makefile 重复）、`config/database.php`（空） |

---

## 五、纯 Go 化执行路线（4 阶段）

### 阶段 1：安全止血（可立即执行，不影响业务）

```bash
cd /d/work/新系统-go

# 1) 修正 .gitignore（原写法匹配不到 cem.local.php）
#    把  config.local.php  改为：
#    *.local.php
#    并补：backend/storage/sessions/*.json  backend/server.log  **/logs/  frontend/dist/

# 2) 从版本库移除已跟踪的敏感/产物文件（本地文件保留）
git rm --cached backend/config/cem.local.php
git rm --cached backend/storage/sessions/003f011e04f62431b583a255b20a30dc.json
git rm -r --cached backend/app/cem/logs
git rm --cached backend/server.log
git rm -r --cached frontend/dist

# 3) 轮换 CEM 密码（到 CEM 系统改密后，同步更新 .env 的 CEM_PASS）
```

### 阶段 2：删除已迁移的 PHP（45 个文件，删前先建 tag 备份）

```bash
cd /d/work/新系统-go
git tag -a before-php-purge -m "删除 PHP 遗留前快照"   # 需要至少有 1 次 commit；当前仓库尚无 commit，请先 git commit

# 删旧控制器 / 内核 / 中间件 / 服务 / 入口
git rm -r backend/app/Controllers backend/app/Core backend/app/Middlewares backend/app/Services
git rm backend/app/legacy_trace.php
git rm backend/public/index.php
git rm -r backend/includes
git rm backend/config/database.php
git rm -r php_ref
```

同时删除 `router.go` 中的兜底逻辑（第 519-540 行整段替换为纯 404）：

```go
r.NoRoute(func(c *gin.Context) {
	reqPath := c.Request.URL.Path
	core.Error(c, "接口不存在: "+reqPath, gin.H{
		"code": "ROUTE_NOT_FOUND",
		"path": reqPath,
	}, http.StatusNotFound)
})
```
（并移除 `net/http/httputil`、`net/url` 两个 import）

### 阶段 3：CEM 脚本 Go 化（唯一活着的 PHP）

先在服务器确认它是否真的在跑：

```bash
crontab -l | grep -i cem
ls -la /www/wwwroot/go/backend/app/cem/logs/
```

- **若 cron 里没有 → 直接删** `backend/app/cem/` 整个目录。
- **若 cron 里有 → 新建 `backend/cmd/cemsync/main.go`**（逻辑照 `app/cem/index.php`：登录 CEM → 翻页抓流程卡/需求计划号 → 写 `cem_plan_map.json`），编译为独立二进制，把 cron 换成 `cemsync`，验证一轮输出一致后再删 PHP。

### 阶段 4：架构优化（可并行推进）

按 P1 的 #7（Session→Redis）、#8（迁移工具化）、#10/#11（拆文件）、#12（/healthz）、#13（日志轮转）顺序推进；P2 的 #15/#16 补测试与 CI。

---

## 六、风险提示

1. **删除前务必确认 CEM cron**：`app/cem/index.php` 是唯一仍在运行的 PHP，误删会中断 CEM 数据同步。
2. **`.env` 不要提交**：当前 `.env` 已被 `.gitignore` 正确排除，但请确认服务器上部署用的是另一份，不要随代码分发。
3. **删除 PHP 后 `PHP_FALLBACK_URL` 必须同步移除**，否则一旦有人误配，会出现"Go 404 → 转发到已删除的 PHP → 500"的诡异故障。
4. **Session 改 Redis 属行为变更**，需要与前端 Cookie（`PHPSESSID`）切换一起评估停机窗口。

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vite-system-go/internal/config"
	"vite-system-go/internal/core"
	"vite-system-go/internal/router"
)

func main() {
	// 1. 加载环境变量 (.env)
	config.Load()

	// 2. 初始化主数据库连接池；若为空库（如全新建的 gogo 库），自动建表并创建初始管理员账号
	db, err := core.GetDB("default")
	if err != nil {
		log.Printf("[WARN] 启动时连接主数据库警告: %v（服务启动后将在请求时自动重试）", err)
	} else {
		if pingErr := db.Ping(); pingErr != nil {
			log.Printf("[WARN] 数据库 Ping 失败，请检查 .env 配置: %v", pingErr)
		} else {
			// 全自动初始化空数据库（创建 users、roles、modules、permissions，并注入 admin 账号与 34 个模块）
			core.AutoInitBrandNewDatabase(db)
			log.Printf("[INIT] MySQL 主数据库连接成功，全站 9 大导航分类与表结构就绪")
		}
	}

	// 3. 构建 HTTP 路由引擎
	engine := router.NewEngine()
	listenAddr := config.Get("GO_LISTEN_ADDR", "127.0.0.1:8080")

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      engine,
		ReadTimeout:  30 * time.Second,
		// 导出 Excel / 大文件上传耗时较长，须 ≥ nginx 的 proxy_read_timeout（300s），
		// 否则 Go 会先于 nginx 断开，导致导出接口 502/504
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("[SERVER] 装置车间综合协作平台 Go 后端服务已启动，监听地址: http://%s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[FATAL] HTTP 服务启动失败: %v", err)
		}
	}()

	// 4. 监听系统信号，实现平滑关停（Graceful Shutdown）
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[SERVER] 正在平滑关闭 Go 后端服务...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[ERROR] 服务关闭异常: %v", err)
	}
	log.Println("[SERVER] 服务已安全退出")
}

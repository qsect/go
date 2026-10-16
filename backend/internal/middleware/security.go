package middleware

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
	"vite-system-go/internal/core"
)

var (
	reConvertPath = regexp.MustCompile(`(?i)^/api/tools/`)
	reExportPath  = regexp.MustCompile(`(?i)^/api/.+/export`)

	pwdGuardWhitelist = map[string]bool{
		"/api/auth/login":           true,
		"/api/auth/logout":          true,
		"/api/auth/me":              true,
		"/api/auth/change_password": true,
		"/api/client/check_version": true,
		"/api/client/download":      true,
	}
)

// RecoveryWithSanitizedError 全局异常捕获中间件：
// Panic 详情与堆栈仅写入服务端日志，向客户端返回统一脱敏 JSON 与 request_id。
func RecoveryWithSanitizedError() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				rid := core.RandomHex(6)
				log.Printf("[%s] PANIC RECOVERED: %v\n%s", rid, rec, string(debug.Stack()))
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"success":    false,
					"message":    "系统内部错误，请稍后重试或联系管理员",
					"request_id": rid,
				})
			}
		}()
		c.Next()
	}
}

// CORSAndSecurityHeaders 跨域白名单与全局安全响应头中间件（对标 index.php 与 nginx 加固策略）。
func CORSAndSecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		allowedRaw := config.Get("CORS_ORIGINS", "https://ieslab.com,https://www.ieslab.com")
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		if origin != "" {
			for _, item := range strings.Split(allowedRaw, ",") {
				if strings.TrimSpace(item) == origin {
					c.Header("Access-Control-Allow-Origin", origin)
					c.Header("Access-Control-Allow-Credentials", "true")
					c.Header("Vary", "Origin")
					break
				}
			}
		}

		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id, X-Requested-With, X-Token")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusOK)
			return
		}

		c.Next()
	}
}

// SessionAndPasswordGuard 会话初始化、空闲超时维护与「强制改密」全局拦截中间件。
// 当非管理员处于 must_change_password=1 状态时，除认证与公开下载接口外，其余 /api/* 一律返回 403 PASSWORD_CHANGE_REQUIRED，
// 触发前端 request.js 自动跳转至 /change-password 页面。
func SessionAndPasswordGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess := core.GetOrCreateSession(c)

		reqPath := "/" + strings.Trim(c.Request.URL.Path, "/")
		if sess != nil && sess.UserID > 0 && sess.MustChangePassword == 1 {
			if strings.HasPrefix(reqPath, "/api/") && !pwdGuardWhitelist[reqPath] {
				core.Error(c, "为保障账号安全，请先将初始密码修改为符合规则的密码", gin.H{
					"code":                 "PASSWORD_CHANGE_REQUIRED",
					"need_change_password": true,
				}, http.StatusForbidden)
				return
			}
		}

		c.Next()
	}
}

// RateLimitGuard 导出与工具箱转换等 CPU 密集型接口滑动窗口限流中间件（对标 L5 RateLimiter::guard）。
func RateLimitGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if config.GetInt("RATE_LIMIT_ENABLED", 1) != 1 {
			c.Next()
			return
		}

		reqPath := "/" + strings.Trim(c.Request.URL.Path, "/")
		bucket := ""
		maxHits := 0
		windowSec := int64(60)

		if reConvertPath.MatchString(reqPath) {
			bucket = "convert"
			maxHits = config.GetInt("RATE_CONVERT_MAX", 3)
			windowSec = int64(config.GetInt("RATE_CONVERT_WINDOW", 60))
		} else if reExportPath.MatchString(reqPath) {
			bucket = "export"
			maxHits = config.GetInt("RATE_EXPORT_MAX", 30)
			windowSec = int64(config.GetInt("RATE_EXPORT_WINDOW", 60))
		}

		if bucket != "" {
			identity := core.RequestIdentity(c)
			if !core.AllowRateLimit(bucket, identity, maxHits, windowSec) {
				c.Header("Retry-After", fmt.Sprintf("%d", windowSec))
				core.Error(c, "操作过于频繁，请稍后重试", gin.H{
					"code":        "RATE_LIMITED",
					"retry_after": windowSec,
				}, http.StatusTooManyRequests)
				return
			}
		}

		c.Next()
	}
}

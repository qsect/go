package middleware

import (
	"database/sql"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/core"
)

// RequireLogin 登录校验中间件（对标 AuthMiddleware / checkUser）。
func RequireLogin() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess := core.GetOrCreateSession(c)
		if sess == nil || sess.UserID <= 0 || len(sess.User) == 0 {
			core.Error(c, "未登录或登录已超时", gin.H{"need_login": true}, http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// RequireAdmin 管理员权限校验中间件（仅限 is_admin=1 或 admin 账号访问系统管理接口）。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess := core.GetOrCreateSession(c)
		if sess == nil || sess.UserID <= 0 || len(sess.User) == 0 {
			core.Error(c, "未登录或登录已超时", gin.H{"need_login": true}, http.StatusUnauthorized)
			return
		}
		if !core.IsAdminUser(sess.User) {
			core.Error(c, "权限不足，仅管理员可访问系统管理", gin.H{}, http.StatusForbidden)
			return
		}
		c.Next()
	}
}

// RequireTraceView 质量追溯模块专属查看权限中间件（对标 TraceViewMiddleware）：
// 1. 管理员直接放行；
// 2. 普通用户优先检查 trace_permissions.can_view（存在且为 1 放行，为 0 拒绝）；
// 3. 若未单独配置 trace_permissions，则兼容 user_permissions / role_modules 或默认可见。
func RequireTraceView() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess := core.GetOrCreateSession(c)
		if sess == nil || sess.UserID <= 0 || len(sess.User) == 0 {
			core.Error(c, "未登录或登录已超时", gin.H{"need_login": true}, http.StatusUnauthorized)
			return
		}
		if core.IsAdminUser(sess.User) {
			c.Next()
			return
		}
		userID := core.AsInt(sess.User["id"])
		if userID <= 0 {
			denyModuleView(c)
			return
		}
		db, err := core.GetDB()
		if err != nil {
			denyModuleView(c)
			return
		}
		var canView int
		qErr := db.QueryRow("SELECT can_view FROM trace_permissions WHERE user_id = ? LIMIT 1", userID).Scan(&canView)
		if qErr == sql.ErrNoRows {
			c.Next()
			return
		}
		if qErr == nil && canView == 1 {
			c.Next()
			return
		}
		core.Error(c, "权限不足：您没有质量追溯模块的查看权限。请联系管理员在「系统管理 → 权限分配」中为您勾选。", gin.H{}, http.StatusForbidden)
	}
}

// RequireModuleView 模块「查看权限」中间件（对标 ModuleViewMiddleware，严格 fail-closed）：
// 1. 未登录返回 401；
// 2. 管理员直接放行；
// 3. 普通用户：若已配置 user_permissions，则以 can_view=1 为唯一准绳；否则回退检查 role_modules；均无则拒绝（403）。
func RequireModuleView(moduleCode string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if moduleCode == "" {
			c.Next()
			return
		}

		sess := core.GetOrCreateSession(c)
		if sess == nil || sess.UserID <= 0 || len(sess.User) == 0 {
			core.Error(c, "未登录或登录已超时", gin.H{"need_login": true}, http.StatusUnauthorized)
			return
		}

		if core.IsAdminUser(sess.User) {
			c.Next()
			return
		}

		userID := core.AsInt(sess.User["id"])
		if userID <= 0 {
			denyModuleView(c)
			return
		}

		db, err := core.GetDB()
		if err != nil {
			log.Printf("[ModuleViewMiddleware] 数据库连接异常(%s): %v", moduleCode, err)
			denyModuleView(c)
			return
		}

		moduleID := core.ResolveModuleID(db, moduleCode)
		if moduleID <= 0 {
			// 模块尚未登记进 modules 表时不误伤，交由前端菜单与控制器兜底
			c.Next()
			return
		}

		// ① 检查该用户是否已配置过用户级权限记录
		var userPermCount int
		if err := db.QueryRow("SELECT COUNT(*) FROM user_permissions WHERE user_id = ?", userID).Scan(&userPermCount); err != nil {
			log.Printf("[ModuleViewMiddleware] 查询用户权限异常(%s): %v", moduleCode, err)
			denyModuleView(c)
			return
		}

		if userPermCount > 0 {
			var canView int
			err := db.QueryRow("SELECT can_view FROM user_permissions WHERE user_id = ? AND module_id = ? LIMIT 1", userID, moduleID).Scan(&canView)
			if err == nil && canView == 1 {
				c.Next()
				return
			}
			denyModuleView(c)
			return
		}

		// ② 角色级权限兜底
		roleID := core.AsInt(sess.User["role_id"])
		if roleID > 0 {
			var roleGrantCount int
			if err := db.QueryRow("SELECT COUNT(*) FROM role_modules WHERE role_id = ? AND module_id = ?", roleID, moduleID).Scan(&roleGrantCount); err == nil && roleGrantCount > 0 {
				c.Next()
				return
			}
		}

		denyModuleView(c)
	}
}

func denyModuleView(c *gin.Context) {
	core.Error(c, "权限不足：您没有该模块的查看权限。请联系管理员在「系统管理 → 权限分配」中为您勾选「查看」。", gin.H{}, http.StatusForbidden)
}

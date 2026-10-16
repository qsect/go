package controller

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
	"vite-system-go/internal/core"
	"vite-system-go/internal/service"
)

// AuthController 负责系统登录、登出、会话身份核验、左侧分类导航菜单下发及密码修改。
type AuthController struct{}

func NewAuthController() *AuthController {
	return &AuthController{}
}

// Login POST /api/auth/login
func (ctrl *AuthController) Login(c *gin.Context) {
	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	if input == nil {
		input = make(map[string]any)
	}

	username := strings.TrimSpace(core.AsString(input["username"]))
	password := strings.TrimSpace(core.AsString(input["password"]))
	if username == "" {
		username = strings.TrimSpace(c.PostForm("username"))
	}
	if password == "" {
		password = strings.TrimSpace(c.PostForm("password"))
	}

	if username == "" || password == "" {
		core.Error(c, "请输入用户名和密码", gin.H{}, http.StatusBadRequest)
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}

	core.EnsureUserPasswordColumns(db)
	core.EnsureLoginAttemptsTable(db)

	clientIP := core.ClientIP(c)
	maxAttempts := config.GetInt("LOGIN_MAX_ATTEMPTS", 5)
	lockWindow := config.GetInt("LOGIN_LOCK_WINDOW", 600)

	if ctrl.isLoginLocked(db, username, clientIP, maxAttempts, lockWindow) {
		service.LogAudit(c, "系统认证", fmt.Sprintf("用户[%s]登录被限速锁定", username), false, "", 0, input)
		minutes := int(math.Max(1, math.Round(float64(lockWindow)/60.0)))
		core.Error(c, fmt.Sprintf("登录失败次数过多，请 %d 分钟后再试", minutes), gin.H{}, http.StatusTooManyRequests)
		return
	}

	user, err := core.QueryRowMap(db, `
		SELECT u.*, r.name AS real_role_name, r.code AS role_code
		FROM users u
		LEFT JOIN roles r ON u.role_id = r.id
		WHERE u.username = ? LIMIT 1
	`, username)
	if err != nil {
		core.Exception(c, err, "登录查询异常", gin.H{})
		return
	}

	if user == nil {
		ctrl.recordLoginAttempt(db, username, clientIP, false)
		service.LogAudit(c, "系统认证", fmt.Sprintf("用户[%s]登录失败: 用户不存在", username), false, "", 0, input)
		core.Error(c, "账号或密码错误", gin.H{}, http.StatusBadRequest)
		return
	}

	storedPassword := core.AsString(user["password"])
	if !core.VerifyPassword(password, storedPassword) {
		ctrl.recordLoginAttempt(db, username, clientIP, false)
		service.LogAudit(c, "系统认证", fmt.Sprintf("用户[%s]登录失败: 密码错误", username), false, "", 0, input)
		core.Error(c, "账号或密码错误", gin.H{}, http.StatusBadRequest)
		return
	}

	if core.AsInt(user["is_active"]) != 1 {
		ctrl.recordLoginAttempt(db, username, clientIP, false)
		service.LogAudit(c, "系统认证", fmt.Sprintf("用户[%s]登录失败: 账号已禁用", username), false, "", 0, input)
		core.Error(c, "该账号已被禁用，请联系管理员", gin.H{}, http.StatusForbidden)
		return
	}

	// 根据关联角色校正 role 展示名与管理员标记
	realRoleName := core.AsString(user["real_role_name"])
	roleCode := core.AsString(user["role_code"])
	if realRoleName != "" {
		user["role"] = realRoleName
	}

	isAdmin := 0
	if core.AsInt(user["is_admin"]) == 1 || roleCode == "admin" || strings.EqualFold(username, "admin") {
		isAdmin = 1
	}
	user["is_admin"] = isAdmin
	if isAdmin == 0 {
		curRole := core.AsString(user["role"])
		if curRole == "管理员" || curRole == "admin" {
			if realRoleName != "" {
				user["role"] = realRoleName
			} else {
				user["role"] = "普通员工"
			}
		}
	}

	// 密码合规检测（管理员豁免）
	mustChange := 0
	if isAdmin != 1 {
		if core.AsInt(user["must_change_password"]) == 1 || !core.IsStrongPassword(password) {
			mustChange = 1
		}
	}
	user["must_change_password"] = mustChange

	// 历史 MD5 / 明文密码自动升级为现代 bcrypt 哈希
	userID := core.AsInt(user["id"])
	if core.NeedsRehash(storedPassword) {
		if hashed := core.HashPassword(password); hashed != "" {
			if _, upErr := db.Exec("UPDATE users SET password = ?, password_updated_at = NOW() WHERE id = ?", hashed, userID); upErr != nil {
				log.Printf("[Login] 口令哈希自动升级失败: %v", upErr)
			}
		}
	}

	_, _ = db.Exec("UPDATE users SET last_login_at = NOW(), last_login_ip = ? WHERE id = ?", clientIP, userID)
	ctrl.recordLoginAttempt(db, username, clientIP, true)

	// 轮换 Session ID（防会话固定攻击）并写入会话
	delete(user, "password")
	oldSess := core.GetOrCreateSession(c)
	newSess := core.RegenerateSession(c, oldSess)
	newSess.UserID = userID
	newSess.Username = username
	newSess.IsAdmin = isAdmin
	newSess.MustChangePassword = mustChange
	newSess.User = user
	core.SaveSession(c, newSess)

	service.LogAudit(c, "系统认证", fmt.Sprintf("用户[%s]登录成功", username), true, "", 0, input)

	msg := "登录成功"
	if mustChange == 1 {
		msg = "登录成功，请先修改初始密码"
	}
	core.Success(c, msg, gin.H{
		"user":                 user,
		"must_change_password": mustChange,
		"password_rule":        core.PasswordRuleText,
	})
}

// Me GET /api/auth/me
// 每次请求均从数据库实时核验用户状态、管理员身份，并计算当前用户可访问的菜单路由与 9 大导航分组。
func (ctrl *AuthController) Me(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if sess == nil || sess.UserID <= 0 {
		core.Error(c, "未登录或会话已过期", gin.H{}, http.StatusUnauthorized)
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取当前用户信息失败", gin.H{})
		return
	}

	core.EnsureUserPasswordColumns(db)
	core.EnsureModulesTable(db)

	user, err := core.QueryRowMap(db, `
		SELECT u.*, r.name AS real_role_name, r.code AS role_code
		FROM users u
		LEFT JOIN roles r ON u.role_id = r.id
		WHERE u.id = ? LIMIT 1
	`, sess.UserID)
	if err != nil || user == nil || core.AsInt(user["is_active"]) != 1 {
		core.DestroySession(c)
		core.Error(c, "账号状态异常，请重新登录", gin.H{"need_login": true}, http.StatusUnauthorized)
		return
	}

	realRoleName := core.AsString(user["real_role_name"])
	roleCode := core.AsString(user["role_code"])
	username := core.AsString(user["username"])
	if realRoleName != "" {
		user["role"] = realRoleName
	}

	isAdmin := 0
	if core.AsInt(user["is_admin"]) == 1 || roleCode == "admin" || strings.EqualFold(username, "admin") {
		isAdmin = 1
	}
	user["is_admin"] = isAdmin
	if isAdmin == 0 {
		curRole := core.AsString(user["role"])
		if curRole == "管理员" || curRole == "admin" {
			if realRoleName != "" {
				user["role"] = realRoleName
			} else {
				user["role"] = "普通员工"
			}
		}
	}

	mustChange := 0
	if isAdmin != 1 && (core.AsInt(user["must_change_password"]) == 1 || sess.MustChangePassword == 1) {
		mustChange = 1
	}
	user["must_change_password"] = mustChange
	delete(user, "password")

	sess.User = user
	sess.IsAdmin = isAdmin
	sess.MustChangePassword = mustChange
	core.SaveSession(c, sess)

	// 若 modules 表为空（全新环境），自动注入 9 大分类 34 个内置导航模块
	var modCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM modules").Scan(&modCount); err == nil && modCount == 0 {
		_, _, _ = core.SyncBuiltinModules(db)
	}

	menus, managedRoutes, navGroups := ctrl.resolveUserNavigation(db, user, isAdmin == 1)

	core.Success(c, "获取成功", gin.H{
		"user":           user,
		"menus":          menus,
		"managed_routes": managedRoutes,
		"nav_groups":     navGroups,
	})
}

// NavTree GET /api/auth/nav_tree
// 返回系统 9 大分类导航树及当前用户可见的菜单项。
func (ctrl *AuthController) NavTree(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if sess == nil || sess.UserID <= 0 {
		core.Error(c, "未登录或会话已过期", gin.H{"need_login": true}, http.StatusUnauthorized)
		return
	}
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取导航分类失败", gin.H{})
		return
	}
	isAdmin := core.IsAdminUser(sess.User)
	menus, managedRoutes, navGroups := ctrl.resolveUserNavigation(db, sess.User, isAdmin)
	core.Success(c, "获取成功", gin.H{
		"menus":          menus,
		"managed_routes": managedRoutes,
		"nav_groups":     navGroups,
	})
}

func (ctrl *AuthController) resolveUserNavigation(db *sql.DB, user map[string]any, isAdmin bool) ([]map[string]any, []string, []core.NavGroupDef) {
	visibleCol := core.ResolveModuleVisibleColumn(db)

	// 1. 查询全部已启用模块的受控路由
	routeRows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT route, module_code FROM modules WHERE %s = 1", visibleCol))
	routeSet := make(map[string]bool)
	managedRoutes := make([]string, 0, len(routeRows))
	for _, r := range routeRows {
		rt := strings.TrimSpace(core.AsString(r["route"]))
		if rt == "" {
			rt = strings.TrimSpace(core.AsString(r["module_code"]))
		}
		if rt != "" && !routeSet[rt] {
			routeSet[rt] = true
			managedRoutes = append(managedRoutes, rt)
		}
	}

	selectCols := `m.id, m.parent_id, m.name AS title, m.module_code AS code,
		COALESCE(NULLIF(m.route, ''), m.module_code) AS path,
		m.icon, m.type, m.sort_order AS sort, m.category`

	var modules []map[string]any
	if isAdmin {
		modules, _ = core.QueryMaps(db, fmt.Sprintf(`
			SELECT %s FROM modules m
			WHERE m.%s = 1
			ORDER BY m.sort_order ASC, m.id ASC
		`, selectCols, visibleCol))
	} else {
		uid := core.AsInt(user["id"])
		rid := core.AsInt(user["role_id"])

		var userPermCnt, rolePermCnt int
		_ = db.QueryRow("SELECT COUNT(*) FROM user_permissions WHERE user_id = ?", uid).Scan(&userPermCnt)
		_ = db.QueryRow("SELECT COUNT(*) FROM role_modules WHERE role_id = ?", rid).Scan(&rolePermCnt)

		rows, _ := core.QueryMaps(db, fmt.Sprintf(`
			SELECT %s,
			       up.can_view AS up_view,
			       (CASE WHEN rm.module_id IS NULL THEN 0 ELSE 1 END) AS role_grant
			FROM modules m
			LEFT JOIN user_permissions up ON up.module_id = m.id AND up.user_id = ?
			LEFT JOIN role_modules rm ON rm.module_id = m.id AND rm.role_id = ?
			WHERE m.%s = 1
			ORDER BY m.sort_order ASC, m.id ASC
		`, selectCols, visibleCol), uid, rid)

		modules = make([]map[string]any, 0)
		for _, r := range rows {
			granted := false
			if userPermCnt > 0 {
				granted = core.AsInt(r["up_view"]) == 1
			} else if rolePermCnt > 0 {
				granted = core.AsInt(r["role_grant"]) == 1
			}
			if granted {
				delete(r, "up_view")
				delete(r, "role_grant")
				modules = append(modules, r)
			}
		}
	}

	if modules == nil {
		modules = make([]map[string]any, 0)
	}

	// 构建按用户权限过滤后的 9 大导航分组树（与前端 Layout.vue 逻辑完全一致）
	allowedPaths := make(map[string]bool, len(modules))
	for _, m := range modules {
		p := strings.Trim(core.AsString(m["path"]), "/")
		if p != "" {
			allowedPaths[p] = true
		}
	}

	allGroups := core.BuiltinNavGroups()
	visibleGroups := make([]core.NavGroupDef, 0, len(allGroups))
	for _, g := range allGroups {
		if g.Key == "system" && !isAdmin {
			continue
		}
		items := make([]core.BuiltinModule, 0, len(g.Items))
		for _, item := range g.Items {
			norm := strings.Trim(item.Path, "/")
			if isAdmin || allowedPaths[norm] {
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			g.Items = items
			visibleGroups = append(visibleGroups, g)
		}
	}

	return modules, managedRoutes, visibleGroups
}

// ChangePassword POST /api/auth/change_password
func (ctrl *AuthController) ChangePassword(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if sess == nil || sess.UserID <= 0 {
		core.Error(c, "未登录或会话已过期", gin.H{}, http.StatusUnauthorized)
		return
	}

	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	if input == nil {
		input = make(map[string]any)
	}

	oldPassword := core.AsString(input["old_password"])
	newPassword := core.AsString(input["new_password"])
	confirmPassword := core.AsString(input["confirm_password"])

	if oldPassword == "" || newPassword == "" {
		core.Error(c, "请输入原密码与新密码", gin.H{})
		return
	}
	if confirmPassword != "" && confirmPassword != newPassword {
		core.Error(c, "两次输入的新密码不一致", gin.H{})
		return
	}
	if newPassword == oldPassword {
		core.Error(c, "新密码不能与原密码相同", gin.H{})
		return
	}
	if !core.IsStrongPassword(newPassword) {
		core.Error(c, core.DescribePasswordStrength(newPassword), gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "密码修改失败", gin.H{})
		return
	}

	core.EnsureUserPasswordColumns(db)

	user, err := core.QueryRowMap(db, "SELECT id, username, password, is_admin FROM users WHERE id = ? LIMIT 1", sess.UserID)
	if err != nil {
		core.Exception(c, err, "密码修改失败", gin.H{})
		return
	}
	if user == nil {
		core.Error(c, "账号不存在或已被删除，请重新登录", gin.H{"need_login": true}, http.StatusUnauthorized)
		return
	}

	username := core.AsString(user["username"])
	if !core.VerifyPassword(oldPassword, core.AsString(user["password"])) {
		service.LogAudit(c, "系统认证", fmt.Sprintf("用户[%s]修改密码失败: 原密码不正确", username), false, "", 0, input)
		core.Error(c, "原密码不正确，请重新输入", gin.H{})
		return
	}

	hashed := core.HashPassword(newPassword)
	if _, err := db.Exec(`
		UPDATE users
		SET password = ?, must_change_password = 0, password_updated_at = NOW()
		WHERE id = ?
	`, hashed, sess.UserID); err != nil {
		core.Exception(c, err, "密码修改失败", gin.H{})
		return
	}

	sess.MustChangePassword = 0
	if sess.User != nil {
		sess.User["must_change_password"] = 0
	}
	core.SaveSession(c, sess)

	service.LogAudit(c, "系统认证", fmt.Sprintf("用户[%s]修改密码成功", username), true, "", 0, input)
	core.Success(c, "密码修改成功，请牢记新密码", gin.H{
		"must_change_password": 0,
	})
}

// Logout POST /api/auth/logout
func (ctrl *AuthController) Logout(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if sess != nil && sess.UserID > 0 {
		service.LogAudit(c, "系统认证", "用户退出登录", true, "", 0)
	}
	core.DestroySession(c)
	core.Success(c, "已退出登录", gin.H{})
}

func (ctrl *AuthController) isLoginLocked(db *sql.DB, username, ip string, limit, windowSeconds int) bool {
	if limit <= 0 || windowSeconds <= 0 {
		return false
	}
	since := time.Now().Add(-time.Duration(windowSeconds) * time.Second).Format("2006-01-02 15:04:05")
	var cnt int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM login_attempts
		WHERE success = 0 AND attempted_at >= ? AND (username = ? OR ip = ?)
	`, since, username, ip).Scan(&cnt)
	if err != nil {
		return false
	}
	return cnt >= limit
}

func (ctrl *AuthController) recordLoginAttempt(db *sql.DB, username, ip string, success bool) {
	if success {
		_, _ = db.Exec("DELETE FROM login_attempts WHERE success = 0 AND (username = ? OR ip = ?)", username, ip)
		return
	}
	_, _ = db.Exec("INSERT INTO login_attempts (username, ip, success, attempted_at) VALUES (?, ?, 0, NOW())", username, ip)
	_, _ = db.Exec("DELETE FROM login_attempts WHERE attempted_at < (NOW() - INTERVAL 1 DAY)")
}

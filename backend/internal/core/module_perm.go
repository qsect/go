package core

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

var (
	moduleIDCache = make(map[string]int)
	modCacheMu    sync.RWMutex
	leaderPattern = regexp.MustCompile(`(?i)(班长|组长|主管|领班|主任|部长|科长|经理|foreman|leader|supervisor)`)
)

// IsAdminUser 判定当前会话用户是否为系统管理员（is_admin=1 或账号名为 admin）。
func IsAdminUser(user map[string]any) bool {
	if len(user) == 0 {
		return false
	}
	if AsInt(user["is_admin"]) == 1 {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(AsString(user["username"])), "admin")
}

// IsLeader 识别「班长 / 组长 / 主管」等一线管理角色（对标 App\Core\UserRole::isLeader）。
func IsLeader(user map[string]any) bool {
	if len(user) == 0 {
		return false
	}
	haystack := strings.Join([]string{
		AsString(user["role"]),
		AsString(user["group_name"]),
		AsString(user["department"]),
		AsString(user["team"]),
	}, " ")
	return leaderPattern.MatchString(haystack)
}

// CanMaintain 是否可维护业务档案数据 = 超级管理员 或 班长级管理角色。
func CanMaintain(user map[string]any) bool {
	if IsAdminUser(user) {
		return true
	}
	return IsLeader(user)
}

// ResolveModuleID 解析模块 ID：优先匹配 module_code，兜底匹配 route（兼容有无前导斜杠）。
func ResolveModuleID(db *sql.DB, moduleCode string) int {
	code := strings.Trim(strings.TrimSpace(moduleCode), "/")
	if code == "" {
		return 0
	}

	modCacheMu.RLock()
	if id, ok := moduleIDCache[code]; ok && id > 0 {
		modCacheMu.RUnlock()
		return id
	}
	modCacheMu.RUnlock()

	hyphenCode := strings.ReplaceAll(code, "_", "-")
	var id int
	err := db.QueryRow(`
		SELECT id FROM modules
		WHERE module_code = ? OR route = ? OR route = ? OR route = ?
		ORDER BY (module_code = ?) DESC, id ASC
		LIMIT 1
	`, code, code, hyphenCode, "/"+hyphenCode, code).Scan(&id)
	if err != nil || id <= 0 {
		return 0
	}

	modCacheMu.Lock()
	moduleIDCache[code] = id
	modCacheMu.Unlock()
	return id
}

// ClearModuleIDCache 在模块增删改后清空模块 ID 缓存。
func ClearModuleIDCache() {
	modCacheMu.Lock()
	moduleIDCache = make(map[string]int)
	modCacheMu.Unlock()
}

// CanEditModule 判定当前用户是否可维护指定模块的数据（对标 App\Core\ModulePerm::canEdit）：
// 1. 管理员恒可维护；
// 2. 普通用户必须在「系统管理 → 权限分配」中显式勾选 can_edit = 1；
// 3. 未配置或异常一律按不可维护处理（fail-closed）。
func CanEditModule(user map[string]any, moduleCode string) bool {
	if len(user) == 0 {
		return false
	}
	if IsAdminUser(user) {
		return true
	}

	userID := AsInt(user["id"])
	if userID <= 0 || strings.TrimSpace(moduleCode) == "" {
		return false
	}

	db, err := GetDB()
	if err != nil {
		log.Printf("[ModulePerm] 数据库连接失败: %v", err)
		return false
	}

	moduleID := ResolveModuleID(db, moduleCode)
	if moduleID <= 0 {
		return false
	}

	var canEdit int
	err = db.QueryRow("SELECT can_edit FROM user_permissions WHERE user_id = ? AND module_id = ? LIMIT 1", userID, moduleID).Scan(&canEdit)
	if err != nil {
		return false
	}
	return canEdit == 1
}

// DenyModuleEdit 输出统一的 403 维护权限不足响应。
func DenyModuleEdit(c *gin.Context, actionLabel string) {
	if actionLabel == "" {
		actionLabel = "该操作"
	}
	msg := fmt.Sprintf("权限不足：您没有%s的维护权限。请联系管理员在「系统管理 → 权限分配」中为您勾选「允许编辑与数据管理」。", actionLabel)
	Error(c, msg, gin.H{}, http.StatusForbidden)
}

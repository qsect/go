package controller

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/core"
	"vite-system-go/internal/service"
)

// SystemController 承载「系统管理」分类下的用户管理、角色权限、权限分配（含 6 类模块细分权限）、模块管理与审计日志接口。
type SystemController struct{}

func NewSystemController() *SystemController {
	return &SystemController{}
}

// Users GET /api/system/users
func (ctrl *SystemController) Users(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取用户列表失败", gin.H{})
		return
	}

	core.EnsureUserPasswordColumns(db)

	keyword := strings.TrimSpace(c.Query("keyword"))
	where := "1=1"
	var args []any

	if keyword != "" {
		where += " AND (u.username LIKE ? OR u.full_name LIKE ? OR u.badge_no LIKE ? OR u.phone LIKE ?)"
		kw := "%" + keyword + "%"
		args = append(args, kw, kw, kw, kw)
	}

	sqlStr := fmt.Sprintf(`
		SELECT u.id, u.username, u.badge_no, u.full_name, u.team, u.role_id, u.phone,
		       u.is_admin, u.is_active, u.must_change_password, u.password_updated_at, r.name AS role_name
		FROM users u
		LEFT JOIN roles r ON u.role_id = r.id
		WHERE %s
		ORDER BY u.id DESC
	`, where)

	list, err := core.QueryMaps(db, sqlStr, args...)
	if err != nil {
		core.Exception(c, err, "获取用户列表失败", gin.H{})
		return
	}

	core.Success(c, "success", list)
}

// SaveUser POST /api/system/users/save
func (ctrl *SystemController) SaveUser(c *gin.Context) {
	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	if input == nil {
		input = make(map[string]any)
	}

	id := core.AsInt(input["id"])
	badgeNo := strings.TrimSpace(core.AsString(input["badge_no"]))
	username := strings.TrimSpace(core.AsString(input["username"]))
	fullName := strings.TrimSpace(core.AsString(input["full_name"]))
	team := strings.TrimSpace(core.AsString(input["team"]))
	roleID := core.AsInt(input["role_id"])
	phone := strings.TrimSpace(core.AsString(input["phone"]))
	password := strings.TrimSpace(core.AsString(input["password"]))

	isAdmin := 0
	if core.AsInt(input["is_admin"]) == 1 {
		isAdmin = 1
	}
	isActive := 1
	if _, exists := input["is_active"]; exists {
		isActive = core.AsInt(input["is_active"])
	}

	if username == "" || fullName == "" {
		core.Error(c, "用户名和姓名不能为空", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存用户失败", gin.H{})
		return
	}

	core.EnsureUserPasswordColumns(db)

	// 根据选中的 role_id 动态解析标准角色名称与管理员身份
	roleText := "员工"
	var roleIDParam any = nil
	if roleID > 0 {
		roleIDParam = roleID
		roleRow, _ := core.QueryRowMap(db, "SELECT name, code FROM roles WHERE id = ? LIMIT 1", roleID)
		if roleRow != nil {
			roleText = core.AsString(roleRow["name"])
			rCode := core.AsString(roleRow["code"])
			if rCode == "admin" || strings.Contains(roleText, "管理员") {
				isAdmin = 1
			}
		}
	}
	if isAdmin == 1 && roleText == "员工" {
		roleText = "管理员"
	}

	if id > 0 {
		// 更新已有用户
		if password != "" {
			hashed := core.HashPassword(password)
			mustChange := 1
			if isAdmin == 1 {
				mustChange = 0
			}
			_, err = db.Exec(`
				UPDATE users
				SET badge_no = ?, full_name = ?, team = ?, role_id = ?, role = ?,
				    phone = ?, is_admin = ?, is_active = ?,
				    password = ?, password_updated_at = NOW(), must_change_password = ?
				WHERE id = ?
			`, badgeNo, fullName, team, roleIDParam, roleText, phone, isAdmin, isActive, hashed, mustChange, id)
		} else {
			_, err = db.Exec(`
				UPDATE users
				SET badge_no = ?, full_name = ?, team = ?, role_id = ?, role = ?,
				    phone = ?, is_admin = ?, is_active = ?
				WHERE id = ?
			`, badgeNo, fullName, team, roleIDParam, roleText, phone, isAdmin, isActive, id)
		}
		if err != nil {
			core.Exception(c, err, "保存用户失败", gin.H{})
			return
		}

		// 自动联动校正全库非管理员用户的 role 字段
		_, _ = db.Exec(`
			UPDATE users u
			LEFT JOIN roles r ON u.role_id = r.id
			SET u.role = COALESCE(r.name, '普通用户')
			WHERE u.is_admin = 0 AND (u.role = '管理员' OR u.role IS NULL)
		`)

		actionNote := fmt.Sprintf("修改用户 [%s] 信息", username)
		if password != "" {
			actionNote += " (包含重置密码)"
		}
		service.LogAudit(c, "用户管理", actionNote, true, "", 0, input)
		core.Success(c, "用户信息更新成功", gin.H{})
		return
	}

	// 新增用户
	if password == "" {
		core.Error(c, "新增用户时密码不能为空", gin.H{})
		return
	}

	var existID int
	if err := db.QueryRow("SELECT id FROM users WHERE username = ? LIMIT 1", username).Scan(&existID); err == nil && existID > 0 {
		core.Error(c, fmt.Sprintf("用户名 [%s] 已存在，请更换", username), gin.H{})
		return
	}

	hashed := core.HashPassword(password)
	mustChange := 1
	if isAdmin == 1 {
		mustChange = 0
	}

	res, err := db.Exec(`
		INSERT INTO users
		(badge_no, username, full_name, team, role_id, role, phone, password, is_admin, is_active, must_change_password, password_updated_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())
	`, badgeNo, username, fullName, team, roleIDParam, roleText, phone, hashed, isAdmin, isActive, mustChange)
	if err != nil {
		core.Exception(c, err, "保存用户失败", gin.H{})
		return
	}

	newID, _ := res.LastInsertId()
	service.LogAudit(c, "用户管理", fmt.Sprintf("新建用户 [%s]", username), true, "", 0, input)
	core.Success(c, "用户创建成功", gin.H{
		"id":                   int(newID),
		"must_change_password": mustChange,
	})
}

// ResetAllPasswords POST /api/system/users/reset-all-passwords
func (ctrl *SystemController) ResetAllPasswords(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "批量重置密码失败", gin.H{})
		return
	}

	core.EnsureUserPasswordColumns(db)

	hashed := core.HashPassword(core.DefaultPassword)
	res, err := db.Exec(`
		UPDATE users u
		LEFT JOIN roles r ON u.role_id = r.id
		SET u.password = ?,
		    u.must_change_password = 1,
		    u.password_updated_at = NOW()
		WHERE (u.is_admin = 0 OR u.is_admin IS NULL)
		  AND (r.code IS NULL OR r.code <> 'admin')
	`, hashed)
	if err != nil {
		core.Exception(c, err, "批量重置密码失败", gin.H{})
		return
	}

	count, _ := res.RowsAffected()
	service.LogAudit(c, "用户管理", fmt.Sprintf("一键重置全部非管理员账号密码为初始密码（影响 %d 个账号）", count), true, "", 0)

	core.Success(c, fmt.Sprintf("已重置 %d 个非管理员账号的密码为 %s，他们下次登录时必须修改密码", count, core.DefaultPassword), gin.H{
		"count":            count,
		"default_password": core.DefaultPassword,
	})
}

// DeleteUser POST /api/system/users/delete
func (ctrl *SystemController) DeleteUser(c *gin.Context) {
	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	id := core.AsInt(input["id"])
	if id <= 0 {
		id, _ = strconv.Atoi(c.Query("id"))
	}
	if id <= 0 {
		core.Error(c, "请提供有效的用户ID", gin.H{})
		return
	}

	sess := core.GetOrCreateSession(c)
	if sess != nil && id == sess.UserID {
		core.Error(c, "不能删除当前登录的账号", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}

	if _, err := db.Exec("DELETE FROM users WHERE id = ?", id); err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}

	service.LogAudit(c, "用户管理", fmt.Sprintf("删除用户 ID: [%d]", id), true, "", 0, input)
	core.Success(c, "用户已删除", gin.H{})
}

// Roles GET /api/system/roles
func (ctrl *SystemController) Roles(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取角色列表失败", gin.H{})
		return
	}

	core.EnsurePresetRoles(db)

	list, err := core.QueryMaps(db, "SELECT * FROM roles ORDER BY id ASC")
	if err != nil {
		core.Exception(c, err, "获取角色列表失败", gin.H{})
		return
	}

	core.Success(c, "success", list)
}

// RoleModules GET /api/system/role-modules
func (ctrl *SystemController) RoleModules(c *gin.Context) {
	roleID, _ := strconv.Atoi(c.Query("role_id"))
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取角色模块权限失败", gin.H{})
		return
	}

	rows, err := db.Query("SELECT module_id FROM role_modules WHERE role_id = ?", roleID)
	if err != nil {
		core.Exception(c, err, "获取角色模块权限失败", gin.H{})
		return
	}
	defer rows.Close()

	ids := make([]int, 0)
	for rows.Next() {
		var mid int
		if err := rows.Scan(&mid); err == nil {
			ids = append(ids, mid)
		}
	}

	core.Success(c, "success", ids)
}

// SaveRolePermissions POST /api/system/role-modules/save
func (ctrl *SystemController) SaveRolePermissions(c *gin.Context) {
	var input struct {
		RoleID    int   `json:"role_id"`
		ModuleIDs []int `json:"module_ids"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || input.RoleID <= 0 {
		core.Error(c, "请选择角色", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	if _, err := tx.Exec("DELETE FROM role_modules WHERE role_id = ?", input.RoleID); err != nil {
		_ = tx.Rollback()
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	if len(input.ModuleIDs) > 0 {
		stmt, err := tx.Prepare("INSERT INTO role_modules (role_id, module_id) VALUES (?, ?)")
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
		defer stmt.Close()

		for _, mid := range input.ModuleIDs {
			if mid > 0 {
				if _, err := stmt.Exec(input.RoleID, mid); err != nil {
					_ = tx.Rollback()
					core.Exception(c, err, "保存失败", gin.H{})
					return
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	service.LogAudit(c, "角色权限", fmt.Sprintf("更新角色 #%d 模块权限", input.RoleID), true, "", 0)
	core.Success(c, "角色权限配置已保存", gin.H{})
}

// UserPermissions GET /api/system/permissions
// 查询用户通用模块权限及全部 6 类细分权限（trace / barcode / discuss / fp / mac / osm）。
func (ctrl *SystemController) UserPermissions(c *gin.Context) {
	userID, _ := strconv.Atoi(c.Query("user_id"))
	if userID <= 0 {
		core.Error(c, "无效用户", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取用户权限失败", gin.H{})
		return
	}

	core.EnsureModulesTable(db)
	core.EnsureAllPermTables(db)

	perms, err := core.QueryMaps(db, `
		SELECT m.id AS module_id, m.name AS module_name, m.module_code, m.category AS category_name,
		       COALESCE(up.can_view, 0) AS can_view,
		       COALESCE(up.can_edit, 0) AS can_edit
		FROM modules m
		LEFT JOIN user_permissions up ON m.id = up.module_id AND up.user_id = ?
		ORDER BY m.sort_order ASC, m.id ASC
	`, userID)
	if err != nil {
		core.Exception(c, err, "获取用户权限失败", gin.H{})
		return
	}

	tracePerms, _ := core.QueryRowMap(db, "SELECT * FROM trace_permissions WHERE user_id = ? LIMIT 1", userID)
	if tracePerms == nil {
		tracePerms = map[string]any{}
	}

	barcodePerms, _ := core.QueryRowMap(db, "SELECT can_save_settings, can_delete, can_edit FROM barcode_validator_permissions WHERE user_id = ? LIMIT 1", userID)
	if barcodePerms == nil {
		barcodePerms = map[string]any{}
	}

	discussPerms, _ := core.QueryRowMap(db, "SELECT can_view, can_create_topic, can_reply, can_edit, can_delete FROM discuss_permissions WHERE user_id = ? LIMIT 1", userID)
	if discussPerms == nil {
		discussPerms = map[string]any{}
	}

	fpPerms, _ := core.QueryRowMap(db, "SELECT can_manage_criteria FROM first_piece_permissions WHERE user_id = ? LIMIT 1", userID)
	if fpPerms == nil {
		fpPerms = map[string]any{}
	}

	macPerms, _ := core.QueryRowMap(db, "SELECT can_apply, can_config_ports, can_import, can_view_records, can_view_stats, can_view_pool, can_view_logs FROM mac_permissions WHERE user_id = ? LIMIT 1", userID)
	if macPerms == nil {
		macPerms = map[string]any{}
	}

	osmPerms, _ := core.QueryRowMap(db, "SELECT can_edit, can_delete, can_upload, can_add_return FROM osm_permissions WHERE user_id = ? LIMIT 1", userID)
	if osmPerms == nil {
		osmPerms = map[string]any{}
	}

	core.Success(c, "success", gin.H{
		"permissions":   perms,
		"trace_perms":   tracePerms,
		"barcode_perms": barcodePerms,
		"discuss_perms": discussPerms,
		"fp_perms":      fpPerms,
		"mac_perms":     macPerms,
		"osm_perms":     osmPerms,
	})
}

// SaveUserPermissions POST /api/system/permissions/save
func (ctrl *SystemController) SaveUserPermissions(c *gin.Context) {
	var input map[string]any
	if err := c.ShouldBindJSON(&input); err != nil {
		core.Error(c, "请求数据格式错误", gin.H{})
		return
	}

	userID := core.AsInt(input["user_id"])
	if userID <= 0 {
		core.Error(c, "无效用户", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	// DDL 必须在事务开始前执行，避免隐式提交
	core.EnsureAllPermTables(db)

	tx, err := db.Begin()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	// 1. 通用模块权限
	if permSlice, ok := input["permissions"].([]any); ok && len(permSlice) > 0 {
		stmt, err := tx.Prepare(`
			INSERT INTO user_permissions (user_id, module_id, can_view, can_edit, updated_at)
			VALUES (?, ?, ?, ?, NOW())
			ON DUPLICATE KEY UPDATE can_view = VALUES(can_view), can_edit = VALUES(can_edit), updated_at = NOW()
		`)
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
		defer stmt.Close()

		for _, item := range permSlice {
			if pm, ok := item.(map[string]any); ok {
				mid := core.AsInt(pm["module_id"])
				if mid > 0 {
					cv := 0
					if core.AsInt(pm["can_view"]) != 0 {
						cv = 1
					}
					ce := 0
					if core.AsInt(pm["can_edit"]) != 0 {
						ce = 1
					}
					if _, err := stmt.Exec(userID, mid, cv, ce); err != nil {
						_ = tx.Rollback()
						core.Exception(c, err, "保存失败", gin.H{})
						return
					}
				}
			}
		}
	}

	// 2. 质量追溯细分权限
	if tp, ok := input["trace_perms"].(map[string]any); ok && len(tp) > 0 {
		_, err := tx.Exec(`
			INSERT INTO trace_permissions (user_id, can_add_material, can_scan, can_view, can_edit_record, can_delete_record, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, NOW())
			ON DUPLICATE KEY UPDATE
				can_add_material = VALUES(can_add_material),
				can_scan = VALUES(can_scan),
				can_view = VALUES(can_view),
				can_edit_record = VALUES(can_edit_record),
				can_delete_record = VALUES(can_delete_record),
				updated_at = NOW()
		`, userID,
			boolInt(tp["can_add_material"]),
			boolInt(tp["can_scan"]),
			boolInt(tp["can_view"]),
			boolInt(tp["can_edit_record"]),
			boolInt(tp["can_delete_record"]),
		)
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
	}

	// 3. 条码验证比对细分权限
	if bp, ok := input["barcode_perms"].(map[string]any); ok && len(bp) > 0 {
		_, err := tx.Exec(`
			INSERT INTO barcode_validator_permissions (user_id, can_save_settings, can_delete, can_edit, updated_at)
			VALUES (?, ?, ?, ?, NOW())
			ON DUPLICATE KEY UPDATE
				can_save_settings = VALUES(can_save_settings),
				can_delete = VALUES(can_delete),
				can_edit = VALUES(can_edit),
				updated_at = NOW()
		`, userID,
			boolInt(bp["can_save_settings"]),
			boolInt(bp["can_delete"]),
			boolInt(bp["can_edit"]),
		)
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
	}

	// 4. 综合讨论交流区细分权限
	if dp, ok := input["discuss_perms"].(map[string]any); ok && len(dp) > 0 {
		_, err := tx.Exec(`
			INSERT INTO discuss_permissions (user_id, can_view, can_create_topic, can_reply, can_edit, can_delete, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, NOW())
			ON DUPLICATE KEY UPDATE
				can_view = VALUES(can_view),
				can_create_topic = VALUES(can_create_topic),
				can_reply = VALUES(can_reply),
				can_edit = VALUES(can_edit),
				can_delete = VALUES(can_delete),
				updated_at = NOW()
		`, userID,
			boolInt(dp["can_view"]),
			boolInt(dp["can_create_topic"]),
			boolInt(dp["can_reply"]),
			boolInt(dp["can_edit"]),
			boolInt(dp["can_delete"]),
		)
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
	}

	// 5. 首件检判据管理细分权限
	if fp, ok := input["fp_perms"].(map[string]any); ok && len(fp) > 0 {
		_, err := tx.Exec(`
			INSERT INTO first_piece_permissions (user_id, can_manage_criteria, updated_at)
			VALUES (?, ?, NOW())
			ON DUPLICATE KEY UPDATE
				can_manage_criteria = VALUES(can_manage_criteria),
				updated_at = NOW()
		`, userID, boolInt(fp["can_manage_criteria"]))
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
	}

	// 6. MAC 地址管理细分权限
	if mp, ok := input["mac_perms"].(map[string]any); ok && len(mp) > 0 {
		_, err := tx.Exec(`
			INSERT INTO mac_permissions (user_id, can_apply, can_config_ports, can_import, can_view_records, can_view_stats, can_view_pool, can_view_logs, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NOW())
			ON DUPLICATE KEY UPDATE
				can_apply = VALUES(can_apply),
				can_config_ports = VALUES(can_config_ports),
				can_import = VALUES(can_import),
				can_view_records = VALUES(can_view_records),
				can_view_stats = VALUES(can_view_stats),
				can_view_pool = VALUES(can_view_pool),
				can_view_logs = VALUES(can_view_logs),
				updated_at = NOW()
		`, userID,
			boolInt(mp["can_apply"]),
			boolInt(mp["can_config_ports"]),
			boolInt(mp["can_import"]),
			boolInt(mp["can_view_records"]),
			boolInt(mp["can_view_stats"]),
			boolInt(mp["can_view_pool"]),
			boolInt(mp["can_view_logs"]),
		)
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
	}

	// 7. 外协管理系统细分权限
	if op, ok := input["osm_perms"].(map[string]any); ok && len(op) > 0 {
		_, err := tx.Exec(`
			INSERT INTO osm_permissions (user_id, can_edit, can_delete, can_upload, can_add_return, updated_at)
			VALUES (?, ?, ?, ?, ?, NOW())
			ON DUPLICATE KEY UPDATE
				can_edit = VALUES(can_edit),
				can_delete = VALUES(can_delete),
				can_upload = VALUES(can_upload),
				can_add_return = VALUES(can_add_return),
				updated_at = NOW()
		`, userID,
			boolInt(op["can_edit"]),
			boolInt(op["can_delete"]),
			boolInt(op["can_upload"]),
			boolInt(op["can_add_return"]),
		)
		if err != nil {
			_ = tx.Rollback()
			core.Exception(c, err, "保存失败", gin.H{})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	service.LogAudit(c, "权限分配", fmt.Sprintf("更新用户 #%d 模块与细分权限", userID), true, "", 0)
	core.Success(c, "权限已成功保存", gin.H{})
}

func boolInt(v any) int {
	if core.AsInt(v) != 0 {
		return 1
	}
	return 0
}

// AuditLogs GET /api/system/audit-logs
func (ctrl *SystemController) AuditLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "15"))
	if limit < 1 {
		limit = 15
	}
	offset := (page - 1) * limit

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取审计日志失败", gin.H{})
		return
	}

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&total)

	list, err := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM audit_logs ORDER BY id DESC LIMIT %d, %d", offset, limit))
	if err != nil {
		core.Exception(c, err, "获取审计日志失败", gin.H{})
		return
	}

	core.Success(c, "success", gin.H{
		"list":  list,
		"total": total,
	})
}

// Modules GET /api/system/modules
// 同时兼容：
// 1. Modules.vue（始终携带 ?category=&search= 参数，要求返回 { list, categories, total }）
// 2. Roles.vue（无查询参数调用 /api/system/modules，要求直接返回带有 parent_id 和 title 的数组以构建权限树）
func (ctrl *SystemController) Modules(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取模块列表失败", gin.H{})
		return
	}

	core.EnsureModulesTable(db)

	var totalInDB int
	if err := db.QueryRow("SELECT COUNT(*) FROM modules").Scan(&totalInDB); err == nil && totalInDB == 0 {
		_, _, _ = core.SyncBuiltinModules(db)
	}

	category := strings.TrimSpace(c.Query("category"))
	search := strings.TrimSpace(c.Query("search"))

	where := "1=1"
	var args []any
	if category != "" {
		where += " AND category = ?"
		args = append(args, category)
	}
	if search != "" {
		kw := "%" + search + "%"
		where += " AND (name LIKE ? OR module_code LIKE ? OR description LIKE ?)"
		args = append(args, kw, kw, kw)
	}

	list, err := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM modules WHERE %s ORDER BY sort_order ASC, id ASC", where), args...)
	if err != nil {
		core.Exception(c, err, "获取模块列表失败", gin.H{})
		return
	}

	for _, item := range list {
		if _, hasParent := item["parent_id"]; !hasParent || item["parent_id"] == nil {
			item["parent_id"] = 0
		}
		item["title"] = core.AsString(item["name"])
	}

	// 判断是否为 Roles.vue 的无参树形调用
	q := c.Request.URL.Query()
	_, hasCatParam := q["category"]
	_, hasSearchParam := q["search"]
	if !hasCatParam && !hasSearchParam {
		core.Success(c, "success", list)
		return
	}

	catRows, _ := db.Query("SELECT DISTINCT category FROM modules WHERE category IS NOT NULL AND category != '' ORDER BY category ASC")
	cats := make([]string, 0)
	if catRows != nil {
		defer catRows.Close()
		for catRows.Next() {
			var cat string
			if err := catRows.Scan(&cat); err == nil && cat != "" {
				cats = append(cats, cat)
			}
		}
	}

	core.Success(c, "success", gin.H{
		"list":       list,
		"categories": cats,
		"total":      len(list),
	})
}

// SaveModule POST /api/system/modules/save
func (ctrl *SystemController) SaveModule(c *gin.Context) {
	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	if input == nil {
		input = make(map[string]any)
	}

	id := core.AsInt(input["id"])
	name := strings.TrimSpace(core.AsString(input["name"]))
	code := strings.TrimSpace(core.AsString(input["module_code"]))
	category := strings.TrimSpace(core.AsString(input["category"]))
	icon := strings.TrimSpace(core.AsString(input["icon"]))
	if icon == "" {
		icon = "Menu"
	}
	route := strings.Trim(strings.TrimSpace(core.AsString(input["route"])), "/")
	if route == "" {
		route = strings.ReplaceAll(code, "_", "-")
	}
	sortOrder := core.AsInt(input["sort_order"])
	if sortOrder == 0 {
		sortOrder = 10
	}
	description := strings.TrimSpace(core.AsString(input["description"]))

	if name == "" || code == "" {
		core.Error(c, "模块名称和代码不能为空", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	core.EnsureModulesTable(db)

	if id > 0 {
		_, err = db.Exec(`
			UPDATE modules
			SET name = ?, module_code = ?, category = ?, icon = ?, route = ?, sort_order = ?, description = ?
			WHERE id = ?
		`, name, code, category, icon, route, sortOrder, description, id)
	} else {
		_, err = db.Exec(`
			INSERT INTO modules (parent_id, name, module_code, category, icon, route, sort_order, description, is_visible, status)
			VALUES (0, ?, ?, ?, ?, ?, ?, ?, 1, 1)
		`, name, code, category, icon, route, sortOrder, description)
	}
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}

	core.ClearModuleIDCache()
	service.LogAudit(c, "模块管理", fmt.Sprintf("保存模块 [%s (%s)]", name, code), true, "", 0, input)
	core.Success(c, "模块保存成功", gin.H{})
}

// ToggleModuleStatus POST /api/system/modules/toggle-status
func (ctrl *SystemController) ToggleModuleStatus(c *gin.Context) {
	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	id := core.AsInt(input["id"])
	if id <= 0 {
		core.Error(c, "无效模块ID", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "操作失败", gin.H{})
		return
	}

	if _, err := db.Exec("UPDATE modules SET status = IF(status = 1, 0, 1), is_visible = IF(status = 1, 0, 1) WHERE id = ?", id); err != nil {
		// 兼容仅有 status 列的表结构
		if _, err2 := db.Exec("UPDATE modules SET status = IF(status = 1, 0, 1) WHERE id = ?", id); err2 != nil {
			core.Exception(c, err2, "操作失败", gin.H{})
			return
		}
	}

	core.ClearModuleIDCache()
	service.LogAudit(c, "模块管理", fmt.Sprintf("切换模块 #%d 启停状态", id), true, "", 0, input)
	core.Success(c, "状态变更成功", gin.H{})
}

// DeleteModule POST /api/system/modules/delete
func (ctrl *SystemController) DeleteModule(c *gin.Context) {
	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	id := core.AsInt(input["id"])
	if id <= 0 {
		core.Error(c, "无效模块ID", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}

	if _, err := db.Exec("DELETE FROM modules WHERE id = ?", id); err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}

	core.ClearModuleIDCache()
	service.LogAudit(c, "模块管理", fmt.Sprintf("删除模块 #%d", id), true, "", 0, input)
	core.Success(c, "模块已删除", gin.H{})
}

// SyncModules POST /api/system/modules/sync
// 一键同步并校准全站 9 大分类与 34 个内置导航模块。
func (ctrl *SystemController) SyncModules(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "同步内置模块失败", gin.H{})
		return
	}

	inserted, updated, err := core.SyncBuiltinModules(db)
	if err != nil {
		core.Exception(c, err, "同步内置模块失败", gin.H{})
		return
	}

	core.ClearModuleIDCache()
	service.LogAudit(c, "模块管理", fmt.Sprintf("一键同步内置模块（新增 %d 个，校准 %d 个）", inserted, updated), true, "", 0)
	core.Success(c, fmt.Sprintf("全量内置导航模块已同步完成（新增 %d 个，校准 %d 个）", inserted, updated), gin.H{
		"inserted": inserted,
		"updated":  updated,
	})
}

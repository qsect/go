package core

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"vite-system-go/internal/config"
)

// SchemaVersion 表结构就绪标记版本号：每次表结构或初始数据变更后 +1，强制重新自检与幂等补齐。
const SchemaVersion = "9"

var (
	readyMemo  = make(map[string]bool)
	schemaMu   sync.RWMutex
	cleanKeyRe = regexp.MustCompile(`[^a-zA-Z0-9_\-]`)

	visibleColOnce sync.Once
	visibleColName = "status"
)

// PresetRole 系统预置岗位角色定义。
type PresetRole struct {
	ID          int
	Name        string
	Code        string
	Description string
}

// PresetRoles 全新库预置的岗位角色清单。
var PresetRoles = []PresetRole{
	{ID: 1, Name: "超级管理员", Code: "admin", Description: "系统超级管理员，拥有全站所有功能与数据的维护权限"},
	{ID: 2, Name: "装配班长", Code: "foreman_assembly", Description: "装配车间一线管理班长"},
	{ID: 3, Name: "调试班长", Code: "foreman_debug", Description: "调试车间一线管理班长"},
	{ID: 4, Name: "普通员工", Code: "staff", Description: "车间普通操作员工"},
	{ID: 5, Name: "实习学生", Code: "intern_student", Description: "实习学生：跟岗学习人员，模块权限需单独分配"},
}

func schemaCacheDir() string {
	root := config.Get("STORAGE_ROOT", "./storage")
	return filepath.Join(root, "schema")
}

func schemaCacheFile(key string) string {
	clean := cleanKeyRe.ReplaceAllString(key, "")
	if clean == "" {
		clean = "default"
	}
	return filepath.Join(schemaCacheDir(), clean+".ready")
}

// IsSchemaReady 判断某模块的表结构是否已在当前版本探测就绪。
func IsSchemaReady(key string) bool {
	schemaMu.RLock()
	if readyMemo[key] {
		schemaMu.RUnlock()
		return true
	}
	schemaMu.RUnlock()

	raw, err := os.ReadFile(schemaCacheFile(key))
	if err == nil && strings.TrimSpace(string(raw)) == SchemaVersion {
		schemaMu.Lock()
		readyMemo[key] = true
		schemaMu.Unlock()
		return true
	}
	return false
}

// MarkSchemaReady 写入表结构就绪标记。
func MarkSchemaReady(key string) {
	schemaMu.Lock()
	readyMemo[key] = true
	schemaMu.Unlock()

	dir := schemaCacheDir()
	_ = os.MkdirAll(dir, 0775)
	_ = os.WriteFile(schemaCacheFile(key), []byte(SchemaVersion), 0644)
}

// HasColumn 检查某张表是否存在指定列。
func HasColumn(db *sql.DB, table, column string) bool {
	var cnt int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?
	`, table, column).Scan(&cnt)
	return err == nil && cnt > 0
}

// EnsureColumn 补齐某张表缺失的列。
func EnsureColumn(db *sql.DB, table, column, definition string) {
	if HasColumn(db, table, column) {
		return
	}
	_, _ = db.Exec(fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN %s", table, definition))
}

// DropAndRecreateIfMissingColumn 若旧表缺少关键列且表中尚无业务数据（或需升级），自动重建为标准表结构。
func DropAndRecreateIfMissingColumn(db *sql.DB, table, requiredColumn string) {
	var tableCnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", table).Scan(&tableCnt); err != nil || tableCnt == 0 {
		return
	}
	if !HasColumn(db, table, requiredColumn) {
		_, _ = db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", table))
	}
}

// EnsureUniqueIndex 补齐某张表缺失的唯一索引。
func EnsureUniqueIndex(db *sql.DB, table, indexName, columnsDef string) {
	var cnt int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?
	`, table, indexName).Scan(&cnt)
	if err != nil || cnt > 0 {
		return
	}
	_, _ = db.Exec(fmt.Sprintf("ALTER TABLE `%s` ADD UNIQUE KEY `%s` (%s)", table, indexName, columnsDef))
}

// ResolveModuleVisibleColumn 探测并缓存 modules 表中表示“启用/可见”的列名。
func ResolveModuleVisibleColumn(db *sql.DB) string {
	visibleColOnce.Do(func() {
		rows, err := db.Query(`
			SELECT COLUMN_NAME FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'modules'
			  AND COLUMN_NAME IN ('status', 'is_visible')
		`)
		if err != nil {
			return
		}
		defer rows.Close()
		hasStatus := false
		hasVisible := false
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err == nil {
				if c == "status" {
					hasStatus = true
				} else if c == "is_visible" {
					hasVisible = true
				}
			}
		}
		if hasStatus {
			visibleColName = "status"
		} else if hasVisible {
			visibleColName = "is_visible"
		}
	})
	return visibleColName
}

// AutoInitBrandNewDatabase 全新空数据库或升级库一键自愈初始化。
func AutoInitBrandNewDatabase(db *sql.DB) {
	EnsureUsersTable(db)
	EnsureUserPasswordColumns(db)
	EnsurePresetRoles(db)
	EnsureModulesTable(db)
	_, _, _ = SyncBuiltinModules(db)
	EnsureAllPermTables(db)
	EnsureLoginAttemptsTable(db)
	EnsureClientVersionTable(db)
	EnsureAuditLogsTable(db)
	EnsureDiscussTables(db)
	EnsureBusinessTables(db)
	log.Printf("[INIT] 全站 9 大分类 34 个模块数据库表结构与关联数据自愈初始化完成")
}

// EnsureUsersTable 确保 users 表存在。
func EnsureUsersTable(db *sql.DB) {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id INT NOT NULL AUTO_INCREMENT,
			badge_no VARCHAR(50) DEFAULT '' COMMENT '工号/编号',
			username VARCHAR(64) NOT NULL UNIQUE COMMENT '登录账号',
			full_name VARCHAR(100) NOT NULL DEFAULT '' COMMENT '真实姓名',
			team VARCHAR(100) NOT NULL DEFAULT '管理/其他' COMMENT '所属班组',
			role_id INT DEFAULT 1 COMMENT '角色ID',
			role VARCHAR(100) DEFAULT '超级管理员' COMMENT '角色名称',
			phone VARCHAR(30) DEFAULT '' COMMENT '联系电话',
			password VARCHAR(255) NOT NULL COMMENT '密码哈希',
			is_admin TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否管理员',
			is_active TINYINT(1) NOT NULL DEFAULT 1 COMMENT '是否启用',
			can_view_all TINYINT(1) NOT NULL DEFAULT 0 COMMENT '工作日记查看全部权限',
			foreman_id INT DEFAULT NULL COMMENT '所属班长用户ID',
			must_change_password TINYINT(1) NOT NULL DEFAULT 0 COMMENT '首次登录强制改密',
			password_updated_at DATETIME DEFAULT NULL,
			last_login_at DATETIME DEFAULT NULL,
			last_login_ip VARCHAR(64) DEFAULT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			INDEX idx_team (team),
			INDEX idx_role_id (role_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户账号表';
	`)
	if err != nil {
		log.Printf("[Schema] 创建 users 表异常: %v", err)
		return
	}

	EnsureColumn(db, "users", "can_view_all", "`can_view_all` TINYINT(1) NOT NULL DEFAULT 0 COMMENT '工作日记查看全部权限'")
	EnsureColumn(db, "users", "foreman_id", "`foreman_id` INT DEFAULT NULL COMMENT '所属班长用户ID'")

	var userCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&userCount); err == nil && userCount == 0 {
		adminHash := HashPassword(DefaultPassword)
		_, insErr := db.Exec(`
			INSERT INTO users (badge_no, username, full_name, team, role_id, role, phone, password, is_admin, is_active, can_view_all, must_change_password, password_updated_at)
			VALUES ('ADMIN001', 'admin', '系统管理员', '管理/其他', 1, '超级管理员', '13800000000', ?, 1, 1, 1, 0, NOW())
		`, adminHash)
		if insErr == nil {
			log.Printf("[INIT] 已自动创建全新系统的超级管理员账号: admin / 初始密码: %s", DefaultPassword)
		}
	}
}

// EnsureUserPasswordColumns 确保 users 表具备强制改密标记与密码修改时间列。
func EnsureUserPasswordColumns(db *sql.DB) {
	if IsSchemaReady("users_password_columns") {
		return
	}
	EnsureColumn(db, "users", "must_change_password", "`must_change_password` TINYINT(1) NOT NULL DEFAULT 0")
	EnsureColumn(db, "users", "password_updated_at", "`password_updated_at` DATETIME NULL")
	MarkSchemaReady("users_password_columns")
}

// EnsurePresetRoles 确保 roles 表与系统预置岗位角色存在。
func EnsurePresetRoles(db *sql.DB) {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS roles (
			id INT NOT NULL AUTO_INCREMENT,
			name VARCHAR(50) NOT NULL,
			code VARCHAR(50) NOT NULL DEFAULT '',
			description VARCHAR(255) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='系统角色表';
	`)
	if err != nil {
		return
	}
	EnsureColumn(db, "roles", "code", "`code` VARCHAR(50) NOT NULL DEFAULT ''")
	EnsureColumn(db, "roles", "description", "`description` VARCHAR(255) NOT NULL DEFAULT ''")

	for _, pr := range PresetRoles {
		var cnt int
		_ = db.QueryRow("SELECT COUNT(*) FROM roles WHERE name = ? OR code = ?", pr.Name, pr.Code).Scan(&cnt)
		if cnt == 0 {
			_, _ = db.Exec("INSERT INTO roles (id, name, code, description) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE name=VALUES(name)",
				pr.ID, pr.Name, pr.Code, pr.Description)
		}
	}

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS role_modules (
			id INT NOT NULL AUTO_INCREMENT,
			role_id INT NOT NULL,
			module_id INT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uk_role_module (role_id, module_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='角色模块授权表';
	`)
}

// EnsureModulesTable 确保 modules 表结构完整。
func EnsureModulesTable(db *sql.DB) {
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS modules (
			id INT NOT NULL AUTO_INCREMENT,
			parent_id INT NOT NULL DEFAULT 0,
			name VARCHAR(100) NOT NULL,
			module_code VARCHAR(100) NOT NULL,
			category VARCHAR(64) NOT NULL DEFAULT '业务功能',
			route VARCHAR(150) NOT NULL DEFAULT '',
			icon VARCHAR(64) NOT NULL DEFAULT 'Menu',
			type CHAR(1) NOT NULL DEFAULT 'C',
			sort_order INT NOT NULL DEFAULT 10,
			is_visible TINYINT(1) NOT NULL DEFAULT 1,
			status TINYINT(1) NOT NULL DEFAULT 1,
			description VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (id),
			INDEX idx_module_code (module_code),
			INDEX idx_route (route)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='系统功能模块表';
	`)
	EnsureColumn(db, "modules", "parent_id", "`parent_id` INT NOT NULL DEFAULT 0")
	EnsureColumn(db, "modules", "category", "`category` VARCHAR(64) NOT NULL DEFAULT '业务功能'")
	EnsureColumn(db, "modules", "route", "`route` VARCHAR(150) NOT NULL DEFAULT ''")
	EnsureColumn(db, "modules", "icon", "`icon` VARCHAR(64) NOT NULL DEFAULT 'Menu'")
	EnsureColumn(db, "modules", "type", "`type` CHAR(1) NOT NULL DEFAULT 'C'")
	EnsureColumn(db, "modules", "sort_order", "`sort_order` INT NOT NULL DEFAULT 10")
	EnsureColumn(db, "modules", "is_visible", "`is_visible` TINYINT(1) NOT NULL DEFAULT 1")
	EnsureColumn(db, "modules", "status", "`status` TINYINT(1) NOT NULL DEFAULT 1")
	EnsureColumn(db, "modules", "description", "`description` VARCHAR(255) NOT NULL DEFAULT ''")
}

// SyncBuiltinModules 将全站 9 大导航分类、34 个标准模块同步登记到 modules 表。
func SyncBuiltinModules(db *sql.DB) (inserted int, updated int, err error) {
	EnsureModulesTable(db)
	for _, bm := range BuiltinModules() {
		cleanRoute := strings.TrimPrefix(bm.Route, "/")
		var id int
		var curRoute, curCat, curDesc string
		qErr := db.QueryRow(`
			SELECT id, COALESCE(route, ''), COALESCE(category, ''), COALESCE(description, '')
			FROM modules
			WHERE module_code = ? OR route = ? OR route = ?
			ORDER BY (module_code = ?) DESC, id ASC
			LIMIT 1
		`, bm.ModuleCode, cleanRoute, "/"+cleanRoute, bm.ModuleCode).Scan(&id, &curRoute, &curCat, &curDesc)

		if qErr == sql.ErrNoRows || id == 0 {
			_, exErr := db.Exec(`
				INSERT INTO modules (parent_id, name, module_code, category, route, icon, type, sort_order, is_visible, status, description)
				VALUES (0, ?, ?, ?, ?, ?, 'C', ?, 1, 1, ?)
			`, bm.Name, bm.ModuleCode, bm.Category, cleanRoute, bm.Icon, bm.SortOrder, bm.Description)
			if exErr != nil {
				return inserted, updated, exErr
			}
			inserted++
		} else if qErr == nil {
			newRoute := strings.TrimPrefix(curRoute, "/")
			if newRoute == "" {
				newRoute = cleanRoute
			}
			newCat := curCat
			if newCat == "" || newCat == "业务功能" {
				newCat = bm.Category
			}
			newDesc := curDesc
			if newDesc == "" {
				newDesc = bm.Description
			}
			if newRoute != curRoute || newCat != curCat || newDesc != curDesc {
				_, _ = db.Exec(`
					UPDATE modules SET route = ?, category = ?, description = ? WHERE id = ?
				`, newRoute, newCat, newDesc, id)
				updated++
			}
		}
	}
	return inserted, updated, nil
}

// EnsureAllPermTables 确保所有用户级权限表、6 类系统细分权限表及各业务模块内部权限表存在。
func EnsureAllPermTables(db *sql.DB) {
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS user_permissions (
			id INT PRIMARY KEY AUTO_INCREMENT,
			user_id INT NOT NULL,
			module_id INT NOT NULL,
			can_view TINYINT(1) DEFAULT 0,
			can_edit TINYINT(1) DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_user_module (user_id, module_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS trace_permissions (
			id INT PRIMARY KEY AUTO_INCREMENT,
			user_id INT NOT NULL,
			can_add_material TINYINT(1) DEFAULT 0,
			can_scan TINYINT(1) DEFAULT 1,
			can_view TINYINT(1) DEFAULT 1,
			can_edit_record TINYINT(1) DEFAULT 0,
			can_delete_record TINYINT(1) DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_trace_user (user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS barcode_validator_permissions (
			id INT PRIMARY KEY AUTO_INCREMENT,
			user_id INT NOT NULL,
			can_save_settings TINYINT(1) DEFAULT 1,
			can_delete TINYINT(1) DEFAULT 0,
			can_edit TINYINT(1) DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_barcode_user (user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS discuss_permissions (
			id INT PRIMARY KEY AUTO_INCREMENT,
			user_id INT NOT NULL,
			can_view TINYINT(1) DEFAULT 1,
			can_create_topic TINYINT(1) DEFAULT 1,
			can_reply TINYINT(1) DEFAULT 1,
			can_edit TINYINT(1) DEFAULT 1,
			can_delete TINYINT(1) DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_discuss_user (user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS first_piece_permissions (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL UNIQUE,
			can_manage_criteria TINYINT(1) NOT NULL DEFAULT 0,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS mac_permissions (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL UNIQUE,
			can_apply TINYINT(1) NOT NULL DEFAULT 1,
			can_config_ports TINYINT(1) NOT NULL DEFAULT 0,
			can_import TINYINT(1) NOT NULL DEFAULT 0,
			can_view_records TINYINT(1) NOT NULL DEFAULT 1,
			can_view_stats TINYINT(1) NOT NULL DEFAULT 1,
			can_view_pool TINYINT(1) NOT NULL DEFAULT 0,
			can_view_logs TINYINT(1) NOT NULL DEFAULT 0,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS osm_permissions (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL UNIQUE,
			can_edit TINYINT(1) NOT NULL DEFAULT 0,
			can_delete TINYINT(1) NOT NULL DEFAULT 0,
			can_upload TINYINT(1) NOT NULL DEFAULT 0,
			can_add_return TINYINT(1) NOT NULL DEFAULT 0,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS module15_permissions (
			user_id INT PRIMARY KEY,
			can_view_records TINYINT(1) DEFAULT 1,
			can_input TINYINT(1) DEFAULT 1
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS quality_permissions (
			user_id INT PRIMARY KEY,
			can_view_records TINYINT(1) DEFAULT 1,
			can_input TINYINT(1) DEFAULT 1
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS impedance_permissions (
			user_id INT PRIMARY KEY,
			can_view_records TINYINT(1) DEFAULT 1,
			can_input TINYINT(1) DEFAULT 1,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS doc_error_permissions (
			user_id INT PRIMARY KEY,
			can_input TINYINT(1) DEFAULT 1,
			can_edit TINYINT(1) DEFAULT 0,
			can_delete TINYINT(1) DEFAULT 0,
			can_export TINYINT(1) DEFAULT 1,
			can_manage TINYINT(1) DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS asset_user_permissions (
			user_id INT PRIMARY KEY,
			allowed_groups TEXT NULL,
			allowed_users TEXT NULL,
			can_edit TINYINT(1) DEFAULT 1,
			can_delete TINYINT(1) DEFAULT 0,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS category_permissions (
			id INT AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL,
			category_id INT NOT NULL,
			can_view TINYINT(1) DEFAULT 1,
			can_edit TINYINT(1) DEFAULT 0,
			updated_at DATETIME DEFAULT NULL,
			UNIQUE KEY uk_user_cat (user_id, category_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
}

// EnsureLoginAttemptsTable 确保登录防爆破限速表存在。
func EnsureLoginAttemptsTable(db *sql.DB) {
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS login_attempts (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			username VARCHAR(64) NOT NULL DEFAULT '',
			ip VARCHAR(64) NOT NULL DEFAULT '',
			success TINYINT(1) NOT NULL DEFAULT 0,
			attempted_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_username_time (username, attempted_at),
			INDEX idx_ip_time (ip, attempted_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
}

// EnsureClientVersionTable 确保客户端版本发布管理表存在。
func EnsureClientVersionTable(db *sql.DB) {
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS client_versions (
			id INT AUTO_INCREMENT PRIMARY KEY,
			client_key VARCHAR(64) NOT NULL DEFAULT 'impedance_recorder',
			version VARCHAR(32) NOT NULL,
			min_version VARCHAR(32) NOT NULL DEFAULT '',
			force_update TINYINT(1) NOT NULL DEFAULT 0,
			download_url VARCHAR(500) NOT NULL DEFAULT '',
			update_log TEXT NULL,
			is_published TINYINT(1) NOT NULL DEFAULT 1,
			created_by VARCHAR(100) NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_client_version (client_key, is_published, id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
}

// EnsureAuditLogsTable 确保安全审计日志表存在。
func EnsureAuditLogsTable(db *sql.DB) {
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_logs (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL DEFAULT 0,
			username VARCHAR(64) NOT NULL DEFAULT 'GUEST',
			full_name VARCHAR(100) NOT NULL DEFAULT '访客',
			module VARCHAR(64) NOT NULL DEFAULT '',
			action VARCHAR(100) NOT NULL DEFAULT '',
			method VARCHAR(16) NOT NULL DEFAULT 'GET',
			url VARCHAR(255) NOT NULL DEFAULT '',
			ip VARCHAR(64) NOT NULL DEFAULT '',
			user_agent VARCHAR(255) NOT NULL DEFAULT '',
			params TEXT NULL,
			status TINYINT(1) NOT NULL DEFAULT 1,
			error_msg TEXT NULL,
			execution_time INT NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_user_id (user_id),
			INDEX idx_created_at (created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
}

// EnsureDiscussTables 确保讨论区分类、主题、回复、通知与公告已读表存在。
func EnsureDiscussTables(db *sql.DB) {
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS discuss_categories (
			id INT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(64) NOT NULL,
			slug VARCHAR(64) NOT NULL DEFAULT '',
			sort_order INT NOT NULL DEFAULT 10,
			status TINYINT(1) NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	var catCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM discuss_categories").Scan(&catCount)
	if catCount < 4 {
		_, _ = db.Exec(`
			INSERT IGNORE INTO discuss_categories (id, name, slug, sort_order, status) VALUES
			(1, '系统公告', 'notice', 1, 1),
			(2, '经验分享', 'share', 2, 1),
			(3, '问题求助', 'help', 3, 1),
			(4, '灌水闲聊', 'chat', 4, 1)
		`)
	}

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS discuss_topics (
			id INT AUTO_INCREMENT PRIMARY KEY,
			category_id INT NOT NULL DEFAULT 1,
			user_id INT NOT NULL DEFAULT 1,
			title VARCHAR(200) NOT NULL,
			content MEDIUMTEXT NULL,
			views INT NOT NULL DEFAULT 0,
			view_count INT NOT NULL DEFAULT 0,
			replies_count INT NOT NULL DEFAULT 0,
			reply_count INT NOT NULL DEFAULT 0,
			is_pinned TINYINT(1) NOT NULL DEFAULT 0,
			is_closed TINYINT(1) NOT NULL DEFAULT 0,
			last_reply_by INT DEFAULT NULL,
			last_reply_user_id INT DEFAULT NULL,
			last_reply_at DATETIME DEFAULT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_cat_time (category_id, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	EnsureColumn(db, "discuss_topics", "views", "`views` INT NOT NULL DEFAULT 0")
	EnsureColumn(db, "discuss_topics", "replies_count", "`replies_count` INT NOT NULL DEFAULT 0")
	EnsureColumn(db, "discuss_topics", "last_reply_by", "`last_reply_by` INT DEFAULT NULL")

	var topicCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM discuss_topics").Scan(&topicCount)
	if topicCount == 0 {
		_, _ = db.Exec(`
			INSERT INTO discuss_topics (category_id, user_id, title, content, is_pinned, last_reply_at, created_at)
			VALUES (1, 1, '欢迎使用装置车间综合协作平台（Go 架构全量业务版）', '<p>系统已成功部署并运行于全新的 Go 后端架构！全站 9 大业务分类、34 个功能模块及跨模块数据关联均已就绪，祝使用愉快！</p>', 1, NOW(), NOW())
		`)
	}

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS discuss_replies (
			id INT AUTO_INCREMENT PRIMARY KEY,
			topic_id INT NOT NULL,
			user_id INT NOT NULL,
			reply_to_id INT NOT NULL DEFAULT 0,
			content TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_topic (topic_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS discuss_notifications (
			id INT AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL,
			sender_id INT NOT NULL DEFAULT 0,
			from_user_id INT NOT NULL DEFAULT 0,
			type VARCHAR(20) NOT NULL DEFAULT 'reply',
			topic_id INT NOT NULL,
			reply_id INT NOT NULL DEFAULT 0,
			message VARCHAR(255) NOT NULL DEFAULT '',
			is_read TINYINT(1) NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_user_read (user_id, is_read)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	EnsureColumn(db, "discuss_notifications", "from_user_id", "`from_user_id` INT NOT NULL DEFAULT 0")
	EnsureColumn(db, "discuss_notifications", "message", "`message` VARCHAR(255) NOT NULL DEFAULT ''")

	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS notice_reads (
			id INT PRIMARY KEY AUTO_INCREMENT,
			user_id INT NOT NULL,
			topic_id INT NOT NULL,
			read_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_user_topic (user_id, topic_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	EnsureColumn(db, "notice_reads", "read_at", "`read_at` DATETIME DEFAULT CURRENT_TIMESTAMP")
}

// EnsureBusinessTables 初始化全站 28 个业务模块的所有数据表（严格对齐原 PHP 表名与列名，并自动平滑升级旧表）。
func EnsureBusinessTables(db *sql.DB) {
	// 1. 周完成统计 (WeeklyController -> production_progress)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS production_progress (
			id INT AUTO_INCREMENT PRIMARY KEY,
			week_number VARCHAR(20) NOT NULL,
			process VARCHAR(50) NOT NULL,
			worker_count DECIMAL(12,2) NOT NULL DEFAULT 0,
			hours_used DECIMAL(12,2) NOT NULL DEFAULT 0,
			standard_device DECIMAL(12,2) NOT NULL DEFAULT 0,
			weekly_output_per_person DECIMAL(12,2) DEFAULT 0,
			hourly_output DECIMAL(12,2) DEFAULT 0,
			remarks TEXT NULL,
			created_by VARCHAR(64) NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			KEY idx_week_number (week_number),
			KEY idx_process (process)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 2. 班组效率 (OeeController -> oee_records)
	DropAndRecreateIfMissingColumn(db, "oee_records", "stat_month")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS oee_records (
			id INT AUTO_INCREMENT PRIMARY KEY,
			team VARCHAR(50) NOT NULL,
			stat_month VARCHAR(7) NOT NULL COMMENT '统计月份(YYYY-MM)',
			total_output DECIMAL(12,2) NOT NULL DEFAULT 0,
			outsourced_output DECIMAL(12,2) NOT NULL DEFAULT 0,
			actual_output DECIMAL(12,2) NOT NULL DEFAULT 0,
			actual_time DECIMAL(12,2) NOT NULL DEFAULT 0,
			efficiency DECIMAL(12,2) NOT NULL DEFAULT 0,
			remarks TEXT NULL,
			created_by INT NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_team (team),
			INDEX idx_stat_month (stat_month)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 3. 包装配送与齐套 (DispatchKittingController)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS tab_delivery_receipt (
			id INT AUTO_INCREMENT PRIMARY KEY,
			peisong_order VARCHAR(64) NOT NULL,
			wo VARCHAR(64) NOT NULL,
			mater_no VARCHAR(64) NOT NULL,
			mater_desc VARCHAR(255) DEFAULT '',
			send_qty DECIMAL(18,4) NOT NULL DEFAULT 0,
			confirm_user VARCHAR(64) DEFAULT '',
			confirm_time DATETIME DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_wo (wo),
			INDEX idx_ps (peisong_order)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS tab_delivery_batch (
			id INT AUTO_INCREMENT PRIMARY KEY,
			peisong_order VARCHAR(64) NOT NULL UNIQUE,
			wo VARCHAR(64) NOT NULL,
			status TINYINT NOT NULL DEFAULT 1,
			confirm_user VARCHAR(64) DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS tab_work_order_material_archive (
			wo VARCHAR(64) PRIMARY KEY,
			is_finished TINYINT NOT NULL DEFAULT 1,
			finished_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			remark VARCHAR(255) DEFAULT ''
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS tab_wo_packing_progress (
			wo VARCHAR(64) PRIMARY KEY,
			done_qty INT NOT NULL DEFAULT 0,
			cached_at DATETIME DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 4. 标签打印管理 (LabelPrintController -> print_records)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS print_records (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			order_number VARCHAR(100) NOT NULL,
			template_code VARCHAR(100) NOT NULL DEFAULT '',
			serial_number VARCHAR(100) NOT NULL DEFAULT '',
			template_path TEXT NULL,
			quantity INT DEFAULT 1,
			print_data TEXT NULL,
			printed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			printed_by VARCHAR(100) NOT NULL DEFAULT '',
			INDEX idx_order_number (order_number),
			INDEX idx_printed_at (printed_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 5. 设备矩阵总览 (EquipmentController -> equipment & equipment_processes)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS equipment (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			code VARCHAR(50) NOT NULL UNIQUE,
			name VARCHAR(100) NOT NULL,
			product_series VARCHAR(50) NOT NULL,
			description VARCHAR(255) DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS equipment_processes (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			equipment_id INT NOT NULL,
			process_code VARCHAR(100) NOT NULL,
			efficiency DECIMAL(6,2) DEFAULT NULL,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			UNIQUE KEY uk_equip_proc (equipment_id, process_code)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 6. MAC 地址管理 (MacController)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS mac_categories (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(50) NOT NULL,
			type VARCHAR(20) NOT NULL DEFAULT 'hex',
			format_pattern VARCHAR(100) DEFAULT NULL,
			total_count INT DEFAULT 0,
			used_count INT DEFAULT 0,
			current_start VARCHAR(100) DEFAULT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	var macCatCnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM mac_categories").Scan(&macCatCnt); err == nil && macCatCnt == 0 {
		_, _ = db.Exec(`
			INSERT INTO mac_categories (id, name, type, format_pattern, total_count, used_count) VALUES
			(1, '保护类', 'decimal', '000-014-234-000-000-000', 0, 0),
			(2, '测控类', 'hex', '00-0E-EA-00-00-00', 0, 0),
			(3, 'SL200/DSA', 'decimal', '000-014-234-000-000-000', 0, 0),
			(4, '交换机类', 'switcher', '00-0E-EA-00-00-00', 0, 0)
		`)
	}
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS mac_address_pool (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			category_id INT NOT NULL DEFAULT 1,
			mac_address VARCHAR(100) NOT NULL UNIQUE,
			status TINYINT(1) DEFAULT 0,
			used_by_work_order VARCHAR(100) DEFAULT NULL,
			used_at DATETIME DEFAULT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_cat_status (category_id, status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS mac_application_records (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			work_order VARCHAR(100) NOT NULL,
			category_id INT NOT NULL DEFAULT 1,
			category_name VARCHAR(50) NOT NULL DEFAULT '',
			order_quantity INT NOT NULL DEFAULT 1,
			port_count INT NOT NULL DEFAULT 1,
			total_mac_count INT NOT NULL DEFAULT 1,
			start_mac VARCHAR(100) NOT NULL,
			end_mac VARCHAR(100) NOT NULL,
			applicant_id INT NOT NULL DEFAULT 0,
			applicant_name VARCHAR(100) NOT NULL DEFAULT '',
			is_reapplied TINYINT(1) DEFAULT 0,
			reapply_reason TEXT NULL,
			task_code VARCHAR(100) DEFAULT NULL,
			description TEXT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_work_order (work_order)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS mac_application_details (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			record_id INT NOT NULL,
			work_order VARCHAR(100) NOT NULL,
			mac_address VARCHAR(100) NOT NULL,
			sequence_no INT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_record_id (record_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	DropAndRecreateIfMissingColumn(db, "mac_import_logs", "category_id")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS mac_import_logs (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			category_id INT NOT NULL DEFAULT 1,
			category_name VARCHAR(50) NOT NULL DEFAULT '',
			start_mac VARCHAR(100) NOT NULL,
			end_mac VARCHAR(100) DEFAULT NULL,
			quantity INT NOT NULL DEFAULT 0,
			imported_by INT NOT NULL DEFAULT 0,
			imported_by_name VARCHAR(100) DEFAULT NULL,
			imported_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			remarks TEXT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS product_port_configs (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			product_code VARCHAR(100) NOT NULL UNIQUE,
			description TEXT NULL,
			port_count INT NOT NULL DEFAULT 1,
			category_id INT NOT NULL DEFAULT 1,
			updated_by VARCHAR(100) DEFAULT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	EnsureColumn(db, "product_port_configs", "category_id", "`category_id` INT NOT NULL DEFAULT 1")

	// 7. 质量追溯 (TraceController)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS trace_tasks (
			id INT AUTO_INCREMENT PRIMARY KEY,
			task_order VARCHAR(100) NOT NULL UNIQUE,
			parent_code VARCHAR(100) NOT NULL,
			description VARCHAR(255) NOT NULL DEFAULT '',
			quantity INT NOT NULL DEFAULT 1,
			batch_prefix VARCHAR(60) NOT NULL DEFAULT '',
			batch_no VARCHAR(100) NOT NULL DEFAULT '',
			scan_points TINYINT(2) NOT NULL DEFAULT 1,
			order_type VARCHAR(20) NOT NULL DEFAULT 'standard',
			tasksid VARCHAR(50) NOT NULL DEFAULT '',
			rel_no VARCHAR(100) NOT NULL DEFAULT '',
			cem_pushed TINYINT(1) NOT NULL DEFAULT 0,
			cem_pushed_at DATETIME DEFAULT NULL,
			cem_push_result VARCHAR(255) NOT NULL DEFAULT '',
			created_by INT NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_task_order (task_order)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	EnsureColumn(db, "trace_tasks", "order_type", "`order_type` VARCHAR(20) NOT NULL DEFAULT 'standard'")
	EnsureColumn(db, "trace_tasks", "tasksid", "`tasksid` VARCHAR(50) NOT NULL DEFAULT ''")
	EnsureColumn(db, "trace_tasks", "rel_no", "`rel_no` VARCHAR(100) NOT NULL DEFAULT ''")
	EnsureColumn(db, "trace_tasks", "batch_no", "`batch_no` VARCHAR(100) NOT NULL DEFAULT ''")
	EnsureColumn(db, "trace_tasks", "cem_pushed", "`cem_pushed` TINYINT(1) NOT NULL DEFAULT 0")
	EnsureColumn(db, "trace_tasks", "cem_pushed_at", "`cem_pushed_at` DATETIME DEFAULT NULL")
	EnsureColumn(db, "trace_tasks", "cem_push_result", "`cem_push_result` VARCHAR(255) NOT NULL DEFAULT ''")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS trace_bom_config (
			id INT AUTO_INCREMENT PRIMARY KEY,
			task_order VARCHAR(100) NOT NULL,
			parent_code VARCHAR(100) NOT NULL,
			parent_name VARCHAR(255) NOT NULL DEFAULT '',
			child_code VARCHAR(50) NOT NULL,
			child_name VARCHAR(255) NOT NULL DEFAULT '',
			unit_quantity DECIMAL(10,2) NOT NULL DEFAULT 1.00,
			scan_point TINYINT(2) NOT NULL DEFAULT 1,
			created_by INT NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_task_point (task_order, scan_point)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS trace_boxes (
			id INT AUTO_INCREMENT PRIMARY KEY,
			task_order VARCHAR(100) NOT NULL,
			box_sn VARCHAR(60) NOT NULL UNIQUE,
			parent_code VARCHAR(100) NOT NULL,
			status TINYINT(1) NOT NULL DEFAULT 0,
			created_by INT NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME DEFAULT NULL,
			INDEX idx_task_status (task_order, status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS trace_scans (
			id INT AUTO_INCREMENT PRIMARY KEY,
			task_order VARCHAR(100) NOT NULL,
			box_id INT NOT NULL,
			scan_point TINYINT(2) NOT NULL DEFAULT 1,
			child_code VARCHAR(50) NOT NULL,
			child_sn VARCHAR(60) NOT NULL UNIQUE,
			scanned_by INT NOT NULL DEFAULT 0,
			scanned_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_box_child (box_id, child_code)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 8. 条码验证比对 (BarcodeValidatorController)
	DropAndRecreateIfMissingColumn(db, "barcode_reference", "row_index")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS barcode_reference (
			id INT PRIMARY KEY AUTO_INCREMENT,
			work_order VARCHAR(50) NOT NULL,
			row_index INT NOT NULL,
			mod1 VARCHAR(100) DEFAULT '',
			mod2 VARCHAR(100) DEFAULT '',
			mod3 VARCHAR(100) DEFAULT '',
			mod4 VARCHAR(100) DEFAULT '',
			mod5 VARCHAR(100) DEFAULT '',
			mask_barcode VARCHAR(100) DEFAULT '',
			mask_qr VARCHAR(100) DEFAULT '',
			batch_prefix VARCHAR(16) DEFAULT '',
			upload_time DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_work_row (work_order, row_index)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS barcode_completed_rows (
			id INT PRIMARY KEY AUTO_INCREMENT,
			work_order VARCHAR(50) NOT NULL,
			row_index INT NOT NULL,
			operator VARCHAR(50) NOT NULL,
			sn VARCHAR(50) NOT NULL,
			mod1 VARCHAR(100) DEFAULT '',
			mod2 VARCHAR(100) DEFAULT '',
			mod3 VARCHAR(100) DEFAULT '',
			mod4 VARCHAR(100) DEFAULT '',
			mod5 VARCHAR(100) DEFAULT '',
			mask_barcode VARCHAR(100) DEFAULT '',
			mask_qr VARCHAR(100) DEFAULT '',
			completed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_wo_row (work_order, row_index),
			INDEX idx_sn (sn)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS barcode_user_settings (
			id INT PRIMARY KEY AUTO_INCREMENT,
			user_id INT NOT NULL DEFAULT 0,
			work_order VARCHAR(50) NOT NULL,
			enabled_fields VARCHAR(255) DEFAULT '',
			order_fields VARCHAR(255) DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_work_order (work_order)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 9. 首件检管理 (FirstPieceController -> module16_records & module16_criteria)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS module16_records (
			id INT AUTO_INCREMENT PRIMARY KEY,
			work_order VARCHAR(100) NOT NULL,
			task_code VARCHAR(100) DEFAULT NULL,
			description TEXT NULL,
			quantity VARCHAR(50) DEFAULT NULL,
			batch_no VARCHAR(50) DEFAULT NULL,
			barcode TEXT NULL,
			qrcode TEXT NULL,
			need_check TINYINT(1) DEFAULT 0,
			check_result VARCHAR(20) DEFAULT '合格',
			created_by INT NOT NULL DEFAULT 0,
			created_by_name VARCHAR(100) NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			is_deleted TINYINT(1) DEFAULT 0,
			INDEX idx_work_order (work_order),
			INDEX idx_is_deleted (is_deleted)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS module16_criteria (
			id INT AUTO_INCREMENT PRIMARY KEY,
			work_order VARCHAR(100) NOT NULL UNIQUE,
			barcode_expected VARCHAR(255) DEFAULT NULL,
			qrcode_expected TEXT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 10. 阻抗测试记录 (ImpedanceController -> impedance_records)
	DropAndRecreateIfMissingColumn(db, "impedance_records", "protect_impedance1")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS impedance_records (
			id INT AUTO_INCREMENT PRIMARY KEY,
			sn VARCHAR(22) NOT NULL,
			work_order VARCHAR(100) NOT NULL,
			code VARCHAR(100) DEFAULT '',
			description TEXT NULL,
			protect_impedance1 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance2 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance3 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance4 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance5 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance6 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance7 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance8 DECIMAL(15,4) DEFAULT NULL,
			protect_impedance9 DECIMAL(15,4) DEFAULT NULL,
			control_impedance1 DECIMAL(15,4) DEFAULT NULL,
			control_impedance2 DECIMAL(15,4) DEFAULT NULL,
			control_impedance3 DECIMAL(15,4) DEFAULT NULL,
			protect_error DECIMAL(10,4) DEFAULT NULL,
			control_error DECIMAL(10,4) DEFAULT NULL,
			protect_conclusion VARCHAR(20) DEFAULT '未填写',
			control_conclusion VARCHAR(20) DEFAULT '未填写',
			operator VARCHAR(100) NOT NULL DEFAULT '',
			error_standard INT DEFAULT 10,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_sn (sn),
			INDEX idx_work_order (work_order)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 11. 质量问题记录 (QualityController -> quality_issues)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS quality_issues (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			batch_no VARCHAR(50) NOT NULL DEFAULT '',
			work_order VARCHAR(100) DEFAULT NULL,
			task_code VARCHAR(100) DEFAULT NULL,
			description TEXT NULL,
			sn VARCHAR(50) NOT NULL,
			process VARCHAR(50) DEFAULT NULL,
			issue_description TEXT NOT NULL,
			image_path VARCHAR(255) DEFAULT NULL,
			outsourced_unit VARCHAR(255) DEFAULT NULL,
			created_by INT NOT NULL DEFAULT 0,
			created_by_name VARCHAR(100) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT NULL,
			is_deleted TINYINT(1) DEFAULT 0,
			KEY idx_sn (sn),
			KEY idx_is_deleted (is_deleted)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 12. 问题排查记录 (TroubleshootController -> troubleshoot_records)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS troubleshoot_records (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			work_order VARCHAR(100) DEFAULT NULL,
			task_code VARCHAR(100) DEFAULT NULL,
			description TEXT NULL,
			sn VARCHAR(50) NOT NULL,
			location VARCHAR(64) DEFAULT '',
			issue_description TEXT NOT NULL,
			conclusion VARCHAR(32) DEFAULT '合格',
			defect_phenomenon TEXT NULL,
			operator_id INT NOT NULL DEFAULT 0,
			operator_name VARCHAR(100) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT NULL,
			is_deleted TINYINT(1) DEFAULT 0,
			KEY idx_sn (sn),
			KEY idx_is_deleted (is_deleted)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	EnsureColumn(db, "troubleshoot_records", "location", "`location` VARCHAR(64) DEFAULT ''")
	EnsureColumn(db, "troubleshoot_records", "conclusion", "`conclusion` VARCHAR(32) DEFAULT '合格'")
	EnsureColumn(db, "troubleshoot_records", "defect_phenomenon", "`defect_phenomenon` TEXT NULL")

	// 13. 北边返厂统计 (BeibianController -> module_records)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS module_records (
			id INT AUTO_INCREMENT PRIMARY KEY,
			work_order VARCHAR(50) NOT NULL,
			description TEXT NOT NULL,
			sn VARCHAR(22) NOT NULL,
			psn VARCHAR(13) NOT NULL,
			return_time DATE NOT NULL,
			is_returned VARCHAR(10) DEFAULT '否',
			actual_return_date DATE NULL,
			operator VARCHAR(100) NOT NULL DEFAULT '',
			remarks TEXT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 14. 文件错误记录 (DocErrorController -> doc_error_records)
	DropAndRecreateIfMissingColumn(db, "doc_error_records", "file_error_desc")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS doc_error_records (
			id INT AUTO_INCREMENT PRIMARY KEY,
			work_order VARCHAR(64) NOT NULL,
			task_code VARCHAR(128) DEFAULT '',
			description TEXT NULL,
			file_error_desc TEXT NOT NULL,
			discovery_process VARCHAR(50) NOT NULL DEFAULT '装置装配',
			discovery_time DATETIME DEFAULT CURRENT_TIMESTAMP,
			discoverer_id INT DEFAULT NULL,
			discoverer_name VARCHAR(64) NOT NULL DEFAULT '',
			is_modified TINYINT(1) NOT NULL DEFAULT 0,
			modify_time DATETIME DEFAULT NULL,
			modifier_id INT DEFAULT NULL,
			modifier_name VARCHAR(64) DEFAULT '',
			remark TEXT NULL,
			is_deleted TINYINT(1) NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			KEY idx_work_order (work_order),
			KEY idx_is_deleted (is_deleted)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 15. 作业指导文件 (ZywjController -> zywj_documents)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS zywj_documents (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			doc_no VARCHAR(100) NOT NULL DEFAULT '',
			title VARCHAR(200) NOT NULL,
			category VARCHAR(50) NOT NULL DEFAULT '装配作业指导',
			product_code VARCHAR(100) DEFAULT '',
			process_name VARCHAR(100) DEFAULT '',
			version VARCHAR(20) NOT NULL DEFAULT 'V1.0',
			file_path VARCHAR(255) NOT NULL,
			file_size INT NOT NULL DEFAULT 0,
			file_type VARCHAR(20) NOT NULL DEFAULT 'pdf',
			remarks TEXT NULL,
			uploader_id INT NOT NULL DEFAULT 0,
			uploader_name VARCHAR(100) NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_product_code (product_code),
			INDEX idx_category (category)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 16. 外协管理系统 (OsmController -> osm_records & osm_returns)
	DropAndRecreateIfMissingColumn(db, "osm_records", "returned_quantity")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS osm_records (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			work_order VARCHAR(50) NOT NULL,
			product_code VARCHAR(50) DEFAULT '',
			description TEXT NULL,
			product_desc VARCHAR(255) DEFAULT '',
			batch_no VARCHAR(100) DEFAULT '',
			vendor VARCHAR(100) NOT NULL DEFAULT '',
			quantity INT NOT NULL DEFAULT 1,
			order_qty INT NOT NULL DEFAULT 1,
			returned_quantity INT NOT NULL DEFAULT 0,
			status VARCHAR(20) NOT NULL DEFAULT '未返回',
			send_date DATE NOT NULL,
			required_return_date DATE DEFAULT NULL,
			remarks TEXT NULL,
			created_by INT NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_work_order (work_order),
			INDEX idx_vendor (vendor)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	DropAndRecreateIfMissingColumn(db, "osm_returns", "return_quantity")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS osm_returns (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			osm_id INT NOT NULL,
			return_quantity INT NOT NULL DEFAULT 1,
			return_date DATE NOT NULL,
			remarks TEXT NULL,
			created_by INT NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_osm_id (osm_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 17. 资产台账管理 (AssetsController -> asset_categories & assets)
	DropAndRecreateIfMissingColumn(db, "asset_categories", "show_fields")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS asset_categories (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(50) NOT NULL UNIQUE,
			show_fields TEXT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	var assetCatCnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM asset_categories").Scan(&assetCatCnt); err == nil && assetCatCnt == 0 {
		defaultAssetCats := []string{"计算机", "仪器仪表", "工装夹具", "办公设备", "其他资产"}
		for _, cn := range defaultAssetCats {
			_, _ = db.Exec("INSERT IGNORE INTO asset_categories (name, show_fields) VALUES (?, '{}')", cn)
		}
	}
	DropAndRecreateIfMissingColumn(db, "assets", "category")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS assets (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			category VARCHAR(50) NOT NULL DEFAULT '计算机',
			name VARCHAR(100) NOT NULL DEFAULT '',
			code VARCHAR(50) NOT NULL DEFAULT '',
			model VARCHAR(100) DEFAULT '',
			config TEXT NULL,
			user VARCHAR(50) NOT NULL DEFAULT '',
			location VARCHAR(100) NOT NULL DEFAULT '',
			purpose VARCHAR(100) DEFAULT '',
			mac1 VARCHAR(30) DEFAULT '',
			ip1 VARCHAR(30) DEFAULT '',
			mac2 VARCHAR(30) DEFAULT '',
			ip2 VARCHAR(30) DEFAULT '',
			status VARCHAR(50) DEFAULT '在用',
			image_path VARCHAR(255) DEFAULT '',
			out_date VARCHAR(30) DEFAULT '',
			price DECIMAL(10,2) DEFAULT 0.00,
			remark TEXT NULL,
			created_by VARCHAR(100) DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_cat (category),
			INDEX idx_user (user)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 18. 老化车维修管理 (AgingCartController)
	DropAndRecreateIfMissingColumn(db, "aging_settings", "cart_total")
	DropAndRecreateIfMissingColumn(db, "aging_carts", "usage_type")
	DropAndRecreateIfMissingColumn(db, "aging_repairs", "reported_at")
	DropAndRecreateIfMissingColumn(db, "aging_repair_workers", "repair_id")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS aging_settings (
			id TINYINT NOT NULL PRIMARY KEY,
			cart_total SMALLINT UNSIGNED NOT NULL DEFAULT 140,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec("INSERT IGNORE INTO aging_settings (id, cart_total) VALUES (1, 140)")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS aging_carts (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			cart_no VARCHAR(10) NOT NULL UNIQUE,
			status VARCHAR(20) NOT NULL DEFAULT 'normal',
			layers TINYINT UNSIGNED NULL DEFAULT NULL,
			usage_type VARCHAR(20) NOT NULL DEFAULT '流动使用',
			location VARCHAR(64) NOT NULL DEFAULT '',
			remark VARCHAR(255) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	var cartCnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM aging_carts").Scan(&cartCnt); err == nil && cartCnt == 0 {
		var total int
		if err := db.QueryRow("SELECT cart_total FROM aging_settings WHERE id = 1").Scan(&total); err != nil || total <= 0 {
			total = 140
		}
		for i := 1; i <= total; i++ {
			_, _ = db.Exec("INSERT IGNORE INTO aging_carts (cart_no, status, usage_type) VALUES (?, 'normal', '流动使用')", fmt.Sprintf("%03d", i))
		}
	}
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS aging_repairs (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			cart_no VARCHAR(10) NOT NULL,
			fault_type VARCHAR(50) NOT NULL DEFAULT '',
			fault_desc VARCHAR(500) NOT NULL DEFAULT '',
			status VARCHAR(20) NOT NULL DEFAULT 'pending',
			reporter_id INT NOT NULL DEFAULT 0,
			reporter_name VARCHAR(64) NOT NULL DEFAULT '',
			reported_at DATETIME NOT NULL,
			repairer_id INT NOT NULL DEFAULT 0,
			repairer_name VARCHAR(64) NOT NULL DEFAULT '',
			repair_started_at DATETIME NULL,
			repair_finished_at DATETIME NULL,
			raw_minutes INT NOT NULL DEFAULT 0,
			duration_minutes INT NOT NULL DEFAULT 0,
			repair_remark VARCHAR(500) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS aging_repair_workers (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			repair_id INT NOT NULL,
			user_id INT NOT NULL DEFAULT 0,
			user_name VARCHAR(64) NOT NULL DEFAULT '',
			is_starter TINYINT(1) NOT NULL DEFAULT 0,
			joined_at DATETIME NOT NULL,
			left_at DATETIME NULL,
			raw_minutes INT NOT NULL DEFAULT 0,
			duration_minutes INT NOT NULL DEFAULT 0,
			status VARCHAR(20) NOT NULL DEFAULT 'working',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_repair_user_join (repair_id, user_id, joined_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 19. 人员离岗监控 (AttendanceController -> att_employees, att_cards, att_offjob_records)
	DropAndRecreateIfMissingColumn(db, "att_employees", "foreman_id")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS att_employees (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(50) NOT NULL,
			employee_no VARCHAR(50) NOT NULL UNIQUE,
			team VARCHAR(50) NOT NULL DEFAULT '装置装配',
			foreman_id INT DEFAULT NULL,
			status VARCHAR(20) NOT NULL DEFAULT 'onjob',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	DropAndRecreateIfMissingColumn(db, "att_cards", "card_uid")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS att_cards (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			card_uid VARCHAR(32) NOT NULL UNIQUE,
			current_employee_id INT DEFAULT NULL,
			status VARCHAR(20) NOT NULL DEFAULT 'free',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	var cardCnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM att_cards").Scan(&cardCnt); err == nil && cardCnt == 0 {
		for i := 1; i <= 10; i++ {
			_, _ = db.Exec("INSERT IGNORE INTO att_cards (card_uid, status) VALUES (?, 'free')", fmt.Sprintf("CARD-%02d", i))
		}
	}
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS att_offjob_records (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			card_id INT DEFAULT NULL,
			employee_id INT NOT NULL,
			start_time DATETIME NOT NULL,
			end_time DATETIME DEFAULT NULL,
			duration_seconds INT DEFAULT NULL,
			INDEX idx_record_employee (employee_id),
			INDEX idx_record_start (start_time)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 20. 工作日记管理 (DiaryController -> work_diaries)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS work_diaries (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			work_date DATE NOT NULL,
			task_order VARCHAR(50) DEFAULT NULL,
			product_code VARCHAR(50) DEFAULT NULL,
			description TEXT NULL,
			task_type VARCHAR(50) DEFAULT NULL,
			work_content VARCHAR(100) NOT NULL,
			quantity INT DEFAULT 0,
			duration INT NOT NULL DEFAULT 60,
			operator VARCHAR(100) NOT NULL DEFAULT '',
			is_ghost_write VARCHAR(10) DEFAULT '否',
			remarks TEXT NULL,
			created_by INT NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_work_date (work_date),
			INDEX idx_task_order (task_order)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 21. 工时汇总系统 (ManhourController -> manhour_categories, category_columns, manhour_summary)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS manhour_categories (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(100) NOT NULL,
			slug VARCHAR(100) NOT NULL UNIQUE,
			description VARCHAR(255) DEFAULT NULL,
			is_active TINYINT(1) NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	var mhCatCnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM manhour_categories").Scan(&mhCatCnt); err == nil && mhCatCnt == 0 {
		_, _ = db.Exec(`
			INSERT INTO manhour_categories (id, name, slug, description) VALUES
			(1, '完整工时', 'full-hours', '全流程工时汇总'),
			(2, '装置装配', 'device-assembly', '装配车间工序工时'),
			(3, '装置调试', 'device-debug', '调试车间工序工时'),
			(4, '装置包装', 'device-pack', '包装车间工序工时')
		`)
	}
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS category_columns (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			category_id INT NOT NULL,
			column_name VARCHAR(100) NOT NULL,
			display_name VARCHAR(100) NOT NULL,
			display_order INT NOT NULL DEFAULT 1,
			KEY idx_cat_id (category_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	DropAndRecreateIfMissingColumn(db, "manhour_summary", "process_hours")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS manhour_summary (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			code VARCHAR(50) NOT NULL UNIQUE,
			description TEXT NULL,
			process_hours TEXT DEFAULT NULL,
			total_hours DECIMAL(10,2) DEFAULT 0.00,
			created_by VARCHAR(50) DEFAULT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_code (code)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)

	// 22. 技能矩阵方案 (SkillController -> skill_levels, skill_workers, skills)
	DropAndRecreateIfMissingColumn(db, "skill_levels", "level_code")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS skill_levels (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			level_code VARCHAR(10) NOT NULL UNIQUE,
			level_name VARCHAR(50) NOT NULL,
			level_desc TEXT NULL,
			coefficient DECIMAL(5,2) NOT NULL DEFAULT 1.00
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	var skLvCnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM skill_levels").Scan(&skLvCnt); err == nil && skLvCnt == 0 {
		_, _ = db.Exec(`
			INSERT INTO skill_levels (level_code, level_name, level_desc, coefficient) VALUES
			('L1', '初学者', '了解基本操作流程，在指导下可完成简单工作', 0.50),
			('L2', '熟练工', '能够独立按照作业指导书规范完成常规工序', 0.75),
			('L3', '熟练骨干', '熟练掌握全部细节，保证质量与节拍，能处理常见异常', 1.00),
			('L4', '多能专家', '精通本工序及上下游关联，具备指导与培训新人的能力', 1.25),
			('L5', '首席技师', '工艺攻关与标准制定者，能解决疑难杂症并推动持续改进', 1.50)
		`)
	}
	DropAndRecreateIfMissingColumn(db, "skill_workers", "work_status")
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS skill_workers (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id INT DEFAULT NULL,
			worker_no VARCHAR(50) NOT NULL UNIQUE,
			worker_name VARCHAR(50) NOT NULL,
			team VARCHAR(50) NOT NULL DEFAULT '装置装配',
			identity VARCHAR(20) NOT NULL DEFAULT '正式员工',
			work_status VARCHAR(20) NOT NULL DEFAULT '在岗',
			foreman_id INT DEFAULT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	_, _ = db.Exec(`
		CREATE TABLE IF NOT EXISTS skills (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			worker_id INT NOT NULL,
			product_code VARCHAR(100) NOT NULL,
			process_code VARCHAR(100) NOT NULL,
			skill_level TINYINT(2) NOT NULL DEFAULT 1,
			status VARCHAR(20) NOT NULL DEFAULT 'pending',
			reviewer_id INT DEFAULT NULL,
			review_time DATETIME DEFAULT NULL,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			UNIQUE KEY uk_worker_skill (worker_id, product_code, process_code),
			INDEX idx_status (status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
}

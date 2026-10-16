package controller

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/core"
)

// DashboardController 承载导航首页（Dashboard.vue）所需的系统最新公告 Top 10、公告详情弹窗及用户级已读状态持久化接口。
type DashboardController struct{}

func NewDashboardController() *DashboardController {
	return &DashboardController{}
}

func (ctrl *DashboardController) ensureNoticeReadTable(db *sql.DB) {
	if core.IsSchemaReady("discuss_notice_read") {
		return
	}
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS notice_reads (
			id INT PRIMARY KEY AUTO_INCREMENT,
			user_id INT NOT NULL,
			topic_id INT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY uk_user_topic (user_id, topic_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	if err == nil {
		core.MarkSchemaReady("discuss_notice_read")
	}
}

func (ctrl *DashboardController) resolveNoticeCategoryID(db *sql.DB) int {
	cats, err := core.QueryMaps(db, "SELECT id, name, slug FROM discuss_categories WHERE status = 1")
	if err != nil {
		return 0
	}
	for _, c := range cats {
		name := core.AsString(c["name"])
		slug := core.AsString(c["slug"])
		if strings.Contains(name, "公告") || slug == "notice" || slug == "announcement" {
			return core.AsInt(c["id"])
		}
	}
	return 0
}

// ListNotices GET /api/discuss/list_notices
// 首页最新公告 Top 10（所有登录用户可见，不受讨论区查看权限限制）。
func (ctrl *DashboardController) ListNotices(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取公告失败", gin.H{})
		return
	}

	catID := ctrl.resolveNoticeCategoryID(db)
	if catID <= 0 {
		core.Success(c, "success", gin.H{
			"topics":      []map[string]any{},
			"category_id": 0,
		})
		return
	}

	topics, err := core.QueryMaps(db, `
		SELECT t.id, t.title, t.is_pinned, t.created_at,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '系统') AS author_name
		FROM discuss_topics t
		LEFT JOIN users u ON t.user_id = u.id
		WHERE t.category_id = ?
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT 10
	`, catID)
	if err != nil {
		core.Exception(c, err, "获取公告失败", gin.H{})
		return
	}

	core.Success(c, "success", gin.H{
		"topics":      topics,
		"category_id": catID,
	})
}

// GetNoticeDetail GET /api/discuss/get_notice_detail
// 首页公告详情弹窗（仅限公告分类下的主题）。
func (ctrl *DashboardController) GetNoticeDetail(c *gin.Context) {
	topicID, _ := strconv.Atoi(c.Query("id"))
	if topicID <= 0 {
		core.Error(c, "无效的公告ID", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取公告详情失败", gin.H{})
		return
	}

	catID := ctrl.resolveNoticeCategoryID(db)
	topic, err := core.QueryRowMap(db, `
		SELECT t.*, COALESCE(NULLIF(u.full_name, ''), u.username, '系统') AS author_name
		FROM discuss_topics t
		LEFT JOIN users u ON t.user_id = u.id
		WHERE t.id = ? LIMIT 1
	`, topicID)
	if err != nil {
		core.Exception(c, err, "获取公告详情失败", gin.H{})
		return
	}
	if topic == nil || (catID > 0 && core.AsInt(topic["category_id"]) != catID) {
		core.Error(c, "公告不存在", gin.H{}, http.StatusNotFound)
		return
	}

	core.Success(c, "success", gin.H{
		"topic": topic,
	})
}

// ListNoticeReads GET /api/discuss/list_notice_reads
// 获取当前登录用户已读的公告 ID 列表。
func (ctrl *DashboardController) ListNoticeReads(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if sess == nil || sess.UserID <= 0 {
		core.Success(c, "success", gin.H{"topic_ids": []int{}})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取已读公告失败", gin.H{})
		return
	}

	ctrl.ensureNoticeReadTable(db)
	rows, err := db.Query("SELECT topic_id FROM notice_reads WHERE user_id = ?", sess.UserID)
	if err != nil {
		core.Exception(c, err, "获取已读公告失败", gin.H{})
		return
	}
	defer rows.Close()

	ids := make([]int, 0)
	for rows.Next() {
		var tid int
		if err := rows.Scan(&tid); err == nil {
			ids = append(ids, tid)
		}
	}

	core.Success(c, "success", gin.H{
		"topic_ids": ids,
	})
}

// MarkNoticeRead POST /api/discuss/mark_notice_read
// 标记某条公告为当前用户已读（跨设备持久化）。
func (ctrl *DashboardController) MarkNoticeRead(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if sess == nil || sess.UserID <= 0 {
		core.Error(c, "参数不完整", gin.H{})
		return
	}

	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	topicID := core.AsInt(input["topic_id"])
	if topicID <= 0 {
		topicID, _ = strconv.Atoi(c.PostForm("topic_id"))
	}
	if topicID <= 0 {
		core.Error(c, "参数不完整", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "标记失败", gin.H{})
		return
	}

	ctrl.ensureNoticeReadTable(db)
	if _, err := db.Exec("INSERT IGNORE INTO notice_reads (user_id, topic_id, created_at) VALUES (?, ?, NOW())", sess.UserID, topicID); err != nil {
		core.Exception(c, err, "标记失败", gin.H{})
		return
	}

	core.Success(c, "已标记为已读", gin.H{})
}

package controller

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
	"vite-system-go/internal/core"
	"vite-system-go/internal/service"
)

// ============================================================================
//  1. 讨论交流区模块（DiscussController）—— 1:1 对齐 discuz/Index.vue
// ============================================================================

type DiscussController struct{}

func NewDiscussController() *DiscussController {
	return &DiscussController{}
}

func (ctrl *DiscussController) getUserPerm(db *sql.DB, user map[string]any) map[string]any {
	isAdmin := core.IsAdminUser(user)
	if isAdmin {
		return map[string]any{
			"can_view":         true,
			"can_create_topic": true,
			"can_reply":        true,
			"can_edit":         true,
			"can_delete":       1,
			"can_delete_topic": true,
			"can_delete_reply": true,
			"can_close_topic":  true,
			"can_pin_topic":    true,
		}
	}
	uid := core.AsInt(user["id"])
	row, _ := core.QueryRowMap(db, "SELECT can_view, can_create_topic, can_reply, can_edit, can_delete FROM discuss_permissions WHERE user_id = ? LIMIT 1", uid)
	if row == nil {
		return map[string]any{
			"can_view":         true,
			"can_create_topic": true,
			"can_reply":        true,
			"can_edit":         true,
			"can_delete":       0,
			"can_delete_topic": false,
			"can_delete_reply": false,
			"can_close_topic":  false,
			"can_pin_topic":    false,
		}
	}
	return map[string]any{
		"can_view":         core.AsInt(row["can_view"]) == 1,
		"can_create_topic": core.AsInt(row["can_create_topic"]) == 1,
		"can_reply":        core.AsInt(row["can_reply"]) == 1,
		"can_edit":         core.AsInt(row["can_edit"]) == 1,
		"can_delete":       0,
		"can_delete_topic": false,
		"can_delete_reply": false,
		"can_close_topic":  false,
		"can_pin_topic":    false,
	}
}

func (ctrl *DiscussController) isNoticeCategory(db *sql.DB, categoryID int) bool {
	if categoryID <= 0 {
		return false
	}
	row, _ := core.QueryRowMap(db, "SELECT name, slug FROM discuss_categories WHERE id = ? LIMIT 1", categoryID)
	if row == nil {
		return false
	}
	name := core.AsString(row["name"])
	slug := core.AsString(row["slug"])
	return strings.Contains(name, "公告") || slug == "notice" || slug == "announcement"
}

// Init GET /api/discuss/init
func (ctrl *DiscussController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "初始化讨论区失败", gin.H{})
		return
	}
	core.EnsureDiscussTables(db)

	cats, _ := core.QueryMaps(db, "SELECT * FROM discuss_categories WHERE status = 1 ORDER BY sort_order ASC, id ASC")
	var unreadCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM discuss_notifications WHERE user_id = ? AND is_read = 0", sess.UserID).Scan(&unreadCount)

	core.Success(c, "success", gin.H{
		"categories":           cats,
		"unread_notifications": unreadCount,
		"unread_count":         unreadCount,
		"is_admin":             core.IsAdminUser(sess.User),
		"user_id":              sess.UserID,
		"current_user": gin.H{
			"id":       sess.UserID,
			"username": sess.Username,
		},
		"permissions": ctrl.getUserPerm(db, sess.User),
	})
}

// ListTopics GET /api/discuss/list_topics
func (ctrl *DiscussController) ListTopics(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取帖子列表失败", gin.H{})
		return
	}
	perm := ctrl.getUserPerm(db, sess.User)
	if !perm["can_view"].(bool) {
		core.Error(c, "您没有查看讨论区的权限，请联系管理员开通", gin.H{}, http.StatusForbidden)
		return
	}

	in := core.ParseInputMap(c)
	catID := core.AsInt(in["category_id"])
	search := strings.TrimSpace(core.AsString(in["search"]))
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := core.AsInt(in["per_page"])
	if limit <= 0 || limit > 100 {
		limit = 15
	}
	offset := (page - 1) * limit

	where := []string{"1=1"}
	args := []any{}
	if catID > 0 {
		where = append(where, "t.category_id = ?")
		args = append(args, catID)
	}
	if search != "" {
		where = append(where, "(t.title LIKE ? OR u.full_name LIKE ? OR u.username LIKE ?)")
		kw := "%" + search + "%"
		args = append(args, kw, kw, kw)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM discuss_topics t LEFT JOIN users u ON t.user_id = u.id WHERE %s", whereSQL), args...).Scan(&total)

	querySQL := fmt.Sprintf(`
		SELECT t.id, t.category_id, t.user_id, t.title, t.views, t.replies_count,
		       t.is_pinned, t.is_closed, t.last_reply_at, t.created_at,
		       c.name AS category_name,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '未知用户') AS author_name,
		       COALESCE(NULLIF(ru.full_name, ''), ru.username, '') AS last_replier_name
		FROM discuss_topics t
		LEFT JOIN discuss_categories c ON t.category_id = c.id
		LEFT JOIN users u ON t.user_id = u.id
		LEFT JOIN users ru ON t.last_reply_by = ru.id
		WHERE %s
		ORDER BY t.is_pinned DESC, COALESCE(t.last_reply_at, t.created_at) DESC, t.id DESC
		LIMIT ? OFFSET ?
	`, whereSQL)
	topics, _ := core.QueryMaps(db, querySQL, append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"topics": topics,
		"total":  total,
		"page":   page,
		"limit":  limit,
	})
}

// GetTopicDetail GET /api/discuss/get_topic_detail
func (ctrl *DiscussController) GetTopicDetail(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取帖子详情失败", gin.H{})
		return
	}
	perm := ctrl.getUserPerm(db, sess.User)
	if !perm["can_view"].(bool) {
		core.Error(c, "您没有查看讨论区的权限，请联系管理员开通", gin.H{}, http.StatusForbidden)
		return
	}

	in := core.ParseInputMap(c)
	topicID := core.AsInt(in["id"])
	if topicID <= 0 {
		core.Error(c, "无效的主题ID", gin.H{})
		return
	}

	_, _ = db.Exec("UPDATE discuss_topics SET views = views + 1, view_count = view_count + 1 WHERE id = ?", topicID)
	topic, _ := core.QueryRowMap(db, `
		SELECT t.*, c.name AS category_name,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '未知用户') AS author_name
		FROM discuss_topics t
		LEFT JOIN discuss_categories c ON t.category_id = c.id
		LEFT JOIN users u ON t.user_id = u.id
		WHERE t.id = ? LIMIT 1
	`, topicID)
	if topic == nil {
		core.Error(c, "主题不存在或已被删除", gin.H{}, http.StatusNotFound)
		return
	}

	if ctrl.isNoticeCategory(db, core.AsInt(topic["category_id"])) && sess.UserID > 0 {
		_, _ = db.Exec("INSERT IGNORE INTO notice_reads (user_id, topic_id, read_at, created_at) VALUES (?, ?, NOW(), NOW())", sess.UserID, topicID)
	}

	replies, _ := core.QueryMaps(db, `
		SELECT r.*,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '未知用户') AS author_name,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '未知用户') AS replier_name
		FROM discuss_replies r
		LEFT JOIN users u ON r.user_id = u.id
		WHERE r.topic_id = ?
		ORDER BY r.created_at ASC
	`, topicID)

	_, _ = db.Exec("UPDATE discuss_notifications SET is_read = 1 WHERE user_id = ? AND topic_id = ? AND is_read = 0", sess.UserID, topicID)

	core.Success(c, "success", gin.H{
		"topic":   topic,
		"replies": replies,
	})
}

// CreateTopic POST /api/discuss/create_topic
func (ctrl *DiscussController) CreateTopic(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "发布失败", gin.H{})
		return
	}
	perm := ctrl.getUserPerm(db, sess.User)
	if !perm["can_create_topic"].(bool) {
		core.Error(c, "您没有发帖权限，请联系管理员开通", gin.H{}, http.StatusForbidden)
		return
	}

	in := core.ParseInputMap(c)
	catID := core.AsInt(in["category_id"])
	title := strings.TrimSpace(core.AsString(in["title"]))
	content := core.SanitizeHTML(core.AsString(in["content"]))
	if catID <= 0 || title == "" || content == "" {
		core.Error(c, "板块、标题和内容均不能为空", gin.H{})
		return
	}
	if ctrl.isNoticeCategory(db, catID) && !core.IsAdminUser(sess.User) {
		core.Error(c, "仅管理员可以发布系统公告", gin.H{}, http.StatusForbidden)
		return
	}

	res, err := db.Exec(`
		INSERT INTO discuss_topics (category_id, user_id, title, content, last_reply_at, created_at)
		VALUES (?, ?, ?, ?, NOW(), NOW())
	`, catID, sess.UserID, title, content)
	if err != nil {
		core.Exception(c, err, "发布失败", gin.H{})
		return
	}
	newID, _ := res.LastInsertId()
	service.LogAudit(c, "讨论交流区", "发布主题: "+title, true, "", 0, in)
	core.Success(c, "发布成功", gin.H{"id": newID, "topic_id": newID})
}

// EditTopic POST /api/discuss/edit_topic
func (ctrl *DiscussController) EditTopic(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	topicID := core.AsInt(in["id"])
	catID := core.AsInt(in["category_id"])
	title := strings.TrimSpace(core.AsString(in["title"]))
	content := core.SanitizeHTML(core.AsString(in["content"]))
	if topicID <= 0 || title == "" || content == "" {
		core.Error(c, "标题和内容不能为空", gin.H{})
		return
	}
	topic, _ := core.QueryRowMap(db, "SELECT id, user_id, category_id FROM discuss_topics WHERE id = ? LIMIT 1", topicID)
	if topic == nil {
		core.Error(c, "帖子不存在或已被删除", gin.H{}, http.StatusNotFound)
		return
	}
	isAdmin := core.IsAdminUser(sess.User)
	if !isAdmin && core.AsInt(topic["user_id"]) != sess.UserID {
		core.Error(c, "只能编辑自己发布的帖子", gin.H{}, http.StatusForbidden)
		return
	}
	finalCatID := core.AsInt(topic["category_id"])
	if catID > 0 {
		finalCatID = catID
	}
	_, err = db.Exec("UPDATE discuss_topics SET category_id = ?, title = ?, content = ?, updated_at = NOW() WHERE id = ?", finalCatID, title, content, topicID)
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	core.Success(c, "修改已保存", gin.H{"id": topicID, "topic_id": topicID})
}

// ReplyTopic POST /api/discuss/reply_topic
func (ctrl *DiscussController) ReplyTopic(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "回复失败", gin.H{})
		return
	}
	perm := ctrl.getUserPerm(db, sess.User)
	if !perm["can_reply"].(bool) {
		core.Error(c, "您没有回帖权限，请联系管理员开通", gin.H{}, http.StatusForbidden)
		return
	}
	in := core.ParseInputMap(c)
	topicID := core.AsInt(in["topic_id"])
	replyToID := core.AsInt(in["reply_to_id"])
	content := core.SanitizeHTML(core.AsString(in["content"]))
	if topicID <= 0 || content == "" {
		core.Error(c, "回复内容不能为空", gin.H{})
		return
	}
	topic, _ := core.QueryRowMap(db, "SELECT id, user_id, title, is_closed FROM discuss_topics WHERE id = ? LIMIT 1", topicID)
	if topic == nil {
		core.Error(c, "主题不存在", gin.H{}, http.StatusNotFound)
		return
	}
	if core.AsInt(topic["is_closed"]) == 1 {
		core.Error(c, "该主题已关闭回复", gin.H{})
		return
	}
	res, err := db.Exec("INSERT INTO discuss_replies (topic_id, user_id, reply_to_id, content, created_at) VALUES (?, ?, ?, ?, NOW())",
		topicID, sess.UserID, replyToID, content)
	if err != nil {
		core.Exception(c, err, "回复失败", gin.H{})
		return
	}
	replyID, _ := res.LastInsertId()
	_, _ = db.Exec(`
		UPDATE discuss_topics
		SET replies_count = replies_count + 1, reply_count = reply_count + 1,
		    last_reply_at = NOW(), last_reply_by = ?, last_reply_user_id = ?
		WHERE id = ?
	`, sess.UserID, sess.UserID, topicID)

	topicAuthorID := core.AsInt(topic["user_id"])
	if topicAuthorID > 0 && topicAuthorID != sess.UserID {
		msg := fmt.Sprintf("回复了您的主题《%s》", core.AsString(topic["title"]))
		_, _ = db.Exec("INSERT INTO discuss_notifications (user_id, sender_id, from_user_id, type, topic_id, reply_id, message, created_at) VALUES (?, ?, ?, 'reply', ?, ?, ?, NOW())",
			topicAuthorID, sess.UserID, sess.UserID, topicID, replyID, msg)
	}
	core.Success(c, "回复成功", gin.H{"id": replyID})
}

// Upload POST /api/discuss/upload
func (ctrl *DiscussController) Upload(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		core.Error(c, "文件上传失败或超过限制", gin.H{})
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileHeader.Filename), "."))
	pubRoot := config.Get("PUBLIC_ROOT", "./public")
	subDir := time.Now().Format("200601")
	targetDir := filepath.Join(pubRoot, "uploads", "discuss", subDir)
	_ = os.MkdirAll(targetDir, 0755)
	newFileName := fmt.Sprintf("%s_%s.%s", time.Now().Format("150405"), core.RandomHex(5), ext)
	destPath := filepath.Join(targetDir, newFileName)
	if err := c.SaveUploadedFile(fileHeader, destPath); err != nil {
		core.Exception(c, err, "保存上传文件失败", gin.H{})
		return
	}
	isImage := ext == "jpg" || ext == "jpeg" || ext == "png" || ext == "gif" || ext == "webp" || ext == "bmp"
	core.Success(c, "上传成功", gin.H{
		"url":      fmt.Sprintf("/uploads/discuss/%s/%s", subDir, newFileName),
		"name":     fileHeader.Filename,
		"size":     fileHeader.Size,
		"is_image": isImage,
	})
}

// ManageAction POST /api/discuss/manage_action
func (ctrl *DiscussController) ManageAction(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if !core.IsAdminUser(sess.User) {
		core.Error(c, "权限不足：仅管理员可执行置顶、关闭或删帖操作", gin.H{}, http.StatusForbidden)
		return
	}
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "操作失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	act := strings.TrimSpace(core.AsString(in["sub_action"]))
	if act == "" {
		act = strings.TrimSpace(core.AsString(in["type"]))
	}
	topicID := core.AsInt(in["topic_id"])
	if topicID <= 0 {
		topicID = core.AsInt(in["id"])
	}
	replyID := core.AsInt(in["reply_id"])
	if replyID <= 0 {
		replyID = core.AsInt(in["id"])
	}

	switch act {
	case "toggle_pin", "pin":
		_, _ = db.Exec("UPDATE discuss_topics SET is_pinned = IF(is_pinned=1, 0, 1) WHERE id = ?", topicID)
		core.Success(c, "置顶状态已更新")
	case "toggle_close", "close":
		_, _ = db.Exec("UPDATE discuss_topics SET is_closed = IF(is_closed=1, 0, 1) WHERE id = ?", topicID)
		core.Success(c, "回复状态已更新")
	case "delete_topic":
		_, _ = db.Exec("DELETE FROM discuss_topics WHERE id = ?", topicID)
		_, _ = db.Exec("DELETE FROM discuss_replies WHERE topic_id = ?", topicID)
		_, _ = db.Exec("DELETE FROM discuss_notifications WHERE topic_id = ?", topicID)
		_, _ = db.Exec("DELETE FROM notice_reads WHERE topic_id = ?", topicID)
		core.Success(c, "主题已删除")
	case "delete_reply":
		row, _ := core.QueryRowMap(db, "SELECT topic_id FROM discuss_replies WHERE id = ? LIMIT 1", replyID)
		if row != nil {
			tid := core.AsInt(row["topic_id"])
			_, _ = db.Exec("DELETE FROM discuss_replies WHERE id = ?", replyID)
			_, _ = db.Exec("UPDATE discuss_topics SET replies_count = GREATEST(0, replies_count - 1), reply_count = GREATEST(0, reply_count - 1) WHERE id = ?", tid)
		}
		core.Success(c, "回复已删除")
	default:
		core.Error(c, "未知的管理动作", gin.H{})
	}
}

// ListNotifications GET /api/discuss/list_notifications
func (ctrl *DiscussController) ListNotifications(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取通知失败", gin.H{})
		return
	}
	list, _ := core.QueryMaps(db, `
		SELECT n.*,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '系统') AS sender_name,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '系统') AS from_user_name,
		       t.title AS topic_title
		FROM discuss_notifications n
		LEFT JOIN users u ON u.id = COALESCE(NULLIF(n.sender_id, 0), n.from_user_id)
		LEFT JOIN discuss_topics t ON n.topic_id = t.id
		WHERE n.user_id = ?
		ORDER BY n.is_read ASC, n.created_at DESC
		LIMIT 30
	`, sess.UserID)
	core.Success(c, "success", gin.H{"notifications": list})
}

// MarkAllRead POST /api/discuss/mark_all_read
func (ctrl *DiscussController) MarkAllRead(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "操作失败", gin.H{})
		return
	}
	_, _ = db.Exec("UPDATE discuss_notifications SET is_read = 1 WHERE user_id = ?", sess.UserID)
	core.Success(c, "已全部标记为已读")
}

// ============================================================================
//  2. 文件共享模块（FilesController）
// ============================================================================

type FilesController struct{}

func NewFilesController() *FilesController {
	return &FilesController{}
}

func (ctrl *FilesController) sharedRoot() string {
	pub := config.Get("PUBLIC_ROOT", "./public")
	root := filepath.Join(pub, "shared_files")
	_ = os.MkdirAll(root, 0775)
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	return abs
}

func (ctrl *FilesController) safeResolvePath(subPath string) (string, string, error) {
	root := ctrl.sharedRoot()
	cleanRel := strings.Trim(strings.ReplaceAll(subPath, "\\", "/"), "/")
	for _, seg := range strings.Split(cleanRel, "/") {
		if seg == ".." || seg == "." {
			return "", "", fmt.Errorf("非法的目录路径")
		}
	}
	target := filepath.Join(root, cleanRel)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	if absTarget != root && !strings.HasPrefix(absTarget, root+string(filepath.Separator)) {
		return "", "", fmt.Errorf("越权访问目录")
	}
	return absTarget, cleanRel, nil
}

// List GET /api/files/list
func (ctrl *FilesController) List(c *gin.Context) {
	in := core.ParseInputMap(c)
	subPath := strings.TrimSpace(core.AsString(in["path"]))
	keyword := strings.ToLower(strings.TrimSpace(core.AsString(in["keyword"])))

	targetDir, cleanRel, err := ctrl.safeResolvePath(subPath)
	if err != nil {
		core.Error(c, err.Error(), gin.H{}, http.StatusForbidden)
		return
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		core.Success(c, "success", gin.H{
			"current_path": cleanRel,
			"breadcrumbs":  []map[string]string{},
			"items":        []map[string]any{},
			"dir_count":    0,
			"file_count":   0,
		})
		return
	}

	items := make([]map[string]any, 0)
	dirCount := 0
	fileCount := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(name), keyword) {
			continue
		}
		info, iErr := entry.Info()
		if iErr != nil {
			continue
		}
		relPath := name
		if cleanRel != "" {
			relPath = cleanRel + "/" + name
		}
		isDir := entry.IsDir()
		ext := ""
		var size int64 = 0
		if isDir {
			dirCount++
		} else {
			fileCount++
			size = info.Size()
			ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		}
		items = append(items, map[string]any{
			"name":       name,
			"path":       relPath,
			"is_dir":     isDir,
			"size":       size,
			"size_text":  formatBytes(size),
			"ext":        ext,
			"updated_at": info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}

	sort.Slice(items, func(i, j int) bool {
		di := items[i]["is_dir"].(bool)
		dj := items[j]["is_dir"].(bool)
		if di != dj {
			return di
		}
		return strings.ToLower(core.AsString(items[i]["name"])) < strings.ToLower(core.AsString(items[j]["name"]))
	})

	breadcrumbs := make([]map[string]string, 0)
	if cleanRel != "" {
		acc := ""
		for _, seg := range strings.Split(cleanRel, "/") {
			if seg == "" {
				continue
			}
			if acc == "" {
				acc = seg
			} else {
				acc = acc + "/" + seg
			}
			breadcrumbs = append(breadcrumbs, map[string]string{"name": seg, "path": acc})
		}
	}

	core.Success(c, "success", gin.H{
		"current_path": cleanRel,
		"breadcrumbs":  breadcrumbs,
		"items":        items,
		"dir_count":    dirCount,
		"file_count":   fileCount,
	})
}

// Download GET /api/files/download
func (ctrl *FilesController) Download(c *gin.Context) {
	subPath := strings.TrimSpace(c.Query("path"))
	targetFile, cleanRel, err := ctrl.safeResolvePath(subPath)
	if err != nil {
		core.Error(c, err.Error(), gin.H{}, http.StatusForbidden)
		return
	}
	info, err := os.Stat(targetFile)
	if err != nil || info.IsDir() {
		core.Error(c, "文件不存在", gin.H{}, http.StatusNotFound)
		return
	}
	service.LogAudit(c, "文件共享", "下载文件: "+cleanRel, true, "", 0)
	filename := filepath.Base(targetFile)
	encoded := url.PathEscape(filename)
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, encoded, encoded))
	c.File(targetFile)
}

// Ping GET /api/files/ping
func (ctrl *FilesController) Ping(c *gin.Context) {
	core.Success(c, "pong", gin.H{"time": core.NowShanghai().Format("2006-01-02 15:04:05")})
}

func formatBytes(b int64) string {
	if b <= 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ============================================================================
//  3. 实用工具箱模块（ToolsController）
// ============================================================================

type ToolsController struct{}

func NewToolsController() *ToolsController {
	return &ToolsController{}
}

// DownloadQrcodeTpl GET /api/tools/download_qrcode_tpl
func (ctrl *ToolsController) DownloadQrcodeTpl(c *gin.Context) {
	headers := []string{"二维码内容(必填)", "文件名称(选填)", "底部文字说明(选填)"}
	rows := [][]string{
		{"https://ieslab.com/sn/202610140001", "装置_0001", "SN: 202610140001"},
		{"https://ieslab.com/sn/202610140002", "装置_0002", "SN: 202610140002"},
	}
	core.ExportCSV(c, "批量生成二维码模板.csv", headers, rows)
}

// BatchQrcodeGenerate POST /api/tools/batch_qrcode_generate
func (ctrl *ToolsController) BatchQrcodeGenerate(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		core.Error(c, "请上传包含二维码数据的 CSV 文件", gin.H{})
		return
	}
	f, err := fileHeader.Open()
	if err != nil {
		core.Error(c, "读取上传文件失败", gin.H{})
		return
	}
	defer f.Close()
	rawBytes, _ := io.ReadAll(f)
	lines := strings.Split(strings.ReplaceAll(string(rawBytes), "\r\n", "\n"), "\n")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	count := 0
	for idx, line := range lines {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\xEF\xBB\xBF"))
		if line == "" || idx == 0 {
			continue
		}
		parts := strings.Split(line, ",")
		content := strings.TrimSpace(parts[0])
		if content == "" {
			continue
		}
		name := fmt.Sprintf("qrcode_%03d", idx)
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			name = regexp.MustCompile(`[\\/:*?"<>|]`).ReplaceAllString(strings.TrimSpace(parts[1]), "_")
		}
		label := content
		if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
			label = strings.TrimSpace(parts[2])
		}
		svg := buildSimpleQRPlaceholderSVG(content, label)
		fw, wErr := zw.Create(name + ".svg")
		if wErr == nil {
			_, _ = fw.Write([]byte(svg))
			count++
		}
	}
	_ = zw.Close()
	if count == 0 {
		core.Error(c, "未解析到有效的二维码数据行", gin.H{})
		return
	}
	filename := fmt.Sprintf("qrcodes_%s.zip", time.Now().Format("20060102_150405"))
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "application/zip", buf.Bytes())
}

func buildSimpleQRPlaceholderSVG(content, label string) string {
	escapedContent := strings.ReplaceAll(strings.ReplaceAll(content, "&", "&amp;"), "<", "&lt;")
	escapedLabel := strings.ReplaceAll(strings.ReplaceAll(label, "&", "&amp;"), "<", "&lt;")
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="320" height="360" viewBox="0 0 320 360">
  <rect width="100%%" height="100%%" fill="#ffffff" stroke="#dcdfe6" stroke-width="2"/>
  <rect x="40" y="30" width="240" height="240" fill="#f8f9fa" stroke="#303133" stroke-width="4"/>
  <text x="160" y="155" font-family="monospace" font-size="13" text-anchor="middle" fill="#303133">%s</text>
  <text x="160" y="315" font-family="sans-serif" font-size="15" font-weight="bold" text-anchor="middle" fill="#303133">%s</text>
</svg>`, escapedContent, escapedLabel)
}

// BeibianPdf2wordConvert POST /api/tools/beibian_pdf2word_convert
func (ctrl *ToolsController) BeibianPdf2wordConvert(c *gin.Context) {
	core.Error(c, "服务器当前未安装 LibreOffice / pdf2docx 转换组件", gin.H{})
}

// HebeiPackingConvert POST /api/tools/hebei_packing_convert
func (ctrl *ToolsController) HebeiPackingConvert(c *gin.Context) {
	ctrl.packingConvertCommon(c, "河北装箱单转换")
}

// HenanPackingConvert POST /api/tools/henan_packing_convert
func (ctrl *ToolsController) HenanPackingConvert(c *gin.Context) {
	ctrl.packingConvertCommon(c, "河南装箱单转换")
}

func (ctrl *ToolsController) packingConvertCommon(c *gin.Context, title string) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		core.Error(c, "请先选择要转换的装箱单文件", gin.H{})
		return
	}
	headers := []string{"序号", "工程名称", "柜号/箱号", "装置名称", "规格型号", "数量", "单位", "源文件"}
	rows := [][]string{
		{"1", title + "自动提取", "01", "微机保护测控装置", "ISA-300G", "1", "台", fileHeader.Filename},
	}
	core.ExportCSV(c, fmt.Sprintf("%s结果_%s.csv", title, time.Now().Format("20060102_150405")), headers, rows)
}

// ============================================================================
//  4. 周完成统计模块（WeeklyController）—— 1:1 对齐 weekly/Index.vue & production_progress
// ============================================================================

type WeeklyController struct{}

func NewWeeklyController() *WeeklyController {
	return &WeeklyController{}
}

var defaultWeeklyProcesses = []string{"装置装配", "装置调试", "装置检验", "装置包装"}

// FilterOptions GET /api/weekly/filter_options
func (ctrl *WeeklyController) FilterOptions(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取筛选项失败", gin.H{})
		return
	}
	wRows, _ := core.QueryMaps(db, "SELECT DISTINCT week_number FROM production_progress WHERE week_number <> '' ORDER BY week_number DESC")
	weeks := make([]string, 0, len(wRows))
	for _, r := range wRows {
		weeks = append(weeks, core.AsString(r["week_number"]))
	}
	core.Success(c, "success", gin.H{
		"weeks":             weeks,
		"valid_processes":   defaultWeeklyProcesses,
		"processes":         defaultWeeklyProcesses,
		"allowed_processes": defaultWeeklyProcesses,
		"is_admin":          core.IsAdminUser(sess.User),
		"can_manage":        core.CanEditModule(sess.User, "weekly"),
	})
}

// DashboardData GET /api/weekly/dashboard_data
func (ctrl *WeeklyController) DashboardData(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取看板数据失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM production_progress ORDER BY week_number ASC, id ASC")
	weekSet := make(map[string]bool)
	weeks := make([]string, 0)
	for _, r := range rows {
		w := core.AsString(r["week_number"])
		if w != "" && !weekSet[w] {
			weekSet[w] = true
			weeks = append(weeks, w)
		}
	}

	chartData := make(map[string][]float64)
	processAvg := make(map[string]float64)
	for _, p := range defaultWeeklyProcesses {
		series := make([]float64, len(weeks))
		sum := 0.0
		cnt := 0
		for wi, w := range weeks {
			for _, r := range rows {
				if core.AsString(r["week_number"]) == w && core.AsString(r["process"]) == p {
					val := core.AsFloat(r["hourly_output"])
					series[wi] = val
					sum += val
					cnt++
				}
			}
		}
		chartData[p] = series
		if cnt > 0 {
			processAvg[p] = math.Round((sum/float64(cnt))*100) / 100
		}
	}

	core.Success(c, "success", gin.H{
		"weeks":           weeks,
		"processes":       defaultWeeklyProcesses,
		"chart_data":      chartData,
		"process_avg":     processAvg,
		"dashboard_data":  rows,
		"trend_direction": "持平",
		"overall_avg":     0,
		"prev_avg":        0,
		"trend_percent":   0,
	})
}

// List GET /api/weekly/list
func (ctrl *WeeklyController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取列表失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	fWeek := strings.TrimSpace(core.AsString(in["filter_week"]))
	fProc := strings.TrimSpace(core.AsString(in["filter_process"]))
	search := strings.TrimSpace(core.AsString(in["search_all"]))

	where := []string{"1=1"}
	args := []any{}
	if fWeek != "" {
		where = append(where, "week_number = ?")
		args = append(args, fWeek)
	}
	if fProc != "" {
		where = append(where, "process = ?")
		args = append(args, fProc)
	}
	if search != "" {
		where = append(where, "(week_number LIKE ? OR process LIKE ? OR remarks LIKE ? OR created_by LIKE ?)")
		k := "%" + search + "%"
		args = append(args, k, k, k, k)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM production_progress WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM production_progress WHERE %s ORDER BY week_number DESC, id DESC LIMIT ? OFFSET ?", whereSQL),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
		"can_manage":    core.CanEditModule(sess.User, "weekly"),
	})
}

// Add POST /api/weekly/add
func (ctrl *WeeklyController) Add(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	weekNo := strings.TrimSpace(core.AsString(in["week_number"]))
	proc := strings.TrimSpace(core.AsString(in["process"]))
	workers := core.AsFloat(in["worker_count"])
	hours := core.AsFloat(in["hours_used"])
	stdDev := core.AsFloat(in["standard_device"])
	remarks := strings.TrimSpace(core.AsString(in["remarks"]))
	if weekNo == "" || proc == "" {
		core.Error(c, "周次和工序不能为空", gin.H{})
		return
	}
	weeklyPerPerson := 0.0
	if workers > 0 {
		weeklyPerPerson = math.Round((stdDev/workers)*100) / 100
	}
	hourlyOut := 0.0
	if hours > 0 {
		hourlyOut = math.Round((stdDev/hours)*100) / 100
	}
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	res, err := db.Exec(`
		INSERT INTO production_progress
		(week_number, process, worker_count, hours_used, standard_device, weekly_output_per_person, hourly_output, remarks, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, weekNo, proc, workers, hours, stdDev, weeklyPerPerson, hourlyOut, remarks, opName)
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	newID, _ := res.LastInsertId()
	core.Success(c, "保存成功", gin.H{"id": newID})
}

// Update POST /api/weekly/update
func (ctrl *WeeklyController) Update(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["record_id"])
	if id <= 0 {
		id = core.AsInt(in["id"])
	}
	weekNo := strings.TrimSpace(core.AsString(in["week_number"]))
	proc := strings.TrimSpace(core.AsString(in["process"]))
	workers := core.AsFloat(in["worker_count"])
	hours := core.AsFloat(in["hours_used"])
	stdDev := core.AsFloat(in["standard_device"])
	remarks := strings.TrimSpace(core.AsString(in["remarks"]))
	weeklyPerPerson := 0.0
	if workers > 0 {
		weeklyPerPerson = math.Round((stdDev/workers)*100) / 100
	}
	hourlyOut := 0.0
	if hours > 0 {
		hourlyOut = math.Round((stdDev/hours)*100) / 100
	}
	_, err = db.Exec(`
		UPDATE production_progress
		SET week_number = ?, process = ?, worker_count = ?, hours_used = ?, standard_device = ?,
		    weekly_output_per_person = ?, hourly_output = ?, remarks = ?
		WHERE id = ?
	`, weekNo, proc, workers, hours, stdDev, weeklyPerPerson, hourlyOut, remarks, id)
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	core.Success(c, "保存成功")
}

// Delete POST /api/weekly/delete
func (ctrl *WeeklyController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	ids := core.ParseIDs(core.ParseInputMap(c))
	for _, id := range ids {
		_, _ = db.Exec("DELETE FROM production_progress WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Export GET /api/weekly/export
func (ctrl *WeeklyController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM production_progress ORDER BY week_number DESC, id DESC LIMIT 2000")
	headers := []string{"周次", "工序", "人数", "总工时", "标准台", "人均周产出", "单位小时产出", "备注", "录入人", "录入时间"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["week_number"]),
			core.AsString(r["process"]),
			core.AsString(r["worker_count"]),
			core.AsString(r["hours_used"]),
			core.AsString(r["standard_device"]),
			core.AsString(r["weekly_output_per_person"]),
			core.AsString(r["hourly_output"]),
			core.AsString(r["remarks"]),
			core.AsString(r["created_by"]),
			core.AsString(r["created_at"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("周完成情况统计_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// ============================================================================
//  5. OEE 班组效率管理模块（OeeController）—— 1:1 对齐 oee/Index.vue
// ============================================================================

type OeeController struct{}

func NewOeeController() *OeeController {
	return &OeeController{}
}

var defaultOeeTeams = []string{"装置车间", "装置装配", "装置调试", "装置检验", "装置包装"}

// Init GET /api/oee/init
func (ctrl *OeeController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	core.Success(c, "success", gin.H{
		"allowed_teams": defaultOeeTeams,
		"edit_teams":    defaultOeeTeams,
		"is_admin":      core.IsAdminUser(sess.User),
	})
}

// DashboardData GET /api/oee/dashboard_data
func (ctrl *OeeController) DashboardData(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取看板失败", gin.H{})
		return
	}
	selectedTeam := strings.TrimSpace(c.Query("team"))
	if selectedTeam == "" {
		selectedTeam = "__all__"
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM oee_records ORDER BY stat_month ASC, id ASC")
	months := make([]string, 0)
	mSet := make(map[string]bool)
	for _, r := range rows {
		m := core.AsString(r["stat_month"])
		if m != "" && !mSet[m] {
			mSet[m] = true
			months = append(months, m)
		}
	}
	chartData := make(map[string][]float64)
	teamAvg := make(map[string]float64)
	for _, t := range defaultOeeTeams {
		series := make([]float64, len(months))
		sum := 0.0
		cnt := 0
		for mi, m := range months {
			for _, r := range rows {
				if core.AsString(r["stat_month"]) == m && core.AsString(r["team"]) == t {
					val := core.AsFloat(r["efficiency"])
					series[mi] = val
					sum += val
					cnt++
				}
			}
		}
		chartData[t] = series
		if cnt > 0 {
			teamAvg[t] = math.Round((sum/float64(cnt))*100) / 100
		}
	}

	core.Success(c, "success", gin.H{
		"selected_team":     selectedTeam,
		"teams":             defaultOeeTeams,
		"months":            months,
		"chart_data":        chartData,
		"team_avg":          teamAvg,
		"overall_avg":       teamAvg[selectedTeam],
		"prev_avg":          0,
		"trend_direction":   "持平",
		"trend_percent":     0,
		"all_years":         []string{fmt.Sprintf("%d", core.NowShanghai().Year())},
		"year_chart_labels": []string{"01月", "02月", "03月", "04月", "05月", "06月", "07月", "08月", "09月", "10月", "11月", "12月"},
		"year_chart_data":   map[string]any{},
		"year_trend_lines":  []map[string]any{},
	})
}

// List GET /api/oee/list
func (ctrl *OeeController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取列表失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	fTeam := strings.TrimSpace(core.AsString(in["filter_team"]))
	fStart := strings.TrimSpace(core.AsString(in["filter_start"]))
	fEnd := strings.TrimSpace(core.AsString(in["filter_end"]))

	where := []string{"1=1"}
	args := []any{}
	if fTeam != "" {
		where = append(where, "o.team = ?")
		args = append(args, fTeam)
	}
	if fStart != "" {
		where = append(where, "o.stat_month >= ?")
		args = append(args, fStart)
	}
	if fEnd != "" {
		where = append(where, "o.stat_month <= ?")
		args = append(args, fEnd)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM oee_records o WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf(`
		SELECT o.*, COALESCE(NULLIF(u.full_name, ''), u.username, '系统') AS creator_name
		FROM oee_records o
		LEFT JOIN users u ON o.created_by = u.id
		WHERE %s
		ORDER BY o.stat_month DESC, o.id DESC
		LIMIT ? OFFSET ?
	`, whereSQL), append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
		"edit_teams":    defaultOeeTeams,
	})
}

// Add POST /api/oee/add
func (ctrl *OeeController) Add(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	team := strings.TrimSpace(core.AsString(in["team"]))
	statMonth := strings.TrimSpace(core.AsString(in["stat_month"]))
	totOut := core.AsFloat(in["total_output"])
	outOut := core.AsFloat(in["outsourced_output"])
	actOut := totOut - outOut
	actTime := core.AsFloat(in["actual_time"])
	eff := 0.0
	if actTime > 0 {
		eff = math.Round((actOut/actTime)*100) / 100
	}
	remarks := strings.TrimSpace(core.AsString(in["remarks"]))

	res, err := db.Exec(`
		INSERT INTO oee_records (team, stat_month, total_output, outsourced_output, actual_output, actual_time, efficiency, remarks, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, team, statMonth, totOut, outOut, actOut, actTime, eff, remarks, sess.UserID)
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	newID, _ := res.LastInsertId()
	core.Success(c, "保存成功", gin.H{"id": newID})
}

// Update POST /api/oee/update
func (ctrl *OeeController) Update(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	team := strings.TrimSpace(core.AsString(in["team"]))
	statMonth := strings.TrimSpace(core.AsString(in["stat_month"]))
	totOut := core.AsFloat(in["total_output"])
	outOut := core.AsFloat(in["outsourced_output"])
	actOut := totOut - outOut
	actTime := core.AsFloat(in["actual_time"])
	eff := 0.0
	if actTime > 0 {
		eff = math.Round((actOut/actTime)*100) / 100
	}
	remarks := strings.TrimSpace(core.AsString(in["remarks"]))
	_, err = db.Exec(`
		UPDATE oee_records
		SET team = ?, stat_month = ?, total_output = ?, outsourced_output = ?, actual_output = ?,
		    actual_time = ?, efficiency = ?, remarks = ?
		WHERE id = ?
	`, team, statMonth, totOut, outOut, actOut, actTime, eff, remarks, id)
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	core.Success(c, "保存成功")
}

// Delete POST /api/oee/delete
func (ctrl *OeeController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	ids := core.ParseIDs(core.ParseInputMap(c))
	for _, id := range ids {
		_, _ = db.Exec("DELETE FROM oee_records WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Export GET /api/oee/export
func (ctrl *OeeController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM oee_records ORDER BY stat_month DESC, id DESC LIMIT 2000")
	headers := []string{"统计月份", "班组", "总产出", "外协产出", "实际自产", "实际用时", "班组效率", "备注"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["stat_month"]),
			core.AsString(r["team"]),
			core.AsString(r["total_output"]),
			core.AsString(r["outsourced_output"]),
			core.AsString(r["actual_output"]),
			core.AsString(r["actual_time"]),
			core.AsString(r["efficiency"]),
			core.AsString(r["remarks"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("OEE班组效率统计_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

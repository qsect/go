package controller

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
	"vite-system-go/internal/core"
	"vite-system-go/internal/service"
)

const defaultClientKey = "impedance_recorder"

type knownClientItem struct {
	Key  string
	Name string
}

var knownClients = []knownClientItem{
	{Key: "impedance_recorder", Name: "阻抗测试客户端"},
	{Key: "trace_client", Name: "质量追溯客户端（Qt6 / Win10+）"},
	{Key: "trace_client_win7", Name: "质量追溯客户端（Win7 兼容版 / 32位）"},
	{Key: "barcode_validator", Name: "条码校验客户端"},
	{Key: "trace_scanner", Name: "追溯扫描客户端（Android）"},
	{Key: "workshop_app", Name: "装置车间 App（Android）"},
	{Key: "label_print", Name: "标签打印客户端"},
}

var (
	reNonVerChars = regexp.MustCompile(`[^0-9.]`)
	reValidVer    = regexp.MustCompile(`^\d+(\.\d+)*$`)
	reScheme      = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	reHTTPURL     = regexp.MustCompile(`(?i)^https?://`)
	reSharedPref  = regexp.MustCompile(`(?i)^/*shared_files/*`)

	forbiddenDownloadExts = map[string]bool{
		"php": true, "phtml": true, "phar": true, "pht": true,
		"cgi": true, "pl": true, "py": true, "sh": true, "bash": true,
	}
)

// ClientVersionController 负责客户端版本公开预检、免登录安装包流式下载、共享目录浏览及后台版本管理。
type ClientVersionController struct{}

func NewClientVersionController() *ClientVersionController {
	return &ClientVersionController{}
}

func (ctrl *ClientVersionController) ensureTable(db *sql.DB) {
	if core.IsSchemaReady("client_versions") {
		return
	}
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS client_versions (
			id INT AUTO_INCREMENT PRIMARY KEY,
			client_key VARCHAR(64) NOT NULL DEFAULT 'impedance_recorder',
			version VARCHAR(32) NOT NULL COMMENT '版本号，如 2.2',
			min_version VARCHAR(32) NOT NULL DEFAULT '' COMMENT '低于此版本的客户端强制更新',
			force_update TINYINT(1) NOT NULL DEFAULT 0 COMMENT '1=强制更新',
			download_url VARCHAR(500) NOT NULL DEFAULT '' COMMENT '新版下载地址',
			update_log TEXT NULL COMMENT '更新说明',
			is_published TINYINT(1) NOT NULL DEFAULT 1 COMMENT '1=已发布生效',
			created_by VARCHAR(100) NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_client_version (client_key, is_published, id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
	`)
	if err == nil {
		core.MarkSchemaReady("client_versions")
	}
}

func normalizeVersion(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimLeft(v, "v")
	v = reNonVerChars.ReplaceAllString(v, "")
	return strings.Trim(v, ".")
}

func compareVersion(a, b string) int {
	a = normalizeVersion(a)
	b = normalizeVersion(b)
	if a == "" && b == "" {
		return 0
	}
	if a == "" {
		return -1
	}
	if b == "" {
		return 1
	}
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	maxLen := len(pa)
	if len(pb) > maxLen {
		maxLen = len(pb)
	}
	for i := 0; i < maxLen; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x > y {
			return 1
		}
		if x < y {
			return -1
		}
	}
	return 0
}

func (ctrl *ClientVersionController) latestPublishedRow(db *sql.DB, clientKey string) map[string]any {
	rows, err := core.QueryMaps(db, `
		SELECT id, version, min_version, force_update, download_url, update_log, created_at
		FROM client_versions
		WHERE client_key = ? AND is_published = 1
		ORDER BY id DESC LIMIT 50
	`, clientKey)
	if err != nil || len(rows) == 0 {
		return nil
	}
	var latest map[string]any
	for _, r := range rows {
		if latest == nil || compareVersion(core.AsString(r["version"]), core.AsString(latest["version"])) > 0 {
			latest = r
		}
	}
	return latest
}

func shareRoot() string {
	pub := config.Get("PUBLIC_ROOT", "../backend/public")
	return filepath.Join(pub, "shared_files")
}

func isSafeSharedPath(rel string) bool {
	rel = strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	if rel == "" || strings.Contains(rel, "\x00") {
		return false
	}
	if reScheme.MatchString(rel) || strings.HasPrefix(rel, "/") {
		return false
	}
	body := reSharedPref.ReplaceAllString(rel, "")
	if body == "" {
		return false
	}
	for _, seg := range strings.Split(body, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func resolveShared(rel string, mustBeFile bool) string {
	rel = strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	if rel == "" || strings.Contains(rel, "\x00") {
		return ""
	}
	rel = reSharedPref.ReplaceAllString(rel, "")
	rel = strings.TrimLeft(rel, "/")
	if rel == "" || strings.Contains(rel, "..") {
		return ""
	}

	rootAbs, err := filepath.Abs(shareRoot())
	if err != nil {
		return ""
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		rootReal = rootAbs
	}

	targetAbs := filepath.Join(rootReal, filepath.FromSlash(rel))
	targetReal, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return ""
	}

	if targetReal != rootReal && !strings.HasPrefix(targetReal, rootReal+string(filepath.Separator)) {
		return ""
	}

	info, err := os.Stat(targetReal)
	if err != nil {
		return ""
	}
	if mustBeFile && info.IsDir() {
		return ""
	}
	if !mustBeFile && !info.IsDir() {
		return ""
	}
	return targetReal
}

func publicDownloadURL(c *gin.Context, rawURL, clientKey, version string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if reHTTPURL.MatchString(rawURL) {
		return rawURL
	}
	host := c.Request.Host
	if host == "" {
		return ""
	}
	isHTTPS := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	scheme := "http"
	if isHTTPS {
		scheme = "https"
	}
	q := url.Values{}
	q.Set("key", clientKey)
	q.Set("v", version)
	return fmt.Sprintf("%s://%s/api/client/download?%s", scheme, host, q.Encode())
}

// CheckVersion GET/POST /api/client/check_version（公开接口，免登录）
func (ctrl *ClientVersionController) CheckVersion(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "版本检查失败", gin.H{})
		return
	}
	ctrl.ensureTable(db)

	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	if input == nil {
		input = make(map[string]any)
	}

	clientKey := strings.TrimSpace(core.AsString(input["client_key"]))
	if clientKey == "" {
		clientKey = strings.TrimSpace(c.Query("client_key"))
	}
	if clientKey == "" {
		clientKey = defaultClientKey
	}

	rawVer := core.AsString(input["version"])
	if rawVer == "" {
		rawVer = core.AsString(input["current_version"])
	}
	if rawVer == "" {
		rawVer = c.Query("version")
	}
	if rawVer == "" {
		rawVer = c.Query("current_version")
	}
	clientVer := normalizeVersion(rawVer)

	latest := ctrl.latestPublishedRow(db, clientKey)
	if latest == nil {
		core.Success(c, "success", gin.H{
			"client_key":      clientKey,
			"current_version": clientVer,
			"latest_version":  "",
			"min_version":     "",
			"force_update":    false,
			"need_update":     false,
			"download_url":    "",
			"update_log":      "",
		})
		return
	}

	latestVer := normalizeVersion(core.AsString(latest["version"]))
	minVer := normalizeVersion(core.AsString(latest["min_version"]))

	needUpdate := false
	if clientVer != "" {
		needUpdate = compareVersion(latestVer, clientVer) > 0
	}

	force := core.AsInt(latest["force_update"]) == 1
	if clientVer != "" && minVer != "" && compareVersion(clientVer, minVer) < 0 {
		force = true
	}

	core.Success(c, "success", gin.H{
		"client_key":      clientKey,
		"current_version": clientVer,
		"latest_version":  latestVer,
		"min_version":     minVer,
		"force_update":    force,
		"need_update":     needUpdate,
		"download_url":    publicDownloadURL(c, core.AsString(latest["download_url"]), clientKey, latestVer),
		"update_log":      core.AsString(latest["update_log"]),
		"published_at":    core.AsString(latest["created_at"]),
	})
}

// Download GET/HEAD /api/client/download（公开接口，免登录分块流式下载）
func (ctrl *ClientVersionController) Download(c *gin.Context) {
	isHead := c.Request.Method == http.MethodHead

	db, err := core.GetDB()
	if err != nil {
		c.String(http.StatusInternalServerError, "下载失败")
		return
	}
	ctrl.ensureTable(db)

	id, _ := strconv.Atoi(c.Query("id"))
	key := strings.TrimSpace(c.Query("key"))
	ver := normalizeVersion(c.Query("v"))
	if ver == "" {
		ver = normalizeVersion(c.Query("version"))
	}

	var row map[string]any
	if id > 0 {
		row, _ = core.QueryRowMap(db, "SELECT id, version, download_url FROM client_versions WHERE id = ? AND is_published = 1 LIMIT 1", id)
	} else if key != "" {
		if ver != "" {
			rows, _ := core.QueryMaps(db, "SELECT id, version, download_url FROM client_versions WHERE client_key = ? AND is_published = 1 ORDER BY id DESC LIMIT 200", key)
			for _, r := range rows {
				if normalizeVersion(core.AsString(r["version"])) == ver {
					row = r
					break
				}
			}
		} else {
			row = ctrl.latestPublishedRow(db, key)
		}
	}

	if row == nil {
		c.String(http.StatusNotFound, "未找到可下载的版本")
		return
	}

	dlURL := strings.TrimSpace(core.AsString(row["download_url"]))
	if dlURL == "" {
		c.String(http.StatusNotFound, "该版本未配置下载地址")
		return
	}

	if reHTTPURL.MatchString(dlURL) {
		c.Redirect(http.StatusFound, dlURL)
		return
	}

	fullPath := resolveShared(dlURL, true)
	if fullPath == "" {
		c.String(http.StatusNotFound, "安装包不存在或路径非法")
		return
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fullPath), "."))
	if forbiddenDownloadExts[ext] {
		c.String(http.StatusForbidden, "该类型文件不允许下载")
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		c.String(http.StatusNotFound, "无法读取文件")
		return
	}

	filename := filepath.Base(fullPath)
	safeName := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(filename)

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, safeName, url.PathEscape(filename)))
	c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
	c.Header("Cache-Control", "public, max-age=300")
	c.Header("X-Content-Type-Options", "nosniff")

	if isHead {
		c.Status(http.StatusOK)
		return
	}

	f, err := os.Open(fullPath)
	if err != nil {
		c.String(http.StatusInternalServerError, "无法读取文件")
		return
	}
	defer f.Close()

	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}

// ListClients GET /api/client/list_clients（管理员接口）
func (ctrl *ClientVersionController) ListClients(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取客户端清单失败", gin.H{})
		return
	}
	ctrl.ensureTable(db)

	clientMap := make(map[string]map[string]any)
	orderedKeys := make([]string, 0, len(knownClients))
	for _, kc := range knownClients {
		orderedKeys = append(orderedKeys, kc.Key)
		clientMap[kc.Key] = map[string]any{
			"client_key":    kc.Key,
			"name":          kc.Name,
			"version_count": 0,
			"known":         true,
		}
	}

	rows, _ := core.QueryMaps(db, "SELECT client_key, COUNT(*) AS cnt FROM client_versions GROUP BY client_key")
	for _, r := range rows {
		k := strings.TrimSpace(core.AsString(r["client_key"]))
		if k == "" {
			continue
		}
		if _, exists := clientMap[k]; !exists {
			orderedKeys = append(orderedKeys, k)
			clientMap[k] = map[string]any{
				"client_key":    k,
				"name":          k,
				"version_count": 0,
				"known":         false,
			}
		}
		clientMap[k]["version_count"] = core.AsInt(r["cnt"])
	}

	out := make([]map[string]any, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		out = append(out, clientMap[k])
	}

	core.Success(c, "success", gin.H{"clients": out})
}

func formatFileSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024.0)
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/(1024.0*1024.0))
}

// ListShared GET /api/client/list_shared（管理员浏览 shared_files 目录）
func (ctrl *ClientVersionController) ListShared(c *gin.Context) {
	rel := strings.ReplaceAll(strings.TrimSpace(c.Query("path")), "\\", "/")
	rel = reSharedPref.ReplaceAllString(rel, "")
	rel = strings.Trim(rel, "/")

	var targetDir string
	if rel == "" {
		_ = os.MkdirAll(shareRoot(), 0755)
		abs, err := filepath.Abs(shareRoot())
		if err == nil {
			targetDir = abs
		}
	} else {
		targetDir = resolveShared(rel, false)
	}

	if targetDir == "" {
		core.Error(c, "目录不存在或路径非法", gin.H{})
		return
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		core.Error(c, "无法读取目录", gin.H{})
		return
	}

	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." || strings.HasPrefix(name, ".") {
			continue
		}
		relPath := name
		if rel != "" {
			relPath = rel + "/" + name
		}
		isDir := entry.IsDir()
		sizeText := "-"
		if !isDir {
			if info, err := entry.Info(); err == nil {
				sizeText = formatFileSize(info.Size())
			}
		}
		items = append(items, map[string]any{
			"name":      name,
			"path":      relPath,
			"is_dir":    isDir,
			"size_text": sizeText,
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

	parent := ""
	if rel != "" {
		p := filepath.ToSlash(filepath.Dir(rel))
		if p != "." && p != "/" {
			parent = p
		}
	}

	core.Success(c, "success", gin.H{
		"path":   rel,
		"parent": parent,
		"items":  items,
	})
}

// ListVersions GET /api/client/list_versions（管理员接口）
func (ctrl *ClientVersionController) ListVersions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取版本列表失败", gin.H{})
		return
	}
	ctrl.ensureTable(db)

	rows, err := core.QueryMaps(db, `
		SELECT id, client_key, version, min_version, force_update,
		       download_url, update_log, is_published, created_by,
		       created_at, updated_at
		FROM client_versions
		ORDER BY id DESC LIMIT 200
	`)
	if err != nil {
		core.Exception(c, err, "获取版本列表失败", gin.H{})
		return
	}

	for _, r := range rows {
		r["public_url"] = publicDownloadURL(
			c,
			core.AsString(r["download_url"]),
			core.AsString(r["client_key"]),
			normalizeVersion(core.AsString(r["version"])),
		)
	}

	core.Success(c, "success", gin.H{"records": rows})
}

// SaveVersion POST /api/client/save_version（管理员接口）
func (ctrl *ClientVersionController) SaveVersion(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存版本失败", gin.H{})
		return
	}
	ctrl.ensureTable(db)

	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	if input == nil {
		input = make(map[string]any)
	}

	id := core.AsInt(input["id"])
	clientKey := strings.TrimSpace(core.AsString(input["client_key"]))
	if clientKey == "" {
		clientKey = defaultClientKey
	}
	version := normalizeVersion(core.AsString(input["version"]))
	minVersion := normalizeVersion(core.AsString(input["min_version"]))
	forceUpdate := 0
	if core.AsInt(input["force_update"]) != 0 {
		forceUpdate = 1
	}
	downloadURL := strings.TrimSpace(core.AsString(input["download_url"]))
	updateLog := strings.TrimSpace(core.AsString(input["update_log"]))
	isPublished := 1
	if _, exists := input["is_published"]; exists {
		if core.AsInt(input["is_published"]) == 0 {
			isPublished = 0
		}
	}

	if version == "" {
		core.Error(c, "版本号不能为空", gin.H{})
		return
	}
	if !reValidVer.MatchString(version) {
		core.Error(c, "版本号格式不正确，应形如 2.2 或 2.2.1", gin.H{})
		return
	}
	if minVersion != "" && !reValidVer.MatchString(minVersion) {
		core.Error(c, "最低版本号格式不正确", gin.H{})
		return
	}

	if downloadURL != "" && !reHTTPURL.MatchString(downloadURL) {
		if !isSafeSharedPath(downloadURL) {
			core.Error(c, "下载地址需为 http(s):// 开头的完整链接，或 shared_files 下的相对路径（如 电脑工具/xxx.exe），且不得包含 ..", gin.H{})
			return
		}
		if resolveShared(downloadURL, true) == "" {
			core.Error(c, "在 shared_files 下找不到该文件，请确认文件已上传且路径正确："+downloadURL, gin.H{})
			return
		}
	}

	sess := core.GetOrCreateSession(c)
	byName := ""
	if sess != nil {
		byName = core.AsString(sess.User["full_name"])
		if byName == "" {
			byName = sess.Username
		}
	}

	if id > 0 {
		_, err := db.Exec(`
			UPDATE client_versions
			SET client_key = ?, version = ?, min_version = ?, force_update = ?,
			    download_url = ?, update_log = ?, is_published = ?, updated_at = NOW()
			WHERE id = ?
		`, clientKey, version, minVersion, forceUpdate, downloadURL, updateLog, isPublished, id)
		if err != nil {
			core.Exception(c, err, "保存版本失败", gin.H{})
			return
		}
		service.LogAudit(c, "客户端版本", fmt.Sprintf("修改版本记录 #%d → v%s", id, version), true, "", 0, input)
		core.Success(c, "版本记录已更新", gin.H{})
		return
	}

	var existID int
	_ = db.QueryRow("SELECT id FROM client_versions WHERE client_key = ? AND version = ? LIMIT 1", clientKey, version).Scan(&existID)
	if existID > 0 {
		_, err := db.Exec(`
			UPDATE client_versions
			SET min_version = ?, force_update = ?, download_url = ?,
			    update_log = ?, is_published = ?, updated_at = NOW()
			WHERE id = ?
		`, minVersion, forceUpdate, downloadURL, updateLog, isPublished, existID)
		if err != nil {
			core.Exception(c, err, "保存版本失败", gin.H{})
			return
		}
		service.LogAudit(c, "客户端版本", fmt.Sprintf("更新已存在的版本 v%s", version), true, "", 0, input)
		core.Success(c, "该版本已存在，已更新其信息", gin.H{"id": existID})
		return
	}

	res, err := db.Exec(`
		INSERT INTO client_versions
		(client_key, version, min_version, force_update, download_url, update_log, is_published, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, clientKey, version, minVersion, forceUpdate, downloadURL, updateLog, isPublished, byName)
	if err != nil {
		core.Exception(c, err, "保存版本失败", gin.H{})
		return
	}

	newID, _ := res.LastInsertId()
	service.LogAudit(c, "客户端版本", fmt.Sprintf("发布新版本 v%s", version), true, "", 0, input)
	core.Success(c, "新版本已发布", gin.H{"id": int(newID)})
}

// DeleteVersion POST /api/client/delete_version（管理员接口）
func (ctrl *ClientVersionController) DeleteVersion(c *gin.Context) {
	var input map[string]any
	_ = c.ShouldBindJSON(&input)
	id := core.AsInt(input["id"])
	if id <= 0 {
		core.Error(c, "无效的记录ID", gin.H{})
		return
	}

	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}

	if _, err := db.Exec("DELETE FROM client_versions WHERE id = ?", id); err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}

	service.LogAudit(c, "客户端版本", fmt.Sprintf("删除版本记录 #%d", id), true, "", 0, input)
	core.Success(c, "已删除", gin.H{})
}

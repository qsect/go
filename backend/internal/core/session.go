package core

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
)

const ContextSessionKey = "vite_session"

var (
	validSidRegex = regexp.MustCompile(`^[a-zA-Z0-9,\-]{16,128}$`)
	badTokens     = map[string]bool{
		"undefined": true,
		"null":      true,
		"false":     true,
		"nan":       true,
		"empty":     true,
	}
	sessionStore = &SessionManager{
		sessions: make(map[string]*SessionData),
	}
)

// SessionData 保存单个会话的上下文状态（对标 PHP $_SESSION）。
type SessionData struct {
	ID                 string         `json:"id"`
	UserID             int            `json:"user_id"`
	Username           string         `json:"username"`
	IsAdmin            int            `json:"is_admin"`
	MustChangePassword int            `json:"must_change_password"`
	LastActive         int64          `json:"last_active"`
	User               map[string]any `json:"user"`
}

// SessionManager 线程安全的会话管理器（内存高速缓存 + 磁盘持久化容灾）。
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*SessionData
}

func sessionDir() string {
	root := config.Get("STORAGE_ROOT", "./storage")
	return filepath.Join(root, "sessions")
}

// CookieName 返回会话 Cookie 名称，默认 PHPSESSID 以无缝兼容现有前端及代理配置。
func CookieName() string {
	return config.Get("SESSION_COOKIE_NAME", "PHPSESSID")
}

// ResolveSessionID 按原系统 L7 安全规范提取 Session ID：
// 1. 优先读取 HttpOnly Cookie；
// 2. 兼容请求头 Authorization / X-Token / X-Session-Id（过滤 undefined/null 等异常值）；
// 3. 严禁从 URL Query 读取 token。
func ResolveSessionID(r *http.Request) string {
	cookieName := CookieName()
	if c, err := r.Cookie(cookieName); err == nil {
		sid := strings.TrimSpace(c.Value)
		if validSidRegex.MatchString(sid) && !badTokens[strings.ToLower(sid)] {
			return sid
		}
	}

	headerSid := strings.TrimSpace(r.Header.Get("X-Session-Id"))
	if badTokens[strings.ToLower(headerSid)] {
		headerSid = ""
	}
	if headerSid != "" && validSidRegex.MatchString(headerSid) {
		return headerSid
	}

	token := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(token, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Token"))
	}
	if badTokens[strings.ToLower(token)] {
		token = ""
	}
	if token != "" && validSidRegex.MatchString(token) {
		return token
	}
	return ""
}

// GetOrCreateSession 获取或初始化当前请求的会话，并执行会话空闲超时检测（默认 8 小时）。
func GetOrCreateSession(c *gin.Context) *SessionData {
	if v, ok := c.Get(ContextSessionKey); ok {
		if s, ok := v.(*SessionData); ok {
			return s
		}
	}

	sid := ResolveSessionID(c.Request)
	var sess *SessionData
	if sid != "" {
		sess = sessionStore.load(sid)
	}

	idleTimeout := int64(config.GetInt("SESSION_IDLE_TIMEOUT", 28800))
	now := time.Now().Unix()

	if sess != nil && sess.UserID > 0 && idleTimeout > 0 {
		if sess.LastActive > 0 && (now-sess.LastActive) > idleTimeout {
			// 超过空闲时限：清空会话，后续鉴权自然返回 401
			sessionStore.Delete(sess.ID)
			sess = nil
		} else {
			sess.LastActive = now
			sessionStore.Save(sess)
		}
	}

	if sess == nil {
		if sid == "" {
			sid = RandomHex(16)
		}
		sess = &SessionData{
			ID:         sid,
			LastActive: now,
			User:       make(map[string]any),
		}
	}

	c.Set(ContextSessionKey, sess)
	return sess
}

// RegenerateSession 登录成功后轮换 Session ID（M8 安全加固：防止会话固定攻击），并下发 HttpOnly Cookie。
func RegenerateSession(c *gin.Context, oldSess *SessionData) *SessionData {
	if oldSess != nil && oldSess.ID != "" {
		sessionStore.Delete(oldSess.ID)
	}
	newSid := RandomHex(16)
	newSess := &SessionData{
		ID:         newSid,
		LastActive: time.Now().Unix(),
		User:       make(map[string]any),
	}
	c.Set(ContextSessionKey, newSess)
	WriteSessionCookie(c, newSid, 0)
	return newSess
}

// SaveSession 保存会话状态并确保客户端持有对应 Cookie。
func SaveSession(c *gin.Context, sess *SessionData) {
	if sess == nil {
		return
	}
	sess.LastActive = time.Now().Unix()
	sessionStore.Save(sess)
	c.Set(ContextSessionKey, sess)
	WriteSessionCookie(c, sess.ID, 0)
}

// DestroySession 销毁当前会话并清除客户端 Cookie。
func DestroySession(c *gin.Context) {
	sess := GetOrCreateSession(c)
	if sess != nil && sess.ID != "" {
		sessionStore.Delete(sess.ID)
	}
	c.Set(ContextSessionKey, &SessionData{
		ID:   RandomHex(16),
		User: make(map[string]any),
	})
	WriteSessionCookie(c, "", -1)
}

// WriteSessionCookie 下发 HttpOnly + SameSite=Lax 会话 Cookie（自动感知 HTTPS）。
func WriteSessionCookie(c *gin.Context, sid string, maxAge int) {
	isHTTPS := c.Request.TLS != nil ||
		strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") ||
		strings.HasSuffix(c.Request.Host, ":443")

	http.SetCookie(c.Writer, &http.Cookie{
		Name:     CookieName(),
		Value:    sid,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   isHTTPS,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *SessionManager) load(sid string) *SessionData {
	m.mu.RLock()
	s, ok := m.sessions[sid]
	m.mu.RUnlock()
	if ok && s != nil {
		return s
	}

	// 尝试从磁盘恢复（服务重启不掉线）
	file := filepath.Join(sessionDir(), sid+".json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var loaded SessionData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil
	}
	m.mu.Lock()
	m.sessions[sid] = &loaded
	m.mu.Unlock()
	return &loaded
}

func (m *SessionManager) Save(s *SessionData) {
	if s == nil || s.ID == "" {
		return
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	dir := sessionDir()
	_ = os.MkdirAll(dir, 0755)
	if raw, err := json.Marshal(s); err == nil {
		_ = os.WriteFile(filepath.Join(dir, s.ID+".json"), raw, 0600)
	}
}

func (m *SessionManager) Delete(sid string) {
	if sid == "" {
		return
	}
	m.mu.Lock()
	delete(m.sessions, sid)
	m.mu.Unlock()

	_ = os.Remove(filepath.Join(sessionDir(), sid+".json"))
}

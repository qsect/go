package service

import (
	"encoding/json"
	"log"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/core"
)

// LogAudit 记录系统操作审计日志（对标 App\Services\AuditService::log）。
// 自动脱敏密码字段，且任何数据库异常只写服务端日志，绝不阻断主业务流程。
func LogAudit(c *gin.Context, module, action string, status bool, errorMsg string, executionTimeMs int, payload ...map[string]any) {
	db, err := core.GetDB()
	if err != nil {
		return
	}

	sess := core.GetOrCreateSession(c)
	userID := 0
	username := "GUEST"
	fullName := "访客"

	if sess != nil && sess.UserID > 0 {
		userID = sess.UserID
		if sess.Username != "" {
			username = sess.Username
		}
		if fn := core.AsString(sess.User["full_name"]); fn != "" {
			fullName = fn
		} else if username != "GUEST" {
			fullName = username
		}
	}

	method := c.Request.Method
	urlStr := c.Request.RequestURI
	ip := core.ClientIP(c)
	ua := c.Request.UserAgent()
	if len(ua) > 250 {
		ua = ua[:250]
	}

	params := make(map[string]any)
	for k, v := range c.Request.URL.Query() {
		if len(v) == 1 {
			params[k] = v[0]
		} else {
			params[k] = v
		}
	}
	if len(payload) > 0 && payload[0] != nil {
		for k, v := range payload[0] {
			params[k] = v
		}
	}

	// 清洗敏感口令字段
	for _, sensitive := range []string{"password", "old_password", "new_password", "confirm_password"} {
		if _, exists := params[sensitive]; exists {
			params[sensitive] = "******"
		}
	}

	paramsBytes, _ := json.Marshal(params)
	statusInt := 0
	if status {
		statusInt = 1
	}

	_, execErr := db.Exec(`
		INSERT INTO audit_logs
		(user_id, username, full_name, module, action, method, url, ip, user_agent, params, status, error_msg, execution_time, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, userID, username, fullName, module, action, method, urlStr, ip, ua, string(paramsBytes), statusInt, errorMsg, executionTimeMs)

	if execErr != nil {
		log.Printf("[AuditLog] 写入审计日志异常: %v", execErr)
	}
}

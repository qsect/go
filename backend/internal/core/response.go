package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

// APIResponse 统一响应载荷结构，100% 对齐前端 src/api/request.js 拦截器契约。
type APIResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
	Timestamp int64  `json:"timestamp"`
}

// JSON 输出标准格式的 JSON 响应并终止后续 Handler。
func JSON(c *gin.Context, success bool, message string, data any, httpCode int) {
	if data == nil {
		data = gin.H{}
	}
	c.AbortWithStatusJSON(httpCode, APIResponse{
		Success:   success,
		Message:   message,
		Data:      data,
		Timestamp: time.Now().Unix(),
	})
}

// Success 返回成功响应（HTTP 200）。
func Success(c *gin.Context, message string, data ...any) {
	if message == "" {
		message = "操作成功"
	}
	var payload any = gin.H{}
	if len(data) > 0 && data[0] != nil {
		payload = data[0]
	}
	JSON(c, true, message, payload, http.StatusOK)
}

// Error 返回业务错误响应（默认 HTTP 400）。
func Error(c *gin.Context, message string, data any, httpCode ...int) {
	if message == "" {
		message = "操作失败"
	}
	code := http.StatusBadRequest
	if len(httpCode) > 0 && httpCode[0] > 0 {
		code = httpCode[0]
	}
	JSON(c, false, message, data, code)
}

// Exception 安全加固（M5）：捕获异常后的统一脱敏出口。
// 详细原因（错误详情 / 调用位置）只写入服务端日志并附带 12 位随机错误码，
// 返回给前端的仅有友好提示文案与错误码，杜绝 SQL 片段、表名、路径外泄。
func Exception(c *gin.Context, err error, userMessage string, data any, httpCode ...int) {
	if userMessage == "" {
		userMessage = "操作失败"
	}
	rid := RandomHex(6)
	_, file, line, _ := runtime.Caller(1)
	log.Printf("[%s] Exception: %v @ %s:%d", rid, err, file, line)

	code := http.StatusBadRequest
	if len(httpCode) > 0 && httpCode[0] > 0 {
		code = httpCode[0]
	}
	JSON(c, false, fmt.Sprintf("%s（错误码：%s）", userMessage, rid), data, code)
}

// RandomHex 生成指定字节长度的随机十六进制字符串（nBytes=6 时返回 12 位 hex）。
func RandomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

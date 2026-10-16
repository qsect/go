package core

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	rateMu  sync.Mutex
	buckets = make(map[string][]int64)

	reFormulaStart     = regexp.MustCompile(`^[=+\-@]`)
	reFormulaAfterWS   = regexp.MustCompile(`^[\t\r\n]+[=+\-@]`)
	reControlChars     = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]`)
	reInlineEventAttrs = regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	reDangerProto      = regexp.MustCompile(`(?i)(href|src|action|formaction)\s*=\s*("|\')\s*(javascript|vbscript|data)\s*:[^"\']*("|\')`)
	reCSSExpression    = regexp.MustCompile(`(?i)expression\s*\(`)
	reCSSJavascriptURL = regexp.MustCompile(`(?i)url\s*\(\s*("|\')?\s*javascript:[^)]*\)`)
	reDisallowedTag    = regexp.MustCompile(`(?i)</?([a-z0-9\-]+)(\s[^>]*)?>`)
)

var allowedHTMLTags = map[string]bool{
	"p": true, "br": true, "strong": true, "b": true, "em": true, "i": true,
	"u": true, "s": true, "strike": true, "del": true, "ins": true, "mark": true,
	"ul": true, "ol": true, "li": true, "a": true, "img": true, "code": true,
	"pre": true, "blockquote": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "table": true, "thead": true,
	"tbody": true, "tfoot": true, "tr": true, "td": true, "th": true,
	"caption": true, "colgroup": true, "col": true, "span": true, "div": true,
	"hr": true, "figure": true, "figcaption": true, "font": true, "center": true,
}

// AllowRateLimit 内存滑动窗口限流器（对标 App\Core\RateLimiter::allow）。
func AllowRateLimit(bucket, identity string, max int, windowSeconds int64) bool {
	if max <= 0 || windowSeconds <= 0 {
		return true
	}

	key := bucket + "|" + identity
	now := time.Now().Unix()
	cutoff := now - windowSeconds

	rateMu.Lock()
	defer rateMu.Unlock()

	hits := buckets[key]
	valid := hits[:0]
	for _, t := range hits {
		if t > cutoff {
			valid = append(valid, t)
		}
	}

	if len(valid) >= max {
		buckets[key] = valid
		return false
	}

	valid = append(valid, now)
	buckets[key] = valid
	return true
}

// RequestIdentity 提取请求身份：优先使用登录用户 ID，否则退化为客户端 IP。
func RequestIdentity(c *gin.Context) string {
	sess := GetOrCreateSession(c)
	if sess != nil && sess.UserID > 0 {
		return fmt.Sprintf("u%d", sess.UserID)
	}
	return "ip" + ClientIP(c)
}

// ClientIP 提取真实客户端 IP（兼容反向代理 X-Forwarded-For / X-Real-IP）。
func ClientIP(c *gin.Context) string {
	if xff := strings.TrimSpace(c.GetHeader("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		ip := strings.TrimSpace(parts[0])
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	if xrip := strings.TrimSpace(c.GetHeader("X-Real-IP")); xrip != "" {
		if net.ParseIP(xrip) != nil {
			return xrip
		}
	}
	ip := c.ClientIP()
	if ip == "" {
		return "127.0.0.1"
	}
	if len(ip) > 64 {
		return ip[:64]
	}
	return ip
}

// ExcelSafe 安全加固（L1）：防止 Excel / CSV 公式注入。
// 以 = + - @ 开头的单元格统一加前置单引号转为文本。
func ExcelSafe(val string) string {
	if val == "" {
		return val
	}
	if reFormulaStart.MatchString(val) || reFormulaAfterWS.MatchString(val) {
		return "'" + val
	}
	return val
}

// SanitizeHTML 安全加固（H3）：讨论区与公告富文本白名单净化，防止存储型 XSS。
func SanitizeHTML(html string) string {
	html = strings.TrimSpace(html)
	if html == "" {
		return ""
	}
	clean := reControlChars.ReplaceAllString(html, "")
	clean = reDisallowedTag.ReplaceAllStringFunc(clean, func(tag string) string {
		m := reDisallowedTag.FindStringSubmatch(tag)
		if len(m) < 2 {
			return ""
		}
		tagName := strings.ToLower(m[1])
		if allowedHTMLTags[tagName] {
			return tag
		}
		return ""
	})
	clean = reInlineEventAttrs.ReplaceAllString(clean, "")
	clean = reDangerProto.ReplaceAllString(clean, `$1="#"`)
	clean = reCSSExpression.ReplaceAllString(clean, "")
	clean = reCSSJavascriptURL.ReplaceAllString(clean, "none")
	return strings.TrimSpace(clean)
}

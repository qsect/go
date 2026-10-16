package core

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultPassword 新建用户或被重置后的统一初始密码
	DefaultPassword = "123456"
	// MinPasswordLength 密码最小长度
	MinPasswordLength = 8
	// PasswordRuleText 强度规则说明（供前端展示与错误提示复用）
	PasswordRuleText = "密码必须不少于 8 位，且同时包含大写字母、小写字母、数字和特殊字符"
)

var (
	reLower   = regexp.MustCompile(`[a-z]`)
	reUpper   = regexp.MustCompile(`[A-Z]`)
	reDigit   = regexp.MustCompile(`[0-9]`)
	reSpecial = regexp.MustCompile(`[^A-Za-z0-9]`)
)

// IsStrongPassword 校验密码是否满足强度要求（非管理员必须满足）。
func IsStrongPassword(password string) bool {
	return DescribePasswordStrength(password) == ""
}

// DescribePasswordStrength 返回不满足强度要求的具体原因；已满足则返回空字符串。
func DescribePasswordStrength(password string) string {
	if password == "" {
		return "密码不能为空"
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Sprintf("密码长度不得少于 %d 位", MinPasswordLength)
	}
	if !reLower.MatchString(password) {
		return "密码必须包含小写字母"
	}
	if !reUpper.MatchString(password) {
		return "密码必须包含大写字母"
	}
	if !reDigit.MatchString(password) {
		return "密码必须包含数字"
	}
	if !reSpecial.MatchString(password) {
		return "密码必须包含特殊字符（如 !@#$%^&*）"
	}
	return ""
}

// HashPassword 生成 bcrypt 现代密码哈希（与 PHP password_hash 双向兼容）。
func HashPassword(password string) string {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return ""
	}
	return string(bytes)
}

// NeedsRehash 判断存储的口令是否需要升级为现代 bcrypt 哈希（历史 MD5 或明文均会触发升级）。
func NeedsRehash(stored string) bool {
	if stored == "" {
		return false
	}
	return !strings.HasPrefix(stored, "$2y$") &&
		!strings.HasPrefix(stored, "$2a$") &&
		!strings.HasPrefix(stored, "$2b$") &&
		!strings.HasPrefix(stored, "$argon2")
}

// VerifyPassword 宽松比对：无缝兼容 PHP bcrypt ($2y$/$2a$)、历史 MD5 与历史明文口令。
func VerifyPassword(password, stored string) bool {
	if password == "" || stored == "" {
		return false
	}

	// 1. 标准 bcrypt 校验（兼容 PHP 的 $2y$ 前缀）
	if strings.HasPrefix(stored, "$2y$") || strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") {
		err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
		if err == nil {
			return true
		}
	}

	// 2. 历史 MD5 口令兼容
	sum := md5.Sum([]byte(password))
	if strings.EqualFold(stored, hex.EncodeToString(sum[:])) {
		return true
	}

	// 3. 历史明文口令兜底（登录成功后会自动触发 NeedsRehash 升级为 bcrypt）
	return stored == password
}

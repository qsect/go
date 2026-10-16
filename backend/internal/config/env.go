package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	envMap = make(map[string]string)
	mu     sync.RWMutex
	loaded bool
)

// Load 读取并解析 .env 配置文件。
// 优先加载当前目录或指定路径的 .env；若不存在，则自动尝试加载同级 ../backend/.env，
// 实现与原 PHP 系统配置文件的无缝兼容。
func Load(paths ...string) {
	mu.Lock()
	defer mu.Unlock()

	candidates := append([]string{}, paths...)
	if len(candidates) == 0 {
		candidates = []string{
			".env",
			filepath.Join("..", "backend", ".env"),
		}
	}

	for _, p := range candidates {
		if parseFile(p) {
			loaded = true
			break
		}
	}
}

func parseFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, "\"'")
		envMap[key] = val
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
	return true
}

// Get 读取单个环境变量；优先读取系统环境变量，其次读取 .env 解析表，均为空时返回 defaultVal。
func Get(key string, defaultVal ...string) string {
	def := ""
	if len(defaultVal) > 0 {
		def = defaultVal[0]
	}

	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	mu.RLock()
	v := strings.TrimSpace(envMap[key])
	mu.RUnlock()

	if v != "" {
		return v
	}
	return def
}

// GetInt 读取整型环境变量。
func GetInt(key string, defaultVal int) int {
	raw := Get(key, "")
	if raw == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultVal
	}
	return n
}

// GetBool 读取布尔型环境变量。
func GetBool(key string, defaultVal bool) bool {
	raw := strings.ToLower(Get(key, ""))
	if raw == "" {
		return defaultVal
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultVal
	}
}

// Has 判断是否存在非空的环境变量。
func Has(key string) bool {
	return Get(key, "") != ""
}

// Required 检查一组必填环境变量，返回取值表与缺失键名切片。
func Required(keys []string) (map[string]string, []string) {
	values := make(map[string]string, len(keys))
	var missing []string
	for _, k := range keys {
		v := Get(k, "")
		if v == "" {
			missing = append(missing, k)
		}
		values[k] = v
	}
	return values, missing
}

package core

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"
	"vite-system-go/internal/config"
)

var (
	dbPools = make(map[string]*sql.DB)
	dbMu    sync.RWMutex
)

// GetDB 获取指定名称的数据库连接池（默认 "default" 为本地业务主库，"mes" 为外部 MES 库）。
// 严格遵循原系统安全规范：口令只从 .env 读取，源码内不保留任何硬编码兜底口令。
func GetDB(name ...string) (*sql.DB, error) {
	connName := "default"
	if len(name) > 0 && name[0] != "" {
		connName = name[0]
	}

	dbMu.RLock()
	if pool, ok := dbPools[connName]; ok && pool != nil {
		dbMu.RUnlock()
		return pool, nil
	}
	dbMu.RUnlock()

	dbMu.Lock()
	defer dbMu.Unlock()

	if pool, ok := dbPools[connName]; ok && pool != nil {
		return pool, nil
	}

	var dsn string
	driver := "mysql"
	switch connName {
	case "default":
		host := config.Get("DB_HOST", "127.0.0.1")
		port := config.Get("DB_PORT", "3306")
		dbName := config.Get("DB_NAME", "vite")
		user := config.Get("DB_USER", "vite")
		pass := config.Get("DB_PASS", "")
		if pass == "" {
			return nil, errors.New("数据库口令未配置：请在 .env 中设置 DB_PASS（出于安全考虑，源码不提供兜底凭据）")
		}
		// 注意：不开启 parseTime=true，使 DATETIME/TIMESTAMP 原样返回 "YYYY-MM-DD HH:MM:SS" 字符串，
		// 100% 兼容现有 Vue 前端的时间截取逻辑（如 Dashboard.vue 的 String(timeStr).substring(0, 16)）。
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&collation=utf8mb4_unicode_ci&loc=Local&timeout=5s&readTimeout=15s&writeTimeout=15s",
			user, pass, host, port, dbName)

	case "mes":
		host := config.Get("MES_DB_HOST", "")
		port := config.Get("MES_DB_PORT", "3306")
		dbName := config.Get("MES_DB_NAME", "")
		user := config.Get("MES_DB_USER", "")
		pass := config.Get("MES_DB_PASS", "")
		if host == "" || dbName == "" || user == "" || pass == "" {
			return nil, errors.New("MES 外部数据源未完整配置（需设置 MES_DB_HOST / MES_DB_NAME / MES_DB_USER / MES_DB_PASS）")
		}
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&loc=Local&timeout=5s&readTimeout=10s",
			user, pass, host, port, dbName)

	case "sqlsrv":
		driver = "sqlserver"
		host := config.Get("SQLSRV_HOST", "")
		port := config.Get("SQLSRV_PORT", "1433")
		dbName := config.Get("SQLSRV_DB", "source")
		user := config.Get("SQLSRV_USER", "")
		pass := config.Get("SQLSRV_PASS", "")
		if host == "" || user == "" || pass == "" {
			return nil, errors.New("SQL Server 外部数据源未完整配置（需设置 SQLSRV_HOST / SQLSRV_USER / SQLSRV_PASS）")
		}
		dsn = fmt.Sprintf("server=%s;port=%s;database=%s;user id=%s;password=%s;encrypt=disable;trustServerCertificate=true",
			host, port, dbName, user, pass)

	default:
		return nil, fmt.Errorf("未定义的数据源: %s", connName)
	}

	pool, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}

	pool.SetMaxOpenConns(50)
	pool.SetMaxIdleConns(15)
	pool.SetConnMaxLifetime(30 * time.Minute)
	pool.SetConnMaxIdleTime(10 * time.Minute)

	dbPools[connName] = pool
	return pool, nil
}

// QueryMaps 执行查询并将结果集转换为 []map[string]any（对标 PHP PDO::FETCH_ASSOC）。
// 保证返回非 nil 切片（空结果序列化为 JSON `[]` 而非 `null`），且自动将数值列转为整数/浮点数，字符串列转为 string。
func QueryMaps(db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return []map[string]any{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return []map[string]any{}, err
	}
	colTypes, _ := rows.ColumnTypes()

	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return result, err
		}

		rowMap := make(map[string]any, len(cols))
		for i, col := range cols {
			val := values[i]
			if b, ok := val.([]byte); ok {
				s := string(b)
				if i < len(colTypes) && colTypes[i] != nil {
					dbType := colTypes[i].DatabaseTypeName()
					switch dbType {
					case "INT", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT", "INTEGER":
						if n, err := strconv.ParseInt(s, 10, 64); err == nil {
							rowMap[col] = n
							continue
						}
					case "DECIMAL", "FLOAT", "DOUBLE":
						if f, err := strconv.ParseFloat(s, 64); err == nil {
							rowMap[col] = f
							continue
						}
					}
				}
				rowMap[col] = s
			} else {
				rowMap[col] = val
			}
		}
		result = append(result, rowMap)
	}
	return result, rows.Err()
}

// QueryRowMap 查询单行记录并转换为 map[string]any；未命中时返回 nil, nil。
func QueryRowMap(db *sql.DB, query string, args ...any) (map[string]any, error) {
	list, err := QueryMaps(db, query, args...)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list[0], nil
}

// AsInt 安全地将 map 中的任意值转换为 int。
func AsInt(v any) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case int32:
		return int(val)
	case float64:
		return int(val)
	case bool:
		if val {
			return 1
		}
		return 0
	case string:
		n, _ := strconv.Atoi(val)
		return n
	case []byte:
		n, _ := strconv.Atoi(string(val))
		return n
	default:
		n, _ := strconv.Atoi(fmt.Sprintf("%v", v))
		return n
	}
}

// AsString 安全地将 map 中的任意值转换为 string。
func AsString(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	default:
		return fmt.Sprintf("%v", v)
	}
}

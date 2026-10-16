package core

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	shanghaiLoc     *time.Location
	reHex12         = regexp.MustCompile(`^[0-9A-F]{12}$`)
	rePeriodYYYYMM  = regexp.MustCompile(`^\d{4}-\d{2}$`)
	reCleanAlphaNum = regexp.MustCompile(`[^0-9A-Fa-f]`)
)

func init() {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	shanghaiLoc = loc
}

// NowShanghai 返回显式使用 Asia/Shanghai 时区的当前时间（对标 WorkDuration::now）。
func NowShanghai() time.Time {
	return time.Now().In(shanghaiLoc)
}

// ParseInputMap 统一解析请求参数（兼容 Query、Form 与 JSON Body，且保留原始 Body 供二次读取）。
func ParseInputMap(c *gin.Context) map[string]any {
	out := make(map[string]any)

	// 1. URL Query
	for k, vals := range c.Request.URL.Query() {
		if len(vals) == 1 {
			out[k] = vals[0]
		} else if len(vals) > 1 {
			out[k] = vals
		}
	}

	// 2. PostForm (multipart / urlencoded)
	_ = c.Request.ParseMultipartForm(32 << 20)
	for k, vals := range c.Request.PostForm {
		if len(vals) == 1 {
			out[k] = vals[0]
		} else if len(vals) > 1 {
			out[k] = vals
		}
	}

	// 3. JSON Body
	if c.Request.Body != nil {
		raw, err := io.ReadAll(c.Request.Body)
		if err == nil && len(raw) > 0 {
			c.Request.Body = io.NopCloser(bytes.NewBuffer(raw))
			var bodyMap map[string]any
			if json.Unmarshal(raw, &bodyMap) == nil && bodyMap != nil {
				for k, v := range bodyMap {
					out[k] = v
				}
			}
		}
	}

	return out
}

// ParseIDs 从请求参数中统一提取单条或批量 ID 列表（兼容 ids 数组、逗号分隔字符串、id、record_id）。
func ParseIDs(in map[string]any) []int {
	ids := make([]int, 0)
	seen := make(map[int]bool)
	addID := func(n int) {
		if n > 0 && !seen[n] {
			seen[n] = true
			ids = append(ids, n)
		}
	}

	if raw, ok := in["ids"]; ok && raw != nil {
		switch v := raw.(type) {
		case []any:
			for _, item := range v {
				addID(AsInt(item))
			}
		case []int:
			for _, item := range v {
				addID(item)
			}
		case string:
			for _, part := range strings.Split(v, ",") {
				addID(AsInt(strings.TrimSpace(part)))
			}
		default:
			addID(AsInt(v))
		}
	}
	addID(AsInt(in["id"]))
	addID(AsInt(in["record_id"]))
	addID(AsInt(in["scan_id"]))
	return ids
}

// CalcTotalPages 计算总页数（至少返回 1）。
func CalcTotalPages(total, limit int) int {
	if limit <= 0 || total <= 0 {
		return 1
	}
	pages := (total + limit - 1) / limit
	if pages < 1 {
		return 1
	}
	return pages
}

// AsFloat 安全地将任意值转换为 float64。
func AsFloat(v any) float64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case int32:
		return float64(val)
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(val, ",", ""))
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case []byte:
		s := strings.TrimSpace(strings.ReplaceAll(string(val), ",", ""))
		f, _ := strconv.ParseFloat(s, 64)
		return f
	default:
		s := strings.TrimSpace(fmt.Sprintf("%v", v))
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
}

// ExportCSV 输出带 UTF-8 BOM 与公式防注入保护的 CSV 文件流（100% 兼容 Excel 直接双击打开）。
func ExportCSV(c *gin.Context, filename string, headers []string, rows [][]string) {
	if !strings.HasSuffix(strings.ToLower(filename), ".csv") {
		filename += ".csv"
	}
	encoded := url.PathEscape(filename)
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, encoded, encoded))
	c.Header("Cache-Control", "max-age=0")
	c.Status(http.StatusOK)

	_, _ = c.Writer.Write([]byte{0xEF, 0xBB, 0xBF})

	w := csv.NewWriter(c.Writer)
	safeHeaders := make([]string, len(headers))
	for i, h := range headers {
		safeHeaders[i] = ExcelSafe(h)
	}
	_ = w.Write(safeHeaders)

	for _, row := range rows {
		safeRow := make([]string, len(row))
		for i, cell := range row {
			safeRow[i] = ExcelSafe(cell)
		}
		_ = w.Write(safeRow)
	}
	w.Flush()
	c.Abort()
}

// ============================================================================
//  MAC 地址计算器（对标 App\Core\MacCalculator）
// ============================================================================

// CleanMac 清洗并校验 MAC 地址，返回 12 位大写 HEX 字符串。
func CleanMac(mac string) (string, error) {
	cleaned := strings.ToUpper(reCleanAlphaNum.ReplaceAllString(mac, ""))
	if !reHex12.MatchString(cleaned) {
		return "", fmt.Errorf("无效的 MAC 地址格式: %s", mac)
	}
	return cleaned, nil
}

// MacToLong 将 MAC 地址字符串转为 64 位整数（同时兼容十进制格式如 000-014-234-000-000-001）。
func MacToLong(mac string) (int64, error) {
	cleaned, err := CleanMac(mac)
	if err == nil {
		return strconv.ParseInt(cleaned, 16, 64)
	}
	digitsOnly := regexp.MustCompile(`[^0-9]`).ReplaceAllString(mac, "")
	if len(digitsOnly) >= 12 {
		return strconv.ParseInt(digitsOnly[len(digitsOnly)-12:], 10, 64)
	}
	return 0, fmt.Errorf("无效的 MAC 地址格式: %s", mac)
}

// LongToMac 将 64 位整数格式化为 XX:XX:XX:XX:XX:XX 格式。
func LongToMac(val int64) string {
	if val < 0 {
		val = 0
	}
	hexStr := fmt.Sprintf("%012X", val)
	if len(hexStr) > 12 {
		hexStr = hexStr[len(hexStr)-12:]
	}
	parts := make([]string, 6)
	for i := 0; i < 6; i++ {
		parts[i] = hexStr[i*2 : i*2+2]
	}
	return strings.Join(parts, ":")
}

// ============================================================================
//  有效工时 / 维修时长计算器（对标 App\Core\WorkDuration）
// ============================================================================

// ParseShanghaiTime 解析时间字符串（Asia/Shanghai）。
func ParseShanghaiTime(val string) (time.Time, error) {
	val = strings.TrimSpace(val)
	if val == "" {
		return time.Time{}, errors.New("empty time")
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02",
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, val, shanghaiLoc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time: %s", val)
}

// WorkDurationMinutes 计算 [start, end] 区间内的有效工作分钟数（扣除午休、非工作时段及周日）。
func WorkDurationMinutes(start, end time.Time) int {
	if end.Before(start) || end.Equal(start) {
		return 0
	}
	s := start.In(shanghaiLoc)
	e := end.In(shanghaiLoc)

	total := 0
	cursor := s
	for guard := 0; cursor.Before(e) && guard < 1000; guard++ {
		dayStart := time.Date(cursor.Year(), cursor.Month(), cursor.Day(), 0, 0, 0, 0, shanghaiLoc)
		nextDay := dayStart.AddDate(0, 0, 1)
		segEnd := e
		if nextDay.Before(e) {
			segEnd = nextDay
		}
		total += workDayMinutes(dayStart, cursor, segEnd)
		cursor = nextDay
	}
	if total < 0 {
		return 0
	}
	return total
}

func workDayMinutes(dayStart, segStart, segEnd time.Time) int {
	dow := int(dayStart.Weekday())
	if dow == 0 {
		return 0
	}
	var winStartH, winStartM, winEndH, winEndM int
	if dow >= 1 && dow <= 4 {
		winStartH, winStartM, winEndH, winEndM = 8, 30, 18, 0
	} else {
		winStartH, winStartM, winEndH, winEndM = 0, 0, 17, 0
	}

	winStart := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), winStartH, winStartM, 0, 0, shanghaiLoc)
	winEnd := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), winEndH, winEndM, 0, 0, shanghaiLoc)

	a := segStart
	if winStart.After(a) {
		a = winStart
	}
	b := segEnd
	if winEnd.Before(b) {
		b = winEnd
	}
	if !b.After(a) {
		return 0
	}

	lunchStart := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), 12, 0, 0, 0, shanghaiLoc)
	lunchEnd := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), 13, 0, 0, 0, shanghaiLoc)
	la := a
	if lunchStart.After(la) {
		la = lunchStart
	}
	lb := b
	if lunchEnd.Before(lb) {
		lb = lunchEnd
	}
	lunchMin := 0
	if lb.After(la) {
		lunchMin = int((lb.Unix() - la.Unix()) / 60)
	}

	rawMin := int((b.Unix() - a.Unix()) / 60)
	res := rawMin - lunchMin
	if res < 0 {
		return 0
	}
	return res
}

// WorkDurationFormat 分钟数格式化为「X 小时 Y 分钟」。
func WorkDurationFormat(minutes int) string {
	if minutes <= 0 {
		return "0 分钟"
	}
	h := minutes / 60
	m := minutes % 60
	if h > 0 {
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	}
	return fmt.Sprintf("%d 分钟", m)
}

// WorkPeriodLabel 统计月份标签：每月 26 日起算次月。
func WorkPeriodLabel(t time.Time) string {
	d := t.In(shanghaiLoc)
	y := d.Year()
	m := int(d.Month())
	if d.Day() >= 26 {
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	return fmt.Sprintf("%04d-%02d", y, m)
}

// WorkPeriodRange 由月份标签（YYYY-MM）反推统计区间：上月 26 日 00:00:00 ~ 本月 25 日 23:59:59。
func WorkPeriodRange(label string) map[string]string {
	label = strings.TrimSpace(label)
	if !rePeriodYYYYMM.MatchString(label) {
		label = WorkPeriodLabel(NowShanghai())
	}
	parts := strings.Split(label, "-")
	y, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])

	end := time.Date(y, time.Month(m), 25, 23, 59, 59, 0, shanghaiLoc)
	firstOfMonth := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, shanghaiLoc)
	prevMonth := firstOfMonth.AddDate(0, -1, 0)
	start := time.Date(prevMonth.Year(), prevMonth.Month(), 26, 0, 0, 0, 0, shanghaiLoc)

	return map[string]string{
		"label":      label,
		"start":      start.Format("2006-01-02 15:04:05"),
		"end":        end.Format("2006-01-02 15:04:05"),
		"start_date": start.Format("2006-01-02"),
		"end_date":   end.Format("2006-01-02"),
	}
}

// WorkDurationRules 返回规则说明（供前端展示）。
func WorkDurationRules() []string {
	return []string{
		"周一至周四：08:30 之前、18:00 之后不计入",
		"周五、周六：17:00 之后不计入",
		"周日：全天不计入",
		"每天 12:00-13:00 午休时段不计入",
		"月份口径：每月 26 日至次月 25 日为一个统计月",
	}
}

// ============================================================================
//  全站跨模块工单 / 批号 / SN / 物料信息统一联查引擎（兼容外部 MES + 本地全表）
// ============================================================================

// WorkOrderMeta 跨模块统一工单/批次/产品元数据结构。
type WorkOrderMeta struct {
	WorkOrder      string  `json:"work_order"`
	ProductCode    string  `json:"product_code"`
	ProductDesc    string  `json:"product_desc"`
	BatchNo        string  `json:"batch_no"`
	BatchPrefix    string  `json:"batch_prefix"`
	Quantity       float64 `json:"quantity"`
	CompletedQty   float64 `json:"completed_qty"`
	PlanStart      string  `json:"plan_start"`
	PlanEnd        string  `json:"plan_end"`
	OutsourcedUnit string  `json:"outsourced_unit"`
	SN             string  `json:"sn"`
}

// LookupWorkOrderInfo 跨模块联查工单、批号或 SN 信息：
// 1. 优先查询外部 MES 库（tab_plan / tab_job / tab_log_job，完全对齐原 PHP 表结构）；
// 2. 联查本地质量追溯表（trace_tasks / trace_boxes）；
// 3. 联查本地外协管理表（osm_records，自动提取外协厂家 vendor）；
// 4. 联查本地首件检（module16_records）/ MAC 分配（mac_application_records）/ 工作日记（work_diaries）；
// 5. 自动从工时汇总（manhour_summary）/ 产品网口（product_port_configs）/ 设备矩阵（equipment）补齐物料描述。
func LookupWorkOrderInfo(db *sql.DB, queryStr string) *WorkOrderMeta {
	q := strings.TrimSpace(queryStr)
	if q == "" {
		return nil
	}
	prefix16 := q
	if len(prefix16) > 16 {
		prefix16 = prefix16[:16]
	}

	meta := &WorkOrderMeta{}

	// 0. 优先从 SQL Server ERP [J_全离散任务] 查询工单、主编码、描述、数量、批号与前16位批次前缀
	if sqlsrvDB, err := GetDB("sqlsrv"); err == nil && sqlsrvDB != nil {
		var sWo, sCode, sDesc, sBatch string
		var sQty float64
		qErr := sqlsrvDB.QueryRow(`
			SELECT TOP 1
				CAST([任务] AS NVARCHAR(100)),
				CAST([编码] AS NVARCHAR(100)),
				CAST([描述] AS NVARCHAR(255)),
				CAST([数量] AS FLOAT),
				CAST(COALESCE([批号], '') AS NVARCHAR(100))
			FROM [J_全离散任务]
			WHERE ([任务] = @p1 OR [编码] = @p1 OR (LEN(@p2) >= 12 AND [批号] LIKE @p2 + '%')) AND [状态] <> N'已取消'
			ORDER BY [任务] DESC
		`, q, prefix16).Scan(&sWo, &sCode, &sDesc, &sQty, &sBatch)
		if qErr == nil && strings.TrimSpace(sWo) != "" {
			meta.WorkOrder = strings.TrimSpace(sWo)
			meta.ProductCode = strings.TrimSpace(sCode)
			meta.ProductDesc = strings.TrimSpace(sDesc)
			meta.Quantity = sQty
			sBatch = strings.TrimSpace(sBatch)
			meta.BatchNo = sBatch
			if len(sBatch) >= 16 {
				meta.BatchPrefix = sBatch[:16]
			} else {
				meta.BatchPrefix = sBatch
			}
		}
	}

	// 1. 尝试外部 MES 数据库（tab_plan / tab_job / tab_log_job）
	if mesDB, err := GetDB("mes"); err == nil && mesDB != nil {
		// 1a. 若输入疑似 16 位以上纯数字 SN，先查 tab_log_job
		if len(q) >= 16 {
			ljRow, _ := QueryRowMap(mesDB, "SELECT sn, task_no FROM tab_log_job WHERE sn = ? LIMIT 1", q)
			if ljRow != nil {
				meta.SN = AsString(ljRow["sn"])
				meta.WorkOrder = AsString(ljRow["task_no"])
			}
		}

		targetWo := q
		if meta.WorkOrder != "" {
			targetWo = meta.WorkOrder
		}

		// 1b. 查 tab_plan（兼容 task_no / mater_no / batch_no）
		planRow, _ := QueryRowMap(mesDB, `
			SELECT task_no, mater_no, mater_desc, task_number, plan_start, plan_end
			FROM tab_plan
			WHERE task_no = ? OR mater_no = ? OR task_no LIKE ?
			ORDER BY id DESC LIMIT 1
		`, targetWo, targetWo, prefix16+"%")
		if planRow == nil && len(prefix16) >= 10 {
			planRow, _ = QueryRowMap(mesDB, `
				SELECT task_no, mater_no, mater_desc, task_number, plan_start, plan_end
				FROM tab_plan
				WHERE batch_no LIKE ? OR task_no LIKE ?
				LIMIT 1
			`, prefix16+"%", "%"+targetWo+"%")
		}
		if planRow != nil {
			meta.WorkOrder = AsString(planRow["task_no"])
			meta.ProductCode = AsString(planRow["mater_no"])
			meta.ProductDesc = AsString(planRow["mater_desc"])
			meta.Quantity = AsFloat(planRow["task_number"])
			meta.PlanStart = AsString(planRow["plan_start"])
			meta.PlanEnd = AsString(planRow["plan_end"])
			meta.BatchNo = prefix16
			meta.BatchPrefix = prefix16
		} else {
			// 1c. 查 tab_job
			jobRow, _ := QueryRowMap(mesDB, `
				SELECT DISTINCT task_no, mater_no, mater_desc, task_number
				FROM tab_job
				WHERE task_no = ? OR mater_no = ?
				LIMIT 1
			`, targetWo, targetWo)
			if jobRow != nil {
				meta.WorkOrder = AsString(jobRow["task_no"])
				meta.ProductCode = AsString(jobRow["mater_no"])
				meta.ProductDesc = AsString(jobRow["mater_desc"])
				meta.Quantity = AsFloat(jobRow["task_number"])
			}
		}
	}

	if db == nil {
		if meta.WorkOrder == "" && meta.ProductCode == "" {
			return nil
		}
		return meta
	}

	// 2. 本地 trace_boxes / trace_tasks 联查
	if meta.WorkOrder == "" {
		boxRow, _ := QueryRowMap(db, `
			SELECT b.box_sn, b.task_order, b.parent_code,
			       t.description, t.quantity, t.batch_no, t.batch_prefix
			FROM trace_boxes b
			LEFT JOIN trace_tasks t ON t.task_order = b.task_order
			WHERE b.box_sn = ? OR b.box_sn LIKE ?
			LIMIT 1
		`, q, q+"%")
		if boxRow != nil {
			meta.SN = AsString(boxRow["box_sn"])
			meta.WorkOrder = AsString(boxRow["task_order"])
			meta.ProductCode = AsString(boxRow["parent_code"])
			meta.ProductDesc = AsString(boxRow["description"])
			meta.Quantity = AsFloat(boxRow["quantity"])
			meta.BatchNo = AsString(boxRow["batch_no"])
			meta.BatchPrefix = AsString(boxRow["batch_prefix"])
		}
	}

	if meta.WorkOrder == "" || meta.BatchNo == "" || meta.ProductDesc == "" {
		taskRow, _ := QueryRowMap(db, `
			SELECT task_order, parent_code, description, quantity, batch_no, batch_prefix
			FROM trace_tasks
			WHERE task_order = ? OR batch_no = ? OR batch_prefix = ? OR parent_code = ?
			   OR (CHAR_LENGTH(?) >= 8 AND (batch_no LIKE ? OR batch_prefix LIKE ? OR task_order LIKE ?))
			ORDER BY id DESC LIMIT 1
		`, q, q, prefix16, q, q, prefix16+"%", prefix16+"%", "%"+q+"%")
		if taskRow != nil {
			if meta.WorkOrder == "" {
				meta.WorkOrder = AsString(taskRow["task_order"])
			}
			if meta.ProductCode == "" {
				meta.ProductCode = AsString(taskRow["parent_code"])
			}
			if meta.ProductDesc == "" {
				meta.ProductDesc = AsString(taskRow["description"])
			}
			if meta.Quantity <= 0 {
				meta.Quantity = AsFloat(taskRow["quantity"])
			}
			if meta.BatchNo == "" {
				meta.BatchNo = AsString(taskRow["batch_no"])
			}
			if meta.BatchPrefix == "" {
				meta.BatchPrefix = AsString(taskRow["batch_prefix"])
			}
		}
	}

	// 3. 本地外协管理表 osm_records 联查（同时关联外协厂家 vendor）
	if meta.WorkOrder == "" || meta.OutsourcedUnit == "" {
		osmRow, _ := QueryRowMap(db, `
			SELECT work_order, product_code, COALESCE(NULLIF(description, ''), product_desc) AS prod_desc,
			       COALESCE(NULLIF(quantity, 0), order_qty) AS qty, batch_no, vendor
			FROM osm_records
			WHERE work_order = ? OR batch_no = ? OR product_code = ? OR work_order = ?
			ORDER BY id DESC LIMIT 1
		`, q, q, q, meta.WorkOrder)
		if osmRow != nil {
			if meta.WorkOrder == "" {
				meta.WorkOrder = AsString(osmRow["work_order"])
			}
			if meta.ProductCode == "" {
				meta.ProductCode = AsString(osmRow["product_code"])
			}
			if meta.ProductDesc == "" {
				meta.ProductDesc = AsString(osmRow["prod_desc"])
			}
			if meta.Quantity <= 0 {
				meta.Quantity = AsFloat(osmRow["qty"])
			}
			if meta.BatchNo == "" {
				meta.BatchNo = AsString(osmRow["batch_no"])
			}
			if meta.OutsourcedUnit == "" {
				meta.OutsourcedUnit = AsString(osmRow["vendor"])
			}
		}
	}

	// 4. 本地首件检 / MAC 申领 / 工作日记兜底
	if meta.WorkOrder == "" {
		fpRow, _ := QueryRowMap(db, `
			SELECT work_order, task_code, description, quantity, batch_no
			FROM module16_records
			WHERE work_order = ? OR batch_no = ? OR task_code = ?
			ORDER BY id DESC LIMIT 1
		`, q, q, q)
		if fpRow != nil {
			meta.WorkOrder = AsString(fpRow["work_order"])
			meta.ProductCode = AsString(fpRow["task_code"])
			meta.ProductDesc = AsString(fpRow["description"])
			meta.Quantity = AsFloat(fpRow["quantity"])
			meta.BatchNo = AsString(fpRow["batch_no"])
		}
	}
	if meta.WorkOrder == "" {
		macRow, _ := QueryRowMap(db, `
			SELECT work_order, task_code, description, order_quantity
			FROM mac_application_records
			WHERE work_order = ? OR task_code = ?
			ORDER BY id DESC LIMIT 1
		`, q, q)
		if macRow != nil {
			meta.WorkOrder = AsString(macRow["work_order"])
			meta.ProductCode = AsString(macRow["task_code"])
			meta.ProductDesc = AsString(macRow["description"])
			meta.Quantity = AsFloat(macRow["order_quantity"])
		}
	}
	if meta.WorkOrder == "" {
		diaryRow, _ := QueryRowMap(db, `
			SELECT task_order, product_code, description, quantity
			FROM work_diaries
			WHERE task_order = ? OR product_code = ?
			ORDER BY id DESC LIMIT 1
		`, q, q)
		if diaryRow != nil {
			meta.WorkOrder = AsString(diaryRow["task_order"])
			meta.ProductCode = AsString(diaryRow["product_code"])
			meta.ProductDesc = AsString(diaryRow["description"])
			meta.Quantity = AsFloat(diaryRow["quantity"])
		}
	}

	// 5. 自动补齐产品描述
	if meta.ProductCode != "" && meta.ProductDesc == "" {
		meta.ProductDesc = LookupProductDesc(db, meta.ProductCode)
	}
	if meta.ProductCode == "" && meta.WorkOrder == "" {
		desc := LookupProductDesc(db, q)
		if desc != "" {
			meta.ProductCode = q
			meta.ProductDesc = desc
		}
	}

	// 6. 自动关联外协厂家
	if meta.WorkOrder != "" && meta.OutsourcedUnit == "" {
		var vendor string
		_ = db.QueryRow("SELECT COALESCE(vendor, '') FROM osm_records WHERE work_order = ? AND vendor <> '' LIMIT 1", meta.WorkOrder).Scan(&vendor)
		meta.OutsourcedUnit = strings.TrimSpace(vendor)
	}

	if meta.BatchPrefix == "" {
		if len(meta.BatchNo) >= 16 {
			meta.BatchPrefix = meta.BatchNo[:16]
		} else if len(q) >= 16 {
			meta.BatchPrefix = q[:16]
		}
	}
	if meta.BatchNo == "" && meta.BatchPrefix != "" {
		meta.BatchNo = meta.BatchPrefix
	}

	if meta.WorkOrder == "" && meta.ProductCode == "" && meta.BatchNo == "" {
		return nil
	}
	return meta
}

// LookupProductDesc 跨模块查询产品编码对应的名称/描述（优先本地，再查外部 MES）。
func LookupProductDesc(db *sql.DB, code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	if db != nil {
		var desc string
		if err := db.QueryRow("SELECT description FROM manhour_summary WHERE code = ? AND description <> '' LIMIT 1", code).Scan(&desc); err == nil && strings.TrimSpace(desc) != "" {
			return strings.TrimSpace(desc)
		}
		if err := db.QueryRow("SELECT description FROM product_port_configs WHERE product_code = ? AND description <> '' LIMIT 1", code).Scan(&desc); err == nil && strings.TrimSpace(desc) != "" {
			return strings.TrimSpace(desc)
		}
		if err := db.QueryRow("SELECT description FROM equipment WHERE product_series = ? AND description <> '' LIMIT 1", code).Scan(&desc); err == nil && strings.TrimSpace(desc) != "" {
			return strings.TrimSpace(desc)
		}
		if err := db.QueryRow("SELECT description FROM trace_tasks WHERE parent_code = ? AND description <> '' ORDER BY id DESC LIMIT 1", code).Scan(&desc); err == nil && strings.TrimSpace(desc) != "" {
			return strings.TrimSpace(desc)
		}
	}
	if mesDB, err := GetDB("mes"); err == nil && mesDB != nil {
		var desc string
		if err := mesDB.QueryRow("SELECT mater_desc FROM tab_job WHERE mater_no = ? AND mater_desc IS NOT NULL AND TRIM(mater_desc) <> '' LIMIT 1", code).Scan(&desc); err == nil && strings.TrimSpace(desc) != "" {
			return strings.TrimSpace(desc)
		}
		if err := mesDB.QueryRow("SELECT mater_desc FROM tab_plan WHERE mater_no = ? AND mater_desc IS NOT NULL AND TRIM(mater_desc) <> '' LIMIT 1", code).Scan(&desc); err == nil && strings.TrimSpace(desc) != "" {
			return strings.TrimSpace(desc)
		}
		if err := mesDB.QueryRow("SELECT `desc` FROM tab_base_mater WHERE CAST(id AS CHAR) = ? AND `desc` IS NOT NULL AND TRIM(`desc`) <> '' LIMIT 1", code).Scan(&desc); err == nil && strings.TrimSpace(desc) != "" {
			return strings.TrimSpace(desc)
		}
	}
	return ""
}

// LookupProductProcesses 查询产品编码对应的工艺路线工序列表（MES tab_craft_route 联查 + 默认标准工序兜底）。
func LookupProductProcesses(code string) []string {
	code = strings.TrimSpace(code)
	if code != "" {
		if mesDB, err := GetDB("mes"); err == nil && mesDB != nil {
			rows, qErr := QueryMaps(mesDB, `
				SELECT DISTINCT g.name AS gx_name, r.seq
				FROM tab_craft_route r
				INNER JOIN tab_base_gx g ON r.gx_id = g.id
				WHERE r.mater_no = ? AND g.name <> '' AND g.name <> '标识打印'
				ORDER BY r.seq ASC
			`, code)
			if qErr == nil && len(rows) > 0 {
				procs := make([]string, 0, len(rows))
				for _, r := range rows {
					name := strings.TrimSpace(AsString(r["gx_name"]))
					if name != "" {
						procs = append(procs, name)
					}
				}
				if len(procs) > 0 {
					return procs
				}
			}
		}
	}
	return []string{"单板装配", "整机组装", "单板初调", "整机调试", "高低温老化", "耐压绝缘", "出厂终检", "装置包装"}
}

package controller

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/core"
	"vite-system-go/internal/service"
)

var reDigitsOnly = regexp.MustCompile(`^\d+$`)

// ============================================================================
//  1. 工单进度与 SN 综合查询模块（ProgressController）—— 1:1 对齐前端与 MES 库
// ============================================================================

type ProgressController struct{}

func NewProgressController() *ProgressController {
	return &ProgressController{}
}

// Query GET|POST /api/progress/query
func (ctrl *ProgressController) Query(c *gin.Context) {
	in := core.ParseInputMap(c)
	query := strings.TrimSpace(core.AsString(in["q"]))
	if query == "" {
		query = strings.TrimSpace(core.AsString(in["keyword"]))
	}
	if query == "" {
		query = strings.TrimSpace(core.AsString(in["work_order"]))
	}
	if query == "" {
		query = strings.TrimSpace(core.AsString(in["sn"]))
	}
	if query == "" {
		core.Error(c, "请输入 SN 或工单号", gin.H{})
		return
	}

	isSn := reDigitsOnly.MatchString(query) && len(query) >= 16

	// 1. 优先尝试连接外部 MES 数据库查询
	if mesDB, err := core.GetDB("mes"); err == nil && mesDB != nil {
		resData, ok, errMsg := ctrl.queryFromMes(mesDB, query, isSn)
		if ok {
			core.Success(c, "success", resData)
			return
		}
		// 若 MES 明确连接成功但未查到，继续尝试本地质量追溯/工单库联查兜底
		_ = errMsg
	}

	// 2. 本地跨模块工单/SN 联查兜底（trace_tasks / trace_boxes / trace_scans / LookupWorkOrderInfo）
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询异常", gin.H{})
		return
	}

	meta := core.LookupWorkOrderInfo(db, query)
	if meta == nil {
		if isSn {
			core.Error(c, "未找到该 SN 的信息，请确认 SN 是否正确", gin.H{})
		} else {
			core.Error(c, "未找到该工单号的信息", gin.H{})
		}
		return
	}

	taskNo := meta.WorkOrder
	if taskNo == "" {
		taskNo = query
	}
	totalSnCount := int(math.Max(meta.Quantity, 1))
	taskInfo := map[string]any{
		"task_no":     taskNo,
		"mater_no":    meta.ProductCode,
		"mater_desc":  meta.ProductDesc,
		"task_number": totalSnCount,
		"plan_start":  meta.PlanStart,
		"plan_end":    meta.PlanEnd,
	}

	var snCurrent any = nil
	snHistory := make([]map[string]any, 0)
	woProgress := make([]map[string]any, 0)

	if isSn {
		boxRow, _ := core.QueryRowMap(db, "SELECT * FROM trace_boxes WHERE box_sn = ? LIMIT 1", query)
		enVal := 1
		updTime := core.NowShanghai().Format("2006-01-02 15:04:05")
		if boxRow != nil {
			if core.AsInt(boxRow["status"]) == 1 {
				enVal = 2
			}
			if ct := core.AsString(boxRow["completed_at"]); ct != "" {
				updTime = ct
			} else if ct := core.AsString(boxRow["created_at"]); ct != "" {
				updTime = ct
			}
		}
		snCurrent = map[string]any{
			"sn":          query,
			"task_no":     taskNo,
			"gx_name":     "质量追溯与整机调试",
			"node_name":   "工位扫码检验",
			"zyzx_name":   "装置装配",
			"update_time": updTime,
			"en":          enVal,
		}
		scans, _ := core.QueryMaps(db, `
			SELECT s.scan_point, s.child_code, s.child_sn, s.scanned_at
			FROM trace_scans s
			INNER JOIN trace_boxes b ON b.id = s.box_id
			WHERE b.box_sn = ?
			ORDER BY s.scan_point ASC, s.id ASC
		`, query)
		for _, s := range scans {
			snHistory = append(snHistory, map[string]any{
				"gx_id":     core.AsInt(s["scan_point"]),
				"gx_name":   fmt.Sprintf("追溯扫码点 %d", core.AsInt(s["scan_point"])),
				"gy_name":   fmt.Sprintf("物料 %s (%s)", core.AsString(s["child_code"]), core.AsString(s["child_sn"])),
				"result":    1,
				"last_time": core.AsString(s["scanned_at"]),
			})
		}
	}

	var completedBoxes int
	_ = db.QueryRow("SELECT COUNT(*) FROM trace_boxes WHERE task_order = ? AND status = 1", taskNo).Scan(&completedBoxes)
	woProgress = append(woProgress,
		map[string]any{"gx_name": "装置装配", "completed_count": completedBoxes},
		map[string]any{"gx_name": "装置调试", "completed_count": completedBoxes},
		map[string]any{"gx_name": "装置检验", "completed_count": completedBoxes},
		map[string]any{"gx_name": "装置包装", "completed_count": completedBoxes},
	)

	core.Success(c, "success", gin.H{
		"is_sn":               isSn,
		"task_info":           taskInfo,
		"sn_current":          snCurrent,
		"sn_history":          snHistory,
		"work_order_progress": woProgress,
		"total_sn_count":      totalSnCount,
	})
}

func (ctrl *ProgressController) queryFromMes(mesDB *sql.DB, query string, isSn bool) (gin.H, bool, string) {
	var snCurrent map[string]any
	var taskInfo map[string]any
	snHistory := make([]map[string]any, 0)
	woProgress := make([]map[string]any, 0)
	totalSnCount := 0
	var taskNo string

	if isSn {
		snCurrent, _ = core.QueryRowMap(mesDB, `
			SELECT lj.*, gx.name AS gx_name, node.name AS node_name, zy.name AS zyzx_name
			FROM tab_log_job lj
			LEFT JOIN tab_base_gx gx ON lj.gx_id = gx.id
			LEFT JOIN tab_base_node node ON lj.node_id = node.id
			LEFT JOIN tab_base_zyzx zy ON lj.zyzx_id = zy.id
			WHERE lj.sn = ?
			ORDER BY lj.update_time DESC LIMIT 1
		`, query)
		if snCurrent == nil {
			return nil, false, "未找到该 SN 的信息"
		}
		taskNo = core.AsString(snCurrent["task_no"])
		taskInfo, _ = core.QueryRowMap(mesDB, "SELECT task_no, mater_no, mater_desc, task_number, plan_start, plan_end FROM tab_plan WHERE task_no = ? LIMIT 1", taskNo)
		if taskInfo == nil {
			taskInfo, _ = core.QueryRowMap(mesDB, "SELECT DISTINCT task_no, mater_no, mater_desc, task_number FROM tab_job WHERE task_no = ? LIMIT 1", taskNo)
		}
		snHistory, _ = core.QueryMaps(mesDB, `
			SELECT ls.gx_id, gx.name AS gx_name, ls.gy_name, ls.result, ls.update_time AS last_time
			FROM tab_log_step ls
			INNER JOIN (
				SELECT sn, gx_id, MAX(id) AS max_id
				FROM tab_log_step
				WHERE sn = ?
				GROUP BY sn, gx_id
			) latest ON ls.id = latest.max_id
			JOIN tab_base_gx gx ON ls.gx_id = gx.id
			JOIN tab_base_zyzx zy ON gx.zyzx_id = zy.id
			WHERE zy.name IN ('装置装配', '装置调试', '装置包装', '装置检验')
			  AND gx.name <> '标识打印'
			ORDER BY ls.update_time ASC, ls.id ASC
		`, query)
	} else {
		taskNo = query
		taskInfo, _ = core.QueryRowMap(mesDB, "SELECT task_no, mater_no, mater_desc, task_number, plan_start, plan_end FROM tab_plan WHERE task_no = ? LIMIT 1", taskNo)
		if taskInfo == nil {
			taskInfo, _ = core.QueryRowMap(mesDB, "SELECT DISTINCT task_no, mater_no, mater_desc, task_number FROM tab_job WHERE task_no = ? LIMIT 1", taskNo)
		}
		if taskInfo == nil {
			return nil, false, "未找到该工单号的信息"
		}
	}

	if taskInfo != nil {
		totalSnCount = core.AsInt(taskInfo["task_number"])
	}
	if totalSnCount <= 0 && taskNo != "" {
		_ = mesDB.QueryRow("SELECT COUNT(DISTINCT sn) FROM tab_log_job WHERE task_no = ?", taskNo).Scan(&totalSnCount)
	}

	if taskInfo != nil && taskNo != "" && totalSnCount > 0 {
		routeProcs, _ := core.QueryMaps(mesDB, `
			SELECT j.gx_id, MIN(j.seq) AS seq, gx.name AS gx_name
			FROM tab_job j
			JOIN tab_base_gx gx ON j.gx_id = gx.id
			JOIN tab_base_zyzx zy ON j.zyzx_id = zy.id
			WHERE j.task_no = ?
			  AND zy.name IN ('装置装配', '装置调试', '装置包装', '装置检验')
			  AND gx.name <> '标识打印'
			GROUP BY j.gx_id, gx.name
			ORDER BY seq ASC
		`, taskNo)

		if len(routeProcs) > 0 {
			progRows, _ := core.QueryMaps(mesDB, `
				SELECT ls.gx_id, COUNT(DISTINCT ls.sn) AS completed_count
				FROM tab_log_step ls
				JOIN tab_base_node node ON ls.node_id = node.id
				WHERE ls.task_no = ?
				  AND ls.result = 1
				  AND node.pos = 2
				  AND ls.sn IN (SELECT DISTINCT sn FROM tab_log_job WHERE task_no = ?)
				GROUP BY ls.gx_id
			`, taskNo, taskNo)
			progMap := make(map[int]int)
			for _, pr := range progRows {
				progMap[core.AsInt(pr["gx_id"])] = core.AsInt(pr["completed_count"])
			}
			for _, rp := range routeProcs {
				gxID := core.AsInt(rp["gx_id"])
				woProgress = append(woProgress, map[string]any{
					"gx_name":         rp["gx_name"],
					"completed_count": progMap[gxID],
				})
			}
			if isSn && snCurrent != nil {
				curGxID := core.AsInt(snCurrent["gx_id"])
				if curGxID > 0 {
					if progMap[curGxID] >= totalSnCount {
						snCurrent["en"] = 2
					} else {
						var hit int
						if err := mesDB.QueryRow(`
							SELECT 1 FROM tab_log_step ls
							JOIN tab_base_node node ON ls.node_id = node.id
							WHERE ls.sn = ? AND ls.gx_id = ? AND node.pos = 2 AND ls.result = 1
							LIMIT 1
						`, query, curGxID).Scan(&hit); err == nil && hit == 1 {
							snCurrent["en"] = 2
						}
					}
				}
			}
		}
	}

	return gin.H{
		"is_sn":               isSn,
		"task_info":           taskInfo,
		"sn_current":          snCurrent,
		"sn_history":          snHistory,
		"work_order_progress": woProgress,
		"total_sn_count":      totalSnCount,
	}, true, ""
}

// ============================================================================
//  2. 工单齐套查询模块（KittingController）
// ============================================================================

type KittingController struct{}

func NewKittingController() *KittingController {
	return &KittingController{}
}

var reDigitsOrHyphen = regexp.MustCompile(`^[-0-9]+$`)

func getExactColName(columns []string, targets []string) string {
	for _, target := range targets {
		for _, col := range columns {
			if strings.TrimSpace(col) == target {
				return col
			}
		}
	}
	for _, target := range targets {
		for _, col := range columns {
			if strings.Contains(col, target) {
				return col
			}
		}
	}
	return ""
}

func resolveKittingProcess(procName, procNo, procShort string) string {
	name := strings.TrimSpace(procName)
	if name != "" && name != "-" && !reDigitsOrHyphen.MatchString(name) {
		return name
	}
	no := strings.TrimSpace(procNo)
	if strings.Contains(no, "30") {
		return "装置装配"
	}
	if strings.Contains(no, "40") {
		return "装置调试"
	}
	if strings.Contains(no, "60") {
		return "装置包装"
	}

	short := strings.TrimSpace(procShort)
	if short != "" {
		if strings.Contains(short, "装配") {
			return "装置装配"
		}
		if strings.Contains(short, "包装") {
			return "装置包装"
		}
		if strings.Contains(short, "调试") {
			return "装置调试"
		}
		if short == "装置" {
			return "装置装配"
		}
		return short
	}
	if name != "" {
		return name
	}
	return "装置装配"
}

// Query GET|POST /api/kitting/query
func (ctrl *KittingController) Query(c *gin.Context) {
	in := core.ParseInputMap(c)
	woRaw := strings.TrimSpace(core.AsString(in["work_orders"]))
	if woRaw == "" {
		woRaw = strings.TrimSpace(core.AsString(in["work_order"]))
	}
	if woRaw == "" {
		woRaw = strings.TrimSpace(core.AsString(in["q"]))
	}
	if woRaw == "" {
		core.Error(c, "请输入工单号/任务号！", gin.H{})
		return
	}

	tokens := strings.FieldsFunc(woRaw, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})

	cleanOrders := make([]string, 0)
	seenOrders := make(map[string]bool)
	for _, wo := range tokens {
		wo = strings.TrimSpace(wo)
		if wo != "" && !seenOrders[wo] {
			seenOrders[wo] = true
			cleanOrders = append(cleanOrders, wo)
		}
	}
	if len(cleanOrders) == 0 {
		core.Error(c, "请输入有效的任务号！", gin.H{})
		return
	}
	workOrdersParam := strings.Join(cleanOrders, ",")

	// 1. 优先直连外部 SQL Server ERP (172.18.10.33:1433/source) 调用存储过程 [dbo].[自定义ERP组件_P]
	if sqlsrvDB, sErr := core.GetDB("sqlsrv"); sErr == nil && sqlsrvDB != nil {
		rows, qErr := sqlsrvDB.Query("SET NOCOUNT ON; SET ANSI_NULLS ON; SET ANSI_WARNINGS ON; EXEC [dbo].[自定义ERP组件_P] @WorkOrders = @p1", workOrdersParam)
		if qErr == nil && rows != nil {
			defer rows.Close()
			cols, _ := rows.Columns()
			for len(cols) == 0 && rows.NextResultSet() {
				cols, _ = rows.Columns()
			}
			if len(cols) > 0 {
				rawResults := make([]map[string]any, 0)
				for rows.Next() {
					values := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range values {
						ptrs[i] = &values[i]
					}
					if err := rows.Scan(ptrs...); err == nil {
						rowMap := make(map[string]any, len(cols))
						for i, cName := range cols {
							val := values[i]
							if b, ok := val.([]byte); ok {
								rowMap[cName] = string(b)
							} else {
								rowMap[cName] = val
							}
						}
						rawResults = append(rawResults, rowMap)
					}
				}

				if len(rawResults) > 0 {
					colWo := getExactColName(cols, []string{"工单号", "任务号", "工单"})
					if colWo == "" {
						colWo = cols[0]
					}
					colProcName := getExactColName(cols, []string{"工序名称"})
					colProcNo := getExactColName(cols, []string{"工序号"})
					colProcShort := getExactColName(cols, []string{"工序名"})
					colReqQty := getExactColName(cols, []string{"需求量", "需求数量"})
					colSendQty := getExactColName(cols, []string{"已发料", "已发料数量", "实发数量", "完成量"})

					results := make([]map[string]any, 0, len(rawResults))
					processList := make(map[string]bool)
					type summaryItem struct {
						wo       string
						process  string
						totalMat int
						kitted   int
						unkitted int
						unissued int
					}
					summaryData := make(map[string]*summaryItem)

					for _, row := range rawResults {
						procNoVal := strings.TrimSpace(core.AsString(row[colProcNo]))
						if procNoVal != "" && (strings.Contains(procNoVal, "-") || core.AsFloat(procNoVal) < 0) {
							continue
						}
						wo := strings.TrimSpace(core.AsString(row[colWo]))
						if wo == "" {
							wo = "通用工单"
						}
						proc := resolveKittingProcess(
							core.AsString(row[colProcName]),
							procNoVal,
							core.AsString(row[colProcShort]),
						)
						reqQty := core.AsFloat(row[colReqQty])
						sendQty := core.AsFloat(row[colSendQty])
						targetQty := math.Ceil(reqQty)

						rowState := "unissued"
						if (sendQty >= targetQty || sendQty >= reqQty) && reqQty > 0 {
							rowState = "kitted"
						} else if sendQty > 0 {
							rowState = "unkitted"
						} else {
							rowState = "unissued"
						}

						diffQty := 0.0
						if rowState == "unkitted" {
							diffQty = math.Round((targetQty-sendQty)*10000) / 10000
							if diffQty <= 0 {
								diffQty = math.Round((reqQty-sendQty)*10000) / 10000
							}
						}
						if diffQty < 0 {
							diffQty = 0
						}

						row["_calc_state"] = rowState
						row["_calc_process"] = proc
						row["_calc_wo"] = wo
						row["_diff_qty"] = diffQty
						results = append(results, row)

						processList[proc] = true

						groupKey := wo + "___" + proc
						item, ok := summaryData[groupKey]
						if !ok {
							item = &summaryItem{
								wo:      wo,
								process: proc,
							}
							summaryData[groupKey] = item
						}
						item.totalMat++
						switch rowState {
						case "kitted":
							item.kitted++
						case "unkitted":
							item.unkitted++
						case "unissued":
							item.unissued++
						}
					}

					summaryList := make([]map[string]any, 0, len(summaryData))
					for _, item := range summaryData {
						rate := 0.0
						if item.totalMat > 0 {
							rate = math.Round(float64(item.kitted)/float64(item.totalMat)*1000) / 10
						}
						stateBadge := "unkitted"
						if item.kitted == item.totalMat && item.totalMat > 0 {
							stateBadge = "kitted"
						} else if item.unissued == item.totalMat {
							stateBadge = "unissued"
						}
						summaryList = append(summaryList, map[string]any{
							"wo":          item.wo,
							"process":     item.process,
							"total_mat":   item.totalMat,
							"kitted":      item.kitted,
							"unkitted":    item.unkitted,
							"unissued":    item.unissued,
							"rate":        rate,
							"state_badge": stateBadge,
						})
					}

					procKeys := make([]string, 0, len(processList))
					for p := range processList {
						procKeys = append(procKeys, p)
					}

					core.Success(c, "success", gin.H{
						"columns":      cols,
						"materials":    results,
						"summary_list": summaryList,
						"processes":    procKeys,
						"total_count":  len(results),
					})
					return
				}
			}
		}
	}

	// 2. 本地兜底逻辑（若 SQL Server 外部数据源未就绪或未查到）
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询工单齐套失败", gin.H{})
		return
	}

	columns := []string{"工单号", "物料编码", "物料描述", "工序", "需求量", "已发料", "缺料数", "齐套状态"}
	processes := []string{"装置装配", "装置调试", "装置包装"}
	materials := make([]map[string]any, 0)
	summaryMap := make(map[string]map[string]int)
	for _, p := range processes {
		summaryMap[p] = map[string]int{"total": 0, "ready": 0}
	}

	for _, wo := range cleanOrders {
		meta := core.LookupWorkOrderInfo(db, wo)
		orderQty := 1.0
		if meta != nil && meta.Quantity > 0 {
			orderQty = meta.Quantity
		}
		boms, _ := core.QueryMaps(db, "SELECT * FROM trace_bom_config WHERE task_order = ? ORDER BY scan_point ASC, id ASC", wo)
		for _, b := range boms {
			uQty := core.AsFloat(b["unit_quantity"])
			if uQty <= 0 {
				uQty = 1
			}
			reqQty := uQty * orderQty
			procName := "装置装配"
			summaryMap[procName]["total"]++
			summaryMap[procName]["ready"]++
			materials = append(materials, map[string]any{
				"工单号":           wo,
				"物料编码":          b["child_code"],
				"物料描述":          b["child_name"],
				"工序":            procName,
				"需求量":           reqQty,
				"已发料":           reqQty,
				"缺料数":           0,
				"齐套状态":          "已齐套",
				"_calc_state":   "kitted",
				"_calc_process": procName,
				"_calc_wo":      wo,
				"_diff_qty":     0,
				"_color":        "green",
			})
		}
	}

	summaryList := make([]map[string]any, 0, len(processes))
	for _, p := range processes {
		tot := summaryMap[p]["total"]
		rdy := summaryMap[p]["ready"]
		rate := 100.0
		if tot > 0 {
			rate = math.Round(float64(rdy)/float64(tot)*10000) / 100
		}
		summaryList = append(summaryList, map[string]any{
			"wo":          cleanOrders[0],
			"process":     p,
			"total_mat":   tot,
			"kitted":      rdy,
			"unkitted":    0,
			"unissued":    0,
			"rate":        rate,
			"state_badge": "kitted",
		})
	}

	core.Success(c, "查询成功", gin.H{
		"materials":    materials,
		"columns":      columns,
		"summary_list": summaryList,
		"processes":    processes,
		"total_count":  len(materials),
	})
}


type DispatchKittingController struct{}

func NewDispatchKittingController() *DispatchKittingController {
	return &DispatchKittingController{}
}

func isPackingProcess(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	return strings.Contains(name, "包装") || strings.Contains(strings.ToLower(name), "pack")
}

func fetchBomInfoByWos(sqlsrvDB *sql.DB, woList []string) map[string]map[string]map[string]any {
	result := make(map[string]map[string]map[string]any)
	if len(woList) == 0 || sqlsrvDB == nil {
		return result
	}

	chunks := make([][]string, 0)
	for i := 0; i < len(woList); i += 50 {
		end := i + 50
		if end > len(woList) {
			end = len(woList)
		}
		chunks = append(chunks, woList[i:end])
	}

	for _, chunk := range chunks {
		wosParam := strings.Join(chunk, ",")
		rows, qErr := sqlsrvDB.Query("SET NOCOUNT ON; SET ANSI_NULLS ON; SET ANSI_WARNINGS ON; EXEC [dbo].[自定义ERP组件_P] @WorkOrders = @p1", wosParam)
		if qErr != nil || rows == nil {
			continue
		}
		cols, _ := rows.Columns()
		for len(cols) == 0 && rows.NextResultSet() {
			cols, _ = rows.Columns()
		}
		if len(cols) == 0 {
			rows.Close()
			continue
		}

		cBWo := getExactColName(cols, []string{"工单号", "任务号", "工单"})
		if cBWo == "" {
			cBWo = cols[0]
		}
		cBProcName := getExactColName(cols, []string{"工序名称"})
		cBProcNo := getExactColName(cols, []string{"工序号"})
		cBProcShort := getExactColName(cols, []string{"工序名"})
		cBMat := getExactColName(cols, []string{"组件编码", "物料代码", "物料编码", "料号", "mat_no", "item_no"})
		cBDesc := getExactColName(cols, []string{"组件描述", "物料名称", "物料描述", "子项名称", "mat_desc", "item_name"})
		cBReq := getExactColName(cols, []string{"需求量", "需求数量", "数量", "qty"})

		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				continue
			}
			bRow := make(map[string]any, len(cols))
			for i, cName := range cols {
				val := values[i]
				if b, ok := val.([]byte); ok {
					bRow[cName] = string(b)
				} else {
					bRow[cName] = val
				}
			}

			wo := strings.TrimSpace(core.AsString(bRow[cBWo]))
			mat := strings.TrimSpace(core.AsString(bRow[cBMat]))
			if wo == "" || mat == "" {
				continue
			}

			procNameRaw := core.AsString(bRow[cBProcName])
			procNoRaw := core.AsString(bRow[cBProcNo])
			procShortRaw := core.AsString(bRow[cBProcShort])
			if procNoRaw != "" && (strings.Contains(procNoRaw, "-") || core.AsFloat(procNoRaw) < 0) {
				continue
			}

			realProc := resolveKittingProcess(procNameRaw, procNoRaw, procShortRaw)
			desc := core.AsString(bRow[cBDesc])
			req := core.AsFloat(bRow[cBReq])
			hasProc := (procNameRaw != "" || procNoRaw != "" || procShortRaw != "")

			if result[wo] == nil {
				result[wo] = make(map[string]map[string]any)
			}
			if result[wo][mat] == nil {
				result[wo][mat] = map[string]any{
					"mater_desc": desc,
					"proc_name":  realProc,
					"has_proc":   hasProc,
					"req_qty":    req,
				}
			} else {
				result[wo][mat]["req_qty"] = core.AsFloat(result[wo][mat]["req_qty"]) + req
				if core.AsString(result[wo][mat]["mater_desc"]) == "" && desc != "" {
					result[wo][mat]["mater_desc"] = desc
				}
				if realProc != "" && isPackingProcess(realProc) {
					result[wo][mat]["proc_name"] = realProc
					result[wo][mat]["has_proc"] = true
				} else if core.AsString(result[wo][mat]["proc_name"]) == "" && realProc != "" {
					result[wo][mat]["proc_name"] = realProc
					result[wo][mat]["has_proc"] = hasProc
				}
			}
		}
		rows.Close()
	}
	return result
}

// Query GET|POST /api/dispatch_kitting/query
func (ctrl *DispatchKittingController) Query(c *gin.Context) {
	in := core.ParseInputMap(c)
	peisongOrder := strings.TrimSpace(core.AsString(in["peisong_order"]))
	billCode := strings.TrimSpace(core.AsString(in["bill_code"]))

	if peisongOrder == "" && billCode == "" {
		core.Error(c, "请输入配送单号或工单号！", gin.H{})
		return
	}

	viewMode := "by_peisong"
	searchVal := peisongOrder
	if peisongOrder == "" {
		viewMode = "by_bill"
		searchVal = billCode
	}

	db, _ := core.GetDB()
	confirmedBatchMap := make(map[string]bool)
	if db != nil {
		rows, _ := db.Query("SELECT peisong_order FROM tab_delivery_batch")
		if rows != nil {
			for rows.Next() {
				var p string
				if rows.Scan(&p) == nil && p != "" {
					confirmedBatchMap[p] = true
				}
			}
			rows.Close()
		}
	}

	// 1. 直连外部 SQL Server ERP 查询配送单/工单物料明细
	if sqlsrvDB, sErr := core.GetDB("sqlsrv"); sErr == nil && sqlsrvDB != nil {
		var sqlProc string
		if viewMode == "by_peisong" {
			sqlProc = "SET NOCOUNT ON; SET ANSI_NULLS ON; SET ANSI_WARNINGS ON; EXEC [dbo].[自定义制单查询_P] @peisong_order = @p1"
		} else {
			sqlProc = "SET NOCOUNT ON; SET ANSI_NULLS ON; SET ANSI_WARNINGS ON; EXEC [dbo].[自定义制单查询_P] @bill_code = @p1"
		}

		rows, qErr := sqlsrvDB.Query(sqlProc, searchVal)
		if qErr == nil && rows != nil {
			defer rows.Close()
			cols, _ := rows.Columns()
			for len(cols) == 0 && rows.NextResultSet() {
				cols, _ = rows.Columns()
			}

			if len(cols) > 0 {
				rawResults := make([]map[string]any, 0)
				for rows.Next() {
					values := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range values {
						ptrs[i] = &values[i]
					}
					if err := rows.Scan(ptrs...); err == nil {
						rowMap := make(map[string]any, len(cols))
						for i, cName := range cols {
							val := values[i]
							if b, ok := val.([]byte); ok {
								rowMap[cName] = string(b)
							} else {
								rowMap[cName] = val
							}
						}
						rawResults = append(rawResults, rowMap)
					}
				}

				if len(rawResults) > 0 {
					colPeisong := getExactColName(cols, []string{"配送单号", "配送单", "peisong", "ps_no"})
					colBill := getExactColName(cols, []string{"工单号", "任务号", "工单", "bill", "mo", "wo"})
					colMatNo := getExactColName(cols, []string{"组件编码", "物料编码", "物料代码", "料号", "mat_no", "item_no"})
					colMatDesc := getExactColName(cols, []string{"组件描述", "物料描述", "物料名称", "子项名称", "规格", "品名", "mat_desc"})
					colQty := getExactColName(cols, []string{"数量", "配送数量", "实发数量", "qty", "send_qty"})

					woSet := make(map[string]bool)
					for _, r := range rawResults {
						if colBill != "" {
							w := strings.TrimSpace(core.AsString(r[colBill]))
							if w != "" {
								woSet[w] = true
							}
						}
					}
					woList := make([]string, 0, len(woSet))
					for w := range woSet {
						woList = append(woList, w)
					}
					woMatInfo := fetchBomInfoByWos(sqlsrvDB, woList)

					groups := make(map[string]map[string]any)
					matchedWoList := make(map[string]bool)
					filteredCount := 0

					for _, r := range rawResults {
						pVal := strings.TrimSpace(core.AsString(r[colPeisong]))
						bVal := strings.TrimSpace(core.AsString(r[colBill]))
						matNo := strings.TrimSpace(core.AsString(r[colMatNo]))
						matDesc := strings.TrimSpace(core.AsString(r[colMatDesc]))
						if pVal == "" {
							pVal = "未知配送单"
						}
						if bVal == "" {
							bVal = "未知工单"
						}

						realProc := ""
						if bVal != "未知工单" && matNo != "" && woMatInfo[bVal] != nil && woMatInfo[bVal][matNo] != nil {
							info := woMatInfo[bVal][matNo]
							if matDesc == "" && core.AsString(info["mater_desc"]) != "" {
								matDesc = core.AsString(info["mater_desc"])
							}
							if info["has_proc"] == true {
								realProc = core.AsString(info["proc_name"])
							}
						}

						// 若明确有工序且不是包装工序，则过滤；若无工序或本身是包装工序，均纳入
						if realProc != "" && !isPackingProcess(realProc) {
							filteredCount++
							continue
						}

						matchedWoList[bVal] = true
						groupKey := bVal
						if viewMode != "by_peisong" {
							groupKey = pVal
						}
						if groups[groupKey] == nil {
							groups[groupKey] = map[string]any{
								"group_code":    groupKey,
								"peisong_order": pVal,
								"wo":            bVal,
								"is_confirmed":  0,
								"items":         make([]map[string]any, 0),
							}
							if confirmedBatchMap[pVal] {
								groups[groupKey]["is_confirmed"] = 1
							}
						}

						qtyVal := core.AsFloat(r[colQty])
						dispProc := realProc
						if dispProc == "" {
							dispProc = "装置包装"
						}
						items := groups[groupKey]["items"].([]map[string]any)
						groups[groupKey]["items"] = append(items, map[string]any{
							"peisong_order": pVal,
							"wo":            bVal,
							"mater_no":      matNo,
							"mater_desc":    matDesc,
							"proc_name":     dispProc,
							"send_qty":      qtyVal,
						})
					}

					if len(groups) == 0 {
						core.Error(c, "该单号下未发现属于【装置包装】工序的配送物料！", gin.H{
							"filtered_count": filteredCount,
						})
						return
					}

					groupList := make([]map[string]any, 0, len(groups))
					for _, g := range groups {
						groupList = append(groupList, g)
					}
					relatedWos := make([]string, 0, len(matchedWoList))
					for w := range matchedWoList {
						relatedWos = append(relatedWos, w)
					}

					core.Success(c, "success", gin.H{
						"view_mode":      viewMode,
						"columns":        cols,
						"groups":         groupList,
						"related_wos":    relatedWos,
						"total_groups":   len(groupList),
						"filtered_count": filteredCount,
					})
					return
				}
			}
		}
	}

	// 2. 本地已确认批次兜底展示
	if db != nil {
		where := "1=1"
		args := []any{}
		if peisongOrder != "" {
			where = "peisong_order LIKE ?"
			args = append(args, "%"+peisongOrder+"%")
		} else if billCode != "" {
			where = "wo LIKE ?"
			args = append(args, "%"+billCode+"%")
		}

		batches, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM tab_delivery_batch WHERE %s ORDER BY id DESC LIMIT 50", where), args...)
		if len(batches) > 0 {
			groups := make([]map[string]any, 0)
			for _, b := range batches {
				psOrder := core.AsString(b["peisong_order"])
				wo := core.AsString(b["wo"])
				items, _ := core.QueryMaps(db, "SELECT mater_no, mater_desc, send_qty, confirm_user, confirm_time FROM tab_delivery_receipt WHERE peisong_order = ?", psOrder)
				groups = append(groups, map[string]any{
					"group_code":    wo,
					"peisong_order": psOrder,
					"wo":            wo,
					"is_confirmed":  1,
					"confirm_user":  b["confirm_user"],
					"confirm_time":  b["created_at"],
					"items":         items,
				})
			}
			core.Success(c, "success", gin.H{
				"view_mode":    viewMode,
				"groups":       groups,
				"total_groups": len(groups),
			})
			return
		}
	}

	core.Error(c, "未检索到属于【装置包装】工序的配送物料！", gin.H{})
}

// ConfirmReceipt POST /api/dispatch_kitting/confirm_receipt
func (ctrl *DispatchKittingController) ConfirmReceipt(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "收料确认失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	psOrder := strings.TrimSpace(core.AsString(in["peisong_order"]))
	wo := strings.TrimSpace(core.AsString(in["wo"]))
	if psOrder == "" {
		psOrder = "PS-" + wo
	}
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = core.AsString(sess.User["display_name"])
	}
	if opName == "" {
		opName = sess.Username
	}

	tx, txErr := db.Begin()
	if txErr != nil {
		core.Exception(c, txErr, "数据库开启事务失败", gin.H{})
		return
	}
	defer tx.Rollback()

	_, _ = tx.Exec("INSERT INTO tab_delivery_batch (peisong_order, wo, status, confirm_user, created_at) VALUES (?, ?, 1, ?, NOW()) ON DUPLICATE KEY UPDATE confirm_user = VALUES(confirm_user), created_at = NOW()",
		psOrder, wo, opName)

	if items, ok := in["items"].([]any); ok && len(items) > 0 {
		_, _ = tx.Exec("DELETE FROM tab_delivery_receipt WHERE peisong_order = ?", psOrder)
		for _, itRaw := range items {
			if it, ok := itRaw.(map[string]any); ok {
				mNo := strings.TrimSpace(core.AsString(it["mater_no"]))
				mDesc := strings.TrimSpace(core.AsString(it["mater_desc"]))
				sQty := core.AsFloat(it["send_qty"])
				iWo := strings.TrimSpace(core.AsString(it["wo"]))
				if iWo == "" {
					iWo = wo
				}
				if mNo != "" {
					_, _ = tx.Exec("INSERT INTO tab_delivery_receipt (peisong_order, wo, mater_no, mater_desc, send_qty, confirm_user, confirm_time) VALUES (?, ?, ?, ?, ?, ?, NOW())",
						psOrder, iWo, mNo, mDesc, sQty, opName)
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		core.Exception(c, err, "确认收料保存失败", gin.H{})
		return
	}
	core.Success(c, "确认收料成功！")
}

// QueryWoKitting GET|POST /api/dispatch_kitting/query_wo_kitting
func (ctrl *DispatchKittingController) QueryWoKitting(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "加载齐套看板失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["wo"]))
	where := "1=1"
	args := []any{}
	if wo != "" {
		where = "task_order LIKE ?"
		args = append(args, "%"+wo+"%")
	}
	tasks, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM trace_tasks WHERE %s ORDER BY id DESC LIMIT 50", where), args...)
	list := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		tOrder := core.AsString(t["task_order"])
		var packDone int
		_ = db.QueryRow("SELECT done_qty FROM tab_wo_packing_progress WHERE wo = ?", tOrder).Scan(&packDone)

		pQty := core.AsInt(t["quantity"])
		statusColor := "white"
		if pQty > 0 && packDone >= pQty {
			statusColor = "green"
		} else if packDone > 0 {
			statusColor = "yellow"
		}

		list = append(list, map[string]any{
			"wo":                tOrder,
			"mater_code":        t["parent_code"],
			"mater_desc":        t["description"],
			"plan_qty":          pQty,
			"done_qty":          packDone,
			"wo_status":         statusColor,
			"is_archived":       0,
			"last_confirm_user": "",
			"last_confirm_time": "",
		})
	}
	core.Success(c, "success", gin.H{
		"is_archived": false,
		"list":        list,
	})
}

// QueryWoMaterials GET|POST /api/dispatch_kitting/query_wo_materials
func (ctrl *DispatchKittingController) QueryWoMaterials(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "加载物料明细失败", gin.H{})
		return
	}
	wo := strings.TrimSpace(c.Query("wo"))
	items, _ := core.QueryMaps(db, "SELECT mater_no, mater_desc, SUM(send_qty) AS received_qty FROM tab_delivery_receipt WHERE wo = ? GROUP BY mater_no, mater_desc", wo)
	core.Success(c, "success", gin.H{
		"wo":    wo,
		"items": items,
	})
}



type LabelPrintController struct{}

func NewLabelPrintController() *LabelPrintController {
	return &LabelPrintController{}
}

// QueryOrder GET|POST /api/label_print/query_order
func (ctrl *LabelPrintController) QueryOrder(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询工单失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	orderNo := strings.TrimSpace(core.AsString(in["order_number"]))
	if orderNo == "" {
		orderNo = strings.TrimSpace(core.AsString(in["work_order"]))
	}
	if orderNo == "" {
		orderNo = strings.TrimSpace(core.AsString(in["task_order"]))
	}
	if orderNo == "" {
		core.Error(c, "请输入工单号", gin.H{})
		return
	}

	meta := core.LookupWorkOrderInfo(db, orderNo)
	if meta == nil {
		core.Error(c, "未查询到该工单信息", gin.H{})
		return
	}

	templates := []map[string]string{
		{"name": "标准装置铭牌标签模板.btw", "path": "templates/standard_device.btw"},
		{"name": "包装外箱条码标签模板.btw", "path": "templates/outer_box.btw"},
	}

	core.Success(c, "查询成功", gin.H{
		"order_number": meta.WorkOrder,
		"product_code": meta.ProductCode,
		"description":  meta.ProductDesc,
		"batch_number": meta.BatchNo,
		"quantity":     int(math.Max(meta.Quantity, 1)),
		"templates":    templates,
	})
}

// ExecutePrint POST /api/label_print/execute_print
func (ctrl *LabelPrintController) ExecutePrint(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "执行打印失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	orderNo := strings.TrimSpace(core.AsString(in["order_number"]))
	tplPath := strings.TrimSpace(core.AsString(in["template_path"]))
	prodCode := strings.TrimSpace(core.AsString(in["product_code"]))
	startSerial := strings.TrimSpace(core.AsString(in["start_serial"]))
	qty := core.AsInt(in["quantity"])
	if qty <= 0 {
		qty = 1
	}
	forcePrint := core.AsInt(in["force_print"]) == 1

	if !forcePrint {
		var cnt int
		_ = db.QueryRow("SELECT COUNT(*) FROM print_records WHERE order_number = ?", orderNo).Scan(&cnt)
		if cnt > 0 {
			core.Success(c, "需要确认", gin.H{
				"need_confirm": true,
				"message":      fmt.Sprintf("工单【%s】已有 %d 次打印记录，是否继续重新打印？", orderNo, cnt),
			})
			return
		}
	}

	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	rawJSON, _ := json.Marshal(in)
	_, err = db.Exec(`
		INSERT INTO print_records (order_number, template_code, serial_number, template_path, quantity, print_data, printed_at, printed_by)
		VALUES (?, ?, ?, ?, ?, ?, NOW(), ?)
	`, orderNo, prodCode, startSerial, tplPath, qty, string(rawJSON), opName)
	if err != nil {
		core.Exception(c, err, "保存打印记录失败", gin.H{})
		return
	}
	service.LogAudit(c, "标签打印管理", fmt.Sprintf("打印工单 %s (%d张)", orderNo, qty), true, "", 0)
	core.Success(c, "打印任务已成功发送！")
}

// ListHistory GET /api/label_print/list_history
func (ctrl *LabelPrintController) ListHistory(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取打印历史失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	search := strings.TrimSpace(core.AsString(in["search"]))

	where := "1=1"
	args := []any{}
	if search != "" {
		where = "(order_number LIKE ? OR template_code LIKE ? OR serial_number LIKE ? OR printed_by LIKE ?)"
		k := "%" + search + "%"
		args = append(args, k, k, k, k)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM print_records WHERE "+where, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM print_records WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", where),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
	})
}

// ============================================================================
//  5. 设备矩阵总览模块（EquipmentController）—— 1:1 对齐 equipment & equipment_processes
// ============================================================================

type EquipmentController struct{}

func NewEquipmentController() *EquipmentController {
	return &EquipmentController{}
}

// Init GET /api/equipment/init
func (ctrl *EquipmentController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	core.Success(c, "success", gin.H{
		"processes":  core.LookupProductProcesses(""),
		"can_manage": core.CanEditModule(sess.User, "equipment"),
		"is_admin":   core.IsAdminUser(sess.User),
	})
}

// ListMatrix GET /api/equipment/list_matrix
func (ctrl *EquipmentController) ListMatrix(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取设备矩阵失败", gin.H{})
		return
	}
	search := strings.TrimSpace(c.Query("search"))
	where := "1=1"
	args := []any{}
	if search != "" {
		where = "(code LIKE ? OR name LIKE ? OR product_series LIKE ? OR description LIKE ?)"
		k := "%" + search + "%"
		args = append(args, k, k, k, k)
	}
	equips, _ := core.QueryMaps(db, fmt.Sprintf("SELECT id, code, name, product_series, description FROM equipment WHERE %s ORDER BY code ASC, id ASC", where), args...)
	procRows, _ := core.QueryMaps(db, "SELECT equipment_id, process_code, efficiency FROM equipment_processes")

	procMap := make(map[int]map[string]any)
	allProcSet := make(map[string]bool)
	defaultProcs := core.LookupProductProcesses("")
	for _, p := range defaultProcs {
		allProcSet[p] = true
	}
	for _, pr := range procRows {
		eid := core.AsInt(pr["equipment_id"])
		pCode := core.AsString(pr["process_code"])
		if procMap[eid] == nil {
			procMap[eid] = make(map[string]any)
		}
		procMap[eid][pCode] = pr["efficiency"]
		if pCode != "" && !allProcSet[pCode] {
			defaultProcs = append(defaultProcs, pCode)
			allProcSet[pCode] = true
		}
	}

	for _, eq := range equips {
		eid := core.AsInt(eq["id"])
		if procMap[eid] != nil {
			eq["procs"] = procMap[eid]
		} else {
			eq["procs"] = map[string]any{}
		}
	}

	core.Success(c, "success", gin.H{
		"processes":  defaultProcs,
		"list":       equips,
		"can_manage": core.CanEditModule(sess.User, "equipment"),
	})
}

// GetProductInfo GET|POST /api/equipment/get_product_info
func (ctrl *EquipmentController) GetProductInfo(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询产品信息失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	code := strings.TrimSpace(core.AsString(in["code"]))
	if code == "" {
		code = strings.TrimSpace(core.AsString(in["product_code"]))
	}
	desc := core.LookupProductDesc(db, code)
	procs := core.LookupProductProcesses(code)
	core.Success(c, "success", gin.H{
		"code":        code,
		"description": desc,
		"processes":   procs,
	})
}

// SaveEquipment POST /api/equipment/save_equipment
func (ctrl *EquipmentController) SaveEquipment(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if !core.CanEditModule(sess.User, "equipment") {
		core.DenyModuleEdit(c, "编辑设备矩阵")
		return
	}
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存设备失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	code := strings.TrimSpace(core.AsString(in["code"]))
	name := strings.TrimSpace(core.AsString(in["name"]))
	series := strings.TrimSpace(core.AsString(in["product_series"]))
	desc := strings.TrimSpace(core.AsString(in["description"]))
	if code == "" || name == "" || series == "" {
		core.Error(c, "设备编号、设备名称及代码不能为空！", gin.H{})
		return
	}
	if desc == "" {
		desc = core.LookupProductDesc(db, series)
	}

	if id > 0 {
		_, err = db.Exec("UPDATE equipment SET code = ?, name = ?, product_series = ?, description = ? WHERE id = ?", code, name, series, desc, id)
	} else {
		res, insErr := db.Exec("INSERT INTO equipment (code, name, product_series, description) VALUES (?, ?, ?, ?)", code, name, series, desc)
		err = insErr
		if err == nil {
			newID, _ := res.LastInsertId()
			id = int(newID)
		}
	}
	if err != nil {
		core.Exception(c, err, "保存设备失败（请检查设备编号是否重复）", gin.H{})
		return
	}

	if effMap, ok := in["efficiencies"].(map[string]any); ok && id > 0 {
		for pCode, val := range effMap {
			if val == nil || strings.TrimSpace(core.AsString(val)) == "" {
				continue
			}
			_, _ = db.Exec(`
				INSERT INTO equipment_processes (equipment_id, process_code, efficiency, updated_at)
				VALUES (?, ?, ?, NOW())
				ON DUPLICATE KEY UPDATE efficiency = VALUES(efficiency), updated_at = NOW()
			`, id, pCode, core.AsFloat(val))
		}
	}
	service.LogAudit(c, "设备矩阵总览", "保存设备: "+code, true, "", 0, in)
	core.Success(c, "保存成功")
}

// DeleteEquipment POST /api/equipment/delete_equipment
func (ctrl *EquipmentController) DeleteEquipment(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	if !core.CanEditModule(sess.User, "equipment") {
		core.DenyModuleEdit(c, "删除设备")
		return
	}
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	if id > 0 {
		_, _ = db.Exec("DELETE FROM equipment_processes WHERE equipment_id = ?", id)
		_, _ = db.Exec("DELETE FROM equipment WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// ============================================================================
//  6. MAC 地址管理模块（MacController）—— 1:1 对齐前端 mac/Index.vue
// ============================================================================

type MacController struct{}

func NewMacController() *MacController {
	return &MacController{}
}

func (ctrl *MacController) getMyPerm(db *sql.DB, user map[string]any) map[string]int {
	if core.IsAdminUser(user) {
		return map[string]int{
			"can_apply":        1,
			"can_config_ports": 1,
			"can_import":       1,
			"can_view_records": 1,
			"can_view_stats":   1,
			"can_view_pool":    1,
			"can_view_logs":    1,
			"is_admin":         1,
		}
	}
	row, _ := core.QueryRowMap(db, "SELECT * FROM mac_permissions WHERE user_id = ? LIMIT 1", core.AsInt(user["id"]))
	if row == nil {
		return map[string]int{
			"can_apply":        1,
			"can_config_ports": 0,
			"can_import":       0,
			"can_view_records": 1,
			"can_view_stats":   1,
			"can_view_pool":    0,
			"can_view_logs":    0,
			"is_admin":         0,
		}
	}
	return map[string]int{
		"can_apply":        core.AsInt(row["can_apply"]),
		"can_config_ports": core.AsInt(row["can_config_ports"]),
		"can_import":       core.AsInt(row["can_import"]),
		"can_view_records": core.AsInt(row["can_view_records"]),
		"can_view_stats":   core.AsInt(row["can_view_stats"]),
		"can_view_pool":    core.AsInt(row["can_view_pool"]),
		"can_view_logs":    core.AsInt(row["can_view_logs"]),
		"is_admin":         0,
	}
}

// Init GET /api/mac/init
func (ctrl *MacController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "初始化MAC模块失败", gin.H{})
		return
	}
	cats, _ := core.QueryMaps(db, "SELECT * FROM mac_categories ORDER BY id ASC")
	perms := ctrl.getMyPerm(db, sess.User)
	core.Success(c, "success", gin.H{
		"categories": categoriesOrEmpty(cats),
		"mac_perms":  perms,
		"is_admin":   core.IsAdminUser(sess.User),
	})
}

func categoriesOrEmpty(cats []map[string]any) []map[string]any {
	if cats == nil {
		return []map[string]any{}
	}
	return cats
}

// GetWorkOrderInfo POST /api/mac/get_work_order_info
func (ctrl *MacController) GetWorkOrderInfo(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询工单失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	if wo == "" {
		core.Error(c, "请输入工单号", gin.H{})
		return
	}

	var appliedCnt int
	_ = db.QueryRow("SELECT COUNT(*) FROM mac_application_records WHERE work_order = ?", wo).Scan(&appliedCnt)

	meta := core.LookupWorkOrderInfo(db, wo)
	var orderInfo map[string]any
	autoPortCount := 0
	autoCategoryID := 0

	if meta != nil {
		orderInfo = map[string]any{
			"work_order":  meta.WorkOrder,
			"code":        meta.ProductCode,
			"description": meta.ProductDesc,
			"quantity":    int(math.Max(meta.Quantity, 1)),
		}
		if meta.ProductCode != "" {
			cfgRow, _ := core.QueryRowMap(db, "SELECT port_count, category_id FROM product_port_configs WHERE product_code = ? LIMIT 1", meta.ProductCode)
			if cfgRow != nil {
				autoPortCount = core.AsInt(cfgRow["port_count"])
				autoCategoryID = core.AsInt(cfgRow["category_id"])
			}
		}
	}

	core.Success(c, "success", gin.H{
		"applied":          appliedCnt > 0,
		"order_info":       orderInfo,
		"auto_port_count":  autoPortCount,
		"auto_category_id": autoCategoryID,
	})
}

// GetProductDesc POST /api/mac/get_product_desc
func (ctrl *MacController) GetProductDesc(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询产品描述失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	code := strings.TrimSpace(core.AsString(in["product_code"]))
	cfgRow, _ := core.QueryRowMap(db, "SELECT description, port_count, category_id FROM product_port_configs WHERE product_code = ? LIMIT 1", code)
	desc := ""
	portCnt := 0
	catID := 0
	if cfgRow != nil {
		desc = core.AsString(cfgRow["description"])
		portCnt = core.AsInt(cfgRow["port_count"])
		catID = core.AsInt(cfgRow["category_id"])
	}
	if desc == "" {
		desc = core.LookupProductDesc(db, code)
	}
	core.Success(c, "success", gin.H{
		"product_code": code,
		"description":  desc,
		"port_count":   portCnt,
		"category_id":  catID,
	})
}

// ListProductPorts GET /api/mac/list_product_ports
func (ctrl *MacController) ListProductPorts(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取产品网口配置失败", gin.H{})
		return
	}
	search := strings.TrimSpace(c.Query("search"))
	where := "1=1"
	args := []any{}
	if search != "" {
		where = "(p.product_code LIKE ? OR p.description LIKE ?)"
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	rows, _ := core.QueryMaps(db, fmt.Sprintf(`
		SELECT p.*, COALESCE(c.name, '') AS category_name
		FROM product_port_configs p
		LEFT JOIN mac_categories c ON c.id = p.category_id
		WHERE %s
		ORDER BY p.updated_at DESC, p.id DESC
	`, where), args...)
	core.Success(c, "success", gin.H{"list": rows})
}

// SaveProductPort POST /api/mac/save_product_port
func (ctrl *MacController) SaveProductPort(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	perms := ctrl.getMyPerm(db, sess.User)
	if perms["can_config_ports"] == 0 {
		core.Error(c, "权限不足", gin.H{}, http.StatusForbidden)
		return
	}
	in := core.ParseInputMap(c)
	code := strings.TrimSpace(core.AsString(in["product_code"]))
	desc := strings.TrimSpace(core.AsString(in["description"]))
	portCount := core.AsInt(in["port_count"])
	catID := core.AsInt(in["category_id"])
	if code == "" || portCount <= 0 {
		core.Error(c, "产品代码和网口数量不能为空", gin.H{})
		return
	}
	if desc == "" {
		desc = core.LookupProductDesc(db, code)
	}
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	_, err = db.Exec(`
		INSERT INTO product_port_configs (product_code, description, port_count, category_id, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE
			description = VALUES(description),
			port_count = VALUES(port_count),
			category_id = VALUES(category_id),
			updated_by = VALUES(updated_by),
			updated_at = NOW()
	`, code, desc, portCount, catID, opName)
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	core.Success(c, "保存成功")
}

// DeleteProductPort POST /api/mac/delete_product_port
func (ctrl *MacController) DeleteProductPort(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	if id > 0 {
		_, _ = db.Exec("DELETE FROM product_port_configs WHERE id = ?", id)
	}
	core.Success(c, "已删除")
}

// ImportMac POST /api/mac/import_mac
func (ctrl *MacController) ImportMac(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "录入失败", gin.H{})
		return
	}
	perms := ctrl.getMyPerm(db, sess.User)
	if perms["can_import"] == 0 {
		core.Error(c, "权限不足", gin.H{}, http.StatusForbidden)
		return
	}
	in := core.ParseInputMap(c)
	catID := core.AsInt(in["category_id"])
	if catID <= 0 {
		catID = 1
	}
	startMacRaw := strings.TrimSpace(core.AsString(in["start_mac"]))
	endMacRaw := strings.TrimSpace(core.AsString(in["end_mac"]))
	qty := core.AsInt(in["quantity"])
	remarks := strings.TrimSpace(core.AsString(in["remarks"]))

	sLong, sErr := core.MacToLong(startMacRaw)
	if sErr != nil {
		core.Error(c, "起始 MAC 地址格式不正确", gin.H{})
		return
	}
	var eLong int64
	if endMacRaw != "" {
		var eErr error
		eLong, eErr = core.MacToLong(endMacRaw)
		if eErr != nil || eLong < sLong {
			core.Error(c, "结束 MAC 地址格式不正确或小于起始地址", gin.H{})
			return
		}
		qty = int(eLong - sLong + 1)
	} else if qty > 0 {
		eLong = sLong + int64(qty) - 1
	} else {
		core.Error(c, "请填写结束 MAC 地址或录入数量", gin.H{})
		return
	}
	if qty > 50000 {
		core.Error(c, "单次批量展开入池数量请勿超过 50000 个", gin.H{})
		return
	}

	catRow, _ := core.QueryRowMap(db, "SELECT * FROM mac_categories WHERE id = ? LIMIT 1", catID)
	catName := "默认类别"
	if catRow != nil {
		catName = core.AsString(catRow["name"])
	}

	inserted := 0
	for cur := sLong; cur <= eLong; cur++ {
		macStr := core.LongToMac(cur)
		res, iErr := db.Exec("INSERT IGNORE INTO mac_address_pool (category_id, mac_address, status, created_at) VALUES (?, ?, 0, NOW())", catID, macStr)
		if iErr == nil {
			if aff, _ := res.RowsAffected(); aff > 0 {
				inserted++
			}
		}
	}
	_, _ = db.Exec("UPDATE mac_categories SET total_count = total_count + ? WHERE id = ?", inserted, catID)

	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	_, _ = db.Exec(`
		INSERT INTO mac_import_logs (category_id, category_name, start_mac, end_mac, quantity, imported_by, imported_by_name, imported_at, remarks)
		VALUES (?, ?, ?, ?, ?, ?, ?, NOW(), ?)
	`, catID, catName, core.LongToMac(sLong), core.LongToMac(eLong), inserted, sess.UserID, opName, remarks)

	core.Success(c, fmt.Sprintf("成功录入 %d 个 MAC 地址", inserted), gin.H{"count": inserted})
}

// ApplyMac POST /api/mac/apply_mac
func (ctrl *MacController) ApplyMac(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "申请分配失败", gin.H{})
		return
	}
	perms := ctrl.getMyPerm(db, sess.User)
	if perms["can_apply"] == 0 {
		core.Error(c, "权限不足", gin.H{}, http.StatusForbidden)
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	catID := core.AsInt(in["category_id"])
	if catID <= 0 {
		catID = 1
	}
	orderQty := core.AsInt(in["order_quantity"])
	if orderQty <= 0 {
		orderQty = core.AsInt(in["order_qty"])
	}
	portCount := core.AsInt(in["port_count"])
	if wo == "" || orderQty <= 0 || portCount <= 0 {
		core.Error(c, "工单号、工单数量和网口数必须大于 0", gin.H{})
		return
	}
	totalNeed := orderQty * portCount

	catRow, _ := core.QueryRowMap(db, "SELECT * FROM mac_categories WHERE id = ? LIMIT 1", catID)
	catName := "默认类别"
	if catRow != nil {
		catName = core.AsString(catRow["name"])
	}

	poolRows, _ := core.QueryMaps(db, "SELECT id, mac_address FROM mac_address_pool WHERE category_id = ? AND status = 0 ORDER BY id ASC LIMIT ?", catID, totalNeed)
	if len(poolRows) < totalNeed {
		core.Error(c, fmt.Sprintf("当前类别【%s】可用 MAC 地址余量不足 %d 个（当前可用 %d 个），请先在「批量录入」中补充地址池！", catName, totalNeed, len(poolRows)), gin.H{})
		return
	}

	startMac := core.AsString(poolRows[0]["mac_address"])
	endMac := core.AsString(poolRows[len(poolRows)-1]["mac_address"])
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}

	res, err := db.Exec(`
		INSERT INTO mac_application_records
		(work_order, category_id, category_name, order_quantity, port_count, total_mac_count, start_mac, end_mac, applicant_id, applicant_name, is_reapplied, reapply_reason, task_code, description, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, wo, catID, catName, orderQty, portCount, totalNeed, startMac, endMac, sess.UserID, opName,
		core.AsInt(in["is_reapplied"]), strings.TrimSpace(core.AsString(in["reapply_reason"])),
		strings.TrimSpace(core.AsString(in["task_code"])), strings.TrimSpace(core.AsString(in["description"])))
	if err != nil {
		core.Exception(c, err, "写入申请记录失败", gin.H{})
		return
	}
	recID, _ := res.LastInsertId()

	for idx, pr := range poolRows {
		pid := core.AsInt(pr["id"])
		mAddr := core.AsString(pr["mac_address"])
		_, _ = db.Exec("UPDATE mac_address_pool SET status = 1, used_by_work_order = ?, used_at = NOW() WHERE id = ?", wo, pid)
		_, _ = db.Exec("INSERT INTO mac_application_details (record_id, work_order, mac_address, sequence_no, created_at) VALUES (?, ?, ?, ?, NOW())", recID, wo, mAddr, idx+1)
	}
	_, _ = db.Exec("UPDATE mac_categories SET used_count = used_count + ? WHERE id = ?", totalNeed, catID)

	core.Success(c, fmt.Sprintf("申请分配成功：%s ~ %s（共 %d 个）", startMac, endMac, totalNeed), gin.H{"id": recID})
}

// GetRecords GET /api/mac/get_records
func (ctrl *MacController) GetRecords(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取记录失败", gin.H{})
		return
	}
	page := core.AsInt(c.Query("page"))
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	search := strings.TrimSpace(c.Query("search"))

	where := "1=1"
	args := []any{}
	if search != "" {
		where = "(work_order LIKE ? OR task_code LIKE ? OR description LIKE ? OR start_mac LIKE ? OR end_mac LIKE ? OR applicant_name LIKE ?)"
		k := "%" + search + "%"
		args = append(args, k, k, k, k, k, k)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM mac_application_records WHERE "+where, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM mac_application_records WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", where),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
	})
}

// GetRecordDetails GET /api/mac/get_record_details
func (ctrl *MacController) GetRecordDetails(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取明细失败", gin.H{})
		return
	}
	recID := core.AsInt(c.Query("record_id"))
	if recID <= 0 {
		recID = core.AsInt(c.Query("id"))
	}
	rec, _ := core.QueryRowMap(db, "SELECT * FROM mac_application_records WHERE id = ? LIMIT 1", recID)
	details, _ := core.QueryMaps(db, "SELECT * FROM mac_application_details WHERE record_id = ? ORDER BY sequence_no ASC", recID)
	core.Success(c, "success", gin.H{
		"record":  rec,
		"details": details,
	})
}

// DeleteRecords POST /api/mac/delete_records
func (ctrl *MacController) DeleteRecords(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	ids := core.ParseIDs(in)
	for _, id := range ids {
		rec, _ := core.QueryRowMap(db, "SELECT * FROM mac_application_records WHERE id = ? LIMIT 1", id)
		if rec != nil {
			catID := core.AsInt(rec["category_id"])
			cnt := core.AsInt(rec["total_mac_count"])
			_, _ = db.Exec("UPDATE mac_address_pool SET status = 0, used_by_work_order = NULL, used_at = NULL WHERE mac_address IN (SELECT mac_address FROM mac_application_details WHERE record_id = ?)", id)
			_, _ = db.Exec("DELETE FROM mac_application_details WHERE record_id = ?", id)
			_, _ = db.Exec("DELETE FROM mac_application_records WHERE id = ?", id)
			_, _ = db.Exec("UPDATE mac_categories SET used_count = GREATEST(0, used_count - ?) WHERE id = ?", cnt, catID)
		}
	}
	core.Success(c, "删除成功并已释放 MAC 地址回池")
}

// GetStats GET /api/mac/get_stats
func (ctrl *MacController) GetStats(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取统计失败", gin.H{})
		return
	}
	cats, _ := core.QueryMaps(db, "SELECT * FROM mac_categories ORDER BY id ASC")
	stats := make([]map[string]any, 0, len(cats))
	for _, cat := range cats {
		tot := core.AsInt(cat["total_count"])
		used := core.AsInt(cat["used_count"])
		avail := tot - used
		if avail < 0 {
			avail = 0
		}
		rate := 0.0
		if tot > 0 {
			rate = math.Round(float64(used)/float64(tot)*10000) / 100
		}
		stats = append(stats, map[string]any{
			"category_id":     cat["id"],
			"category_name":   cat["name"],
			"name":            cat["name"],
			"total_count":     tot,
			"used_count":      used,
			"available_count": avail,
			"available":       avail,
			"usage_rate":      rate,
		})
	}
	core.Success(c, "success", gin.H{"stats": stats})
}

// GetPool GET /api/mac/get_pool
func (ctrl *MacController) GetPool(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取地址池失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 30
	offset := (page - 1) * limit
	catID := core.AsInt(in["category_id"])
	status := core.AsInt(in["status"])
	search := strings.TrimSpace(core.AsString(in["search"]))

	where := []string{"1=1"}
	args := []any{}
	if catID > 0 {
		where = append(where, "p.category_id = ?")
		args = append(args, catID)
	}
	if _, hasSt := in["status"]; hasSt && status >= 0 {
		where = append(where, "p.status = ?")
		args = append(args, status)
	}
	if search != "" {
		where = append(where, "(p.mac_address LIKE ? OR p.used_by_work_order LIKE ?)")
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM mac_address_pool p WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf(`
		SELECT p.*, COALESCE(c.name, '') AS category_name
		FROM mac_address_pool p
		LEFT JOIN mac_categories c ON c.id = p.category_id
		WHERE %s
		ORDER BY p.id ASC LIMIT ? OFFSET ?
	`, whereSQL), append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"list":        rows,
		"total":       total,
		"total_pages": core.CalcTotalPages(total, limit),
	})
}

// GetImportLogs GET /api/mac/get_import_logs
func (ctrl *MacController) GetImportLogs(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取录入日志失败", gin.H{})
		return
	}
	page := core.AsInt(c.Query("page"))
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM mac_import_logs").Scan(&total)
	rows, _ := core.QueryMaps(db, "SELECT * FROM mac_import_logs ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	core.Success(c, "success", gin.H{
		"logs":        rows,
		"total":       total,
		"total_pages": core.CalcTotalPages(total, limit),
	})
}

// ExportDetails GET /api/mac/export_details
func (ctrl *MacController) ExportDetails(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	recID := core.AsInt(c.Query("record_id"))
	where := "1=1"
	args := []any{}
	if recID > 0 {
		where = "record_id = ?"
		args = append(args, recID)
	}
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT sequence_no, work_order, mac_address, created_at FROM mac_application_details WHERE %s ORDER BY id ASC LIMIT 5000", where), args...)
	headers := []string{"序号", "工单号", "MAC地址", "分配时间"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["sequence_no"]),
			core.AsString(r["work_order"]),
			core.AsString(r["mac_address"]),
			core.AsString(r["created_at"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("MAC分配明细_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// ============================================================================
//  7. MAC / ID 信息反查模块（MacQueryController）
// ============================================================================

type MacQueryController struct{}

func NewMacQueryController() *MacQueryController {
	return &MacQueryController{}
}

// Query GET|POST /api/mac_query/query
func (ctrl *MacQueryController) Query(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	snVal := strings.TrimSpace(core.AsString(in["sn"]))
	taskVal := strings.TrimSpace(core.AsString(in["task"]))
	if snVal == "" && taskVal == "" {
		snVal = strings.TrimSpace(core.AsString(in["keyword"]))
	}
	if snVal == "" && taskVal == "" {
		core.Error(c, "请至少输入【装置 SN】或【工单号】", gin.H{})
		return
	}

	list := make([]map[string]any, 0)
	rawCount := 0

	// 1. 尝试外部 MES tab_log_qr
	if mesDB, mErr := core.GetDB("mes"); mErr == nil && mesDB != nil {
		where := []string{"1=1"}
		args := []any{}
		if snVal != "" {
			where = append(where, "sn LIKE ?")
			args = append(args, "%"+snVal+"%")
		}
		if taskVal != "" {
			where = append(where, "task_no LIKE ?")
			args = append(args, "%"+taskVal+"%")
		}
		qrRows, _ := core.QueryMaps(mesDB, fmt.Sprintf("SELECT * FROM tab_log_qr WHERE %s ORDER BY id DESC LIMIT 200", strings.Join(where, " AND ")), args...)
		rawCount = len(qrRows)
		list = append(list, qrRows...)
	}

	// 2. 本地 mac_application_details 补充查询
	where := []string{"1=1"}
	args := []any{}
	if snVal != "" {
		where = append(where, "d.mac_address LIKE ?")
		args = append(args, "%"+snVal+"%")
	}
	if taskVal != "" {
		where = append(where, "d.work_order LIKE ?")
		args = append(args, "%"+taskVal+"%")
	}
	localRows, _ := core.QueryMaps(db, fmt.Sprintf(`
		SELECT d.work_order AS task_no, d.mac_address AS sn, d.mac_address, d.sequence_no, d.created_at AS update_time,
		       r.task_code AS mater_no, r.description AS mater_desc
		FROM mac_application_details d
		LEFT JOIN mac_application_records r ON r.id = d.record_id
		WHERE %s
		ORDER BY d.id DESC LIMIT 200
	`, strings.Join(where, " AND ")), args...)
	list = append(list, localRows...)

	core.Success(c, "查询成功", gin.H{
		"list":       list,
		"from_cache": false,
		"raw_count":  rawCount + len(localRows),
	})
}

// Export GET /api/mac_query/export
func (ctrl *MacQueryController) Export(c *gin.Context) {
	NewMacController().ExportDetails(c)
}

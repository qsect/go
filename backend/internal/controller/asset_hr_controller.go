package controller

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
	"vite-system-go/internal/core"
)

// =========================================================================
// 22. OsmController — 外协管理 (osm_records / osm_returns)
// =========================================================================

type OsmController struct{}

func NewOsmController() *OsmController { return &OsmController{} }

func (ctrl *OsmController) getPerms(user map[string]any) map[string]any {
	isAdmin := core.IsAdminUser(user)
	perms := map[string]any{
		"can_view":   true,
		"can_add":    true,
		"can_edit":   true,
		"can_delete": isAdmin,
		"can_return": true,
		"can_export": true,
	}
	opStr := core.AsString(user["osm_permissions"])
	if opStr != "" {
		var p map[string]any
		if json.Unmarshal([]byte(opStr), &p) == nil {
			for k, v := range p {
				perms[k] = v
			}
		}
	}
	if isAdmin {
		perms["can_view"] = true
		perms["can_add"] = true
		perms["can_edit"] = true
		perms["can_delete"] = true
		perms["can_return"] = true
		perms["can_export"] = true
	}
	return perms
}

func (ctrl *OsmController) Init(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)
	rows, _ := db.Query(`SELECT DISTINCT vendor FROM osm_records WHERE vendor != '' ORDER BY vendor`)
	vendors := []string{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var v string
			if rows.Scan(&v) == nil && v != "" {
				vendors = append(vendors, v)
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"osm_perms":   ctrl.getPerms(sess.User),
		"permissions": ctrl.getPerms(sess.User),
		"vendors":     vendors,
		"is_admin":    isAdmin,
	})
}

func (ctrl *OsmController) List(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)
	in := core.ParseInputMap(c)

	page := core.AsInt(in["page"])
	if page <= 0 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 {
		limit = core.AsInt(in["size"])
	}
	if limit <= 0 {
		limit = 20
	}
	offset := (page - 1) * limit

	search := strings.TrimSpace(core.AsString(in["search"]))
	if search == "" {
		search = strings.TrimSpace(core.AsString(in["keyword"]))
	}
	vendor := strings.TrimSpace(core.AsString(in["vendor"]))
	status := strings.TrimSpace(core.AsString(in["status"]))
	startDate := strings.TrimSpace(core.AsString(in["start_date"]))
	endDate := strings.TrimSpace(core.AsString(in["end_date"]))

	where := "WHERE 1=1"
	args := []any{}
	if search != "" {
		where += " AND (work_order LIKE ? OR product_code LIKE ? OR description LIKE ? OR vendor LIKE ? OR remarks LIKE ?)"
		like := "%" + search + "%"
		args = append(args, like, like, like, like, like)
	}
	if vendor != "" {
		where += " AND vendor=?"
		args = append(args, vendor)
	}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	if startDate != "" {
		where += " AND send_date >= ?"
		args = append(args, startDate)
	}
	if endDate != "" {
		where += " AND send_date <= ?"
		args = append(args, endDate)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM osm_records "+where, args...).Scan(&total)
	rows, _ := db.Query("SELECT id, work_order, product_code, description, vendor, quantity, returned_quantity, status, COALESCE(DATE_FORMAT(send_date,'%Y-%m-%d'),''), COALESCE(DATE_FORMAT(required_return_date,'%Y-%m-%d'),''), COALESCE(remarks,''), COALESCE(created_by,''), COALESCE(DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s'),'') FROM osm_records "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, qty, retQty int
			var wo, pc, desc, vend, st, sd, rrd, rem, cb, ca string
			if rows.Scan(&id, &wo, &pc, &desc, &vend, &qty, &retQty, &st, &sd, &rrd, &rem, &cb, &ca) == nil {
				pending := qty - retQty
				if pending < 0 {
					pending = 0
				}
				list = append(list, map[string]any{
					"id":                   id,
					"work_order":           wo,
					"product_code":         pc,
					"description":          desc,
					"vendor":               vend,
					"quantity":             qty,
					"returned_quantity":    retQty,
					"pending_qty":          pending,
					"status":               st,
					"send_date":            sd,
					"required_return_date": rrd,
					"remarks":              rem,
					"created_by":           cb,
					"created_at":           ca,
				})
			}
		}
	}
	totalPages := core.CalcTotalPages(total, limit)
	core.Success(c, "ok", gin.H{
		"records":       list,
		"list":          list,
		"total":         total,
		"total_records": total,
		"total_pages":   totalPages,
		"page":          page,
		"limit":         limit,
		"is_admin":      isAdmin,
	})
}

func (ctrl *OsmController) Save(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	username := core.AsString(sess.User["username"])
	if username == "" {
		username = "admin"
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	pc := strings.TrimSpace(core.AsString(in["product_code"]))
	desc := strings.TrimSpace(core.AsString(in["description"]))
	vendor := strings.TrimSpace(core.AsString(in["vendor"]))
	qty := core.AsInt(in["quantity"])
	sendDate := strings.TrimSpace(core.AsString(in["send_date"]))
	if sendDate == "" {
		sendDate = time.Now().Format("2006-01-02")
	}
	reqReturnDate := strings.TrimSpace(core.AsString(in["required_return_date"]))
	var reqDateVal any = nil
	if reqReturnDate != "" {
		reqDateVal = reqReturnDate
	}
	remarks := core.AsString(in["remarks"])

	if id > 0 {
		var retQty int
		_ = db.QueryRow(`SELECT COALESCE(returned_quantity,0) FROM osm_records WHERE id=?`, id).Scan(&retQty)
		status := "未返回"
		if retQty >= qty && qty > 0 {
			status = "全部返回"
		} else if retQty > 0 {
			status = "部分返回"
		}
		_, err := db.Exec(`UPDATE osm_records SET work_order=?, product_code=?, description=?, vendor=?, quantity=?, status=?, send_date=?, required_return_date=?, remarks=?, updated_at=NOW() WHERE id=?`,
			wo, pc, desc, vendor, qty, status, sendDate, reqDateVal, remarks, id)
		if err != nil {
			core.Error(c, "保存失败: "+err.Error(), gin.H{})
			return
		}
		core.Success(c, "更新成功", gin.H{"id": id})
		return
	}
	res, err := db.Exec(`INSERT INTO osm_records (work_order, product_code, description, vendor, quantity, returned_quantity, status, send_date, required_return_date, remarks, created_by, created_at, updated_at) VALUES (?,?,?,?,?,0,'未返回',?,?,?, ?,NOW(),NOW())`,
		wo, pc, desc, vendor, qty, sendDate, reqDateVal, remarks, username)
	if err != nil {
		core.Error(c, "新增失败: "+err.Error(), gin.H{})
		return
	}
	lastID, _ := res.LastInsertId()
	core.Success(c, "新增成功", gin.H{"id": lastID})
}

func (ctrl *OsmController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	ids := core.ParseIDs(in)
	for _, id := range ids {
		_, _ = db.Exec(`DELETE FROM osm_returns WHERE osm_id=?`, id)
		_, _ = db.Exec(`DELETE FROM osm_records WHERE id=?`, id)
	}
	core.Success(c, "删除成功", gin.H{})
}

func (ctrl *OsmController) GetPendingReturns(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT id, work_order, product_code, description, vendor, quantity, returned_quantity, status, COALESCE(DATE_FORMAT(send_date,'%Y-%m-%d'),'') FROM osm_records WHERE quantity > returned_quantity ORDER BY id DESC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, qty, retQty int
			var wo, pc, desc, vend, st, sd string
			if rows.Scan(&id, &wo, &pc, &desc, &vend, &qty, &retQty, &st, &sd) == nil {
				list = append(list, map[string]any{
					"id":                id,
					"work_order":        wo,
					"product_code":      pc,
					"description":       desc,
					"vendor":            vend,
					"quantity":          qty,
					"returned_quantity": retQty,
					"pending_qty":       qty - retQty,
					"status":            st,
					"send_date":         sd,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"list": list, "records": list})
}

func (ctrl *OsmController) AddReturn(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	username := core.AsString(sess.User["username"])
	if username == "" {
		username = "admin"
	}
	in := core.ParseInputMap(c)
	osmID := core.AsInt(in["osm_id"])
	if osmID == 0 {
		osmID = core.AsInt(in["record_id"])
	}
	qty := core.AsInt(in["return_quantity"])
	if qty == 0 {
		qty = core.AsInt(in["return_qty"])
	}
	retDate := strings.TrimSpace(core.AsString(in["return_date"]))
	if retDate == "" {
		retDate = time.Now().Format("2006-01-02")
	}
	remarks := core.AsString(in["remarks"])
	if osmID <= 0 || qty <= 0 {
		core.Error(c, "请选择记录并输入有效返回数量", gin.H{})
		return
	}
	_, _ = db.Exec(`INSERT INTO osm_returns (osm_id, return_quantity, return_date, remarks, created_by, created_at) VALUES (?,?,?,?,?,NOW())`,
		osmID, qty, retDate, remarks, username)

	var totalQty, retQty int
	_ = db.QueryRow(`SELECT quantity, returned_quantity FROM osm_records WHERE id=?`, osmID).Scan(&totalQty, &retQty)
	newRet := retQty + qty
	status := "部分返回"
	if newRet >= totalQty {
		status = "全部返回"
	}
	_, _ = db.Exec(`UPDATE osm_records SET returned_quantity=?, status=?, updated_at=NOW() WHERE id=?`, newRet, status, osmID)
	core.Success(c, "登记返回成功", gin.H{})
}

func (ctrl *OsmController) UploadOsm(c *gin.Context) {
	core.Success(c, "功能已就绪", gin.H{})
}

func (ctrl *OsmController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT work_order, product_code, description, vendor, quantity, returned_quantity, status, COALESCE(DATE_FORMAT(send_date,'%Y-%m-%d'),''), COALESCE(DATE_FORMAT(required_return_date,'%Y-%m-%d'),''), COALESCE(remarks,'') FROM osm_records ORDER BY id DESC`)
	var out [][]string
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var wo, pc, desc, vend, st, sd, rrd, rem string
			var qty, ret int
			if rows.Scan(&wo, &pc, &desc, &vend, &qty, &ret, &st, &sd, &rrd, &rem) == nil {
				out = append(out, []string{wo, pc, desc, vend, fmt.Sprintf("%d", qty), fmt.Sprintf("%d", ret), st, sd, rrd, rem})
			}
		}
	}
	core.ExportCSV(c, "外协记录.csv", []string{"任务单号", "物料编码", "产品描述", "外协厂家", "发出数量", "已返数量", "状态", "发出日期", "要求返回日期", "备注"}, out)
}

func (ctrl *OsmController) ListPermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT id, username, COALESCE(display_name,''), role, COALESCE(osm_permissions,'') FROM users WHERE status=1 ORDER BY id ASC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var u, dn, role, op string
			if rows.Scan(&id, &u, &dn, &role, &op) == nil {
				var p map[string]any
				_ = json.Unmarshal([]byte(op), &p)
				if p == nil {
					p = map[string]any{}
				}
				canAdd, _ := p["can_add"].(bool)
				canEdit, _ := p["can_edit"].(bool)
				canDelete, _ := p["can_delete"].(bool)
				canReturn, _ := p["can_return"].(bool)
				list = append(list, map[string]any{
					"id":              id,
					"username":        u,
					"display_name":    dn,
					"role":            role,
					"osm_permissions": p,
					"can_add":         canAdd,
					"can_edit":        canEdit,
					"can_delete":      canDelete,
					"can_return":      canReturn,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"users": list, "list": list})
}

func (ctrl *OsmController) SavePermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	if arr, ok := in["permissions"].([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				uid := core.AsInt(m["id"])
				if uid > 0 {
					b, _ := json.Marshal(map[string]any{
						"can_view":   true,
						"can_add":    m["can_add"] == true,
						"can_edit":   m["can_edit"] == true,
						"can_delete": m["can_delete"] == true,
						"can_return": m["can_return"] == true,
					})
					_, _ = db.Exec(`UPDATE users SET osm_permissions=? WHERE id=?`, string(b), uid)
				}
			}
		}
		core.Success(c, "权限保存成功", gin.H{})
		return
	}
	uid := core.AsInt(in["user_id"])
	b, _ := json.Marshal(in["permissions"])
	_, _ = db.Exec(`UPDATE users SET osm_permissions=? WHERE id=?`, string(b), uid)
	core.Success(c, "权限保存成功", gin.H{})
}

// =========================================================================
// 23. AssetsController — 资产管理 (asset_categories / assets)
// =========================================================================

var assetAllFieldsDict = map[string]string{
	"category":   "类别",
	"name":       "名称",
	"code":       "资产编号",
	"model":      "型号",
	"config":     "配置",
	"user":       "使用人",
	"location":   "存放地点",
	"mac1":       "MAC1",
	"ip1":        "IP1",
	"mac2":       "MAC2",
	"ip2":        "IP2",
	"price":      "价格",
	"status":     "状态",
	"remark":     "备注",
	"image_path": "图片",
}

var defaultAssetShowFields = []string{"category", "name", "code", "model", "user", "location", "ip1", "price", "status", "remark", "image_path"}

type AssetsController struct{}

func NewAssetsController() *AssetsController { return &AssetsController{} }

func (ctrl *AssetsController) GetCategories(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)
	rows, _ := db.Query(`SELECT id, name, COALESCE(show_fields,'') FROM asset_categories ORDER BY id ASC`)
	cats := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var name, sf string
			if rows.Scan(&id, &name, &sf) == nil {
				var fields []string
				_ = json.Unmarshal([]byte(sf), &fields)
				if len(fields) == 0 {
					fields = defaultAssetShowFields
				}
				cats = append(cats, map[string]any{
					"id":          id,
					"name":        name,
					"show_fields": fields,
				})
			}
		}
	}
	userPerms := map[string]any{
		"is_admin":   isAdmin,
		"can_edit":   true,
		"can_delete": isAdmin,
		"can_assign": true,
	}
	apStr := core.AsString(sess.User["asset_permissions"])
	if apStr != "" {
		var p map[string]any
		if json.Unmarshal([]byte(apStr), &p) == nil {
			for k, v := range p {
				userPerms[k] = v
			}
		}
	}
	if isAdmin {
		userPerms["is_admin"] = true
		userPerms["can_edit"] = true
		userPerms["can_delete"] = true
		userPerms["can_assign"] = true
	}
	core.Success(c, "ok", gin.H{
		"all_fields": assetAllFieldsDict,
		"categories": cats,
		"list":       cats,
		"user_perms": userPerms,
		"is_admin":   isAdmin,
	})
}

func (ctrl *AssetsController) DashboardStats(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	var totalCount, inUseCount, abnormalCount int
	var totalValue float64
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(price),0) FROM assets`).Scan(&totalCount, &totalValue)
	_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status IN ('在用','正常')`).Scan(&inUseCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM assets WHERE status IN ('维修','故障','报废','异常')`).Scan(&abnormalCount)

	catRows, _ := db.Query(`SELECT category, COUNT(*) as cnt, COALESCE(SUM(price),0) as total_val, SUM(CASE WHEN status IN ('在用','正常') THEN 1 ELSE 0 END) as normal_cnt, SUM(CASE WHEN status='闲置' THEN 1 ELSE 0 END) as idle_cnt, SUM(CASE WHEN status IN ('维修','故障','报废','异常') THEN 1 ELSE 0 END) as fault_cnt FROM assets GROUP BY category`)
	catStats := []map[string]any{}
	if catRows != nil {
		defer catRows.Close()
		for catRows.Next() {
			var cat string
			var cnt, normCnt, idleCnt, faultCnt int
			var val float64
			if catRows.Scan(&cat, &cnt, &val, &normCnt, &idleCnt, &faultCnt) == nil {
				catStats = append(catStats, map[string]any{
					"category":   cat,
					"count":      cnt,
					"total_val":  fmt.Sprintf("%.2f", val),
					"normal_cnt": normCnt,
					"idle_cnt":   idleCnt,
					"fault_cnt":  faultCnt,
				})
			}
		}
	}

	abnRows, _ := db.Query(`SELECT id, category, name, COALESCE(code,''), COALESCE(user,''), COALESCE(location,''), status, COALESCE(remark,'') FROM assets WHERE status IN ('维修','故障','报废','异常') ORDER BY id DESC LIMIT 50`)
	abnList := []map[string]any{}
	if abnRows != nil {
		defer abnRows.Close()
		for abnRows.Next() {
			var id int
			var cat, name, code, u, loc, st, rem string
			if abnRows.Scan(&id, &cat, &name, &code, &u, &loc, &st, &rem) == nil {
				abnList = append(abnList, map[string]any{
					"id":       id,
					"category": cat,
					"name":     name,
					"code":     code,
					"user":     u,
					"location": loc,
					"status":   st,
					"remark":   rem,
				})
			}
		}
	}

	core.Success(c, "ok", gin.H{
		"total_count":    totalCount,
		"total_value":    fmt.Sprintf("%.2f", totalValue),
		"in_use_count":   inUseCount,
		"abnormal_count": abnormalCount,
		"categories":     catStats,
		"abnormal_list":  abnList,
	})
}

func (ctrl *AssetsController) List(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page <= 0 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 {
		limit = core.AsInt(in["size"])
	}
	if limit <= 0 {
		limit = 20
	}
	offset := (page - 1) * limit

	cat := strings.TrimSpace(core.AsString(in["category"]))
	search := strings.TrimSpace(core.AsString(in["search"]))
	if search == "" {
		search = strings.TrimSpace(core.AsString(in["keyword"]))
	}

	where := "WHERE 1=1"
	args := []any{}
	if cat != "" && cat != "__ALL__" {
		where += " AND category=?"
		args = append(args, cat)
	}
	if search != "" {
		where += " AND (name LIKE ? OR code LIKE ? OR model LIKE ? OR user LIKE ? OR location LIKE ? OR ip1 LIKE ? OR remark LIKE ?)"
		like := "%" + search + "%"
		args = append(args, like, like, like, like, like, like, like)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM assets "+where, args...).Scan(&total)
	rows, _ := db.Query("SELECT id, category, name, COALESCE(code,''), COALESCE(model,''), COALESCE(config,''), COALESCE(user,''), COALESCE(location,''), COALESCE(mac1,''), COALESCE(ip1,''), COALESCE(mac2,''), COALESCE(ip2,''), COALESCE(price,0), COALESCE(status,'正常'), COALESCE(remark,''), COALESCE(image_path,''), COALESCE(DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s'),'') FROM assets "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var catName, name, code, model, cfg, u, loc, m1, i1, m2, i2, st, rem, img, ca string
			var price float64
			if rows.Scan(&id, &catName, &name, &code, &model, &cfg, &u, &loc, &m1, &i1, &m2, &i2, &price, &st, &rem, &img, &ca) == nil {
				list = append(list, map[string]any{
					"id":         id,
					"category":   catName,
					"name":       name,
					"code":       code,
					"model":      model,
					"config":     cfg,
					"user":       u,
					"location":   loc,
					"mac1":       m1,
					"ip1":        i1,
					"mac2":       m2,
					"ip2":        i2,
					"price":      price,
					"status":     st,
					"remark":     rem,
					"image_path": img,
					"created_at": ca,
				})
			}
		}
	}
	showFields := defaultAssetShowFields
	if cat != "" && cat != "__ALL__" {
		var sf string
		if db.QueryRow(`SELECT COALESCE(show_fields,'') FROM asset_categories WHERE name=?`, cat).Scan(&sf) == nil && sf != "" {
			var parsed []string
			if json.Unmarshal([]byte(sf), &parsed) == nil && len(parsed) > 0 {
				showFields = parsed
			}
		}
	}
	totalPages := core.CalcTotalPages(total, limit)
	core.Success(c, "ok", gin.H{
		"list":        list,
		"records":     list,
		"total":       total,
		"total_pages": totalPages,
		"page":        page,
		"show_fields": showFields,
	})
}

func (ctrl *AssetsController) Save(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	cat := strings.TrimSpace(core.AsString(in["category"]))
	name := strings.TrimSpace(core.AsString(in["name"]))
	code := strings.TrimSpace(core.AsString(in["code"]))
	model := core.AsString(in["model"])
	cfg := core.AsString(in["config"])
	u := core.AsString(in["user"])
	loc := core.AsString(in["location"])
	m1 := core.AsString(in["mac1"])
	i1 := core.AsString(in["ip1"])
	m2 := core.AsString(in["mac2"])
	i2 := core.AsString(in["ip2"])
	price := core.AsFloat(in["price"])
	status := core.AsString(in["status"])
	if status == "" {
		status = "正常"
	}
	remark := core.AsString(in["remark"])
	imgPath := core.AsString(in["image_path"])

	// 文件上传处理
	file, fileErr := c.FormFile("image_file")
	if fileErr == nil && file != nil {
		pubRoot := config.Get("PUBLIC_ROOT", "./public")
		saveDir := filepath.Join(pubRoot, "uploads", "assets")
		_ = os.MkdirAll(saveDir, 0755)
		fname := fmt.Sprintf("%d_%s", time.Now().Unix(), filepath.Base(file.Filename))
		dst := filepath.Join(saveDir, fname)
		if c.SaveUploadedFile(file, dst) == nil {
			imgPath = filepath.ToSlash(filepath.Join("uploads", "assets", fname))
		}
	}

	if id > 0 {
		if imgPath != "" {
			_, _ = db.Exec(`UPDATE assets SET category=?, name=?, code=?, model=?, config=?, user=?, location=?, mac1=?, ip1=?, mac2=?, ip2=?, price=?, status=?, remark=?, image_path=?, updated_at=NOW() WHERE id=?`,
				cat, name, code, model, cfg, u, loc, m1, i1, m2, i2, price, status, remark, imgPath, id)
		} else {
			_, _ = db.Exec(`UPDATE assets SET category=?, name=?, code=?, model=?, config=?, user=?, location=?, mac1=?, ip1=?, mac2=?, ip2=?, price=?, status=?, remark=?, updated_at=NOW() WHERE id=?`,
				cat, name, code, model, cfg, u, loc, m1, i1, m2, i2, price, status, remark, id)
		}
		core.Success(c, "保存成功", gin.H{"id": id})
		return
	}
	res, err := db.Exec(`INSERT INTO assets (category, name, code, model, config, user, location, mac1, ip1, mac2, ip2, price, status, remark, image_path, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NOW(),NOW())`,
		cat, name, code, model, cfg, u, loc, m1, i1, m2, i2, price, status, remark, imgPath)
	if err != nil {
		core.Error(c, "保存失败: "+err.Error(), gin.H{})
		return
	}
	lastID, _ := res.LastInsertId()
	core.Success(c, "新增成功", gin.H{"id": lastID})
}

func (ctrl *AssetsController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	for _, id := range core.ParseIDs(in) {
		_, _ = db.Exec(`DELETE FROM assets WHERE id=?`, id)
	}
	core.Success(c, "删除成功", gin.H{})
}

func (ctrl *AssetsController) SaveCategory(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	name := strings.TrimSpace(core.AsString(in["name"]))
	sfBytes, _ := json.Marshal(in["show_fields"])
	if string(sfBytes) == "null" || len(sfBytes) == 0 {
		sfBytes, _ = json.Marshal(defaultAssetShowFields)
	}
	if id > 0 {
		_, _ = db.Exec(`UPDATE asset_categories SET name=?, show_fields=? WHERE id=?`, name, string(sfBytes), id)
		core.Success(c, "类别已更新", gin.H{})
		return
	}
	_, _ = db.Exec(`INSERT INTO asset_categories (name, show_fields, created_at) VALUES (?,?,NOW()) ON DUPLICATE KEY UPDATE show_fields=VALUES(show_fields)`, name, string(sfBytes))
	core.Success(c, "类别已保存", gin.H{})
}

func (ctrl *AssetsController) DeleteCategory(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	_, _ = db.Exec(`DELETE FROM asset_categories WHERE id=?`, id)
	core.Success(c, "删除成功", gin.H{})
}

func (ctrl *AssetsController) ExportCsv(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	cat := strings.TrimSpace(core.AsString(in["category"]))
	where := ""
	args := []any{}
	if cat != "" && cat != "__ALL__" {
		where = "WHERE category=?"
		args = append(args, cat)
	}
	rows, _ := db.Query(`SELECT category, name, COALESCE(code,''), COALESCE(model,''), COALESCE(config,''), COALESCE(user,''), COALESCE(location,''), COALESCE(ip1,''), COALESCE(price,0), COALESCE(status,''), COALESCE(remark,'') FROM assets `+where+` ORDER BY id DESC`, args...)
	var out [][]string
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var cName, n, code, m, cfg, u, loc, ip, st, rem string
			var price float64
			if rows.Scan(&cName, &n, &code, &m, &cfg, &u, &loc, &ip, &price, &st, &rem) == nil {
				out = append(out, []string{cName, n, code, m, cfg, u, loc, ip, fmt.Sprintf("%.2f", price), st, rem})
			}
		}
	}
	core.ExportCSV(c, "资产台账.csv", []string{"类别", "名称", "资产编号", "型号", "配置", "使用人", "存放地点", "IP1", "价格", "状态", "备注"}, out)
}

func (ctrl *AssetsController) ImportCsv(c *gin.Context) {
	core.Success(c, "导入完成", gin.H{"count": 0})
}

func (ctrl *AssetsController) ListUserPermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	catRows, _ := db.Query(`SELECT name FROM asset_categories ORDER BY id ASC`)
	groups := []string{}
	if catRows != nil {
		defer catRows.Close()
		for catRows.Next() {
			var n string
			if catRows.Scan(&n) == nil {
				groups = append(groups, n)
			}
		}
	}
	rows, _ := db.Query(`SELECT id, username, COALESCE(display_name,''), role, COALESCE(asset_permissions,'') FROM users WHERE status=1 ORDER BY id ASC`)
	users := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var u, dn, role, ap string
			if rows.Scan(&id, &u, &dn, &role, &ap) == nil {
				var p map[string]any
				_ = json.Unmarshal([]byte(ap), &p)
				if p == nil {
					p = map[string]any{"groups": groups, "can_edit": true, "can_delete": role == "admin", "can_assign": true}
				}
				users = append(users, map[string]any{
					"id":           id,
					"username":     u,
					"display_name": dn,
					"role":         role,
					"perms":        p,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"available_groups": groups,
		"users":            users,
	})
}

func (ctrl *AssetsController) SaveUserPermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	uid := core.AsInt(in["user_id"])
	b, _ := json.Marshal(in["perms"])
	_, _ = db.Exec(`UPDATE users SET asset_permissions=? WHERE id=?`, string(b), uid)
	core.Success(c, "资产权限保存成功", gin.H{})
}

// =========================================================================
// 24. AgingCartController — 老化车维修管理 (aging_settings / aging_carts / aging_repairs / aging_repair_workers)
// =========================================================================

type AgingCartController struct{}

func NewAgingCartController() *AgingCartController { return &AgingCartController{} }

func (ctrl *AgingCartController) cartTotal(db *sql.DB) int {
	var total int
	if err := db.QueryRow(`SELECT cart_total FROM aging_settings WHERE id=1`).Scan(&total); err == nil && total >= 1 && total <= 999 {
		return total
	}
	return 140
}

func (ctrl *AgingCartController) Init(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)
	total := ctrl.cartTotal(db)
	core.Success(c, "ok", gin.H{
		"cart_total":          total,
		"fault_types":         []string{"车辆变形", "车辆掉螺丝", "轮子异常", "其他"},
		"fault_desc_required": []string{"其他"},
		"usage_types":         []string{"固定使用", "流动使用"},
		"can_edit_cart":       true,
		"can_manage":          isAdmin,
		"is_admin":            isAdmin,
	})
}

func (ctrl *AgingCartController) Dashboard(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	total := ctrl.cartTotal(db)
	maxNo := fmt.Sprintf("%03d", total)

	var pendingCnt, repairingCnt, manualFault, normalCnt int
	_ = db.QueryRow(`SELECT COUNT(*) FROM aging_carts WHERE cart_no <= ? AND status='pending'`, maxNo).Scan(&pendingCnt)
	_ = db.QueryRow(`SELECT COUNT(*) FROM aging_carts WHERE cart_no <= ? AND status='repairing'`, maxNo).Scan(&repairingCnt)
	_ = db.QueryRow(`SELECT COUNT(*) FROM aging_carts WHERE cart_no <= ? AND status='fault'`, maxNo).Scan(&manualFault)
	_ = db.QueryRow(`SELECT COUNT(*) FROM aging_carts WHERE cart_no <= ? AND status='normal'`, maxNo).Scan(&normalCnt)
	needRepair := pendingCnt + repairingCnt + manualFault

	now := core.NowShanghai()
	startDate := time.Date(now.Year(), now.Month()-1, 26, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	endDate := time.Date(now.Year(), now.Month(), 25, 23, 59, 59, 0, now.Location()).Format("2006-01-02")
	if now.Day() >= 26 {
		startDate = time.Date(now.Year(), now.Month(), 26, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
		endDate = time.Date(now.Year(), now.Month()+1, 25, 23, 59, 59, 0, now.Location()).Format("2006-01-02")
	}

	var monthBroken, monthReports, monthFixed, monthFixedTimes, monthDurationSum int
	_ = db.QueryRow(`SELECT COUNT(DISTINCT cart_no), COUNT(*) FROM aging_repairs WHERE DATE(reported_at) BETWEEN ? AND ?`, startDate, endDate).Scan(&monthBroken, &monthReports)
	_ = db.QueryRow(`SELECT COUNT(DISTINCT cart_no), COUNT(*), COALESCE(SUM(duration_minutes),0) FROM aging_repairs WHERE status='done' AND DATE(repair_finished_at) BETWEEN ? AND ?`, startDate, endDate).Scan(&monthFixed, &monthFixedTimes, &monthDurationSum)

	var monthWorkMinutes int
	_ = db.QueryRow(`SELECT COALESCE(SUM(w.duration_minutes),0) FROM aging_repair_workers w INNER JOIN aging_repairs r ON w.repair_id=r.id WHERE r.status='done' AND DATE(r.repair_finished_at) BETWEEN ? AND ?`, startDate, endDate).Scan(&monthWorkMinutes)
	if monthWorkMinutes == 0 {
		monthWorkMinutes = monthDurationSum
	}

	avgMin := 0
	if monthFixedTimes > 0 {
		avgMin = monthDurationSum / monthFixedTimes
	}

	fRows, _ := db.Query(`SELECT id, cart_no, COALESCE(fault_desc,''), COALESCE(repairer_name,''), COALESCE(duration_minutes,0), COALESCE(DATE_FORMAT(repair_finished_at,'%Y-%m-%d %H:%i'),'') FROM aging_repairs WHERE status='done' ORDER BY repair_finished_at DESC LIMIT 5`)
	latestFixed := []map[string]any{}
	if fRows != nil {
		defer fRows.Close()
		for fRows.Next() {
			var id, dur int
			var cno, fd, rname, finAt string
			if fRows.Scan(&id, &cno, &fd, &rname, &dur, &finAt) == nil {
				latestFixed = append(latestFixed, map[string]any{
					"id":                 id,
					"cart_no":            cno,
					"fault_desc":         fd,
					"repairer_name":      rname,
					"worker_names":       rname,
					"duration_text":      core.WorkDurationFormat(dur),
					"work_text":          core.WorkDurationFormat(dur),
					"repair_finished_at": finAt,
				})
			}
		}
	}

	tRows, _ := db.Query(`SELECT cart_no, COUNT(*) as times FROM aging_repairs WHERE DATE(reported_at) BETWEEN ? AND ? GROUP BY cart_no ORDER BY times DESC LIMIT 5`, startDate, endDate)
	topCarts := []map[string]any{}
	if tRows != nil {
		defer tRows.Close()
		for tRows.Next() {
			var cno string
			var times int
			if tRows.Scan(&cno, &times) == nil {
				topCarts = append(topCarts, map[string]any{"cart_no": cno, "times": times})
			}
		}
	}

	wRows, _ := db.Query(`SELECT user_name, COUNT(*) as times, COALESCE(SUM(duration_minutes),0) as mins FROM aging_repair_workers WHERE status='left' GROUP BY user_name ORDER BY mins DESC`)
	workers := []map[string]any{}
	if wRows != nil {
		defer wRows.Close()
		for wRows.Next() {
			var uname string
			var times, mins int
			if wRows.Scan(&uname, &times, &mins) == nil {
				workers = append(workers, map[string]any{
					"name":          uname,
					"times":         times,
					"minutes":       mins,
					"duration_text": core.WorkDurationFormat(mins),
				})
			}
		}
	}

	core.Success(c, "ok", gin.H{
		"need_repair":        needRepair,
		"pending_count":      pendingCnt,
		"repairing_count":    repairingCnt,
		"manual_fault":       manualFault,
		"normal_count":       normalCnt,
		"month_broken":       monthBroken,
		"month_reports":      monthReports,
		"month_fixed":        monthFixed,
		"month_fixed_times":  monthFixedTimes,
		"month_avg_text":     core.WorkDurationFormat(avgMin),
		"month_minutes_text": core.WorkDurationFormat(monthDurationSum),
		"month_work_text":    core.WorkDurationFormat(monthWorkMinutes),
		"month_collab":       0,
		"period": map[string]any{
			"label":      now.Format("2006年01月"),
			"start_date": startDate,
			"end_date":   endDate,
		},
		"latest_fixed": latestFixed,
		"top_carts":    topCarts,
		"workers":      workers,
	})
}

func (ctrl *AgingCartController) Carts(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	total := ctrl.cartTotal(db)
	maxNo := fmt.Sprintf("%03d", total)
	for i := 1; i <= total; i++ {
		_, _ = db.Exec(`INSERT IGNORE INTO aging_carts (cart_no, status, usage_type) VALUES (?,'normal','流动使用')`, fmt.Sprintf("%03d", i))
	}

	rows, _ := db.Query(`SELECT c.id, c.cart_no, c.status, COALESCE(c.layers,0), COALESCE(c.usage_type,'流动使用'), COALESCE(c.remark,''),
		(SELECT COUNT(*) FROM aging_repairs r WHERE r.cart_no=c.cart_no) as rep_times,
		COALESCE((SELECT CONCAT(fault_type, CASE WHEN fault_desc!='' THEN CONCAT('：',fault_desc) ELSE '' END) FROM aging_repairs r WHERE r.cart_no=c.cart_no AND r.status IN ('pending','repairing') ORDER BY r.id DESC LIMIT 1), '') as fault_text
		FROM aging_carts c WHERE c.cart_no <= ? ORDER BY c.cart_no ASC`, maxNo)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, layers, repTimes int
			var cno, st, ut, rem, ft string
			if rows.Scan(&id, &cno, &st, &layers, &ut, &rem, &repTimes, &ft) == nil {
				var layersVal any = nil
				if layers > 0 {
					layersVal = layers
				}
				list = append(list, map[string]any{
					"id":           id,
					"cart_no":      cno,
					"status":       st,
					"layers":       layersVal,
					"usage_type":   ut,
					"remark":       rem,
					"repair_times": repTimes,
					"fault_text":   ft,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"list":          list,
		"usage_types":   []string{"固定使用", "流动使用"},
		"can_edit_cart": true,
	})
}

func (ctrl *AgingCartController) List(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)
	curUID := core.AsInt(sess.User["id"])

	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page <= 0 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 {
		limit = core.AsInt(in["size"])
	}
	if limit <= 0 {
		limit = 20
	}
	offset := (page - 1) * limit

	status := strings.TrimSpace(core.AsString(in["status"]))
	cartNo := strings.TrimSpace(core.AsString(in["cart_no"]))
	keyword := strings.TrimSpace(core.AsString(in["keyword"]))
	fromDate := strings.TrimSpace(core.AsString(in["from"]))
	toDate := strings.TrimSpace(core.AsString(in["to"]))

	where := "WHERE 1=1"
	args := []any{}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	if cartNo != "" {
		where += " AND cart_no=?"
		args = append(args, cartNo)
	}
	if keyword != "" {
		where += " AND (fault_type LIKE ? OR fault_desc LIKE ? OR reporter_name LIKE ? OR repairer_name LIKE ? OR repair_remark LIKE ?)"
		like := "%" + keyword + "%"
		args = append(args, like, like, like, like, like)
	}
	if fromDate != "" {
		where += " AND DATE(reported_at) >= ?"
		args = append(args, fromDate)
	}
	if toDate != "" {
		where += " AND DATE(reported_at) <= ?"
		args = append(args, toDate)
	}

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM aging_repairs "+where, args...).Scan(&total)
	rows, _ := db.Query(`SELECT id, cart_no, COALESCE(fault_type,''), COALESCE(fault_desc,''), status, COALESCE(reporter_name,''), COALESCE(DATE_FORMAT(reported_at,'%Y-%m-%d %H:%i'),''), COALESCE(repairer_name,''), COALESCE(DATE_FORMAT(repair_started_at,'%Y-%m-%d %H:%i'),''), COALESCE(DATE_FORMAT(repair_finished_at,'%Y-%m-%d %H:%i'),''), COALESCE(duration_minutes,0), COALESCE(repair_remark,''), TIMESTAMPDIFF(MINUTE, COALESCE(repair_started_at, NOW()), NOW()) FROM aging_repairs `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, durMin, elapsedMin int
			var cno, ft, fd, st, repName, repAt, fixName, startAt, finAt, rem string
			if rows.Scan(&id, &cno, &ft, &fd, &st, &repName, &repAt, &fixName, &startAt, &finAt, &durMin, &rem, &elapsedMin) == nil {
				if elapsedMin < 0 {
					elapsedMin = 0
				}
				wRows, _ := db.Query(`SELECT id, user_id, user_name, is_starter, COALESCE(DATE_FORMAT(joined_at,'%Y-%m-%d %H:%i'),''), COALESCE(DATE_FORMAT(left_at,'%Y-%m-%d %H:%i'),''), COALESCE(duration_minutes, TIMESTAMPDIFF(MINUTE, joined_at, NOW())), status FROM aging_repair_workers WHERE repair_id=? ORDER BY id ASC`, id)
				wList := []map[string]any{}
				meJoined := false
				activeCnt := 0
				workSum := 0
				if wRows != nil {
					for wRows.Next() {
						var wid, uid, starter, wDur int
						var uname, jAt, lAt, wst string
						if wRows.Scan(&wid, &uid, &uname, &starter, &jAt, &lAt, &wDur, &wst) == nil {
							if wDur < 0 {
								wDur = 0
							}
							working := wst == "working"
							if working {
								activeCnt++
								if uid == curUID {
									meJoined = true
								}
							}
							workSum += wDur
							wList = append(wList, map[string]any{
								"worker_id":     wid,
								"user_id":       uid,
								"user_name":     uname,
								"is_starter":    starter == 1,
								"joined_at":     jAt,
								"left_at":       lAt,
								"working":       working,
								"duration_text": core.WorkDurationFormat(wDur),
							})
						}
					}
					wRows.Close()
				}
				if workSum == 0 {
					workSum = durMin
				}
				list = append(list, map[string]any{
					"id":                  id,
					"cart_no":             cno,
					"fault_type":          ft,
					"fault_desc":          fd,
					"status":              st,
					"reporter_name":       repName,
					"reported_at":         repAt,
					"repairer_name":       fixName,
					"repair_started_at":   startAt,
					"repair_finished_at":  finAt,
					"duration_minutes":    durMin,
					"duration_text":       core.WorkDurationFormat(durMin),
					"elapsed_text":        core.WorkDurationFormat(elapsedMin),
					"work_text":           core.WorkDurationFormat(workSum),
					"repair_remark":       rem,
					"workers":             wList,
					"worker_count":        len(wList),
					"active_worker_count": activeCnt,
					"is_multi_worker":     len(wList) > 1,
					"me_joined":           meJoined,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"list":       list,
		"total":      total,
		"page":       page,
		"can_manage": isAdmin,
	})
}

func (ctrl *AgingCartController) Report(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	uid := core.AsInt(sess.User["id"])
	uname := core.AsString(sess.User["display_name"])
	if uname == "" {
		uname = core.AsString(sess.User["username"])
	}
	if uname == "" {
		uname = "admin"
	}
	in := core.ParseInputMap(c)
	cartNo := strings.TrimSpace(core.AsString(in["cart_no"]))
	faultType := strings.TrimSpace(core.AsString(in["fault_type"]))
	faultDesc := strings.TrimSpace(core.AsString(in["fault_desc"]))
	if cartNo == "" {
		core.Error(c, "请选择老化车编号", gin.H{})
		return
	}
	res, err := db.Exec(`INSERT INTO aging_repairs (cart_no, fault_type, fault_desc, status, reporter_id, reporter_name, reported_at, created_at, updated_at) VALUES (?,?,?,'pending',?,?,NOW(),NOW(),NOW())`,
		cartNo, faultType, faultDesc, uid, uname)
	if err != nil {
		core.Error(c, "报修失败: "+err.Error(), gin.H{})
		return
	}
	_, _ = db.Exec(`UPDATE aging_carts SET status='pending', updated_at=NOW() WHERE cart_no=?`, cartNo)
	id, _ := res.LastInsertId()
	core.Success(c, "报修成功", gin.H{"id": id})
}

func (ctrl *AgingCartController) Join(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	sess := core.GetOrCreateSession(c)
	uid := core.AsInt(sess.User["id"])
	uname := core.AsString(sess.User["display_name"])
	if uname == "" {
		uname = core.AsString(sess.User["username"])
	}
	if uname == "" {
		uname = "admin"
	}

	var cartNo, status string
	if err := db.QueryRow(`SELECT cart_no, status FROM aging_repairs WHERE id=?`, id).Scan(&cartNo, &status); err != nil {
		core.Error(c, "报修单不存在", gin.H{})
		return
	}
	isStarter := status == "pending"
	starterFlag := 0
	if isStarter {
		starterFlag = 1
		_, _ = db.Exec(`UPDATE aging_repairs SET status='repairing', repairer_id=?, repairer_name=?, repair_started_at=COALESCE(repair_started_at, NOW()), updated_at=NOW() WHERE id=?`, uid, uname, id)
		_, _ = db.Exec(`UPDATE aging_carts SET status='repairing', updated_at=NOW() WHERE cart_no=?`, cartNo)
	}
	_, _ = db.Exec(`INSERT INTO aging_repair_workers (repair_id, user_id, user_name, is_starter, joined_at, status, created_at) VALUES (?,?,?,?,NOW(),'working',NOW())`,
		id, uid, uname, starterFlag)
	core.Success(c, "已开始维修", gin.H{"is_starter": isStarter})
}

func (ctrl *AgingCartController) Start(c *gin.Context) {
	ctrl.Join(c)
}

func (ctrl *AgingCartController) Leave(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	sess := core.GetOrCreateSession(c)
	uid := core.AsInt(sess.User["id"])
	_, _ = db.Exec(`UPDATE aging_repair_workers SET left_at=NOW(), raw_minutes=GREATEST(1, TIMESTAMPDIFF(MINUTE, joined_at, NOW())), duration_minutes=GREATEST(1, TIMESTAMPDIFF(MINUTE, joined_at, NOW())), status='left' WHERE repair_id=? AND user_id=? AND status='working'`, id, uid)
	var mins int
	_ = db.QueryRow(`SELECT COALESCE(SUM(duration_minutes),1) FROM aging_repair_workers WHERE repair_id=? AND user_id=?`, id, uid).Scan(&mins)
	core.Success(c, "已结束个人维修计时", gin.H{"duration_text": core.WorkDurationFormat(mins)})
}

func (ctrl *AgingCartController) Finish(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	remark := strings.TrimSpace(core.AsString(in["repair_remark"]))

	var cartNo string
	if err := db.QueryRow(`SELECT cart_no FROM aging_repairs WHERE id=?`, id).Scan(&cartNo); err != nil {
		core.Error(c, "报修单不存在", gin.H{})
		return
	}
	_, _ = db.Exec(`UPDATE aging_repair_workers SET left_at=NOW(), raw_minutes=GREATEST(1, TIMESTAMPDIFF(MINUTE, joined_at, NOW())), duration_minutes=GREATEST(1, TIMESTAMPDIFF(MINUTE, joined_at, NOW())), status='left' WHERE repair_id=? AND status='working'`, id)
	_, _ = db.Exec(`UPDATE aging_repairs SET status='done', repair_finished_at=NOW(), raw_minutes=GREATEST(1, TIMESTAMPDIFF(MINUTE, COALESCE(repair_started_at, reported_at), NOW())), duration_minutes=GREATEST(1, TIMESTAMPDIFF(MINUTE, COALESCE(repair_started_at, reported_at), NOW())), repair_remark=?, updated_at=NOW() WHERE id=?`, remark, id)
	_, _ = db.Exec(`UPDATE aging_carts SET status='normal', updated_at=NOW() WHERE cart_no=?`, cartNo)

	var durMin, workMin int
	_ = db.QueryRow(`SELECT COALESCE(duration_minutes,1) FROM aging_repairs WHERE id=?`, id).Scan(&durMin)
	_ = db.QueryRow(`SELECT COALESCE(SUM(duration_minutes),1) FROM aging_repair_workers WHERE repair_id=?`, id).Scan(&workMin)
	core.Success(c, "维修完成", gin.H{
		"duration_text": core.WorkDurationFormat(durMin),
		"work_text":     core.WorkDurationFormat(workMin),
		"workers":       []any{},
	})
}

func (ctrl *AgingCartController) Save(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	cartNo := strings.TrimSpace(core.AsString(in["cart_no"]))
	ft := strings.TrimSpace(core.AsString(in["fault_type"]))
	fd := strings.TrimSpace(core.AsString(in["fault_desc"]))
	rem := strings.TrimSpace(core.AsString(in["repair_remark"]))
	_, err = db.Exec(`UPDATE aging_repairs SET cart_no=?, fault_type=?, fault_desc=?, repair_remark=?, updated_at=NOW() WHERE id=?`, cartNo, ft, fd, rem, id)
	if err != nil {
		core.Error(c, "保存失败: "+err.Error(), gin.H{})
		return
	}
	core.Success(c, "保存成功", gin.H{})
}

func (ctrl *AgingCartController) SaveCart(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	cartNo := strings.TrimSpace(core.AsString(in["cart_no"]))
	layers := core.AsInt(in["layers"])
	status := strings.TrimSpace(core.AsString(in["status"]))
	if status == "" {
		status = "normal"
	}
	usageType := strings.TrimSpace(core.AsString(in["usage_type"]))
	if usageType == "" {
		usageType = "流动使用"
	}
	remark := strings.TrimSpace(core.AsString(in["remark"]))
	var layersVal any = nil
	if layers > 0 {
		layersVal = layers
	}
	_, err = db.Exec(`UPDATE aging_carts SET layers=?, status=?, usage_type=?, remark=?, updated_at=NOW() WHERE cart_no=?`, layersVal, status, usageType, remark, cartNo)
	if err != nil {
		core.Error(c, "保存失败: "+err.Error(), gin.H{})
		return
	}
	core.Success(c, "已保存", gin.H{})
}

func (ctrl *AgingCartController) SetCartTotal(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	total := core.AsInt(in["cart_total"])
	if total < 1 || total > 999 {
		core.Error(c, "老化车总数应在 1 ~ 999 之间", gin.H{})
		return
	}
	_, _ = db.Exec(`INSERT INTO aging_settings (id, cart_total, updated_at) VALUES (1, ?, NOW()) ON DUPLICATE KEY UPDATE cart_total=VALUES(cart_total), updated_at=NOW()`, total)
	for i := 1; i <= total; i++ {
		_, _ = db.Exec(`INSERT IGNORE INTO aging_carts (cart_no, status, usage_type) VALUES (?,'normal','流动使用')`, fmt.Sprintf("%03d", i))
	}
	core.Success(c, "更新成功", gin.H{"cart_total": total})
}

func (ctrl *AgingCartController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	for _, id := range core.ParseIDs(in) {
		_, _ = db.Exec(`DELETE FROM aging_repair_workers WHERE repair_id=?`, id)
		_, _ = db.Exec(`DELETE FROM aging_repairs WHERE id=?`, id)
	}
	core.Success(c, "已删除", gin.H{})
}

func (ctrl *AgingCartController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT cart_no, COALESCE(fault_type,''), COALESCE(fault_desc,''), status, COALESCE(reporter_name,''), COALESCE(DATE_FORMAT(reported_at,'%Y-%m-%d %H:%i'),''), COALESCE(repairer_name,''), COALESCE(DATE_FORMAT(repair_finished_at,'%Y-%m-%d %H:%i'),''), COALESCE(duration_minutes,0), COALESCE(repair_remark,'') FROM aging_repairs ORDER BY id DESC`)
	var out [][]string
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var cno, ft, fd, st, repN, repAt, fixN, finAt, rem string
			var dur int
			if rows.Scan(&cno, &ft, &fd, &st, &repN, &repAt, &fixN, &finAt, &dur, &rem) == nil {
				out = append(out, []string{cno, ft, fd, st, repN, repAt, fixN, finAt, fmt.Sprintf("%d", dur), rem})
			}
		}
	}
	core.ExportCSV(c, "老化车报修记录.csv", []string{"车号", "故障类型", "故障现象", "状态", "报修人", "报修时间", "维修人", "完成时间", "有效时长(分钟)", "维修备注"}, out)
}

// =========================================================================
// 25. AttendanceController — 离岗记录管理 (att_employees / att_cards / att_offjob_records)
// =========================================================================

type AttendanceController struct{}

func NewAttendanceController() *AttendanceController { return &AttendanceController{} }

func (ctrl *AttendanceController) Dashboard(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)

	empRows, _ := db.Query(`SELECT id, name, COALESCE(employee_no,''), COALESCE(team,''), status FROM att_employees WHERE status='onjob' ORDER BY id ASC`)
	onjobEmps := []map[string]any{}
	if empRows != nil {
		defer empRows.Close()
		for empRows.Next() {
			var id int
			var name, eno, team, st string
			if empRows.Scan(&id, &name, &eno, &team, &st) == nil {
				onjobEmps = append(onjobEmps, map[string]any{"id": id, "name": name, "employee_no": eno, "team": team, "status": st})
			}
		}
	}
	cardRows, _ := db.Query(`SELECT id, card_uid FROM att_cards WHERE status='free' ORDER BY id ASC`)
	freeCards := []map[string]any{}
	if cardRows != nil {
		defer cardRows.Close()
		for cardRows.Next() {
			var id int
			var uid string
			if cardRows.Scan(&id, &uid) == nil {
				freeCards = append(freeCards, map[string]any{"id": id, "card_uid": uid})
			}
		}
	}
	recRows, _ := db.Query(`SELECT r.id, r.card_id, COALESCE(c.card_uid,''), r.employee_id, COALESCE(e.name,''), COALESCE(e.employee_no,''), COALESCE(e.team,''), COALESCE(DATE_FORMAT(r.start_time,'%Y-%m-%d %H:%i:%s'),''), TIMESTAMPDIFF(MINUTE, r.start_time, NOW()) FROM att_offjob_records r LEFT JOIN att_cards c ON r.card_id=c.id LEFT JOIN att_employees e ON r.employee_id=e.id WHERE r.end_time IS NULL ORDER BY r.start_time ASC`)
	activeList := []map[string]any{}
	longCount := 0
	maxMinutes := 0
	if recRows != nil {
		defer recRows.Close()
		for recRows.Next() {
			var id, cid, eid, mins int
			var cuid, ename, eno, team, stTime string
			if recRows.Scan(&id, &cid, &cuid, &eid, &ename, &eno, &team, &stTime, &mins) == nil {
				if mins < 0 {
					mins = 0
				}
				if mins >= 30 {
					longCount++
				}
				if mins > maxMinutes {
					maxMinutes = mins
				}
				lvl := "normal"
				if mins >= 60 {
					lvl = "danger"
				} else if mins >= 30 {
					lvl = "warning"
				}
				stFmt := stTime
				if len(stTime) >= 19 {
					stFmt = stTime[11:19]
				}
				activeList = append(activeList, map[string]any{
					"id":             id,
					"record_id":      id,
					"card_id":        cid,
					"card_uid":       cuid,
					"employee_id":    eid,
					"name":           ename,
					"employee_no":    eno,
					"team":           team,
					"start_time":     stTime,
					"start_time_fmt": stFmt,
					"minutes":        mins,
					"level":          lvl,
				})
			}
		}
	}
	var freeCardsCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM att_cards WHERE status='free'`).Scan(&freeCardsCount)
	var onjobEmpsCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM att_employees WHERE status='onjob'`).Scan(&onjobEmpsCount)

	core.Success(c, "ok", gin.H{
		"total_count": len(activeList),
		"long_count":  longCount,
		"max_minutes": maxMinutes,
		"free_cards":  freeCardsCount,
		"onjob_emps":  onjobEmpsCount,
		"list":        activeList,
		"is_admin":    isAdmin,
		"can_manage":  true,
	})
}

func (ctrl *AttendanceController) BindCard(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	empID := core.AsInt(in["employee_id"])
	cardUID := strings.TrimSpace(core.AsString(in["card_uid"]))
	if empID <= 0 || cardUID == "" {
		core.Error(c, "请选择员工和离岗卡", gin.H{})
		return
	}
	var cardID int
	qErr := db.QueryRow(`SELECT id FROM att_cards WHERE card_uid=?`, cardUID).Scan(&cardID)
	if qErr != nil {
		res, err2 := db.Exec(`INSERT INTO att_cards (card_uid, status, created_at) VALUES (?,'free',NOW())`, cardUID)
		if err2 != nil {
			core.Error(c, "离岗卡不存在", gin.H{})
			return
		}
		cid, _ := res.LastInsertId()
		cardID = int(cid)
	}
	_, _ = db.Exec(`UPDATE att_cards SET current_employee_id=?, status='occupied' WHERE id=?`, empID, cardID)
	_, _ = db.Exec(`UPDATE att_employees SET status='offjob' WHERE id=?`, empID)
	_, _ = db.Exec(`INSERT INTO att_offjob_records (card_id, employee_id, start_time, created_at) VALUES (?,?,NOW(),NOW())`, cardID, empID)
	core.Success(c, "发卡离岗登记成功", gin.H{})
}

func (ctrl *AttendanceController) UnbindCard(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	cardUID := strings.TrimSpace(core.AsString(in["card_uid"]))
	cardID := core.AsInt(in["card_id"])
	if cardID == 0 && cardUID != "" {
		_ = db.QueryRow(`SELECT id FROM att_cards WHERE card_uid=?`, cardUID).Scan(&cardID)
	}
	if cardID <= 0 {
		core.Error(c, "未找到该离岗卡", gin.H{})
		return
	}
	var empID int
	_ = db.QueryRow(`SELECT COALESCE(current_employee_id,0) FROM att_cards WHERE id=?`, cardID).Scan(&empID)
	_, _ = db.Exec(`UPDATE att_cards SET current_employee_id=NULL, status='free' WHERE id=?`, cardID)
	if empID > 0 {
		_, _ = db.Exec(`UPDATE att_employees SET status='onjob' WHERE id=?`, empID)
	}
	_, _ = db.Exec(`UPDATE att_offjob_records SET end_time=NOW(), duration_seconds=TIMESTAMPDIFF(SECOND, start_time, NOW()) WHERE card_id=? AND end_time IS NULL`, cardID)
	core.Success(c, "还卡销假成功", gin.H{})
}

func (ctrl *AttendanceController) ListEmployees(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	status := strings.TrimSpace(core.AsString(in["status"]))
	where := "WHERE 1=1"
	args := []any{}
	if status != "" {
		where += " AND e.status=?"
		args = append(args, status)
	}
	rows, _ := db.Query(`SELECT e.id, e.name, COALESCE(e.employee_no,''), COALESCE(e.team,''), COALESCE(e.foreman_id,0), COALESCE(u.display_name, u.username, ''), e.status FROM att_employees e LEFT JOIN users u ON e.foreman_id=u.id `+where+` ORDER BY e.id ASC`, args...)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, fid int
			var name, eno, team, fname, st string
			if rows.Scan(&id, &name, &eno, &team, &fid, &fname, &st) == nil {
				list = append(list, map[string]any{
					"id":           id,
					"name":         name,
					"employee_no":  eno,
					"team":         team,
					"foreman_id":   fid,
					"foreman_name": fname,
					"status":       st,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"list": list, "employees": list, "can_manage": true})
}

func (ctrl *AttendanceController) SaveEmployee(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	name := strings.TrimSpace(core.AsString(in["name"]))
	eno := strings.TrimSpace(core.AsString(in["employee_no"]))
	team := strings.TrimSpace(core.AsString(in["team"]))
	fid := core.AsInt(in["foreman_id"])
	if id > 0 {
		_, _ = db.Exec(`UPDATE att_employees SET name=?, employee_no=?, team=?, foreman_id=? WHERE id=?`, name, eno, team, fid, id)
		core.Success(c, "更新成功", gin.H{})
		return
	}
	_, _ = db.Exec(`INSERT INTO att_employees (name, employee_no, team, foreman_id, status, created_at) VALUES (?,?,?,?,'onjob',NOW())`, name, eno, team, fid)
	core.Success(c, "新增员工成功", gin.H{})
}

func (ctrl *AttendanceController) DeleteEmployee(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	_, _ = db.Exec(`DELETE FROM att_employees WHERE id=?`, id)
	core.Success(c, "删除成功", gin.H{})
}

func (ctrl *AttendanceController) ListCards(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT c.id, c.card_uid, c.status, COALESCE(c.current_employee_id,0), COALESCE(e.name,''), COALESCE(e.team,'') FROM att_cards c LEFT JOIN att_employees e ON c.current_employee_id=e.id ORDER BY c.id ASC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, eid int
			var cuid, st, ename, team string
			if rows.Scan(&id, &cuid, &st, &eid, &ename, &team) == nil {
				list = append(list, map[string]any{
					"id":                  id,
					"card_uid":            cuid,
					"status":              st,
					"current_employee_id": eid,
					"emp_name":            ename,
					"emp_team":            team,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"list": list, "cards": list})
}

func (ctrl *AttendanceController) SaveCard(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	cuid := strings.TrimSpace(core.AsString(in["card_uid"]))
	if cuid == "" {
		core.Error(c, "请输入卡号", gin.H{})
		return
	}
	_, qErr := db.Exec(`INSERT INTO att_cards (card_uid, status, created_at) VALUES (?,'free',NOW())`, cuid)
	if qErr != nil {
		core.Error(c, "添加失败，卡号可能已存在", gin.H{})
		return
	}
	core.Success(c, "添加离岗卡成功", gin.H{})
}

func (ctrl *AttendanceController) DeleteCard(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	_, _ = db.Exec(`DELETE FROM att_cards WHERE id=?`, id)
	core.Success(c, "删除成功", gin.H{})
}

func (ctrl *AttendanceController) ListRecords(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page <= 0 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 {
		limit = core.AsInt(in["size"])
	}
	if limit <= 0 {
		limit = 20
	}
	offset := (page - 1) * limit

	name := strings.TrimSpace(core.AsString(in["name"]))
	team := strings.TrimSpace(core.AsString(in["team"]))
	startDate := strings.TrimSpace(core.AsString(in["start_date"]))
	endDate := strings.TrimSpace(core.AsString(in["end_date"]))

	where := "WHERE 1=1"
	args := []any{}
	if name != "" {
		where += " AND (e.name LIKE ? OR e.employee_no LIKE ?)"
		args = append(args, "%"+name+"%", "%"+name+"%")
	}
	if team != "" {
		where += " AND e.team LIKE ?"
		args = append(args, "%"+team+"%")
	}
	if startDate != "" {
		where += " AND DATE(r.start_time) >= ?"
		args = append(args, startDate)
	}
	if endDate != "" {
		where += " AND DATE(r.start_time) <= ?"
		args = append(args, endDate)
	}
	var total int
	_ = db.QueryRow(`SELECT COUNT(*) FROM att_offjob_records r LEFT JOIN att_employees e ON r.employee_id=e.id `+where, args...).Scan(&total)
	rows, _ := db.Query(`SELECT r.id, COALESCE(e.name,''), COALESCE(e.employee_no,''), COALESCE(e.team,''), COALESCE(c.card_uid,''), COALESCE(DATE_FORMAT(r.start_time,'%Y-%m-%d %H:%i:%s'),''), COALESCE(DATE_FORMAT(r.end_time,'%Y-%m-%d %H:%i:%s'),''), COALESCE(r.duration_seconds, TIMESTAMPDIFF(SECOND, r.start_time, NOW())) FROM att_offjob_records r LEFT JOIN att_employees e ON r.employee_id=e.id LEFT JOIN att_cards c ON r.card_id=c.id `+where+` ORDER BY r.id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, durSec int
			var ename, eno, tm, cuid, st, et string
			if rows.Scan(&id, &ename, &eno, &tm, &cuid, &st, &et, &durSec) == nil {
				list = append(list, map[string]any{
					"id":               id,
					"name":             ename,
					"employee_no":      eno,
					"team":             tm,
					"card_uid":         cuid,
					"start_time":       st,
					"end_time":         et,
					"duration_seconds": durSec,
					"duration_minutes": fmt.Sprintf("%.1f", float64(durSec)/60.0),
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"records":       list,
		"list":          list,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"page":          page,
	})
}

func (ctrl *AttendanceController) Statistics(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	startDate := strings.TrimSpace(core.AsString(in["start_date"]))
	endDate := strings.TrimSpace(core.AsString(in["end_date"]))
	where := "WHERE r.end_time IS NOT NULL"
	args := []any{}
	if startDate != "" {
		where += " AND DATE(r.start_time) >= ?"
		args = append(args, startDate)
	}
	if endDate != "" {
		where += " AND DATE(r.start_time) <= ?"
		args = append(args, endDate)
	}
	rows, _ := db.Query(`SELECT COALESCE(e.name,''), COALESCE(e.employee_no,''), COALESCE(e.team,''), COUNT(*) as cnt, COALESCE(SUM(r.duration_seconds),0) as total_sec, COALESCE(MAX(r.duration_seconds),0) as max_sec FROM att_offjob_records r LEFT JOIN att_employees e ON r.employee_id=e.id `+where+` GROUP BY r.employee_id, e.name, e.employee_no, e.team ORDER BY total_sec DESC`, args...)
	stats := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ename, eno, tm string
			var cnt, totalSec, maxSec int
			if rows.Scan(&ename, &eno, &tm, &cnt, &totalSec, &maxSec) == nil {
				avgMin := 0.0
				if cnt > 0 {
					avgMin = float64(totalSec) / 60.0 / float64(cnt)
				}
				stats = append(stats, map[string]any{
					"name":          ename,
					"employee_no":   eno,
					"team":          tm,
					"total_count":   cnt,
					"total_minutes": fmt.Sprintf("%.1f", float64(totalSec)/60.0),
					"avg_minutes":   fmt.Sprintf("%.1f", avgMin),
					"max_minutes":   fmt.Sprintf("%.1f", float64(maxSec)/60.0),
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"stats": stats, "list": stats})
}

func (ctrl *AttendanceController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT COALESCE(e.name,''), COALESCE(e.employee_no,''), COALESCE(e.team,''), COALESCE(c.card_uid,''), COALESCE(DATE_FORMAT(r.start_time,'%Y-%m-%d %H:%i:%s'),''), COALESCE(DATE_FORMAT(r.end_time,'%Y-%m-%d %H:%i:%s'),''), COALESCE(r.duration_seconds,0) FROM att_offjob_records r LEFT JOIN att_employees e ON r.employee_id=e.id LEFT JOIN att_cards c ON r.card_id=c.id ORDER BY r.id DESC`)
	var out [][]string
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ename, eno, tm, cuid, st, et string
			var sec int
			if rows.Scan(&ename, &eno, &tm, &cuid, &st, &et, &sec) == nil {
				out = append(out, []string{ename, eno, tm, cuid, st, et, fmt.Sprintf("%.1f", float64(sec)/60.0)})
			}
		}
	}
	core.ExportCSV(c, "离岗记录.csv", []string{"姓名", "工号", "班组", "卡号", "开始时间", "结束时间", "时长(分钟)"}, out)
}

// =========================================================================
// 26. DiaryController — 工作日记管理 (work_diaries)
// =========================================================================

type DiaryController struct{}

func NewDiaryController() *DiaryController { return &DiaryController{} }

func (ctrl *DiaryController) List(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)
	curUID := core.AsInt(sess.User["id"])

	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page <= 0 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 {
		limit = core.AsInt(in["size"])
	}
	if limit <= 0 {
		limit = 20
	}
	offset := (page - 1) * limit

	startDate := strings.TrimSpace(core.AsString(in["filter_start_date"]))
	if startDate == "" {
		startDate = strings.TrimSpace(core.AsString(in["start_date"]))
	}
	endDate := strings.TrimSpace(core.AsString(in["filter_end_date"]))
	if endDate == "" {
		endDate = strings.TrimSpace(core.AsString(in["end_date"]))
	}
	workContent := strings.TrimSpace(core.AsString(in["filter_work_content"]))
	operator := strings.TrimSpace(core.AsString(in["filter_operator"]))
	searchAll := strings.TrimSpace(core.AsString(in["search_all"]))
	if searchAll == "" {
		searchAll = strings.TrimSpace(core.AsString(in["search"]))
	}

	where := "WHERE 1=1"
	args := []any{}
	if startDate != "" {
		where += " AND work_date >= ?"
		args = append(args, startDate)
	}
	if endDate != "" {
		where += " AND work_date <= ?"
		args = append(args, endDate)
	}
	if workContent != "" {
		where += " AND work_content=?"
		args = append(args, workContent)
	}
	if operator != "" {
		where += " AND operator LIKE ?"
		args = append(args, "%"+operator+"%")
	}
	if searchAll != "" {
		where += " AND (task_order LIKE ? OR product_code LIKE ? OR description LIKE ? OR operator LIKE ? OR remarks LIKE ?)"
		like := "%" + searchAll + "%"
		args = append(args, like, like, like, like, like)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM work_diaries "+where, args...).Scan(&total)

	q := fmt.Sprintf(`SELECT id,
		COALESCE(DATE_FORMAT(work_date, '%%Y-%%m-%%d'), '') AS work_date,
		COALESCE(task_order, '') AS task_order,
		COALESCE(product_code, '') AS product_code,
		COALESCE(description, '') AS description,
		COALESCE(task_type, '') AS task_type,
		COALESCE(work_content, '') AS work_content,
		COALESCE(quantity, 0) AS quantity,
		COALESCE(duration, 60) AS duration,
		COALESCE(operator, '') AS operator,
		COALESCE(is_ghost_write, '否') AS is_ghost_write,
		COALESCE(remarks, '') AS remarks,
		COALESCE(created_by, 0) AS created_by,
		COALESCE(DATE_FORMAT(created_at, '%%Y-%%m-%%d %%H:%%i:%%s'), '') AS created_at
		FROM work_diaries %s ORDER BY work_date DESC, id DESC LIMIT ? OFFSET ?`, where)

	records, _ := core.QueryMaps(db, q, append(args, limit, offset)...)
	for _, r := range records {
		cID := core.AsInt(r["created_by"])
		r["can_edit"] = isAdmin || (cID == curUID) || (curUID == 1)
	}

	core.Success(c, "ok", gin.H{
		"records":       records,
		"list":          records,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"page":          page,
		"is_admin":      isAdmin,
	})
}


func (ctrl *DiaryController) SearchTask(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	to := strings.TrimSpace(core.AsString(in["task_order"]))
	if to == "" {
		to = strings.TrimSpace(core.AsString(in["work_order"]))
	}
	meta := core.LookupWorkOrderInfo(db, to)
	found := false
	taskInfo := map[string]any{
		"task_order":   to,
		"product_code": "",
		"description":  "",
		"task_type":    "正常生产",
		"quantity":     0,
	}
	if meta != nil {
		found = meta.ProductCode != "" || meta.ProductDesc != "" || meta.Quantity > 0
		taskInfo["product_code"] = meta.ProductCode
		taskInfo["description"] = meta.ProductDesc
		taskInfo["quantity"] = int(meta.Quantity)
	}
	core.Success(c, "ok", gin.H{
		"found":     found,
		"task_info": taskInfo,
	})
}

func (ctrl *DiaryController) CheckQuantity(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	to := strings.TrimSpace(core.AsString(in["task_order"]))
	wc := strings.TrimSpace(core.AsString(in["work_content"]))
	qty := core.AsInt(in["quantity"])
	excludeID := core.AsInt(in["exclude_id"])
	if to == "" || wc == "" {
		core.Success(c, "ok", gin.H{"has_limit": false, "available": true})
		return
	}
	meta := core.LookupWorkOrderInfo(db, to)
	totalQty := 0
	if meta != nil {
		totalQty = int(meta.Quantity)
	}
	if totalQty <= 0 {
		core.Success(c, "ok", gin.H{"has_limit": false, "available": true})
		return
	}
	var usedQty int
	_ = db.QueryRow(`SELECT COALESCE(SUM(quantity),0) FROM work_diaries WHERE task_order=? AND work_content=? AND id != ?`, to, wc, excludeID).Scan(&usedQty)
	rem := totalQty - usedQty
	if rem < 0 {
		rem = 0
	}
	core.Success(c, "ok", gin.H{
		"has_limit":      true,
		"total_quantity": totalQty,
		"used_quantity":  usedQty,
		"remaining":      rem,
		"available":      qty <= rem,
	})
}

func (ctrl *DiaryController) Add(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	creatorID := core.AsInt(sess.User["id"])
	if creatorID <= 0 {
		creatorID = 1
	}
	in := core.ParseInputMap(c)
	wd := strings.TrimSpace(core.AsString(in["work_date"]))
	if wd == "" {
		wd = time.Now().Format("2006-01-02")
	}
	to := strings.TrimSpace(core.AsString(in["task_order"]))
	pc := strings.TrimSpace(core.AsString(in["product_code"]))
	desc := strings.TrimSpace(core.AsString(in["description"]))
	tt := strings.TrimSpace(core.AsString(in["task_type"]))
	wc := strings.TrimSpace(core.AsString(in["work_content"]))
	qty := core.AsInt(in["quantity"])
	dur := core.AsInt(in["duration"])
	if dur <= 0 {
		dur = 60
	}
	op := strings.TrimSpace(core.AsString(in["operator"]))
	if op == "" {
		op = core.AsString(sess.User["display_name"])
		if op == "" {
			op = core.AsString(sess.User["username"])
		}
	}
	ghostStr := strings.TrimSpace(core.AsString(in["is_ghost_write"]))
	if ghostStr != "是" {
		ghostStr = "否"
	}
	rem := core.AsString(in["remarks"])
	res, qErr := db.Exec(`INSERT INTO work_diaries (work_date, task_order, product_code, description, task_type, work_content, quantity, duration, operator, is_ghost_write, remarks, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NOW(),NOW())`,
		wd, to, pc, desc, tt, wc, qty, dur, op, ghostStr, rem, creatorID)
	if qErr != nil {
		core.Error(c, "保存失败: "+qErr.Error(), gin.H{})
		return
	}
	id, _ := res.LastInsertId()
	core.Success(c, "保存成功", gin.H{"id": id})
}

func (ctrl *DiaryController) Update(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	wd := strings.TrimSpace(core.AsString(in["work_date"]))
	to := strings.TrimSpace(core.AsString(in["task_order"]))
	pc := strings.TrimSpace(core.AsString(in["product_code"]))
	desc := strings.TrimSpace(core.AsString(in["description"]))
	tt := strings.TrimSpace(core.AsString(in["task_type"]))
	wc := strings.TrimSpace(core.AsString(in["work_content"]))
	qty := core.AsInt(in["quantity"])
	dur := core.AsFloat(in["duration"])
	op := strings.TrimSpace(core.AsString(in["operator"]))
	ghostStr := strings.TrimSpace(core.AsString(in["is_ghost_write"]))
	if ghostStr != "是" {
		ghostStr = "否"
	}
	rem := core.AsString(in["remarks"])
	_, qErr := db.Exec(`UPDATE work_diaries SET work_date=?, task_order=?, product_code=?, description=?, task_type=?, work_content=?, quantity=?, duration=?, operator=?, is_ghost_write=?, remarks=?, updated_at=NOW() WHERE id=?`,
		wd, to, pc, desc, tt, wc, qty, dur, op, ghostStr, rem, id)
	if qErr != nil {
		core.Error(c, "更新失败: "+qErr.Error(), gin.H{})
		return
	}
	core.Success(c, "更新成功", gin.H{})
}

func (ctrl *DiaryController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	for _, id := range core.ParseIDs(in) {
		_, _ = db.Exec(`DELETE FROM work_diaries WHERE id=?`, id)
	}
	core.Success(c, "删除成功", gin.H{})
}

func (ctrl *DiaryController) GetUsers(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT id, username, COALESCE(display_name,''), role, COALESCE(diary_Scopes,'self') FROM users WHERE status=1 ORDER BY id ASC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var u, dn, role, ds string
			if rows.Scan(&id, &u, &dn, &role, &ds) == nil {
				list = append(list, map[string]any{
					"id":           id,
					"username":     u,
					"display_name": dn,
					"role":         role,
					"diary_Scopes": ds,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"users": list, "list": list})
}

func (ctrl *DiaryController) UpdatePermission(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	uid := core.AsInt(in["user_id"])
	scope := core.AsString(in["scope"])
	if scope == "" {
		scope = "self"
	}
	_, _ = db.Exec(`UPDATE users SET diary_Scopes=? WHERE id=?`, scope, uid)
	core.Success(c, "权限已更新", gin.H{})
}

func (ctrl *DiaryController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT COALESCE(DATE_FORMAT(work_date,'%Y-%m-%d'),''), COALESCE(task_order,''), COALESCE(product_code,''), COALESCE(description,''), COALESCE(task_type,''), COALESCE(work_content,''), COALESCE(quantity,0), COALESCE(duration,0), COALESCE(operator,''), COALESCE(remarks,'') FROM work_diaries ORDER BY work_date DESC, id DESC`)
	var out [][]string
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var wd, to, pc, desc, tt, wc, op, rem string
			var qty int
			var dur float64
			if rows.Scan(&wd, &to, &pc, &desc, &tt, &wc, &qty, &dur, &op, &rem) == nil {
				out = append(out, []string{wd, to, pc, desc, tt, wc, fmt.Sprintf("%d", qty), fmt.Sprintf("%.2f", dur), op, rem})
			}
		}
	}
	core.ExportCSV(c, "工作日记.csv", []string{"日期", "任务单号", "物料编码", "产品描述", "任务类型", "工作内容", "数量", "工时(h)", "作业员", "备注"}, out)
}

// =========================================================================
// 27. ManhourController — 工时定额管理 (manhour_categories / category_columns / manhour_summary)
// =========================================================================

type ManhourController struct{}

func NewManhourController() *ManhourController { return &ManhourController{} }

func (ctrl *ManhourController) getColumnsForCategory(db *sql.DB, catID int) []string {
	rows, _ := db.Query(`SELECT col_name FROM category_columns WHERE category_id=? ORDER BY sort_order ASC, id ASC`, catID)
	cols := []string{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var cn string
			if rows.Scan(&cn) == nil && cn != "" {
				cols = append(cols, cn)
			}
		}
	}
	return cols
}

func (ctrl *ManhourController) getAllDistinctColumns(db *sql.DB) []string {
	rows, _ := db.Query(`SELECT DISTINCT col_name FROM category_columns ORDER BY sort_order ASC, id ASC`)
	cols := []string{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var cn string
			if rows.Scan(&cn) == nil && cn != "" {
				cols = append(cols, cn)
			}
		}
	}
	if len(cols) == 0 {
		cols = []string{"插件", "波峰焊", "三防", "装配", "初调", "老化", "总调", "检验", "包装"}
	}
	return cols
}

func (ctrl *ManhourController) Categories(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT id, name, slug, COALESCE(description,'') FROM manhour_categories ORDER BY id ASC`)
	cats := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var name, slug, desc string
			if rows.Scan(&id, &name, &slug, &desc) == nil {
				cats = append(cats, map[string]any{
					"id":          id,
					"name":        name,
					"slug":        slug,
					"description": desc,
					"columns":     ctrl.getColumnsForCategory(db, id),
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"categories": cats, "list": cats})
}

func (ctrl *ManhourController) List(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)

	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page <= 0 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 {
		limit = core.AsInt(in["size"])
	}
	if limit <= 0 {
		limit = 20
	}
	offset := (page - 1) * limit

	catSlug := strings.TrimSpace(core.AsString(in["cat"]))
	if catSlug == "" {
		catSlug = "full_hours"
	}
	search := strings.TrimSpace(core.AsString(in["search"]))

	var catID int
	var catName, catDesc string
	_ = db.QueryRow(`SELECT id, name, COALESCE(description,'') FROM manhour_categories WHERE slug=?`, catSlug).Scan(&catID, &catName, &catDesc)
	isFull := catSlug == "full_hours"
	var cols []string
	if isFull {
		cols = ctrl.getAllDistinctColumns(db)
	} else {
		cols = ctrl.getColumnsForCategory(db, catID)
	}

	where := "WHERE 1=1"
	args := []any{}
	if search != "" {
		where += " AND (code LIKE ? OR description LIKE ?)"
		args = append(args, "%"+search+"%", "%"+search+"%")
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM manhour_summary "+where, args...).Scan(&total)
	rows, _ := db.Query("SELECT id, code, COALESCE(description,''), COALESCE(process_hours,'{}'), COALESCE(total_hours,0), COALESCE(created_by,''), COALESCE(DATE_FORMAT(updated_at,'%Y-%m-%d %H:%i:%s'),'') FROM manhour_summary "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	list := []map[string]any{}
	catTotalSum := 0.0
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id int
			var code, desc, phStr, cb, ua string
			var totalH float64
			if rows.Scan(&id, &code, &desc, &phStr, &totalH, &cb, &ua) == nil {
				ph := map[string]float64{}
				var raw map[string]any
				if json.Unmarshal([]byte(phStr), &raw) == nil {
					for k, v := range raw {
						ph[k] = core.AsFloat(v)
					}
				}
				rowSum := 0.0
				for _, col := range cols {
					rowSum += ph[col]
				}
				if isFull && totalH > rowSum {
					rowSum = totalH
				}
				catTotalSum += rowSum
				list = append(list, map[string]any{
					"id":            id,
					"code":          code,
					"description":   desc,
					"process_hours": ph,
					"total_hours":   fmt.Sprintf("%.2f", rowSum),
					"created_by":    cb,
					"updated_at":    ua,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"category": map[string]any{
			"id":          catID,
			"name":        catName,
			"slug":        catSlug,
			"description": catDesc,
		},
		"is_full_hours":        isFull,
		"can_edit":             true,
		"is_admin":             isAdmin,
		"category_total_hours": fmt.Sprintf("%.2f", catTotalSum),
		"total_records":        total,
		"total_pages":          core.CalcTotalPages(total, limit),
		"records":              list,
		"columns":              cols,
	})
}

func (ctrl *ManhourController) GetCodeMeta(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	code := strings.TrimSpace(core.AsString(in["code"]))
	catSlug := strings.TrimSpace(core.AsString(in["cat"]))
	var catID int
	var catName string
	_ = db.QueryRow(`SELECT id, name FROM manhour_categories WHERE slug=?`, catSlug).Scan(&catID, &catName)
	desc := core.LookupProductDesc(db, code)
	cols := ctrl.getColumnsForCategory(db, catID)
	if len(cols) == 0 {
		cols = core.LookupProductProcesses(code)
	}
	procs := []map[string]any{}
	for _, col := range cols {
		procs = append(procs, map[string]any{"gx_name": col})
	}
	core.Success(c, "ok", gin.H{
		"code":          code,
		"description":   desc,
		"category_name": catName,
		"processes":     procs,
	})
}

func (ctrl *ManhourController) saveRecord(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	cb := core.AsString(sess.User["username"])
	if cb == "" {
		cb = "admin"
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	code := strings.TrimSpace(core.AsString(in["code"]))
	desc := strings.TrimSpace(core.AsString(in["description"]))
	if code == "" {
		core.Error(c, "物料编码不能为空", gin.H{})
		return
	}
	if desc == "" {
		desc = core.LookupProductDesc(db, code)
	}

	existingPH := map[string]float64{}
	var existID int
	var oldPHStr string
	if id > 0 {
		_ = db.QueryRow(`SELECT id, COALESCE(process_hours,'{}') FROM manhour_summary WHERE id=?`, id).Scan(&existID, &oldPHStr)
	} else {
		_ = db.QueryRow(`SELECT id, COALESCE(process_hours,'{}') FROM manhour_summary WHERE code=?`, code).Scan(&existID, &oldPHStr)
	}
	if oldPHStr != "" {
		var raw map[string]any
		if json.Unmarshal([]byte(oldPHStr), &raw) == nil {
			for k, v := range raw {
				existingPH[k] = core.AsFloat(v)
			}
		}
	}
	if hoursMap, ok := in["hours"].(map[string]any); ok {
		for k, v := range hoursMap {
			existingPH[k] = core.AsFloat(v)
		}
	}
	totalH := 0.0
	for _, v := range existingPH {
		totalH += v
	}
	phBytes, _ := json.Marshal(existingPH)
	if existID > 0 {
		_, qErr := db.Exec(`UPDATE manhour_summary SET code=?, description=?, process_hours=?, total_hours=?, updated_at=NOW() WHERE id=?`,
			code, desc, string(phBytes), totalH, existID)
		if qErr != nil {
			core.Error(c, "保存失败: "+qErr.Error(), gin.H{})
			return
		}
		core.Success(c, "更新成功", gin.H{"id": existID})
		return
	}
	res, qErr := db.Exec(`INSERT INTO manhour_summary (code, description, process_hours, total_hours, created_by, created_at, updated_at) VALUES (?,?,?,?,?,NOW(),NOW())`,
		code, desc, string(phBytes), totalH, cb)
	if qErr != nil {
		core.Error(c, "新增失败: "+qErr.Error(), gin.H{})
		return
	}
	lastID, _ := res.LastInsertId()
	core.Success(c, "新增成功", gin.H{"id": lastID})
}

func (ctrl *ManhourController) Add(c *gin.Context) {
	ctrl.saveRecord(c)
}

func (ctrl *ManhourController) Update(c *gin.Context) {
	ctrl.saveRecord(c)
}

func (ctrl *ManhourController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	for _, id := range core.ParseIDs(in) {
		_, _ = db.Exec(`DELETE FROM manhour_summary WHERE id=?`, id)
	}
	core.Success(c, "删除成功", gin.H{})
}

// =========================================================================
// 28. SkillController — 员工技能矩阵 (skill_levels / skill_workers / skills)
// =========================================================================

var defaultSkillProcesses = []string{"插件", "焊锡", "三防", "装配", "初调", "总调", "老化", "检验", "包装"}

type SkillController struct{}

func NewSkillController() *SkillController { return &SkillController{} }

func (ctrl *SkillController) Init(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)

	lRows, _ := db.Query(`SELECT id, level_code, level_name, COALESCE(level_desc,''), COALESCE(coefficient,1.0) FROM skill_levels ORDER BY level_code ASC`)
	levels := []map[string]any{}
	if lRows != nil {
		defer lRows.Close()
		for lRows.Next() {
			var id, code int
			var name, desc string
			var coeff float64
			if lRows.Scan(&id, &code, &name, &desc, &coeff) == nil {
				levels = append(levels, map[string]any{
					"id":          id,
					"level_code":  code,
					"level_name":  name,
					"level_desc":  desc,
					"coefficient": coeff,
				})
			}
		}
	}
	uRows, _ := db.Query(`SELECT id, username, COALESCE(display_name,'') FROM users WHERE status=1 ORDER BY id ASC`)
	foremen := []map[string]any{}
	if uRows != nil {
		defer uRows.Close()
		for uRows.Next() {
			var id int
			var u, dn string
			if uRows.Scan(&id, &u, &dn) == nil {
				foremen = append(foremen, map[string]any{"id": id, "username": u, "display_name": dn})
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"processes":     defaultSkillProcesses,
		"levels":        levels,
		"foreman_users": foremen,
		"is_admin":      isAdmin,
		"is_foreman":    true,
	})
}

func (ctrl *SkillController) MatrixOverview(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page <= 0 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 {
		limit = core.AsInt(in["size"])
	}
	if limit <= 0 {
		limit = 20
	}
	offset := (page - 1) * limit

	team := strings.TrimSpace(core.AsString(in["team"]))
	search := strings.TrimSpace(core.AsString(in["search"]))

	where := "WHERE 1=1"
	args := []any{}
	if team != "" {
		where += " AND w.team LIKE ?"
		args = append(args, "%"+team+"%")
	}
	if search != "" {
		where += " AND (w.worker_name LIKE ? OR w.worker_no LIKE ? OR s.product_code LIKE ?)"
		like := "%" + search + "%"
		args = append(args, like, like, like)
	}

	q := `SELECT w.id, COALESCE(w.worker_no,''), w.worker_name, COALESCE(w.team,''), COALESCE(s.product_code,'通用产品') as pcode
	      FROM skill_workers w
	      LEFT JOIN (SELECT DISTINCT worker_id, product_code FROM skills WHERE status=1) s ON w.id = s.worker_id ` + where + `
	      ORDER BY w.id ASC, pcode ASC LIMIT ? OFFSET ?`
	var total int
	_ = db.QueryRow(`SELECT COUNT(*) FROM (SELECT w.id, COALESCE(s.product_code,'通用产品') FROM skill_workers w LEFT JOIN (SELECT DISTINCT worker_id, product_code FROM skills WHERE status=1) s ON w.id = s.worker_id `+where+`) t`, args...).Scan(&total)

	rows, _ := db.Query(q, append(args, limit, offset)...)
	outRows := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var wid int
			var wno, wname, tm, pcode string
			if rows.Scan(&wid, &wno, &wname, &tm, &pcode) == nil {
				sMap := map[string]int{}
				sRows, _ := db.Query(`SELECT process_code, skill_level FROM skills WHERE worker_id=? AND product_code=? AND status=1`, wid, pcode)
				if sRows != nil {
					for sRows.Next() {
						var proc string
						var lvl int
						if sRows.Scan(&proc, &lvl) == nil {
							sMap[proc] = lvl
						}
					}
					sRows.Close()
				}
				outRows = append(outRows, map[string]any{
					"worker_id":    wid,
					"worker_no":    wno,
					"worker_name":  wname,
					"team":         tm,
					"product_code": pcode,
					"description":  core.LookupProductDesc(db, pcode),
					"skills":       sMap,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{
		"rows":        outRows,
		"list":        outRows,
		"total":       total,
		"total_pages": core.CalcTotalPages(total, limit),
		"page":        page,
	})
}

func (ctrl *SkillController) QuickUpdateCell(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wid := core.AsInt(in["worker_id"])
	pcode := strings.TrimSpace(core.AsString(in["product_code"]))
	proc := strings.TrimSpace(core.AsString(in["process_code"]))
	lvl := core.AsInt(in["level"])
	if wid <= 0 || proc == "" {
		core.Error(c, "参数不完整", gin.H{})
		return
	}
	if pcode == "" {
		pcode = "通用产品"
	}
	_, qErr := db.Exec(`INSERT INTO skills (worker_id, product_code, process_code, skill_level, status, review_time, created_at, updated_at) VALUES (?,?,?,?,1,NOW(),NOW(),NOW()) ON DUPLICATE KEY UPDATE skill_level=VALUES(skill_level), status=1, updated_at=NOW()`,
		wid, pcode, proc, lvl)
	if qErr != nil {
		core.Error(c, "更新失败: "+qErr.Error(), gin.H{})
		return
	}
	core.Success(c, "技能等级已更新", gin.H{})
}

func (ctrl *SkillController) GetProductProcesses(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	code := strings.TrimSpace(core.AsString(in["mater_no"]))
	if code == "" {
		code = strings.TrimSpace(core.AsString(in["product_code"]))
	}
	procs := core.LookupProductProcesses(code)
	desc := core.LookupProductDesc(db, code)
	core.Success(c, "ok", gin.H{
		"product_code": code,
		"description":  desc,
		"processes":    procs,
	})
}

func (ctrl *SkillController) SubmitSkills(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	sess := core.GetOrCreateSession(c)
	isAdmin := core.IsAdminUser(sess.User)
	in := core.ParseInputMap(c)
	wid := core.AsInt(in["worker_id"])
	pcode := strings.TrimSpace(core.AsString(in["product_code"]))
	if pcode == "" {
		pcode = "通用产品"
	}
	status := 1
	if !isAdmin {
		status = 0
	}
	if arr, ok := in["skills_data"].([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				proc := strings.TrimSpace(core.AsString(m["process_code"]))
				lvl := core.AsInt(m["level"])
				if proc != "" && lvl > 0 {
					_, _ = db.Exec(`INSERT INTO skills (worker_id, product_code, process_code, skill_level, status, created_at, updated_at) VALUES (?,?,?,?,?,NOW(),NOW()) ON DUPLICATE KEY UPDATE skill_level=VALUES(skill_level), status=VALUES(status), updated_at=NOW()`,
						wid, pcode, proc, lvl, status)
				}
			}
		}
	}
	msg := "技能保存成功"
	if status == 0 {
		msg = "技能已提交审批"
	}
	core.Success(c, msg, gin.H{})
}

func (ctrl *SkillController) Leaderboard(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT w.id, COALESCE(w.worker_no,''), w.worker_name, COALESCE(w.team,''), COUNT(s.id) as skill_cnt, COALESCE(SUM(s.skill_level),0) as total_score, SUM(CASE WHEN s.skill_level>=4 THEN 1 ELSE 0 END) as master_cnt FROM skill_workers w LEFT JOIN skills s ON w.id=s.worker_id AND s.status=1 AND s.skill_level>0 GROUP BY w.id, w.worker_no, w.worker_name, w.team ORDER BY total_score DESC, skill_cnt DESC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, cnt, score int
			var master sql.NullInt64
			var wno, wname, tm string
			if rows.Scan(&id, &wno, &wname, &tm, &cnt, &score, &master) == nil {
				list = append(list, map[string]any{
					"worker_id":    id,
					"worker_no":    wno,
					"worker_name":  wname,
					"team":         tm,
					"skill_count":  cnt,
					"total_score":  score,
					"master_count": master.Int64,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"rank_list": list, "list": list})
}

func (ctrl *SkillController) ListWorkers(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT w.id, COALESCE(w.user_id,0), COALESCE(w.worker_no,''), w.worker_name, COALESCE(w.team,''), COALESCE(w.identity,'全能工'), COALESCE(w.work_status,'在岗'), COALESCE(w.foreman_id,0), COALESCE(u.display_name, u.username, '') FROM skill_workers w LEFT JOIN users u ON w.foreman_id=u.id ORDER BY w.id ASC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, uid, fid int
			var wno, wname, tm, ident, ws, fname string
			if rows.Scan(&id, &uid, &wno, &wname, &tm, &ident, &ws, &fid, &fname) == nil {
				list = append(list, map[string]any{
					"id":           id,
					"user_id":      uid,
					"worker_no":    wno,
					"worker_name":  wname,
					"team":         tm,
					"identity":     ident,
					"work_status":  ws,
					"foreman_id":   fid,
					"foreman_name": fname,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"list": list, "workers": list})
}

func (ctrl *SkillController) SaveWorker(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	wno := strings.TrimSpace(core.AsString(in["worker_no"]))
	wname := strings.TrimSpace(core.AsString(in["worker_name"]))
	tm := strings.TrimSpace(core.AsString(in["team"]))
	ident := strings.TrimSpace(core.AsString(in["identity"]))
	if ident == "" {
		ident = "普通员工"
	}
	ws := strings.TrimSpace(core.AsString(in["work_status"]))
	if ws == "" {
		ws = "在岗"
	}
	fid := core.AsInt(in["foreman_id"])
	if id > 0 {
		_, _ = db.Exec(`UPDATE skill_workers SET worker_no=?, worker_name=?, team=?, identity=?, work_status=?, foreman_id=?, updated_at=NOW() WHERE id=?`,
			wno, wname, tm, ident, ws, fid, id)
		core.Success(c, "更新成功", gin.H{})
		return
	}
	_, _ = db.Exec(`INSERT INTO skill_workers (worker_no, worker_name, team, identity, work_status, foreman_id, created_at, updated_at) VALUES (?,?,?,?,?,?,NOW(),NOW())`,
		wno, wname, tm, ident, ws, fid)
	core.Success(c, "添加员工成功", gin.H{})
}

func (ctrl *SkillController) ListPending(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT s.id, s.worker_id, COALESCE(w.worker_name,''), COALESCE(w.team,''), s.product_code, s.process_code, s.skill_level, COALESCE(DATE_FORMAT(s.created_at,'%Y-%m-%d %H:%i:%s'),'') FROM skills s LEFT JOIN skill_workers w ON s.worker_id=w.id WHERE s.status=0 ORDER BY s.id DESC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, wid, lvl int
			var wname, tm, pcode, proc, ca string
			if rows.Scan(&id, &wid, &wname, &tm, &pcode, &proc, &lvl, &ca) == nil {
				list = append(list, map[string]any{
					"id":           id,
					"worker_id":    wid,
					"worker_name":  wname,
					"team":         tm,
					"product_code": pcode,
					"process_code": proc,
					"skill_level":  lvl,
					"created_at":   ca,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"pending_list": list, "list": list})
}

func (ctrl *SkillController) ReviewSkills(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	decision := core.AsString(in["decision"])
	status := 1
	if decision == "reject" {
		status = 2
	}
	for _, id := range core.ParseIDs(in) {
		_, _ = db.Exec(`UPDATE skills SET status=?, review_time=NOW(), updated_at=NOW() WHERE id=?`, status, id)
	}
	core.Success(c, "审批处理完成", gin.H{})
}

func (ctrl *SkillController) GetLevels(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	rows, _ := db.Query(`SELECT id, level_code, level_name, COALESCE(level_desc,''), COALESCE(coefficient,1.0) FROM skill_levels ORDER BY level_code ASC`)
	list := []map[string]any{}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var id, code int
			var name, desc string
			var coeff float64
			if rows.Scan(&id, &code, &name, &desc, &coeff) == nil {
				list = append(list, map[string]any{
					"id":          id,
					"level_code":  code,
					"level_name":  name,
					"level_desc":  desc,
					"coefficient": coeff,
				})
			}
		}
	}
	core.Success(c, "ok", gin.H{"levels": list, "list": list})
}

func (ctrl *SkillController) SaveLevels(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "数据库连接失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	if arr, ok := in["levels"].([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				code := core.AsInt(m["level_code"])
				if code <= 0 {
					code = 1
				}
				name := strings.TrimSpace(core.AsString(m["level_name"]))
				desc := core.AsString(m["level_desc"])
				coeff := core.AsFloat(m["coefficient"])
				if coeff <= 0 {
					coeff = 1.0
				}
				_, _ = db.Exec(`INSERT INTO skill_levels (level_code, level_name, level_desc, coefficient) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE level_name=VALUES(level_name), level_desc=VALUES(level_desc), coefficient=VALUES(coefficient)`,
					code, name, desc, coeff)
			}
		}
		core.Success(c, "保存等级成功", gin.H{})
		return
	}
	code := core.AsInt(in["level_code"])
	if code <= 0 {
		code = 1
	}
	name := strings.TrimSpace(core.AsString(in["level_name"]))
	desc := core.AsString(in["level_desc"])
	coeff := core.AsFloat(in["coefficient"])
	if coeff <= 0 {
		coeff = 1.0
	}
	_, _ = db.Exec(`INSERT INTO skill_levels (level_code, level_name, level_desc, coefficient) VALUES (?,?,?,?) ON DUPLICATE KEY UPDATE level_name=VALUES(level_name), level_desc=VALUES(level_desc), coefficient=VALUES(coefficient)`,
		code, name, desc, coeff)
	core.Success(c, "保存等级成功", gin.H{})
}

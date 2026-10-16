package router

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
	"vite-system-go/internal/controller"
	"vite-system-go/internal/core"
	"vite-system-go/internal/middleware"
)

// NewEngine 构建并配置 Gin 路由引擎。
// 全站 9 大分类、34 个功能模块的所有接口均在此显式注册并挂载模块权限守卫。
func NewEngine() *gin.Engine {
	if config.GetBool("APP_DEBUG", false) {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(middleware.RecoveryWithSanitizedError())
	r.Use(middleware.CORSAndSecurityHeaders())
	r.Use(middleware.SessionAndPasswordGuard())
	r.Use(middleware.RateLimitGuard())

	// 静态上传目录与共享文件目录直通（兼容 Nginx 反代或直连）
	pubRoot := config.Get("PUBLIC_ROOT", "./public")
	r.Static("/uploads", filepath.Join(pubRoot, "uploads"))
	r.Static("/shared_files", filepath.Join(pubRoot, "shared_files"))

	// 实例化全站控制器
	authCtrl := controller.NewAuthController()
	dashCtrl := controller.NewDashboardController()
	sysCtrl := controller.NewSystemController()
	clientCtrl := controller.NewClientVersionController()

	discussCtrl := controller.NewDiscussController()
	filesCtrl := controller.NewFilesController()
	toolsCtrl := controller.NewToolsController()
	weeklyCtrl := controller.NewWeeklyController()
	oeeCtrl := controller.NewOeeController()

	progressCtrl := controller.NewProgressController()
	kittingCtrl := controller.NewKittingController()
	dispatchCtrl := controller.NewDispatchKittingController()
	labelPrintCtrl := controller.NewLabelPrintController()
	equipCtrl := controller.NewEquipmentController()
	macCtrl := controller.NewMacController()
	macQueryCtrl := controller.NewMacQueryController()

	traceCtrl := controller.NewTraceController()
	qtCtrl := controller.NewQualityTraceController()
	barcodeCtrl := controller.NewBarcodeValidatorController()
	fpCtrl := controller.NewFirstPieceController()
	impCtrl := controller.NewImpedanceController()
	qualCtrl := controller.NewQualityController()
	tsCtrl := controller.NewTroubleshootController()
	beibianCtrl := controller.NewBeibianController()
	docErrCtrl := controller.NewDocErrorController()
	zywjCtrl := controller.NewZywjController()

	osmCtrl := controller.NewOsmController()
	assetsCtrl := controller.NewAssetsController()
	agingCtrl := controller.NewAgingCartController()
	attCtrl := controller.NewAttendanceController()
	diaryCtrl := controller.NewDiaryController()
	manhourCtrl := controller.NewManhourController()
	skillCtrl := controller.NewSkillController()

	// ============================================================
	// 1. 认证与左侧分类导航核心接口 (/api/auth/*)
	// ============================================================
	r.POST("/api/auth/login", authCtrl.Login)
	r.GET("/api/auth/me", authCtrl.Me)
	r.GET("/api/auth/nav_tree", authCtrl.NavTree)
	r.POST("/api/auth/logout", authCtrl.Logout)
	r.POST("/api/auth/change_password", authCtrl.ChangePassword)

	// ============================================================
	// 2. 导航落地首页（Dashboard）最新公告与已读持久化接口（公开登录态）
	// ============================================================
	r.GET("/api/discuss/list_notices", dashCtrl.ListNotices)
	r.GET("/api/discuss/get_notice_detail", dashCtrl.GetNoticeDetail)
	r.GET("/api/discuss/list_notice_reads", dashCtrl.ListNoticeReads)
	r.POST("/api/discuss/mark_notice_read", dashCtrl.MarkNoticeRead)

	// ============================================================
	// 3. 系统管理分类下属 6 个核心管理模块 (/api/system/* & /api/client/*)
	// ============================================================
	sysGroup := r.Group("/api/system", middleware.RequireAdmin())
	{
		sysGroup.GET("/users", sysCtrl.Users)
		sysGroup.POST("/users/save", sysCtrl.SaveUser)
		sysGroup.POST("/users/reset-all-passwords", sysCtrl.ResetAllPasswords)
		sysGroup.POST("/users/delete", sysCtrl.DeleteUser)

		sysGroup.GET("/roles", sysCtrl.Roles)
		sysGroup.GET("/role-modules", sysCtrl.RoleModules)
		sysGroup.POST("/role-modules/save", sysCtrl.SaveRolePermissions)

		sysGroup.GET("/permissions", sysCtrl.UserPermissions)
		sysGroup.POST("/permissions/save", sysCtrl.SaveUserPermissions)

		sysGroup.GET("/modules", sysCtrl.Modules)
		sysGroup.POST("/modules/sync", sysCtrl.SyncModules)
		sysGroup.POST("/modules/save", sysCtrl.SaveModule)
		sysGroup.POST("/modules/toggle-status", sysCtrl.ToggleModuleStatus)
		sysGroup.POST("/modules/delete", sysCtrl.DeleteModule)

		sysGroup.GET("/audit-logs", sysCtrl.AuditLogs)
	}

	r.GET("/api/client/check_version", clientCtrl.CheckVersion)
	r.POST("/api/client/check_version", clientCtrl.CheckVersion)
	r.GET("/api/client/download", clientCtrl.Download)
	r.HEAD("/api/client/download", clientCtrl.Download)

	clientAdmin := r.Group("/api/client", middleware.RequireAdmin())
	{
		clientAdmin.GET("/list_clients", clientCtrl.ListClients)
		clientAdmin.GET("/list_shared", clientCtrl.ListShared)
		clientAdmin.GET("/list_versions", clientCtrl.ListVersions)
		clientAdmin.POST("/save_version", clientCtrl.SaveVersion)
		clientAdmin.POST("/delete_version", clientCtrl.DeleteVersion)
	}

	// ============================================================
	// 4. 协作与工具分类（discuss / files / tools）
	// ============================================================
	discussGroup := r.Group("/api/discuss", middleware.RequireModuleView("discuss"))
	{
		discussGroup.GET("/init", discussCtrl.Init)
		discussGroup.GET("/list_topics", discussCtrl.ListTopics)
		discussGroup.GET("/get_topic_detail", discussCtrl.GetTopicDetail)
		discussGroup.POST("/create_topic", discussCtrl.CreateTopic)
		discussGroup.POST("/edit_topic", discussCtrl.EditTopic)
		discussGroup.POST("/reply_topic", discussCtrl.ReplyTopic)
		discussGroup.POST("/upload", discussCtrl.Upload)
		discussGroup.POST("/manage_action", discussCtrl.ManageAction)
		discussGroup.GET("/list_notifications", discussCtrl.ListNotifications)
		discussGroup.POST("/mark_all_read", discussCtrl.MarkAllRead)
	}

	r.GET("/api/files/ping", filesCtrl.Ping)
	filesGroup := r.Group("/api/files", middleware.RequireModuleView("files"))
	{
		filesGroup.GET("/list", filesCtrl.List)
		filesGroup.GET("/download", filesCtrl.Download)
	}

	toolsGroup := r.Group("/api/tools", middleware.RequireModuleView("tools"))
	{
		toolsGroup.GET("/download_qrcode_tpl", toolsCtrl.DownloadQrcodeTpl)
		toolsGroup.POST("/beibian_pdf2word_convert", toolsCtrl.BeibianPdf2wordConvert)
		toolsGroup.POST("/batch_qrcode_generate", toolsCtrl.BatchQrcodeGenerate)
		toolsGroup.POST("/hebei_packing_convert", toolsCtrl.HebeiPackingConvert)
		toolsGroup.POST("/henan_packing_convert", toolsCtrl.HenanPackingConvert)
	}

	// ============================================================
	// 5. 统计看板分类（weekly / oee）
	// ============================================================
	weeklyGroup := r.Group("/api/weekly", middleware.RequireModuleView("weekly"))
	{
		weeklyGroup.GET("/filter_options", weeklyCtrl.FilterOptions)
		weeklyGroup.GET("/list", weeklyCtrl.List)
		weeklyGroup.GET("/dashboard_data", weeklyCtrl.DashboardData)
		weeklyGroup.POST("/add", weeklyCtrl.Add)
		weeklyGroup.POST("/update", weeklyCtrl.Update)
		weeklyGroup.POST("/delete", weeklyCtrl.Delete)
		weeklyGroup.GET("/export", weeklyCtrl.Export)
	}

	oeeGroup := r.Group("/api/oee", middleware.RequireModuleView("oee"))
	{
		oeeGroup.GET("/init", oeeCtrl.Init)
		oeeGroup.GET("/list", oeeCtrl.List)
		oeeGroup.POST("/add", oeeCtrl.Add)
		oeeGroup.POST("/update", oeeCtrl.Update)
		oeeGroup.GET("/dashboard_data", oeeCtrl.DashboardData)
		oeeGroup.POST("/delete", oeeCtrl.Delete)
		oeeGroup.GET("/export", oeeCtrl.Export)
	}

	// ============================================================
	// 6. 生产执行分类（progress / kitting / dispatch_kitting / label_print / equipment）
	// ============================================================
	progressGroup := r.Group("/api/progress", middleware.RequireModuleView("progress"))
	{
		progressGroup.GET("/query", progressCtrl.Query)
		progressGroup.POST("/query", progressCtrl.Query)
	}

	kittingGroup := r.Group("/api/kitting", middleware.RequireModuleView("kitting"))
	{
		kittingGroup.GET("/query", kittingCtrl.Query)
		kittingGroup.POST("/query", kittingCtrl.Query)
	}

	dispatchGroup := r.Group("/api/dispatch_kitting", middleware.RequireModuleView("dispatch_kitting"))
	{
		dispatchGroup.GET("/query", dispatchCtrl.Query)
		dispatchGroup.POST("/query", dispatchCtrl.Query)
		dispatchGroup.POST("/confirm_receipt", dispatchCtrl.ConfirmReceipt)
		dispatchGroup.GET("/query_wo_kitting", dispatchCtrl.QueryWoKitting)
		dispatchGroup.POST("/query_wo_kitting", dispatchCtrl.QueryWoKitting)
		dispatchGroup.GET("/query_wo_materials", dispatchCtrl.QueryWoMaterials)
		dispatchGroup.POST("/query_wo_materials", dispatchCtrl.QueryWoMaterials)
	}

	labelGroup := r.Group("/api/label_print", middleware.RequireModuleView("label_print"))
	{
		labelGroup.GET("/query_order", labelPrintCtrl.QueryOrder)
		labelGroup.POST("/query_order", labelPrintCtrl.QueryOrder)
		labelGroup.POST("/execute_print", labelPrintCtrl.ExecutePrint)
		labelGroup.GET("/list_history", labelPrintCtrl.ListHistory)
	}

	equipGroup := r.Group("/api/equipment", middleware.RequireModuleView("equipment"))
	{
		equipGroup.GET("/init", equipCtrl.Init)
		equipGroup.GET("/list_matrix", equipCtrl.ListMatrix)
		equipGroup.GET("/get_product_info", equipCtrl.GetProductInfo)
		equipGroup.POST("/get_product_info", equipCtrl.GetProductInfo)
		equipGroup.POST("/save_equipment", equipCtrl.SaveEquipment)
		equipGroup.POST("/delete_equipment", equipCtrl.DeleteEquipment)
	}

	// ============================================================
	// 7. 物料与标识分类（mac / mac_query）
	// ============================================================
	macGroup := r.Group("/api/mac", middleware.RequireModuleView("mac"))
	{
		macGroup.GET("/init", macCtrl.Init)
		macGroup.POST("/get_work_order_info", macCtrl.GetWorkOrderInfo)
		macGroup.POST("/get_product_desc", macCtrl.GetProductDesc)
		macGroup.GET("/list_product_ports", macCtrl.ListProductPorts)
		macGroup.POST("/save_product_port", macCtrl.SaveProductPort)
		macGroup.POST("/delete_product_port", macCtrl.DeleteProductPort)
		macGroup.POST("/import_mac", macCtrl.ImportMac)
		macGroup.POST("/apply_mac", macCtrl.ApplyMac)
		macGroup.GET("/get_records", macCtrl.GetRecords)
		macGroup.GET("/get_record_details", macCtrl.GetRecordDetails)
		macGroup.GET("/get_stats", macCtrl.GetStats)
		macGroup.GET("/get_pool", macCtrl.GetPool)
		macGroup.POST("/delete_records", macCtrl.DeleteRecords)
		macGroup.GET("/get_import_logs", macCtrl.GetImportLogs)
		macGroup.GET("/export_details", macCtrl.ExportDetails)
	}

	macQueryGroup := r.Group("/api/mac_query", middleware.RequireModuleView("mac_query"))
	{
		macQueryGroup.GET("/query", macQueryCtrl.Query)
		macQueryGroup.POST("/query", macQueryCtrl.Query)
		macQueryGroup.GET("/export", macQueryCtrl.Export)
	}

	// ============================================================
	// 8. 质量与检验分类（trace / quality_trace / barcode_validator / first_piece / impedance）
	// ============================================================
	traceGroup := r.Group("/api/trace", middleware.RequireTraceView())
	{
		traceGroup.GET("/init", traceCtrl.Init)
		traceGroup.GET("/checkOrFetchErp", traceCtrl.CheckOrFetchErp)
		traceGroup.POST("/checkOrFetchErp", traceCtrl.CheckOrFetchErp)
		traceGroup.POST("/saveConfig", traceCtrl.SaveConfig)
		traceGroup.GET("/startTrace", traceCtrl.StartTrace)
		traceGroup.POST("/scanBarcode", traceCtrl.ScanBarcode)
		traceGroup.GET("/queryRecords", traceCtrl.QueryRecords)
		traceGroup.POST("/updateScanRecord", traceCtrl.UpdateScanRecord)
		traceGroup.POST("/deleteScanRecord", traceCtrl.DeleteScanRecord)
		traceGroup.GET("/listConfigs", traceCtrl.ListConfigs)
		traceGroup.GET("/listPermissions", traceCtrl.ListPermissions)
		traceGroup.POST("/savePermissions", traceCtrl.SavePermissions)
		traceGroup.GET("/exportExcel", traceCtrl.ExportExcel)
		traceGroup.POST("/exportExcel", traceCtrl.ExportExcel)
		traceGroup.POST("/manualPushCem", traceCtrl.ManualPushCem)
	}

	qtGroup := r.Group("/api/quality_trace", middleware.RequireModuleView("quality_trace"))
	{
		qtGroup.GET("/query", qtCtrl.Query)
		qtGroup.POST("/query", qtCtrl.Query)
		qtGroup.POST("/detail", qtCtrl.Detail)
		qtGroup.POST("/ccjl", qtCtrl.Ccjl)
		qtGroup.GET("/export", qtCtrl.Export)
	}

	bvGroup := r.Group("/api/barcode_validator", middleware.RequireModuleView("barcode_validator"))
	{
		bvGroup.GET("/fetch_workorder", barcodeCtrl.FetchWorkorder)
		bvGroup.POST("/fetch_workorder", barcodeCtrl.FetchWorkorder)
		bvGroup.POST("/upload_reference", barcodeCtrl.UploadReference)
		bvGroup.GET("/progress", barcodeCtrl.Progress)
		bvGroup.POST("/scan_validate", barcodeCtrl.ScanValidate)
		bvGroup.POST("/submit_row", barcodeCtrl.SubmitRow)
		bvGroup.GET("/get_completed_rows", barcodeCtrl.GetCompletedRows)
		bvGroup.GET("/get_settings", barcodeCtrl.GetSettings)
		bvGroup.POST("/save_settings", barcodeCtrl.SaveSettings)
		bvGroup.GET("/get_reference_rows", barcodeCtrl.GetReferenceRows)
		bvGroup.POST("/delete_completed", barcodeCtrl.DeleteCompleted)
		bvGroup.GET("/export_completed", barcodeCtrl.ExportCompleted)
		bvGroup.GET("/get_my_perm", barcodeCtrl.GetMyPerm)
		bvGroup.POST("/update_completed", barcodeCtrl.UpdateCompleted)
	}

	fpGroup := r.Group("/api/first_piece", middleware.RequireModuleView("first_piece"))
	{
		fpGroup.GET("/init", fpCtrl.Init)
		fpGroup.GET("/get_work_order", fpCtrl.GetWorkOrder)
		fpGroup.POST("/get_work_order", fpCtrl.GetWorkOrder)
		fpGroup.POST("/save_record", fpCtrl.SaveRecord)
		fpGroup.GET("/list", fpCtrl.List)
		fpGroup.GET("/get_criteria", fpCtrl.GetCriteria)
		fpGroup.POST("/save_criteria", fpCtrl.SaveCriteria)
		fpGroup.GET("/list_criteria", fpCtrl.ListCriteria)
		fpGroup.POST("/delete_criteria", fpCtrl.DeleteCriteria)
		fpGroup.POST("/delete", fpCtrl.Delete)
		fpGroup.GET("/export", fpCtrl.Export)
	}

	impGroup := r.Group("/api/impedance", middleware.RequireModuleView("impedance"))
	{
		impGroup.GET("/get_batch_info", impCtrl.GetBatchInfo)
		impGroup.POST("/get_batch_info", impCtrl.GetBatchInfo)
		impGroup.POST("/calculate_errors", impCtrl.CalculateErrors)
		impGroup.GET("/list", impCtrl.List)
		impGroup.POST("/add", impCtrl.Add)
		impGroup.POST("/update", impCtrl.Update)
		impGroup.POST("/delete", impCtrl.Delete)
		impGroup.GET("/export", impCtrl.Export)
		impGroup.GET("/get_permissions", impCtrl.GetPermissions)
		impGroup.POST("/update_permission", impCtrl.UpdatePermission)
	}

	// ============================================================
	// 9. 质量异常处理分类（quality / troubleshoot / beibian / doc_error / zywj）
	// ============================================================
	qualGroup := r.Group("/api/quality", middleware.RequireModuleView("quality"))
	{
		qualGroup.GET("/get_batch_info", qualCtrl.GetBatchInfo)
		qualGroup.POST("/get_batch_info", qualCtrl.GetBatchInfo)
		qualGroup.GET("/list", qualCtrl.List)
		qualGroup.POST("/add", qualCtrl.Add)
		qualGroup.POST("/update", qualCtrl.Update)
		qualGroup.POST("/delete", qualCtrl.Delete)
		qualGroup.GET("/export", qualCtrl.Export)
		qualGroup.GET("/get_permissions", qualCtrl.GetPermissions)
		qualGroup.POST("/update_permission", qualCtrl.UpdatePermission)
	}

	tsGroup := r.Group("/api/troubleshoot", middleware.RequireModuleView("troubleshoot"))
	{
		tsGroup.GET("/get_batch_info", tsCtrl.GetBatchInfo)
		tsGroup.POST("/get_batch_info", tsCtrl.GetBatchInfo)
		tsGroup.GET("/list", tsCtrl.List)
		tsGroup.POST("/add", tsCtrl.Add)
		tsGroup.POST("/delete", tsCtrl.Delete)
		tsGroup.GET("/get_permissions", tsCtrl.GetPermissions)
		tsGroup.POST("/update_permission", tsCtrl.UpdatePermission)
		tsGroup.GET("/export", tsCtrl.Export)
	}

	bbGroup := r.Group("/api/beibian", middleware.RequireModuleView("beibian"))
	{
		bbGroup.GET("/list", beibianCtrl.List)
		bbGroup.POST("/add", beibianCtrl.Add)
		bbGroup.POST("/update", beibianCtrl.Update)
		bbGroup.GET("/detail", beibianCtrl.Detail)
		bbGroup.POST("/delete", beibianCtrl.Delete)
		bbGroup.GET("/search_sn", beibianCtrl.SearchSn)
		bbGroup.POST("/search_sn", beibianCtrl.SearchSn)
		bbGroup.GET("/search_order", beibianCtrl.SearchOrder)
		bbGroup.POST("/search_order", beibianCtrl.SearchOrder)
		bbGroup.GET("/export", beibianCtrl.Export)
	}

	docErrGroup := r.Group("/api/doc_error", middleware.RequireModuleView("doc_error"))
	{
		docErrGroup.GET("/init", docErrCtrl.Init)
		docErrGroup.GET("/get_my_perm", docErrCtrl.GetMyPerm)
		docErrGroup.GET("/get_work_order", docErrCtrl.GetWorkOrder)
		docErrGroup.POST("/get_work_order", docErrCtrl.GetWorkOrder)
		docErrGroup.GET("/list", docErrCtrl.List)
		docErrGroup.POST("/list", docErrCtrl.List)
		docErrGroup.GET("/get", docErrCtrl.Get)
		docErrGroup.POST("/get", docErrCtrl.Get)
		docErrGroup.POST("/add", docErrCtrl.Add)
		docErrGroup.POST("/update", docErrCtrl.Update)
		docErrGroup.POST("/delete", docErrCtrl.Delete)
		docErrGroup.GET("/export", docErrCtrl.Export)
		docErrGroup.GET("/get_permissions", docErrCtrl.GetPermissions)
		docErrGroup.POST("/update_permission", docErrCtrl.UpdatePermission)
	}

	zywjGroup := r.Group("/api/zywj", middleware.RequireModuleView("zywj"))
	{
		zywjGroup.GET("/init", zywjCtrl.Init)
		zywjGroup.GET("/list", zywjCtrl.List)
		zywjGroup.POST("/upload", zywjCtrl.Upload)
		zywjGroup.POST("/delete", zywjCtrl.Delete)
	}

	// ============================================================
	// 10. 外协与资产分类（osm / assets / aging_cart）
	// ============================================================
	osmGroup := r.Group("/api/osm", middleware.RequireModuleView("osm"))
	{
		osmGroup.GET("/init", osmCtrl.Init)
		osmGroup.GET("/list", osmCtrl.List)
		osmGroup.POST("/save", osmCtrl.Save)
		osmGroup.POST("/delete", osmCtrl.Delete)
		osmGroup.GET("/get_pending_returns", osmCtrl.GetPendingReturns)
		osmGroup.POST("/add_return", osmCtrl.AddReturn)
		osmGroup.POST("/upload_osm", osmCtrl.UploadOsm)
		osmGroup.GET("/export", osmCtrl.Export)
		osmGroup.GET("/list_permissions", osmCtrl.ListPermissions)
		osmGroup.POST("/save_permissions", osmCtrl.SavePermissions)
	}

	assetsGroup := r.Group("/api/assets", middleware.RequireModuleView("assets"))
	{
		assetsGroup.GET("/dashboard_stats", assetsCtrl.DashboardStats)
		assetsGroup.GET("/get_categories", assetsCtrl.GetCategories)
		assetsGroup.GET("/list", assetsCtrl.List)
		assetsGroup.POST("/save", assetsCtrl.Save)
		assetsGroup.GET("/list_user_permissions", assetsCtrl.ListUserPermissions)
		assetsGroup.POST("/save_user_permissions", assetsCtrl.SaveUserPermissions)
		assetsGroup.POST("/delete", assetsCtrl.Delete)
		assetsGroup.POST("/save_category", assetsCtrl.SaveCategory)
		assetsGroup.POST("/delete_category", assetsCtrl.DeleteCategory)
		assetsGroup.POST("/import_csv", assetsCtrl.ImportCsv)
		assetsGroup.GET("/export_csv", assetsCtrl.ExportCsv)
	}

	agingGroup := r.Group("/api/aging_cart", middleware.RequireModuleView("aging_cart"))
	{
		agingGroup.GET("/init", agingCtrl.Init)
		agingGroup.GET("/dashboard", agingCtrl.Dashboard)
		agingGroup.GET("/carts", agingCtrl.Carts)
		agingGroup.GET("/list", agingCtrl.List)
		agingGroup.POST("/report", agingCtrl.Report)
		agingGroup.POST("/start", agingCtrl.Start)
		agingGroup.POST("/join", agingCtrl.Join)
		agingGroup.POST("/leave", agingCtrl.Leave)
		agingGroup.POST("/finish", agingCtrl.Finish)
		agingGroup.POST("/save", agingCtrl.Save)
		agingGroup.POST("/save_cart", agingCtrl.SaveCart)
		agingGroup.POST("/set_cart_total", agingCtrl.SetCartTotal)
		agingGroup.POST("/delete", agingCtrl.Delete)
		agingGroup.GET("/export", agingCtrl.Export)
	}

	// ============================================================
	// 11. 人员与工时分类（attendance / diary / manhour / skill）
	// ============================================================
	attGroup := r.Group("/api/attendance", middleware.RequireModuleView("attendance"))
	{
		attGroup.GET("/dashboard", attCtrl.Dashboard)
		attGroup.POST("/bind_card", attCtrl.BindCard)
		attGroup.POST("/unbind_card", attCtrl.UnbindCard)
		attGroup.GET("/list_employees", attCtrl.ListEmployees)
		attGroup.POST("/save_employee", attCtrl.SaveEmployee)
		attGroup.POST("/delete_employee", attCtrl.DeleteEmployee)
		attGroup.GET("/list_cards", attCtrl.ListCards)
		attGroup.POST("/save_card", attCtrl.SaveCard)
		attGroup.POST("/delete_card", attCtrl.DeleteCard)
		attGroup.GET("/list_records", attCtrl.ListRecords)
		attGroup.GET("/statistics", attCtrl.Statistics)
		attGroup.GET("/export", attCtrl.Export)
	}

	diaryGroup := r.Group("/api/diary", middleware.RequireModuleView("diary"))
	{
		diaryGroup.GET("/list", diaryCtrl.List)
		diaryGroup.POST("/add", diaryCtrl.Add)
		diaryGroup.POST("/update", diaryCtrl.Update)
		diaryGroup.POST("/delete", diaryCtrl.Delete)
		diaryGroup.POST("/search_task", diaryCtrl.SearchTask)
		diaryGroup.POST("/check_quantity", diaryCtrl.CheckQuantity)
		diaryGroup.GET("/get_users", diaryCtrl.GetUsers)
		diaryGroup.POST("/update_permission", diaryCtrl.UpdatePermission)
		diaryGroup.GET("/export", diaryCtrl.Export)
	}

	manhourGroup := r.Group("/api/manhour", middleware.RequireModuleView("manhour"))
	{
		manhourGroup.GET("/categories", manhourCtrl.Categories)
		manhourGroup.GET("/get_code_meta", manhourCtrl.GetCodeMeta)
		manhourGroup.GET("/list", manhourCtrl.List)
		manhourGroup.POST("/add", manhourCtrl.Add)
		manhourGroup.POST("/update", manhourCtrl.Update)
		manhourGroup.POST("/delete", manhourCtrl.Delete)
	}

	skillGroup := r.Group("/api/skill", middleware.RequireModuleView("skill"))
	{
		skillGroup.GET("/init", skillCtrl.Init)
		skillGroup.GET("/matrix_overview", skillCtrl.MatrixOverview)
		skillGroup.POST("/quick_update_cell", skillCtrl.QuickUpdateCell)
		skillGroup.GET("/get_product_processes", skillCtrl.GetProductProcesses)
		skillGroup.POST("/get_product_processes", skillCtrl.GetProductProcesses)
		skillGroup.GET("/get_levels", skillCtrl.GetLevels)
		skillGroup.POST("/save_levels", skillCtrl.SaveLevels)
		skillGroup.GET("/list_workers", skillCtrl.ListWorkers)
		skillGroup.POST("/save_worker", skillCtrl.SaveWorker)
		skillGroup.POST("/submit_skills", skillCtrl.SubmitSkills)
		skillGroup.GET("/leaderboard", skillCtrl.Leaderboard)
		skillGroup.GET("/list_pending", skillCtrl.ListPending)
		skillGroup.POST("/review_skills", skillCtrl.ReviewSkills)
	}

	// ============================================================
	// 12. 未知路由兜底
	// ============================================================
	fallbackURL := strings.TrimSpace(config.Get("PHP_FALLBACK_URL", ""))
	var proxy *httputil.ReverseProxy
	if fallbackURL != "" {
		if target, err := url.Parse(fallbackURL); err == nil {
			proxy = httputil.NewSingleHostReverseProxy(target)
		}
	}

	r.NoRoute(func(c *gin.Context) {
		reqPath := c.Request.URL.Path
		if proxy != nil && strings.HasPrefix(reqPath, "/api/") {
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		core.Error(c, "接口不存在: "+reqPath, gin.H{
			"code": "ROUTE_NOT_FOUND",
			"path": reqPath,
		}, http.StatusNotFound)
	})

	return r
}

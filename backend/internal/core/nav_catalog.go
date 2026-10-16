package core

// NavGroupDef 定义左侧手风琴导航栏的一级分类（完全对齐 frontend/src/layout/Layout.vue 的 menuGroups）。
type NavGroupDef struct {
	Key   string          `json:"key"`
	Label string          `json:"label"`
	Icon  string          `json:"icon"`
	Items []BuiltinModule `json:"items"`
}

// BuiltinModule 定义系统内置的标准功能模块（用于初始化/一键同步 modules 表及构建分类导航树）。
type BuiltinModule struct {
	Name        string `json:"name"`
	ModuleCode  string `json:"module_code"`
	Category    string `json:"category"`
	CategoryKey string `json:"category_key"`
	Route       string `json:"route"`
	Path        string `json:"path"`
	Icon        string `json:"icon"`
	SortOrder   int    `json:"sort_order"`
	Description string `json:"description"`
	AdminOnly   bool   `json:"admin_only"`
}

// BuiltinNavGroups 返回全站 9 大导航分类及下属 34 个内置子模块的完整拓扑定义。
// 该定义与前端 Layout.vue 中的 menuGroups 及 tools/菜单模块对账.sql 严格保持 1:1 对齐。
func BuiltinNavGroups() []NavGroupDef {
	return []NavGroupDef{
		{
			Key:   "production",
			Label: "生产执行",
			Icon:  "Operation",
			Items: []BuiltinModule{
				{Name: "工单进度查询", ModuleCode: "progress", Category: "生产执行", CategoryKey: "production", Route: "progress", Path: "/progress", Icon: "Tickets", SortOrder: 101, Description: "工单生产进度与 SN 实时状态查询"},
				{Name: "工单齐套查询", ModuleCode: "kitting", Category: "生产执行", CategoryKey: "production", Route: "kitting", Path: "/kitting", Icon: "Files", SortOrder: 102, Description: "生产工单物料齐套率与缺料明细查询"},
				{Name: "包装配送齐套", ModuleCode: "dispatch_kitting", Category: "生产执行", CategoryKey: "production", Route: "dispatch-kitting", Path: "/dispatch-kitting", Icon: "Box", SortOrder: 103, Description: "包装配送齐套看板与收货确认管理"},
				{Name: "标签打印管理", ModuleCode: "label_print", Category: "生产执行", CategoryKey: "production", Route: "label-print", Path: "/label-print", Icon: "Printer", SortOrder: 104, Description: "工单条码标签查询、打印执行与历史记录"},
				{Name: "设备矩阵总览", ModuleCode: "equipment", Category: "生产执行", CategoryKey: "production", Route: "equipment", Path: "/equipment", Icon: "Operation", SortOrder: 105, Description: "车间生产与测试设备矩阵台账总览"},
			},
		},
		{
			Key:   "quality",
			Label: "质量与检验",
			Icon:  "Checked",
			Items: []BuiltinModule{
				{Name: "质量追溯系统", ModuleCode: "trace", Category: "质量与检验", CategoryKey: "quality", Route: "trace", Path: "/trace", Icon: "Checked", SortOrder: 201, Description: "关键物料扫码绑定、质量追溯与 CEM 推送"},
				{Name: "CEM质量追溯", ModuleCode: "quality_trace", Category: "质量与检验", CategoryKey: "quality", Route: "quality-trace", Path: "/quality-trace", Icon: "Search", SortOrder: 202, Description: "CEM 质量追溯数据同步与综合查询"},
				{Name: "条码验证比对", ModuleCode: "barcode_validator", Category: "质量与检验", CategoryKey: "quality", Route: "barcode-validator", Path: "/barcode-validator", Icon: "Select", SortOrder: 203, Description: "工单条码规则防错验证与参考表比对"},
				{Name: "首件检管理", ModuleCode: "first_piece", Category: "质量与检验", CategoryKey: "quality", Route: "first-piece", Path: "/first-piece", Icon: "Crop", SortOrder: 204, Description: "首件检验记录录入与判据标准维护"},
				{Name: "阻抗测试记录", ModuleCode: "impedance", Category: "质量与检验", CategoryKey: "quality", Route: "impedance", Path: "/impedance", Icon: "Odometer", SortOrder: 205, Description: "阻抗测试数据采集、误差计算与导出"},
			},
		},
		{
			Key:   "issue",
			Label: "质量异常处理",
			Icon:  "WarningFilled",
			Items: []BuiltinModule{
				{Name: "质量问题记录", ModuleCode: "quality", Category: "质量异常处理", CategoryKey: "issue", Route: "quality", Path: "/quality", Icon: "WarningFilled", SortOrder: 301, Description: "生产过程质量异常问题登记与闭环跟踪"},
				{Name: "问题排查记录", ModuleCode: "troubleshoot", Category: "质量异常处理", CategoryKey: "issue", Route: "troubleshoot", Path: "/troubleshoot", Icon: "HelpFilled", SortOrder: 302, Description: "装置故障与疑难问题排查经验库"},
				{Name: "北边返厂统计", ModuleCode: "beibian", Category: "质量异常处理", CategoryKey: "issue", Route: "beibian", Path: "/beibian", Icon: "Van", SortOrder: 303, Description: "返厂维修装置统计、SN 追踪与分析"},
				{Name: "文件错误记录", ModuleCode: "doc_error", Category: "质量异常处理", CategoryKey: "issue", Route: "doc-error", Path: "/doc-error", Icon: "DocumentChecked", SortOrder: 304, Description: "图纸、工艺及设计文件错误反馈记录"},
				{Name: "作业指导文件", ModuleCode: "zywj", Category: "质量异常处理", CategoryKey: "issue", Route: "zywj", Path: "/zywj", Icon: "Document", SortOrder: 305, Description: "标准作业指导书（SOP）在线查阅与管理"},
			},
		},
		{
			Key:   "material",
			Label: "物料与标识",
			Icon:  "Connection",
			Items: []BuiltinModule{
				{Name: "MAC 地址管理", ModuleCode: "mac", Category: "物料与标识", CategoryKey: "material", Route: "mac", Path: "/mac", Icon: "Connection", SortOrder: 401, Description: "MAC 地址池导入、按工单分配与网口配置"},
				{Name: "MAC/ID 信息查询", ModuleCode: "mac_query", Category: "物料与标识", CategoryKey: "material", Route: "mac-query", Path: "/mac-query", Icon: "Search", SortOrder: 402, Description: "历史分配 MAC 地址与设备唯一标识反查"},
			},
		},
		{
			Key:   "asset",
			Label: "外协与资产",
			Icon:  "Box",
			Items: []BuiltinModule{
				{Name: "外协管理系统", ModuleCode: "osm", Category: "外协与资产", CategoryKey: "asset", Route: "osm", Path: "/osm", Icon: "Van", SortOrder: 501, Description: "外协加工发出、待返跟踪与回厂核销"},
				{Name: "资产台账管理", ModuleCode: "assets", Category: "外协与资产", CategoryKey: "asset", Route: "assets", Path: "/assets", Icon: "Box", SortOrder: 502, Description: "车间固定资产、仪器仪表分类台账与盘点"},
				{Name: "老化车维修管理", ModuleCode: "aging_cart", Category: "外协与资产", CategoryKey: "asset", Route: "aging-cart", Path: "/aging-cart", Icon: "Bicycle", SortOrder: 503, Description: "老化车故障报修、多人协同维修计时与看板"},
			},
		},
		{
			Key:   "hr",
			Label: "人员与工时",
			Icon:  "User",
			Items: []BuiltinModule{
				{Name: "人员离岗监控", ModuleCode: "attendance", Category: "人员与工时", CategoryKey: "hr", Route: "attendance", Path: "/attendance", Icon: "Timer", SortOrder: 601, Description: "车间人员离岗刷卡登记、时长统计与看板"},
				{Name: "工作日记管理", ModuleCode: "diary", Category: "人员与工时", CategoryKey: "hr", Route: "diary", Path: "/diary", Icon: "Notebook", SortOrder: 602, Description: "日常生产任务报工、数量核对与工作日记"},
				{Name: "工时汇总系统", ModuleCode: "manhour", Category: "人员与工时", CategoryKey: "hr", Route: "manhour", Path: "/manhour", Icon: "Calendar", SortOrder: 603, Description: "定额工时维护与班组人员工时核算汇总"},
				{Name: "技能矩阵方案", ModuleCode: "skill", Category: "人员与工时", CategoryKey: "hr", Route: "skill", Path: "/skill", Icon: "Grid", SortOrder: 604, Description: "员工多工序技能等级评定、审批与排行榜"},
			},
		},
		{
			Key:   "report",
			Label: "统计看板",
			Icon:  "DataAnalysis",
			Items: []BuiltinModule{
				{Name: "周完成统计", ModuleCode: "weekly", Category: "统计看板", CategoryKey: "report", Route: "weekly", Path: "/weekly", Icon: "DataAnalysis", SortOrder: 701, Description: "周生产计划完成率统计与多维趋势分析看板"},
				{Name: "班组效率", ModuleCode: "oee", Category: "统计看板", CategoryKey: "report", Route: "oee", Path: "/oee", Icon: "TrendCharts", SortOrder: 702, Description: "OEE 班组综合效率分析与历史趋势图表"},
			},
		},
		{
			Key:   "tools",
			Label: "协作与工具",
			Icon:  "Tools",
			Items: []BuiltinModule{
				{Name: "实用工具箱", ModuleCode: "tools", Category: "协作与工具", CategoryKey: "tools", Route: "tools", Path: "/tools", Icon: "Tools", SortOrder: 801, Description: "批量二维码生成、装箱单转换与文档处理工具"},
				{Name: "讨论交流区", ModuleCode: "discuss", Category: "协作与工具", CategoryKey: "tools", Route: "discuz", Path: "/discuz", Icon: "ChatLineRound", SortOrder: 802, Description: "车间公告发布、技术经验交流与互动通知"},
				{Name: "文件共享", ModuleCode: "files", Category: "协作与工具", CategoryKey: "tools", Route: "files", Path: "/files", Icon: "FolderOpened", SortOrder: 803, Description: "车间公共软件包、模板及常用资料在线共享下载"},
			},
		},
		{
			Key:   "system",
			Label: "系统管理",
			Icon:  "Setting",
			Items: []BuiltinModule{
				{Name: "用户管理", ModuleCode: "system_users", Category: "系统管理", CategoryKey: "system", Route: "system/users", Path: "/system/users", Icon: "User", SortOrder: 901, Description: "系统账号、工号、所属班组与岗位角色管理", AdminOnly: true},
				{Name: "权限分配", ModuleCode: "system_permissions", Category: "系统管理", CategoryKey: "system", Route: "system/permissions", Path: "/system/permissions", Icon: "Key", SortOrder: 902, Description: "按用户分配模块查看权、维护权及各模块细分权限", AdminOnly: true},
				{Name: "角色权限", ModuleCode: "system_roles", Category: "系统管理", CategoryKey: "system", Route: "system/roles", Path: "/system/roles", Icon: "Lock", SortOrder: 903, Description: "岗位角色定义及角色级默认可访问模块树配置", AdminOnly: true},
				{Name: "模块管理", ModuleCode: "system_modules", Category: "系统管理", CategoryKey: "system", Route: "system/modules", Path: "/system/modules", Icon: "Menu", SortOrder: 904, Description: "全站导航分类、模块路由、图标排序与启停控制", AdminOnly: true},
				{Name: "审计日志", ModuleCode: "system_audit_logs", Category: "系统管理", CategoryKey: "system", Route: "system/audit-logs", Path: "/system/audit-logs", Icon: "Document", SortOrder: 905, Description: "全站关键业务操作、登录认证与权限变更安全审计", AdminOnly: true},
				{Name: "客户端版本", ModuleCode: "system_client_version", Category: "系统管理", CategoryKey: "system", Route: "system/client-version", Path: "/system/client-version", Icon: "Upload", SortOrder: 906, Description: "桌面与移动端客户端版本发布、强制升级与安装包分发", AdminOnly: true},
			},
		},
	}
}

// BuiltinModules 将 9 大分类下的全部内置模块展平为切片，供数据库同步与权限对账使用。
func BuiltinModules() []BuiltinModule {
	groups := BuiltinNavGroups()
	out := make([]BuiltinModule, 0, 36)
	for _, g := range groups {
		out = append(out, g.Items...)
	}
	return out
}

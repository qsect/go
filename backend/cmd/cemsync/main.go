// Command cemsync 把外部 CEM 质量追溯系统的「流程卡号 ↔ 需求计划号」关联关系
// 增量同步到本地映射库，供追溯模块按 XQ 需求计划号反查流程卡号。
//
// 这是原 PHP 脚本 backend/app/cem/index.php 的 Go 等价实现（含翻页逻辑），
// 与旧系统行为保持一致：
//   - 只处理创建人为「楚永杰」的流程卡
//   - 只扫描近 N 天（默认 30 天）的单据
//   - 连续 SKIP_THRESHOLD 条已存在则停止翻页（增量策略，页末判断）
//   - 本地库未收录的流程卡才抓详情页提取计划号
//
// 用法（由 cron 定时调用）：
//   cd /www/wwwroot/go/backend && ./bin/cemsync -days 30 >> /www/wwwlogs/cemsync.log 2>&1
//
// 常用参数：
//   -days     扫描最近 N 天（默认 30）
//   -skip     连续 N 条已存在则停止翻页（默认 4，调大更不易漏同步但耗时更长）
//   -map      本地映射库路径（默认 storage/cem_plan_map.json，与 Go 后端共用）
//   -dry-run  只扫描不写盘，用于验证
//
// 停止条件（满足任一即停）：
//   1. 连续 -skip 条命中记录在本地库已存在（页末判断，遇到新卡则计数清零）
//   2. 本页所有单据日期均早于 -days 天前的截止日
//   3. 页面已无下一页链接
//   4. 翻页达到 300 页硬上限（防死循环）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"

	"vite-system-go/internal/config"
)

// ============================================================================
//  常量（严格对齐原 PHP backend/app/cem/index.php 顶部的 define）
// ============================================================================

const (
	cemDefaultUA         = "Mozilla/5.0 (Windows NT 10.0.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	cemLoginURL          = "/login.aspx"
	cemMainURL           = "/main.aspx"
	cemUvQueryURL        = "/Quality_Track/UV_Query.aspx"
	cemDetailURLFormat   = "/Quality_Track/Quality_Tasks_Operation.aspx?Tasksid=%s"
	cemCreatorFilter     = "楚永杰"
	cemDefaultDays       = 30
	// 连续多少条已存在后，在页末停止翻页。
	// 放宽（如 4~6）：翻页更多、耗时更长，但更不容易漏掉夹杂在旧记录中间的新卡。
	// 收紧（如 2）：更快收敛，但若新卡后面紧跟旧卡，可能提前停而漏同步。
	// 可用 -skip 参数覆盖，无需重新编译。
	cemDefaultSkipThreshold = 4
	cemMaxPages          = 300 // 翻页硬上限，防止死循环
	cemDetailInterval    = 60 * time.Millisecond
	cemDefaultMapRelPath = "storage/cem_plan_map.json"
)

// ============================================================================
//  正则
// ============================================================================

var (
	reTrBlock    = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	reTdBlock    = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	reHtmlTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reCharset    = regexp.MustCompile(`(?i)charset\s*=\s*["']?([\w-]+)`)
	reHiddenTag  = regexp.MustCompile(`(?i)<input[^>]*type=["']hidden["'][^>]*>`)
	reInputName  = regexp.MustCompile(`(?i)name=["']([^"']*)["']`)
	reInputValue = regexp.MustCompile(`(?i)value=["']([^"']*)["']`)

	// 流程卡号提取（4 级兜底，按原 PHP 顺序）
	reCardLink1 = regexp.MustCompile(`(?i)GridView1_lktransid_\d+["'][^>]*>([^<]+)</a>`)
	reCardLink2 = regexp.MustCompile(`(?i)[a-zA-Z0-9_]+_lktransid_\d+["'][^>]*>([^<]+)</a>`)
	reCardLink3 = regexp.MustCompile(`(?i)<a[^>]*>(\d{8,14})</a>`)
	reCardLink4 = regexp.MustCompile(`(?i)>(\d{8,14})</a>`)

	reDateOnly  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reDateInner = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

	// 翻页参数提取
	reNextExact  = regexp.MustCompile(`__doPostBack\(["']GridView1["'],\s*["'](Page\$%d)["']\)`)
	reNextAnchor = regexp.MustCompile(`(?is)<a[^>]*href=["']javascript:__doPostBack\((\&quot;|['"])GridView1(\&quot;|['"]),\s*(\&quot;|['"])(Page\$[^'"]+)(\&quot;|['"])\)["'][^>]*>(.*?)</a>`)
	reNextPageNo = regexp.MustCompile(`Page\$(\d+)`)

	// 需求计划号提取（4 级兜底，按原 PHP 顺序）
	rePlanCode1 = regexp.MustCompile(`(?i)(XQ\d{4}[-_][a-zA-Z0-9]+)`)
	rePlanCode2 = regexp.MustCompile(`(?i)id=["'][^"']*Text_plancode[^"']*["'][^>]*>([^<]+)</`)
	rePlanCode3 = regexp.MustCompile(`(?i)<input[^>]*id=["'][^"']*Text_plancode[^"']*["'][^>]*value=["']([^"']+)["']`)
	rePlanCode4 = regexp.MustCompile(`(?i)(XQ\d{6,})`)
)

// ============================================================================
//  本地映射库（与 internal/controller 中的 cemPlanMapFile 结构保持一致）
// ============================================================================

type planRecord struct {
	CardNo     string `json:"card_no"`
	PlanCode   string `json:"plan_code"`
	CreateDate string `json:"create_date"`
	Creator    string `json:"creator"`
	SyncedAt   string `json:"synced_at"`
}

type planMapFile struct {
	UpdatedAt string                 `json:"updated_at"`
	Records   map[string]planRecord `json:"records"`
}

func loadMap(path string) planMapFile {
	res := planMapFile{Records: make(map[string]planRecord)}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return res
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		logf("⚠ 映射库解析失败，本次从空库开始: %v", err)
		return planMapFile{Records: make(map[string]planRecord)}
	}
	if res.Records == nil {
		res.Records = make(map[string]planRecord)
	}
	return res
}

// saveMap 原子写入：先写临时文件再 rename，避免与 Go 后端并发读写时产生半截文件
func saveMap(path string, m planMapFile) error {
	if m.UpdatedAt == "" {
		m.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	}
	data, err := json.MarshalIndent(m, "", "    ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ============================================================================
//  CEM 客户端
// ============================================================================

type client struct {
	host    string
	user    string
	pass    string
	httpCli *http.Client
}

func newClient(host, user, pass string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{
		host: strings.TrimRight(host, "/"),
		user: user,
		pass: pass,
		httpCli: &http.Client{
			Jar:     jar,
			Timeout: 90 * time.Second,
		},
	}
}

// decodeBody 按页面声明的 charset 解码，缺省按 GB18030 兜底。
// 原 PHP toUtf8() 等价实现——CEM 是 ASP.NET 老系统，页面多为 GBK/GB2312，
// 不做真正转码会导致「楚永杰」等中文关键字匹配不到。
func decodeBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	probe := b
	if len(probe) > 4096 {
		probe = probe[:4096]
	}
	if m := reCharset.FindStringSubmatch(string(probe)); len(m) > 1 {
		cs := strings.ToUpper(m[1])
		if cs != "UTF-8" && cs != "UTF8" {
			if dec := decoderFor(cs); dec != nil {
				if out, _, err := transform.Bytes(dec, b); err == nil {
					return string(out)
				}
			}
		}
		return string(b)
	}
	if utf8.Valid(b) {
		return string(b)
	}
	if out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), b); err == nil {
		return string(out)
	}
	return string(b)
}

// decoderFor 返回 *encoding.Decoder（Decoder 是值类型，不可与 nil 比较，故取指针）
func decoderFor(cs string) *encoding.Decoder {
	switch {
	case strings.Contains(cs, "GB"):
		return simplifiedchinese.GB18030.NewDecoder()
	case strings.Contains(cs, "BIG5"):
		return traditionalchinese.Big5.NewDecoder()
	}
	return nil
}

func (c *client) request(target string, post url.Values, referer string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	c.httpCli.Timeout = timeout

	var req *http.Request
	var err error
	if post != nil {
		req, err = http.NewRequest("POST", target, strings.NewReader(post.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, err = http.NewRequest("GET", target, nil)
		if err != nil {
			return "", err
		}
	}
	req.Header.Set("User-Agent", cemDefaultUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return decodeBody(body), nil
}

func (c *client) hasValidSession() bool {
	htmlStr, err := c.request(c.host+cemMainURL, nil, "", 15*time.Second)
	if err != nil || htmlStr == "" {
		return false
	}
	if strings.Contains(htmlStr, "userLogin") || strings.Contains(htmlStr, "userPwd") {
		return false
	}
	return strings.Contains(htmlStr, c.user) || strings.Contains(htmlStr, "欢迎您")
}

func (c *client) login() bool {
	loginURL := c.host + cemLoginURL
	htmlStr, err := c.request(loginURL, nil, "", 25*time.Second)
	if err != nil {
		return false
	}
	vals := url.Values{}
	for k, v := range extractHiddenFields(htmlStr) {
		vals.Set(k, v)
	}
	vals.Set("userName", c.user)
	vals.Set("userPwd", c.pass)
	vals.Set("userLogin", "登 录")
	if _, err := c.request(loginURL, vals, loginURL, 25*time.Second); err != nil {
		return false
	}
	return c.hasValidSession()
}

func (c *client) ensureLogin() bool {
	if c.hasValidSession() {
		return true
	}
	return c.login()
}

func extractHiddenFields(htmlStr string) map[string]string {
	fields := make(map[string]string)
	for _, tag := range reHiddenTag.FindAllString(htmlStr, -1) {
		nm := reInputName.FindStringSubmatch(tag)
		if len(nm) < 2 || nm[1] == "" {
			continue
		}
		val := ""
		if vm := reInputValue.FindStringSubmatch(tag); len(vm) > 1 {
			val = html.UnescapeString(vm[1])
		}
		fields[nm[1]] = val
	}
	return fields
}

// ============================================================================
//  列表页解析
// ============================================================================

type cardRow struct {
	CardNo     string
	CreateDate string
}

func cleanCell(s string) string {
	s = reHtmlTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00A0", " ")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	return strings.TrimSpace(s)
}

// parseListPageRows 翻译自 PHP parseListPageRows()
// 返回命中的流程卡、本页是否全部早于 cutoffDate、本页最早日期
func parseListPageRows(htmlStr, cutoffDate string) ([]cardRow, bool, string) {
	results := make([]cardRow, 0)
	validDates := make([]string, 0)

	for _, trM := range reTrBlock.FindAllStringSubmatch(htmlStr, -1) {
		tr := trM[1]
		if strings.Contains(strings.ToLower(tr), "<th") || strings.Contains(tr, "Page$") {
			continue
		}

		cardNo := ""
		if m := reCardLink1.FindStringSubmatch(tr); len(m) > 1 {
			cardNo = strings.TrimSpace(m[1])
		} else if m := reCardLink2.FindStringSubmatch(tr); len(m) > 1 {
			cardNo = strings.TrimSpace(m[1])
		} else if m := reCardLink3.FindStringSubmatch(tr); len(m) > 1 {
			cardNo = strings.TrimSpace(m[1])
		} else if m := reCardLink4.FindStringSubmatch(tr); len(m) > 1 {
			cardNo = strings.TrimSpace(m[1])
		}
		if cardNo == "" {
			continue
		}

		tdMatches := reTdBlock.FindAllStringSubmatch(tr, -1)
		if len(tdMatches) == 0 {
			continue
		}
		tds := make([]string, 0, len(tdMatches))
		for _, td := range tdMatches {
			tds = append(tds, cleanCell(td[1]))
		}

		creator := ""
		createDate := ""
		for i := len(tds) - 1; i >= 0; i-- {
			if reDateOnly.MatchString(tds[i]) {
				createDate = tds[i]
				if i > 0 {
					creator = tds[i-1]
				}
				break
			}
		}

		if !strings.Contains(creator, cemCreatorFilter) {
			for _, td := range tds {
				if td != "" && td != "创建人" && strings.Contains(td, cemCreatorFilter) {
					creator = td
					if createDate == "" {
						for _, td2 := range tds {
							if m := reDateInner.FindString(td2); m != "" {
								createDate = m
								break
							}
						}
					}
					break
				}
			}
		}

		if createDate != "" {
			validDates = append(validDates, createDate)
		}
		if strings.Contains(creator, cemCreatorFilter) {
			results = append(results, cardRow{CardNo: cardNo, CreateDate: createDate})
		}
	}

	minDateOnPage := ""
	isOlderThanCutoff := false
	if len(validDates) > 0 {
		sort.Strings(validDates)
		minDateOnPage = validDates[0]
		allOlder := true
		for _, d := range validDates {
			if d >= cutoffDate {
				allOlder = false
				break
			}
		}
		isOlderThanCutoff = allOlder
	}
	return results, isOlderThanCutoff, minDateOnPage
}

// findNextPageArgument 翻译自 PHP findNextPageArgument()
func findNextPageArgument(htmlStr string, currentPage int) string {
	targetNum := currentPage + 1

	if m := regexp.MustCompile(fmt.Sprintf(reNextExact.String(), targetNum)).FindStringSubmatch(htmlStr); len(m) > 1 {
		return m[1]
	}

	if m := reNextAnchor.FindAllStringSubmatch(htmlStr, -1); len(m) > 0 {
		for _, item := range m {
			if len(item) < 7 {
				continue
			}
			btnText := strings.TrimSpace(reHtmlTag.ReplaceAllString(item[6], ""))
			if btnText == "..." || btnText == ">" || btnText == "&gt;" || btnText == "下一页" {
				arg := item[4]
				if nm := reNextPageNo.FindStringSubmatch(arg); len(nm) > 1 {
					if atoi(nm[1]) > currentPage {
						return arg
					}
					continue
				}
				return arg
			}
		}
	}

	if strings.Contains(htmlStr, fmt.Sprintf("Page$%d", targetNum)) {
		return fmt.Sprintf("Page$%d", targetNum)
	}
	return ""
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func extractPlanCodeFromDetail(htmlStr string) string {
	if m := rePlanCode1.FindStringSubmatch(htmlStr); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	if m := rePlanCode2.FindStringSubmatch(htmlStr); len(m) > 1 {
		v := strings.TrimSpace(m[1])
		if v != "" && v != "&nbsp;" {
			return v
		}
	}
	if m := rePlanCode3.FindStringSubmatch(htmlStr); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	if m := rePlanCode4.FindStringSubmatch(htmlStr); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ============================================================================
//  同步主流程（翻译自 PHP runSyncRealtime）
// ============================================================================

func logf(format string, args ...any) {
	fmt.Printf("[%s] "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func runSync(days int, mapPath string, dryRun bool, skipThreshold int) error {
	if skipThreshold < 1 {
		skipThreshold = cemDefaultSkipThreshold
	}
	host := strings.TrimSpace(config.Get("CEM_HOST", "http://cem.ieslab.cn"))
	user := strings.TrimSpace(config.Get("CEM_USER", ""))
	pass := strings.TrimSpace(config.Get("CEM_PASS", ""))
	if user == "" || pass == "" {
		return fmt.Errorf("CEM 凭据未配置：请在 backend/.env 中设置 CEM_USER 与 CEM_PASS")
	}

	cutoffDate := time.Now().AddDate(0, 0, -days).Format("2006-01-02")

	logf("==================================================")
	logf("启动 CEM 流程卡增量同步（创建人【%s】，近 %d 天，截止 %s）", cemCreatorFilter, days, cutoffDate)
	logf("增量策略：连续 %d 条已存在则停止翻页；映射库：%s", skipThreshold, mapPath)
	if dryRun {
		logf("⚠ DRY-RUN 模式：只扫描，不写入映射库")
	}
	logf("==================================================")

	c := newClient(host, user, pass)
	logf("正在验证 CEM 登录状态...")
	if !c.ensureLogin() {
		return fmt.Errorf("CEM 登录失败，请检查凭据或网络连通性")
	}
	logf("✔ 登录成功，会话保持正常")

	listURL := host + cemUvQueryURL
	currentHTML, err := c.request(listURL, nil, host+cemMainURL, 25*time.Second)
	if err != nil {
		return fmt.Errorf("打开工单列表失败: %w", err)
	}

	m := loadMap(mapPath)
	page := 1
	totalAdded, totalSkipped, consecutiveSkipped := 0, 0, 0

	for page <= cemMaxPages {
		logf("--------------------------------------------------")
		logf("正在扫描第 %d 页工单表格...", page)

		rows, isOlderThanCutoff, minDateOnPage := parseListPageRows(currentHTML, cutoffDate)
		logf("第 %d 页命中【%s】的流程卡 %d 条（本页最早日期: %s）", page, cemCreatorFilter, len(rows), minDateOnPage)

		for _, row := range rows {
			if existing, ok := m.Records[row.CardNo]; ok && existing.PlanCode != "" {
				consecutiveSkipped++
				totalSkipped++
				logf("  ↳ [已存在] %s 已绑定 [%s]（连续已存在 %d/%d）", row.CardNo, existing.PlanCode, consecutiveSkipped, skipThreshold)
				continue
			}
			consecutiveSkipped = 0

			logf("  ↳ 正在读取新卡号 [%s] 的详情页...", row.CardNo)
			detailURL := host + fmt.Sprintf(cemDetailURLFormat, url.QueryEscape(row.CardNo))
			detailHTML, dErr := c.request(detailURL, nil, listURL, 12*time.Second)
			if dErr != nil {
				logf("  ✖ 流程卡 %s 详情页请求失败: %v", row.CardNo, dErr)
				time.Sleep(cemDetailInterval)
				continue
			}
			planCode := extractPlanCodeFromDetail(detailHTML)
			if planCode == "" {
				logf("  ✖ 【未识别】流程卡 %s 未找到 XQ 计划号", row.CardNo)
			} else {
				m.Records[row.CardNo] = planRecord{
					CardNo:     row.CardNo,
					PlanCode:   planCode,
					CreateDate: row.CreateDate,
					Creator:    cemCreatorFilter,
					SyncedAt:   time.Now().Format("2006-01-02 15:04:05"),
				}
				totalAdded++
				logf("  ✔ 【成功绑定】流程卡 %s ➔ 需求计划号: %s", row.CardNo, planCode)
			}
			time.Sleep(cemDetailInterval)
		}

		// 停止条件 1：连续 N 条已存在（页末判断，不丢当前页剩余卡片）
		if consecutiveSkipped >= skipThreshold {
			logf("本页处理完毕，已连续遇到 %d 条已同步记录（阈值 %d），判定已抵达上次数据边界，停止翻页", consecutiveSkipped, skipThreshold)
			break
		}
		// 停止条件 2：日期越界（硬兜底）
		if isOlderThanCutoff {
			logf("第 %d 页所有单据均早于 %s（本页最早 %s），已达到天数边界，停止翻页", page, cutoffDate, minDateOnPage)
			break
		}

		nextArg := findNextPageArgument(currentHTML, page)
		if nextArg == "" {
			logf("页面已无下一页链接，所有工单扫描完毕")
			break
		}
		logf("正在翻向下一页（导航参数: %s）...", nextArg)

		post := url.Values{}
		for k, v := range extractHiddenFields(currentHTML) {
			post.Set(k, v)
		}
		post.Set("__EVENTTARGET", "GridView1")
		post.Set("__EVENTARGUMENT", nextArg)
		post.Set("Txt_Find", "")

		nextHTML, nErr := c.request(listURL, post, listURL, 30*time.Second)
		if nErr != nil {
			logf("翻页失败: %v，重试一次...", nErr)
			time.Sleep(time.Second)
			nextHTML, nErr = c.request(listURL, post, listURL, 30*time.Second)
			if nErr != nil {
				logf("重试依然失败，本次同步安全终止，保留已入库数据")
				break
			}
		}
		currentHTML = nextHTML
		page++
	}

	if !dryRun {
		m.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
		if err := saveMap(mapPath, m); err != nil {
			return fmt.Errorf("写入映射库失败: %w", err)
		}
	}

	logf("==================================================")
	logf("同步结束：本次新增 %d 条，跳过已存在 %d 条，本地库累计 %d 条", totalAdded, totalSkipped, len(m.Records))
	return nil
}

func main() {
	days := flag.Int("days", cemDefaultDays, "扫描最近 N 天创建的流程卡")
	mapPath := flag.String("map", cemDefaultMapRelPath, "本地映射库路径")
	skip := flag.Int("skip", cemDefaultSkipThreshold, "连续 N 条已存在则停止翻页（调大可减少漏同步，但扫描页数更多、耗时更长）")
	dryRun := flag.Bool("dry-run", false, "只扫描不写盘，用于验证")
	envFile := flag.String("env", "", "指定 .env 路径（默认自动探测）")
	flag.Parse()

	if *envFile != "" {
		config.Load(*envFile)
	} else {
		config.Load()
	}

	if err := runSync(*days, *mapPath, *dryRun, *skip); err != nil {
		logf("❌ %v", err)
		os.Exit(1)
	}
}

package controller

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"vite-system-go/internal/config"
	"vite-system-go/internal/core"
	"vite-system-go/internal/service"
)

// ============================================================================
//  1. 质量追溯系统（TraceController）—— 1:1 严格对齐老系统 TraceController.php + legacy_trace.php + CemClient.php + CemPushService.php
// ============================================================================

const (
	cemDefaultUA         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	cemFilterPrefixesStr = "6002,6003,6004,7004,7005,7006,710"
	cemUploadURL         = "/Quality_Track/ExcelToQuality.aspx"
	cemUvQueryURL        = "/Quality_Track/UV_Query.aspx"
	cemDeviceNo          = "CK3R084X1400604"
)

var (
	reHiddenInput    = regexp.MustCompile(`(?i)<input[^>]*type=["']hidden["'][^>]*>`)
	reInputName      = regexp.MustCompile(`(?i)name=["']([^"']*)["']`)
	reInputValue     = regexp.MustCompile(`(?i)value=["']([^"']*)["']`)
	reAlert          = regexp.MustCompile(`(?i)alert\s*\(\s*["']([^"']+)["']\s*\)`)
	reHidUserName    = regexp.MustCompile(`(?i)name=["']Hid_userName["'][^>]*value=["']([^"']*)["']`)
	reHtmlTag        = regexp.MustCompile(`(?is)<[^>]*>`)
	reWhitespace     = regexp.MustCompile(`\s+`)
	reCardPattern    = regexp.MustCompile(`^2\d{10}$`)
	reBarcodeAnchor1 = regexp.MustCompile(`>(\d{20,26})<`)
	reBarcodeAnchor2 = regexp.MustCompile(`(?is)<td[^>]*>[\s\x{00A0}]*(\d{20,26})[\s\x{00A0}]*</td>`)
	reBarcodeAnchor3 = regexp.MustCompile(`\b(\d{20,26})\b`)
	reHidlabidId     = regexp.MustCompile(`(?i)id=["']Hidlabid["'][^>]*value=["']([^"']*)["']`)
	reHidlabidName   = regexp.MustCompile(`(?i)name=["']Hidlabid["'][^>]*value=["']([^"']*)["']`)
	reTxtBarcodeMId  = regexp.MustCompile(`(?i)id=["']TxtBarcodeM["'][^>]*value=["']([^"']*)["']`)
	reTxtBarcodeMNm  = regexp.MustCompile(`(?i)name=["']TxtBarcodeM["'][^>]*value=["']([^"']*)["']`)
	reDropTypeSelect = regexp.MustCompile(`(?is)<select[^>]*id=["']Drop_Type["'][^>]*>(.*?)</select>`)
	reOptSelected1   = regexp.MustCompile(`(?i)<option[^>]*selected=["']selected["'][^>]*value=["']([^"']*)["']`)
	reOptSelected2   = regexp.MustCompile(`(?i)<option[^>]*value=["']([^"']*)["'][^>]*selected`)
	reLabname        = regexp.MustCompile(`(?i)id=["']Labname["'][^>]*>([^<]+)</`)
	reMyGridView     = regexp.MustCompile(`(?is)<table[^>]*id=["']myGridView["'][^>]*>(.*?)</table>`)
	reGridView1      = regexp.MustCompile(`(?is)<table[^>]*id=["']GridView1["'][^>]*>(.*?)</table>`)
	reTrBlock        = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	reThBlock        = regexp.MustCompile(`(?is)<th[^>]*>(.*?)</th>`)
	reTdBlock        = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	rePcsQty         = regexp.MustCompile(`(?i)PCS\s*([0-9]+(?:\.[0-9]+)?)`)
	reNumQty         = regexp.MustCompile(`^([1-9]\d*(?:\.\d+)?)$`)
	rePlanCode1      = regexp.MustCompile(`(?i)(XQ\d{4}[-_][a-zA-Z0-9]+)`)
	rePlanCode2      = regexp.MustCompile(`(?i)id=["'][^"']*Text_plancode[^"']*["'][^>]*>([^<]+)</`)
	rePlanCode3      = regexp.MustCompile(`(?i)<input[^>]*id=["'][^"']*Text_plancode[^"']*["'][^>]*value=["']([^"']+)["']`)
	rePlanCode4      = regexp.MustCompile(`(?i)(XQ\d{6,})`)
	reGridCardLink   = regexp.MustCompile(`(?i)GridView1_lktransid_\d+["'][^>]*>([^<]+)</a>`)
	reSafeFile       = regexp.MustCompile(`[^A-Za-z0-9_\-]`)
	reAbsPathSanit   = regexp.MustCompile(`/[A-Za-z0-9_.\-]+(/[A-Za-z0-9_.\-]+)+`)

	cemSessionMu     sync.Mutex
	sharedCemJar     http.CookieJar
	erpFetchCacheMap sync.Map // map[string]cachedErpFetch
)

type cachedErpFetch struct {
	Task      map[string]any
	Materials []map[string]any
	UpdatedAt time.Time
}

func getSharedCemJar() http.CookieJar {
	if sharedCemJar == nil {
		sharedCemJar, _ = cookiejar.New(nil)
	}
	return sharedCemJar
}

func resetSharedCemJar() http.CookieJar {
	sharedCemJar, _ = cookiejar.New(nil)
	return sharedCemJar
}

// ============================================================================
//  CemClient 完整封装（对标 backend/includes/CemClient.php + CemConfig.php）
// ============================================================================

type cemClient struct {
	host      string
	user      string
	pass      string
	lastError string
	httpCli   *http.Client
}

func newCemClient() *cemClient {
	h := strings.TrimSpace(config.Get("CEM_HOST", "http://cem.ieslab.cn"))
	if h == "" {
		h = "http://cem.ieslab.cn"
	}
	// ★ 安全规范：凭据一律只从 .env / 环境变量读取，源码内不得保留任何硬编码兜底口令
	u := strings.TrimSpace(config.Get("CEM_USER", ""))
	p := strings.TrimSpace(config.Get("CEM_PASS", ""))
	cemSessionMu.Lock()
	jar := getSharedCemJar()
	cemSessionMu.Unlock()
	return &cemClient{
		host: strings.TrimRight(h, "/"),
		user: u,
		pass: p,
		httpCli: &http.Client{
			Jar:     jar,
			Timeout: 90 * time.Second,
		},
	}
}

func (c *cemClient) assertConfigured() error {
	var miss []string
	if c.user == "" {
		miss = append(miss, "CEM_USER")
	}
	if c.pass == "" {
		miss = append(miss, "CEM_PASS")
	}
	if len(miss) > 0 {
		return fmt.Errorf("CEM 凭据未配置（缺 %s）。请在 backend/.env 中补充后重试。", strings.Join(miss, "、"))
	}
	return nil
}

func (c *cemClient) rawRequest(targetURL string, postData url.Values, referer string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	c.httpCli.Timeout = timeout

	var req *http.Request
	var err error
	if postData != nil {
		req, err = http.NewRequest("POST", targetURL, strings.NewReader(postData.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, err = http.NewRequest("GET", targetURL, nil)
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
		return "", fmt.Errorf("CURL 错误: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("页面不存在 (404): %s", targetURL)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return gbkToUtf8IfNeeded(bodyBytes), nil
}

func (c *cemClient) hasValidSession() bool {
	if c.user == "" {
		return false
	}
	htmlStr, err := c.rawRequest(c.host+"/main.aspx", nil, "", 15*time.Second)
	if err != nil || htmlStr == "" {
		return false
	}
	if m := reHidUserName.FindStringSubmatch(htmlStr); len(m) > 1 {
		return strings.TrimSpace(m[1]) == c.user
	}
	if strings.Contains(htmlStr, "userLogin") || strings.Contains(htmlStr, "userPwd") {
		return false
	}
	return strings.Contains(htmlStr, c.user) || strings.Contains(htmlStr, "欢迎您")
}

func (c *cemClient) login() bool {
	cemSessionMu.Lock()
	defer cemSessionMu.Unlock()

	c.httpCli.Jar = getSharedCemJar()
	if c.hasValidSession() {
		return true
	}

	c.httpCli.Jar = resetSharedCemJar()
	loginURL := c.host + "/login.aspx"
	htmlStr, err := c.rawRequest(loginURL, nil, "", 25*time.Second)
	if err != nil {
		c.lastError = "登录请求异常: " + err.Error()
		return false
	}

	fields := extractHiddenFields(htmlStr)
	vals := url.Values{}
	for k, v := range fields {
		vals.Set(k, v)
	}
	vals.Set("userName", c.user)
	vals.Set("userPwd", c.pass)
	vals.Set("userLogin", "登 录")

	_, err = c.rawRequest(loginURL, vals, loginURL, 25*time.Second)
	if err != nil {
		c.lastError = "登录请求异常: " + err.Error()
		return false
	}

	ok := c.hasValidSession()
	if !ok {
		c.lastError = "提交登录后会话校验仍未通过（账号或口令可能不正确，或 CEM 登录页字段已变更）"
	}
	return ok
}

func (c *cemClient) ensureLogin() bool {
	if c.hasValidSession() {
		return true
	}
	return c.login()
}

// queryProjectCardNo 对标 CemClient::queryProjectCardNo
func (c *cemClient) queryProjectCardNo(taskOrder string) (string, error) {
	return c.queryProjectCardNoInternal(taskOrder, false)
}

func (c *cemClient) queryProjectCardNoInternal(taskOrder string, retried bool) (string, error) {
	c.ensureLogin()
	taskOrder = strings.TrimSpace(taskOrder)
	targetURL := c.host + cemUvQueryURL

	pageHtml, err := c.rawRequest(targetURL, nil, c.host+"/main.aspx", 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("无法打开 CEM 任务查询页面: %v", err)
	}

	if matched := c.parseCardNoFromHtml(pageHtml, taskOrder); matched != "" {
		_ = os.WriteFile("/tmp/cem_query_success.txt", []byte(fmt.Sprintf("时间: %s\n来源: 页面首屏\n工单: %s\n流程卡号: %s\n",
			time.Now().Format("2006-01-02 15:04:05"), taskOrder, matched)), 0644)
		return matched, nil
	}

	baseFields := extractHiddenFields(pageHtml)
	postData := url.Values{}
	for k, v := range baseFields {
		postData.Set(k, v)
	}
	postData.Set("__EVENTTARGET", "")
	postData.Set("__EVENTARGUMENT", "")
	postData.Set("Txt_Find", taskOrder)
	postData.Set("imgbtn.x", "10")
	postData.Set("imgbtn.y", "10")
	postData.Set("hidSQL", "")

	resultHtml, err := c.rawRequest(targetURL, postData, targetURL, 45*time.Second)
	if err != nil {
		return "", fmt.Errorf("查询 CEM 任务失败: %v", err)
	}

	if !retried && strings.Contains(resultHtml, "userLogin") && strings.Contains(resultHtml, "userPwd") {
		if c.login() {
			return c.queryProjectCardNoInternal(taskOrder, true)
		}
	}

	debugFile := "/tmp/cem_query_result_" + reSafeFile.ReplaceAllString(taskOrder, "_") + ".html"
	_ = os.WriteFile(debugFile, []byte(resultHtml), 0644)

	if cardNo := c.parseCardNoFromHtml(resultHtml, taskOrder); cardNo != "" {
		_ = os.WriteFile("/tmp/cem_query_success.txt", []byte(fmt.Sprintf("时间: %s\n来源: POST 查询\n工单: %s\n流程卡号: %s\n",
			time.Now().Format("2006-01-02 15:04:05"), taskOrder, cardNo)), 0644)
		return cardNo, nil
	}

	plain := stripTagsAndCollapseSpace(resultHtml)
	runes := []rune(plain)
	if len(runes) > 120 {
		plain = string(runes[:120])
	}
	return "", fmt.Errorf("在 CEM 任务管理页面未查到任务【%s】对应的流程卡号 (响应片段: %s)", taskOrder, plain)
}

// parseCardNoFromHtml 对标 CemClient::parseCardNoFromHtml
func (c *cemClient) parseCardNoFromHtml(htmlStr, taskOrder string) string {
	if htmlStr == "" || taskOrder == "" {
		return ""
	}
	tableHtml := htmlStr
	if tm := reGridView1.FindStringSubmatch(htmlStr); len(tm) > 0 {
		tableHtml = tm[0]
	}

	cardIdx := 0
	orderIdx := 4

	if headerTr := reTrBlock.FindStringSubmatch(tableHtml); len(headerTr) > 1 {
		thMatches := reThBlock.FindAllStringSubmatch(headerTr[1], -1)
		for i, thM := range thMatches {
			headText := reWhitespace.ReplaceAllString(stripHtmlTags(thM[1]), "")
			if headText == "流程卡号" {
				cardIdx = i
			}
			if headText == "关联单据" {
				orderIdx = i
			}
		}
	}

	trMatches := reTrBlock.FindAllStringSubmatch(tableHtml, -1)
	for _, trM := range trMatches {
		tr := trM[1]
		if strings.Contains(strings.ToLower(tr), "<th") {
			continue
		}
		if !strings.Contains(tr, "GridView1_lktransid") {
			continue
		}
		tdMatches := reTdBlock.FindAllStringSubmatch(tr, -1)
		if len(tdMatches) == 0 {
			continue
		}
		cleanTds := make([]string, 0, len(tdMatches))
		for _, tdM := range tdMatches {
			cleanTds = append(cleanTds, stripTagsAndCollapseSpace(tdM[1]))
		}
		if cardIdx >= len(cleanTds) || orderIdx >= len(cleanTds) {
			continue
		}
		card := strings.TrimSpace(cleanTds[cardIdx])
		order := strings.TrimSpace(cleanTds[orderIdx])

		orderMatched := (order == taskOrder) ||
			strings.Contains(order, taskOrder) ||
			(order != "" && strings.Contains(taskOrder, order))
		if !orderMatched {
			continue
		}
		if reCardPattern.MatchString(card) {
			return card
		}
	}
	return ""
}

// queryRelNoByCardNo 对标 CemClient::queryRelNoByCardNo
func (c *cemClient) queryRelNoByCardNo(cardNo string) string {
	return c.queryRelNoByCardNoInternal(cardNo, false)
}

func (c *cemClient) queryRelNoByCardNoInternal(cardNo string, retried bool) string {
	c.ensureLogin()
	cardNo = strings.TrimSpace(cardNo)
	if cardNo == "" {
		return ""
	}
	targetURL := c.host + cemUvQueryURL

	pageHtml, err := c.rawRequest(targetURL, nil, c.host+"/main.aspx", 30*time.Second)
	if err != nil {
		return ""
	}
	if rel := c.parseRelNoFromGrid(pageHtml, cardNo); rel != "" {
		return rel
	}

	baseFields := extractHiddenFields(pageHtml)
	postData := url.Values{}
	for k, v := range baseFields {
		postData.Set(k, v)
	}
	postData.Set("__EVENTTARGET", "")
	postData.Set("__EVENTARGUMENT", "")
	postData.Set("Txt_Find", cardNo)
	postData.Set("imgbtn.x", "10")
	postData.Set("imgbtn.y", "10")
	postData.Set("hidSQL", "")

	resultHtml, err := c.rawRequest(targetURL, postData, targetURL, 45*time.Second)
	if err != nil {
		return ""
	}
	if !retried && strings.Contains(resultHtml, "userLogin") && strings.Contains(resultHtml, "userPwd") {
		if c.login() {
			return c.queryRelNoByCardNoInternal(cardNo, true)
		}
	}
	return c.parseRelNoFromGrid(resultHtml, cardNo)
}

// parseRelNoFromGrid 对标 CemClient::parseRelNoFromGrid
func (c *cemClient) parseRelNoFromGrid(htmlStr, cardNo string) string {
	if htmlStr == "" || cardNo == "" {
		return ""
	}
	tableHtml := htmlStr
	if tm := reGridView1.FindStringSubmatch(htmlStr); len(tm) > 0 {
		tableHtml = tm[0]
	}

	cardIdx := 0
	relIdx := 4

	if headerTr := reTrBlock.FindStringSubmatch(tableHtml); len(headerTr) > 1 {
		thMatches := reThBlock.FindAllStringSubmatch(headerTr[1], -1)
		for i, thM := range thMatches {
			headText := reWhitespace.ReplaceAllString(stripHtmlTags(thM[1]), "")
			if headText == "流程卡号" {
				cardIdx = i
			}
			if headText == "关联单据" {
				relIdx = i
			}
		}
	}

	trMatches := reTrBlock.FindAllStringSubmatch(tableHtml, -1)
	for _, trM := range trMatches {
		tr := trM[1]
		if strings.Contains(strings.ToLower(tr), "<th") {
			continue
		}
		if !strings.Contains(tr, "GridView1_lktransid") {
			continue
		}
		tdMatches := reTdBlock.FindAllStringSubmatch(tr, -1)
		if len(tdMatches) == 0 {
			continue
		}
		cleanTds := make([]string, 0, len(tdMatches))
		for _, tdM := range tdMatches {
			cleanTds = append(cleanTds, stripTagsAndCollapseSpace(tdM[1]))
		}
		if cardIdx >= len(cleanTds) || relIdx >= len(cleanTds) {
			continue
		}
		card := strings.TrimSpace(cleanTds[cardIdx])
		rel := strings.TrimSpace(cleanTds[relIdx])
		if card == cardNo {
			return rel
		}
	}
	return ""
}

// uploadQualityExcel 对标 CemClient::uploadQualityExcel
func (c *cemClient) uploadQualityExcel(localFilePath, uploadName string) (string, string, error) {
	return c.uploadQualityExcelInternal(localFilePath, uploadName, false)
}

func (c *cemClient) uploadQualityExcelInternal(localFilePath, uploadName string, retried bool) (string, string, error) {
	fileBytes, err := os.ReadFile(localFilePath)
	if err != nil {
		return "", "", fmt.Errorf("待上传的本地 Excel 文件不存在: %s", localFilePath)
	}
	if !c.ensureLogin() {
		reason := c.lastError
		if reason == "" {
			reason = "未知原因"
		}
		return "", "", fmt.Errorf("CEM 登录失败，无法上传：%s", reason)
	}

	targetURL := c.host + cemUploadURL
	pageHtml, err := c.rawRequest(targetURL, nil, c.host+"/main.aspx", 45*time.Second)
	if err != nil {
		return "", "", err
	}
	if !strings.Contains(pageHtml, "FileUpload1") && !strings.Contains(pageHtml, "Button1") {
		plain := stripTagsAndCollapseSpace(pageHtml)
		runes := []rune(plain)
		if len(runes) > 150 {
			plain = string(runes[:150])
		}
		return "", "", fmt.Errorf("无法载入 CEM Excel 导入页面: %s", plain)
	}

	fields := extractHiddenFields(pageHtml)
	if uploadName == "" {
		uploadName = filepath.Base(localFilePath)
	}

	var bodyBuf bytes.Buffer
	mpw := multipart.NewWriter(&bodyBuf)

	viewStateGen := fields["__VIEWSTATEGENERATOR"]
	if viewStateGen == "" {
		viewStateGen = "C4357332"
	}

	_ = mpw.WriteField("__EVENTTARGET", "")
	_ = mpw.WriteField("__EVENTARGUMENT", "")
	_ = mpw.WriteField("__VIEWSTATE", fields["__VIEWSTATE"])
	_ = mpw.WriteField("__VIEWSTATEGENERATOR", viewStateGen)
	_ = mpw.WriteField("__EVENTVALIDATION", fields["__EVENTVALIDATION"])
	if enc, ok := fields["__VIEWSTATEENCRYPTED"]; ok {
		_ = mpw.WriteField("__VIEWSTATEENCRYPTED", enc)
	}

	mimeType := "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	if strings.HasSuffix(strings.ToLower(localFilePath), ".xls") {
		mimeType = "application/vnd.ms-excel"
	}
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="FileUpload1"; filename="%s"`, uploadName))
	partHeader.Set("Content-Type", mimeType)
	partWriter, err := mpw.CreatePart(partHeader)
	if err != nil {
		return "", "", err
	}
	_, _ = partWriter.Write(fileBytes)

	_ = mpw.WriteField("Button1", "导入数据")
	_ = mpw.Close()

	c.httpCli.Timeout = 180 * time.Second
	req, err := http.NewRequest("POST", targetURL, &bodyBuf)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", mpw.FormDataContentType())
	req.Header.Set("User-Agent", cemDefaultUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Origin", c.host)
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Referer", targetURL)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("CURL 上传错误: %v", err)
	}
	respBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resultHtml := gbkToUtf8IfNeeded(respBytes)

	if !retried && strings.Contains(resultHtml, "userLogin") && strings.Contains(resultHtml, "userPwd") {
		if c.login() {
			return c.uploadQualityExcelInternal(localFilePath, uploadName, true)
		}
	}

	alertMsg := ""
	if mAlert := reAlert.FindStringSubmatch(resultHtml); len(mAlert) > 1 {
		alertMsg = strings.TrimSpace(mAlert[1])
	}

	_ = os.WriteFile("/tmp/cem_last_upload_result.html", []byte(resultHtml), 0644)
	return alertMsg, resultHtml, nil
}

// fetchExistingBarcode 对标 legacy_trace.php fetchExistingBarcode + extractMinBarcodeFromList
func (c *cemClient) fetchExistingBarcode(tasksid, detailHtml string) string {
	specid := "1"
	if m := reHidlabidId.FindStringSubmatch(detailHtml); len(m) > 1 {
		specid = strings.TrimSpace(m[1])
	} else if m := reHidlabidName.FindStringSubmatch(detailHtml); len(m) > 1 {
		specid = strings.TrimSpace(m[1])
	}

	batchno := ""
	if m := reTxtBarcodeMId.FindStringSubmatch(detailHtml); len(m) > 1 {
		batchno = strings.TrimSpace(m[1])
	} else if m := reTxtBarcodeMNm.FindStringSubmatch(detailHtml); len(m) > 1 {
		batchno = strings.TrimSpace(m[1])
	}

	droptype := "105"
	if sm := reDropTypeSelect.FindStringSubmatch(detailHtml); len(sm) > 1 {
		if om := reOptSelected1.FindStringSubmatch(sm[1]); len(om) > 1 {
			droptype = strings.TrimSpace(om[1])
		} else if om := reOptSelected2.FindStringSubmatch(sm[1]); len(om) > 1 {
			droptype = strings.TrimSpace(om[1])
		}
	}

	printURL := fmt.Sprintf("%s/Quality_Track/cg_PrintModleP.aspx?taskID=%s&specid=%s&batchno=%s&droptype=%s",
		c.host, url.QueryEscape(tasksid), url.QueryEscape(specid), url.QueryEscape(batchno), url.QueryEscape(droptype))
	referer := c.host + "/Quality_Track/Quality_Tasks_Operation.aspx?Tasksid=" + url.QueryEscape(tasksid)

	printHtml, err := c.rawRequest(printURL, nil, referer, 15*time.Second)
	if err == nil && printHtml != "" {
		if minCode := extractMinBarcodeFromList(printHtml); minCode != "" {
			return minCode
		}
	}
	return extractMinBarcodeFromList(detailHtml)
}

func extractMinBarcodeFromList(htmlStr string) string {
	if htmlStr == "" {
		return ""
	}
	anchors := []string{">条码<", ">条码：<", ">条码:<", ">条码</", ">条码 "}
	anchorPos := -1
	for _, a := range anchors {
		if p := strings.Index(htmlStr, a); p != -1 && (anchorPos == -1 || p < anchorPos) {
			anchorPos = p
		}
	}
	if anchorPos == -1 {
		for _, a := range []string{">工程号<", ">箱柜号<", ">任务号<"} {
			if p := strings.Index(htmlStr, a); p != -1 {
				anchorPos = p
				break
			}
		}
	}
	scanHtml := htmlStr
	if anchorPos != -1 {
		scanHtml = htmlStr[anchorPos:]
	}

	seen := make(map[string]bool)
	barcodes := make([]string, 0)
	addMatches := func(matches [][]string) {
		for _, m := range matches {
			if len(m) > 1 && m[1] != "" && !seen[m[1]] {
				seen[m[1]] = true
				barcodes = append(barcodes, m[1])
			}
		}
	}
	addMatches(reBarcodeAnchor1.FindAllStringSubmatch(scanHtml, -1))
	addMatches(reBarcodeAnchor2.FindAllStringSubmatch(scanHtml, -1))
	addMatches(reBarcodeAnchor3.FindAllStringSubmatch(scanHtml, -1))

	if len(barcodes) == 0 {
		return ""
	}
	sort.Strings(barcodes)
	return barcodes[0]
}

// ============================================================================
//  本地 CEM 需求计划号映射库（cem_plan_map.json）管理与增量同步
// ============================================================================

type cemPlanRecord struct {
	CardNo     string `json:"card_no"`
	PlanCode   string `json:"plan_code"`
	CreateDate string `json:"create_date"`
	Creator    string `json:"creator"`
	SyncedAt   string `json:"synced_at"`
	Barcode    string `json:"barcode,omitempty"`
}

type cemPlanMapFile struct {
	UpdatedAt string                   `json:"updated_at"`
	Records   map[string]cemPlanRecord `json:"records"`
}

func cemPlanMapCandidatePaths() []string {
	return []string{
		"storage/cem_plan_map.json",
		// 过渡期数据源（只读）：旧 PHP 系统的 cron 仍在维护这份映射，
		// 作为补充来源合并读取。待 Go 侧覆盖度足够、或把 CEM 同步改造为
		// Go 定时任务后，再移除此路径。
		"/www/wwwroot/vite/backend/app/cem/cem_plan_map.json",
	}
}

func loadCemPlanMap() cemPlanMapFile {
	res := cemPlanMapFile{
		Records: make(map[string]cemPlanRecord),
	}
	for _, p := range cemPlanMapCandidatePaths() {
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) == 0 {
			continue
		}
		var parsed cemPlanMapFile
		if err := json.Unmarshal(raw, &parsed); err == nil && len(parsed.Records) > 0 {
			if parsed.UpdatedAt > res.UpdatedAt {
				res.UpdatedAt = parsed.UpdatedAt
			}
			for k, v := range parsed.Records {
				if v.CardNo == "" {
					v.CardNo = k
				}
				if existing, ok := res.Records[k]; !ok || existing.PlanCode == "" {
					res.Records[k] = v
				}
			}
		}
	}
	return res
}

func saveCemPlanMap(m cemPlanMapFile) {
	if m.UpdatedAt == "" {
		m.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	}
	data, err := json.MarshalIndent(m, "", "    ")
	if err != nil {
		return
	}
	// 统一写入 storage/。
	// 不再写 app/cem/ —— 该 PHP 脚本目录已随纯 Go 化删除，若保留写回逻辑
	// 会把 app/cem 目录重新创建出来，导致每次清理都被代码自动破坏。
	_ = os.MkdirAll("storage", 0755)
	_ = os.WriteFile("storage/cem_plan_map.json", data, 0644)
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

// syncRecentCemPlanMap 当本地映射库未命中某个 XQ 计划号时，自动增量扫描 CEM UV_Query 首页由「楚永杰」创建的新流程卡
func syncRecentCemPlanMap(cem *cemClient, planMap *cemPlanMapFile) {
	if !cem.ensureLogin() {
		return
	}
	listURL := cem.host + cemUvQueryURL
	htmlStr, err := cem.rawRequest(listURL, nil, cem.host+"/main.aspx", 20*time.Second)
	if err != nil || htmlStr == "" {
		return
	}
	trMatches := reTrBlock.FindAllStringSubmatch(htmlStr, -1)
	updated := false
	for _, trM := range trMatches {
		tr := trM[1]
		if strings.Contains(strings.ToLower(tr), "<th") || strings.Contains(tr, "Page$") {
			continue
		}
		if !strings.Contains(tr, "楚永杰") {
			continue
		}
		cm := reGridCardLink.FindStringSubmatch(tr)
		if len(cm) < 2 {
			continue
		}
		cardNo := strings.TrimSpace(cm[1])
		if cardNo == "" {
			continue
		}
		if existing, ok := planMap.Records[cardNo]; ok && existing.PlanCode != "" {
			continue
		}
		detailURL := cem.host + "/Quality_Track/Quality_Tasks_Operation.aspx?Tasksid=" + url.QueryEscape(cardNo)
		detailHtml, dErr := cem.rawRequest(detailURL, nil, listURL, 12*time.Second)
		if dErr != nil || detailHtml == "" {
			continue
		}
		planCode := extractPlanCodeFromDetail(detailHtml)
		if planCode != "" {
			planMap.Records[cardNo] = cemPlanRecord{
				CardNo:   cardNo,
				PlanCode: planCode,
				Creator:  "楚永杰",
				SyncedAt: time.Now().Format("2006-01-02 15:04:05"),
			}
			updated = true
		}
	}
	if updated {
		planMap.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
		saveCemPlanMap(*planMap)
	}
}

// resolveXqTasksid 根据 XQ 单号从本地映射库匹配对应的 11 位流程卡号
func resolveXqTasksid(wo string, cem *cemClient) (string, error) {
	kwClean := strings.ToUpper(strings.TrimSpace(wo))
	if reDigitsOnly.MatchString(kwClean) {
		return kwClean, nil
	}

	planMap := loadCemPlanMap()
	findMatches := func(m cemPlanMapFile) []string {
		var matched []string
		for cNo, data := range m.Records {
			if data.PlanCode != "" && strings.Contains(strings.ToUpper(data.PlanCode), kwClean) {
				matched = append(matched, cNo)
			}
		}
		sort.Strings(matched)
		return matched
	}

	matched := findMatches(planMap)
	if len(matched) == 0 && cem != nil {
		syncRecentCemPlanMap(cem, &planMap)
		matched = findMatches(planMap)
	}

	if len(matched) == 1 {
		return matched[0], nil
	}
	if len(matched) > 1 {
		return "", fmt.Errorf("需求计划号【%s】对应多个流程卡号：%s，请使用具体流程卡号查询！", wo, strings.Join(matched, ", "))
	}
	return "", fmt.Errorf("在本地映射库未查询到【%s】对应的流程卡！请先在流程卡查询页面同步。", wo)
}

// resolveXqCard 对标 backfill_tasksid.php resolveXqCard(wo) -> (tasksid, relNo, error)
func resolveXqCard(wo string) (string, string, error) {
	cem := newCemClient()
	tasksid, err := resolveXqTasksid(wo, cem)
	if err != nil {
		return "", "", err
	}
	if !cem.ensureLogin() {
		return tasksid, "", fmt.Errorf("登录 CEM 失败，请检查网络连接与凭据配置")
	}
	relNo := cem.queryRelNoByCardNo(tasksid)
	return tasksid, relNo, nil
}

func extractHiddenFields(htmlContent string) map[string]string {
	fields := make(map[string]string)
	matches := reHiddenInput.FindAllString(htmlContent, -1)
	for _, tag := range matches {
		nameMatch := reInputName.FindStringSubmatch(tag)
		if len(nameMatch) > 1 && nameMatch[1] != "" {
			name := nameMatch[1]
			val := ""
			valMatch := reInputValue.FindStringSubmatch(tag)
			if len(valMatch) > 1 {
				val = html.UnescapeString(valMatch[1])
			}
			fields[name] = val
		}
	}
	return fields
}

func stripHtmlTags(s string) string {
	return reHtmlTag.ReplaceAllString(s, "")
}

func stripTagsAndCollapseSpace(s string) string {
	txt := html.UnescapeString(stripHtmlTags(s))
	txt = strings.ReplaceAll(txt, "\xc2\xa0", " ")
	txt = strings.ReplaceAll(txt, "&nbsp;", " ")
	return strings.TrimSpace(reWhitespace.ReplaceAllString(txt, " "))
}

// ============================================================================
//  TraceController 结构体与权限 / 用户字典辅助方法
// ============================================================================

type TraceController struct{}

func NewTraceController() *TraceController {
	return &TraceController{}
}

func (ctrl *TraceController) calcUserPerms(db *sql.DB, user map[string]any) map[string]int {
	if core.IsAdminUser(user) {
		return map[string]int{
			"can_add_material":  1,
			"can_scan":          1,
			"can_view":          1,
			"can_edit_record":   1,
			"can_delete_record": 1,
			"is_admin":          1,
		}
	}
	uid := core.AsInt(user["id"])
	row, _ := core.QueryRowMap(db, "SELECT can_add_material, can_scan, can_view, can_edit_record, can_delete_record FROM trace_permissions WHERE user_id = ? LIMIT 1", uid)
	if row == nil {
		return map[string]int{
			"can_add_material":  0,
			"can_scan":          1,
			"can_view":          1,
			"can_edit_record":   0,
			"can_delete_record": 0,
			"is_admin":          0,
		}
	}
	return map[string]int{
		"can_add_material":  core.AsInt(row["can_add_material"]),
		"can_scan":          core.AsInt(row["can_scan"]),
		"can_view":          core.AsInt(row["can_view"]),
		"can_edit_record":   core.AsInt(row["can_edit_record"]),
		"can_delete_record": core.AsInt(row["can_delete_record"]),
		"is_admin":          0,
	}
}

// buildUserNameDictionaries 对标 TraceController.php 全量用户字典与历史特殊编号映射
func (ctrl *TraceController) buildUserNameDictionaries(db *sql.DB) (map[string]string, map[string]string, map[string]string) {
	usersList, _ := core.QueryMaps(db, "SELECT id, username, full_name, badge_no FROM users")
	userByBadge := make(map[string]string)
	userById := make(map[string]string)
	for _, u := range usersList {
		name := strings.TrimSpace(core.AsString(u["full_name"]))
		if name == "" {
			name = strings.TrimSpace(core.AsString(u["username"]))
		}
		badge := strings.TrimSpace(core.AsString(u["badge_no"]))
		if badge != "" {
			userByBadge[badge] = name
			trimmed := strings.TrimLeft(badge, "0")
			if trimmed != "" {
				userByBadge[trimmed] = name
			}
		}
		uidStr := strings.TrimSpace(core.AsString(u["id"]))
		if uidStr != "" {
			userById[uidStr] = name
		}
		uname := strings.TrimSpace(core.AsString(u["username"]))
		if uname != "" && userByBadge[uname] == "" {
			userByBadge[uname] = name
		}
	}

	pick := func(candidates ...string) string {
		for _, c := range candidates {
			if c != "" {
				return c
			}
		}
		return ""
	}
	specialMap := map[string]string{
		"1":  pick(userByBadge["10279"], "李日辉"),
		"19": pick(userByBadge["01531"], userByBadge["1531"], "刘莉"),
		"26": pick(userByBadge["20280"], "孙远超"),
		"71": pick(userByBadge["20280"], "孙远超"),
	}
	return userByBadge, userById, specialMap
}

func (ctrl *TraceController) resolveUserRealName(rawVal string, userByBadge, userById, specialMap map[string]string) string {
	raw := strings.TrimSpace(rawVal)
	if raw == "" {
		return ""
	}
	if v, ok := specialMap[raw]; ok && v != "" {
		return v
	}
	if v, ok := userByBadge[raw]; ok && v != "" {
		return v
	}
	if trimmed := strings.TrimLeft(raw, "0"); trimmed != "" {
		if v, ok := userByBadge[trimmed]; ok && v != "" {
			return v
		}
	}
	if v, ok := userById[raw]; ok && v != "" {
		return v
	}
	return raw
}

// Init GET /api/trace/init
func (ctrl *TraceController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "初始化失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	core.Success(c, "success", gin.H{
		"user_perms": perms,
		"user": gin.H{
			"id":        sess.User["id"],
			"username":  sess.User["username"],
			"full_name": core.AsString(sess.User["full_name"]),
			"is_admin":  perms["is_admin"],
		},
	})
}

type erpKittingStat struct {
	req  float64
	send float64
}

// querySqlsrvKittingSummary 调用 SQL Server [dbo].[自定义ERP组件_P] 汇总物料需求量与已发料数量
func (ctrl *TraceController) querySqlsrvKittingSummary(sqlsrvDB *sql.DB, wo string) ([]map[string]any, []string, map[string]*erpKittingStat, error) {
	summary := make(map[string]*erpKittingStat)
	if sqlsrvDB == nil {
		return nil, nil, summary, fmt.Errorf("SQL Server 未连接")
	}
	rows, err := sqlsrvDB.Query("SET NOCOUNT ON; SET ANSI_NULLS ON; SET ANSI_WARNINGS ON; EXEC [dbo].[自定义ERP组件_P] @WorkOrders = @p1", wo)
	if err != nil {
		return nil, nil, summary, err
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	for len(cols) == 0 && rows.NextResultSet() {
		cols, _ = rows.Columns()
	}
	if len(cols) == 0 {
		return nil, nil, summary, nil
	}

	colCode := getExactColName(cols, []string{"物料编码", "组件编码", "编码", "子项物料代码"})
	if colCode == "" {
		colCode = "物料编码"
	}
	colReqQty := getExactColName(cols, []string{"需求量", "需求数量"})
	colSendQty := getExactColName(cols, []string{"已发料", "已发料数量", "实发数量", "完成量"})

	rawRows := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		rMap := make(map[string]any, len(cols))
		for i, cName := range cols {
			val := values[i]
			if b, ok := val.([]byte); ok {
				rMap[cName] = gbkToUtf8IfNeeded(b)
			} else {
				rMap[cName] = val
			}
		}
		rawRows = append(rawRows, rMap)

		mCode := strings.TrimSpace(core.AsString(rMap[colCode]))
		if mCode == "" {
			continue
		}
		reqStr := "0"
		if colReqQty != "" {
			reqStr = strings.ReplaceAll(strings.TrimSpace(core.AsString(rMap[colReqQty])), ",", "")
		}
		sendStr := "0"
		if colSendQty != "" {
			sendStr = strings.ReplaceAll(strings.TrimSpace(core.AsString(rMap[colSendQty])), ",", "")
		}
		reqQty := core.AsFloat(reqStr)
		sendQty := core.AsFloat(sendStr)

		st, ok := summary[mCode]
		if !ok {
			st = &erpKittingStat{}
			summary[mCode] = st
		}
		st.req += reqQty
		st.send += sendQty
	}

	return rawRows, cols, summary, nil
}

// CheckOrFetchErp GET|POST /api/trace/checkOrFetchErp
// 严格对齐 legacy_trace.php 的 check_or_fetch_erp 分支（含 XQ 工程发货单 + 生产工单）
func (ctrl *TraceController) CheckOrFetchErp(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询工单失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_add_material"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有添加/编辑追溯物料的权限！", gin.H{})
		return
	}

	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	if wo == "" {
		core.Error(c, "请输入工单号！", gin.H{})
		return
	}

	isXq := strings.HasPrefix(strings.ToUpper(wo), "XQ")

	existingTask, _ := core.QueryRowMap(db, "SELECT * FROM trace_tasks WHERE task_order = ? LIMIT 1", wo)
	savedConfig := make(map[string]map[string]any)
	if existingTask != nil {
		stBoms, _ := core.QueryMaps(db, "SELECT child_code, unit_quantity, scan_point FROM trace_bom_config WHERE task_order = ?", wo)
		for _, b := range stBoms {
			cc := strings.TrimSpace(core.AsString(b["child_code"]))
			savedConfig[cc] = map[string]any{
				"unit_quantity": core.AsFloat(b["unit_quantity"]),
				"scan_point":    core.AsInt(b["scan_point"]),
			}
		}
	}

	sqlsrvDB, _ := core.GetDB("sqlsrv")
	_, _, kittingSummary, _ := ctrl.querySqlsrvKittingSummary(sqlsrvDB, wo)

	// ==================== 分支 A：工程发货 XQ 单号处理 ====================
	if isXq {
		cem := newCemClient()
		tasksid, rErr := resolveXqTasksid(wo, cem)
		if rErr != nil {
			core.Error(c, rErr.Error(), gin.H{})
			return
		}

		if !cem.ensureLogin() {
			core.Error(c, "登录 CEM 失败，请检查网络连接。", gin.H{})
			return
		}

		detailURL := cem.host + "/Quality_Track/Quality_Tasks_Operation.aspx?Tasksid=" + url.QueryEscape(tasksid)
		rawHtml, dErr := cem.rawRequest(detailURL, nil, "", 25*time.Second)
		if dErr != nil || len(rawHtml) < 600 || strings.Contains(rawHtml, "未找到") || strings.Contains(rawHtml, "不存在") {
			core.Error(c, fmt.Sprintf("系统内未查询到流程卡号【%s】的相关数据！", tasksid), gin.H{})
			return
		}

		// ★ rel_no 取自已服务端渲染的 UV_Query 列表页第 4 列「关联单据」
		relNo := cem.queryRelNoByCardNo(tasksid)
		labName := ""
		if m := reLabname.FindStringSubmatch(rawHtml); len(m) > 1 {
			labName = strings.TrimSpace(m[1])
		}

		detailBarcode := cem.fetchExistingBarcode(tasksid, rawHtml)
		if existingTask != nil && detailBarcode != "" && core.AsString(existingTask["parent_code"]) != detailBarcode {
			bp := detailBarcode
			if len(bp) > 16 {
				bp = bp[:16]
			}
			_, _ = db.Exec("UPDATE trace_tasks SET parent_code = ?, batch_prefix = ? WHERE task_order = ?", detailBarcode, bp, wo)
			existingTask["parent_code"] = detailBarcode
			existingTask["batch_prefix"] = bp
		}

		parentCode := wo
		batchPrefix := ""
		if detailBarcode != "" {
			parentCode = detailBarcode
			if len(detailBarcode) > 16 {
				batchPrefix = detailBarcode[:16]
			} else {
				batchPrefix = detailBarcode
			}
		}
		desc := labName
		if desc == "" {
			desc = "工程发货单 " + wo
		}

		task := map[string]any{
			"task_order":   wo,
			"tasksid":      tasksid,
			"parent_code":  parentCode,
			"description":  desc,
			"quantity":     1,
			"batch_prefix": batchPrefix,
			"batch_no":     detailBarcode,
			"order_type":   "xq_project",
			"rel_no":       relNo,
			"existing_sn":  detailBarcode,
		}

		prefixes := strings.Split(cemFilterPrefixesStr, ",")
		materials := make([]map[string]any, 0)

		if gridMatch := reMyGridView.FindStringSubmatch(rawHtml); len(gridMatch) > 1 {
			inner := gridMatch[1]
			trMatches := reTrBlock.FindAllStringSubmatch(inner, -1)
			for _, trM := range trMatches {
				row := trM[0]
				if strings.Contains(strings.ToLower(row), "<th") || strings.Contains(row, "Page$") {
					continue
				}
				tdMatches := reTdBlock.FindAllStringSubmatch(row, -1)
				if len(tdMatches) == 0 {
					continue
				}
				code := reWhitespace.ReplaceAllString(strings.TrimSpace(stripHtmlTags(tdMatches[0][1])), "")
				if code == "" {
					continue
				}

				matched := len(prefixes) == 0
				for _, p := range prefixes {
					pTrim := strings.TrimSpace(p)
					if pTrim != "" && strings.HasPrefix(code, pTrim) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}

				tds := make([]string, 0, len(tdMatches))
				for _, tdM := range tdMatches {
					cText := html.UnescapeString(stripHtmlTags(tdM[1]))
					cText = strings.NewReplacer("&nbsp;", " ", "\xc2\xa0", " ", "\r", " ", "\n", " ", "\t", " ").Replace(cText)
					tds = append(tds, strings.TrimSpace(cText))
				}

				mCode := code
				mName := mCode
				if len(tds) > 1 && tds[1] != "" {
					mName = tds[1]
				}

				rowText := stripHtmlTags(row)
				mQty := 1
				if qm := rePcsQty.FindStringSubmatch(rowText); len(qm) > 1 {
					mQty = int(math.Round(core.AsFloat(qm[1])))
				} else {
					for _, tVal := range tds {
						if qm2 := reNumQty.FindStringSubmatch(tVal); len(qm2) > 1 {
							mQty = int(math.Round(core.AsFloat(qm2[1])))
							break
						}
					}
				}
				if mQty <= 0 {
					mQty = 1
				}

				reqTotal := float64(mQty)
				sendTotal := float64(mQty)
				if ks, ok := kittingSummary[mCode]; ok {
					reqTotal = ks.req
					sendTotal = ks.send
				}

				colorState := "red"
				if sendTotal >= reqTotal && reqTotal > 0 {
					colorState = "green"
				} else if sendTotal > 0 && sendTotal < reqTotal {
					colorState = "orange"
				}

				scanPt := 1
				if cfg, ok := savedConfig[mCode]; ok {
					if sp := core.AsInt(cfg["scan_point"]); sp >= 1 {
						scanPt = sp
					}
				}

				materials = append(materials, map[string]any{
					"code":          mCode,
					"name":          mName,
					"unit_quantity": mQty,
					"scan_point":    scanPt,
					"req_quantity":  reqTotal,
					"send_quantity": sendTotal,
					"color_state":   colorState,
				})
			}
		}

		erpFetchCacheMap.Store(wo, cachedErpFetch{
			Task:      task,
			Materials: materials,
			UpdatedAt: time.Now(),
		})

		service.LogAudit(c, "质量追溯", "拉取ERP齐套数据", true, "", 0, nil)
		core.Success(c, "success", gin.H{
			"is_configured": existingTask != nil,
			"existing_task": existingTask,
			"task":          task,
			"materials":     materials,
			"is_xq":         true,
		})
		return
	}

	// ==================== 分支 B：标准 ERP 生产工单处理 ====================
	if sqlsrvDB == nil {
		core.Error(c, "读取 ERP 工单组件失败，请稍后重试或联系管理员！", gin.H{})
		return
	}

	var sWo, sCode, sDesc, sBatch string
	var sQty float64
	rowErr := sqlsrvDB.QueryRow(`
		SELECT TOP 1
			CAST([任务] AS NVARCHAR(100)) AS task_order,
			CAST([编码] AS NVARCHAR(100)) AS parent_code,
			CAST([描述] AS NVARCHAR(255)) AS description,
			CAST([数量] AS FLOAT) AS quantity,
			CAST(COALESCE([批号], '') AS NVARCHAR(100)) AS batch
		FROM [J_全离散任务]
		WHERE ([任务] = @p1 OR [编码] = @p1) AND [状态] <> N'已取消'
	`, wo).Scan(&sWo, &sCode, &sDesc, &sQty, &sBatch)
	if rowErr != nil {
		rowErr = sqlsrvDB.QueryRow(`
			SELECT TOP 1 [任务], [编码], [描述], [数量], COALESCE([批号], '')
			FROM [J_全离散任务]
			WHERE ([任务] = @p1 OR [编码] = @p1) AND [状态] <> '已取消'
		`, wo).Scan(&sWo, &sCode, &sDesc, &sQty, &sBatch)
	}
	if rowErr != nil || strings.TrimSpace(sWo) == "" {
		core.Error(c, "未在 ERP 中查到该工单信息", gin.H{})
		return
	}

	woNo := strings.TrimSpace(sWo)
	totalQty := int(math.Round(sQty))
	if totalQty <= 0 {
		totalQty = 1
	}
	sCode = strings.TrimSpace(sCode)
	sDesc = strings.TrimSpace(gbkToUtf8IfNeeded([]byte(sDesc)))
	sBatch = strings.TrimSpace(sBatch)
	batchPrefix := sBatch
	if len(batchPrefix) > 16 {
		batchPrefix = batchPrefix[:16]
	}

	task := map[string]any{
		"task_order":   woNo,
		"parent_code":  sCode,
		"description":  sDesc,
		"quantity":     totalQty,
		"batch":        sBatch,
		"batch_prefix": batchPrefix,
		"batch_no":     sBatch,
		"order_type":   "standard",
	}

	if existingTask != nil {
		if core.AsInt(existingTask["quantity"]) != totalQty ||
			strings.TrimSpace(core.AsString(existingTask["batch_prefix"])) != batchPrefix ||
			strings.TrimSpace(core.AsString(existingTask["batch_no"])) != sBatch {
			_, _ = db.Exec("UPDATE trace_tasks SET quantity = ?, batch_prefix = ?, batch_no = ? WHERE task_order = ?", totalQty, batchPrefix, sBatch, woNo)
			existingTask["quantity"] = totalQty
			existingTask["batch_prefix"] = batchPrefix
			existingTask["batch_no"] = sBatch
		}
	}

	rawRows, cols, _, procErr := ctrl.querySqlsrvKittingSummary(sqlsrvDB, woNo)
	if procErr != nil {
		log.Printf("[trace][check_or_fetch_erp] %v", procErr)
		core.Error(c, "读取 ERP 工单组件失败，请稍后重试或联系管理员！", gin.H{})
		return
	}

	colCode := getExactColName(cols, []string{"物料编码", "组件编码", "编码", "子项物料代码"})
	if colCode == "" {
		colCode = "物料编码"
	}
	colName := getExactColName(cols, []string{"物料名称", "组件描述", "物料描述", "描述"})
	if colName == "" {
		colName = "物料名称"
	}
	colProcNo := getExactColName(cols, []string{"工序号", "工序代码"})
	colReqQty := getExactColName(cols, []string{"需求量", "需求数量"})
	colSendQty := getExactColName(cols, []string{"已发料", "已发料数量", "实发数量", "完成量"})

	validPrefixes := map[string]bool{
		"6002": true, "6003": true, "6004": true,
		"7004": true, "7005": true, "7006": true,
	}

	type stdMatAgg struct {
		code    string
		name    string
		reqQty  float64
		sendQty float64
	}
	matMap := make(map[string]*stdMatAgg)
	matOrder := make([]string, 0)

	for _, r := range rawRows {
		procNoVal := ""
		if colProcNo != "" {
			procNoVal = strings.TrimSpace(core.AsString(r[colProcNo]))
		}
		if procNoVal != "" {
			if strings.Contains(procNoVal, "-") || core.AsFloat(procNoVal) < 0 {
				continue
			}
		}

		mCode := strings.TrimSpace(core.AsString(r[colCode]))
		if len(mCode) < 4 || !validPrefixes[mCode[:4]] {
			continue
		}

		mName := ""
		if colName != "" {
			mName = strings.TrimSpace(core.AsString(r[colName]))
		}
		reqStr := "0"
		if colReqQty != "" {
			reqStr = strings.ReplaceAll(strings.TrimSpace(core.AsString(r[colReqQty])), ",", "")
		}
		sendStr := "0"
		if colSendQty != "" {
			sendStr = strings.ReplaceAll(strings.TrimSpace(core.AsString(r[colSendQty])), ",", "")
		}
		reqQty := core.AsFloat(reqStr)
		sendQty := core.AsFloat(sendStr)

		agg, ok := matMap[mCode]
		if !ok {
			agg = &stdMatAgg{code: mCode, name: mName}
			matMap[mCode] = agg
			matOrder = append(matOrder, mCode)
		}
		if agg.name == "" && mName != "" {
			agg.name = mName
		}
		agg.reqQty += reqQty
		agg.sendQty += sendQty
	}

	resultMaterials := make([]map[string]any, 0, len(matOrder))
	for _, mCode := range matOrder {
		m := matMap[mCode]
		reqTotal := m.reqQty
		sendTotal := m.sendQty

		unitQty := reqTotal
		if totalQty > 0 {
			unitQty = math.Round(reqTotal/float64(totalQty)*100) / 100
		}
		if unitQty <= 0 {
			unitQty = 1
		}

		colorState := "red"
		if reqTotal > 0 && sendTotal >= reqTotal {
			colorState = "green"
		} else if sendTotal > 0 && sendTotal < reqTotal {
			colorState = "orange"
		}

		scanPt := 1
		if cfg, ok := savedConfig[mCode]; ok {
			if sp := core.AsInt(cfg["scan_point"]); sp >= 1 {
				scanPt = sp
			}
		}

		dispName := m.name
		if dispName == "" {
			dispName = mCode
		}

		resultMaterials = append(resultMaterials, map[string]any{
			"code":          mCode,
			"name":          dispName,
			"unit_quantity": unitQty,
			"scan_point":    scanPt,
			"req_quantity":  reqTotal,
			"send_quantity": sendTotal,
			"color_state":   colorState,
		})
	}

	erpFetchCacheMap.Store(woNo, cachedErpFetch{
		Task:      task,
		Materials: resultMaterials,
		UpdatedAt: time.Now(),
	})
	if wo != woNo {
		erpFetchCacheMap.Store(wo, cachedErpFetch{
			Task:      task,
			Materials: resultMaterials,
			UpdatedAt: time.Now(),
		})
	}

	service.LogAudit(c, "质量追溯", "拉取ERP齐套数据", true, "", 0, nil)
	core.Success(c, "success", gin.H{
		"is_configured": existingTask != nil,
		"existing_task": existingTask,
		"task":          task,
		"materials":     resultMaterials,
		"is_xq":         false,
	})
}

// SaveConfig POST /api/trace/saveConfig
// 严格对齐 legacy_trace.php save_config 分支
func (ctrl *TraceController) SaveConfig(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存追溯配置失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_add_material"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有添加/编辑追溯物料的权限！", gin.H{})
		return
	}

	in := core.ParseInputMap(c)
	taskOrder := strings.TrimSpace(core.AsString(in["task_order"]))
	parentCode := strings.TrimSpace(core.AsString(in["parent_code"]))
	parentName := strings.TrimSpace(core.AsString(in["parent_name"]))
	if parentName == "" {
		parentName = strings.TrimSpace(core.AsString(in["description"]))
	}
	quantity := core.AsInt(in["quantity"])
	batchNo := strings.TrimSpace(core.AsString(in["batch_no"]))
	scanPoints := core.AsInt(in["scan_points"])
	tasksid := strings.TrimSpace(core.AsString(in["tasksid"]))
	relNo := strings.TrimSpace(core.AsString(in["rel_no"]))

	if taskOrder == "" || parentCode == "" {
		core.Error(c, "参数不完整，请重新查询工单后再保存！", gin.H{})
		return
	}
	if quantity <= 0 {
		quantity = 1
	}
	if scanPoints < 1 {
		scanPoints = 1
	}
	if scanPoints > 5 {
		scanPoints = 5
	}

	var children []map[string]any
	if rawChildren, ok := in["children"]; ok && rawChildren != nil {
		switch v := rawChildren.(type) {
		case string:
			_ = json.Unmarshal([]byte(v), &children)
		default:
			b, _ := json.Marshal(v)
			_ = json.Unmarshal(b, &children)
		}
	}

	// ★ 兼容保障：若旧前端静态包在 saveConfig 时未传 children，自动从刚刚 checkOrFetchErp 缓存的物料列表中补齐
	if len(children) == 0 {
		if v, ok := erpFetchCacheMap.Load(taskOrder); ok {
			cached := v.(cachedErpFetch)
			for _, m := range cached.Materials {
				if core.AsFloat(m["send_quantity"]) > 0 {
					children = append(children, m)
				}
			}
			if tasksid == "" {
				tasksid = strings.TrimSpace(core.AsString(cached.Task["tasksid"]))
			}
			if relNo == "" {
				relNo = strings.TrimSpace(core.AsString(cached.Task["rel_no"]))
			}
		}
	}

	if len(children) == 0 {
		core.Error(c, "请至少勾选一项追溯物料！", gin.H{})
		return
	}

	for _, ch := range children {
		cCode := strings.TrimSpace(core.AsString(ch["code"]))
		if cCode == "" {
			cCode = strings.TrimSpace(core.AsString(ch["child_code"]))
		}
		if cCode == "" {
			core.Error(c, "存在无效的物料行，请重新查询工单！", gin.H{})
			return
		}
		// 若前端传入了 send_quantity 则严格校验实发出库数 > 0
		if _, hasSend := ch["send_quantity"]; hasSend {
			if core.AsFloat(ch["send_quantity"]) <= 0 {
				core.Error(c, fmt.Sprintf("物料【%s】实发出库数为 0，禁止保存追溯配置！", cCode), gin.H{})
				return
			}
		}
	}

	// ★ 业务规则：扫码点数量 > 1 时，1..N 每个扫码点都必须至少挂一种物料。
	//   否则多设的扫码点没有任何物料，扫码时永远扫不到内容，多点扫码就失去意义。
	if scanPoints > 1 {
		covered := make(map[int]bool, scanPoints)
		for _, ch := range children {
			sp := core.AsInt(ch["scan_point"])
			if sp < 1 {
				sp = 1
			}
			if sp > scanPoints {
				sp = scanPoints
			}
			covered[sp] = true
		}
		missing := make([]string, 0, scanPoints)
		for i := 1; i <= scanPoints; i++ {
			if !covered[i] {
				missing = append(missing, strconv.Itoa(i))
			}
		}
		if len(missing) > 0 {
			core.Error(c, fmt.Sprintf("扫码点【%s】下没有任何物料。已设置 %d 个扫码点，请为每个扫码点至少分配一种物料后再保存！",
				strings.Join(missing, "、"), scanPoints), gin.H{
				"code":                "SCAN_POINT_EMPTY",
				"missing_scan_points": missing,
			})
			return
		}
	}

	batchPrefix := strings.TrimSpace(core.AsString(in["batch_prefix"]))
	if v, ok := erpFetchCacheMap.Load(taskOrder); ok {
		cached := v.(cachedErpFetch)
		if batchNo == "" {
			batchNo = strings.TrimSpace(core.AsString(cached.Task["batch_no"]))
		}
		if batchPrefix == "" {
			batchPrefix = strings.TrimSpace(core.AsString(cached.Task["batch_prefix"]))
		}
		if tasksid == "" {
			tasksid = strings.TrimSpace(core.AsString(cached.Task["tasksid"]))
		}
		if relNo == "" {
			relNo = strings.TrimSpace(core.AsString(cached.Task["rel_no"]))
		}
	}

	isXq := strings.HasPrefix(strings.ToUpper(taskOrder), "XQ")
	orderType := "standard"
	if isXq {
		orderType = "xq_project"
	}

	// 若未带 tasksid / rel_no / batch_no / batch_prefix，优先保留数据库已有值或从 ERP / CEM 自动补齐
	existRow, _ := core.QueryRowMap(db, "SELECT id, tasksid, rel_no, batch_no, batch_prefix FROM trace_tasks WHERE task_order = ? LIMIT 1", taskOrder)
	if existRow != nil {
		if tasksid == "" {
			tasksid = strings.TrimSpace(core.AsString(existRow["tasksid"]))
		}
		if relNo == "" {
			relNo = strings.TrimSpace(core.AsString(existRow["rel_no"]))
		}
		if batchNo == "" {
			batchNo = strings.TrimSpace(core.AsString(existRow["batch_no"]))
		}
		if batchPrefix == "" {
			batchPrefix = strings.TrimSpace(core.AsString(existRow["batch_prefix"]))
		}
	}
	if !isXq && (batchNo == "" || len(batchPrefix) < 16) {
		if sqlsrvDB, sErr := core.GetDB("sqlsrv"); sErr == nil && sqlsrvDB != nil {
			var sBatch string
			var sQty float64
			if qErr := sqlsrvDB.QueryRow(`
				SELECT TOP 1 CAST([数量] AS FLOAT), CAST(COALESCE([批号], '') AS NVARCHAR(100))
				FROM [J_全离散任务]
				WHERE ([任务] = @p1 OR [编码] = @p1) AND [状态] <> N'已取消'
				ORDER BY [任务] DESC
			`, taskOrder).Scan(&sQty, &sBatch); qErr == nil {
				sBatch = strings.TrimSpace(sBatch)
				if sBatch != "" {
					batchNo = sBatch
					if len(sBatch) >= 16 {
						batchPrefix = sBatch[:16]
					} else {
						batchPrefix = sBatch
					}
				}
				if sQty > 0 && quantity <= 1 {
					quantity = int(math.Round(sQty))
				}
			}
		}
	}
	if isXq && (tasksid == "" || relNo == "") {
		if rTasksid, rRelNo, rErr := resolveXqCard(taskOrder); rErr == nil {
			if tasksid == "" {
				tasksid = rTasksid
			}
			if relNo == "" {
				relNo = rRelNo
			}
		}
	}

	if batchPrefix == "" && batchNo != "" {
		batchPrefix = batchNo
	}
	if isXq && batchPrefix == "" && len(parentCode) >= 16 && reDigitsOnly.MatchString(parentCode[:16]) {
		batchPrefix = parentCode[:16]
		if batchNo == "" {
			batchNo = parentCode
		}
	}
	if len(batchPrefix) > 16 {
		batchPrefix = batchPrefix[:16]
	}

	tx, err := db.Begin()
	if err != nil {
		core.Error(c, "保存追溯物料配置失败，请稍后重试或联系管理员！", gin.H{})
		return
	}

	if existRow != nil {
		_, err = tx.Exec(`
			UPDATE trace_tasks
			SET parent_code = ?, description = ?, quantity = ?, batch_prefix = ?, batch_no = ?,
			    scan_points = ?, order_type = ?, tasksid = ?, rel_no = ?
			WHERE task_order = ?
		`, parentCode, parentName, quantity, batchPrefix, batchNo, scanPoints, orderType, tasksid, relNo, taskOrder)
	} else {
		_, err = tx.Exec(`
			INSERT INTO trace_tasks (task_order, parent_code, description, quantity, batch_prefix, batch_no, scan_points, order_type, tasksid, rel_no, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, taskOrder, parentCode, parentName, quantity, batchPrefix, batchNo, scanPoints, orderType, tasksid, relNo, sess.UserID)
	}
	if err != nil {
		_ = tx.Rollback()
		log.Printf("[trace][save_config] %v", err)
		core.Error(c, "保存追溯物料配置失败，请稍后重试或联系管理员！", gin.H{})
		return
	}

	if _, err := tx.Exec("DELETE FROM trace_bom_config WHERE task_order = ?", taskOrder); err != nil {
		_ = tx.Rollback()
		core.Error(c, "保存追溯物料配置失败，请稍后重试或联系管理员！", gin.H{})
		return
	}

	for _, ch := range children {
		cCode := strings.TrimSpace(core.AsString(ch["code"]))
		if cCode == "" {
			cCode = strings.TrimSpace(core.AsString(ch["child_code"]))
		}
		cName := strings.TrimSpace(core.AsString(ch["name"]))
		if cName == "" {
			cName = strings.TrimSpace(core.AsString(ch["child_name"]))
		}
		unitQty := core.AsFloat(ch["unit_quantity"])
		if unitQty <= 0 {
			unitQty = 1
		}
		sp := core.AsInt(ch["scan_point"])
		if sp < 1 {
			sp = 1
		}
		if sp > scanPoints {
			sp = scanPoints
		}
		if _, err := tx.Exec(`
			INSERT INTO trace_bom_config (task_order, parent_code, parent_name, child_code, child_name, unit_quantity, scan_point, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, taskOrder, parentCode, parentName, cCode, cName, unitQty, sp, sess.UserID); err != nil {
			_ = tx.Rollback()
			log.Printf("[trace][save_config][bom] %v", err)
			core.Error(c, "保存追溯物料配置失败，请稍后重试或联系管理员！", gin.H{})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		core.Error(c, "保存追溯物料配置失败，请稍后重试或联系管理员！", gin.H{})
		return
	}

	service.LogAudit(c, "质量追溯", "保存追溯物料配置", true, "", 0, nil)
	core.Success(c, "追溯物料配置保存成功")
}

// ensureTaskBatchInfo 确保 trace_tasks 中的 batch_prefix（离散任务批次前16位）、batch_no 与 quantity 完整准确
func (ctrl *TraceController) ensureTaskBatchInfo(db *sql.DB, task map[string]any) {
	if task == nil {
		return
	}
	taskOrder := strings.TrimSpace(core.AsString(task["task_order"]))
	orderType := strings.TrimSpace(core.AsString(task["order_type"]))
	isXq := (orderType == "xq_project" || strings.HasPrefix(strings.ToUpper(taskOrder), "XQ"))

	batchPrefix := strings.TrimSpace(core.AsString(task["batch_prefix"]))
	batchNo := strings.TrimSpace(core.AsString(task["batch_no"]))
	parentCode := strings.TrimSpace(core.AsString(task["parent_code"]))
	qty := core.AsInt(task["quantity"])

	if batchPrefix == "" && len(batchNo) >= 16 {
		batchPrefix = batchNo[:16]
		task["batch_prefix"] = batchPrefix
	}

	if isXq {
		if batchPrefix == "" && len(parentCode) >= 16 && reDigitsOnly.MatchString(parentCode[:16]) {
			batchPrefix = parentCode[:16]
			task["batch_prefix"] = batchPrefix
			if batchNo == "" {
				batchNo = parentCode
				task["batch_no"] = batchNo
			}
			_, _ = db.Exec("UPDATE trace_tasks SET batch_prefix = ?, batch_no = ? WHERE task_order = ?", batchPrefix, batchNo, taskOrder)
		}
		return
	}

	// 标准生产工单：若数据库内缺失 batch_prefix（不足16位）或 batch_no，实时从 SQL Server [J_全离散任务] 同步并回写
	if len(batchPrefix) < 16 || batchNo == "" || qty <= 0 {
		if sqlsrvDB, sErr := core.GetDB("sqlsrv"); sErr == nil && sqlsrvDB != nil {
			var sBatch string
			var sQty float64
			err := sqlsrvDB.QueryRow(`
				SELECT TOP 1 CAST([数量] AS FLOAT), CAST(COALESCE([批号], '') AS NVARCHAR(100))
				FROM [J_全离散任务]
				WHERE ([任务] = @p1 OR [编码] = @p1) AND [状态] <> N'已取消'
				ORDER BY [任务] DESC
			`, taskOrder).Scan(&sQty, &sBatch)
			if err != nil {
				_ = sqlsrvDB.QueryRow(`
					SELECT TOP 1 [数量], COALESCE([批号], '')
					FROM [J_全离散任务]
					WHERE ([任务] = @p1 OR [编码] = @p1) AND [状态] <> '已取消'
					ORDER BY [任务] DESC
				`, taskOrder).Scan(&sQty, &sBatch)
			}
			sBatch = strings.TrimSpace(sBatch)
			newQty := int(math.Round(sQty))
			if newQty <= 0 {
				newQty = qty
			}
			if newQty <= 0 {
				newQty = 1
			}
			if sBatch != "" {
				bp := sBatch
				if len(bp) > 16 {
					bp = bp[:16]
				}
				task["batch_no"] = sBatch
				task["batch_prefix"] = bp
				task["quantity"] = newQty
				_, _ = db.Exec("UPDATE trace_tasks SET batch_prefix = ?, batch_no = ?, quantity = ? WHERE task_order = ?", bp, sBatch, newQty, taskOrder)
			}
		}
	}
}

// StartTrace GET /api/trace/startTrace
func (ctrl *TraceController) StartTrace(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "启动追溯失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_scan"] == 0 && perms["can_add_material"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有质量追溯的权限！", gin.H{})
		return
	}

	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	if wo == "" {
		core.Error(c, "请输入工单号！", gin.H{})
		return
	}
	task, _ := core.QueryRowMap(db, "SELECT * FROM trace_tasks WHERE task_order = ? LIMIT 1", wo)
	if task == nil {
		core.Error(c, "该单号尚未配置追溯物料，请先在【添加/编辑追溯物料】中配置！", gin.H{})
		return
	}
	ctrl.ensureTaskBatchInfo(db, task)

	rawBoms, _ := core.QueryMaps(db, "SELECT child_code, child_name, unit_quantity, scan_point FROM trace_bom_config WHERE task_order = ? ORDER BY scan_point ASC, id ASC", wo)
	materials := make([]map[string]any, 0, len(rawBoms))
	for _, r := range rawBoms {
		materials = append(materials, map[string]any{
			"child_code":    core.AsString(r["child_code"]),
			"child_name":    core.AsString(r["child_name"]),
			"unit_quantity": core.AsFloat(r["unit_quantity"]),
			"scan_point":    core.AsInt(r["scan_point"]),
		})
	}
	var completedBoxes int
	_ = db.QueryRow("SELECT COUNT(*) FROM trace_boxes WHERE task_order = ? AND status = 1", wo).Scan(&completedBoxes)

	service.LogAudit(c, "质量追溯", "启动追溯任务", true, "", 0, nil)
	core.Success(c, "success", gin.H{
		"task":            task,
		"materials":       materials,
		"completed_boxes": completedBoxes,
	})
}

func (ctrl *TraceController) boxPointScanned(db *sql.DB, boxID, scanPoint int) map[string]int {
	rows, _ := core.QueryMaps(db, "SELECT child_code, COUNT(*) AS cnt FROM trace_scans WHERE box_id = ? AND scan_point = ? GROUP BY child_code", boxID, scanPoint)
	out := make(map[string]int)
	for _, r := range rows {
		out[core.AsString(r["child_code"])] = core.AsInt(r["cnt"])
	}
	return out
}

func (ctrl *TraceController) isBoxFullyCompleted(db *sql.DB, taskOrder string, boxID int) bool {
	boms, _ := core.QueryMaps(db, "SELECT child_code, unit_quantity, scan_point FROM trace_bom_config WHERE task_order = ?", taskOrder)
	if len(boms) == 0 {
		return false
	}
	need := make(map[string]float64)
	for _, r := range boms {
		k := fmt.Sprintf("%d|%s", core.AsInt(r["scan_point"]), core.AsString(r["child_code"]))
		need[k] = core.AsFloat(r["unit_quantity"])
	}
	scans, _ := core.QueryMaps(db, "SELECT scan_point, child_code, COUNT(*) AS cnt FROM trace_scans WHERE box_id = ? GROUP BY scan_point, child_code", boxID)
	got := make(map[string]int)
	for _, r := range scans {
		k := fmt.Sprintf("%d|%s", core.AsInt(r["scan_point"]), core.AsString(r["child_code"]))
		got[k] = core.AsInt(r["cnt"])
	}
	for k, qty := range need {
		if qty <= 0 {
			qty = 1
		}
		if float64(got[k]) < qty {
			return false
		}
	}
	return true
}

func (ctrl *TraceController) refreshBoxStatus(db *sql.DB, taskOrder string, boxID int) {
	if boxID <= 0 {
		return
	}
	if ctrl.isBoxFullyCompleted(db, taskOrder, boxID) {
		_, _ = db.Exec("UPDATE trace_boxes SET status = 1, completed_at = IFNULL(completed_at, NOW()) WHERE id = ?", boxID)
	} else {
		_, _ = db.Exec("UPDATE trace_boxes SET status = 0, completed_at = NULL WHERE id = ?", boxID)
	}
}

// ScanBarcode POST /api/trace/scanBarcode
// 严格对标 trace.php 真实业务判据（含一级物料 22 位/批次前16位/后6位流水上限/工位防呆 + 二级物料 22 位/前10位编码/年月规则）
func (ctrl *TraceController) ScanBarcode(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "扫码失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_scan"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有追溯扫码的操作权限！", gin.H{})
		return
	}

	in := core.ParseInputMap(c)
	taskOrder := strings.TrimSpace(core.AsString(in["task_order"]))
	sn := strings.TrimSpace(core.AsString(in["sn"]))
	boxID := core.AsInt(in["box_id"])
	curPoint := core.AsInt(in["scan_point"])
	if curPoint < 1 {
		curPoint = 1
	}
	if curPoint > 5 {
		curPoint = 5
	}

	if taskOrder == "" || sn == "" {
		core.Error(c, "参数不完整", gin.H{})
		return
	}

	task, _ := core.QueryRowMap(db, "SELECT * FROM trace_tasks WHERE task_order = ? LIMIT 1", taskOrder)
	if task == nil {
		core.Error(c, "工单不存在", gin.H{})
		return
	}

	totalPlanQty := core.AsInt(task["quantity"])
	if totalPlanQty <= 0 {
		totalPlanQty = 1
	}

	var compCountNow int
	_ = db.QueryRow("SELECT COUNT(*) FROM trace_boxes WHERE task_order = ? AND status = 1", taskOrder).Scan(&compCountNow)
	if compCountNow >= totalPlanQty {
		core.Error(c, fmt.Sprintf("该单据【%s】已全部完成追溯！", taskOrder), gin.H{})
		return
	}

	orderType := strings.TrimSpace(core.AsString(task["order_type"]))
	isXqOrder := (orderType == "xq_project" || strings.HasPrefix(strings.ToUpper(taskOrder), "XQ"))

	// 查询当前工单已配置的全部二级物料清单
	allConfiguredMaterials, _ := core.QueryMaps(db, "SELECT child_code, child_name, unit_quantity, scan_point FROM trace_bom_config WHERE task_order = ?", taskOrder)

	var matchedChildConfig map[string]any
	for _, cfgItem := range allConfiguredMaterials {
		cCode := strings.TrimSpace(core.AsString(cfgItem["child_code"]))
		if cCode != "" && strings.HasPrefix(sn, cCode) {
			matchedChildConfig = cfgItem
			break
		}
	}

	firstChar := ""
	if len(sn) > 0 {
		firstChar = sn[:1]
	}

	isPrimarySn := false
	if matchedChildConfig == nil {
		if isXqOrder {
			isPrimarySn = true
		} else if firstChar == "1" {
			isPrimarySn = true
		} else if len(sn) == 22 && reDigitsOnly.MatchString(sn) {
			// ★★★ 关键修复（对标 trace.php 第 902-916 行）★★★
			// 只有当"已扫过一级条码（boxId > 0）"即正在扫二级物料时，
			// 才把与工单批号前缀不匹配的 22 位条码判定为"无效二级条码"。
			// 若 boxId == 0，说明当前正处于等待一级条码的阶段，
			// 此时不匹配批号前缀的 22 位条码应作为一级条码继续走一级校验，
			// 由一级校验给出准确的"一级条码拦截"提示。
			taskBatchNoChk := strings.TrimSpace(core.AsString(task["batch_no"]))
			if boxID > 0 && taskBatchNoChk != "" && len(taskBatchNoChk) >= 16 {
				if len(sn) >= 16 && sn[:16] != taskBatchNoChk[:16] {
					core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
					return
				}
			}
			isPrimarySn = true
		}
	}

	// ==================== 阶段一：一级物料条码校验与登记 ====================
	if isPrimarySn {
		// ★★★ 纯数字工单一级物料范围校验（含实时从 ERP [J_全离散任务] 兜底查询） ★★★
		if !isXqOrder {
			taskBatchNo := strings.TrimSpace(core.AsString(task["batch_no"]))

			// 兜底：数据库里没存过批号，实时从 ERP 查一次并回填（处理老工单）
			if (taskBatchNo == "" || len(taskBatchNo) < 16) && reDigitsOnly.MatchString(taskOrder) {
				if sqlsrvDB, sErr := core.GetDB("sqlsrv"); sErr == nil && sqlsrvDB != nil {
					var sBatch string
					var sQty float64
					err := sqlsrvDB.QueryRow(`
						SELECT TOP 1 CAST([数量] AS FLOAT), CAST(COALESCE([批号], '') AS NVARCHAR(100))
						FROM [J_全离散任务]
						WHERE ([任务] = @p1 OR [编码] = @p1) AND [状态] <> N'已取消'
						ORDER BY [任务] DESC
					`, taskOrder).Scan(&sQty, &sBatch)
					if err != nil {
						_ = sqlsrvDB.QueryRow(`
							SELECT TOP 1 [数量], COALESCE([批号], '')
							FROM [J_全离散任务]
							WHERE ([任务] = @p1 OR [编码] = @p1) AND [状态] <> '已取消'
							ORDER BY [任务] DESC
						`, taskOrder).Scan(&sQty, &sBatch)
					}
					sBatch = strings.TrimSpace(sBatch)
					if sBatch != "" {
						taskBatchNo = sBatch
						bp := sBatch
						if len(bp) > 16 {
							bp = bp[:16]
						}
						qVal := int(math.Round(sQty))
						if qVal > 0 {
							totalPlanQty = qVal
						}
						_, _ = db.Exec("UPDATE trace_tasks SET batch_no = ?, batch_prefix = ?, quantity = ? WHERE task_order = ?", sBatch, bp, totalPlanQty, taskOrder)
						task["batch_no"] = sBatch
						task["batch_prefix"] = bp
						task["quantity"] = totalPlanQty
					}
				}
			}

			if taskBatchNo != "" {
				taskBatch16 := taskBatchNo
				if len(taskBatch16) > 16 {
					taskBatch16 = taskBatch16[:16]
				}
				snPrefix16 := ""
				if len(sn) >= 16 {
					snPrefix16 = sn[:16]
				}

				// ★★★ 一级物料错误信息不再暴露前 16 位详情，只报"物料不符"（对标 trace.php 第 938-941 行） ★★★
				if len(sn) != 22 || snPrefix16 != taskBatch16 {
					core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
					return
				}

				seqPart := sn[16:22]
				if !reDigitsOnly.MatchString(seqPart) {
					core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
					return
				}
				seqNum, sErr := strconv.Atoi(seqPart)
				if sErr != nil || seqNum < 1 || seqNum > totalPlanQty {
					core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
					return
				}
			}
		}

		if isXqOrder {
			expectedParentCode := strings.TrimSpace(core.AsString(task["parent_code"]))
			if expectedParentCode != "" && sn != expectedParentCode {
				core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工程发货单不符，禁止扫码！", sn), gin.H{})
				return
			}
		}

		// ★★★ 工位防呆拦截（对标 trace.php 第 960-1018 行） ★★★
		unDoneBoxes, _ := core.QueryMaps(db, "SELECT id, box_sn FROM trace_boxes WHERE task_order = ? AND status = 0 ORDER BY id ASC", taskOrder)
		curPointMats, _ := core.QueryMaps(db, "SELECT child_code, child_name, unit_quantity FROM trace_bom_config WHERE task_order = ? AND scan_point = ?", taskOrder, curPoint)

		var pendingUnfinishedBox map[string]any
		missingMaterialName := ""
		missingNeed := 0.0
		missingDone := 0

	boxLoop:
		for _, ubox := range unDoneBoxes {
			uBoxID := core.AsInt(ubox["id"])
			var totalScannedInThisPoint int
			_ = db.QueryRow("SELECT COUNT(*) FROM trace_scans WHERE box_id = ? AND scan_point = ?", uBoxID, curPoint).Scan(&totalScannedInThisPoint)

			if totalScannedInThisPoint <= 0 {
				continue
			}

			if curPoint > 1 {
				prevPoint := curPoint - 1
				prevMats, _ := core.QueryMaps(db, "SELECT child_code, unit_quantity FROM trace_bom_config WHERE task_order = ? AND scan_point = ?", taskOrder, prevPoint)
				prevAllDone := true
				for _, pm := range prevMats {
					var pCnt int
					_ = db.QueryRow("SELECT COUNT(*) FROM trace_scans WHERE box_id = ? AND child_code = ?", uBoxID, pm["child_code"]).Scan(&pCnt)
					if float64(pCnt) < core.AsFloat(pm["unit_quantity"]) {
						prevAllDone = false
						break
					}
				}
				if !prevAllDone {
					continue
				}
			}

			for _, m := range curPointMats {
				var doneQty int
				_ = db.QueryRow("SELECT COUNT(*) FROM trace_scans WHERE box_id = ? AND child_code = ?", uBoxID, m["child_code"]).Scan(&doneQty)
				needQty := core.AsFloat(m["unit_quantity"])
				if float64(doneQty) < needQty {
					pendingUnfinishedBox = ubox
					missingMaterialName = core.AsString(m["child_name"])
					missingNeed = needQty
					missingDone = doneQty
					break boxLoop
				}
			}
		}

		if pendingUnfinishedBox != nil && core.AsString(pendingUnfinishedBox["box_sn"]) != sn {
			core.Error(c, fmt.Sprintf("【工位防呆拦截】当前单据在【扫码点 %d】存在未扫齐的装置【%s】！\n未齐套物料：【%s】(需 %v / 已扫 %d)",
				curPoint, core.AsString(pendingUnfinishedBox["box_sn"]), missingMaterialName, missingNeed, missingDone), gin.H{})
			return
		}

		tx, txErr := db.Begin()
		if txErr != nil {
			core.Error(c, "系统数据库异常", gin.H{})
			return
		}

		var finalBoxID int
		var boxRow map[string]any
		row := tx.QueryRow("SELECT id, box_sn, parent_code, status FROM trace_boxes WHERE box_sn = ? LIMIT 1", sn)
		var bID, bStatus int
		var bSn, bParent string
		if qErr := row.Scan(&bID, &bSn, &bParent, &bStatus); qErr == nil {
			if bStatus == 1 {
				_ = tx.Rollback()
				core.Error(c, fmt.Sprintf("该一级物料/装置【%s】此前已追溯完毕！", sn), gin.H{})
				return
			}
			finalBoxID = bID
			boxRow = map[string]any{"id": bID, "box_sn": bSn, "status": bStatus}
		} else {
			res, insErr := tx.Exec("INSERT INTO trace_boxes (task_order, box_sn, parent_code, status, created_by) VALUES (?, ?, ?, 0, ?)",
				taskOrder, sn, core.AsString(task["parent_code"]), sess.UserID)
			if insErr != nil {
				_ = tx.Rollback()
				core.Error(c, "该一级装置条码已被其他工单占用，请核对！", gin.H{})
				return
			}
			newID, _ := res.LastInsertId()
			finalBoxID = int(newID)
			boxRow = map[string]any{"id": finalBoxID, "box_sn": sn, "status": 0}
		}
		_ = tx.Commit()

		scannedMap := ctrl.boxPointScanned(db, finalBoxID, curPoint)
		var compCount int
		_ = db.QueryRow("SELECT COUNT(*) FROM trace_boxes WHERE task_order = ? AND status = 1", taskOrder).Scan(&compCount)

		service.LogAudit(c, "质量追溯", "扫码登记一级条码", true, "", 0, nil)
		core.Success(c, fmt.Sprintf("一级条码【%s】载入成功，请开始扫描二级物料条码 (SN)", sn), gin.H{
			"is_box":              true,
			"box_id":              finalBoxID,
			"box_sn":              sn,
			"box_scanned":         scannedMap,
			"completed_boxes":     compCount,
			"cur_point_completed": false,
			"task_all_completed":  false,
		})
		_ = boxRow
		return
	}

	// ==================== 阶段二：二级物料条码校验与录入 ====================
	if boxID <= 0 {
		core.Error(c, "请先扫描（一级物料/装置条码）！", gin.H{})
		return
	}

	if matchedChildConfig == nil {
		if len(sn) >= 10 {
			childCode10 := sn[:10]
			matchedChildConfig, _ = core.QueryRowMap(db, "SELECT * FROM trace_bom_config WHERE task_order = ? AND child_code = ? LIMIT 1", taskOrder, childCode10)
		}
	}

	if matchedChildConfig == nil {
		core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
		return
	}

	// ★★★ 二级物料条码格式强校验（规则不外露，统一报"物料不符"——对标 trace.php 第 1062-1089 行） ★★★
	// 内部规则：必须 22 位纯数字；前 10 位与添加物料编码一致；
	//          11-12 位 ∈ [20,50]；13-14 位 ∈ [01,12]；15-16 位 ≠ 00
	if len(sn) != 22 || !reDigitsOnly.MatchString(sn) {
		core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
		return
	}

	childCodeForCheck := strings.TrimSpace(core.AsString(matchedChildConfig["child_code"]))
	if len(childCodeForCheck) >= 10 {
		if sn[:10] != childCodeForCheck[:10] {
			core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
			return
		}
	}

	seg11_12 := sn[10:12]
	val11_12, err1 := strconv.Atoi(seg11_12)
	if err1 != nil || val11_12 < 20 || val11_12 > 50 {
		core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
		return
	}

	seg13_14 := sn[12:14]
	val13_14, err2 := strconv.Atoi(seg13_14)
	if err2 != nil || val13_14 < 1 || val13_14 > 12 {
		core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
		return
	}

	if sn[14:16] == "00" {
		core.Error(c, fmt.Sprintf("【物料校验拦截】条码【%s】的物料与本工单不符，禁止扫码！", sn), gin.H{})
		return
	}

	childCode := core.AsString(matchedChildConfig["child_code"])
	matPoint := core.AsInt(matchedChildConfig["scan_point"])
	reqUnitQty := core.AsFloat(matchedChildConfig["unit_quantity"])

	if matPoint != curPoint {
		core.Error(c, fmt.Sprintf("物料【%s】属于【扫码点 %d】，当前为【扫码点 %d】！",
			core.AsString(matchedChildConfig["child_name"]), matPoint, curPoint), gin.H{})
		return
	}

	var curCnt int
	_ = db.QueryRow("SELECT COUNT(*) FROM trace_scans WHERE box_id = ? AND child_code = ?", boxID, childCode).Scan(&curCnt)
	if float64(curCnt) >= reqUnitQty {
		core.Error(c, fmt.Sprintf("物料【%s】已扫齐 (需 %v 个)！", core.AsString(matchedChildConfig["child_name"]), reqUnitQty), gin.H{})
		return
	}

	var dupID int
	if err := db.QueryRow("SELECT id FROM trace_scans WHERE child_sn = ? LIMIT 1", sn).Scan(&dupID); err == nil && dupID > 0 {
		core.Error(c, fmt.Sprintf("条码 SN【%s】此前已被使用过！", sn), gin.H{})
		return
	}

	tx, txErr := db.Begin()
	if txErr != nil {
		core.Error(c, "数据库事务异常", gin.H{})
		return
	}
	_, insErr := tx.Exec("INSERT INTO trace_scans (task_order, box_id, scan_point, child_code, child_sn, scanned_by) VALUES (?, ?, ?, ?, ?, ?)",
		taskOrder, boxID, curPoint, childCode, sn, sess.UserID)
	if insErr != nil {
		_ = tx.Rollback()
		core.Error(c, fmt.Sprintf("条码 SN【%s】此前已被使用过！", sn), gin.H{})
		return
	}

	curPointMats, _ := core.QueryMaps(db, "SELECT child_code, unit_quantity FROM trace_bom_config WHERE task_order = ? AND scan_point = ?", taskOrder, curPoint)
	curPointCompleted := true
	for _, m := range curPointMats {
		var cnt int
		_ = tx.QueryRow("SELECT COUNT(*) FROM trace_scans WHERE box_id = ? AND child_code = ?", boxID, m["child_code"]).Scan(&cnt)
		if float64(cnt) < core.AsFloat(m["unit_quantity"]) {
			curPointCompleted = false
			break
		}
	}

	allMats, _ := core.QueryMaps(db, "SELECT child_code, unit_quantity FROM trace_bom_config WHERE task_order = ?", taskOrder)
	boxAllCompleted := true
	for _, m := range allMats {
		var cnt int
		_ = tx.QueryRow("SELECT COUNT(*) FROM trace_scans WHERE box_id = ? AND child_code = ?", boxID, m["child_code"]).Scan(&cnt)
		if float64(cnt) < core.AsFloat(m["unit_quantity"]) {
			boxAllCompleted = false
			break
		}
	}

	if boxAllCompleted {
		_, _ = tx.Exec("UPDATE trace_boxes SET status = 1, completed_at = NOW() WHERE id = ?", boxID)
	}
	_ = tx.Commit()

	scannedMap := ctrl.boxPointScanned(db, boxID, curPoint)
	var compCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM trace_boxes WHERE task_order = ? AND status = 1", taskOrder).Scan(&compCount)

	taskAllCompleted := (boxAllCompleted && compCount >= totalPlanQty)

	service.LogAudit(c, "质量追溯", "扫码录入二级物料", true, "", 0, nil)
	core.Success(c, fmt.Sprintf("二级物料【%s】扫码成功", core.AsString(matchedChildConfig["child_name"])), gin.H{
		"is_box":              false,
		"box_id":              boxID,
		"cur_point_completed": curPointCompleted,
		"box_all_completed":   boxAllCompleted,
		"task_all_completed":  taskAllCompleted,
		"completed_boxes":     compCount,
		"total_quantity":      totalPlanQty,
		"box_scanned":         scannedMap,
	})
}

// QueryRecords GET /api/trace/queryRecords
// 严格对齐 TraceController.php queryRecords
func (ctrl *TraceController) QueryRecords(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询追溯记录异常", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_view"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有查看追溯记录的权限！", gin.H{}, http.StatusForbidden)
		return
	}

	in := core.ParseInputMap(c)
	search := strings.TrimSpace(core.AsString(in["search"]))
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 30
	offset := (page - 1) * limit

	where := "1=1"
	args := []any{}
	if search != "" {
		where = "(b.task_order LIKE ? OR b.box_sn LIKE ? OR s.child_code LIKE ? OR s.child_sn LIKE ?)"
		k := "%" + search + "%"
		args = append(args, k, k, k, k)
	}

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM trace_boxes b INNER JOIN trace_scans s ON b.id = s.box_id WHERE "+where, args...).Scan(&total)

	rows, _ := core.QueryMaps(db, fmt.Sprintf(`
		SELECT b.task_order, b.box_sn, b.parent_code, s.id AS scan_id, s.scan_point,
		       s.child_code, s.child_sn, s.scanned_at, s.scanned_by,
		       u.badge_no AS scanner_emp_no,
		       COALESCE(NULLIF(u.full_name, ''), u.username, CAST(s.scanned_by AS CHAR)) AS scanner_name,
		       u.username AS scanner_user
		FROM trace_boxes b
		INNER JOIN trace_scans s ON b.id = s.box_id
		LEFT JOIN users u ON u.id = s.scanned_by
		WHERE %s
		ORDER BY s.id DESC
		LIMIT ? OFFSET ?
	`, where), append(append([]any{}, args...), limit, offset)...)

	userByBadge, userById, specialMap := ctrl.buildUserNameDictionaries(db)
	for _, r := range rows {
		rawScanned := strings.TrimSpace(core.AsString(r["scanned_by"]))
		resolved := ctrl.resolveUserRealName(rawScanned, userByBadge, userById, specialMap)
		if resolved != "" && resolved != rawScanned {
			r["scanner_name"] = resolved
		} else if core.AsString(r["scanner_name"]) == "" {
			r["scanner_name"] = resolved
		}
	}

	core.Success(c, "success", gin.H{
		"records":     rows,
		"list":        rows,
		"total":       total,
		"total_pages": core.CalcTotalPages(total, limit),
		"page":        page,
	})
}

// UpdateScanRecord POST /api/trace/updateScanRecord
// 严格对齐 legacy_trace.php update_scan_record
func (ctrl *TraceController) UpdateScanRecord(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "修改失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_edit_record"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有修改追溯记录的权限！", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	scanID := core.AsInt(in["scan_id"])
	childCode := strings.TrimSpace(core.AsString(in["child_code"]))
	childSn := strings.TrimSpace(core.AsString(in["child_sn"]))
	scanPoint := core.AsInt(in["scan_point"])
	boxSn := strings.TrimSpace(core.AsString(in["box_sn"]))

	if scanID <= 0 {
		core.Error(c, "记录不存在或已被删除！", gin.H{})
		return
	}
	if childSn == "" {
		core.Error(c, "二级物料条码不能为空！", gin.H{})
		return
	}
	if scanPoint < 1 {
		scanPoint = 1
	}

	rec, _ := core.QueryRowMap(db, "SELECT * FROM trace_scans WHERE id = ? LIMIT 1", scanID)
	if rec == nil {
		core.Error(c, "记录不存在或已被删除！", gin.H{})
		return
	}

	boxID := core.AsInt(rec["box_id"])
	taskOrder := core.AsString(rec["task_order"])
	if boxSn != "" {
		var foundBoxID int
		err := db.QueryRow("SELECT id FROM trace_boxes WHERE task_order = ? AND box_sn = ? LIMIT 1", taskOrder, boxSn).Scan(&foundBoxID)
		if err == sql.ErrNoRows {
			_, _ = db.Exec("UPDATE trace_boxes SET box_sn = ? WHERE id = ?", boxSn, boxID)
		} else if err == nil && foundBoxID != boxID {
			boxID = foundBoxID
		}
	}

	var dupID int
	if err := db.QueryRow("SELECT id FROM trace_scans WHERE child_sn = ? AND id <> ? LIMIT 1", childSn, scanID).Scan(&dupID); err == nil && dupID > 0 {
		core.Error(c, fmt.Sprintf("条码【%s】已存在于其他记录中！", childSn), gin.H{})
		return
	}

	if childCode == "" {
		childCode = core.AsString(rec["child_code"])
	}

	_, err = db.Exec("UPDATE trace_scans SET box_id = ?, child_code = ?, child_sn = ?, scan_point = ? WHERE id = ?",
		boxID, childCode, childSn, scanPoint, scanID)
	if err != nil {
		core.Error(c, "保存失败：条码可能与已有记录冲突！", gin.H{})
		return
	}

	ctrl.refreshBoxStatus(db, taskOrder, boxID)
	oldBoxID := core.AsInt(rec["box_id"])
	if oldBoxID != boxID {
		ctrl.refreshBoxStatus(db, taskOrder, oldBoxID)
	}

	service.LogAudit(c, "质量追溯", "修改扫码记录", true, "", 0, nil)
	core.Success(c, "修改记录成功")
}

// DeleteScanRecord POST /api/trace/deleteScanRecord
// 严格对齐 legacy_trace.php delete_scan_record
func (ctrl *TraceController) DeleteScanRecord(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_delete_record"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有删除追溯记录的权限！", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	scanID := core.AsInt(in["scan_id"])
	childSn := strings.TrimSpace(core.AsString(in["child_sn"]))
	if scanID <= 0 && childSn == "" {
		core.Error(c, "删除参数不完整！", gin.H{})
		return
	}

	var rec map[string]any
	if scanID > 0 {
		rec, _ = core.QueryRowMap(db, "SELECT * FROM trace_scans WHERE id = ? LIMIT 1", scanID)
	} else {
		rec, _ = core.QueryRowMap(db, "SELECT * FROM trace_scans WHERE child_sn = ? LIMIT 1", childSn)
	}
	if rec == nil {
		core.Error(c, "记录不存在或已被删除！", gin.H{})
		return
	}

	_, _ = db.Exec("DELETE FROM trace_scans WHERE id = ?", rec["id"])
	ctrl.refreshBoxStatus(db, core.AsString(rec["task_order"]), core.AsInt(rec["box_id"]))

	service.LogAudit(c, "质量追溯", "删除扫码记录", true, "", 0, nil)
	core.Success(c, "删除成功")
}

// ListConfigs GET /api/trace/listConfigs
// 严格对齐 TraceController.php listConfigs
func (ctrl *TraceController) ListConfigs(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取单据配置库异常", gin.H{})
		return
	}
	page := core.AsInt(c.Query("page"))
	if page < 1 {
		page = 1
	}
	limit := 15
	offset := (page - 1) * limit

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM trace_tasks").Scan(&total)

	userByBadge, userById, specialMap := ctrl.buildUserNameDictionaries(db)

	list, _ := core.QueryMaps(db, `
		SELECT t.*,
		       (SELECT COUNT(*) FROM trace_boxes WHERE task_order = t.task_order AND status = 1) AS completed_count
		FROM trace_tasks t
		ORDER BY t.id DESC
		LIMIT ? OFFSET ?
	`, limit, offset)

	for _, item := range list {
		rawCreated := strings.TrimSpace(core.AsString(item["created_by"]))
		realName := ctrl.resolveUserRealName(rawCreated, userByBadge, userById, specialMap)
		item["creator_name"] = realName
		item["created_by_name"] = realName
		item["creator"] = realName
		item["created_by_raw"] = rawCreated
		item["created_by"] = realName
		item["completed_boxes"] = core.AsInt(item["completed_count"])
	}

	core.Success(c, "success", gin.H{
		"list":        list,
		"tasks":       list,
		"configs":     list,
		"total":       total,
		"total_pages": core.CalcTotalPages(total, limit),
		"page":        page,
	})
}

// ListPermissions GET /api/trace/listPermissions
// 严格对齐 TraceController.php listPermissions
func (ctrl *TraceController) ListPermissions(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取权限列表异常", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["is_admin"] == 0 {
		core.Error(c, "只有管理员可以管理权限！", gin.H{}, http.StatusForbidden)
		return
	}

	rows, err := core.QueryMaps(db, `
		SELECT u.id, u.id AS user_id, u.username, u.full_name, u.badge_no, u.role, u.is_admin,
		       COALESCE(p.can_add_material, 0) AS can_add_material,
		       COALESCE(p.can_scan, 1) AS can_scan,
		       COALESCE(p.can_view, 1) AS can_view,
		       COALESCE(p.can_edit_record, 0) AS can_edit_record,
		       COALESCE(p.can_delete_record, 0) AS can_delete_record
		FROM users u
		LEFT JOIN trace_permissions p ON u.id = p.user_id
		WHERE u.is_active = 1
		ORDER BY u.is_admin DESC, u.id ASC
	`)
	if err != nil {
		core.Exception(c, err, "获取权限列表异常", gin.H{})
		return
	}

	for _, item := range rows {
		item["id"] = core.AsInt(item["id"])
		item["user_id"] = core.AsInt(item["user_id"])
		item["can_add_material"] = core.AsInt(item["can_add_material"])
		item["can_scan"] = core.AsInt(item["can_scan"])
		item["can_view"] = core.AsInt(item["can_view"])
		item["can_edit_record"] = core.AsInt(item["can_edit_record"])
		item["can_delete_record"] = core.AsInt(item["can_delete_record"])
		item["is_admin"] = core.AsInt(item["is_admin"])
	}

	core.Success(c, "success", gin.H{
		"list":        rows,
		"users":       rows,
		"permissions": rows,
	})
}

// SavePermissions POST /api/trace/savePermissions
// 同时兼容前端 Index.vue 批量提交 { permissions: [...] } 与单条提交 { user_id, ... }
func (ctrl *TraceController) SavePermissions(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存权限异常", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["is_admin"] == 0 {
		core.Error(c, "只有管理员可以修改权限！", gin.H{}, http.StatusForbidden)
		return
	}

	in := core.ParseInputMap(c)
	upsertPerm := func(m map[string]any) {
		uid := core.AsInt(m["user_id"])
		if uid <= 0 {
			uid = core.AsInt(m["id"])
		}
		if uid <= 0 {
			return
		}
		canAdd := 0
		if core.AsInt(m["can_add_material"]) != 0 {
			canAdd = 1
		}
		canScan := 0
		if core.AsInt(m["can_scan"]) != 0 {
			canScan = 1
		}
		canView := 0
		if core.AsInt(m["can_view"]) != 0 {
			canView = 1
		}
		canEdit := 0
		if core.AsInt(m["can_edit_record"]) != 0 {
			canEdit = 1
		}
		canDel := 0
		if core.AsInt(m["can_delete_record"]) != 0 {
			canDel = 1
		}
		_, _ = db.Exec(`
			INSERT INTO trace_permissions (user_id, can_add_material, can_scan, can_view, can_edit_record, can_delete_record, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, NOW())
			ON DUPLICATE KEY UPDATE
				can_add_material = VALUES(can_add_material),
				can_scan = VALUES(can_scan),
				can_view = VALUES(can_view),
				can_edit_record = VALUES(can_edit_record),
				can_delete_record = VALUES(can_delete_record),
				updated_at = NOW()
		`, uid, canAdd, canScan, canView, canEdit, canDel)
	}

	if rawList, ok := in["permissions"]; ok && rawList != nil {
		var list []map[string]any
		b, _ := json.Marshal(rawList)
		_ = json.Unmarshal(b, &list)
		for _, item := range list {
			upsertPerm(item)
		}
	} else if core.AsInt(in["user_id"]) > 0 {
		upsertPerm(in)
	} else {
		core.Error(c, "无效的用户ID", gin.H{})
		return
	}

	service.LogAudit(c, "质量追溯", "保存追溯权限", true, "", 0, nil)
	core.Success(c, "权限保存成功")
}

// ============================================================================
//  CEM 9列标准 Excel 生成、推送与导出（对标 CemPushService.php + legacy_trace.php）
// ============================================================================

func writeCemPushLog(msg string) {
	_ = os.MkdirAll("storage/logs", 0755)
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
	f, err := os.OpenFile("storage/logs/cem_push.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
}

func generateCemXlsx(sheetTitle string, headers []string, rows [][]string) ([]byte, error) {
	if sheetTitle == "" {
		sheetTitle = "Sheet1"
	}
	var sheetBuf bytes.Buffer
	sheetBuf.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sheetBuf.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)

	sheetBuf.WriteString(`<row r="1">`)
	for cIdx, h := range headers {
		colLetter := string(rune('A' + cIdx))
		escaped := html.EscapeString(h)
		sheetBuf.WriteString(fmt.Sprintf(`<c r="%s1" t="inlineStr"><is><t>%s</t></is></c>`, colLetter, escaped))
	}
	sheetBuf.WriteString(`</row>`)

	for rIdx, row := range rows {
		rowNum := rIdx + 2
		sheetBuf.WriteString(fmt.Sprintf(`<row r="%d">`, rowNum))
		for cIdx, val := range row {
			colLetter := string(rune('A' + cIdx))
			escaped := html.EscapeString(val)
			sheetBuf.WriteString(fmt.Sprintf(`<c r="%s%d" t="inlineStr"><is><t>%s</t></is></c>`, colLetter, rowNum, escaped))
		}
		sheetBuf.WriteString(`</row>`)
	}
	sheetBuf.WriteString(`</sheetData></worksheet>`)

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`

	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

	workbook := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="%s" sheetId="1" r:id="rId1"/></sheets>
</workbook>`, html.EscapeString(sheetTitle))

	wbRels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`

	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	w, _ := zw.Create("[Content_Types].xml")
	_, _ = w.Write([]byte(contentTypes))
	w, _ = zw.Create("_rels/.rels")
	_, _ = w.Write([]byte(rels))
	w, _ = zw.Create("xl/workbook.xml")
	_, _ = w.Write([]byte(workbook))
	w, _ = zw.Create("xl/_rels/workbook.xml.rels")
	_, _ = w.Write([]byte(wbRels))
	w, _ = zw.Create("xl/worksheets/sheet1.xml")
	_, _ = w.Write(sheetBuf.Bytes())

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// pushTaskToCem 严格对标 CemPushService::pushTask + backfill_tasksid.php 自动补数
func (ctrl *TraceController) pushTaskToCem(db *sql.DB, taskOrder string) (int, string, error) {
	writeCemPushLog(fmt.Sprintf("========== 开始推送工单: %s ==========", taskOrder))

	records, err := core.QueryMaps(db, `
		SELECT b.task_order, b.box_sn, b.parent_code,
		       s.child_code, s.child_sn, s.scanned_at,
		       u.id AS scanner_id, COALESCE(u.badge_no, u.username, CAST(u.id AS CHAR)) AS scanner_emp_no,
		       t.order_type, t.tasksid, t.rel_no
		FROM trace_boxes b
		INNER JOIN trace_scans s ON b.id = s.box_id
		LEFT JOIN users u ON s.scanned_by = u.id
		LEFT JOIN trace_tasks t ON b.task_order = t.task_order
		WHERE b.task_order = ?
		ORDER BY b.id ASC, s.id ASC
	`, taskOrder)
	if err != nil {
		return 0, "", fmt.Errorf("数据库查询异常: %v", err)
	}
	if len(records) == 0 {
		return 0, "", fmt.Errorf("工单 [%s] 没有任何扫码追溯记录", taskOrder)
	}

	orderType := strings.TrimSpace(core.AsString(records[0]["order_type"]))
	tasksid := strings.TrimSpace(core.AsString(records[0]["tasksid"]))
	relNo := strings.TrimSpace(core.AsString(records[0]["rel_no"]))
	isXqOrder := (orderType == "xq_project" || strings.HasPrefix(strings.ToUpper(taskOrder), "XQ"))

	client := newCemClient()
	if err := client.assertConfigured(); err != nil {
		return 0, "", err
	}
	if !client.ensureLogin() {
		reason := client.lastError
		if reason == "" {
			reason = "未知原因"
		}
		return 0, "", fmt.Errorf("CEM 登录失败：%s", reason)
	}

	finalTaskNo := ""
	finalProjectNo := ""
	cardError := ""

	if isXqOrder {
		// ★ 若历史 XQ 单在 trace_tasks 中缺失 tasksid 或 rel_no，按 backfill_tasksid.php 同口径自动回填
		if tasksid == "" || relNo == "" {
			if bTasksid, err := resolveXqTasksid(taskOrder, client); err == nil && bTasksid != "" {
				if tasksid == "" {
					tasksid = bTasksid
				}
				if relNo == "" {
					relNo = client.queryRelNoByCardNo(tasksid)
				}
				_, _ = db.Exec("UPDATE trace_tasks SET tasksid = ?, rel_no = ? WHERE task_order = ?", tasksid, relNo, taskOrder)
			}
		}
		if tasksid != "" {
			finalTaskNo = tasksid
		} else {
			finalTaskNo = taskOrder
		}
		finalProjectNo = relNo
	} else {
		finalProjectNo = taskOrder
		cardNo, qErr := client.queryProjectCardNo(taskOrder)
		if qErr != nil {
			cardError = qErr.Error()
		}
		if cardNo != "" {
			finalTaskNo = cardNo
		} else {
			finalTaskNo = taskOrder
		}
	}

	headers := []string{"装置条码", "装置物料编码", "箱柜条码", "箱柜编码", "扫码人员ID", "扫码时间", "设备号", "任务号", "工程号"}
	dataRows := make([][]string, 0, len(records))
	for _, r := range records {
		scannerID := strings.TrimSpace(core.AsString(r["scanner_emp_no"]))
		if scannerID == "" {
			scannerID = strings.TrimSpace(core.AsString(r["scanner_id"]))
		}
		if scannerID == "" {
			scannerID = "20280"
		}
		cSn := core.AsString(r["child_sn"])
		cCode := core.AsString(r["child_code"])
		if len(cCode) > 10 {
			cCode = cCode[:10]
		}
		bSn := core.AsString(r["box_sn"])
		pCode := core.AsString(r["parent_code"])
		if len(pCode) > 10 {
			pCode = pCode[:10]
		}
		sTime := strings.TrimSpace(core.AsString(r["scanned_at"]))
		if sTime == "" {
			sTime = time.Now().Format("2006-01-02 15:04:05")
		}
		dataRows = append(dataRows, []string{
			cSn, cCode, bSn, pCode, scannerID, sTime, cemDeviceNo, finalTaskNo, finalProjectNo,
		})
	}

	xlsxBytes, err := generateCemXlsx("Sheet1", headers, dataRows)
	if err != nil {
		return 0, "", fmt.Errorf("生成 CEM Excel 失败: %v", err)
	}

	exportDir := "exports/quality_trace"
	_ = os.MkdirAll(exportDir, 0755)
	safeName := reSafeFile.ReplaceAllString(taskOrder, "_")
	filename := fmt.Sprintf("MES_%s_%s.xlsx", safeName, time.Now().Format("20060102150405"))
	filepathStr := filepath.Join(exportDir, filename)
	if err := os.WriteFile(filepathStr, xlsxBytes, 0644); err != nil {
		return 0, "", fmt.Errorf("无法写入导出文件: %v", err)
	}

	if cardError != "" {
		return 0, filename, fmt.Errorf("%s (已保存Excel: %s)", cardError, filename)
	}

	success := false
	resultMsg := ""

	alertMsg, respHtml, upErr := client.uploadQualityExcel(filepathStr, filename)
	if upErr != nil {
		success = false
		resultMsg = upErr.Error()
	} else if alertMsg != "" {
		lowerAlert := strings.ToLower(alertMsg)
		if strings.Contains(alertMsg, "失败") || strings.Contains(alertMsg, "错误") || strings.Contains(lowerAlert, "error") {
			success = false
			resultMsg = "CEM 提示: " + alertMsg
		} else {
			success = true
			resultMsg = alertMsg
		}
	} else {
		if strings.Contains(respHtml, "成功") {
			success = true
			resultMsg = "CEM 导入完成"
		} else {
			plain := stripTagsAndCollapseSpace(respHtml)
			runes := []rune(plain)
			if len(runes) > 100 {
				plain = string(runes[:100])
			}
			success = false
			resultMsg = fmt.Sprintf("CEM 未返回导入成功弹窗 (响应: %s)，详细内容见 /tmp/cem_last_upload_result.html", plain)
		}
	}

	pushFlag := 0
	if success {
		pushFlag = 1
	}
	dbResult := resultMsg
	if rRunes := []rune(dbResult); len(rRunes) > 255 {
		dbResult = string(rRunes[:255])
	}
	_, _ = db.Exec("UPDATE trace_tasks SET cem_pushed = ?, cem_pushed_at = NOW(), cem_push_result = ? WHERE task_order = ?",
		pushFlag, dbResult, taskOrder)

	if !success {
		return 0, filename, fmt.Errorf("%s", resultMsg)
	}

	writeCemPushLog(fmt.Sprintf("推送成功: %s, 共 %d 条, 结果: %s", taskOrder, len(records), resultMsg))
	return len(records), filename, nil
}

// ManualPushCem POST /api/trace/manualPushCem
// 严格对齐 legacy_trace.php manual_push_cem
func (ctrl *TraceController) ManualPushCem(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "推送失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_scan"] == 0 && perms["can_add_material"] == 0 && perms["can_view"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有推送 CEM 的权限！", gin.H{})
		return
	}

	in := core.ParseInputMap(c)
	taskOrder := strings.TrimSpace(core.AsString(in["task_order"]))
	if taskOrder == "" {
		core.Error(c, "请输入要推送的单号！", gin.H{})
		return
	}

	cnt, file, pushErr := ctrl.pushTaskToCem(db, taskOrder)
	if pushErr != nil {
		log.Printf("[trace][manual_push_cem][%s] %v", taskOrder, pushErr)
		msg := pushErr.Error()
		msg = reAbsPathSanit.ReplaceAllString(msg, "")
		msg = strings.TrimSpace(reWhitespace.ReplaceAllString(msg, " "))
		if msg == "" {
			msg = "CEM 平台未返回明确结果，请稍后重试"
		}
		if runes := []rune(msg); len(runes) > 160 {
			msg = string(runes[:160]) + "..."
		}
		core.Error(c, "CEM 推送失败："+msg, gin.H{})
		return
	}

	service.LogAudit(c, "质量追溯", "手动推送CEM", true, "", 0, nil)
	core.Success(c, fmt.Sprintf("CEM 推送成功：共上传 %d 条追溯明细", cnt), gin.H{
		"count": cnt,
		"file":  file,
	})
}

// ExportExcel GET|POST /api/trace/exportExcel
// 严格对齐 legacy_trace.php export_excel（9 列标准 .xlsx）
func (ctrl *TraceController) ExportExcel(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	perms := ctrl.calcUserPerms(db, sess.User)
	if perms["can_view"] == 0 && perms["is_admin"] == 0 {
		core.Error(c, "您没有导出追溯记录的权限！", gin.H{})
		return
	}

	in := core.ParseInputMap(c)
	search := strings.TrimSpace(core.AsString(in["search"]))
	where := "1=1"
	params := []any{}
	if search != "" {
		where = "(b.task_order LIKE ? OR b.box_sn LIKE ? OR s.child_code LIKE ? OR s.child_sn LIKE ?)"
		k := "%" + search + "%"
		params = append(params, k, k, k, k)
	}

	rows, err := core.QueryMaps(db, fmt.Sprintf(`
		SELECT b.task_order, b.box_sn, b.parent_code, s.scan_point, s.child_code, s.child_sn, s.scanned_at,
		       u.id AS scanner_id, COALESCE(u.badge_no, u.username, CAST(u.id AS CHAR)) AS scanner_emp_no,
		       t.order_type, t.tasksid, t.rel_no
		FROM trace_boxes b
		INNER JOIN trace_scans s ON b.id = s.box_id
		LEFT JOIN users u ON s.scanned_by = u.id
		LEFT JOIN trace_tasks t ON b.task_order = t.task_order
		WHERE %s
		ORDER BY s.id DESC
	`, where), params...)
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}

	client := newCemClient()
	cachedCards := make(map[string]string)
	exportRows := make([][]string, 0, len(rows))

	for _, r := range rows {
		taskOrder := strings.TrimSpace(core.AsString(r["task_order"]))
		orderType := strings.TrimSpace(core.AsString(r["order_type"]))
		isXq := (orderType == "xq_project" || strings.HasPrefix(strings.ToUpper(taskOrder), "XQ"))

		finalTaskNo := ""
		finalProjectNo := ""
		if isXq {
			tasksid := strings.TrimSpace(core.AsString(r["tasksid"]))
			relNo := strings.TrimSpace(core.AsString(r["rel_no"]))
			if tasksid != "" {
				finalTaskNo = tasksid
			} else {
				finalTaskNo = taskOrder
			}
			finalProjectNo = relNo
		} else {
			if taskOrder != "" {
				if _, exists := cachedCards[taskOrder]; !exists {
					cardNo, qErr := client.queryProjectCardNo(taskOrder)
					if qErr == nil {
						cachedCards[taskOrder] = cardNo
					} else {
						cachedCards[taskOrder] = ""
					}
				}
			}
			finalTaskNo = cachedCards[taskOrder]
			finalProjectNo = taskOrder
		}

		scannerID := strings.TrimSpace(core.AsString(r["scanner_emp_no"]))
		if scannerID == "" {
			scannerID = strings.TrimSpace(core.AsString(r["scanner_id"]))
		}
		if scannerID == "" {
			scannerID = "20280"
		}

		cCode := core.AsString(r["child_code"])
		if len(cCode) > 10 {
			cCode = cCode[:10]
		}
		pCode := core.AsString(r["parent_code"])
		if len(pCode) > 10 {
			pCode = pCode[:10]
		}

		exportRows = append(exportRows, []string{
			core.AsString(r["child_sn"]),
			cCode,
			core.AsString(r["box_sn"]),
			pCode,
			scannerID,
			core.AsString(r["scanned_at"]),
			cemDeviceNo,
			finalTaskNo,
			finalProjectNo,
		})
	}

	headers := []string{"装置条码", "装置物料编码", "箱柜条码", "箱柜编码", "扫码人员ID", "扫码时间", "设备号", "任务号", "工程号"}
	suffix := time.Now().Format("20060102_150405")
	if search != "" {
		suffix = reSafeFile.ReplaceAllString(search, "_")
	}
	filename := "MES_Quality_" + suffix + ".xlsx"

	xlsxBytes, xErr := generateCemXlsx("质量追溯明细", headers, exportRows)
	if xErr != nil {
		core.ExportCSV(c, "MES_Quality_"+suffix+".csv", headers, exportRows)
		return
	}

	c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, url.PathEscape(filename)))
	c.Header("Cache-Control", "max-age=0")
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", xlsxBytes)
}

// ============================================================================
//  2. CEM 质量追溯查询（QualityTraceController）
// ============================================================================

type QualityTraceController struct{}

func NewQualityTraceController() *QualityTraceController {
	return &QualityTraceController{}
}

// Query GET|POST /api/quality_trace/query
func (ctrl *QualityTraceController) Query(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	taskID := strings.TrimSpace(core.AsString(in["task_id"]))
	start := strings.TrimSpace(core.AsString(in["start"]))
	end := strings.TrimSpace(core.AsString(in["end"]))

	where := []string{"1=1"}
	args := []any{}
	if taskID != "" {
		where = append(where, "(b.task_order LIKE ? OR b.box_sn LIKE ? OR b.parent_code LIKE ?)")
		k := "%" + taskID + "%"
		args = append(args, k, k, k)
	}
	if start != "" {
		where = append(where, "b.created_at >= ?")
		args = append(args, start+" 00:00:00")
	}
	if end != "" {
		where = append(where, "b.created_at <= ?")
		args = append(args, end+" 23:59:59")
	}
	boxes, _ := core.QueryMaps(db, fmt.Sprintf(`
		SELECT b.id, b.task_order, b.box_sn, b.parent_code, b.status, b.created_at,
		       COALESCE(t.description, '') AS product_desc,
		       COALESCE(t.batch_no, '') AS batch_no,
		       (SELECT COUNT(*) FROM trace_scans s WHERE s.box_id = b.id) AS scan_count
		FROM trace_boxes b
		LEFT JOIN trace_tasks t ON t.task_order = b.task_order
		WHERE %s
		ORDER BY b.id DESC LIMIT 200
	`, strings.Join(where, " AND ")), args...)

	headers := []string{"序号", "任务号/工单号", "一级装置条码", "产品编码", "产品名称", "批号", "二级件数", "追溯状态", "登记时间"}
	tableRows := make([]map[string]any, 0, len(boxes))
	for idx, b := range boxes {
		stText := "追溯进行中"
		if core.AsInt(b["status"]) == 1 {
			stText = "完整追溯"
		}
		boxSn := core.AsString(b["box_sn"])
		tableRows = append(tableRows, map[string]any{
			"cells": []string{
				strconv.Itoa(idx + 1),
				core.AsString(b["task_order"]),
				boxSn,
				core.AsString(b["parent_code"]),
				core.AsString(b["product_desc"]),
				core.AsString(b["batch_no"]),
				strconv.Itoa(core.AsInt(b["scan_count"])),
				stText,
				core.AsString(b["created_at"]),
			},
			"barcodes": map[string]string{"2": boxSn},
		})
	}
	core.Success(c, "查询成功", gin.H{"headers": headers, "rows": tableRows})
}

// Detail POST /api/quality_trace/detail
func (ctrl *QualityTraceController) Detail(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询下级物料失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	boxSn := strings.TrimSpace(core.AsString(in["id"]))
	scans, _ := core.QueryMaps(db, `
		SELECT s.scan_point, s.child_code, s.child_sn, s.scanned_at,
		       COALESCE(cfg.child_name, '') AS child_name,
		       COALESCE(NULLIF(u.full_name, ''), u.username, '系统') AS scanner_name
		FROM trace_scans s
		INNER JOIN trace_boxes b ON b.id = s.box_id
		LEFT JOIN trace_bom_config cfg ON cfg.task_order = s.task_order AND cfg.child_code = s.child_code AND cfg.scan_point = s.scan_point
		LEFT JOIN users u ON u.id = s.scanned_by
		WHERE b.box_sn = ?
		ORDER BY s.scan_point ASC, s.id ASC
	`, boxSn)
	if len(scans) == 0 {
		core.Success(c, "success", gin.H{"html": "<div style='padding:8px;color:#888;'>该装置暂无二级物料扫码明细</div>"})
		return
	}
	var sb strings.Builder
	sb.WriteString("<table><thead><tr><th>工位点</th><th>二级物料编码</th><th>物料名称</th><th>二级物料条码(SN)</th><th>扫码人</th><th>扫码时间</th></tr></thead><tbody>")
	for _, s := range scans {
		sb.WriteString(fmt.Sprintf("<tr><td>扫码点 %d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
			core.AsInt(s["scan_point"]),
			html.EscapeString(core.AsString(s["child_code"])),
			html.EscapeString(core.AsString(s["child_name"])),
			html.EscapeString(core.AsString(s["child_sn"])),
			html.EscapeString(core.AsString(s["scanner_name"])),
			html.EscapeString(core.AsString(s["scanned_at"])),
		))
	}
	sb.WriteString("</tbody></table>")
	core.Success(c, "success", gin.H{"html": sb.String()})
}

// Ccjl POST /api/quality_trace/ccjl
func (ctrl *QualityTraceController) Ccjl(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "加载调检记录失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	taskID := strings.TrimSpace(core.AsString(in["task_id"]))
	where := "is_deleted = 0"
	args := []any{}
	if taskID != "" {
		where += " AND (work_order LIKE ? OR task_code LIKE ? OR batch_no LIKE ?)"
		k := "%" + taskID + "%"
		args = append(args, k, k, k)
	}
	fpRows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM module16_records WHERE %s ORDER BY id DESC LIMIT 30", where), args...)
	var sb strings.Builder
	sb.WriteString("<h4 style='margin:0 0 10px 0;color:#2c3e50;'>首件检与装置调检综合记录</h4>")
	if len(fpRows) == 0 {
		sb.WriteString("<p style='color:#888;'>暂无匹配的首件检验与调检记录</p>")
	} else {
		sb.WriteString("<table style='width:100%;border-collapse:collapse;font-size:13px;' border='1' cellpadding='6'><thead><tr style='background:#f0f4f8;'><th>工单号</th><th>产品编码</th><th>产品名称</th><th>条码</th><th>检验结论</th><th>检验员</th><th>检验时间</th></tr></thead><tbody>")
		for _, r := range fpRows {
			sb.WriteString(fmt.Sprintf("<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
				html.EscapeString(core.AsString(r["work_order"])),
				html.EscapeString(core.AsString(r["task_code"])),
				html.EscapeString(core.AsString(r["description"])),
				html.EscapeString(core.AsString(r["barcode"])),
				html.EscapeString(core.AsString(r["check_result"])),
				html.EscapeString(core.AsString(r["created_by_name"])),
				html.EscapeString(core.AsString(r["created_at"])),
			))
		}
		sb.WriteString("</tbody></table>")
	}
	core.Success(c, "success", gin.H{"html": sb.String()})
}

// Export GET /api/quality_trace/export
func (ctrl *QualityTraceController) Export(c *gin.Context) {
	NewTraceController().ExportExcel(c)
}

// ============================================================================
//  3. 条码验证比对模块（BarcodeValidatorController）—— 1:1 对齐 barcode/Index.vue
// ============================================================================

type BarcodeValidatorController struct{}

func NewBarcodeValidatorController() *BarcodeValidatorController {
	return &BarcodeValidatorController{}
}

func (ctrl *BarcodeValidatorController) getPerm(db *sql.DB, user map[string]any) map[string]int {
	if core.IsAdminUser(user) {
		return map[string]int{"can_save_settings": 1, "can_delete": 1, "can_edit": 1, "is_admin": 1}
	}
	row, _ := core.QueryRowMap(db, "SELECT can_save_settings, can_delete, can_edit FROM barcode_validator_permissions WHERE user_id = ? LIMIT 1", core.AsInt(user["id"]))
	if row == nil {
		return map[string]int{"can_save_settings": 1, "can_delete": 0, "can_edit": 0, "is_admin": 0}
	}
	return map[string]int{
		"can_save_settings": core.AsInt(row["can_save_settings"]),
		"can_delete":        core.AsInt(row["can_delete"]),
		"can_edit":          core.AsInt(row["can_edit"]),
		"is_admin":          0,
	}
}

// GetMyPerm GET /api/barcode_validator/get_my_perm
func (ctrl *BarcodeValidatorController) GetMyPerm(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取权限失败", gin.H{})
		return
	}
	perm := ctrl.getPerm(db, sess.User)
	core.Success(c, "success", gin.H{"perm": perm, "permissions": perm})
}

// FetchWorkorder GET|POST /api/barcode_validator/fetch_workorder
func (ctrl *BarcodeValidatorController) FetchWorkorder(c *gin.Context) {
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
	meta := core.LookupWorkOrderInfo(db, wo)
	prodCode := ""
	desc := ""
	batchPrefix := ""
	qty := 1
	if meta != nil {
		if meta.WorkOrder != "" {
			wo = meta.WorkOrder
		}
		prodCode = meta.ProductCode
		desc = meta.ProductDesc
		batchPrefix = meta.BatchPrefix
		if meta.Quantity > 0 {
			qty = int(meta.Quantity)
		}
	}
	var refCnt int
	_ = db.QueryRow("SELECT COUNT(*) FROM barcode_reference WHERE work_order = ?", wo).Scan(&refCnt)
	if refCnt > qty {
		qty = refCnt
	}

	core.Success(c, "success", gin.H{
		"work_order":   wo,
		"product_code": prodCode,
		"description":  desc,
		"batch_prefix": batchPrefix,
		"quantity":     qty,
	})
}

// Progress GET /api/barcode_validator/progress
func (ctrl *BarcodeValidatorController) Progress(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取进度失败", gin.H{})
		return
	}
	wo := strings.TrimSpace(c.Query("work_order"))
	refs, _ := core.QueryMaps(db, "SELECT * FROM barcode_reference WHERE work_order = ? ORDER BY row_index ASC", wo)
	compRows, _ := core.QueryMaps(db, "SELECT DISTINCT row_index FROM barcode_completed_rows WHERE work_order = ? ORDER BY row_index ASC", wo)
	completedIdx := make([]int, 0, len(compRows))
	for _, r := range compRows {
		completedIdx = append(completedIdx, core.AsInt(r["row_index"]))
	}
	totalRows := len(refs)
	if totalRows == 0 {
		if meta := core.LookupWorkOrderInfo(db, wo); meta != nil && meta.Quantity > 0 {
			totalRows = int(meta.Quantity)
		}
	}
	core.Success(c, "success", gin.H{
		"totalRows":     totalRows,
		"completedRows": completedIdx,
		"reference":     refs,
	})
}

// GetSettings GET /api/barcode_validator/get_settings
func (ctrl *BarcodeValidatorController) GetSettings(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取设置失败", gin.H{})
		return
	}
	wo := strings.TrimSpace(c.Query("work_order"))
	row, _ := core.QueryRowMap(db, "SELECT enabled_fields, order_fields FROM barcode_user_settings WHERE work_order = ? ORDER BY id DESC LIMIT 1", wo)
	enabled := []string{"mod1", "mod2", "mod3", "mod4", "mod5", "mask_barcode", "mask_qr"}
	ordered := []string{}
	if row != nil {
		if ef := strings.TrimSpace(core.AsString(row["enabled_fields"])); ef != "" {
			enabled = strings.Split(ef, ",")
		}
		if of := strings.TrimSpace(core.AsString(row["order_fields"])); of != "" {
			ordered = strings.Split(of, ",")
		}
	}
	core.Success(c, "success", gin.H{
		"enabled_fields": enabled,
		"order_fields":   ordered,
	})
}

// SaveSettings POST /api/barcode_validator/save_settings
func (ctrl *BarcodeValidatorController) SaveSettings(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存设置失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	ef := strings.TrimSpace(core.AsString(in["enabled_fields"]))
	of := strings.TrimSpace(core.AsString(in["order_fields"]))
	_, _ = db.Exec("DELETE FROM barcode_user_settings WHERE work_order = ?", wo)
	_, _ = db.Exec("INSERT INTO barcode_user_settings (user_id, work_order, enabled_fields, order_fields, updated_at) VALUES (?, ?, ?, ?, NOW())",
		sess.UserID, wo, ef, of)
	core.Success(c, "设置已保存")
}

// ScanValidate POST /api/barcode_validator/scan_validate
func (ctrl *BarcodeValidatorController) ScanValidate(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "校验失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	rowIdx := core.AsInt(in["row_index"])
	field := strings.TrimSpace(core.AsString(in["field"]))
	val := strings.TrimSpace(core.AsString(in["value"]))
	batchPrefix := strings.TrimSpace(core.AsString(in["batch_prefix"]))
	quantity := core.AsInt(in["quantity"])

	if wo == "" || rowIdx < 0 || field == "" || val == "" {
		core.Error(c, "校验参数不完整", gin.H{})
		return
	}

	if field == "sn" {
		if len(val) != 22 || !reDigitsOnly.MatchString(val) {
			core.Error(c, "装置 SN 长度必须为 22 位，当前输入: "+val, gin.H{})
			return
		}
		if batchPrefix != "" && val[:16] != batchPrefix {
			core.Error(c, fmt.Sprintf("SN 前 16 位与工单批次不匹配！输入: %s，期望: %s", val[:16], batchPrefix), gin.H{})
			return
		}
		snSuffix, convErr := strconv.Atoi(val[16:22])
		if quantity > 0 && (convErr != nil || snSuffix < 1 || snSuffix > quantity) {
			core.Error(c, fmt.Sprintf("SN 编号超出计划范围 (000001 ~ %06d)", quantity), gin.H{})
			return
		}
		var dupID int
		if err := db.QueryRow("SELECT id FROM barcode_completed_rows WHERE work_order = ? AND sn = ? LIMIT 1", wo, val).Scan(&dupID); err == nil && dupID > 0 {
			core.Error(c, "该 SN 在本工单中已完成过校验，禁止重复使用", gin.H{})
			return
		}
		service.LogAudit(c, "条码校验", "SN校验通过", true, "", 0, nil)
		core.Success(c, "SN 校验通过")
		return
	}

	refRow, _ := core.QueryRowMap(db, "SELECT * FROM barcode_reference WHERE work_order = ? AND row_index = ? LIMIT 1", wo, rowIdx)
	if refRow == nil {
		core.Error(c, "该字段在参考表中未配置期望值", gin.H{})
		return
	}
	expected := strings.TrimSpace(core.AsString(refRow[field]))
	if expected == "" {
		core.Error(c, "该字段在参考表中未配置期望值", gin.H{})
		return
	}
	re22 := regexp.MustCompile(`\d{22}`)
	compareVal := re22.FindString(val)
	if compareVal == "" {
		digits := regexp.MustCompile(`\D`).ReplaceAllString(val, "")
		if len(digits) == 22 {
			compareVal = digits
		}
	}
	if compareVal == "" {
		core.Error(c, "扫描内容中未找到有效的 22 位纯数字编码: "+val, gin.H{})
		return
	}
	if compareVal != expected {
		core.Error(c, fmt.Sprintf("比对不一致！扫描值: %s，参考期望值: %s", compareVal, expected), gin.H{})
		return
	}
	service.LogAudit(c, "条码校验", "字段比对通过: "+field, true, "", 0, nil)
	core.Success(c, "校验通过")
}

// SubmitRow POST /api/barcode_validator/submit_row
func (ctrl *BarcodeValidatorController) SubmitRow(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "提交失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	_, err = db.Exec(`
		INSERT INTO barcode_completed_rows
		(work_order, row_index, operator, sn, mod1, mod2, mod3, mod4, mod5, mask_barcode, mask_qr, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, strings.TrimSpace(core.AsString(in["work_order"])),
		core.AsInt(in["row_index"]),
		opName,
		strings.TrimSpace(core.AsString(in["sn"])),
		strings.TrimSpace(core.AsString(in["mod1"])),
		strings.TrimSpace(core.AsString(in["mod2"])),
		strings.TrimSpace(core.AsString(in["mod3"])),
		strings.TrimSpace(core.AsString(in["mod4"])),
		strings.TrimSpace(core.AsString(in["mod5"])),
		strings.TrimSpace(core.AsString(in["mask_barcode"])),
		strings.TrimSpace(core.AsString(in["mask_qr"])))
	if err != nil {
		core.Exception(c, err, "保存验证记录失败", gin.H{})
		return
	}
	core.Success(c, "整行校验完成")
}

// GetCompletedRows GET /api/barcode_validator/get_completed_rows
func (ctrl *BarcodeValidatorController) GetCompletedRows(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取记录失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	search := strings.TrimSpace(core.AsString(in["search"]))
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	offset := (page - 1) * limit

	where := []string{"1=1"}
	args := []any{}
	if wo != "" {
		where = append(where, "work_order = ?")
		args = append(args, wo)
	}
	if search != "" {
		where = append(where, "(sn LIKE ? OR mod1 LIKE ? OR mod2 LIKE ? OR operator LIKE ?)")
		k := "%" + search + "%"
		args = append(args, k, k, k, k)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM barcode_completed_rows WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM barcode_completed_rows WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", whereSQL),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"rows":        rows,
		"total":       total,
		"total_pages": core.CalcTotalPages(total, limit),
	})
}

// GetReferenceRows GET /api/barcode_validator/get_reference_rows
func (ctrl *BarcodeValidatorController) GetReferenceRows(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取参考底表失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := core.AsInt(in["limit"])
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	offset := (page - 1) * limit

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM barcode_reference WHERE work_order = ?", wo).Scan(&total)
	rows, _ := core.QueryMaps(db, "SELECT * FROM barcode_reference WHERE work_order = ? ORDER BY row_index ASC LIMIT ? OFFSET ?", wo, limit, offset)
	core.Success(c, "success", gin.H{
		"rows":        rows,
		"total":       total,
		"total_pages": core.CalcTotalPages(total, limit),
	})
}

// UploadReference POST /api/barcode_validator/upload_reference
func (ctrl *BarcodeValidatorController) UploadReference(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "上传失败", gin.H{})
		return
	}
	wo := strings.TrimSpace(c.PostForm("work_order"))
	fileHeader, fErr := c.FormFile("file")
	if wo == "" || fErr != nil {
		core.Error(c, "请提供工单号并选择参考表文件", gin.H{})
		return
	}
	f, err := fileHeader.Open()
	if err != nil {
		core.Error(c, "读取文件失败", gin.H{})
		return
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	records, _ := r.ReadAll()

	_, _ = db.Exec("DELETE FROM barcode_reference WHERE work_order = ?", wo)
	inserted := 0
	for idx, row := range records {
		if idx == 0 || len(row) == 0 {
			continue
		}
		getCol := func(i int) string {
			if i < len(row) {
				return strings.TrimSpace(strings.TrimPrefix(row[i], "\xEF\xBB\xBF"))
			}
			return ""
		}
		_, _ = db.Exec(`
			INSERT INTO barcode_reference (work_order, row_index, mod1, mod2, mod3, mod4, mod5, mask_barcode, mask_qr, upload_time)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
		`, wo, inserted, getCol(0), getCol(1), getCol(2), getCol(3), getCol(4), getCol(5), getCol(6))
		inserted++
	}
	core.Success(c, fmt.Sprintf("成功导入 %d 行参考底表数据", inserted), gin.H{"count": inserted})
}

// UpdateCompleted POST /api/barcode_validator/update_completed
func (ctrl *BarcodeValidatorController) UpdateCompleted(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	_, _ = db.Exec(`
		UPDATE barcode_completed_rows
		SET sn = ?, mod1 = ?, mod2 = ?, mod3 = ?, mod4 = ?, mod5 = ?, mask_barcode = ?, mask_qr = ?
		WHERE id = ?
	`, strings.TrimSpace(core.AsString(in["sn"])),
		strings.TrimSpace(core.AsString(in["mod1"])),
		strings.TrimSpace(core.AsString(in["mod2"])),
		strings.TrimSpace(core.AsString(in["mod3"])),
		strings.TrimSpace(core.AsString(in["mod4"])),
		strings.TrimSpace(core.AsString(in["mod5"])),
		strings.TrimSpace(core.AsString(in["mask_barcode"])),
		strings.TrimSpace(core.AsString(in["mask_qr"])), id)
	core.Success(c, "记录已更新")
}

// DeleteCompleted POST /api/barcode_validator/delete_completed
func (ctrl *BarcodeValidatorController) DeleteCompleted(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("DELETE FROM barcode_completed_rows WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// ExportCompleted GET /api/barcode_validator/export_completed
func (ctrl *BarcodeValidatorController) ExportCompleted(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	wo := strings.TrimSpace(c.Query("work_order"))
	where := "1=1"
	args := []any{}
	if wo != "" {
		where = "work_order = ?"
		args = append(args, wo)
	}
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM barcode_completed_rows WHERE %s ORDER BY id ASC", where), args...)
	headers := []string{"工单号", "行号", "装置SN", "模组1", "模组2", "模组3", "模组4", "模组5", "面膜条码", "面膜二维码", "作业员", "完成时间"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["work_order"]),
			 strconv.Itoa(core.AsInt(r["row_index"]) + 1),
			core.AsString(r["sn"]),
			core.AsString(r["mod1"]),
			core.AsString(r["mod2"]),
			core.AsString(r["mod3"]),
			core.AsString(r["mod4"]),
			core.AsString(r["mod5"]),
			core.AsString(r["mask_barcode"]),
			core.AsString(r["mask_qr"]),
			core.AsString(r["operator"]),
			core.AsString(r["completed_at"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("条码比对记录_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// ============================================================================
//  4. 首件检管理模块（FirstPieceController）—— 1:1 对齐 first_piece/Index.vue
// ============================================================================

type FirstPieceController struct{}

func NewFirstPieceController() *FirstPieceController {
	return &FirstPieceController{}
}

func (ctrl *FirstPieceController) canManageCriteria(db *sql.DB, user map[string]any) bool {
	if core.IsAdminUser(user) {
		return true
	}
	var flag int
	_ = db.QueryRow("SELECT can_manage_criteria FROM first_piece_permissions WHERE user_id = ? LIMIT 1", core.AsInt(user["id"])).Scan(&flag)
	return flag == 1
}

// Init GET /api/first_piece/init
func (ctrl *FirstPieceController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "初始化失败", gin.H{})
		return
	}
	core.Success(c, "success", gin.H{
		"is_admin":            core.IsAdminUser(sess.User),
		"can_manage_criteria": ctrl.canManageCriteria(db, sess.User),
		"can_manage":          core.CanEditModule(sess.User, "first_piece"),
	})
}

// GetWorkOrder GET|POST /api/first_piece/get_work_order
func (ctrl *FirstPieceController) GetWorkOrder(c *gin.Context) {
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
	meta := core.LookupWorkOrderInfo(db, wo)
	if meta == nil {
		core.Error(c, "未查到该工单信息", gin.H{})
		return
	}
	crit, _ := core.QueryRowMap(db, "SELECT * FROM module16_criteria WHERE work_order = ? LIMIT 1", meta.WorkOrder)
	bcExp := ""
	qrExp := ""
	if crit != nil {
		bcExp = core.AsString(crit["barcode_expected"])
		qrExp = core.AsString(crit["qrcode_expected"])
	}
	core.Success(c, "success", gin.H{
		"work_order":       meta.WorkOrder,
		"task_code":        meta.ProductCode,
		"description":      meta.ProductDesc,
		"quantity":         int(math.Max(meta.Quantity, 1)),
		"batch_no":         meta.BatchNo,
		"barcode_expected": bcExp,
		"qrcode_expected":  qrExp,
	})
}

// SaveRecord POST /api/first_piece/save_record
func (ctrl *FirstPieceController) SaveRecord(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	barcode := strings.TrimSpace(core.AsString(in["barcode"]))
	qrcode := strings.TrimSpace(core.AsString(in["qrcode"]))
	needCheck := core.AsInt(in["need_check"])

	checkResult := "合格"
	if needCheck == 1 {
		crit, _ := core.QueryRowMap(db, "SELECT * FROM module16_criteria WHERE work_order = ? LIMIT 1", wo)
		if crit != nil {
			bcExp := strings.TrimSpace(core.AsString(crit["barcode_expected"]))
			qrExp := strings.TrimSpace(core.AsString(crit["qrcode_expected"]))
			if (bcExp != "" && barcode != "" && barcode != bcExp) || (qrExp != "" && qrcode != "" && qrcode != qrExp) {
				checkResult = "不合格"
			}
		}
	}
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	res, err := db.Exec(`
		INSERT INTO module16_records
		(work_order, task_code, description, quantity, batch_no, barcode, qrcode, need_check, check_result, created_by, created_by_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, wo,
		strings.TrimSpace(core.AsString(in["task_code"])),
		strings.TrimSpace(core.AsString(in["description"])),
		strings.TrimSpace(core.AsString(in["quantity"])),
		strings.TrimSpace(core.AsString(in["batch_no"])),
		barcode, qrcode, needCheck, checkResult, sess.UserID, opName)
	if err != nil {
		core.Exception(c, err, "保存记录失败", gin.H{})
		return
	}
	newID, _ := res.LastInsertId()
	core.Success(c, "保存成功", gin.H{"id": newID, "check_result": checkResult})
}

// List GET /api/first_piece/list
func (ctrl *FirstPieceController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取列表失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	bc := strings.TrimSpace(core.AsString(in["barcode"]))
	resFilter := strings.TrimSpace(core.AsString(in["check_result"]))

	where := []string{"is_deleted = 0"}
	args := []any{}
	if wo != "" {
		where = append(where, "work_order LIKE ?")
		args = append(args, "%"+wo+"%")
	}
	if bc != "" {
		where = append(where, "(barcode LIKE ? OR qrcode LIKE ?)")
		args = append(args, "%"+bc+"%", "%"+bc+"%")
	}
	if resFilter != "" {
		where = append(where, "check_result = ?")
		args = append(args, resFilter)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM module16_records WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM module16_records WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", whereSQL),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":             rows,
		"total_records":       total,
		"total_pages":         core.CalcTotalPages(total, limit),
		"is_admin":            core.IsAdminUser(sess.User),
		"can_manage_criteria": ctrl.canManageCriteria(db, sess.User),
		"can_manage":          core.CanEditModule(sess.User, "first_piece"),
	})
}

// GetCriteria GET /api/first_piece/get_criteria
func (ctrl *FirstPieceController) GetCriteria(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取判据失败", gin.H{})
		return
	}
	wo := strings.TrimSpace(c.Query("work_order"))
	row, _ := core.QueryRowMap(db, "SELECT * FROM module16_criteria WHERE work_order = ? LIMIT 1", wo)
	core.Success(c, "success", gin.H{"criteria": row})
}

// SaveCriteria POST /api/first_piece/save_criteria
func (ctrl *FirstPieceController) SaveCriteria(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存判据失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	bc := strings.TrimSpace(core.AsString(in["barcode_expected"]))
	qr := strings.TrimSpace(core.AsString(in["qrcode_expected"]))
	if wo == "" {
		core.Error(c, "工单号不能为空", gin.H{})
		return
	}
	_, err = db.Exec(`
		INSERT INTO module16_criteria (work_order, barcode_expected, qrcode_expected, created_at, updated_at)
		VALUES (?, ?, ?, NOW(), NOW())
		ON DUPLICATE KEY UPDATE barcode_expected = VALUES(barcode_expected), qrcode_expected = VALUES(qrcode_expected), updated_at = NOW()
	`, wo, bc, qr)
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	core.Success(c, "保存成功")
}

// ListCriteria GET /api/first_piece/list_criteria
func (ctrl *FirstPieceController) ListCriteria(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取判据列表失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM module16_criteria ORDER BY updated_at DESC, id DESC")
	core.Success(c, "success", gin.H{"list": rows})
}

// DeleteCriteria POST /api/first_piece/delete_criteria
func (ctrl *FirstPieceController) DeleteCriteria(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("DELETE FROM module16_criteria WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Delete POST /api/first_piece/delete
func (ctrl *FirstPieceController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("UPDATE module16_records SET is_deleted = 1 WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Export GET /api/first_piece/export
func (ctrl *FirstPieceController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM module16_records WHERE is_deleted = 0 ORDER BY id DESC LIMIT 2000")
	headers := []string{"工单号", "物料代码", "产品描述", "批号", "一维条码", "二维码", "判定结论", "检验员", "检验时间"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["work_order"]),
			core.AsString(r["task_code"]),
			core.AsString(r["description"]),
			core.AsString(r["batch_no"]),
			core.AsString(r["barcode"]),
			core.AsString(r["qrcode"]),
			core.AsString(r["check_result"]),
			core.AsString(r["created_by_name"]),
			core.AsString(r["created_at"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("首件检记录_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// ============================================================================
//  5. 阻抗测试记录模块（ImpedanceController）—— 1:1 对齐 impedance/Index.vue
// ============================================================================

type ImpedanceController struct{}

func NewImpedanceController() *ImpedanceController {
	return &ImpedanceController{}
}

// GetBatchInfo GET|POST /api/impedance/get_batch_info
func (ctrl *ImpedanceController) GetBatchInfo(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询批次失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	snVal := strings.TrimSpace(core.AsString(in["sn"]))
	if snVal == "" {
		snVal = strings.TrimSpace(core.AsString(in["work_order"]))
	}
	meta := core.LookupWorkOrderInfo(db, snVal)

	var histCnt int
	_ = db.QueryRow("SELECT COUNT(*) FROM impedance_records WHERE sn = ?", snVal).Scan(&histCnt)
	lastRec, _ := core.QueryRowMap(db, "SELECT id, created_at, operator FROM impedance_records WHERE sn = ? ORDER BY id DESC LIMIT 1", snVal)

	if meta == nil {
		core.Success(c, "success", gin.H{
			"found":         false,
			"history_count": histCnt,
			"last_record":   lastRec,
		})
		return
	}
	core.Success(c, "success", gin.H{
		"found":           true,
		"batch_no":        meta.BatchNo,
		"work_order":      meta.WorkOrder,
		"code":            meta.ProductCode,
		"task_code":       meta.ProductCode,
		"description":     meta.ProductDesc,
		"outsourced_unit": meta.OutsourcedUnit,
		"history_count":   histCnt,
		"last_record":     lastRec,
	})
}

func calcImpedanceGroup(vals []float64, stdPct float64) (*float64, string) {
	if len(vals) == 0 {
		return nil, "未填写"
	}
	if len(vals) == 1 {
		z := 0.0
		return &z, "单值不计算"
	}
	minV, maxV, sumV := vals[0], vals[0], 0.0
	for _, v := range vals {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
		sumV += v
	}
	avg := sumV / float64(len(vals))
	errVal := 0.0
	if avg != 0 {
		errVal = math.Round(((maxV-minV)/avg)*10000) / 100
	}
	conc := "合格"
	if errVal > stdPct {
		conc = "不合格"
	}
	return &errVal, conc
}

func (ctrl *ImpedanceController) computeImpedanceFromInput(in map[string]any) (*float64, *float64, string, string, int) {
	std := core.AsInt(in["error_standard"])
	if std <= 0 {
		std = 10
	}
	pVals := []float64{}
	for i := 1; i <= 9; i++ {
		s := strings.TrimSpace(core.AsString(in[fmt.Sprintf("protect_impedance%d", i)]))
		if s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				pVals = append(pVals, f)
			}
		}
	}
	cVals := []float64{}
	for i := 1; i <= 3; i++ {
		s := strings.TrimSpace(core.AsString(in[fmt.Sprintf("control_impedance%d", i)]))
		if s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				cVals = append(cVals, f)
			}
		}
	}
	pErr, pConc := calcImpedanceGroup(pVals, float64(std))
	cErr, cConc := calcImpedanceGroup(cVals, float64(std))
	return pErr, cErr, pConc, cConc, std
}

// CalculateErrors POST /api/impedance/calculate_errors
func (ctrl *ImpedanceController) CalculateErrors(c *gin.Context) {
	in := core.ParseInputMap(c)
	pErr, cErr, pConc, cConc, _ := ctrl.computeImpedanceFromInput(in)
	core.Success(c, "success", gin.H{
		"protect_error":      pErr,
		"control_error":      cErr,
		"protect_conclusion": pConc,
		"control_conclusion": cConc,
	})
}

// List GET /api/impedance/list
func (ctrl *ImpedanceController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取记录失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	search := strings.TrimSpace(core.AsString(in["search_all"]))

	where := "1=1"
	args := []any{}
	if search != "" {
		where = "(sn LIKE ? OR work_order LIKE ? OR code LIKE ? OR description LIKE ? OR operator LIKE ?)"
		k := "%" + search + "%"
		args = append(args, k, k, k, k, k)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM impedance_records WHERE "+where, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM impedance_records WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", where),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
		"user_perm":     gin.H{"can_input": 1, "can_view_records": 1},
	})
}

func nullableFloat(in map[string]any, key string) any {
	s := strings.TrimSpace(core.AsString(in[key]))
	if s == "" || s == "null" {
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return nil
}

// Add POST /api/impedance/add
func (ctrl *ImpedanceController) Add(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	sn := strings.TrimSpace(core.AsString(in["sn"]))
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	forceDup := core.AsInt(in["force_duplicate"]) == 1

	if !forceDup {
		var cnt int
		_ = db.QueryRow("SELECT COUNT(*) FROM impedance_records WHERE sn = ?", sn).Scan(&cnt)
		if cnt > 0 {
			core.Success(c, "存在复测记录", gin.H{
				"is_duplicate": true,
				"message":      fmt.Sprintf("SN [%s] 已存在 %d 条历史阻抗测试记录，是否作为复测继续保存？", sn, cnt),
			})
			return
		}
	}

	pErr, cErr, pConc, cConc, std := ctrl.computeImpedanceFromInput(in)
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	res, err := db.Exec(`
		INSERT INTO impedance_records
		(sn, work_order, code, description,
		 protect_impedance1, protect_impedance2, protect_impedance3, protect_impedance4, protect_impedance5,
		 protect_impedance6, protect_impedance7, protect_impedance8, protect_impedance9,
		 control_impedance1, control_impedance2, control_impedance3,
		 protect_error, control_error, protect_conclusion, control_conclusion, operator, error_standard, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, sn, wo, strings.TrimSpace(core.AsString(in["code"])), strings.TrimSpace(core.AsString(in["description"])),
		nullableFloat(in, "protect_impedance1"), nullableFloat(in, "protect_impedance2"), nullableFloat(in, "protect_impedance3"),
		nullableFloat(in, "protect_impedance4"), nullableFloat(in, "protect_impedance5"), nullableFloat(in, "protect_impedance6"),
		nullableFloat(in, "protect_impedance7"), nullableFloat(in, "protect_impedance8"), nullableFloat(in, "protect_impedance9"),
		nullableFloat(in, "control_impedance1"), nullableFloat(in, "control_impedance2"), nullableFloat(in, "control_impedance3"),
		pErr, cErr, pConc, cConc, opName, std)
	if err != nil {
		core.Exception(c, err, "保存阻抗测试记录失败", gin.H{})
		return
	}
	newID, _ := res.LastInsertId()
	core.Success(c, "保存成功", gin.H{"id": newID})
}

// Update POST /api/impedance/update
func (ctrl *ImpedanceController) Update(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["record_id"])
	if id <= 0 {
		id = core.AsInt(in["id"])
	}
	pErr, cErr, pConc, cConc, std := ctrl.computeImpedanceFromInput(in)
	_, err = db.Exec(`
		UPDATE impedance_records
		SET sn = ?, work_order = ?, code = ?, description = ?,
		    protect_impedance1 = ?, protect_impedance2 = ?, protect_impedance3 = ?, protect_impedance4 = ?, protect_impedance5 = ?,
		    protect_impedance6 = ?, protect_impedance7 = ?, protect_impedance8 = ?, protect_impedance9 = ?,
		    control_impedance1 = ?, control_impedance2 = ?, control_impedance3 = ?,
		    protect_error = ?, control_error = ?, protect_conclusion = ?, control_conclusion = ?, error_standard = ?
		WHERE id = ?
	`, strings.TrimSpace(core.AsString(in["sn"])),
		strings.TrimSpace(core.AsString(in["work_order"])),
		strings.TrimSpace(core.AsString(in["code"])),
		strings.TrimSpace(core.AsString(in["description"])),
		nullableFloat(in, "protect_impedance1"), nullableFloat(in, "protect_impedance2"), nullableFloat(in, "protect_impedance3"),
		nullableFloat(in, "protect_impedance4"), nullableFloat(in, "protect_impedance5"), nullableFloat(in, "protect_impedance6"),
		nullableFloat(in, "protect_impedance7"), nullableFloat(in, "protect_impedance8"), nullableFloat(in, "protect_impedance9"),
		nullableFloat(in, "control_impedance1"), nullableFloat(in, "control_impedance2"), nullableFloat(in, "control_impedance3"),
		pErr, cErr, pConc, cConc, std, id)
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	core.Success(c, "保存成功")
}

// Delete POST /api/impedance/delete
func (ctrl *ImpedanceController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("DELETE FROM impedance_records WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Export GET /api/impedance/export
func (ctrl *ImpedanceController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM impedance_records ORDER BY id DESC LIMIT 2000")
	headers := []string{"SN", "工单号", "产品编码", "产品描述", "保护阻抗误差(%)", "保护结论", "控制阻抗误差(%)", "控制结论", "测试人", "测试时间"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["sn"]),
			core.AsString(r["work_order"]),
			core.AsString(r["code"]),
			core.AsString(r["description"]),
			core.AsString(r["protect_error"]),
			core.AsString(r["protect_conclusion"]),
			core.AsString(r["control_error"]),
			core.AsString(r["control_conclusion"]),
			core.AsString(r["operator"]),
			core.AsString(r["created_at"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("阻抗测试记录_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// GetPermissions GET /api/impedance/get_permissions
func (ctrl *ImpedanceController) GetPermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取权限失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, `
		SELECT u.id, u.id AS user_id, u.username, u.full_name, u.team,
		       COALESCE(p.can_view_records, 1) AS can_view_records,
		       COALESCE(p.can_input, 1) AS can_input
		FROM users u
		LEFT JOIN impedance_permissions p ON p.user_id = u.id
		WHERE u.is_active = 1
		ORDER BY u.id ASC
	`)
	core.Success(c, "success", gin.H{"users": rows})
}

// UpdatePermission POST /api/impedance/update_permission
func (ctrl *ImpedanceController) UpdatePermission(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存权限失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	uid := core.AsInt(in["user_id"])
	_, _ = db.Exec(`
		INSERT INTO impedance_permissions (user_id, can_view_records, can_input, updated_at)
		VALUES (?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE can_view_records = VALUES(can_view_records), can_input = VALUES(can_input), updated_at = NOW()
	`, uid, core.AsInt(in["can_view_records"]), core.AsInt(in["can_input"]))
	core.Success(c, "权限更新成功")
}

// ============================================================================
//  6. 质量问题记录模块（QualityController）—— 1:1 对齐 quality/Index.vue & quality_issues
// ============================================================================

type QualityController struct{}

func NewQualityController() *QualityController {
	return &QualityController{}
}

// GetBatchInfo GET|POST /api/quality/get_batch_info
func (ctrl *QualityController) GetBatchInfo(c *gin.Context) {
	NewImpedanceController().GetBatchInfo(c)
}

// List GET /api/quality/list
func (ctrl *QualityController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取质量问题列表失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	sn := strings.TrimSpace(core.AsString(in["sn"]))
	proc := strings.TrimSpace(core.AsString(in["process"]))

	where := []string{"is_deleted = 0"}
	args := []any{}
	if wo != "" {
		where = append(where, "work_order LIKE ?")
		args = append(args, "%"+wo+"%")
	}
	if sn != "" {
		where = append(where, "sn LIKE ?")
		args = append(args, "%"+sn+"%")
	}
	if proc != "" {
		where = append(where, "process = ?")
		args = append(args, proc)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM quality_issues WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM quality_issues WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", whereSQL),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
		"user_perm":     gin.H{"can_input": 1, "can_view_records": 1},
	})
}

// Add POST /api/quality/add
func (ctrl *QualityController) Add(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "添加失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	typeStr := strings.TrimSpace(core.AsString(in["type"]))
	batchNo := strings.TrimSpace(core.AsString(in["batch_no"]))
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	taskCode := strings.TrimSpace(core.AsString(in["task_code"]))
	desc := strings.TrimSpace(core.AsString(in["description"]))
	proc := strings.TrimSpace(core.AsString(in["process"]))
	issueDesc := strings.TrimSpace(core.AsString(in["issue_description"]))

	// 自动关联外协单位
	outsourced := ""
	if wo != "" {
		if meta := core.LookupWorkOrderInfo(db, wo); meta != nil && meta.OutsourcedUnit != "" {
			outsourced = meta.OutsourcedUnit
		}
	}

	imagePath := ""
	if fileHeader, fErr := c.FormFile("image"); fErr == nil && fileHeader != nil {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileHeader.Filename), "."))
		pubRoot := config.Get("PUBLIC_ROOT", "./public")
		subDir := time.Now().Format("200601")
		targetDir := filepath.Join(pubRoot, "uploads", "quality", subDir)
		_ = os.MkdirAll(targetDir, 0755)
		fileName := fmt.Sprintf("q_%s.%s", core.RandomHex(5), ext)
		if c.SaveUploadedFile(fileHeader, filepath.Join(targetDir, fileName)) == nil {
			imagePath = fmt.Sprintf("/uploads/quality/%s/%s", subDir, fileName)
		}
	}

	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}

	if typeStr == "batch" {
		var snList []string
		_ = json.Unmarshal([]byte(core.AsString(in["sn_list"])), &snList)
		for _, snItem := range snList {
			snItem = strings.TrimSpace(snItem)
			if snItem == "" {
				continue
			}
			_, _ = db.Exec(`
				INSERT INTO quality_issues
				(batch_no, work_order, task_code, description, sn, process, issue_description, image_path, outsourced_unit, created_by, created_by_name, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
			`, batchNo, wo, taskCode, desc, snItem, proc, issueDesc, imagePath, outsourced, sess.UserID, opName)
		}
		core.Success(c, "批量添加成功")
		return
	}

	sn := strings.TrimSpace(core.AsString(in["sn"]))
	if batchNo == "" && len(sn) >= 16 {
		batchNo = sn[:16]
	}
	_, err = db.Exec(`
		INSERT INTO quality_issues
		(batch_no, work_order, task_code, description, sn, process, issue_description, image_path, outsourced_unit, created_by, created_by_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, batchNo, wo, taskCode, desc, sn, proc, issueDesc, imagePath, outsourced, sess.UserID, opName)
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	core.Success(c, "质量问题记录添加成功")
}

// Update POST /api/quality/update
func (ctrl *QualityController) Update(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "修改失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	proc := strings.TrimSpace(core.AsString(in["process"]))
	issueDesc := strings.TrimSpace(core.AsString(in["issue_description"]))
	_, _ = db.Exec("UPDATE quality_issues SET process = ?, issue_description = ?, updated_at = NOW() WHERE id = ?", proc, issueDesc, id)
	core.Success(c, "修改成功")
}

// Delete POST /api/quality/delete
func (ctrl *QualityController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("UPDATE quality_issues SET is_deleted = 1 WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Export GET /api/quality/export
func (ctrl *QualityController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM quality_issues WHERE is_deleted = 0 ORDER BY id DESC LIMIT 2000")
	headers := []string{"批次号", "工单号", "物料代码", "产品描述", "SN", "发现工序", "问题现象", "外协单位", "记录人", "记录时间"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["batch_no"]),
			core.AsString(r["work_order"]),
			core.AsString(r["task_code"]),
			core.AsString(r["description"]),
			core.AsString(r["sn"]),
			core.AsString(r["process"]),
			core.AsString(r["issue_description"]),
			core.AsString(r["outsourced_unit"]),
			core.AsString(r["created_by_name"]),
			core.AsString(r["created_at"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("质量问题记录_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// GetPermissions GET /api/quality/get_permissions
func (ctrl *QualityController) GetPermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取权限失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, `
		SELECT u.id, u.id AS user_id, u.username, u.full_name, u.team,
		       COALESCE(p.can_view_records, 1) AS can_view_records,
		       COALESCE(p.can_input, 1) AS can_input
		FROM users u
		LEFT JOIN quality_permissions p ON p.user_id = u.id
		WHERE u.is_active = 1
		ORDER BY u.id ASC
	`)
	core.Success(c, "success", gin.H{"users": rows})
}

// UpdatePermission POST /api/quality/update_permission
func (ctrl *QualityController) UpdatePermission(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存权限失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	uid := core.AsInt(in["user_id"])
	_, _ = db.Exec(`
		INSERT INTO quality_permissions (user_id, can_view_records, can_input)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE can_view_records = VALUES(can_view_records), can_input = VALUES(can_input)
	`, uid, core.AsInt(in["can_view_records"]), core.AsInt(in["can_input"]))
	core.Success(c, "权限更新成功")
}

// ============================================================================
//  7. 问题排查记录系统（TroubleshootController）—— 1:1 对齐 troubleshoot/Index.vue
// ============================================================================

type TroubleshootController struct{}

func NewTroubleshootController() *TroubleshootController {
	return &TroubleshootController{}
}

// GetBatchInfo GET|POST /api/troubleshoot/get_batch_info
func (ctrl *TroubleshootController) GetBatchInfo(c *gin.Context) {
	NewImpedanceController().GetBatchInfo(c)
}

// List GET /api/troubleshoot/list
func (ctrl *TroubleshootController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取排查记录失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	kw := strings.TrimSpace(core.AsString(in["keyword"]))

	where := "is_deleted = 0"
	args := []any{}
	if kw != "" {
		where += " AND (work_order LIKE ? OR task_code LIKE ? OR sn LIKE ? OR issue_description LIKE ? OR operator_name LIKE ?)"
		k := "%" + kw + "%"
		args = append(args, k, k, k, k, k)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM troubleshoot_records WHERE "+where, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM troubleshoot_records WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", where),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
	})
}

// Add POST /api/troubleshoot/add
func (ctrl *TroubleshootController) Add(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	_, err = db.Exec(`
		INSERT INTO troubleshoot_records
		(work_order, task_code, description, sn, location, issue_description, conclusion, defect_phenomenon, operator_id, operator_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, strings.TrimSpace(core.AsString(in["work_order"])),
		strings.TrimSpace(core.AsString(in["task_code"])),
		strings.TrimSpace(core.AsString(in["description"])),
		strings.TrimSpace(core.AsString(in["sn"])),
		strings.TrimSpace(core.AsString(in["location"])),
		strings.TrimSpace(core.AsString(in["issue_description"])),
		strings.TrimSpace(core.AsString(in["conclusion"])),
		strings.TrimSpace(core.AsString(in["defect_phenomenon"])),
		sess.UserID, opName)
	if err != nil {
		core.Exception(c, err, "保存排查记录失败", gin.H{})
		return
	}
	core.Success(c, "排查记录已保存！")
}

// Delete POST /api/troubleshoot/delete
func (ctrl *TroubleshootController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("UPDATE troubleshoot_records SET is_deleted = 1 WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// GetPermissions GET /api/troubleshoot/get_permissions
func (ctrl *TroubleshootController) GetPermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取权限失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, `
		SELECT u.id, u.id AS user_id, u.username, u.full_name, u.team,
		       COALESCE(p.can_view_records, 1) AS can_view_records,
		       COALESCE(p.can_input, 1) AS can_input
		FROM users u
		LEFT JOIN module15_permissions p ON p.user_id = u.id
		WHERE u.is_active = 1
		ORDER BY u.id ASC
	`)
	core.Success(c, "success", gin.H{"users": rows})
}

// UpdatePermission POST /api/troubleshoot/update_permission
func (ctrl *TroubleshootController) UpdatePermission(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存权限失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	uid := core.AsInt(in["user_id"])
	_, _ = db.Exec(`
		INSERT INTO module15_permissions (user_id, can_view_records, can_input)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE can_view_records = VALUES(can_view_records), can_input = VALUES(can_input)
	`, uid, core.AsInt(in["can_view_records"]), core.AsInt(in["can_input"]))
	core.Success(c, "权限更新成功")
}

// Export GET /api/troubleshoot/export
func (ctrl *TroubleshootController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM troubleshoot_records WHERE is_deleted = 0 ORDER BY id DESC LIMIT 2000")
	headers := []string{"SN", "工单号", "产品编码", "产品描述", "排查地点", "问题说明", "结论", "不合格现象", "排查人", "时间"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["sn"]),
			core.AsString(r["work_order"]),
			core.AsString(r["task_code"]),
			core.AsString(r["description"]),
			core.AsString(r["location"]),
			core.AsString(r["issue_description"]),
			core.AsString(r["conclusion"]),
			core.AsString(r["defect_phenomenon"]),
			core.AsString(r["operator_name"]),
			core.AsString(r["created_at"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("问题排查记录_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// ============================================================================
//  8. 北边返厂统计模块（BeibianController）—— 1:1 对齐 beibian/Index.vue & module_records
// ============================================================================

type BeibianController struct{}

func NewBeibianController() *BeibianController {
	return &BeibianController{}
}

// List GET /api/beibian/list
func (ctrl *BeibianController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取返厂记录失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	searchAll := strings.TrimSpace(core.AsString(in["search_all"]))

	where := "1=1"
	args := []any{}
	if searchAll != "" {
		where = "(work_order LIKE ? OR description LIKE ? OR sn LIKE ? OR psn LIKE ? OR operator LIKE ?)"
		k := "%" + searchAll + "%"
		args = append(args, k, k, k, k, k)
	}
	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM module_records WHERE "+where, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM module_records WHERE %s ORDER BY return_time DESC, id DESC LIMIT ? OFFSET ?", where),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
	})
}

// SearchSn GET|POST /api/beibian/search_sn
func (ctrl *BeibianController) SearchSn(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	snVal := strings.TrimSpace(core.AsString(in["sn"]))
	meta := core.LookupWorkOrderInfo(db, snVal)
	if meta == nil {
		core.Success(c, "success", gin.H{"found": false})
		return
	}
	core.Success(c, "success", gin.H{
		"found":       true,
		"work_order":  meta.WorkOrder,
		"description": meta.ProductDesc,
	})
}

// SearchOrder GET|POST /api/beibian/search_order
func (ctrl *BeibianController) SearchOrder(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	meta := core.LookupWorkOrderInfo(db, wo)
	if meta == nil {
		core.Success(c, "success", gin.H{"found": false})
		return
	}
	core.Success(c, "success", gin.H{
		"found":       true,
		"work_order":  meta.WorkOrder,
		"description": meta.ProductDesc,
	})
}

// Add POST /api/beibian/add
func (ctrl *BeibianController) Add(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	var actRet any = strings.TrimSpace(core.AsString(in["actual_return_date"]))
	if actRet == "" {
		actRet = nil
	}
	res, err := db.Exec(`
		INSERT INTO module_records (work_order, description, sn, psn, return_time, is_returned, actual_return_date, operator, remarks, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, strings.TrimSpace(core.AsString(in["work_order"])),
		strings.TrimSpace(core.AsString(in["description"])),
		strings.TrimSpace(core.AsString(in["sn"])),
		strings.TrimSpace(core.AsString(in["psn"])),
		strings.TrimSpace(core.AsString(in["return_time"])),
		strings.TrimSpace(core.AsString(in["is_returned"])),
		actRet,
		strings.TrimSpace(core.AsString(in["operator"])),
		strings.TrimSpace(core.AsString(in["remarks"])))
	if err != nil {
		core.Exception(c, err, "保存返厂记录失败", gin.H{})
		return
	}
	newID, _ := res.LastInsertId()
	core.Success(c, "保存成功", gin.H{"id": newID})
}

// Update POST /api/beibian/update
func (ctrl *BeibianController) Update(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	var actRet any = strings.TrimSpace(core.AsString(in["actual_return_date"]))
	if actRet == "" {
		actRet = nil
	}
	_, err = db.Exec(`
		UPDATE module_records
		SET work_order = ?, description = ?, sn = ?, psn = ?, return_time = ?,
		    is_returned = ?, actual_return_date = ?, operator = ?, remarks = ?
		WHERE id = ?
	`, strings.TrimSpace(core.AsString(in["work_order"])),
		strings.TrimSpace(core.AsString(in["description"])),
		strings.TrimSpace(core.AsString(in["sn"])),
		strings.TrimSpace(core.AsString(in["psn"])),
		strings.TrimSpace(core.AsString(in["return_time"])),
		strings.TrimSpace(core.AsString(in["is_returned"])),
		actRet,
		strings.TrimSpace(core.AsString(in["operator"])),
		strings.TrimSpace(core.AsString(in["remarks"])), id)
	if err != nil {
		core.Exception(c, err, "更新失败", gin.H{})
		return
	}
	core.Success(c, "保存成功")
}

// Detail GET /api/beibian/detail
func (ctrl *BeibianController) Detail(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取详情失败", gin.H{})
		return
	}
	id := core.AsInt(c.Query("id"))
	row, _ := core.QueryRowMap(db, "SELECT * FROM module_records WHERE id = ? LIMIT 1", id)
	core.Success(c, "success", gin.H{"record": row, "data": row})
}

// Delete POST /api/beibian/delete
func (ctrl *BeibianController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("DELETE FROM module_records WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Export GET /api/beibian/export
func (ctrl *BeibianController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM module_records ORDER BY return_time DESC, id DESC LIMIT 2000")
	headers := []string{"工单号", "物料描述", "SN", "PSN", "返回北变时间", "是否回厂", "实际回厂日期", "登记人", "备注"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		csvRows = append(csvRows, []string{
			core.AsString(r["work_order"]),
			core.AsString(r["description"]),
			core.AsString(r["sn"]),
			core.AsString(r["psn"]),
			core.AsString(r["return_time"]),
			core.AsString(r["is_returned"]),
			core.AsString(r["actual_return_date"]),
			core.AsString(r["operator"]),
			core.AsString(r["remarks"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("北边返厂统计_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// ============================================================================
//  9. 文件错误记录模块（DocErrorController）—— 1:1 对齐 doc_error/Index.vue
// ============================================================================

type DocErrorController struct{}

func NewDocErrorController() *DocErrorController {
	return &DocErrorController{}
}

func (ctrl *DocErrorController) getPerm(db *sql.DB, user map[string]any) map[string]int {
	if core.IsAdminUser(user) {
		return map[string]int{"can_input": 1, "can_edit": 1, "can_delete": 1, "can_export": 1, "can_manage": 1, "is_admin": 1}
	}
	row, _ := core.QueryRowMap(db, "SELECT * FROM doc_error_permissions WHERE user_id = ? LIMIT 1", core.AsInt(user["id"]))
	if row == nil {
		return map[string]int{"can_input": 1, "can_edit": 0, "can_delete": 0, "can_export": 1, "can_manage": 0, "is_admin": 0}
	}
	return map[string]int{
		"can_input":  core.AsInt(row["can_input"]),
		"can_edit":   core.AsInt(row["can_edit"]),
		"can_delete": core.AsInt(row["can_delete"]),
		"can_export": core.AsInt(row["can_export"]),
		"can_manage": core.AsInt(row["can_manage"]),
		"is_admin":   0,
	}
}

// Init GET /api/doc_error/init
func (ctrl *DocErrorController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "初始化失败", gin.H{})
		return
	}
	perm := ctrl.getPerm(db, sess.User)
	core.Success(c, "success", gin.H{
		"is_admin":  core.IsAdminUser(sess.User),
		"processes": []string{"装置装配", "装置调试", "装置包装"},
		"perm":      perm,
	})
}

// GetMyPerm GET /api/doc_error/get_my_perm
func (ctrl *DocErrorController) GetMyPerm(c *gin.Context) {
	ctrl.Init(c)
}

// GetWorkOrder GET|POST /api/doc_error/get_work_order
func (ctrl *DocErrorController) GetWorkOrder(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	wo := strings.TrimSpace(core.AsString(in["work_order"]))
	meta := core.LookupWorkOrderInfo(db, wo)
	if meta == nil {
		core.Success(c, "success", gin.H{"found": false})
		return
	}
	core.Success(c, "success", gin.H{
		"found":       true,
		"work_order":  meta.WorkOrder,
		"code":        meta.ProductCode,
		"description": meta.ProductDesc,
	})
}

// List GET|POST /api/doc_error/list
func (ctrl *DocErrorController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取列表失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	kw := strings.TrimSpace(core.AsString(in["keyword"]))
	isMod := strings.TrimSpace(core.AsString(in["is_modified"]))

	where := []string{"is_deleted = 0"}
	args := []any{}
	if kw != "" {
		where = append(where, "(work_order LIKE ? OR task_code LIKE ? OR description LIKE ? OR file_error_desc LIKE ? OR discoverer_name LIKE ?)")
		k := "%" + kw + "%"
		args = append(args, k, k, k, k, k)
	}
	if isMod == "0" || isMod == "1" {
		where = append(where, "is_modified = ?")
		args = append(args, core.AsInt(isMod))
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM doc_error_records WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM doc_error_records WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", whereSQL),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"list":          rows,
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
		"perm":          ctrl.getPerm(db, sess.User),
	})
}

// Get GET|POST /api/doc_error/get
func (ctrl *DocErrorController) Get(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "查询失败", gin.H{})
		return
	}
	id := core.AsInt(core.ParseInputMap(c)["id"])
	row, _ := core.QueryRowMap(db, "SELECT * FROM doc_error_records WHERE id = ? LIMIT 1", id)
	core.Success(c, "success", row)
}

// Add POST /api/doc_error/add
func (ctrl *DocErrorController) Add(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	_, err = db.Exec(`
		INSERT INTO doc_error_records
		(work_order, task_code, description, file_error_desc, discovery_process, discovery_time, discoverer_id, discoverer_name, remark, is_modified, is_deleted, created_at)
		VALUES (?, ?, ?, ?, ?, NOW(), ?, ?, ?, 0, 0, NOW())
	`, strings.TrimSpace(core.AsString(in["work_order"])),
		strings.TrimSpace(core.AsString(in["task_code"])),
		strings.TrimSpace(core.AsString(in["description"])),
		strings.TrimSpace(core.AsString(in["file_error_desc"])),
		strings.TrimSpace(core.AsString(in["discovery_process"])),
		sess.UserID, opName,
		strings.TrimSpace(core.AsString(in["remark"])))
	if err != nil {
		core.Exception(c, err, "提交失败", gin.H{})
		return
	}
	core.Success(c, "提交成功")
}

// Update POST /api/doc_error/update
func (ctrl *DocErrorController) Update(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "修改失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	id := core.AsInt(in["id"])
	isMod := core.AsInt(in["is_modified"])
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	_, err = db.Exec(`
		UPDATE doc_error_records
		SET file_error_desc = ?, discovery_process = ?, is_modified = ?, remark = ?,
		    modify_time = IF(? = 1, NOW(), modify_time),
		    modifier_id = IF(? = 1, ?, modifier_id),
		    modifier_name = IF(? = 1, ?, modifier_name)
		WHERE id = ?
	`, strings.TrimSpace(core.AsString(in["file_error_desc"])),
		strings.TrimSpace(core.AsString(in["discovery_process"])),
		isMod, strings.TrimSpace(core.AsString(in["remark"])),
		isMod, isMod, sess.UserID, isMod, opName, id)
	if err != nil {
		core.Exception(c, err, "修改失败", gin.H{})
		return
	}
	core.Success(c, "记录修改成功")
}

// Delete POST /api/doc_error/delete
func (ctrl *DocErrorController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("UPDATE doc_error_records SET is_deleted = 1 WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}

// Export GET /api/doc_error/export
func (ctrl *DocErrorController) Export(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "导出失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, "SELECT * FROM doc_error_records WHERE is_deleted = 0 ORDER BY id DESC LIMIT 2000")
	headers := []string{"工单号", "物料代码", "产品描述", "发现工序", "文件问题描述", "发现人", "发现时间", "是否已修改", "修改人", "备注"}
	csvRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		modStr := "未修改"
		if core.AsInt(r["is_modified"]) == 1 {
			modStr = "已修改"
		}
		csvRows = append(csvRows, []string{
			core.AsString(r["work_order"]),
			core.AsString(r["task_code"]),
			core.AsString(r["description"]),
			core.AsString(r["discovery_process"]),
			core.AsString(r["file_error_desc"]),
			core.AsString(r["discoverer_name"]),
			core.AsString(r["discovery_time"]),
			modStr,
			core.AsString(r["modifier_name"]),
			core.AsString(r["remark"]),
		})
	}
	core.ExportCSV(c, fmt.Sprintf("文件错误记录_%s.csv", time.Now().Format("20060102_150405")), headers, csvRows)
}

// GetPermissions GET /api/doc_error/get_permissions
func (ctrl *DocErrorController) GetPermissions(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取权限失败", gin.H{})
		return
	}
	rows, _ := core.QueryMaps(db, `
		SELECT u.id, u.id AS user_id, u.username, u.full_name, u.team,
		       COALESCE(p.can_input, 1) AS can_input,
		       COALESCE(p.can_edit, 0) AS can_edit,
		       COALESCE(p.can_delete, 0) AS can_delete,
		       COALESCE(p.can_export, 1) AS can_export,
		       COALESCE(p.can_manage, 0) AS can_manage
		FROM users u
		LEFT JOIN doc_error_permissions p ON p.user_id = u.id
		WHERE u.is_active = 1
		ORDER BY u.id ASC
	`)
	core.Success(c, "success", gin.H{"users": rows})
}

// UpdatePermission POST /api/doc_error/update_permission
func (ctrl *DocErrorController) UpdatePermission(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "保存权限失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	uid := core.AsInt(in["user_id"])
	_, _ = db.Exec(`
		INSERT INTO doc_error_permissions (user_id, can_input, can_edit, can_delete, can_export, can_manage, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE
			can_input = VALUES(can_input),
			can_edit = VALUES(can_edit),
			can_delete = VALUES(can_delete),
			can_export = VALUES(can_export),
			can_manage = VALUES(can_manage),
			updated_at = NOW()
	`, uid, core.AsInt(in["can_input"]), core.AsInt(in["can_edit"]), core.AsInt(in["can_delete"]), core.AsInt(in["can_export"]), core.AsInt(in["can_manage"]))
	core.Success(c, "权限更新成功")
}

// ============================================================================
//  10. 作业指导文件管理模块（ZywjController）—— 1:1 对齐 zywj/Index.vue & zywj_documents
// ============================================================================

type ZywjController struct{}

func NewZywjController() *ZywjController {
	return &ZywjController{}
}

// Init GET /api/zywj/init
func (ctrl *ZywjController) Init(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	core.Success(c, "success", gin.H{
		"categories": []string{"装配作业指导", "调试作业指导", "包装作业指导", "通用指导"},
		"is_admin":   core.IsAdminUser(sess.User),
	})
}

// List GET /api/zywj/list
func (ctrl *ZywjController) List(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "获取列表失败", gin.H{})
		return
	}
	in := core.ParseInputMap(c)
	page := core.AsInt(in["page"])
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	cat := strings.TrimSpace(core.AsString(in["category"]))
	search := strings.TrimSpace(core.AsString(in["search"]))

	where := []string{"1=1"}
	args := []any{}
	if cat != "" {
		where = append(where, "category = ?")
		args = append(args, cat)
	}
	if search != "" {
		where = append(where, "(doc_no LIKE ? OR title LIKE ? OR product_code LIKE ? OR process_name LIKE ?)")
		k := "%" + search + "%"
		args = append(args, k, k, k, k)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	_ = db.QueryRow("SELECT COUNT(*) FROM zywj_documents WHERE "+whereSQL, args...).Scan(&total)
	rows, _ := core.QueryMaps(db, fmt.Sprintf("SELECT * FROM zywj_documents WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", whereSQL),
		append(append([]any{}, args...), limit, offset)...)

	core.Success(c, "success", gin.H{
		"records":       rows,
		"total_records": total,
		"total_pages":   core.CalcTotalPages(total, limit),
		"is_admin":      core.IsAdminUser(sess.User),
	})
}

// Upload POST /api/zywj/upload
func (ctrl *ZywjController) Upload(c *gin.Context) {
	sess := core.GetOrCreateSession(c)
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "上传失败", gin.H{})
		return
	}
	fileHeader, fErr := c.FormFile("file")
	if fErr != nil {
		core.Error(c, "请选择需要上传的指导书文件！", gin.H{})
		return
	}
	title := strings.TrimSpace(c.PostForm("title"))
	docNo := strings.TrimSpace(c.PostForm("doc_no"))
	if docNo == "" {
		docNo = "SOP-" + time.Now().Format("200601021504")
	}
	cat := strings.TrimSpace(c.PostForm("category"))
	if cat == "" {
		cat = "装配作业指导"
	}
	version := strings.TrimSpace(c.PostForm("version"))
	if version == "" {
		version = "V1.0"
	}
	prodCode := strings.TrimSpace(c.PostForm("product_code"))
	procName := strings.TrimSpace(c.PostForm("process_name"))
	remarks := strings.TrimSpace(c.PostForm("remarks"))

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileHeader.Filename), "."))
	pubRoot := config.Get("PUBLIC_ROOT", "./public")
	targetDir := filepath.Join(pubRoot, "uploads", "zywj")
	_ = os.MkdirAll(targetDir, 0755)
	storedName := fmt.Sprintf("sop_%s.%s", core.RandomHex(6), ext)
	if err := c.SaveUploadedFile(fileHeader, filepath.Join(targetDir, storedName)); err != nil {
		core.Exception(c, err, "保存文件失败", gin.H{})
		return
	}
	webPath := "/uploads/zywj/" + storedName
	opName := core.AsString(sess.User["full_name"])
	if opName == "" {
		opName = sess.Username
	}
	_, err = db.Exec(`
		INSERT INTO zywj_documents
		(doc_no, title, category, product_code, process_name, version, file_path, file_size, file_type, remarks, uploader_id, uploader_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
	`, docNo, title, cat, prodCode, procName, version, webPath, fileHeader.Size, ext, remarks, sess.UserID, opName)
	if err != nil {
		core.Exception(c, err, "写入数据库失败", gin.H{})
		return
	}
	core.Success(c, "上传成功！")
}

// Delete POST /api/zywj/delete
func (ctrl *ZywjController) Delete(c *gin.Context) {
	db, err := core.GetDB()
	if err != nil {
		core.Exception(c, err, "删除失败", gin.H{})
		return
	}
	for _, id := range core.ParseIDs(core.ParseInputMap(c)) {
		_, _ = db.Exec("DELETE FROM zywj_documents WHERE id = ?", id)
	}
	core.Success(c, "删除成功")
}
func gbkToUtf8IfNeeded(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		if b[i] < 0x80 {
			sb.WriteByte(b[i])
			i++
		} else if i+1 < len(b) {
			r, size := utf8.DecodeRune(b[i:])
			if r != utf8.RuneError {
				sb.WriteRune(r)
				i += size
			} else {
				sb.WriteByte(b[i])
				i++
			}
		} else {
			sb.WriteByte(b[i])
			i++
		}
	}
	return sb.String()
}

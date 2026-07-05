package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	xhtml "golang.org/x/net/html"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const (
	defaultBaseURL    = "https://jpxkc.cbex.com"
	defaultListingURL = "https://jpxkc.cbex.com/page/jpxkc/zt/list"
	defaultFrom       = "onboarding@resend.dev"
	defaultFromName   = "CBEX Cron"
)

type Config struct {
	Location      *time.Location
	ScheduleTime  string
	RunOnStart    bool
	BaseURL       string
	ListingURL    string
	Timeout       time.Duration
	MaxBatches    int
	WeComCorpID   string
	WeComSecret   string
	WeComAgentID  int
	WeComToUser   string
	EmailEnabled  bool
	EmailTo       []string
	EmailFrom     string
	EmailFromName string
	ResendAPIKey  string
	ResendBaseURL string
}

type AssetBatch struct {
	Title                string
	DetailURL            string
	RegistrationDeadline string
	AuctionStartTime     string
	AssetCount           int
	ViewCountText        string
	Projects             []AssetPreview
}

type AssetPreview struct {
	DetailURL string
	ImageURL  string
	PriceText string
}

type FetchResult struct {
	FetchedAt time.Time
	Batches   []AssetBatch
}

type Report struct {
	Title    string
	Markdown string
	HTML     string
}

type WeComClient struct {
	http        *http.Client
	corpID      string
	secret      string
	agentID     int
	toUser      string
	accessToken string
	tokenUntil  time.Time
}

var (
	reAssetCount = regexp.MustCompile(`共\s*(\d+)\s*件`)
	reViewCount  = regexp.MustCompile(`件标的\s*([\d.]+万?)\s*次围观`)
	reWhitespace = regexp.MustCompile(`\s+`)
	rePeriod     = regexp.MustCompile(`第(\d+)期`)
)

func main() {
	loadEnvFiles(".env", filepath.Join("jobs", ".env"))

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("cron-cbex started; schedule=%s Asia/Shanghai run_on_start=%v", cfg.ScheduleTime, cfg.RunOnStart)
	if cfg.RunOnStart {
		runOnce(ctx, cfg)
	}

	for {
		next := nextRun(time.Now().In(cfg.Location), cfg.ScheduleTime, cfg.Location)
		log.Printf("next run at %s", next.Format("2006-01-02 15:04:05 MST"))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Print("shutdown complete")
			return
		case <-timer.C:
			runOnce(ctx, cfg)
		}
	}
}

func loadConfig() (*Config, error) {
	loc, err := time.LoadLocation(env("TZ", "Asia/Shanghai"))
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	timeout, err := time.ParseDuration(env("CBEX_TIMEOUT", "30s"))
	if err != nil {
		return nil, fmt.Errorf("CBEX_TIMEOUT: %w", err)
	}
	agentID, err := strconv.Atoi(strings.TrimSpace(os.Getenv("WECOM_AGENT_ID")))
	if err != nil || agentID <= 0 {
		return nil, errors.New("WECOM_AGENT_ID is required and must be a positive integer")
	}
	maxBatches := envInt("CBEX_MAX_BATCHES", 3)
	resendKey := firstNonEmpty(os.Getenv("RESEND_API_KEY"), readSecretFile(env("RESEND_API_KEY_FILE", filepath.Join("secrets", "resend_api_key"))))
	emailEnabled := envBool("EMAIL_ENABLED", resendKey != "")
	cfg := &Config{
		Location:      loc,
		ScheduleTime:  env("SCHEDULE_TIME", "09:30"),
		RunOnStart:    envBool("RUN_ON_START", false),
		BaseURL:       env("CBEX_BASE_URL", defaultBaseURL),
		ListingURL:    env("CBEX_LISTING_URL", defaultListingURL),
		Timeout:       timeout,
		MaxBatches:    maxBatches,
		WeComCorpID:   strings.TrimSpace(os.Getenv("WECOM_CORP_ID")),
		WeComSecret:   strings.TrimSpace(os.Getenv("WECOM_CORP_SECRET")),
		WeComAgentID:  agentID,
		WeComToUser:   strings.TrimSpace(os.Getenv("WECOM_TOUSER")),
		EmailEnabled:  emailEnabled,
		EmailTo:       splitList(os.Getenv("EMAIL_TO")),
		EmailFrom:     env("MAIL_DEFAULT_FROM", defaultFrom),
		EmailFromName: env("MAIL_DEFAULT_FROM_NAME", defaultFromName),
		ResendAPIKey:  resendKey,
		ResendBaseURL: env("RESEND_BASE_URL", "https://api.resend.com"),
	}
	if cfg.WeComCorpID == "" {
		return nil, errors.New("WECOM_CORP_ID is required")
	}
	if cfg.WeComSecret == "" {
		return nil, errors.New("WECOM_CORP_SECRET is required")
	}
	if cfg.WeComToUser == "" {
		return nil, errors.New("WECOM_TOUSER is required")
	}
	if _, err := parseClock(cfg.ScheduleTime); err != nil {
		return nil, err
	}
	if cfg.EmailEnabled && cfg.ResendAPIKey == "" {
		return nil, errors.New("EMAIL_ENABLED=true requires RESEND_API_KEY or RESEND_API_KEY_FILE")
	}
	if cfg.EmailEnabled && len(cfg.EmailTo) == 0 {
		return nil, errors.New("EMAIL_ENABLED=true requires EMAIL_TO")
	}
	return cfg, nil
}

func runOnce(ctx context.Context, cfg *Config) {
	start := time.Now()
	log.Print("fetching CBEX listing")
	result, err := fetchCBEX(ctx, cfg)
	if err != nil {
		log.Printf("fetch failed: %v", err)
		return
	}
	report := buildReport(result, cfg)
	log.Printf("fetched %d batches; sending notifications", len(result.Batches))

	wc := &WeComClient{
		http:    &http.Client{Timeout: cfg.Timeout},
		corpID:  cfg.WeComCorpID,
		secret:  cfg.WeComSecret,
		agentID: cfg.WeComAgentID,
		toUser:  cfg.WeComToUser,
	}
	if err := wc.SendMarkdown(ctx, report.Markdown); err != nil {
		log.Printf("wecom send failed: %v", err)
	} else {
		log.Print("wecom send ok")
	}
	if cfg.EmailEnabled {
		if err := sendResendEmail(ctx, cfg, report); err != nil {
			log.Printf("email send failed: %v", err)
		} else {
			log.Print("email send ok")
		}
	}
	log.Printf("run finished in %s", time.Since(start).Round(time.Millisecond))
}

func fetchCBEX(ctx context.Context, cfg *Config) (*FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ListingURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cron-cbex/1.0")
	resp, err := (&http.Client{Timeout: cfg.Timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("CBEX returned HTTP %d", resp.StatusCode)
	}
	body, err := decodeResponse(resp)
	if err != nil {
		return nil, err
	}
	batches, err := parseListing(body, cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.MaxBatches > 0 && len(batches) > cfg.MaxBatches {
		batches = batches[:cfg.MaxBatches]
	}
	return &FetchResult{FetchedAt: time.Now().In(cfg.Location), Batches: batches}, nil
}

func decodeResponse(resp *http.Response) (string, error) {
	reader := io.Reader(resp.Body)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "gb") {
		reader = transform.NewReader(resp.Body, simplifiedchinese.GB18030.NewDecoder())
	}
	b, err := io.ReadAll(io.LimitReader(reader, 4<<20))
	return string(b), err
}

func parseListing(htmlBody, baseURL string) ([]AssetBatch, error) {
	root, err := xhtml.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return nil, err
	}
	var items []AssetBatch
	walkNodes(root, func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "div" && hasClass(n, "zclist_cont") {
			items = append(items, parseBatch(n, baseURL))
		}
	})
	return items, nil
}

func parseBatch(n *xhtml.Node, baseURL string) AssetBatch {
	var b AssetBatch
	walkChildren(n, func(child *xhtml.Node) {
		if child.Type != xhtml.ElementNode {
			return
		}
		switch {
		case child.Data == "p" && hasClass(child, "zclist_title"):
			b.Title = normalizeText(textContent(child))
			if a := firstNode(child, "a"); a != nil {
				b.DetailURL = resolveURL(baseURL, getAttr(a, "href"))
			}
		case child.Data == "p" && hasClass(child, "zclist_period"):
			b.RegistrationDeadline = afterColon(normalizeText(textContent(child)))
		case child.Data == "p" && hasClass(child, "zclist_time"):
			b.AuctionStartTime = afterColon(normalizeText(textContent(child)))
		case child.Data == "p" && hasClass(child, "zclist_data"):
			dataText := normalizeText(textContent(child))
			b.AssetCount = matchInt(reAssetCount, dataText)
			b.ViewCountText = matchStr(reViewCount, dataText)
		case child.Data == "div" && hasClass(child, "zclist_bd_pic"):
			b.Projects = parseProjects(child, baseURL)
		}
	})
	return b
}

func parseProjects(n *xhtml.Node, baseURL string) []AssetPreview {
	var projects []AssetPreview
	walkChildren(n, func(child *xhtml.Node) {
		if child.Type != xhtml.ElementNode || child.Data != "li" {
			return
		}
		var p AssetPreview
		if a := firstNode(child, "a"); a != nil {
			p.DetailURL = resolveURL(baseURL, getAttr(a, "href"))
		}
		if img := firstNode(child, "img"); img != nil {
			p.ImageURL = resolveURL(baseURL, firstNonEmpty(getAttr(img, "data-original"), getAttr(img, "src")))
		}
		if title := firstNodeByClass(child, "p", "zclist_bd_title"); title != nil {
			p.PriceText = normalizeText(textContent(title))
		}
		if p != (AssetPreview{}) {
			projects = append(projects, p)
		}
	})
	return projects
}

func buildReport(result *FetchResult, cfg *Config) Report {
	nowStr := result.FetchedAt.In(cfg.Location).Format("2006-01-02 15:04")
	today := startOfDay(result.FetchedAt.In(cfg.Location))
	var current, ended []AssetBatch
	for _, b := range result.Batches {
		if b.RegistrationDeadline != "" && isAfterToday(b.RegistrationDeadline, today, cfg.Location) {
			current = append(current, b)
		} else {
			ended = append(ended, b)
		}
	}
	var prev *AssetBatch
	if len(ended) > 0 {
		prev = &ended[0]
	}

	title := "CBEx 京牌小客车司法处置日报"
	var md strings.Builder
	md.WriteString(fmt.Sprintf("**%s**\n", title))
	md.WriteString(fmt.Sprintf("> %s\n\n", nowStr))
	if len(current) > 0 {
		md.WriteString("### 当前可参与\n")
		for _, b := range current {
			writeBatchMarkdown(&md, b, true)
		}
	} else {
		md.WriteString("### 当前无可参与批次\n\n")
	}
	if prev != nil {
		md.WriteString(fmt.Sprintf("### 上一期：%s\n", extractPeriod(prev.Title)))
		writeBatchMarkdown(&md, *prev, false)
	}
	return Report{Title: title, Markdown: md.String(), HTML: buildHTML(title, nowStr, current, prev)}
}

func writeBatchMarkdown(md *strings.Builder, b AssetBatch, includeMarker bool) {
	period := extractPeriod(b.Title)
	marker := ""
	if includeMarker {
		marker = " [当前]"
	}
	md.WriteString(fmt.Sprintf("**%s**%s\n", period, marker))
	if b.DetailURL != "" {
		md.WriteString(fmt.Sprintf("[查看详情](%s)\n", b.DetailURL))
	}
	if b.RegistrationDeadline != "" {
		md.WriteString(fmt.Sprintf("- 报名截止：%s\n", b.RegistrationDeadline))
	}
	if b.AuctionStartTime != "" {
		md.WriteString(fmt.Sprintf("- 竞价开始：%s\n", b.AuctionStartTime))
	}
	if b.AssetCount > 0 {
		md.WriteString(fmt.Sprintf("- %d件标的\n", b.AssetCount))
	}
	if b.ViewCountText != "" {
		md.WriteString(fmt.Sprintf("- 围观：%s次\n", b.ViewCountText))
	}
	if len(b.Projects) > 0 {
		sort.Slice(b.Projects, func(i, j int) bool {
			return extractPriceValue(b.Projects[i].PriceText) < extractPriceValue(b.Projects[j].PriceText)
		})
		for i, p := range b.Projects {
			if i >= 5 {
				md.WriteString(fmt.Sprintf("> ...共%d辆车\n", len(b.Projects)))
				break
			}
			price := strings.TrimSpace(p.PriceText)
			if price == "" {
				price = "暂无报价"
			}
			md.WriteString("- " + price)
			if p.DetailURL != "" {
				md.WriteString(fmt.Sprintf(" [查看](%s)", p.DetailURL))
			}
			md.WriteString("\n")
		}
	}
	md.WriteString("\n")
}

func buildHTML(title, nowStr string, current []AssetBatch, prev *AssetBatch) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><style>body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;max-width:760px;margin:0 auto;padding:20px;color:#222}h1{font-size:22px}h2{font-size:18px;margin-top:24px}.meta{color:#666}.batch{border-top:1px solid #ddd;padding:12px 0}.label{font-weight:600}a{color:#0b66c3}</style></head><body>`)
	b.WriteString("<h1>" + html.EscapeString(title) + "</h1>")
	b.WriteString(`<p class="meta">` + html.EscapeString(nowStr) + `</p>`)
	if len(current) == 0 {
		b.WriteString("<h2>当前无可参与批次</h2>")
	} else {
		b.WriteString("<h2>当前可参与</h2>")
		for _, item := range current {
			writeBatchHTML(&b, item)
		}
	}
	if prev != nil {
		b.WriteString("<h2>上一期</h2>")
		writeBatchHTML(&b, *prev)
	}
	b.WriteString("</body></html>")
	return b.String()
}

func writeBatchHTML(b *strings.Builder, item AssetBatch) {
	b.WriteString(`<div class="batch">`)
	if item.DetailURL != "" {
		b.WriteString(`<p class="label"><a href="` + html.EscapeString(item.DetailURL) + `">` + html.EscapeString(item.Title) + `</a></p>`)
	} else {
		b.WriteString(`<p class="label">` + html.EscapeString(item.Title) + `</p>`)
	}
	if item.RegistrationDeadline != "" {
		b.WriteString("<p>报名截止：" + html.EscapeString(item.RegistrationDeadline) + "</p>")
	}
	if item.AuctionStartTime != "" {
		b.WriteString("<p>竞价开始：" + html.EscapeString(item.AuctionStartTime) + "</p>")
	}
	if item.AssetCount > 0 {
		b.WriteString(fmt.Sprintf("<p>%d件标的</p>", item.AssetCount))
	}
	b.WriteString(`</div>`)
}

func (c *WeComClient) SendMarkdown(ctx context.Context, markdown string) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"touser":  c.toUser,
		"msgtype": "markdown",
		"agentid": c.agentID,
		"markdown": map[string]string{
			"content": markdown,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u := "https://qyapi.weixin.qq.com/cgi-bin/message/send?access_token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var decoded struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		MsgID   string `json:"msgid"`
	}
	if err := decodeJSON(resp.Body, &decoded); err != nil {
		return err
	}
	if decoded.ErrCode != 0 {
		return fmt.Errorf("wecom send error %d: %s", decoded.ErrCode, decoded.ErrMsg)
	}
	return nil
}

func (c *WeComClient) token(ctx context.Context) (string, error) {
	if c.accessToken != "" && time.Now().Before(c.tokenUntil) {
		return c.accessToken, nil
	}
	u := "https://qyapi.weixin.qq.com/cgi-bin/gettoken?corpid=" + url.QueryEscape(c.corpID) + "&corpsecret=" + url.QueryEscape(c.secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var decoded struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decodeJSON(resp.Body, &decoded); err != nil {
		return "", err
	}
	if decoded.ErrCode != 0 {
		return "", fmt.Errorf("wecom token error %d: %s", decoded.ErrCode, decoded.ErrMsg)
	}
	c.accessToken = decoded.AccessToken
	c.tokenUntil = time.Now().Add(time.Duration(decoded.ExpiresIn-300) * time.Second)
	return c.accessToken, nil
}

func sendResendEmail(ctx context.Context, cfg *Config, report Report) error {
	from := cfg.EmailFrom
	if cfg.EmailFromName != "" {
		from = fmt.Sprintf("%s <%s>", cfg.EmailFromName, cfg.EmailFrom)
	}
	payload := map[string]any{
		"from":    from,
		"to":      cfg.EmailTo,
		"subject": report.Title,
		"html":    report.HTML,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.ResendBaseURL, "/")+"/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.ResendAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: cfg.Timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("resend returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func loadEnvFiles(paths ...string) {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		func() {
			defer f.Close()
			body, _ := io.ReadAll(f)
			for _, line := range strings.Split(string(body), "\n") {
				key, val, ok := parseEnvLine(line)
				if !ok {
					continue
				}
				_ = os.Setenv(key, val)
			}
		}()
	}
}

func parseEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimSpace(parts[0])
	val := strings.TrimSpace(parts[1])
	if i := strings.Index(val, " #"); i >= 0 {
		val = strings.TrimSpace(val[:i])
	}
	val = strings.Trim(val, `"'`)
	return key, val, key != ""
}

func nextRun(now time.Time, clock string, loc *time.Location) time.Time {
	tod, _ := parseClock(clock)
	next := time.Date(now.Year(), now.Month(), now.Day(), tod.Hour(), tod.Minute(), 0, 0, loc)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func parseClock(clock string) (time.Time, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(clock))
	if err != nil {
		return time.Time{}, fmt.Errorf("SCHEDULE_TIME must be HH:MM, got %q", clock)
	}
	return t, nil
}

func decodeJSON(r io.Reader, v any) error {
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode json: %w: %s", err, strings.TrimSpace(string(body)))
	}
	return nil
}

func walkNodes(n *xhtml.Node, fn func(*xhtml.Node)) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		fn(c)
		walkNodes(c, fn)
	}
}

func walkChildren(n *xhtml.Node, fn func(*xhtml.Node)) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		fn(c)
	}
}

func firstNode(n *xhtml.Node, tag string) *xhtml.Node {
	var found *xhtml.Node
	walkNodes(n, func(child *xhtml.Node) {
		if found == nil && child.Type == xhtml.ElementNode && child.Data == tag {
			found = child
		}
	})
	return found
}

func firstNodeByClass(n *xhtml.Node, tag, class string) *xhtml.Node {
	var found *xhtml.Node
	walkNodes(n, func(child *xhtml.Node) {
		if found == nil && child.Type == xhtml.ElementNode && child.Data == tag && hasClass(child, class) {
			found = child
		}
	})
	return found
}

func hasClass(n *xhtml.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key != "class" {
			continue
		}
		for _, part := range strings.Fields(a.Val) {
			if part == class {
				return true
			}
		}
	}
	return false
}

func getAttr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textContent(n *xhtml.Node) string {
	var b strings.Builder
	walkNodes(n, func(child *xhtml.Node) {
		if child.Type == xhtml.TextNode {
			b.WriteString(child.Data)
			b.WriteByte(' ')
		}
	})
	return b.String()
}

func normalizeText(value string) string {
	return strings.TrimSpace(reWhitespace.ReplaceAllString(strings.ReplaceAll(value, "　", " "), " "))
}

func afterColon(value string) string {
	parts := regexp.MustCompile(`[:：]`).Split(value, 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(value)
}

func matchInt(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func matchStr(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func resolveURL(baseURL, ref string) string {
	if ref == "" {
		return ""
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}

func extractPeriod(title string) string {
	m := rePeriod.FindStringSubmatch(title)
	if len(m) >= 2 {
		return m[1] + "期"
	}
	return title
}

func extractPriceValue(priceText string) float64 {
	s := strings.TrimSpace(priceText)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "¥"), "￥")
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "万", "0000")
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}

func isAfterToday(deadline string, today time.Time, loc *time.Location) bool {
	formats := []string{"2006年01月02日 15:04", "2006年01月02日", "2006-01-02 15:04", "2006-01-02", "2006/01/02 15:04", "2006/01/02"}
	for _, f := range formats {
		if t, err := time.ParseInLocation(f, deadline, loc); err == nil {
			return t.After(today)
		}
	}
	return false
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func splitList(v string) []string {
	var result []string
	for _, part := range regexp.MustCompile(`[,\s;]+`).Split(v, -1) {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func readSecretFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

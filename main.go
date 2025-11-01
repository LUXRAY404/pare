// parafinder_ultra_advanced.go
package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/briandowns/spinner"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
)

// ------------------ Types ------------------
type Result struct {
	Domain       string            `json:"domain"`
	URLs         []string          `json:"urls"`
	Parameters   map[string]int    `json:"parameters"`
	HighValue    []string          `json:"high_value"`
	ParamScores  map[string]string `json:"param_scores"`
	ScanDuration string            `json:"scan_duration"`
	Timestamp    string            `json:"timestamp"`
}

type sourceFunc func(string, *Config) ([]string, error)

type Config struct {
	Threads     int
	IncludeRx   *regexp.Regexp
	ExcludeRx   *regexp.Regexp
	Follow      bool
	RateLimit   time.Duration
	Retries     int
	Sources     map[string]bool
	GitHubToken string
	Samples     int
	OutDir      string
	Timeout     time.Duration
}

// ------------------ Globals ------------------
var skipExt = []string{
	".jpg", ".jpeg", ".png", ".gif", ".css", ".js", ".svg",
	".ico", ".woff", ".woff2", ".ttf", ".eot", ".pdf", ".zip",
	".mp4", ".mp3", ".avi", ".exe", ".iso", ".apk",
}

var highRiskParams = []string{"auth", "token", "password", "pass", "session", "secret", "cmd", "exec", "eval", "admin", "key", "api"}
var medRiskParams = []string{"id", "user", "uid", "redirect", "next", "page", "q", "query", "search", "ref"}

// ------------------ Helpers ------------------
func banner() {
	color.Cyan("\n╔═╗┌─┐┬─┐┌─┐┬─┐┌─┐┬─┐┌─┐┬─┐  ╔═╗╔═╗╦╔╗╔╔╦╗╔═╗╦═╗")
	color.Cyan("╚═╗├┤ ├┬┘├┤ ├┬┘├─┤├┬┘├─┤├┬┘  ╠═╝║╣ ║║║║ ║ ║╣ ╠╦╝")
	color.Cyan("╚═╝└─┘┴└─└─┘┴└─┴ ┴┴└─┴ ┴┴└─  ╩  ╚═╝╩╝╚╝ ╩ ╚═╝╩╚═")
	color.Magenta("       ⚡ ParaFinder ⚡\n")
}

func containsSkipExt(u string) bool {
	u = strings.ToLower(u)
	for _, ext := range skipExt {
		if strings.HasSuffix(u, ext) {
			return true
		}
	}
	return false
}

func normalizeURL(u string) string {
	u = strings.TrimSpace(u)
	// remove trailing # fragments
	if idx := strings.Index(u, "#"); idx != -1 {
		u = u[:idx]
	}
	return u
}

func dedupe(urls []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		n := normalizeURL(u)
		if n == "" {
			continue
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// ------------------ Param analysis ------------------
func extractParams(urls []string) map[string]int {
	paramRegex := regexp.MustCompile(`[?&]([^=&#]+)=`)
	params := map[string]int{}
	for _, u := range urls {
		for _, m := range paramRegex.FindAllStringSubmatch(u, -1) {
			k := strings.ToLower(m[1])
			params[k]++
		}
	}
	return params
}

func scoreParams(params map[string]int) (map[string]string, []string) {
	scores := map[string]string{}
	high := []string{}
	for p := range params {
		l := strings.ToLower(p)
		r := "low"
		for _, h := range highRiskParams {
			if strings.Contains(l, h) {
				r = "high"
				break
			}
		}
		if r != "high" {
			for _, m := range medRiskParams {
				if strings.Contains(l, m) {
					r = "medium"
					break
				}
			}
		}
		scores[p] = r
		if r == "high" {
			high = append(high, p)
		}
	}
	return scores, high
}

// ------------------ HTTP client with options ------------------
func newHTTPClient(cfg *Config) *http.Client {
	client := &http.Client{
		Timeout: cfg.Timeout,
	}
	if !cfg.Follow {
		// disable redirects
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

// ------------------ Rate-limited fetch utility ------------------
func doGetWithRetries(client *http.Client, urlStr string, rate <-chan time.Time, retries int) (body []byte, err error) {
	var resp *http.Response
	for attempt := 0; attempt <= retries; attempt++ {
		if rate != nil {
			<-rate
		}
		resp, err = client.Get(urlStr)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			body, err = io.ReadAll(resp.Body)
			return
		}
		// non-2xx: treat as empty but not fatal
		body, _ = io.ReadAll(resp.Body)
		return body, nil
	}
	return nil, err
}

// ------------------ Sources ------------------
// Wayback CDX
func fetchWayback(domain string, cfg *Config) ([]string, error) {
	// Query CDX for original URLs (text output)
	cdx := fmt.Sprintf("https://web.archive.org/cdx/search/cdx?url=*.%s/*&output=text&fl=original&collapse=urlkey", domain)
	client := newHTTPClient(cfg)
	rate := make(<-chan time.Time)
	if cfg.RateLimit > 0 {
		t := time.NewTicker(cfg.RateLimit)
		rate = t.C
		defer t.Stop()
	}
	body, err := doGetWithRetries(client, cdx, rate, cfg.Retries)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(body), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// filter query presence & skip ext
		if strings.Contains(l, "?") && !containsSkipExt(l) {
			out = append(out, normalizeURL(l))
		}
	}
	return out, nil
}

// URLScan.io search
func fetchURLScan(domain string, cfg *Config) ([]string, error) {
	api := fmt.Sprintf("https://urlscan.io/api/v1/search/?q=domain:%s&size=1000", domain)
	client := newHTTPClient(cfg)
	rate := make(<-chan time.Time)
	if cfg.RateLimit > 0 {
		t := time.NewTicker(cfg.RateLimit)
		rate = t.C
		defer t.Stop()
	}
	body, err := doGetWithRetries(client, api, rate, cfg.Retries)
	if err != nil {
		return nil, err
	}
	// find urls in JSON with a regex (robust)
	re := regexp.MustCompile(`https?://[^\s"']+`)
	found := re.FindAllString(string(body), -1)
	out := []string{}
	for _, u := range found {
		if strings.Contains(u, "?") && !containsSkipExt(u) {
			out = append(out, normalizeURL(u))
		}
	}
	return out, nil
}

// CommonCrawl (best-effort)
func fetchCommonCrawl(domain string, cfg *Config) ([]string, error) {
	// Using a public index service - note indices vary by time (best effort)
	// Try a few indices (this is conservative)
	indexes := []string{
		"CC-MAIN-2023-14-index",
		"CC-MAIN-2022-05-index",
	}
	out := []string{}
	client := newHTTPClient(cfg)
	rate := make(<-chan time.Time)
	if cfg.RateLimit > 0 {
		t := time.NewTicker(cfg.RateLimit)
		rate = t.C
		defer t.Stop()
	}
	for _, idx := range indexes {
		q := fmt.Sprintf("http://index.commoncrawl.org/%s?url=*.%s/*&output=json", idx, domain)
		body, err := doGetWithRetries(client, q, rate, cfg.Retries)
		if err != nil || len(body) == 0 {
			continue
		}
		// each line is a JSON object with "url"
		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(line), &obj); err == nil {
				if u, ok := obj["url"].(string); ok {
					if strings.Contains(u, "?") && !containsSkipExt(u) {
						out = append(out, normalizeURL(u))
					}
				}
			}
		}
	}
	return out, nil
}

// GitHub code search (requires token) - optional
func fetchGitHub(domain string, cfg *Config) ([]string, error) {
	if cfg.GitHubToken == "" {
		return nil, nil
	}
	// Use Search API for code: q=domain in:file
	api := fmt.Sprintf("https://api.github.com/search/code?q=%s+in:file&per_page=100", url.QueryEscape(domain))
	client := newHTTPClient(cfg)
	req, _ := http.NewRequest("GET", api, nil)
	req.Header.Set("Authorization", "token "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github.v3.text-match+json")

	rate := make(<-chan time.Time)
	if cfg.RateLimit > 0 {
		t := time.NewTicker(cfg.RateLimit)
		rate = t.C
		defer t.Stop()
	}
	// do request with retries
	var body []byte
	var err error
	for i := 0; i <= cfg.Retries; i++ {
		if rate != nil {
			<-rate
		}
		resp, err2 := client.Do(req)
		if err2 != nil {
			err = err2
			time.Sleep(200 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		break
	}
	if err != nil {
		return nil, err
	}
	// find URLs using regex
	re := regexp.MustCompile(`https?://[^\s"']+`)
	found := re.FindAllString(string(body), -1)
	out := []string{}
	for _, u := range found {
		if strings.Contains(u, "?") && !containsSkipExt(u) {
			out = append(out, normalizeURL(u))
		}
	}
	return out, nil
}

// ------------------ Runner ------------------
func collectFromSources(domain string, cfg *Config) ([]string, error) {
	// map available sources to functions
	sourceMap := map[string]sourceFunc{
		"wayback":     fetchWayback,
		"urlscan":     fetchURLScan,
		"commoncrawl": fetchCommonCrawl,
		"github":      fetchGitHub,
	}
	var sources []string
	for s := range cfg.Sources {
		if cfg.Sources[s] {
			sources = append(sources, s)
		}
	}
	if len(sources) == 0 {
		// default set
		sources = []string{"wayback", "urlscan", "commoncrawl"}
	}

	// concurrency: spawn goroutines limited by cfg.Threads
	sem := make(chan struct{}, cfg.Threads)
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	all := []string{}

	s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
	s.Prefix = color.HiYellowString("[*] Collecting URLs for %s ", domain)
	s.Start()

	for _, name := range sources {
		fn, ok := sourceMap[name]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(n string, f sourceFunc) {
			defer wg.Done()
			sem <- struct{}{}
			color.HiCyan("[*] Fetching from %s...", n)
			data, err := f(domain, cfg)
			<-sem
			if err != nil {
				color.Red("[!] %s fetch error: %v", n, err)
				return
			}
			mu.Lock()
			all = append(all, data...)
			mu.Unlock()
			color.HiGreen("[✓] %s fetched %d URLs", n, len(data))
		}(name, fn)
	}

	wg.Wait()
	s.Stop()

	// apply include/exclude filters and dedupe
	filtered := []string{}
	for _, u := range dedupe(all) {
		if cfg.IncludeRx != nil && !cfg.IncludeRx.MatchString(u) {
			continue
		}
		if cfg.ExcludeRx != nil && cfg.ExcludeRx.MatchString(u) {
			continue
		}
		filtered = append(filtered, u)
	}
	return filtered, nil
}

// ------------------ Save outputs ------------------
func saveAllURLs(urls []string, outdir, base string) (string, error) {
	fn := fmt.Sprintf("%s/%s_all_urls.txt", outdir, base)
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, u := range urls {
		fmt.Fprintln(w, u)
	}
	w.Flush()
	return fn, nil
}

func saveHighURLs(urls []string, params map[string]int, scores map[string]string, outdir, base string) (string, error) {
	// collect URLs that contain any high param
	highList := []string{}
	highParams := []string{}
	for p, s := range scores {
		if s == "high" {
			highParams = append(highParams, p)
		}
	}
	if len(highParams) == 0 {
		return "", nil
	}
	for _, u := range urls {
		for _, p := range highParams {
			if strings.Contains(strings.ToLower(u), p+"=") {
				highList = append(highList, u)
				break
			}
		}
	}
	fn := fmt.Sprintf("%s/%s_high_value_urls.txt", outdir, base)
	f, err := os.Create(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, u := range dedupe(highList) {
		fmt.Fprintln(w, u)
	}
	w.Flush()
	return fn, nil
}

func saveTXT(res *Result, outdir, base string) (string, error) {
	fn := fmt.Sprintf("%s/%s.txt", outdir, base)
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "ParaFinder Ultra — TXT Report\nTarget: %s\nScan Time: %s\nDuration: %s\n\n", res.Domain, res.Timestamp, res.ScanDuration)
	fmt.Fprintf(w, "Total URLs: %d\nUnique Params: %d\nHigh-Value Params: %d\n\n", len(res.URLs), len(res.Parameters), len(res.HighValue))
	fmt.Fprintln(w, "Extracted Parameters (param:count - score):")
	// sort params by count desc
	type kv struct{ k string; v int; s string }
	list := []kv{}
	for k, v := range res.Parameters {
		list = append(list, kv{k, v, res.ParamScores[k]})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	for _, it := range list {
		fmt.Fprintf(w, "  %s : %d - %s\n", it.k, it.v, it.s)
	}
	fmt.Fprintln(w, "\nSample URLs:")
	for i, u := range res.URLs {
		if i >= 50 {
			break
		}
		fmt.Fprintln(w, " ", u)
	}
	w.Flush()
	return fn, nil
}

func saveJSONFile(res *Result, outdir, base string) (string, error) {
	fn := fmt.Sprintf("%s/%s.json", outdir, base)
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return "", err
	}
	return fn, nil
}

func saveCSVFile(res *Result, outdir, base string) (string, error) {
	fn := fmt.Sprintf("%s/%s_params.csv", outdir, base)
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return "", err
	}
	f, err := os.Create(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"parameter", "count", "score"})
	// sort params desc
	type kv struct{ k string; v int; s string }
	list := []kv{}
	for k, v := range res.Parameters {
		list = append(list, kv{k, v, res.ParamScores[k]})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	for _, it := range list {
		w.Write([]string{it.k, fmt.Sprintf("%d", it.v), it.s})
	}
	return fn, nil
}

// HTML report (basic, inline CSS)
const htmlTpl = `<!doctype html>
<html>
<head>
<meta charset="utf-8"/>
<title>ParaFinder Ultra Report - {{.Domain}}</title>
<style>
body{font-family:Inter,Segoe UI,Arial;margin:20px;color:#111}
h1{color:#2b6cb0}
.badge{display:inline-block;padding:6px 10px;border-radius:6px;background:#eee;margin-right:6px}
.high{background:#ffdddd;color:#900}
.medium{background:#fff4e5;color:#c65a00}
.low{background:#e6ffed;color:#0a7a3b}
table{border-collapse:collapse;width:100%;margin-top:12px}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#f7fafc}
.url{font-family:monospace;font-size:13px}
</style>
</head>
<body>
<h1>ParaFinder Ultra Report — {{.Domain}}</h1>
<p><strong>Scan Time:</strong> {{.Timestamp}} &nbsp; <strong>Duration:</strong> {{.ScanDuration}}</p>
<p>
<span class="badge">Total URLs: {{len .URLs}}</span>
<span class="badge">Unique Params: {{len .Parameters}}</span>
<span class="badge">High-Value: {{len .HighValue}}</span>
</p>

<h2>Top Parameters</h2>
<table>
<tr><th>Parameter</th><th>Count</th><th>Score</th></tr>
{{range $k,$v := .ParamsSorted}}
<tr>
<td>{{$v.Name}}</td>
<td>{{$v.Count}}</td>
<td class="{{if eq $v.Score "high"}}high{{else if eq $v.Score "medium"}}medium{{else}}low{{end}}">{{$v.Score}}</td>
</tr>
{{end}}
</table>

<h2>Sample URLs</h2>
<table>
<tr><th>URL</th></tr>
{{range .SampleURLs}}
<tr><td class="url">{{.}}</td></tr>
{{end}}
</table>

</body>
</html>`

func saveHTML(res *Result, outdir, base string) (string, error) {
	fn := fmt.Sprintf("%s/%s.html", outdir, base)
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return "", err
	}
	type ps struct {
		Name  string
		Count int
		Score string
	}
	paramsList := []ps{}
	for k, v := range res.Parameters {
		paramsList = append(paramsList, ps{k, v, res.ParamScores[k]})
	}
	sort.Slice(paramsList, func(i, j int) bool { return paramsList[i].Count > paramsList[j].Count })
	samples := res.URLs
	if len(samples) > 50 {
		samples = samples[:50]
	}
	t, _ := template.New("report").Parse(htmlTpl)
	f, err := os.Create(fn)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data := map[string]interface{}{
		"Domain":      res.Domain,
		"Timestamp":   res.Timestamp,
		"ScanDuration": res.ScanDuration,
		"URLs":        res.URLs,
		"Parameters":  res.Parameters,
		"HighValue":   res.HighValue,
		"ParamsSorted": paramsList,
		"SampleURLs":  samples,
	}
	if err := t.Execute(f, data); err != nil {
		return "", err
	}
	return fn, nil
}

// ------------------ Main run ------------------
func run(domain string, cfg *Config, outFormats []string) {
	banner()
	start := time.Now()
	base := fmt.Sprintf("parafinder_%s_%d", strings.ReplaceAll(domain, ".", "_"), time.Now().Unix())

	urls, err := collectFromSources(domain, cfg)
	if err != nil {
		color.Red("[!] error collecting: %v", err)
	}

	if len(urls) == 0 {
		color.Red("[!] No URLs found. Exiting.")
		return
	}

	// param extraction & scoring
	params := extractParams(urls)
	scores, high := scoreParams(params)

	// sort URLs (optional: could rank by param count)
	unique := dedupe(urls)

	// Build result
	res := &Result{
		Domain:       domain,
		URLs:         unique,
		Parameters:   params,
		HighValue:    high,
		ParamScores:  scores,
		ScanDuration: time.Since(start).String(),
		Timestamp:    time.Now().Format(time.RFC3339),
	}

	// CLI Summary
	color.HiMagenta("\n────────── SUMMARY ──────────")
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Metric", "Value"})
	table.SetAutoWrapText(false)
	table.Append([]string{"Total URLs", fmt.Sprintf("%d", len(res.URLs))})
	table.Append([]string{"Unique Params", fmt.Sprintf("%d", len(res.Parameters))})
	table.Append([]string{"High-Value Params", fmt.Sprintf("%d", len(res.HighValue))})
	table.Append([]string{"Duration", res.ScanDuration})
	table.Render()
	color.HiMagenta("──────────────────────────────\n")

	// Save outputs
	if cfg.OutDir == "" {
		cfg.OutDir = "."
	}
	saved := []string{}
	for _, fmtName := range outFormats {
		switch strings.ToLower(fmtName) {
		case "txt":
			if p, err := saveTXT(res, cfg.OutDir, base); err == nil {
				color.Green("[+] TXT saved: %s", p); saved = append(saved, p)
			}
		case "json":
			if p, err := saveJSONFile(res, cfg.OutDir, base); err == nil {
				color.Green("[+] JSON saved: %s", p); saved = append(saved, p)
			}
		case "csv":
			if p, err := saveCSVFile(res, cfg.OutDir, base); err == nil {
				color.Green("[+] CSV saved: %s", p); saved = append(saved, p)
			}
		case "html":
			if p, err := saveHTML(res, cfg.OutDir, base); err == nil {
				color.Green("[+] HTML saved: %s", p); saved = append(saved, p)
			}
		}
	}
	// always save all_urls and high_value_urls
	if p, err := saveAllURLs(res.URLs, cfg.OutDir, base); err == nil {
		color.Green("[+] All URLs saved: %s", p); saved = append(saved, p)
	}
	if p, err := saveHighURLs(res.URLs, res.Parameters, res.ParamScores, cfg.OutDir, base); err == nil && p != "" {
		color.Green("[+] High-value URLs saved: %s", p); saved = append(saved, p)
	}

	// Show top parameters colored
	color.HiBlue("\nTop Parameters:")
	type kv struct{ k string; v int; s string }
	list := []kv{}
	for k, v := range res.Parameters {
		list = append(list, kv{k, v, res.ParamScores[k]})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	max := 15
	if len(list) < max {
		max = len(list)
	}
	for i := 0; i < max; i++ {
		it := list[i]
		switch it.s {
		case "high":
			color.Red("  %s — %d (HIGH)", it.k, it.v)
		case "medium":
			color.Yellow("  %s — %d (MED)", it.k, it.v)
		default:
			color.Green("  %s — %d (low)", it.k, it.v)
		}
	}

	// Show sample URLs
	color.HiMagenta("\nSample URLs (first %d):", cfg.Samples)
	for i := 0; i < cfg.Samples && i < len(res.URLs); i++ {
		fmt.Println("  ", res.URLs[i])
	}

	color.HiGreen("\n🎯 Scan completed. Saved files:")
	for _, s := range saved {
		color.Green("  %s", s)
	}
}

// ------------------ Main ------------------
func main() {
	domain := flag.String("d", "", "Target domain (example.com)")
	threads := flag.Int("t", runtime.NumCPU(), "Concurrent threads")
	out := flag.String("o", "txt,json,csv,html", "Output formats (csv,json,txt,html) comma-separated")
	include := flag.String("include", "", "Include regex (only URLs matching will be kept)")
	exclude := flag.String("exclude", "", "Exclude regex (URLs matching will be dropped)")
	follow := flag.Bool("follow", false, "Follow redirects")
	rate := flag.Int("rate", 0, "Rate limit per-source in ms (0 = unlimited)")
	retries := flag.Int("retries", 2, "Retries per request")
	sources := flag.String("sources", "", "Comma list of sources: wayback,urlscan,commoncrawl,github (empty = defaults)")
	gitToken := flag.String("github-token", "", "GitHub token for code search (optional)")
	samples := flag.Int("samples", 8, "Number of sample URLs to show")
	outdir := flag.String("outdir", "results", "Output directory")
	timeout := flag.Int("timeout", 30, "HTTP timeout seconds")

	flag.Parse()

	if *domain == "" {
		fmt.Println("Usage: ./parafinder_ultra_advanced -d example.com [options]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	var includeRx *regexp.Regexp
	var excludeRx *regexp.Regexp
	var err error
	if *include != "" {
		includeRx, err = regexp.Compile(*include)
		if err != nil {
			color.Red("[!] include regex invalid: %v", err); os.Exit(1)
		}
	}
	if *exclude != "" {
		excludeRx, err = regexp.Compile(*exclude)
		if err != nil {
			color.Red("[!] exclude regex invalid: %v", err); os.Exit(1)
		}
	}
	// prepare sources map
	srcMap := map[string]bool{}
	if *sources != "" {
		for _, s := range strings.Split(*sources, ",") {
			srcMap[strings.TrimSpace(s)] = true
		}
	}

	cfg := &Config{
		Threads:     *threads,
		IncludeRx:   includeRx,
		ExcludeRx:   excludeRx,
		Follow:      *follow,
		RateLimit:   time.Duration(*rate) * time.Millisecond,
		Retries:     *retries,
		Sources:     srcMap,
		GitHubToken: *gitToken,
		Samples:     *samples,
		OutDir:      *outdir,
		Timeout:     time.Duration(*timeout) * time.Second,
	}

	outFormats := []string{}
	for _, s := range strings.Split(*out, ",") {
		if s = strings.TrimSpace(s); s != "" {
			outFormats = append(outFormats, s)
		}
	}

	run(*domain, cfg, outFormats)
}

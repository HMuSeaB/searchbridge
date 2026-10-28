package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// httpClient 是带超时的最小 HTTP 客户端封装。
type httpClient struct {
	timeout time.Duration
	client  *http.Client
}

func newHTTPClient(timeout time.Duration) *httpClient {
	return &httpClient{timeout: timeout, client: &http.Client{Timeout: timeout}}
}

func (h *httpClient) get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return h.do(req)
}

func (h *httpClient) postJSON(ctx context.Context, rawURL string, payload any, headers map[string]string) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return h.do(req)
}

func (h *httpClient) postForm(ctx context.Context, rawURL string, form url.Values, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return h.do(req)
}

func (h *httpClient) do(req *http.Request) ([]byte, error) {
	if h.client == nil {
		// 惰性初始化，防止直接构造 httpClient 时遗漏。
		h.client = &http.Client{Timeout: h.timeout}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------- remote：wbmux 等 Anthropic Messages 搜索端点 ----------

type remoteBackend struct {
	name       string
	endpoint   string
	apiKey     string
	model      string
	httpClient *httpClient
}

func (b *remoteBackend) Name() string { return b.name }

func (b *remoteBackend) Search(ctx context.Context, query string, limit int) ([]searchResult, error) {
	payload := map[string]any{
		"model":      b.model,
		"max_tokens": 4096,
		"system":     "You are an assistant for performing a web search tool use.",
		"messages":   []map[string]string{{"role": "user", "content": "Perform a web search for the query: " + query}},
		"tools": []map[string]any{{
			"type":     "web_search_20260209",
			"name":     "web_search",
			"max_uses": limit,
		}},
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	if b.apiKey != "" {
		headers["x-api-key"] = b.apiKey
	}

	body, err := b.httpClient.postJSON(ctx, b.endpoint, payload, headers)
	if err != nil {
		return nil, err
	}
	var msg anthropicMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("unparseable search response: %v", err)
	}
	var results []searchResult
	for _, block := range msg.Content {
		if block.Type != "web_search_tool_result" {
			continue
		}
		var items []struct {
			Type  string `json:"type"`
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(block.Content, &items); err != nil {
			continue
		}
		for _, it := range items {
			if it.Type == "web_search_result" || it.Type == "" {
				results = append(results, searchResult{Title: it.Title, URL: it.URL})
			}
		}
	}
	return results, nil
}

// ---------- ddg：DuckDuckGo Lite（免 key） ----------

type ddgLiteBackend struct {
	endpoint   string
	httpClient *httpClient
}

func (b *ddgLiteBackend) Name() string { return "ddg" }

var (
	// DDG Lite 的结果链接：<a ... class='result-link' href="...">标题</a>，
	// href 与 class 的先后顺序不固定，因此先整标签匹配再分别提取。
	ddgAnchorRe  = regexp.MustCompile(`(?is)<a\s([^>]*class=["']?result-link["']?[^>]*)>(.*?)</a>`)
	ddgHrefRe    = regexp.MustCompile(`(?is)href=["']([^"']+)["']`)
	ddgSnippetRe = regexp.MustCompile(`(?is)<td[^>]*class=["']?result-snippet["']?[^>]*>(.*?)</td>`)
)

func (b *ddgLiteBackend) Search(ctx context.Context, query string, limit int) ([]searchResult, error) {
	form := url.Values{"q": {query}}
	body, err := b.httpClient.postForm(ctx, b.endpoint, form, map[string]string{
		"user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) searchbridge/0.2",
	})
	if err != nil {
		return nil, err
	}
	html := string(body)

	anchors := ddgAnchorRe.FindAllStringSubmatch(html, -1)
	snippets := ddgSnippetRe.FindAllStringSubmatch(html, -1)

	var results []searchResult
	for i, m := range anchors {
		if len(results) >= limit {
			break
		}
		hrefMatch := ddgHrefRe.FindStringSubmatch(m[1])
		if hrefMatch == nil {
			continue
		}
		u := decodeDDGURL(hrefMatch[1])
		if u == "" {
			continue
		}
		snippet := ""
		if i < len(snippets) {
			snippet = stripTags(snippets[i][1])
		}
		results = append(results, searchResult{
			Title:   stripTags(m[2]),
			URL:     u,
			Snippet: snippet,
		})
	}
	return results, nil
}

// decodeDDGURL 从 DDG 的跳转链接中解出真实 URL（//duckduckgo.com/l/?uddg=<encoded>）。
func decodeDDGURL(href string) string {
	if !strings.Contains(href, "uddg=") {
		if strings.HasPrefix(href, "http") {
			return href
		}
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	decoded := u.Query().Get("uddg")
	if decoded == "" {
		return ""
	}
	return decoded
}

// ---------- bing：Bing 网页搜索（国内默认走 cn.bing.com） ----------

type bingBackend struct {
	host       string
	httpClient *httpClient
}

func (b *bingBackend) Name() string { return "bing" }

var bingItemRe = regexp.MustCompile(`(?is)<li[^>]*class=["']?b_algo["']?[^>]*>(.*?)</li>`)
var (
	bingTitleRe = regexp.MustCompile(`(?is)<h2[^>]*>\s*<a[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	bingSnipRe  = regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
)

func (b *bingBackend) Search(ctx context.Context, query string, limit int) ([]searchResult, error) {
	u := b.host + "/search?q=" + url.QueryEscape(query) + "&count=20"
	body, err := b.httpClient.get(ctx, u, map[string]string{
		"user-agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36",
		"accept-language": "zh-CN,zh;q=0.9,en;q=0.8",
	})
	if err != nil {
		return nil, err
	}
	html := string(body)

	var results []searchResult
	for _, item := range bingItemRe.FindAllStringSubmatch(html, -1) {
		if len(results) >= limit {
			break
		}
		tm := bingTitleRe.FindStringSubmatch(item[1])
		if tm == nil {
			continue
		}
		snippet := ""
		if sm := bingSnipRe.FindStringSubmatch(item[1]); sm != nil {
			snippet = stripTags(sm[1])
		}
		results = append(results, searchResult{
			Title:   stripTags(tm[2]),
			URL:     tm[1],
			Snippet: snippet,
		})
	}
	return results, nil
}

// ---------- searxng：自建 / 公共 SearXNG 实例（JSON API） ----------

type searxngBackend struct {
	baseURL    string
	httpClient *httpClient
}

func (b *searxngBackend) Name() string { return "searxng" }

func (b *searxngBackend) Search(ctx context.Context, query string, limit int) ([]searchResult, error) {
	u := fmt.Sprintf("%s/search?q=%s&format=json&language=zh-CN", b.baseURL, url.QueryEscape(query))
	body, err := b.httpClient.get(ctx, u, map[string]string{"accept": "application/json"})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("searxng returned non-JSON (instance may have JSON API disabled): %v", err)
	}
	var results []searchResult
	for i, r := range resp.Results {
		if i >= limit {
			break
		}
		results = append(results, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return results, nil
}

// ---------- tavily：Tavily Search API（有免费额度） ----------

type tavilyBackend struct {
	apiKey     string
	baseURL    string
	httpClient *httpClient
}

func (b *tavilyBackend) Name() string { return "tavily" }

func (b *tavilyBackend) Search(ctx context.Context, query string, limit int) ([]searchResult, error) {
	payload := map[string]any{
		"api_key":      b.apiKey,
		"query":        query,
		"max_results":  limit,
		"search_depth": "basic",
	}
	body, err := b.httpClient.postJSON(ctx, b.baseURL, payload, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unparseable tavily response: %v", err)
	}
	var results []searchResult
	for _, r := range resp.Results {
		results = append(results, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return results, nil
}

// ---------- brave：Brave Search API（2026 年起已无免费档，需订阅 Key） ----------

type braveBackend struct {
	apiKey     string
	baseURL    string
	httpClient *httpClient
}

func (b *braveBackend) Name() string { return "brave" }

func (b *braveBackend) Search(ctx context.Context, query string, limit int) ([]searchResult, error) {
	u := fmt.Sprintf("%s/res/v1/web/search?q=%s&count=%d", b.baseURL, url.QueryEscape(query), limit)
	body, err := b.httpClient.get(ctx, u, map[string]string{
		"accept":               "application/json",
		"x-subscription-token": b.apiKey,
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unparseable brave response: %v", err)
	}
	var results []searchResult
	for _, r := range resp.Web.Results {
		results = append(results, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return results, nil
}

// ---------- HTML 工具 ----------

var (
	tagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	wsRe      = regexp.MustCompile(`\s+`)
	entities  = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ")
)

// stripTags 去掉 HTML 标签与注释、反转义实体、压缩空白。
func stripTags(s string) string {
	s = commentRe.ReplaceAllString(s, "")
	s = tagRe.ReplaceAllString(s, "")
	s = entities.Replace(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

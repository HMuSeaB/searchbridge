package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- stripTags ----------

func TestStripTags(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<b>粗体</b>文本`, "粗体文本"},
		{`<a href="x">链接&amp;更多</a>`, "链接&更多"},
		{`  多个    空白  `, "多个 空白"},
		{`<!-- 注释 -->保留`, "保留"},
		{`<p>引号&quot;与&#39;单引号&#39;</p>`, `引号"与'单引号'`},
	}
	for _, c := range cases {
		if got := stripTags(c.in); got != c.want {
			t.Errorf("stripTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- remote 后端 ----------

func TestRemoteBackend(t *testing.T) {
	var gotTool string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if len(payload.Tools) > 0 {
			gotTool = payload.Tools[0].Name
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"msg_x","type":"message","role":"assistant","model":"web-search",
			"content":[
				{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[
					{"type":"web_search_result","url":"https://a.example","title":"A"},
					{"type":"web_search_result","url":"https://b.example","title":"B"}
				]},
				{"type":"text","text":""}
			],
			"stop_reason":"end_turn"
		}`))
	}))
	defer server.Close()

	b := &remoteBackend{name: "remote", endpoint: server.URL, httpClient: newHTTPClient(0)}
	results, err := b.Search(context.Background(), "查询", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotTool != "web_search" {
		t.Errorf("request tool name = %q, want web_search", gotTool)
	}
	if len(results) != 2 || results[0].URL != "https://a.example" || results[1].Title != "B" {
		t.Errorf("results = %+v", results)
	}
}

func TestRemoteBackendHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"declare web_search tool"}}`))
	}))
	defer server.Close()

	b := &remoteBackend{name: "remote", endpoint: server.URL, httpClient: newHTTPClient(0)}
	if _, err := b.Search(context.Background(), "查询", 5); err == nil {
		t.Fatal("expected error on HTTP 400")
	}
}

// ---------- ddg 后端 ----------

const ddgFixture = `<html><body>
<table>
<tr><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage1&amp;rut=xyz" class='result-link'>第一个结果</a></td></tr>
<tr><td class='result-snippet'>这是第一个摘要 &amp; 更多内容</td></tr>
<tr><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage2" class='result-link'>第二个结果</a></td></tr>
<tr><td class='result-snippet'>第二个摘要</td></tr>
</table>
</body></html>`

func TestDDGBackend(t *testing.T) {
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForm = r.PostFormValue("q")
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer server.Close()

	b := &ddgLiteBackend{endpoint: server.URL, httpClient: newHTTPClient(0)}
	results, err := b.Search(context.Background(), "测试", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotForm != "测试" {
		t.Errorf("form q = %q, want 测试", gotForm)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(results), results)
	}
	if results[0].Title != "第一个结果" || results[0].URL != "https://example.com/page1" {
		t.Errorf("result[0] = %+v", results[0])
	}
	if results[0].Snippet != "这是第一个摘要 & 更多内容" {
		t.Errorf("snippet[0] = %q", results[0].Snippet)
	}
	if results[1].URL != "https://example.com/page2" {
		t.Errorf("result[1] = %+v", results[1])
	}
}

func TestDDGBackendRespectsLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ddgFixture))
	}))
	defer server.Close()

	b := &ddgLiteBackend{endpoint: server.URL, httpClient: newHTTPClient(0)}
	results, err := b.Search(context.Background(), "测试", 1)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want 1", len(results))
	}
}

// ---------- bing 后端 ----------

const bingFixture = `<html><body><ol id="b_results">
<li class="b_algo"><h2><a href="https://go.dev/doc/go1.24" href2="x">Go 1.24 Release Notes</a></h2><p>Go 1.24 arrives in February 2025.</p></li>
<li class="b_algo"><h2><a href="https://example.com/other">另一条结果</a></h2><p>另一段摘要</p></li>
</ol></body></html>`

func TestBingBackend(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		_, _ = w.Write([]byte(bingFixture))
	}))
	defer server.Close()

	b := &bingBackend{host: server.URL, httpClient: newHTTPClient(0)}
	results, err := b.Search(context.Background(), "go 1.24", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotQuery != "go 1.24" {
		t.Errorf("query = %q, want go 1.24", gotQuery)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(results), results)
	}
	if results[0].Title != "Go 1.24 Release Notes" || results[0].URL != "https://go.dev/doc/go1.24" {
		t.Errorf("result[0] = %+v", results[0])
	}
	if results[0].Snippet != "Go 1.24 arrives in February 2025." {
		t.Errorf("snippet[0] = %q", results[0].Snippet)
	}
}

// ---------- searxng / tavily / brave 后端 ----------

func TestSearxngBackend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format = %q, want json", r.URL.Query().Get("format"))
		}
		_, _ = w.Write([]byte(`{"results":[
			{"title":"SearXNG 结果","url":"https://sx.example","content":"摘要"},
			{"title":"第二条","url":"https://sx2.example","content":"摘要2"}
		]}`))
	}))
	defer server.Close()

	b := &searxngBackend{baseURL: server.URL, httpClient: newHTTPClient(0)}
	results, err := b.Search(context.Background(), "查询", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 2 || results[0].Title != "SearXNG 结果" {
		t.Errorf("results = %+v", results)
	}
}

func TestSearxngBackendNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>JSON API disabled</html>`))
	}))
	defer server.Close()

	b := &searxngBackend{baseURL: server.URL, httpClient: newHTTPClient(0)}
	if _, err := b.Search(context.Background(), "查询", 5); err == nil {
		t.Fatal("expected error when instance returns non-JSON")
	}
}

func TestTavilyBackend(t *testing.T) {
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			APIKey string `json:"api_key"`
			Query  string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotKey = payload.APIKey
		_, _ = w.Write([]byte(`{"results":[{"title":"Tavily 结果","url":"https://tv.example","content":"内容"}]}`))
	}))
	defer server.Close()

	b := &tavilyBackend{apiKey: "tvly-test", baseURL: server.URL, httpClient: newHTTPClient(0)}
	results, err := b.Search(context.Background(), "查询", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotKey != "tvly-test" {
		t.Errorf("api_key = %q", gotKey)
	}
	if len(results) != 1 || results[0].Title != "Tavily 结果" {
		t.Errorf("results = %+v", results)
	}
}

func TestBraveBackend(t *testing.T) {
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("x-subscription-token")
		if !strings.Contains(r.URL.Path, "/res/v1/web/search") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"Brave 结果","url":"https://br.example","description":"描述"}]}}`))
	}))
	defer server.Close()

	b := &braveBackend{apiKey: "brave-test", baseURL: server.URL, httpClient: newHTTPClient(0)}
	results, err := b.Search(context.Background(), "查询", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if gotToken != "brave-test" {
		t.Errorf("token = %q", gotToken)
	}
	if len(results) != 1 || results[0].Title != "Brave 结果" {
		t.Errorf("results = %+v", results)
	}
}

// ---------- 链路构建 ----------

func TestBuildBackendsOrderAndSkip(t *testing.T) {
	// 显式指定顺序；未配 key 的付费后端被跳过。
	t.Setenv("SB_SEARCH_BACKENDS", "remote,ddg,bing,tavily,brave,unknown")
	t.Setenv("SB_SEARCH_URL", "http://127.0.0.1:55464/v1/search/messages")
	cfg := loadConfig()
	names := []string{}
	for _, b := range buildBackends(cfg) {
		names = append(names, b.Name())
	}
	want := []string{"remote", "ddg", "bing"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("backends = %v, want %v", names, want)
	}
}

func TestBuildBackendsDefaultWithoutRemote(t *testing.T) {
	t.Setenv("SB_SEARCH_BACKENDS", "")
	t.Setenv("SB_SEARCH_URL", "off") // 显式禁用 remote
	cfg := loadConfig()
	names := []string{}
	for _, b := range buildBackends(cfg) {
		names = append(names, b.Name())
	}
	want := []string{"ddg", "bing"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("backends = %v, want %v", names, want)
	}
}

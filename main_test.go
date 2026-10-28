package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- web_search 工具判定 ----------

func TestHasWebSearchTool(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"nil tools", ``, false},
		{"empty tools", `[]`, false},
		{"client-side WebSearch tool", `[{"name":"WebSearch","input_schema":{"type":"object"}}]`, false},
		{"other client tools", `[{"name":"Bash"},{"name":"Read"}]`, false},
		{"anthropic server tool 20260209", `[{"type":"web_search_20260209","name":"web_search","max_uses":8}]`, true},
		{"anthropic server tool 20250305", `[{"type":"web_search_20250305","name":"web_search"}]`, true},
		{"bare web_search name", `[{"name":"web_search"}]`, true},
		{"mixed with client tools", `[{"name":"Bash"},{"type":"web_search_20260209","name":"web_search"}]`, true},
		{"invalid json", `{oops`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw json.RawMessage
			if c.raw != "" {
				raw = json.RawMessage(c.raw)
			}
			if got := hasWebSearchTool(raw); got != c.want {
				t.Errorf("hasWebSearchTool(%s) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// ---------- 查询词与条数提取 ----------

func TestExtractSearchQuery(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"zcode side request",
			`{"messages":[{"role":"system","content":"You are an assistant for performing a web search tool use."},{"role":"user","content":"Perform a web search for the query: golang 1.24 新特性"}]}`,
			"golang 1.24 新特性",
		},
		{
			"plain user message",
			`{"messages":[{"role":"user","content":"直接搜这个"}]}`,
			"直接搜这个",
		},
		{
			"content blocks",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: 块格式查询"}]}]}`,
			"块格式查询",
		},
		{"empty body", ``, ""},
		{"no messages", `{"model":"m"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractSearchQuery([]byte(c.body)); got != c.want {
				t.Errorf("extractSearchQuery() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSearchLimit(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"tools":[{"type":"web_search_20260209","name":"web_search","max_uses":7}]}`, 7},
		{`{"tools":[{"name":"web_search"}]}`, 5},
		{`{"tools":[{"name":"web_search","max_uses":99}]}`, 5}, // 超上限回退默认
		{`{"tools":[{"name":"Bash"}]}`, 5},
		{`{}`, 5},
	}
	for _, c := range cases {
		if got := searchLimit([]byte(c.body)); got != c.want {
			t.Errorf("searchLimit(%s) = %d, want %d", c.body, got, c.want)
		}
	}
}

// ---------- 测试辅助 ----------

// stubBackend 是可控的测试后端。
type stubBackend struct {
	name    string
	results []searchResult
	err     error
	calls   int
}

func (b *stubBackend) Name() string { return b.name }

func (b *stubBackend) Search(_ context.Context, _ string, _ int) ([]searchResult, error) {
	b.calls++
	return b.results, b.err
}

func newTestServer(t *testing.T, upstreamURL string, backends ...searchBackend) *server {
	t.Helper()
	return &server{
		cfg: config{
			listenAddr:  "127.0.0.1:0",
			upstreamURL: upstreamURL,
		},
		client: &http.Client{},
		chain:  newBackendChain(backends, 10*time.Minute, 30*time.Minute),
	}
}

func doJSON(t *testing.T, srv *server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func stubResults(n int) []searchResult {
	results := make([]searchResult, n)
	for i := range results {
		results[i] = searchResult{
			Title:   "结果 " + string(rune('A'+i)),
			URL:     "https://example.com/" + string(rune('a'+i)),
			Snippet: "摘要 " + string(rune('A'+i)),
		}
	}
	return results
}

// ---------- 搜索路由（走后端链） ----------

func TestSearchRequestUsesBackendChain(t *testing.T) {
	primary := &stubBackend{name: "primary", results: stubResults(3)}
	srv := newTestServer(t, "http://upstream.invalid", primary)

	reqBody := `{"model":"step-5-preview","max_tokens":4096,"messages":[{"role":"user","content":"Perform a web search for the query: 测试查询"}],` +
		`"tools":[{"type":"web_search_20260209","name":"web_search","max_uses":3}]}`

	rec := doJSON(t, srv, http.MethodPost, "/v1/messages", reqBody, map[string]string{
		"content-type":      "application/json",
		"x-api-key":         "client-dummy-key",
		"anthropic-version": "2023-06-01",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if primary.calls != 1 {
		t.Errorf("primary backend calls = %d, want 1", primary.calls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "web_search_tool_result") {
		t.Errorf("response missing web_search_tool_result: %s", body)
	}
	if !strings.Contains(body, "https://example.com/a") {
		t.Errorf("response missing search result URL: %s", body)
	}
}

func TestSearchRequestStreamSynthesizesSSE(t *testing.T) {
	primary := &stubBackend{name: "primary", results: stubResults(2)}
	srv := newTestServer(t, "http://upstream.invalid", primary)

	reqBody := `{"model":"step-5-preview","stream":true,"messages":[{"role":"user","content":"Perform a web search for the query: 流式测试"}],` +
		`"tools":[{"type":"web_search_20260209","name":"web_search"}]}`

	rec := doJSON(t, srv, http.MethodPost, "/v1/messages", reqBody, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("content-type = %q, want event-stream", ct)
	}

	events := parseSSE(t, rec.Body.String())
	wantOrder := []string{
		"ping", "message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // server_tool_use
		"content_block_start", "content_block_stop", // web_search_tool_result
		"message_delta", "message_stop",
	}
	if len(events) != len(wantOrder) {
		t.Fatalf("got %d events %v, want %d %v", len(events), eventNames(events), len(wantOrder), wantOrder)
	}
	for i, want := range wantOrder {
		if events[i].event != want {
			t.Errorf("event[%d] = %q, want %q", i, events[i].event, want)
		}
	}
	if !strings.Contains(rec.Body.String(), "https://example.com/a") {
		t.Error("SSE stream missing search result URL")
	}
	if !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Error("SSE stream missing stop_reason")
	}
}

func TestSearchAllBackendsFailReturnsError(t *testing.T) {
	failing := &stubBackend{name: "failing", err: io.ErrUnexpectedEOF}
	srv := newTestServer(t, "http://upstream.invalid", failing)

	rec := doJSON(t, srv, http.MethodPost, "/v1/messages",
		`{"model":"m","messages":[{"role":"user","content":"Perform a web search for the query: 会失败的查询"}],"tools":[{"name":"web_search"}]}`, nil)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "all search backends failed") {
		t.Errorf("error body = %s", rec.Body.String())
	}
}

func TestSearchJSONEndpoint(t *testing.T) {
	primary := &stubBackend{name: "primary", results: stubResults(2)}
	srv := newTestServer(t, "http://upstream.invalid", primary)

	rec := doJSON(t, srv, http.MethodGet, "/v1/search?q=json%20endpoint&n=2", ``, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload struct {
		OK      bool           `json:"ok"`
		Query   string         `json:"query"`
		Via     string         `json:"via"`
		Results []searchResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !payload.OK || payload.Query != "json endpoint" || payload.Via != "primary" || len(payload.Results) != 2 {
		t.Errorf("unexpected payload: %s", rec.Body.String())
	}

	// 缺少 q 参数。
	rec = doJSON(t, srv, http.MethodGet, "/v1/search", ``, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing query status = %d, want 400", rec.Code)
	}
}

// ---------- 上游转发 ----------

func TestChatRequestForwardedToUpstream(t *testing.T) {
	var gotPath, gotAuth, gotVersion, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotVersion = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}]}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL)
	reqBody := `{"model":"step-5-preview","messages":[{"role":"user","content":"ping"}],` +
		`"tools":[{"name":"WebSearch","input_schema":{"type":"object"}}]}`

	rec := doJSON(t, srv, http.MethodPost, "/v1/messages", reqBody, map[string]string{
		"content-type":      "application/json",
		"x-api-key":         "client-dummy-key",
		"anthropic-version": "2023-06-01",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("upstream x-api-key = %q, want empty (no upstream key configured)", gotAuth)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
	if gotBody != reqBody {
		t.Errorf("upstream body mismatch:\n got: %s\nwant: %s", gotBody, reqBody)
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("response not passed through: %s", rec.Body.String())
	}
}

func TestChatRequestStreamPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL)
	rec := doJSON(t, srv, http.MethodPost, "/v1/messages",
		`{"model":"m","stream":true,"messages":[]}`, nil)

	if !strings.Contains(rec.Header().Get("content-type"), "event-stream") {
		t.Errorf("content-type = %q, want event-stream", rec.Header().Get("content-type"))
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Errorf("streamed bytes not passed through: %s", rec.Body.String())
	}
}

func TestCountTokensForwardedUpstream(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer upstream.Close()

	srv := newTestServer(t, upstream.URL)
	rec := doJSON(t, srv, http.MethodPost, "/v1/messages/count_tokens", `{"model":"m","messages":[]}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q", gotPath)
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer(t, "http://up.invalid", &stubBackend{name: "b1", results: stubResults(1)})
	rec := doJSON(t, srv, http.MethodGet, "/healthz", ``, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "searchbridge") || !strings.Contains(body, "b1") {
		t.Errorf("health body = %s", body)
	}
}

// ---------- SSE 解析辅助 ----------

type sseEvent struct {
	event string
	data  string
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var cur sseEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.event != "" || cur.data != "" {
				events = append(events, cur)
				cur = sseEvent{}
			}
		}
	}
	if cur.event != "" {
		events = append(events, cur)
	}
	return events
}

func eventNames(events []sseEvent) []string {
	names := make([]string, len(events))
	for i, e := range events {
		names[i] = e.event
	}
	return names
}

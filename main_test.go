package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// ---------- 测试辅助 ----------

// fakeSearchMessage 模拟 wbmux 搜索端点返回的 Anthropic Message。
const fakeSearchMessage = `{
  "id": "msg_wbmux_test",
  "type": "message",
  "role": "assistant",
  "model": "step-5-preview",
  "content": [
    {"type": "web_search_tool_result", "tool_use_id": "srvtoolu_test",
     "content": [{"type": "web_search_result", "url": "https://example.com/a", "title": "示例 A"}]},
    {"type": "text", "text": "根据搜索结果，答案是 42。",
     "citations": [{"type": "web_search_result_location", "url": "https://example.com/a", "cited_text": "42"}]}
  ],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 12, "output_tokens": 0}
}`

func newTestServer(t *testing.T, upstreamURL, searchURL string) *server {
	t.Helper()
	return newServer(config{
		listenAddr:     "127.0.0.1:0",
		upstreamURL:    upstreamURL,
		upstreamAPIKey: "up-key",
		searchURL:      searchURL,
		searchAPIKey:   "",
	})
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

// ---------- 搜索路由 ----------

func TestSearchRequestRoutesToSearchEndpoint(t *testing.T) {
	var gotAuth, gotBody string
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(fakeSearchMessage))
	}))
	defer search.Close()

	srv := newTestServer(t, "http://upstream.invalid", search.URL)
	reqBody := `{"model":"step-5-preview","max_tokens":4096,"messages":[{"role":"user","content":"搜索测试"}],` +
		`"tools":[{"type":"web_search_20260209","name":"web_search","max_uses":8}]}`

	rec := doJSON(t, srv, http.MethodPost, "/v1/messages", reqBody, map[string]string{
		"content-type":      "application/json",
		"x-api-key":         "client-dummy-key", // 应被剥离，不透传给搜索端点
		"anthropic-version": "2023-06-01",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "" {
		t.Errorf("search endpoint received x-api-key %q, want empty (client key must not leak)", gotAuth)
	}
	if !strings.Contains(gotBody, "web_search_20260209") {
		t.Errorf("search endpoint body missing web_search tool: %s", gotBody)
	}
	// 非流式：JSON 原样返回。
	if !strings.Contains(rec.Body.String(), "msg_wbmux_test") {
		t.Errorf("response missing search message id: %s", rec.Body.String())
	}
}

func TestSearchRequestStreamSynthesizesSSE(t *testing.T) {
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// wbmux 即使收到 stream:true 也返回 JSON。
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(fakeSearchMessage))
	}))
	defer search.Close()

	srv := newTestServer(t, "http://upstream.invalid", search.URL)
	reqBody := `{"model":"step-5-preview","stream":true,"messages":[{"role":"user","content":"搜索测试"}],` +
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
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // text + citations
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

	// 关键载荷校验。
	if !strings.Contains(rec.Body.String(), `"web_search_tool_result"`) {
		t.Error("SSE stream missing web_search_tool_result block")
	}
	if !strings.Contains(rec.Body.String(), "https://example.com/a") {
		t.Error("SSE stream missing search result URL")
	}
	if !strings.Contains(rec.Body.String(), "根据搜索结果，答案是 42。") {
		t.Error("SSE stream missing text delta")
	}
	if !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Error("SSE stream missing stop_reason")
	}
}

func TestSearchEndpointErrorPassthrough(t *testing.T) {
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"no web_search tool"}}`))
	}))
	defer search.Close()

	srv := newTestServer(t, "http://upstream.invalid", search.URL)
	rec := doJSON(t, srv, http.MethodPost, "/v1/messages",
		`{"model":"m","messages":[],"tools":[{"type":"web_search_20260209","name":"web_search"}]}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no web_search tool") {
		t.Errorf("error body not passed through: %s", rec.Body.String())
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

	srv := newTestServer(t, upstream.URL, "http://search.invalid")
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
	if gotAuth != "up-key" {
		t.Errorf("upstream x-api-key = %q, want configured up-key", gotAuth)
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

	srv := newTestServer(t, upstream.URL, "http://search.invalid")
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

	srv := newTestServer(t, upstream.URL, "http://search.invalid")
	rec := doJSON(t, srv, http.MethodPost, "/v1/messages/count_tokens", `{"model":"m","messages":[]}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q", gotPath)
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer(t, "http://up.invalid", "http://search.invalid")
	rec := doJSON(t, srv, http.MethodGet, "/healthz", ``, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "searchbridge") {
		t.Errorf("health body = %s", rec.Body.String())
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

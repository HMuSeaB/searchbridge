// searchbridge — 给任意 Anthropic Messages 端点补上原生 web_search 的本地网关。
//
// 工作原理：
//   - 普通聊天请求（tools 里没有 web_search 服务端工具）→ 原样转发到上游模型端点
//   - 联网搜索请求（ZCode / Claude Code 的 WebSearch 工具会发出一个只带
//     web_search 服务端工具的旁路请求）→ 转发到搜索端点（如 wbmux 的
//     /v1/search/messages），并把返回的 JSON 合成为 Anthropic SSE 事件流
//
// 这样，本身不支持 web_search 服务端工具的自定义模型（例如 StepFun）也能在
// ZCode / Claude Code 里获得可用的原生联网搜索，且普通对话完全不受影响。
//
// 零第三方依赖，单二进制，所有配置通过环境变量完成。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ---------- 配置 ----------

type config struct {
	listenAddr string // 监听地址，默认 127.0.0.1:8787

	upstreamURL    string // 普通聊天转发目标，如 https://api.stepfun.com/step_plan
	upstreamAPIKey string // 上游 API Key（可选，以 x-api-key 发送）
	upstreamBearer bool   // true 时以 Authorization: Bearer 发送上游 Key

	searchURL    string // 搜索端点，默认 http://127.0.0.1:55464/v1/search/messages
	searchAPIKey string // 搜索端点 Key（可选；wbmux 本机回环免密）
}

func loadConfig() config {
	return config{
		listenAddr:     env("SB_LISTEN", "127.0.0.1:8787"),
		upstreamURL:    strings.TrimRight(env("SB_UPSTREAM_URL", ""), "/"),
		upstreamAPIKey: env("SB_UPSTREAM_API_KEY", ""),
		upstreamBearer: env("SB_UPSTREAM_BEARER", "") == "1",
		searchURL:      env("SB_SEARCH_URL", "http://127.0.0.1:55464/v1/search/messages"),
		searchAPIKey:   env("SB_SEARCH_API_KEY", ""),
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// ---------- Anthropic 协议结构（只取需要的字段） ----------

type anthropicRequest struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Tools  json.RawMessage `json:"tools"`
}

type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type contentBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	ToolUseID  string          `json:"tool_use_id"`
	Content    json.RawMessage `json:"content"`
	Citations  json.RawMessage `json:"citations"`
	StopReason string          `json:"stop_reason"`
}

type anthropicMessage struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// hasWebSearchTool 判断请求是否声明了 Anthropic 服务端 web_search 工具。
// ZCode / Claude Code 的 WebSearch 旁路请求形态：
//
//	{"type":"web_search_20260209","name":"web_search","max_uses":8}
//
// 注意与客户端自定义工具 "WebSearch"（带 input_schema）区分。
func hasWebSearchTool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var tools []anthropicTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return false
	}
	for _, t := range tools {
		if t.Name == "web_search" {
			return true
		}
		if strings.HasPrefix(t.Type, "web_search_") {
			return true
		}
	}
	return false
}

// ---------- 服务器 ----------

type server struct {
	cfg    config
	client *http.Client
	nextID atomic.Uint64
}

func newServer(cfg config) *server {
	return &server{
		cfg: cfg,
		// 不设整体超时：LLM 流式响应可能持续很久，由请求上下文负责取消。
		client: &http.Client{},
	}
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/", s.handleMessages) // /v1/messages、/v1/messages/count_tokens 等
	mux.HandleFunc("/", s.handleMessages)
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"service":  "searchbridge",
		"upstream": s.cfg.upstreamURL,
		"search":   s.cfg.searchURL,
	})
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.cfg.upstreamURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	s.forwardUpstream(w, r, body)
}

// handleMessages 处理所有 /v1/** 请求：按是否携带 web_search 工具决定路由。
func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost && r.URL.Path != "/v1/models" {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "only POST is supported")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	// 非消息类端点（如 count_tokens）直接转发上游。
	if !strings.HasSuffix(r.URL.Path, "/messages") {
		if s.cfg.upstreamURL == "" {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "SB_UPSTREAM_URL is not configured")
			return
		}
		s.forwardUpstream(w, r, body)
		return
	}

	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}

	if hasWebSearchTool(req.Tools) {
		log.Printf("[search] %s %s model=%s stream=%t", r.Method, r.URL.Path, req.Model, req.Stream)
		s.handleSearch(w, r, body, req)
	} else {
		log.Printf("[chat]   %s %s model=%s stream=%t -> %s", r.Method, r.URL.Path, req.Model, req.Stream, s.cfg.upstreamURL)
		if s.cfg.upstreamURL == "" {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "SB_UPSTREAM_URL is not configured")
			return
		}
		s.forwardUpstream(w, r, body)
	}
	log.Printf("        done in %s", time.Since(start).Round(time.Millisecond))
}

// ---------- 搜索路由 ----------

// handleSearch 把 web_search 旁路请求转发到搜索端点。
// 搜索端点通常只返回 JSON（即使请求了 stream），这里按需合成为 SSE。
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request, body []byte, req anthropicRequest) {
	if s.cfg.searchURL == "" {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "SB_SEARCH_URL is not configured")
		return
	}

	resp, err := s.do(r.Context(), http.MethodPost, s.cfg.searchURL, body, s.searchHeaders(r))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "search endpoint unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read search response")
		return
	}

	if resp.StatusCode >= 400 {
		// 上游错误（如搜索端点不可用）原样透传状态与错误体。
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	// 搜索端点万一直接返回了 SSE 流，原样透传。
	if strings.Contains(resp.Header.Get("content-type"), "event-stream") {
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
		return
	}

	var msg anthropicMessage
	if err := json.Unmarshal(respBody, &msg); err != nil {
		// 不是标准 Message 结构时按纯 JSON 透传，避免吞掉响应。
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
		return
	}

	if req.Stream {
		s.writeSearchAsSSE(w, &msg)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
}

// writeSearchAsSSE 把搜索端点返回的完整 Message 合成为 Anthropic SSE 事件序列，
// 使按流式解析的客户端（ZCode / Claude Code）能正常消费。
func (s *server) writeSearchAsSSE(w http.ResponseWriter, msg *anthropicMessage) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)

	if msg.ID == "" {
		msg.ID = s.syntheticID("msg")
	}
	if msg.Model == "" {
		msg.Model = "searchbridge"
	}
	stopReason := msg.StopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	emit := func(event string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	emit("ping", map[string]string{"type": "ping"})
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msg.ID,
			"type":          "message",
			"role":          "assistant",
			"model":         msg.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  msg.Usage.InputTokens,
				"output_tokens": 0,
			},
		},
	})

	index := 0
	for _, block := range msg.Content {
		switch block.Type {
		case "web_search_tool_result":
			toolUseID := block.ToolUseID
			if toolUseID == "" {
				toolUseID = s.syntheticID("srvtoolu")
			}
			// 先补一个 server_tool_use 块，与 Anthropic 真实流式形态保持一致。
			emit("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":  "server_tool_use",
					"id":    toolUseID,
					"name":  "web_search",
					"input": map[string]any{},
				},
			})
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"},
			})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			index++

			content := block.Content
			if len(content) == 0 {
				content = json.RawMessage(`[]`)
			}
			emit("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":        "web_search_tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
				},
			})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			index++

		case "text":
			emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			if block.Text != "" {
				emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{"type": "text_delta", "text": block.Text},
				})
			}
			if len(block.Citations) > 0 {
				var citations []json.RawMessage
				if json.Unmarshal(block.Citations, &citations) == nil {
					for _, c := range citations {
						emit("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": index,
							"delta": map[string]any{"type": "citations_delta", "citation": c},
						})
					}
				}
			}
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			index++

		default:
			// 其他块（tool_use 等）按原始 JSON 透传为单个 content_block。
			raw, err := json.Marshal(block)
			if err != nil {
				continue
			}
			emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": json.RawMessage(raw),
			})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			index++
		}
	}

	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": msg.Usage.OutputTokens},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

// ---------- 上游转发 ----------

// forwardUpstream 原样转发请求到上游模型端点（流式 / 非流式均字节级透传）。
func (s *server) forwardUpstream(w http.ResponseWriter, r *http.Request, body []byte) {
	target := s.cfg.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	resp, err := s.do(r.Context(), r.Method, target, body, s.upstreamHeaders(r))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "content-length", "connection", "transfer-encoding":
			continue // 由 Go 自行计算
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ---------- 出站请求 ----------

func (s *server) do(ctx context.Context, method, url string, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return s.client.Do(req)
}

func (s *server) upstreamHeaders(r *http.Request) map[string]string {
	h := forwardHeaders(r)
	applyKey(h, s.cfg.upstreamAPIKey, s.cfg.upstreamBearer)
	return h
}

func (s *server) searchHeaders(r *http.Request) map[string]string {
	h := forwardHeaders(r)
	applyKey(h, s.cfg.searchAPIKey, false)
	return h
}

// forwardHeaders 复制客户端请求头，但替换掉客户端自带的上游认证信息。
func forwardHeaders(r *http.Request) map[string]string {
	skip := map[string]bool{
		"host": true, "content-length": true, "connection": true,
		"x-api-key": true, "authorization": true, "accept-encoding": true,
	}
	h := make(map[string]string)
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if skip[lk] {
			continue
		}
		h[k] = vs[0]
	}
	if h["Content-Type"] == "" {
		h["Content-Type"] = "application/json"
	}
	if h["Accept"] == "" {
		h["Accept"] = "application/json"
	}
	return h
}

func applyKey(h map[string]string, key string, bearer bool) {
	if key == "" {
		return
	}
	if bearer {
		h["Authorization"] = "Bearer " + key
		return
	}
	h["x-api-key"] = key
}

// ---------- 工具函数 ----------

func (s *server) syntheticID(prefix string) string {
	return fmt.Sprintf("%s_sb_%d_%d", prefix, time.Now().UnixMilli(), s.nextID.Add(1))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

func main() {
	cfg := loadConfig()
	if cfg.upstreamURL == "" {
		log.Printf("[warn] SB_UPSTREAM_URL 未配置：普通聊天请求将返回 502，仅搜索可用")
	}
	log.Printf("searchbridge listening on http://%s", cfg.listenAddr)
	log.Printf("  chat   -> %s", valueOr(cfg.upstreamURL, "(未配置)"))
	log.Printf("  search -> %s", valueOr(cfg.searchURL, "(未配置)"))

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           newServer(cfg).routes(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

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
	"strconv"
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

	searchURL       string        // 搜索端点（remote 后端），默认 wbmux 的 /v1/search/messages
	searchAPIKey    string        // 搜索端点 Key（可选；wbmux 本机回环免密）
	searchBackends  string        // 搜索后端链（顺序即优先级），默认 remote,ddg,bing
	searchCooldown  time.Duration // 后端失败后的冷却时间
	searchCacheTTL  time.Duration // 搜索结果缓存时间
	searchMerge     bool          // 合并模式：多后端并行查询 + 共识加权
	searchMergeWide int           // 合并模式同时查询的后端数
}

func loadConfig() config {
	return config{
		listenAddr:     env("SB_LISTEN", "127.0.0.1:8787"),
		upstreamURL:    strings.TrimRight(env("SB_UPSTREAM_URL", ""), "/"),
		upstreamAPIKey: env("SB_UPSTREAM_API_KEY", ""),
		upstreamBearer: env("SB_UPSTREAM_BEARER", "") == "1",

		searchURL:       searchURLEnv(),
		searchAPIKey:    env("SB_SEARCH_API_KEY", ""),
		searchBackends:  env("SB_SEARCH_BACKENDS", ""),
		searchCooldown:  time.Duration(envInt("SB_SEARCH_COOLDOWN_MIN", 10)) * time.Minute,
		searchCacheTTL:  time.Duration(envInt("SB_SEARCH_CACHE_MIN", 30)) * time.Minute,
		searchMerge:     env("SB_SEARCH_MERGE", "") == "1",
		searchMergeWide: envInt("SB_SEARCH_MERGE_WIDTH", 3),
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// searchURLEnv 读取搜索端点；未设置时默认指向本机 wbmux，
// 设为 off/none/- 可显式禁用 remote 后端。
func searchURLEnv() string {
	v := strings.TrimSpace(os.Getenv("SB_SEARCH_URL"))
	switch strings.ToLower(v) {
	case "off", "none", "-":
		return ""
	case "":
		return "http://127.0.0.1:55464/v1/search/messages"
	default:
		return v
	}
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
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	Citations  json.RawMessage `json:"citations,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
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
	chain  *backendChain
	nextID atomic.Uint64
}

func newServer(cfg config) *server {
	backends := buildBackends(cfg)
	chain := newBackendChain(backends, cfg.searchCooldown, cfg.searchCacheTTL)
	chain.merge = cfg.searchMerge
	chain.mergeWidth = cfg.searchMergeWide
	return &server{
		cfg:    cfg,
		client: &http.Client{},
		chain:  chain,
	}
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/search", s.handleSearchJSON) // 结构化 JSON 搜索端点（脚本 / MCP 用）
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
		"backends": s.chain.Names(),
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

// extractSearchQuery 从 WebSearch 旁路请求中提取搜索词。
// ZCode / Claude Code 的旁路请求形态：user 消息为
// "Perform a web search for the query: <query>"。
func extractSearchQuery(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	var lastUser string
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		// content 可能是字符串或内容块数组，两种都支持。
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			lastUser = s
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					lastUser = b.Text
				}
			}
		}
	}
	const marker = "Perform a web search for the query:"
	if i := strings.Index(lastUser, marker); i != -1 {
		return strings.TrimSpace(lastUser[i+len(marker):])
	}
	return strings.TrimSpace(lastUser)
}

// searchLimit 从请求声明的 web_search 工具中取 max_uses 作为结果条数。
func searchLimit(body []byte) int {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Tools) == 0 {
		return 5
	}
	var tools []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		MaxUses int    `json:"max_uses"`
	}
	if json.Unmarshal(req.Tools, &tools) != nil {
		return 5
	}
	for _, t := range tools {
		if t.Name == "web_search" || strings.HasPrefix(t.Type, "web_search_") {
			if t.MaxUses >= 1 && t.MaxUses <= 10 {
				return t.MaxUses
			}
			return 5
		}
	}
	return 5
}

// handleSearch 处理 WebSearch 旁路请求：通过后端链路取结果，合成为
// Anthropic Message（流式请求则合成 SSE）。
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request, body []byte, req anthropicRequest) {
	query := extractSearchQuery(body)
	if len(query) < 2 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "search query is empty or too short")
		return
	}
	limit := searchLimit(body)

	start := time.Now()
	results, via, err := s.chain.Search(r.Context(), query, limit)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"all search backends failed: "+err.Error())
		return
	}
	log.Printf("[search] query=%q limit=%d via=%s (%d results, %s)", query, limit, via, len(results), time.Since(start).Round(time.Millisecond))

	msg := buildSearchMessage(req.Model, query, results)
	if req.Stream {
		s.writeSearchAsSSE(w, &msg)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// handleSearchJSON 提供结构化 JSON 搜索端点（与 wbmux 的 /v1/search 兼容）：
// GET/POST /v1/search?q=关键词&n=5
func (s *server) handleSearchJSON(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 5
	if n := r.URL.Query().Get("n"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v >= 1 && v <= 10 {
			limit = v
		}
	}
	if query == "" && r.Method == http.MethodPost {
		var payload struct {
			Query string `json:"q"`
			Limit int    `json:"n"`
		}
		if json.NewDecoder(r.Body).Decode(&payload) == nil {
			query = strings.TrimSpace(payload.Query)
			if payload.Limit >= 1 && payload.Limit <= 10 {
				limit = payload.Limit
			}
		}
	}
	if len(query) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "query parameter 'q' is required (min 2 chars)",
		})
		return
	}

	start := time.Now()
	results, via, err := s.chain.Search(r.Context(), query, limit)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": "all search backends failed: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"query":            query,
		"via":              via,
		"results":          results,
		"stats":            s.chain.LastStats(),
		"response_time_ms": time.Since(start).Milliseconds(),
	})
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
	srv := newServer(cfg)
	log.Printf("searchbridge listening on http://%s", cfg.listenAddr)
	log.Printf("  chat    -> %s", valueOr(cfg.upstreamURL, "(未配置)"))
	log.Printf("  search  -> 后端链: %s", strings.Join(srv.chain.Names(), " -> "))

	httpSrv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

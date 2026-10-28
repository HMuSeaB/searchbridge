package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// searchResult 是各搜索后端归一化后的结果。
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// searchBackend 是一个搜索后端的统一接口。
type searchBackend interface {
	Name() string
	// Search 执行一次搜索，返回归一化结果；失败时返回 error（触发链路故障转移）。
	Search(ctx context.Context, query string, limit int) ([]searchResult, error)
}

// backendChain 按优先级依次尝试多个后端：
//   - 某个后端失败后进入冷却期（默认 10 分钟），期间直接跳过；
//   - 查询结果按 (query, limit) 缓存（默认 30 分钟），避免重复请求；
//   - 全部后端失败时才返回错误。
type backendChain struct {
	backends []searchBackend
	cooldown time.Duration
	cacheTTL time.Duration

	mu       sync.Mutex
	failedAt map[string]time.Time
	cache    map[string]cacheEntry
}

type cacheEntry struct {
	results  []searchResult
	storedAt time.Time
}

func newBackendChain(backends []searchBackend, cooldown, cacheTTL time.Duration) *backendChain {
	if cooldown <= 0 {
		cooldown = 10 * time.Minute
	}
	if cacheTTL <= 0 {
		cacheTTL = 30 * time.Minute
	}
	return &backendChain{
		backends: backends,
		cooldown: cooldown,
		cacheTTL: cacheTTL,
		failedAt: make(map[string]time.Time),
		cache:    make(map[string]cacheEntry),
	}
}

// Names 返回链路中的后端名称（按顺序）。
func (c *backendChain) Names() []string {
	names := make([]string, len(c.backends))
	for i, b := range c.backends {
		names[i] = b.Name()
	}
	return names
}

// Search 依次尝试链路中的后端，返回首个成功的结果。
func (c *backendChain) Search(ctx context.Context, query string, limit int) ([]searchResult, string, error) {
	key := fmt.Sprintf("%d|%s", limit, query)

	c.mu.Lock()
	if entry, ok := c.cache[key]; ok && time.Since(entry.storedAt) < c.cacheTTL {
		results := entry.results
		c.mu.Unlock()
		return results, "cache", nil
	}
	c.mu.Unlock()

	var lastErr error
	for _, b := range c.backends {
		if c.inCooldown(b.Name()) {
			continue
		}
		start := time.Now()
		results, err := b.Search(ctx, query, limit)
		if err != nil {
			log.Printf("[search] backend %s failed after %s: %v", b.Name(), time.Since(start).Round(time.Millisecond), err)
			c.markFailed(b.Name())
			lastErr = err
			continue
		}
		if len(results) == 0 {
			// 空结果视为失败，尝试下一个后端。
			log.Printf("[search] backend %s returned no results", b.Name())
			c.markFailed(b.Name())
			lastErr = fmt.Errorf("backend %s returned no results", b.Name())
			continue
		}
		c.markOK(b.Name())
		log.Printf("[search] backend %s returned %d results in %s", b.Name(), len(results), time.Since(start).Round(time.Millisecond))

		c.mu.Lock()
		c.cache[key] = cacheEntry{results: results, storedAt: time.Now()}
		c.mu.Unlock()
		return results, b.Name(), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all search backends are in cooldown")
	}
	return nil, "", lastErr
}

func (c *backendChain) inCooldown(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.failedAt[name]
	return ok && time.Since(at) < c.cooldown
}

func (c *backendChain) markFailed(name string) {
	c.mu.Lock()
	c.failedAt[name] = time.Now()
	c.mu.Unlock()
}

func (c *backendChain) markOK(name string) {
	c.mu.Lock()
	delete(c.failedAt, name)
	c.mu.Unlock()
}

// ---------- 后端构造 ----------

// buildBackends 按配置的后端名称列表构造链路。
// 支持：remote（wbmux 等 Anthropic 搜索端点）、ddg、bing、searxng、tavily、brave。
func buildBackends(cfg config) []searchBackend {
	spec := strings.TrimSpace(cfg.searchBackends)
	if spec == "" {
		// 未显式配置时的默认链路：有 remote 端点就带上，再兜底免费后端。
		if cfg.searchURL != "" {
			spec = "remote,ddg,bing"
		} else {
			spec = "ddg,bing"
		}
	}

	client := newHTTPClient(20 * time.Second)
	var backends []searchBackend
	for _, name := range strings.Split(spec, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		switch name {
		case "remote":
			if cfg.searchURL != "" {
				backends = append(backends, &remoteBackend{
					name:       "remote",
					endpoint:   cfg.searchURL,
					apiKey:     cfg.searchAPIKey,
					model:      env("SB_SEARCH_MODEL", "web-search"),
					httpClient: client,
				})
			} else {
				log.Printf("[warn] 后端 remote 已跳过：未配置 SB_SEARCH_URL")
			}
		case "ddg":
			backends = append(backends, &ddgLiteBackend{
				endpoint:   env("SB_DDG_URL", "https://lite.duckduckgo.com/lite/"),
				httpClient: client,
			})
		case "bing":
			backends = append(backends, &bingBackend{
				host:       env("SB_BING_HOST", "https://cn.bing.com"),
				httpClient: client,
			})
		case "searxng":
			if url := env("SB_SEARXNG_URL", ""); url != "" {
				backends = append(backends, &searxngBackend{
					baseURL:    strings.TrimRight(url, "/"),
					httpClient: client,
				})
			} else {
				log.Printf("[warn] 后端 searxng 已跳过：未配置 SB_SEARXNG_URL")
			}
		case "tavily":
			if key := env("SB_TAVILY_API_KEY", ""); key != "" {
				backends = append(backends, &tavilyBackend{
					apiKey:     key,
					baseURL:    env("SB_TAVILY_URL", "https://api.tavily.com/search"),
					httpClient: client,
				})
			} else {
				log.Printf("[warn] 后端 tavily 已跳过：未配置 SB_TAVILY_API_KEY")
			}
		case "brave":
			if key := env("SB_BRAVE_API_KEY", ""); key != "" {
				backends = append(backends, &braveBackend{
					apiKey:     key,
					baseURL:    env("SB_BRAVE_URL", "https://api.search.brave.com"),
					httpClient: client,
				})
			} else {
				log.Printf("[warn] 后端 brave 已跳过：未配置 SB_BRAVE_API_KEY")
			}
		case "":
		default:
			log.Printf("[warn] 未知搜索后端 %q，已跳过", name)
		}
	}
	return backends
}

// ---------- Anthropic 响应合成 ----------

// buildSearchMessage 把归一化结果合成为一个 Anthropic Message（仅含 web_search_tool_result）。
// 客户端的模型随后会基于这些结果生成带引用的回答。
func buildSearchMessage(model string, query string, results []searchResult) anthropicMessage {
	items := make([]map[string]any, 0, len(results))
	for _, r := range results {
		items = append(items, map[string]any{
			"type":  "web_search_result",
			"url":   r.URL,
			"title": r.Title,
		})
	}
	content := []map[string]any{{
		"type":        "web_search_tool_result",
		"tool_use_id": fmt.Sprintf("srvtoolu_sb_%d", time.Now().UnixNano()),
		"content":     items,
	}}

	// 序列化为 contentBlock 以便复用 SSE 合成逻辑。
	contentJSON, _ := json.Marshal(content)
	var blocks []contentBlock
	_ = json.Unmarshal(contentJSON, &blocks)

	if model == "" {
		model = "searchbridge"
	}
	return anthropicMessage{
		ID:      fmt.Sprintf("msg_sb_%d", time.Now().UnixNano()),
		Type:    "message",
		Role:    "assistant",
		Model:   model,
		Content: blocks,
	}
}

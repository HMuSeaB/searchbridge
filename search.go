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
	backends   []searchBackend
	cooldown   time.Duration
	cacheTTL   time.Duration
	merge      bool
	mergeWidth int

	mu        sync.Mutex
	failedAt  map[string]time.Time
	cache     map[string]cacheEntry
	lastStats curationStats
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
		merge:    false,
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

// Search 依次尝试链路中的后端；每个后端的结果都会经过精选（垃圾过滤 /
// 相关性分级 / 去重 / 排序）。全垃圾的结果会触发下一个后端，但不进入冷却
// （该后端对其他查询可能仍然可用）；错误与空结果才进入冷却。
// merge 模式下改为多后端并行查询 + 共识加权合并。
func (c *backendChain) Search(ctx context.Context, query string, limit int) ([]searchResult, string, error) {
	key := fmt.Sprintf("%d|%s", limit, query)

	c.mu.Lock()
	if entry, ok := c.cache[key]; ok && time.Since(entry.storedAt) < c.cacheTTL {
		results := entry.results
		c.mu.Unlock()
		return results, "cache", nil
	}
	c.mu.Unlock()

	if c.merge {
		return c.searchMerged(ctx, query, limit, key)
	}

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
		curated, stats := curate(results, query, limit)
		if !hasUsableResults(curated) {
			// 全是垃圾/不相关：不冷却（换个查询它可能就正常），直接试下一个。
			log.Printf("[search] backend %s returned only junk (good=%d partial=%d junk=%d deduped=%d), trying next",
				b.Name(), stats.Good, stats.Partial, stats.Junk, stats.Deduped)
			lastErr = fmt.Errorf("backend %s returned only irrelevant results", b.Name())
			continue
		}
		c.markOK(b.Name())
		log.Printf("[search] backend %s returned %d results in %s, curated good=%d partial=%d junk=%d deduped=%d",
			b.Name(), len(results), time.Since(start).Round(time.Millisecond), stats.Good, stats.Partial, stats.Junk, stats.Deduped)

		out := curatedToResults(curated)
		c.mu.Lock()
		c.lastStats = stats
		c.cache[key] = cacheEntry{results: out, storedAt: time.Now()}
		c.mu.Unlock()
		return out, b.Name(), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all search backends are in cooldown")
	}
	return nil, "", lastErr
}

// searchMerged 并行查询最多 mergeWidth 个健康后端，各自精选后按
// 共识加权合并。某个后端失败/全垃圾不影响其他后端的结果。
func (c *backendChain) searchMerged(ctx context.Context, query string, limit int, key string) ([]searchResult, string, error) {
	c.mu.Lock()
	var picked []searchBackend
	for _, b := range c.backends {
		if len(picked) >= c.mergeWidth {
			break
		}
		if !c.inCooldownLocked(b.Name()) {
			picked = append(picked, b)
		}
	}
	c.mu.Unlock()

	if len(picked) == 0 {
		return nil, "", fmt.Errorf("all search backends are in cooldown")
	}

	type outcome struct {
		name    string
		curated []curatedResult
		stats   curationStats
		err     error
	}
	ch := make(chan outcome, len(picked))
	for _, b := range picked {
		go func(b searchBackend) {
			start := time.Now()
			results, err := b.Search(ctx, query, limit)
			if err != nil {
				log.Printf("[search] backend %s failed after %s: %v", b.Name(), time.Since(start).Round(time.Millisecond), err)
				ch <- outcome{name: b.Name(), err: err}
				return
			}
			if len(results) == 0 {
				ch <- outcome{name: b.Name(), err: fmt.Errorf("no results")}
				return
			}
			curated, stats := curate(results, query, limit)
			for i := range curated {
				curated[i].sources = []string{b.Name()}
			}
			log.Printf("[search] backend %s returned %d results in %s, curated good=%d partial=%d junk=%d deduped=%d",
				b.Name(), len(results), time.Since(start).Round(time.Millisecond), stats.Good, stats.Partial, stats.Junk, stats.Deduped)
			ch <- outcome{name: b.Name(), curated: curated, stats: stats}
		}(b)
	}

	var perBackend [][]curatedResult
	var used []string
	var lastErr error
	var agg curationStats
	for range picked {
		o := <-ch
		if o.err != nil {
			c.markFailed(o.name)
			lastErr = o.err
			continue
		}
		agg.Total += o.stats.Total
		agg.Good += o.stats.Good
		agg.Partial += o.stats.Partial
		agg.Junk += o.stats.Junk
		agg.Deduped += o.stats.Deduped
		if len(o.curated) == 0 {
			continue // 全垃圾，不冷却
		}
		c.markOK(o.name)
		perBackend = append(perBackend, o.curated)
		used = append(used, o.name)
	}
	if len(perBackend) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no backend returned relevant results")
		}
		return nil, "", lastErr
	}

	merged := mergeCurated(perBackend, limit)
	out := curatedToResults(merged)
	via := strings.Join(used, "+")
	c.mu.Lock()
	c.lastStats = agg
	c.cache[key] = cacheEntry{results: out, storedAt: time.Now()}
	c.mu.Unlock()
	log.Printf("[search] merged %d results from %s", len(out), via)
	return out, via, nil
}

// curatedToResults 去掉评分包装，返回纯结果列表。
func curatedToResults(curated []curatedResult) []searchResult {
	out := make([]searchResult, len(curated))
	for i, cr := range curated {
		out[i] = cr.searchResult
	}
	return out
}

// LastStats 返回最近一次搜索的精选统计（供 /v1/search 端点暴露质量信息）。
func (c *backendChain) LastStats() curationStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastStats
}

// inCooldownLocked 是 inCooldown 的无锁版本，调用方必须已持有 c.mu。
func (c *backendChain) inCooldownLocked(name string) bool {
	at, ok := c.failedAt[name]
	return ok && time.Since(at) < c.cooldown
}

func (c *backendChain) inCooldown(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inCooldownLocked(name)
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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// ---------- 后端链：顺序、故障转移、冷却、缓存 ----------

func TestChainTriesBackendsInOrder(t *testing.T) {
	failing := &stubBackend{name: "failing", err: errors.New("boom")}
	ok := &stubBackend{name: "ok", results: []searchResult{{Title: "query 结果", URL: "https://a.example"}}}
	chain := newBackendChain([]searchBackend{failing, ok}, 10*time.Minute, 30*time.Minute)

	results, via, err := chain.Search(context.Background(), "query", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if via != "ok" || len(results) != 1 {
		t.Errorf("via = %q, results = %d, want ok/1", via, len(results))
	}
	if failing.calls != 1 || ok.calls != 1 {
		t.Errorf("calls: failing=%d ok=%d, want 1/1", failing.calls, ok.calls)
	}
}

func TestChainSkipsCooldownBackends(t *testing.T) {
	failing := &stubBackend{name: "failing", err: errors.New("boom")}
	ok := &stubBackend{name: "ok", results: []searchResult{{Title: "query 结果", URL: "https://a.example"}}}
	chain := newBackendChain([]searchBackend{failing, ok}, 10*time.Minute, 0)

	// 第一次：failing 失败一次，进入冷却。
	if _, _, err := chain.Search(context.Background(), "query one", 5); err != nil {
		t.Fatalf("first search error = %v", err)
	}
	// 第二次（不同查询，绕过缓存）：failing 应在冷却期内被跳过。
	if _, via, err := chain.Search(context.Background(), "query two", 5); err != nil {
		t.Fatalf("second search error = %v", err)
	} else if via != "ok" {
		t.Errorf("via = %q, want ok", via)
	}
	if failing.calls != 1 {
		t.Errorf("failing calls = %d, want 1 (cooldown should skip it)", failing.calls)
	}
	if ok.calls != 2 {
		t.Errorf("ok calls = %d, want 2", ok.calls)
	}
}

func TestChainCacheHits(t *testing.T) {
	backend := &stubBackend{name: "b", results: []searchResult{{Title: "query 结果", URL: "https://a.example"}}}
	chain := newBackendChain([]searchBackend{backend}, 10*time.Minute, 30*time.Minute)

	for i := 0; i < 3; i++ {
		if _, _, err := chain.Search(context.Background(), "same query", 5); err != nil {
			t.Fatalf("search %d error = %v", i, err)
		}
	}
	if backend.calls != 1 {
		t.Errorf("backend calls = %d, want 1 (cache should serve repeats)", backend.calls)
	}

	// 显式验证第四次命中缓存（via=cache）。
	if _, via, _ := chain.Search(context.Background(), "same query", 5); via != "cache" {
		t.Errorf("via = %q, want cache", via)
	}
}

func TestChainAllCooldown(t *testing.T) {
	failing := &stubBackend{name: "failing", err: errors.New("boom")}
	chain := newBackendChain([]searchBackend{failing}, 10*time.Minute, 0)

	_, _, _ = chain.Search(context.Background(), "query one", 5)
	_, _, err := chain.Search(context.Background(), "query two", 5)
	if err == nil {
		t.Fatal("expected error when all backends are in cooldown")
	}
}

func TestChainMergeMode(t *testing.T) {
	a := &stubBackend{name: "a", results: []searchResult{
		{Title: "query 共享结果", URL: "https://example.com/shared"},
		{Title: "query A 独有", URL: "https://a.com/only"},
	}}
	b := &stubBackend{name: "b", results: []searchResult{
		{Title: "query 共享结果", URL: "https://www.example.com/shared"},
		{Title: "query B 独有", URL: "https://b.com/only"},
	}}
	chain := newBackendChain([]searchBackend{a, b}, 10*time.Minute, 0)
	chain.merge = true
	chain.mergeWidth = 2

	results, via, err := chain.Search(context.Background(), "query 测试", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	// 两个后端都成功（完成顺序不定，via 顺序不保证）。
	if via != "a+b" && via != "b+a" {
		t.Errorf("via = %q, want a+b or b+a", via)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (shared deduped)", len(results))
	}
	// 共识结果（两个后端都返回）应排第一，保留哪个副本均可。
	if normalizeURL(results[0].URL) != "example.com/shared" {
		t.Errorf("results[0] = %s, want shared (consensus boost)", results[0].URL)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("calls: a=%d b=%d, want 1/1", a.calls, b.calls)
	}
}

// ---------- 结果合成 ----------

func TestBuildSearchMessage(t *testing.T) {
	results := []searchResult{
		{Title: "标题A", URL: "https://a.example", Snippet: "摘要A"},
		{Title: "标题B", URL: "https://b.example"},
	}
	msg := buildSearchMessage("step-5-preview", "查询", results)

	if msg.Role != "assistant" || msg.Model != "step-5-preview" {
		t.Errorf("role/model = %q/%q", msg.Role, msg.Model)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "web_search_tool_result" {
		t.Fatalf("content = %+v, want single web_search_tool_result", msg.Content)
	}
	block := msg.Content[0]
	if block.ToolUseID == "" {
		t.Error("missing tool_use_id")
	}
	var items []struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(block.Content, &items); err != nil {
		t.Fatalf("content items not parseable: %v", err)
	}
	if len(items) != 2 || items[0].URL != "https://a.example" || items[1].Title != "标题B" {
		t.Errorf("items = %+v", items)
	}
}

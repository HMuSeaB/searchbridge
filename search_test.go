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
	ok := &stubBackend{name: "ok", results: []searchResult{{Title: "T", URL: "https://a"}}}
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
	ok := &stubBackend{name: "ok", results: []searchResult{{Title: "T", URL: "https://a"}}}
	chain := newBackendChain([]searchBackend{failing, ok}, 10*time.Minute, 0)

	// 第一次：failing 失败一次，进入冷却。
	if _, _, err := chain.Search(context.Background(), "q1", 5); err != nil {
		t.Fatalf("first search error = %v", err)
	}
	// 第二次（不同查询，绕过缓存）：failing 应在冷却期内被跳过。
	if _, via, err := chain.Search(context.Background(), "q2", 5); err != nil {
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
	backend := &stubBackend{name: "b", results: []searchResult{{Title: "T", URL: "https://a"}}}
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

	_, _, _ = chain.Search(context.Background(), "q1", 5)
	_, _, err := chain.Search(context.Background(), "q2", 5)
	if err == nil {
		t.Fatal("expected error when all backends are in cooldown")
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

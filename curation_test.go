package main

import (
	"context"
	"testing"
)

// ---------- 分词 ----------

func TestTokenizeQuery(t *testing.T) {
	tokens := tokenizeQuery("golang 1.24 新特性")
	texts := make(map[string]float64)
	for _, tk := range tokens {
		texts[tk.text] = tk.weight
	}
	want := map[string]float64{
		"golang 1.24 新特性": 3, // 完整短语
		"golang":          2, // 拉丁整词
		"1.24":            2, // 数字整词
		"新特性":             2, // CJK 整段
		"新特":              1, // CJK 二元组
		"特性":              1, // CJK 二元组
	}
	for text, weight := range want {
		got, ok := texts[text]
		if !ok {
			t.Errorf("token %q missing from %v", text, texts)
			continue
		}
		if got != weight {
			t.Errorf("token %q weight = %v, want %v", text, got, weight)
		}
	}
}

func TestTokenizeQueryEdgeCases(t *testing.T) {
	if got := tokenizeQuery(""); got != nil {
		t.Errorf("empty query tokens = %v, want nil", got)
	}
	// 单字 CJK：短语与单字相同，去重后只剩一个 token。
	single := tokenizeQuery("猫")
	if len(single) != 1 || single[0].text != "猫" {
		t.Errorf("single CJK char tokens = %+v", single)
	}
	// 二元组不越界。
	for _, tk := range tokenizeQuery("测试abc长句子的二元组切分") {
		if len([]rune(tk.text)) > 20 {
			t.Errorf("unexpected long token %q", tk.text)
		}
	}
}

// ---------- 垃圾过滤 ----------

func TestIsSpam(t *testing.T) {
	cases := []struct {
		name string
		r    searchResult
		want bool
	}{
		{"gambling title", searchResult{Title: "澳门娱乐城注册送体验金", URL: "https://x.com"}, true},
		{"casino", searchResult{Title: "Best online casino games", URL: "https://x.com"}, true},
		{"normal doc", searchResult{Title: "Go 1.24 Release Notes", URL: "https://go.dev/doc/go1.24"}, false},
		{"normal chinese", searchResult{Title: "Go 1.24 新特性介绍", URL: "https://juejin.cn/post/123"}, false},
		{"github repo", searchResult{Title: "golang/go: The Go programming language", URL: "https://github.com/golang/go"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSpam(c.r); got != c.want {
				t.Errorf("isSpam(%+v) = %v, want %v", c.r, got, c.want)
			}
		})
	}
}

// ---------- 评分与分级 ----------

func TestScoreResult(t *testing.T) {
	tokens := tokenizeQuery("golang 1.24 新特性")
	phrase := "golang 1.24 新特性"

	// 标题含全部关键词 → good。
	good := searchResult{Title: "Go 1.24 新特性：golang 版本更新详解", URL: "https://go.dev/doc/go1.24"}
	if _, grade := scoreResult(good, tokens, phrase); grade != gradeGood {
		t.Errorf("good result grade = %v, want good", grade)
	}

	// 完全无关 → junk。
	junk := searchResult{Title: "今天天气真好适合出门散步", URL: "https://weather.example.com"}
	if _, grade := scoreResult(junk, tokens, phrase); grade != gradeJunk {
		t.Errorf("junk result grade = %v, want junk", grade)
	}

	// 只命中一个词 → partial。
	partial := searchResult{Title: "golang 基础入门教程", URL: "https://example.com/go"}
	_, grade := scoreResult(partial, tokens, phrase)
	if grade != gradePartial {
		t.Errorf("partial result grade = %v, want partial", grade)
	}
}

// ---------- 精选主流程 ----------

func TestCurateFiltersAndSorts(t *testing.T) {
	results := []searchResult{
		{Title: "澳门赌场娱乐城注册送体验金", URL: "https://spam1.com"},                                                        // spam
		{Title: "今天天气真好适合出门散步", URL: "https://weather.example.com"},                                               // junk（不相关）
		{Title: "Go 1.24 新特性：golang 版本更新详解", URL: "https://go.dev/doc/go1.24", Snippet: "Go 1.24 发布于 2025 年 2 月"}, // good，标题最全
		{Title: "Go 1.24 新特性：golang 版本更新详解", URL: "https://go.dev/doc/go1.24/", Snippet: "重复 URL"},                // dedup（URL 规范化后相同）
		{Title: "golang 1.24 发布说明翻译", URL: "https://juejin.cn/post/123"},                                          // good，略弱
		{Title: "golang 基础入门教程", URL: "https://example.com/go"},                                                   // partial
	}

	curated, stats := curate(results, "golang 1.24 新特性", 5)

	if stats.Total != 6 {
		t.Errorf("stats.Total = %d, want 6", stats.Total)
	}
	if stats.Junk < 2 { // spam + 不相关各一
		t.Errorf("stats.Junk = %d, want >= 2", stats.Junk)
	}
	if stats.Deduped < 1 {
		t.Errorf("stats.Deduped = %d, want >= 1", stats.Deduped)
	}
	if len(curated) == 0 {
		t.Fatal("curated is empty")
	}
	// 第一条应当是标题最全的 good 结果。
	if curated[0].URL != "https://go.dev/doc/go1.24" {
		t.Errorf("curated[0] = %s, want go.dev doc", curated[0].URL)
	}
	if curated[0].grade != gradeGood {
		t.Errorf("curated[0] grade = %v, want good", curated[0].grade)
	}
	// 所有返回结果都必须非垃圾。
	for _, cr := range curated {
		if cr.grade == gradeJunk {
			t.Errorf("junk result leaked through: %+v", cr.searchResult)
		}
		if isSpam(cr.searchResult) {
			t.Errorf("spam result leaked through: %+v", cr.searchResult)
		}
	}
}

func TestCurateLimitAndPartialFill(t *testing.T) {
	results := []searchResult{
		{Title: "golang 基础入门教程", URL: "https://a.com"}, // partial
		{Title: "golang 并发编程模式", URL: "https://b.com"}, // partial
		{Title: "golang 性能优化指南", URL: "https://c.com"}, // partial
	}
	curated, _ := curate(results, "golang 并发", 2)
	if len(curated) != 2 {
		t.Errorf("len(curated) = %d, want 2 (limit respected)", len(curated))
	}
}

func TestCurateAllJunk(t *testing.T) {
	results := []searchResult{
		{Title: "澳门赌场娱乐城", URL: "https://spam.com"},
		{Title: "今天天气真好", URL: "https://weather.com"},
	}
	curated, _ := curate(results, "golang 1.24", 5)
	if len(curated) != 0 {
		t.Errorf("curated = %d results, want 0 (all junk)", len(curated))
	}
}

// ---------- 规范化 ----------

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://www.Go.dev/doc/go1.24", "go.dev/doc/go1.24"},
		{"http://example.com/a/b?utm_source=x&id=1", "example.com/a/b"},
		{"https://example.com", "example.com"},
		{"example.com/path", "example.com/path"},
		{"https://example.com/a", "example.com/a"},         // 不同路径不是同一页面
		{"https://example.com/b", "example.com/b"},         // 不同路径不是同一页面
		{"https://Example.com/a/", "example.com/a"},        // 末尾斜杠
		{"https://example.com/a#section", "example.com/a"}, // fragment
	}
	for _, c := range cases {
		if got := normalizeURL(c.in); got != c.want {
			t.Errorf("normalizeURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeTitle(t *testing.T) {
	a := normalizeTitle("Go 1.24 新特性 - 官方发布说明")
	b := normalizeTitle("go1.24新特性官方发布说明")
	if a != b {
		t.Errorf("normalized titles differ: %q vs %q", a, b)
	}
}

// ---------- 合并共识 ----------

func TestMergeCuratedConsensus(t *testing.T) {
	// 后端 A 和 B 都返回了 example.com/shared → 应获得共识加权并去重。
	a := []curatedResult{
		{searchResult: searchResult{Title: "共享结果", URL: "https://example.com/shared"}, score: 1.0, grade: gradeGood, sources: []string{"a"}},
		{searchResult: searchResult{Title: "A 独有", URL: "https://a.com/only"}, score: 0.8, grade: gradeGood, sources: []string{"a"}},
	}
	b := []curatedResult{
		{searchResult: searchResult{Title: "共享结果", URL: "https://www.example.com/shared"}, score: 0.9, grade: gradeGood, sources: []string{"b"}},
		{searchResult: searchResult{Title: "B 独有", URL: "https://b.com/only"}, score: 0.7, grade: gradeGood, sources: []string{"b"}},
	}

	merged := mergeCurated([][]curatedResult{a, b}, 5)
	if len(merged) != 3 {
		t.Fatalf("merged = %d results, want 3 (dedup)", len(merged))
	}
	// 共识结果应排第一。
	if merged[0].URL != "https://example.com/shared" {
		t.Errorf("merged[0] = %s, want shared", merged[0].URL)
	}
	if merged[0].consensus != 2 {
		t.Errorf("merged[0] consensus = %d, want 2", merged[0].consensus)
	}
	if merged[0].score <= 1.0 {
		t.Errorf("merged[0] score = %v, want > 1.0 (consensus boost)", merged[0].score)
	}
	// 其余按分数降序。
	for i := 1; i < len(merged); i++ {
		if merged[i].score > merged[i-1].score {
			t.Errorf("merged not sorted at %d: %v > %v", i, merged[i].score, merged[i-1].score)
		}
	}
}

// ---------- 链路集成 ----------

func TestChainJunkResultsTriggerFailoverWithoutCooldown(t *testing.T) {
	junkBackend := &stubBackend{name: "junk", results: []searchResult{
		{Title: "澳门赌场娱乐城注册送体验金", URL: "https://spam.com"},
		{Title: "今天天气真好", URL: "https://weather.com"},
	}}
	goodBackend := &stubBackend{name: "good", results: []searchResult{
		{Title: "golang 1.24 新特性详解", URL: "https://go.dev/doc/go1.24"},
	}}
	chain := newBackendChain([]searchBackend{junkBackend, goodBackend}, 10*60, 0)

	results, via, err := chain.Search(context.Background(), "golang 1.24 新特性", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if via != "good" || len(results) != 1 {
		t.Fatalf("via = %q results = %d, want good/1", via, len(results))
	}

	// 垃圾后端不应被冷却：换一个查询它仍然会被尝试。
	junkBackend.calls = 0
	_, _, _ = chain.Search(context.Background(), "另一个查询", 5)
	if junkBackend.calls != 1 {
		t.Errorf("junk backend calls = %d, want 1 (no cooldown after junk)", junkBackend.calls)
	}
}

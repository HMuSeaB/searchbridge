package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// delayedStub 是带可控延迟的后端，用于验证对冲竞速的时序。
// 遵守 ctx 取消——落败方被取消时应立刻返回，否则测试会拖满延迟。
type delayedStub struct {
	name    string
	delay   time.Duration
	results []searchResult
	err     error
	calls   int32
}

func (b *delayedStub) Name() string { return b.name }

func (b *delayedStub) Search(ctx context.Context, _ string, _ int) ([]searchResult, error) {
	atomic.AddInt32(&b.calls, 1)
	if b.delay > 0 {
		select {
		case <-time.After(b.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.results, b.err
}

func (b *delayedStub) callCount() int { return int(atomic.LoadInt32(&b.calls)) }

func relevantResults(n int) []searchResult {
	out := make([]searchResult, n)
	for i := range out {
		out[i] = searchResult{
			Title:   "query 结果 " + string(rune('A'+i)),
			URL:     "https://example.com/" + string(rune('a'+i)),
			Snippet: "关于 query 的摘要",
		}
	}
	return out
}

// TestChainHedgeSlowFirstBackend：首后端 3s、次后端 100ms、对冲窗口 200ms。
// 期望次后端胜出，总耗时远小于 3s。
func TestChainHedgeSlowFirstBackend(t *testing.T) {
	slow := &delayedStub{name: "slow", delay: 3 * time.Second, results: relevantResults(2)}
	fast := &delayedStub{name: "fast", delay: 100 * time.Millisecond, results: relevantResults(2)}
	chain := newBackendChain([]searchBackend{slow, fast}, 10*time.Minute, 0)
	chain.hedgeDelay = 200 * time.Millisecond

	start := time.Now()
	results, via, err := chain.Search(context.Background(), "query 测试", 5)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if via != "fast" {
		t.Errorf("via = %q, want fast（对冲竞速应由快后端胜出）", via)
	}
	if len(results) != 2 {
		t.Errorf("results = %d, want 2", len(results))
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("耗时 %s，期望 < 1.5s（对冲应避免干等首后端 3s）", elapsed.Round(time.Millisecond))
	}
}

// TestChainHedgeFastFirstWins：首后端 50ms 先于对冲窗口返回，次后端
// 根本不应被启动。
func TestChainHedgeFastFirstWins(t *testing.T) {
	fast := &delayedStub{name: "fast", delay: 50 * time.Millisecond, results: relevantResults(2)}
	slow := &delayedStub{name: "slow", delay: 3 * time.Second, results: relevantResults(2)}
	chain := newBackendChain([]searchBackend{fast, slow}, 10*time.Minute, 0)
	chain.hedgeDelay = 500 * time.Millisecond

	results, via, err := chain.Search(context.Background(), "query 测试", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if via != "fast" || len(results) != 2 {
		t.Errorf("via = %q results = %d, want fast/2", via, len(results))
	}
	if slow.callCount() != 0 {
		t.Errorf("慢后端被调用了 %d 次，want 0（首后端先返回时不应对冲）", slow.callCount())
	}
}

// TestChainHedgeFailureFillsImmediately：首后端立刻失败时应立刻补位，
// 不等对冲计时器（失败补位是 failover，不是对冲）。
func TestChainHedgeFailureFillsImmediately(t *testing.T) {
	failing := &delayedStub{name: "failing", err: errors.New("boom")}
	good := &delayedStub{name: "good", delay: 50 * time.Millisecond, results: relevantResults(1)}
	chain := newBackendChain([]searchBackend{failing, good}, 10*time.Minute, 0)
	chain.hedgeDelay = 5 * time.Second // 窗口远大于测试时长：若在等窗口才补位就会超时

	start := time.Now()
	results, via, err := chain.Search(context.Background(), "query 测试", 5)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if via != "good" || len(results) != 1 {
		t.Errorf("via = %q results = %d, want good/1", via, len(results))
	}
	if elapsed > 2*time.Second {
		t.Errorf("耗时 %s，期望立刻补位（< 2s）", elapsed.Round(time.Millisecond))
	}
}

// TestChainHedgeJunkFillsImmediately：首后端快速返回全垃圾时应立刻补位，
// 且不进入冷却。
func TestChainHedgeJunkFillsImmediately(t *testing.T) {
	junk := &delayedStub{name: "junk", results: []searchResult{
		{Title: "澳门赌场娱乐城", URL: "https://spam.com"},
	}}
	good := &delayedStub{name: "good", results: relevantResults(1)}
	chain := newBackendChain([]searchBackend{junk, good}, 10*time.Minute, 0)
	chain.hedgeDelay = 5 * time.Second

	_, via, err := chain.Search(context.Background(), "query 测试", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if via != "good" {
		t.Errorf("via = %q, want good", via)
	}
	// 垃圾后端不进冷却：换个查询仍应尝试它。
	_, _, _ = chain.Search(context.Background(), "query 另一个", 5)
	if junk.callCount() != 2 {
		t.Errorf("垃圾后端调用 %d 次，want 2（不应被冷却）", junk.callCount())
	}
}

// TestChainHedgeAllFail：全部后端失败时返回错误。
func TestChainHedgeAllFail(t *testing.T) {
	a := &delayedStub{name: "a", err: errors.New("boom a")}
	b := &delayedStub{name: "b", err: errors.New("boom b")}
	chain := newBackendChain([]searchBackend{a, b}, 10*time.Minute, 0)
	chain.hedgeDelay = 100 * time.Millisecond

	if _, _, err := chain.Search(context.Background(), "query 测试", 5); err == nil {
		t.Fatal("全部失败时期望返回错误")
	}
}

// TestChainHedgeCacheStillWorks：对冲胜出后结果同样进缓存。
func TestChainHedgeCacheStillWorks(t *testing.T) {
	slow := &delayedStub{name: "slow", delay: 3 * time.Second, results: relevantResults(2)}
	fast := &delayedStub{name: "fast", delay: 50 * time.Millisecond, results: relevantResults(2)}
	chain := newBackendChain([]searchBackend{slow, fast}, 10*time.Minute, 30*time.Minute)
	chain.hedgeDelay = 100 * time.Millisecond

	if _, via, _ := chain.Search(context.Background(), "query 测试", 5); via != "fast" {
		t.Fatalf("via = %q, want fast", via)
	}
	fastCalls := fast.callCount()

	// 同查询第二次：应命中缓存，两个后端都不再被打。
	_, via, err := chain.Search(context.Background(), "query 测试", 5)
	if err != nil || via != "cache" {
		t.Errorf("二次查询 via = %q err = %v, want cache/nil", via, err)
	}
	if fast.callCount() != fastCalls {
		t.Errorf("缓存命中后快后端仍被调用了")
	}
}

// TestChainHedgeDisabled：hedgeDelay <= 0 时走串行 failover（原有行为）。
func TestChainHedgeDisabled(t *testing.T) {
	slow := &delayedStub{name: "slow", delay: 2 * time.Second, results: relevantResults(1)}
	chain := newBackendChain([]searchBackend{slow}, 10*time.Minute, 0)
	// 不设置 hedgeDelay（零值）

	start := time.Now()
	_, via, err := chain.Search(context.Background(), "query 测试", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if via != "slow" {
		t.Errorf("via = %q, want slow", via)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("耗时 %s，串行模式应等待首个后端完整返回（2s）", elapsed.Round(time.Millisecond))
	}
}

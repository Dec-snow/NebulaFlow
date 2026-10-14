package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 构造一个必定失败的首个 provider，验证 fallback 切到第二个。
func TestGateway_ChatFallback(t *testing.T) {
	failing := NewMockProvider("primary")
	failing.FailRate = 1.0 // 100% 失败
	backup := NewMockProvider("backup")

	g := NewGateway([]Provider{failing, backup}, slog.Default(), 5*time.Second, nil)
	resp, err := g.Chat(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	if resp.Provider != "backup" {
		t.Fatalf("expected backup provider, got %s", resp.Provider)
	}
	if !strings.Contains(resp.Content, "backup") {
		t.Fatalf("unexpected content: %s", resp.Content)
	}
}

// 全部失败返回聚合错误。
func TestGateway_AllFail(t *testing.T) {
	f1 := NewMockProvider("p1")
	f1.FailRate = 1.0
	f2 := NewMockProvider("p2")
	f2.FailRate = 1.0
	g := NewGateway([]Provider{f1, f2}, slog.Default(), 5*time.Second, nil)
	_, err := g.Chat(context.Background(), ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
	if !strings.Contains(err.Error(), "all llm providers failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// 无 provider 时报错。
func TestGateway_NoProviders(t *testing.T) {
	g := NewGateway(nil, slog.Default(), 0, nil)
	_, err := g.Chat(context.Background(), ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "no llm provider") {
		t.Fatalf("expected no-provider error, got %v", err)
	}
}

// usage 回调在成功与失败时都会被调用。
func TestGateway_UsageCallback(t *testing.T) {
	var calls []Usage
	ok := NewMockProvider("ok")
	g := NewGateway([]Provider{ok}, slog.Default(), 5*time.Second, func(u *Usage) {
		calls = append(calls, *u)
	})
	if _, err := g.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x y z"}}}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 usage callback, got %d", len(calls))
	}
	if calls[0].Provider != "ok" || calls[0].InputTokens != 3 {
		t.Fatalf("unexpected usage: %+v", calls[0])
	}
}

// Stream 在首个 provider 首块前失败时切换 fallback。
func TestGateway_StreamFallback(t *testing.T) {
	failing := NewMockProvider("primary")
	failing.FailRate = 1.0
	backup := NewMockProvider("backup")
	g := NewGateway([]Provider{failing, backup}, slog.Default(), 5*time.Second, nil)

	ch, err := g.Stream(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "stream me"}}})
	if err != nil {
		t.Fatalf("stream fallback should succeed: %v", err)
	}
	var sb strings.Builder
	for c := range ch {
		if c.Done {
			break
		}
		sb.WriteString(c.Content)
	}
	if !strings.Contains(sb.String(), "backup") {
		t.Fatalf("expected backup content, got: %s", sb.String())
	}
}

// ---------- 熔断全开时的错误信息 ----------

// errUpstreamBoom 是 alwaysFailProvider 的确定性上游错误。
var errUpstreamBoom = errors.New("upstream returned 500")

// alwaysFailProvider 是一个确定性失败的 provider。
// 不用 MockProvider：它的失败依赖 time.Now().UnixNano() 取模，不够确定。
type alwaysFailProvider struct{ name string }

func (p alwaysFailProvider) Name() string { return p.name }

func (p alwaysFailProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return nil, fmt.Errorf("provider %s: %w", p.name, errUpstreamBoom)
}

func (p alwaysFailProvider) Stream(context.Context, ChatRequest) (<-chan Chunk, error) {
	return nil, fmt.Errorf("provider %s: %w", p.name, errUpstreamBoom)
}

// assertNoMalformedError 是"聚合错误必须是真错误"的公共断言。
//
// 场景：所有 provider 都被熔断器跳过时，lastErr 从未被赋值，
// 原实现 `fmt.Errorf("...: %w", lastErr)` 会产出
// `all llm providers failed: %!w(<nil>)` —— 既是畸形字符串，
// 又让 errors.Is / errors.Unwrap 彻底失效（"模型全挂了"时最需要信息）。
func assertNoMalformedError(t *testing.T, err error, skippedProvider string) {
	t.Helper()
	if err == nil {
		t.Fatal("所有 provider 都被熔断跳过时必须返回错误，而不是 nil")
	}
	msg := err.Error()
	t.Logf("聚合错误 = %v", err)
	if strings.Contains(msg, "%!w(") {
		t.Fatalf("错误串畸形（%%w 收到了 nil）: %q", msg)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("聚合错误必须可 Unwrap，实际: %q", msg)
	}
	if !strings.Contains(msg, skippedProvider) && !strings.Contains(msg, "circuit breaker") {
		t.Fatalf("错误里应包含被跳过的 provider 名（%s）或 circuit breaker 字样，实际: %q",
			skippedProvider, msg)
	}
}

// Chat：所有 provider 被熔断跳过时，错误必须是有信息量的真错误。
func TestGateway_ChatAllSkippedByCircuitBreaker(t *testing.T) {
	const name = "breaker-chat"
	g := NewGateway([]Provider{&alwaysFailProvider{name: name}}, slog.Default(), 2*time.Second, nil)
	// 阈值 1：第一次真实失败后立刻熔断（冷却时间给足，避免半开）
	g.SetCircuitBreaker(NewCircuitBreakerRegistryWithConfig(1, time.Hour))

	ctx := context.Background()
	if _, err := g.Chat(ctx, ChatRequest{Model: "m"}); err == nil {
		t.Fatal("第一次调用（provider 真实失败）应返回错误")
	}
	if got := g.breakers.State(name); got != StateOpen {
		t.Fatalf("阈值=1 时一次失败后熔断器应为 open，实际 %s", got)
	}
	// 第二次：provider 被熔断跳过，本次调用没有任何 provider 被真正调用
	_, err := g.Chat(ctx, ChatRequest{Model: "m"})
	assertNoMalformedError(t, err, name)
	if !errors.Is(err, ErrAllProvidersCircuitOpen) {
		t.Fatalf("全部被熔断跳过时应可 errors.Is 到 ErrAllProvidersCircuitOpen，实际: %v", err)
	}
}

// Stream：同上，"全部被跳过"这条分支也必须给出可读错误。
func TestGateway_StreamAllSkippedByCircuitBreaker(t *testing.T) {
	const name = "breaker-stream"
	g := NewGateway([]Provider{&alwaysFailProvider{name: name}}, slog.Default(), 2*time.Second, nil)
	g.SetCircuitBreaker(NewCircuitBreakerRegistryWithConfig(1, time.Hour))

	ctx := context.Background()
	if _, err := g.Stream(ctx, ChatRequest{Model: "m"}); err == nil {
		t.Fatal("第一次调用（provider 真实失败）应返回错误")
	}
	if got := g.breakers.State(name); got != StateOpen {
		t.Fatalf("阈值=1 时一次失败后熔断器应为 open，实际 %s", got)
	}
	_, err := g.Stream(ctx, ChatRequest{Model: "m"})
	assertNoMalformedError(t, err, name)
	if !errors.Is(err, ErrAllProvidersCircuitOpen) {
		t.Fatalf("全部被熔断跳过时应可 errors.Is 到 ErrAllProvidersCircuitOpen，实际: %v", err)
	}
}

// 部分 provider 被熔断跳过、其余真实失败的混合场景：
// 错误里既要有真实失败原因，也要说明有 provider 被跳过。
func TestGateway_MixedSkipAndFailureKeepsRealError(t *testing.T) {
	const (
		brokenName = "always-broken"
		freshName  = "fresh"
	)
	broken := &alwaysFailProvider{name: brokenName}
	fresh := &alwaysFailProvider{name: freshName}
	g := NewGateway([]Provider{broken, fresh}, slog.Default(), 2*time.Second, nil)
	g.SetCircuitBreaker(NewCircuitBreakerRegistryWithConfig(1, time.Hour))

	ctx := context.Background()
	// 第一轮：两个都真实失败 → 两个都熔断
	if _, err := g.Chat(ctx, ChatRequest{Model: "m"}); err == nil {
		t.Fatal("第一轮应返回错误")
	}
	// 第二轮：两个都被跳过 → 仍然必须有可读错误
	_, err := g.Chat(ctx, ChatRequest{Model: "m"})
	assertNoMalformedError(t, err, brokenName)
	// 一个 provider 都没真正调用过：必须能 errors.Is 到熔断哨兵
	if !errors.Is(err, ErrAllProvidersCircuitOpen) {
		t.Fatalf("全部被熔断跳过时应可 errors.Is 到 ErrAllProvidersCircuitOpen，实际: %v", err)
	}
	if !strings.Contains(err.Error(), brokenName) || !strings.Contains(err.Error(), freshName) {
		t.Fatalf("错误里应列出所有被跳过的 provider 名，实际: %v", err)
	}
}

// ---------- 流式专项 ----------


// emptyStreamProvider 返回一个"立刻关闭且毫无内容"的流，
// 模拟 provider 连上了但一个 token 都没吐出来的情况（限流、空响应）。
type emptyStreamProvider struct{ name string }

func (p emptyStreamProvider) Name() string { return p.name }
func (p emptyStreamProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return nil, errors.New("not implemented")
}
func (p emptyStreamProvider) Stream(context.Context, ChatRequest) (<-chan Chunk, error) {
	ch := make(chan Chunk)
	close(ch)
	return ch, nil
}

// 首个 provider 一个 token 都没产出时，必须切换到下一个而不是返回空内容。
// 这条正是"空响应被当成成功"那类线上问题的防线。
func TestGateway_StreamEmptyFallsBack(t *testing.T) {
	backup := NewMockProvider("backup")
	g := NewGateway([]Provider{emptyStreamProvider{"silent"}, backup}, slog.Default(), 5*time.Second, nil)

	ch, err := g.Stream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("should fall back to backup, got error: %v", err)
	}
	var sb strings.Builder
	for c := range ch {
		if c.Done {
			break
		}
		sb.WriteString(c.Content)
	}
	if !strings.Contains(sb.String(), "backup") {
		t.Fatalf("expected backup content, got %q", sb.String())
	}
}

// 所有 provider 都没有产出时，Stream 必须返回错误而不是一个空内容的通道。
func TestGateway_StreamAllEmptyReturnsError(t *testing.T) {
	g := NewGateway([]Provider{emptyStreamProvider{"a"}, emptyStreamProvider{"b"}},
		slog.Default(), 5*time.Second, nil)
	if _, err := g.Stream(context.Background(), ChatRequest{Model: "m"}); err == nil {
		t.Fatal("expected error when no provider produces content")
	}
}

// 取消上下文后通道必须被关闭：调用方的 `for range ch` 不能永久阻塞。
func TestGateway_StreamClosesOnContextCancel(t *testing.T) {
	slow := NewMockProvider("slow")
	slow.Latency = 50 * time.Millisecond
	g := NewGateway([]Provider{slow}, slog.Default(), 5*time.Second, nil)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := g.Stream(ctx, ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // 立刻取消：转发协程必须退出并 close(out)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch { // 若 out 永不关闭这里会挂住
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream channel was not closed after context cancellation")
	}
}

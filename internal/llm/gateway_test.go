package llm

import (
	"context"
	"errors"
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

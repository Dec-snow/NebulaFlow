package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// 这组用例守的是全链路 trace 的**跨进程那一环**：提交侧把 traceparent 写进队列消息。
//
// 为什么单独测它：调用链的父子关系靠 span context 传递，而"提交任务的进程"与
// "执行任务的进程"之间**唯一的**数据通路就是这条消息。少了它，
// worker 只能自起一条新链，同一次请求会变成两条互不相连的 trace
// （API 侧一条、worker 侧一条）——恰好把最该看清的"跨进程那一段"切没了。
//
// 单测能覆盖到的正是"有没有写进消息"；"worker 有没有接上"在 scheduler 包测。

// captureQueue 记录所有入队消息，供断言检查。
type captureQueue struct {
	jobs []queue.Job
}

func (c *captureQueue) Name() string { return "capture" }

func (c *captureQueue) Enqueue(_ context.Context, j queue.Job) error {
	c.jobs = append(c.jobs, j)
	return nil
}

func (c *captureQueue) Dequeue(context.Context, string, time.Duration) (queue.Job, string, error) {
	return queue.Job{}, "", queue.ErrNoMessage
}

func (c *captureQueue) Ack(context.Context, string) error { return nil }

func (c *captureQueue) Nack(context.Context, string, string, int) error { return nil }

func (c *captureQueue) Len(context.Context) (int64, error) { return 0, nil }

func (c *captureQueue) Recover(context.Context, string, int64) (int, error) { return 0, nil }

// withTracing 打开 tracing 并在用例结束后还原全局 provider，
// 避免影响同包其他用例（Setup 会改进程级全局状态）。
func withTracing(t *testing.T) *tracing.Provider {
	t.Helper()
	p, err := tracing.Setup(context.Background(), tracing.Config{Enabled: true})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		if _, err := tracing.Setup(context.Background(), tracing.Config{Enabled: false}); err != nil {
			t.Fatalf("还原 tracing 全局状态失败: %v", err)
		}
	})
	return p
}

func TestCreateTaskPropagatesTraceParent(t *testing.T) {
	withTracing(t)

	q := &captureQueue{}
	svc := NewService(store.NewMemory().Store, q, NewHub())

	ctx, span := tracing.StartInternal(context.Background(), "test.entry")
	wantTrace := tracing.TraceID(ctx)
	span.End()

	// 装置自检：tracing 没真正生效时 wantTrace 会是空串，
	// 而 strings.Contains(tp, "") 恒为真 —— 后面的断言就成了永远通过的假绿。
	if wantTrace == "" {
		t.Fatal("装置自检失败：tracing 未生效，本用例无法证明任何事")
	}

	if _, err := svc.CreateTask(ctx, 1, 1, "hi"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if len(q.jobs) != 1 {
		t.Fatalf("应入队 1 条消息，实际 %d", len(q.jobs))
	}
	tp := q.jobs[0].TraceParent
	if tp == "" {
		t.Fatal("队列消息必须携带 traceparent，否则 worker 侧只能自起一条互不相连的新链")
	}
	// 格式无关的强断言：把消息里的值还原回去，必须得到提交侧的同一个 trace id
	if got := tracing.TraceID(tracing.Extract(context.Background(), tp)); got != wantTrace {
		t.Fatalf("消息里的 traceparent 还原后应是提交侧 trace %s，实际 %s（tp=%q）",
			wantTrace, got, tp)
	}
	// 顺带钉住 W3C 格式（version-traceid-spanid-flags 四段），
	// 免得将来有人"顺手"把它换成自定义编码。
	if parts := strings.Split(tp, "-"); len(parts) != 4 {
		t.Fatalf("traceparent 应是 4 段 W3C 格式，实际 %q", tp)
	}
}

// tracing 未启用时，行为必须与改造前**逐字节一致**：消息体里不能凭空多出一个字段。
// 这条挡的是"新功能静默改变了旧协议"——压测基线与旧客户端的兼容性都依赖它。
func TestCreateTaskOmitsTraceParentWhenTracingDisabled(t *testing.T) {
	if _, err := tracing.Setup(context.Background(), tracing.Config{Enabled: false}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = tracing.Setup(context.Background(), tracing.Config{Enabled: false})
	})

	q := &captureQueue{}
	svc := NewService(store.NewMemory().Store, q, NewHub())
	if _, err := svc.CreateTask(context.Background(), 1, 1, "hi"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Fatalf("应入队 1 条消息，实际 %d", len(q.jobs))
	}
	if tp := q.jobs[0].TraceParent; tp != "" {
		t.Fatalf("tracing 关闭时不应往消息里塞 traceparent，实际 %q", tp)
	}
	if b := q.jobs[0].Bytes(); strings.Contains(string(b), "traceparent") {
		t.Fatalf("tracing 关闭时消息体里不应出现 traceparent 字段，实际 %s", b)
	}
}

package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// 这组基准回答一个具体问题：**trace 在热路径上到底要花多少**。
//
// 它存在的意义是给"默认关闭（OTEL_ENABLED=false）"这个决策一个量化依据，
// 而不是一句"怕影响性能"。P2-11 之后一个 agentic 节点内部可能有
// N 轮对话 + M 次工具调用，也就是说**单个任务**就会产生十几个 span，
// 所以"每个 span 多少钱"必须是个已知数。
//
// 测量范围（如实说明，避免过度外推）：
//   - 覆盖：进程内 span 生命周期、W3C 传播编码/解码、查询侧的聚合计算；
//   - **不覆盖**：OTLP 网络导出（走 Batch，不在请求路径上）、
//     以及端到端任务吞吐影响（那需要压测装置，不是微基准能回答的）。
//
// 运行：
//   go test -run '^$' -bench . -benchmem ./internal/observability/tracing/

func benchProvider(b *testing.B, enabled bool) *Provider {
	b.Helper()
	p, err := Setup(context.Background(), Config{Enabled: enabled, SampleRatio: 1})
	if err != nil {
		b.Fatalf("Setup: %v", err)
	}
	b.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		// 还原全局 provider，避免上一条基准的配置串到下一条
		if _, err := Setup(context.Background(), Config{Enabled: false}); err != nil {
			b.Fatalf("还原 tracing 全局状态失败: %v", err)
		}
	})
	return p
}

// 热路径上最常见的一对操作：起一个 span、结束它。
//
// 关闭态这一条是**基线**：它量的不是"零成本"，而是"noop provider 的成本"——
// 关掉 tracing 并不等于不执行 tracing.StartInternal，调用点一个都没少，
// 只是 Tracer 变成了空实现。这个数字决定了"关掉它到底省了多少"。
func BenchmarkSpanLifecycleDisabled(b *testing.B) {
	benchProvider(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, span := StartInternal(context.Background(), SpanNodeExecute)
		_ = ctx
		Finish(span, nil)
	}
}

// 开启态：span 会真的被采样、被记录、被导出到进程内环形缓冲。
func BenchmarkSpanLifecycleEnabled(b *testing.B) {
	benchProvider(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, span := StartInternal(context.Background(), SpanNodeExecute)
		_ = ctx
		Finish(span, nil)
	}
}

// 带属性与错误记录：这是失败任务、以及 llm.chat / tool.call 这些
// 真正带业务属性的 span 的形态，比空 span 更接近实际。
func BenchmarkSpanLifecycleEnabledWithAttrs(b *testing.B) {
	benchProvider(b, true)
	attrs := []Attr{
		KV("nebulaflow.task.id", int64(12345)),
		KV("nebulaflow.llm.attempt", 2),
		KV("gen_ai.system", "openai"),
	}
	errBoom := context.DeadlineExceeded
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, span := StartInternal(context.Background(), SpanLLMChat, attrs...)
		_ = ctx
		Finish(span, errBoom)
	}
}

// 跨进程传播：每个任务恰好一次 Inject（提交侧）+ 一次 Extract（消费侧）。
func BenchmarkInjectExtract(b *testing.B) {
	benchProvider(b, true)
	ctx, span := StartInternal(context.Background(), SpanTaskCreate)
	defer span.End()

	// 装置自检：Inject 拿不到 traceparent 的话，这条基准量的是两个空函数。
	if Inject(ctx) == "" {
		b.Fatal("装置自检失败：Inject 未产出 traceparent，基准无意义")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tp := Inject(ctx)
		_ = Extract(context.Background(), tp)
	}
}

// 查询侧：一条打满上限的 trace（1 根 + 199 子）在每次查询时都要重算
// 自耗时（区间并集）、瓶颈、树结构、队列等待。这是"点开 trace 面板"
// 那一刻的真实成本——它不在任务热路径上，但会决定面板能不能用。
func BenchmarkSnapshotFullTrace(b *testing.B) {
	p := benchProvider(b, true)
	store := p.Store()

	ctx, root := StartInternal(context.Background(), SpanTaskExecute)
	kids := make([]trace.Span, 0, 199)
	for i := 0; i < 199; i++ {
		_, sp := StartInternal(ctx, SpanNodeExecute)
		kids = append(kids, sp)
	}
	for _, sp := range kids {
		sp.End()
	}
	root.End()

	traceID := TraceID(ctx)
	tr, ok := store.Snapshot(traceID)
	if !ok || tr.SpanCount != 200 {
		b.Fatalf("装置自检失败：应合成 200 条 span 的 trace，实际 ok=%v", ok)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := store.Snapshot(traceID); !ok {
			b.Fatal("快照丢失")
		}
	}
}

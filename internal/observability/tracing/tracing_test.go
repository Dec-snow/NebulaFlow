package tracing

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// 本文件覆盖 P2-12 的三类不变量：
//  1. **跨进程传播**：traceparent 的生成/还原，以及"远端采样决定必须被继承"；
//  2. **时间计算**：自耗时（并行子 span 的区间并集）、瓶颈、队列等待；
//  3. **存储边界**：环形缓冲的淘汰方向、归属记录、孤儿 span 不丢。
//
// 其中 2 全部走 buildTrace / coveredMS 这些纯函数，不依赖真实 SDK 的调度，
// 因此可以构造出"两个子 span 完全重叠"这种关键输入——靠 sleep 是构造不出来的。

// ---------- 测试辅助 ----------

func newTestProvider(t *testing.T, cfg Config) *Provider {
	t.Helper()
	cfg.Enabled = true
	p, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p
}

// rec 构造一个合成 span。时间用毫秒相对值，便于把区间关系写得一目了然。
func rec(id, parent string, startMS, endMS float64) SpanRecord {
	base := time.Unix(1700000000, 0)
	return SpanRecord{
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:     id,
		ParentID:   parent,
		Name:       id,
		StartTime:  base.Add(ms(startMS)),
		EndTime:    base.Add(ms(endMS)),
		DurationMS: endMS - startMS,
	}
}

func ms(v float64) time.Duration {
	return time.Duration(v * float64(time.Millisecond))
}

func spanNames(tr *Trace) []string {
	out := make([]string, 0, len(tr.Spans))
	for _, s := range tr.Spans {
		out = append(out, s.Name)
	}
	return out
}

func hasSpan(tr *Trace, name string) bool {
	for _, s := range tr.Spans {
		if s.Name == name {
			return true
		}
	}
	return false
}

func spanByName(t *testing.T, tr *Trace, name string) SpanRecord {
	t.Helper()
	for _, s := range tr.Spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("span %q 不在 trace 里，实际有 %v", name, spanNames(tr))
	return SpanRecord{}
}

// ---------- 传播 ----------

// W3C Trace Context 规范里的示例值。
const (
	sampledParent   = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	unsampledParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"
)

// 关闭时必须彻底退化成空操作：既不产生 span，也不传播上下文。
// 这一条挡的是"tracing 关了却还在往队列消息里塞 traceparent"这类残留。
func TestSetupDisabledIsNoop(t *testing.T) {
	p, err := Setup(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if p.Enabled() {
		t.Fatal("Enabled() 应为 false")
	}
	if p.Store() != nil {
		t.Fatal("未启用时不应有 span 存储")
	}

	ctx, span := StartInternal(context.Background(), "should-not-exist")
	span.End()
	if got := TraceID(ctx); got != "" {
		t.Fatalf("未启用时不应产生有效 span context，实际 trace id=%q", got)
	}
	if got := Inject(ctx); got != "" {
		t.Fatalf("未启用时 Inject 应返回空串，实际 %q", got)
	}
	if Sampled(ctx) {
		t.Fatal("未启用时不应报告为已采样")
	}
	// Extract 必须原样返回（不能凭一个字符串就凭空造出上下文）
	if Extract(context.Background(), sampledParent) == nil {
		t.Fatal("Extract 不应返回 nil")
	}
}

func TestInjectEmptyWithoutSpan(t *testing.T) {
	newTestProvider(t, Config{})
	if got := Inject(context.Background()); got != "" {
		t.Fatalf("没有 span 时 Inject 应返回空串，实际 %q", got)
	}
}

// **这是 P2-12 最核心的一条**：跨进程续接。
//
// 只断言"traceparent 格式对"是不够的——格式对但消费者没把它当父上下文用，
// 依然会得到两条互不相连的链。所以这里断言的是**父子关系真的接上了**：
// 还原之后起的子 span，其 ParentID 必须等于上游那个 span 的 SpanID，
// 且 TraceID 保持不变。
func TestPropagationRoundTripKeepsParentChildLink(t *testing.T) {
	newTestProvider(t, Config{})

	// --- 模拟 API 侧：起一个 span，编码成 traceparent 塞进队列消息 ---
	apiCtx, apiSpan := StartInternal(context.Background(), "api-side")
	apiSpanID := trace.SpanContextFromContext(apiCtx).SpanID().String()
	traceParent := Inject(apiCtx)
	apiSpan.End()

	if traceParent == "" {
		t.Fatal("在 span 内 Inject 必须返回 traceparent")
	}
	if !strings.HasPrefix(traceParent, "00-") {
		t.Fatalf("traceparent 应形如 00-<32hex>-<16hex>-<2hex>，实际 %q", traceParent)
	}

	// --- 模拟 worker 侧：从队列消息里还原 ---
	workerCtx := Extract(context.Background(), traceParent)
	workerCtx, workerSpan := StartConsumer(workerCtx, "worker-side")
	workerSC := trace.SpanContextFromContext(workerCtx)
	workerSpan.End()

	if workerSC.TraceID().String() != trace.SpanContextFromContext(apiCtx).TraceID().String() {
		t.Fatalf("worker 侧必须沿用上游 trace id：api=%s worker=%s",
			trace.SpanContextFromContext(apiCtx).TraceID(), workerSC.TraceID())
	}
	if got := workerSC.SpanID().String(); got == apiSpanID {
		t.Fatal("worker 侧必须生成**新的** span id，复用上游的会让父子关系退化成一个环")
	}
}

// 采样决策必须在链路入口做一次，之后所有进程继承。
//
// 这条测试的判据是刻意的：把采样率设成 1e-9（几乎不可能采样），
// 于是"本地起链不被采样"与"远端传来的已采样决定被继承"同时成立时，
// 才能排除"采样率根本没生效、其实一直在全采样"这个假绿。
func TestParentBasedSamplingInheritsRemoteDecision(t *testing.T) {
	newTestProvider(t, Config{SampleRatio: 1e-9})

	// 装置自检：本地起链几乎不可能被采样。若这里失败，说明采样率没生效，
	// 下面的断言即使通过也证明不了任何事。
	_, local := StartInternal(context.Background(), "local")
	if local.SpanContext().IsSampled() {
		t.Fatal("采样率 1e-9 下本地起链却被采样了：采样器没生效，本用例无效")
	}
	local.End()

	// 远端说"采样" → 必须继承，否则整条链会缺掉 worker 那一段
	remoteCtx := Extract(context.Background(), sampledParent)
	remoteCtx, sampled := StartInternal(remoteCtx, "child-of-sampled")
	if !sampled.SpanContext().IsSampled() {
		t.Fatal("上游 traceparent 标记为已采样，下游必须继承这个决定")
	}
	if got := sampled.SpanContext().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("应沿用上游 trace id，实际 %s", got)
	}
	sampled.End()

	// 远端说"不采样" → 同样必须继承。只传"采样"不传"不采样"的话，
	// 下游会自己再决定一次，同一条链就会被采成半截。
	unsampledCtx := Extract(context.Background(), unsampledParent)
	unsampledCtx, dropped := StartInternal(unsampledCtx, "child-of-unsampled")
	if dropped.SpanContext().IsSampled() {
		t.Fatal("上游标记为不采样，下游不应自作主张采样")
	}
	dropped.End()
}

// 未采样的链路**依然要传播** traceparent（flags=00）。
// 不传播的话下游会当作"没有上游"，自己起一条新链，同一次请求被拆成两条 trace。
func TestUnsampledLinkStillPropagatesDecision(t *testing.T) {
	newTestProvider(t, Config{SampleRatio: 1e-9})

	ctx, span := StartInternal(context.Background(), "unsampled")
	tp := Inject(ctx)
	span.End()

	if tp == "" {
		t.Fatal("未采样也必须传播 traceparent，否则下游会另起一条链")
	}
	if !strings.HasSuffix(tp, "-00") {
		t.Fatalf("未采样的 traceparent 应以 -00 结尾，实际 %q", tp)
	}
}

func TestExtractIgnoresGarbage(t *testing.T) {
	newTestProvider(t, Config{})
	for _, raw := range []string{"", "   ", "not-a-traceparent", "00-xyz-abc-01", "00-"} {
		ctx := Extract(context.Background(), raw)
		if got := TraceID(ctx); got != "" {
			t.Errorf("Extract(%q) 不应产生有效上下文，实际 trace id=%q", raw, got)
		}
	}
}

func TestNormalizeEndpointStripsScheme(t *testing.T) {
	// otlptracehttp.WithEndpoint 不接受 scheme，而官方的
	// OTEL_EXPORTER_OTLP_ENDPOINT 是带 scheme 的——两者混用不会立刻报错，
	// 只会在后台批量导出时静默失败（表现为"trace 一条都看不到"）。
	cases := map[string]string{
		"localhost:4318":            "localhost:4318",
		"http://localhost:4318":     "localhost:4318",
		"https://collector:4318/":   "collector:4318",
		"  http://127.0.0.1:4318  ": "127.0.0.1:4318",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------- 时间计算 ----------

// coveredMS 必须是区间**并集**，不能是简单相加。
//
// 这是整条 trace 里最容易算错的一处：DAG 的同层节点并行执行，
// 两个各 1 秒的子 span 完全可能同时开始同时结束。相加会得到 2 秒，
// 于是父 span 的"自耗时"变成负数——一个"某个节点比它的总耗时还长"的荒谬结论。
func TestCoveredMSClipsAndMergesIntervals(t *testing.T) {
	parent := rec("p", "", 0, 100)
	cases := []struct {
		name string
		kids []SpanRecord
		want float64
	}{
		{"没有子 span", nil, 0},
		{"单个子 span", []SpanRecord{rec("a", "p", 10, 60)}, 50},
		{
			// 嵌套：b 完全落在 a 里，只能算一次
			name: "完全重叠只算一次",
			kids: []SpanRecord{rec("a", "p", 10, 60), rec("b", "p", 20, 50)},
			want: 50,
		},
		{
			// 简单相加会得到 60+60=120 > 100，父的自耗时变成 -20
			name: "部分重叠且合计超出父区间",
			kids: []SpanRecord{rec("a", "p", 0, 60), rec("b", "p", 40, 100)},
			want: 100,
		},
		{"不重叠相加", []SpanRecord{rec("a", "p", 0, 30), rec("b", "p", 40, 70)}, 60},
		{"首尾相接不重复计算", []SpanRecord{rec("a", "p", 0, 30), rec("b", "p", 30, 60)}, 60},
		{"超出父区间被裁剪", []SpanRecord{rec("a", "p", -20, 150)}, 100},
		{"完全落在父区间之外", []SpanRecord{rec("a", "p", 200, 300)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coveredMS(parent, tc.kids); got != tc.want {
				t.Fatalf("coveredMS = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestSelfTimeIsNeverNegative(t *testing.T) {
	// 两个子 span 合起来覆盖了整个父区间
	spans := []SpanRecord{
		rec("p", "", 0, 100),
		rec("a", "p", 0, 60),
		rec("b", "p", 40, 100),
	}
	computeSelfTimes(spans)
	for _, s := range spans {
		if s.SelfMS < 0 {
			t.Fatalf("span %s 的自耗时不应为负，实际 %v", s.SpanID, s.SelfMS)
		}
	}
	if got := spans[0].SelfMS; got != 0 {
		t.Fatalf("父 span 区间被子 span 完全覆盖时自耗时应为 0，实际 %v", got)
	}
}

// 瓶颈必须是"自身耗时最长"的那个 span，而不是最外层那个。
// 只看 DurationMS 的话，永远只能得出"最外面那个最慢"这个废话。
func TestBottleneckIsTheSpanWithLargestSelfTime(t *testing.T) {
	spans := []SpanRecord{
		rec("HTTP POST /api/tasks", "", 0, 100),
		rec("task.create", "HTTP POST /api/tasks", 1, 5),
		rec("task.execute", "HTTP POST /api/tasks", 6, 100),
		// 慢在工具上：它自己就占了 80ms
		rec("tool.call", "task.execute", 10, 90),
		rec("llm.chat", "task.execute", 90, 95),
	}
	tr := buildTrace("t", spans, 0, false)

	if tr.Bottleneck == nil {
		t.Fatal("应算出瓶颈 span")
	}
	if tr.Bottleneck.Name != "tool.call" {
		t.Fatalf("瓶颈应是自身耗时最长的 tool.call，实际 %s（自耗时 %v）",
			tr.Bottleneck.Name, tr.Bottleneck.SelfMS)
	}
	if got := tr.Bottleneck.SelfMS; got != 80 {
		t.Fatalf("tool.call 自耗时应为 80ms，实际 %v", got)
	}
	// 根 span 的时长是最大的（100ms），但它**不该**被当成瓶颈
	root := spanByName(t, tr, "HTTP POST /api/tasks")
	if tr.Bottleneck.SelfMS >= root.DurationMS {
		t.Fatalf("瓶颈自耗时（%v）应显著小于根 span 总时长（%v）", tr.Bottleneck.SelfMS, root.DurationMS)
	}
}

// 队列等待不属于任何 span —— 任务躺在队列里时没有任何代码在跑。
// 不算出来的话，就会出现"所有 span 加起来 200ms，而用户等了 8 秒"这种看不出问题的 trace。
func TestQueueWaitIsComputedBetweenEnqueueAndExecute(t *testing.T) {
	enq := rec("queue.enqueue", "", 0, 10)
	exec := rec("task.execute", "queue.enqueue", 160, 200)
	enq.Name = SpanQueueEnqueue
	exec.Name = SpanTaskExecute

	tr := buildTrace("t", []SpanRecord{enq, exec}, 0, false)
	if got := tr.QueueWaitMS; got != 150 {
		t.Fatalf("队列等待应为 150ms（160-10），实际 %v", got)
	}
}

func TestQueueWaitZeroWhenExecutePrecedesEnqueue(t *testing.T) {
	// 时间倒挂（时钟回拨 / 数据异常）时不能算出负数
	enq := rec("e", "", 100, 200)
	enq.Name = SpanQueueEnqueue
	exec := rec("x", "", 10, 50)
	exec.Name = SpanTaskExecute
	if got := buildTrace("t", []SpanRecord{enq, exec}, 0, false).QueueWaitMS; got != 0 {
		t.Fatalf("时间倒挂时队列等待应为 0，实际 %v", got)
	}
}

// 这个竞态是**端到端跑出来的**，不是想出来的：
// 入队是一次写操作，写完 worker 就可能立刻取走并开始执行，
// 而 producer 那边还要再走几行才调用 span.End()。两者并发赛跑，
// 于是 `execStart - enqueueEnd` 可以是负数（实测内存队列下就发生了，
// 被夹成 0，看上去像"从来没有队列等待"）。
//
// 所以锚点必须是"worker 开始之前最后一个结束的**非 worker 侧** span"。
func TestQueueWaitSurvivesEnqueueSpanRacingWithWorker(t *testing.T) {
	httpSpan := rec("http", "", 0, 5)
	httpSpan.Name = "HTTP POST /api/tasks"
	create := rec("create", "http", 1, 3)
	create.Name = SpanTaskCreate
	enq := rec("enq", "create", 3, 3.5)
	enq.Name = SpanQueueEnqueue
	// worker 在入队 span 结束（3.5）**之前**就开始了（3.2）
	exec := rec("exec", "enq", 3.2, 40)
	exec.Name = SpanTaskExecute

	got := buildTrace("t", []SpanRecord{httpSpan, create, enq, exec}, 0, false).QueueWaitMS
	// 锚点应回退到 task.create 的结束（3ms），而不是那个还在赛跑的入队 span
	if got < 0.19 || got > 0.21 {
		t.Fatalf("队列等待应回退到提交侧最后一个已结束的 span（3→3.2 = 0.2ms），实际 %v", got)
	}
}

// worker 侧自己的 span **永远**不能当锚点，即使它的时间戳因为时钟抖动 /
// 乱序导出看起来"结束得比 worker 启动还早"。
//
// 这条排除之所以必要：如果只按"结束时间早于 exec 起点"来挑锚点，
// 一个时间戳错乱的 worker 侧 span 就会被选中，于是队列等待变成
// 一个凭空捏造的正数——比算不出来更糟，因为它看起来像真的。
func TestQueueWaitNeverAnchorsOnWorkerSideSpan(t *testing.T) {
	exec := rec("exec", "", 100, 200)
	exec.Name = SpanTaskExecute
	// 父是 exec 的 span（worker 侧），但结束时间早于 exec 的起点
	stray := rec("stray", "exec", 10, 90)
	stray.Name = SpanNodeExecute

	got := buildTrace("t", []SpanRecord{exec, stray}, 0, false).QueueWaitMS
	if got != 0 {
		t.Fatalf("worker 侧 span 不能当队列等待的锚点，实际算出 %v", got)
	}
}

// 没有任何"提交侧"span 时（例如只有 worker 侧那一段），应返回 0 而不是乱算。
func TestQueueWaitWithoutSubmitSideSpans(t *testing.T) {
	exec := rec("exec", "", 10, 50)
	exec.Name = SpanTaskExecute
	if got := buildTrace("t", []SpanRecord{exec}, 0, false).QueueWaitMS; got != 0 {
		t.Fatalf("没有提交侧 span 时应返回 0，实际 %v", got)
	}
}

// 整条链的时长不能直接用根 span 的时长：根 span 在 API 侧就结束了，
// 任务是在 worker 侧才跑完的，中间隔着整个队列等待。
func TestTraceDurationSpansTheWholeChain(t *testing.T) {
	spans := []SpanRecord{
		rec("http", "", 0, 20),                // API 侧，20ms 就返回了
		rec("task.execute", "http", 500, 900), // worker 侧 500ms 后才开始
	}
	tr := buildTrace("t", spans, 0, false)
	if got := tr.DurationMS; got != 900 {
		t.Fatalf("整条链时长应为 900ms（0→900），实际 %v", got)
	}
	if tr.Spans[0].DurationMS != 20 {
		t.Fatalf("根 span 自身仍是 20ms，实际 %v", tr.Spans[0].DurationMS)
	}
}

func TestErrorCountCountsOnlyErrorSpans(t *testing.T) {
	ok := rec("ok", "", 0, 10)
	ok.Status = "ok"
	bad := rec("bad", "", 0, 10)
	bad.Status = "error"
	unset := rec("unset", "", 0, 10)
	unset.Status = "unset"

	tr := buildTrace("t", []SpanRecord{ok, bad, unset}, 0, false)
	if tr.ErrorCount != 1 {
		t.Fatalf("错误 span 数应为 1，实际 %d", tr.ErrorCount)
	}
}

// ---------- 树结构 ----------

func TestBuildTreeLinksChildrenToParents(t *testing.T) {
	spans := []SpanRecord{
		rec("root", "", 0, 100),
		rec("a", "root", 10, 50),
		rec("b", "root", 50, 80),
		rec("a1", "a", 20, 30),
	}
	tr := buildTrace("t", spans, 0, false)

	if len(tr.Roots) != 1 {
		t.Fatalf("应只有一个根，实际 %d", len(tr.Roots))
	}
	root := tr.Roots[0]
	if root.SpanID != "root" {
		t.Fatalf("根应是 root，实际 %s", root.SpanID)
	}
	if len(root.Children) != 2 {
		t.Fatalf("root 应有 2 个子节点，实际 %d", len(root.Children))
	}
	// 子节点按开始时间排序
	if root.Children[0].SpanID != "a" || root.Children[1].SpanID != "b" {
		t.Fatalf("子节点应按开始时间排序，实际 %s,%s",
			root.Children[0].SpanID, root.Children[1].SpanID)
	}
	if len(root.Children[0].Children) != 1 || root.Children[0].Children[0].SpanID != "a1" {
		t.Fatal("a 下面应挂着 a1")
	}
}

// 父 span 不在快照里（被采样丢弃 / 被容量截断 / 尚未导出）时，
// 子 span 必须提升为根**而不是被丢掉**——丢掉的话，"链路缺了一段"
// 会被显示成"这一段没发生过"，而这两件事的排查方向完全不同。
func TestOrphanSpanBecomesRootInsteadOfBeingDropped(t *testing.T) {
	spans := []SpanRecord{
		rec("orphan", "missing-parent", 0, 50),
	}
	tr := buildTrace("t", spans, 0, false)
	if tr.SpanCount != 1 {
		t.Fatalf("孤儿 span 不能被丢弃，实际 %d 条", tr.SpanCount)
	}
	if len(tr.Roots) != 1 || tr.Roots[0].SpanID != "orphan" {
		t.Fatalf("孤儿 span 应提升为根，实际 %+v", tr.Roots)
	}
}

// ---------- 存储边界 ----------

func TestStoreExportsSpansFromRealSDK(t *testing.T) {
	p := newTestProvider(t, Config{})
	ctx, root := StartInternal(context.Background(), "root")
	_, child := StartInternal(ctx, "child")
	child.End()
	root.End()

	store := p.Store()
	traceID := TraceID(ctx)
	tr, ok := store.Snapshot(traceID)
	if !ok {
		t.Fatal("SimpleSpanProcessor 应在 span 结束后立刻可查")
	}
	if tr.SpanCount != 2 {
		t.Fatalf("应有 2 条 span，实际 %d（%v）", tr.SpanCount, spanNames(tr))
	}
	if !hasSpan(tr, "root") || !hasSpan(tr, "child") {
		t.Fatalf("span 名不对：%v", spanNames(tr))
	}
	if tr.Spans[0].Kind != "internal" {
		t.Fatalf("kind 应为 internal，实际 %q", tr.Spans[0].Kind)
	}
	if tr.Spans[0].TraceID != traceID || tr.Spans[0].SpanID == "" {
		t.Fatalf("快照应带完整的 trace/span id，实际 %+v", tr.Spans[0])
	}
}

// 淘汰方向必须是"丢最老的 trace"，且要有计数可观测 —— 静默丢弃是最糟的。
func TestStoreEvictsOldestTrace(t *testing.T) {
	p := newTestProvider(t, Config{MaxTraces: 2})
	for i := 0; i < 3; i++ {
		ctx, sp := StartInternal(context.Background(), "trace-"+string(rune('A'+i)))
		_ = ctx
		sp.End()
	}
	st := p.Store().Stats()
	if st.TracesKept != 2 {
		t.Fatalf("最多保留 2 条 trace，实际 %d", st.TracesKept)
	}
	if st.TracesEvicted != 1 {
		t.Fatalf("应淘汰 1 条最老的 trace，实际 %d", st.TracesEvicted)
	}
	if st.SpansExported != 3 {
		t.Fatalf("应导出 3 条 span，实际 %d", st.SpansExported)
	}
}

// 超过单条 trace 的 span 上限时，丢的必须是**新到的** span。
//
// 反过来丢最老的话，被删掉的会是 trace 的开头（HTTP 入口、task.create），
// 那样连"这是哪个请求"都看不出来了 —— 而开头恰恰是最有用的锚点。
func TestStoreTruncatesNewestSpansNotOldest(t *testing.T) {
	p := newTestProvider(t, Config{MaxSpansPerTrace: 2})

	ctx, root := StartInternal(context.Background(), "root")
	_, c1 := StartInternal(ctx, "c1")
	c1.End()
	_, c2 := StartInternal(ctx, "c2")
	c2.End()
	_, c3 := StartInternal(ctx, "c3")
	c3.End()
	root.End()

	tr, ok := p.Store().Snapshot(TraceID(ctx))
	if !ok {
		t.Fatal("应能查到 trace")
	}
	if !hasSpan(tr, "c1") || !hasSpan(tr, "c2") {
		t.Fatalf("应保留最先到的 c1/c2，实际 %v", spanNames(tr))
	}
	if hasSpan(tr, "c3") || hasSpan(tr, "root") {
		t.Fatalf("超限时应丢弃**新到**的 span，实际 %v", spanNames(tr))
	}
	if !tr.Truncated {
		t.Fatal("截断必须被标记出来，否则查询方会以为这条链就这么长")
	}
	st := p.Store().Stats()
	if st.SpansRejected != 2 {
		t.Fatalf("应记录 2 条被拒的 span，实际 %d", st.SpansRejected)
	}
}

func TestStoreLinkAndOwnership(t *testing.T) {
	p := newTestProvider(t, Config{})
	ctx, sp := StartInternal(context.Background(), "root")
	sp.End()
	id := TraceID(ctx)

	store := p.Store()
	if _, known := store.Owner(id); !known {
		t.Fatal("span 导出后 trace 记录应已存在")
	}
	if owner, _ := store.Owner(id); owner != 0 {
		t.Fatalf("未关联时 owner 应为 0，实际 %d", owner)
	}

	store.Link(id, 42, 7)
	if owner, _ := store.Owner(id); owner != 7 {
		t.Fatalf("owner 应为 7，实际 %d", owner)
	}
	if got, ok := store.TraceForTask(42); !ok || got != id {
		t.Fatalf("task 42 应反查到 %s，实际 %s（ok=%v）", id, got, ok)
	}
	if _, ok := store.TraceForTask(43); ok {
		t.Fatal("不存在的 task 不应反查到 trace")
	}

	// Link(0, 0) 不应把已有信息清空
	store.Link(id, 0, 0)
	if owner, _ := store.Owner(id); owner != 7 {
		t.Fatalf("Link 传 0 不应清空已有归属，实际 %d", owner)
	}
	if got, _ := store.TraceForTask(42); got != id {
		t.Fatalf("Link 传 0 不应清空已有 task 关联，实际 %s", got)
	}
}

func TestStoreRecentFiltersByOwner(t *testing.T) {
	p := newTestProvider(t, Config{})
	store := p.Store()
	for i := 0; i < 3; i++ {
		ctx, sp := StartInternal(context.Background(), "t")
		sp.End()
		if i == 0 {
			store.Link(TraceID(ctx), int64(100+i), int64(1))
		} else {
			store.Link(TraceID(ctx), int64(100+i), int64(2))
		}
	}
	// 只能看到自己的
	mine := store.Recent(10, 2)
	if len(mine) != 2 {
		t.Fatalf("用户 2 应看到 2 条 trace，实际 %d", len(mine))
	}
	for _, s := range mine {
		if s.TaskID == 100 {
			t.Fatal("不应看到其他用户的 trace")
		}
	}
	if all := store.Recent(10, 0); len(all) != 3 {
		t.Fatalf("ownerID=0 表示不过滤，应看到 3 条，实际 %d", len(all))
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	// 未启用 tracing 时 Store() 返回 nil，调用方不应因此 panic。
	// 这是热路径上的常见情形（每个任务都会调一次 Link）。
	var s *SpanStore
	s.Link("x", 1, 1)
	if _, ok := s.Owner("x"); ok {
		t.Fatal("nil store 不应报告任何归属")
	}
	if _, ok := s.Snapshot("x"); ok {
		t.Fatal("nil store 不应查到任何 trace")
	}
	if s.Recent(5, 0) != nil {
		t.Fatal("nil store 不应返回结果")
	}
	if s.Stats() != (StoreStats{}) {
		t.Fatal("nil store 的统计应为零值")
	}
	if err := s.ExportSpans(context.Background(), nil); err != nil {
		t.Fatalf("nil store 导出应为 no-op，实际 %v", err)
	}
}

// ---------- HTTP 状态码与错误 ----------

// 只有 5xx 算错误：把 4xx 也算进去，一个扫描器打几百个 401 就能把错误率刷红。
func TestMarkHTTPStatusOnly5xxIsError(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, "unset"},
		{201, "unset"},
		{401, "unset"},
		{404, "unset"},
		{429, "unset"},
		{500, "error"},
		{503, "error"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			p := newTestProvider(t, Config{})
			ctx, span := StartServer(context.Background(), "HTTP GET /x")
			MarkHTTPStatus(span, tc.status)
			span.End()
			tr, ok := p.Store().Snapshot(TraceID(ctx))
			if !ok {
				t.Fatal("span 应已导出")
			}
			got := tr.Spans[0]
			if got.Status != tc.want {
				t.Fatalf("HTTP %d 的 span 状态应为 %q，实际 %q", tc.status, tc.want, got.Status)
			}
			if got.Attributes["http.response.status_code"] == nil {
				t.Fatalf("HTTP %d 应记录状态码属性", tc.status)
			}
		})
	}
}

func TestMarkHTTPStatusIgnoresZero(t *testing.T) {
	newTestProvider(t, Config{})
	_, span := StartServer(context.Background(), "x")
	MarkHTTPStatus(span, 0)
	span.End() // 不应 panic
}

// 错误文本会写进 span status，必须按字节截断但不能切碎 UTF-8
// （错误消息里常有中文，切碎了在 JSON 里会变成替换字符）。
func TestTruncateMsgKeepsUTF8Intact(t *testing.T) {
	long := strings.Repeat("错误", 1000) // 6000 字节
	got := truncateMsg(long)
	if len(got) > maxStatusMsg+3 {
		t.Fatalf("截断后长度 %d 超出上限 %d", len(got), maxStatusMsg)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatal("截断应带省略号")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatal("截断把 UTF-8 字符切碎了")
	}
	if short := truncateMsg("短错误"); short != "短错误" {
		t.Fatalf("短消息不应被改动，实际 %q", short)
	}
}

func TestFinishRecordsErrorOnSpan(t *testing.T) {
	p := newTestProvider(t, Config{})
	ctx, span := StartInternal(context.Background(), "boom")
	Finish(span, context.DeadlineExceeded)

	tr, ok := p.Store().Snapshot(TraceID(ctx))
	if !ok {
		t.Fatal("span 应已导出")
	}
	got := tr.Spans[0]
	if got.Status != "error" {
		t.Fatalf("失败 span 的状态应为 error，实际 %q", got.Status)
	}
	if !strings.Contains(got.StatusMsg, "deadline exceeded") {
		t.Fatalf("应保留错误文本，实际 %q", got.StatusMsg)
	}
	if len(got.Events) == 0 {
		t.Fatal("RecordError 应留下一个事件")
	}
}

func TestFinishNilErrorKeepsSpanOK(t *testing.T) {
	p := newTestProvider(t, Config{})
	ctx, span := StartInternal(context.Background(), "fine")
	Finish(span, nil)
	tr, _ := p.Store().Snapshot(TraceID(ctx))
	if tr.Spans[0].Status != "unset" {
		t.Fatalf("成功 span 不应被标记为错误，实际 %q", tr.Spans[0].Status)
	}
}

func TestKVFormatsUnknownTypes(t *testing.T) {
	// 未知类型不能 panic，也不能丢属性——用 fmt.Sprint 兜底比"静默不记"好。
	kv := KV("nebulaflow.test", []int{1, 2})
	if string(kv.Key) != "nebulaflow.test" {
		t.Fatalf("key 不对：%q", kv.Key)
	}
	if kv.Value.AsString() != "[1 2]" {
		t.Fatalf("未知类型应被格式化成字符串，实际 %q", kv.Value.AsString())
	}
}

// 数值属性在 JSON 里必须保持**数值**类型。
//
// 这一条是端到端脚本抓出来的真缺陷：`attribute.Value.Emit()`
// （OTel v1.46.0 的实现）返回的是 `string`，连 int64 都会走 `strconv.FormatInt`。
// 后果是所有数值属性变成 `"nebulaflow.agent.round": "1"`，
// 调用方做数值过滤（`round > 2`）必须先解析字符串。
//
// 它之所以难发现，是因为失败信息把值打印成 `1` —— 肉眼看不出那是字符串 `"1"`。
func TestNumericAttributesStayNumericInJSON(t *testing.T) {
	p := newTestProvider(t, Config{})
	ctx, span := StartInternal(context.Background(), "attrs",
		KV("nebulaflow.int", 7),
		KV("nebulaflow.int64", int64(8)),
		KV("nebulaflow.float", 1.5),
		KV("nebulaflow.bool", true),
		KV("nebulaflow.str", "x"),
		KV("nebulaflow.strs", []string{"a", "b"}),
	)
	span.End()

	tr, ok := p.Store().Snapshot(TraceID(ctx))
	if !ok {
		t.Fatal("span 应已导出")
	}
	attrs := tr.Spans[0].Attributes
	if got := attrs["nebulaflow.int"]; got != int64(7) {
		t.Fatalf("整型属性应保持整型，实际 %#v", got)
	}
	if got := attrs["nebulaflow.int64"]; got != int64(8) {
		t.Fatalf("int64 属性应保持整型，实际 %#v", got)
	}
	if got := attrs["nebulaflow.float"]; got != 1.5 {
		t.Fatalf("浮点属性应保持浮点，实际 %#v", got)
	}
	if got := attrs["nebulaflow.bool"]; got != true {
		t.Fatalf("布尔属性应保持布尔，实际 %#v", got)
	}
	if got := attrs["nebulaflow.str"]; got != "x" {
		t.Fatalf("字符串属性应保持字符串，实际 %#v", got)
	}

	// 再走一遍真正的 JSON 序列化，确认落到报文里也是数字而不是 "7"
	b, err := json.Marshal(attrs)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, want := range []string{`"nebulaflow.int":7`, `"nebulaflow.float":1.5`, `"nebulaflow.bool":true`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("JSON 里应出现 %s，实际 %s", want, b)
		}
	}
	if strings.Contains(string(b), `"nebulaflow.int":"7"`) {
		t.Fatalf("数值属性被序列化成了字符串：%s", b)
	}
}

func TestSpanNameConstantsAreStable(t *testing.T) {
	// 端到端脚本按这些名字断言。改名字而漏改脚本，是这类测试最常见的假绿来源，
	// 所以把名字钉在测试里：改名字会立刻让这条用例失败，提醒你同步脚本。
	want := map[string]string{
		SpanTaskCreate:   "task.create",
		SpanQueueEnqueue: "queue.enqueue",
		SpanTaskExecute:  "task.execute",
		SpanNodeExecute:  "node.execute",
		SpanLLMChat:      "llm.chat",
		SpanRAGRetrieve:  "rag.retrieve",
		SpanToolCall:     "tool.call",
		SpanAgentRound:   "agent.round",
		SpanAgentTool:    "agent.tool",
	}
	for got, exp := range want {
		if got != exp {
			t.Errorf("span 名常量被改动：%q != %q（端到端脚本与文档需同步）", got, exp)
		}
	}
}

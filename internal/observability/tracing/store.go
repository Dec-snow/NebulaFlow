package tracing

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ---------- 对外视图 ----------

// SpanEvent 是 span 上的一个事件。
type SpanEvent struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	OffsetMS   float64        `json:"offset_ms"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// SpanRecord 是 sdktrace.ReadOnlySpan 的可序列化快照。
//
// 为什么不让调用方直接拿 ReadOnlySpan：那个接口的生命周期由 SDK 管理，
// 一旦 span 被回收，你手里就只剩一个悬空引用；而且它无法 JSON 序列化，
// 查询接口就得自己再遍历一遍。快照一次、之后只读，边界最清楚。
type SpanRecord struct {
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_span_id,omitempty"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	StartTime  time.Time      `json:"start_time"`
	EndTime    time.Time      `json:"end_time"`
	DurationMS float64        `json:"duration_ms"`
	Status     string         `json:"status"` // unset / ok / error
	StatusMsg  string         `json:"status_message,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Events     []SpanEvent    `json:"events,omitempty"`
	// SelfMS 是**去掉子 span 占用的时间区间**之后，这个 span 自己的耗时。
	//
	// 为什么需要它：一条 trace 里最外层的 span 一定最慢（它包住所有子 span），
	// 光看 DurationMS 永远只能得出"最外面那个最慢"这个废话。
	// 真正想知道的是"时间花在了哪一层"，那必须减掉子 span 的贡献。
	SelfMS float64 `json:"self_ms"`
}

// SpanNode 是 span 树上的一个节点。
type SpanNode struct {
	SpanRecord
	Children []*SpanNode `json:"children,omitempty"`
}

// Trace 是一条完整调用链的查询结果。
type Trace struct {
	TraceID string `json:"trace_id"`
	TaskID  int64  `json:"task_id,omitempty"`

	// Roots 是树根（通常只有一个：API 侧的那个 HTTP span）。
	//
	// 用复数是因为它**可能不止一个**：如果父 span 被采样丢弃、或者导出没跟上，
	// 子 span 就会成为一个没有父节点的孤点。这种情况必须原样呈现，
	// 而不是悄悄丢掉——丢掉的话，"链路缺了一段"看起来会像"这一段没发生过"。
	Roots []*SpanNode `json:"roots,omitempty"`

	// Spans 是平铺列表（按开始时间排序），便于脚本按名字直接断言。
	Spans      []SpanRecord `json:"spans"`
	SpanCount  int          `json:"span_count"`
	ErrorCount int          `json:"error_count"`
	DurationMS float64      `json:"duration_ms"`

	// QueueWaitMS 是「提交侧完成」到「worker 开始执行」之间的等待。
	//
	// 它**不属于任何 span**——任务躺在队列里的时候没有任何代码在跑。
	// 但这恰恰是"提交瞬间返回、结果很久才出来"最常见的原因。
	// 不算出来的话，看一条 trace 会觉得"每个 span 都很快，那用户到底在等什么"。
	//
	// 不加 omitempty：0 与"字段缺失"是两件事，前者是"几乎没等"，
	// 后者是"算不出来"。让调用方自己去猜这两者的区别是接口设计的失误。
	QueueWaitMS float64 `json:"queue_wait_ms"`

	// Bottleneck 是自身耗时最长的那个 span —— 直接回答"卡在哪一轮、哪个工具"。
	Bottleneck *SpanRecord `json:"bottleneck,omitempty"`

	Truncated bool `json:"truncated,omitempty"`
}

// TraceSummary 是列表视图（不含 span 明细）。
type TraceSummary struct {
	TraceID     string    `json:"trace_id"`
	TaskID      int64     `json:"task_id,omitempty"`
	SpanCount   int       `json:"span_count"`
	ErrorCount  int       `json:"error_count"`
	DurationMS  float64   `json:"duration_ms"`
	StartedAt   time.Time `json:"started_at"`
	Bottleneck  string    `json:"bottleneck,omitempty"`
	BottleneckM float64   `json:"bottleneck_ms,omitempty"`
}

// StoreStats 是 span 存储的容量状况。
type StoreStats struct {
	TracesKept    int   `json:"traces_kept"`
	SpansKept     int   `json:"spans_kept"`
	MaxTraces     int   `json:"max_traces"`
	MaxSpansPerTr int   `json:"max_spans_per_trace"`
	SpansExported int64 `json:"spans_exported"`
	SpansRejected int64 `json:"spans_rejected"`
	TracesEvicted int64 `json:"traces_evicted"`
}

// ---------- 存储实现 ----------

// SpanStore 既是 OTel 的 SpanExporter，也是查询后端。
//
// 为什么默认导出到进程内而不是直接推给 collector：
//   - 本项目本机没有 Jaeger/Tempo，把"能不能验证"绑定在外部服务上，
//     就等于把"真做出来了"降级成"看起来做出来了"；
//   - 内存存储可以**同步**导出（SimpleSpanProcessor），span 一结束就能查到，
//     端到端脚本不需要 sleep 等批量导出，断言是确定性的。
//
// 局限（必须说清楚）：进程重启即丢；多实例部署时每个实例只有自己那部分 span。
// 生产环境应当把 OTLP endpoint 配上，让 collector 负责汇聚——本包两条路都支持。
type SpanStore struct {
	mu       sync.RWMutex
	traces   map[string]*traceRecord
	order    []string // 创建顺序，用于淘汰最老的 trace
	maxTr    int
	maxSpans int

	spansExported atomic.Int64
	spansRejected atomic.Int64
	tracesEvicted atomic.Int64
}

type traceRecord struct {
	spans     []SpanRecord
	taskID    int64
	ownerID   int64
	truncated bool
	created   time.Time
}

func NewSpanStore(maxTraces, maxSpansPerTrace int) *SpanStore {
	if maxTraces <= 0 {
		maxTraces = 256
	}
	if maxSpansPerTrace <= 0 {
		maxSpansPerTrace = 200
	}
	return &SpanStore{
		traces:   make(map[string]*traceRecord),
		maxTr:    maxTraces,
		maxSpans: maxSpansPerTrace,
	}
}

// ExportSpans 实现 sdktrace.SpanExporter。
//
// 它必须是幂等且不返回错误的：导出失败会被 SDK 记为错误并重试，
// 而这里唯一可能"失败"的原因是容量上限——那不是故障，是设计好的淘汰，
// 所以记进 spansRejected 而不是返回 error。
func (s *SpanStore) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if s == nil || len(spans) == 0 {
		return nil
	}
	recs := make([]SpanRecord, 0, len(spans))
	for _, sp := range spans {
		recs = append(recs, snapshotSpan(sp))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range recs {
		if r.TraceID == "" {
			continue
		}
		rec := s.ensureLocked(r.TraceID)
		if len(rec.spans) >= s.maxSpans {
			// 超限时丢**新到的** span，并标记 truncated。
			//
			// 反过来丢最老的话，被删掉的会是 trace 的开头（HTTP 入口、task.create），
			// 那样连"这是哪个请求"都看不出来了——而开头恰恰是最有用的锚点。
			rec.truncated = true
			s.spansRejected.Add(1)
			continue
		}
		rec.spans = append(rec.spans, r)
		s.spansExported.Add(1)
	}
	return nil
}

// Shutdown 实现 sdktrace.SpanExporter（内存存储无需释放资源）。
func (s *SpanStore) Shutdown(context.Context) error { return nil }

func (s *SpanStore) ensureLocked(traceID string) *traceRecord {
	if rec, ok := s.traces[traceID]; ok {
		return rec
	}
	// 淘汰最老的 trace，为新的腾位置
	for len(s.order) >= s.maxTr {
		oldest := s.order[0]
		s.order = s.order[1:]
		if _, ok := s.traces[oldest]; ok {
			delete(s.traces, oldest)
			s.tracesEvicted.Add(1)
		}
	}
	rec := &traceRecord{created: time.Now()}
	s.traces[traceID] = rec
	s.order = append(s.order, traceID)
	return rec
}

// Link 把 trace 与任务/用户关联起来，供 /api/tasks/:id/trace 与归属校验使用。
func (s *SpanStore) Link(traceID string, taskID, ownerID int64) {
	if s == nil || traceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ensureLocked(traceID)
	if taskID != 0 {
		rec.taskID = taskID
	}
	if ownerID != 0 {
		rec.ownerID = ownerID
	}
}

// Owner 返回该 trace 的归属用户，未记录时返回 0。
func (s *SpanStore) Owner(traceID string) (int64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.traces[traceID]
	if !ok {
		return 0, false
	}
	return rec.ownerID, true
}

// TraceForTask 返回某个任务对应的 trace id。
func (s *SpanStore) TraceForTask(taskID int64) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, rec := range s.traces {
		if rec.taskID == taskID {
			return id, true
		}
	}
	return "", false
}

// Snapshot 返回一条 trace 的完整视图。
func (s *SpanStore) Snapshot(traceID string) (*Trace, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	rec, ok := s.traces[traceID]
	if !ok {
		s.mu.RUnlock()
		return nil, false
	}
	spans := make([]SpanRecord, len(rec.spans))
	copy(spans, rec.spans)
	truncated := rec.truncated
	taskID := rec.taskID
	s.mu.RUnlock()

	if len(spans) == 0 {
		return nil, false
	}
	return buildTrace(traceID, spans, taskID, truncated), true
}

// buildTrace 把一组 span 聚合成查询视图。
//
// 抽成纯函数（不碰锁、不碰存储）是为了让"自耗时 / 瓶颈 / 队列等待"这些
// 计算能被**确定性**地单测：走真实 SDK 的话，子 span 的时间区间取决于调度，
// 想构造"两个子 span 完全重叠"这种关键输入就只能靠 sleep 碰运气。
func buildTrace(traceID string, spans []SpanRecord, taskID int64, truncated bool) *Trace {
	sort.Slice(spans, func(i, j int) bool { return spans[i].StartTime.Before(spans[j].StartTime) })
	computeSelfTimes(spans)

	t := &Trace{
		TraceID:   traceID,
		TaskID:    taskID,
		Spans:     spans,
		SpanCount: len(spans),
		Truncated: truncated,
	}
	// 整条链的耗时：最早开始到最晚结束。
	// 注意**不能**直接用根 span 的时长——根 span 在 API 侧就结束了，
	// 而任务是在 worker 侧才跑完的，两者之间隔着整个队列等待。
	first, last := spans[0].StartTime, spans[0].EndTime
	for _, sp := range spans {
		if sp.StartTime.Before(first) {
			first = sp.StartTime
		}
		if sp.EndTime.After(last) {
			last = sp.EndTime
		}
	}
	t.DurationMS = msBetween(first, last)
	t.QueueWaitMS = queueWaitMS(spans)
	for i := range spans {
		if spans[i].Status == "error" {
			t.ErrorCount++
		}
		if t.Bottleneck == nil || spans[i].SelfMS > t.Bottleneck.SelfMS {
			b := spans[i]
			t.Bottleneck = &b
		}
	}
	t.Roots = buildTree(spans)
	return t
}

// Recent 返回最近的 trace 摘要（只含 ownerID 匹配的，ownerID<=0 表示不过滤）。
func (s *SpanStore) Recent(limit int, ownerID int64) []TraceSummary {
	if s == nil {
		return nil
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	s.mu.RLock()
	ids := make([]string, 0, len(s.order))
	owners := make(map[string]int64, len(s.order))
	for _, id := range s.order {
		if rec, ok := s.traces[id]; ok {
			ids = append(ids, id)
			owners[id] = rec.ownerID
		}
	}
	s.mu.RUnlock()

	out := make([]TraceSummary, 0, limit)
	// 从最新往回走
	for i := len(ids) - 1; i >= 0 && len(out) < limit; i-- {
		if ownerID > 0 && owners[ids[i]] != ownerID {
			continue
		}
		t, ok := s.Snapshot(ids[i])
		if !ok {
			continue
		}
		sum := TraceSummary{
			TraceID: t.TraceID, TaskID: t.TaskID,
			SpanCount: t.SpanCount, ErrorCount: t.ErrorCount,
			DurationMS: t.DurationMS, StartedAt: t.Spans[0].StartTime,
		}
		if t.Bottleneck != nil {
			sum.Bottleneck = t.Bottleneck.Name
			sum.BottleneckM = t.Bottleneck.SelfMS
		}
		out = append(out, sum)
	}
	return out
}

// Stats 返回容量状况。
func (s *SpanStore) Stats() StoreStats {
	if s == nil {
		return StoreStats{}
	}
	s.mu.RLock()
	spans := 0
	for _, rec := range s.traces {
		spans += len(rec.spans)
	}
	kept := len(s.traces)
	s.mu.RUnlock()
	return StoreStats{
		TracesKept:    kept,
		SpansKept:     spans,
		MaxTraces:     s.maxTr,
		MaxSpansPerTr: s.maxSpans,
		SpansExported: s.spansExported.Load(),
		SpansRejected: s.spansRejected.Load(),
		TracesEvicted: s.tracesEvicted.Load(),
	}
}

// ---------- 快照与计算 ----------

func snapshotSpan(sp sdktrace.ReadOnlySpan) SpanRecord {
	sc := sp.SpanContext()
	r := SpanRecord{
		Name:      sp.Name(),
		Kind:      sp.SpanKind().String(),
		StartTime: sp.StartTime(),
	}
	if sc.IsValid() {
		r.TraceID = sc.TraceID().String()
		r.SpanID = sc.SpanID().String()
	}
	if p := sp.Parent(); p.IsValid() {
		r.ParentID = p.SpanID().String()
	}
	end := sp.EndTime()
	// 未结束的 span（理论上不会出现在导出里）或时间倒挂时，按 0 处理，
	// 否则 UnixNano() 会给出一个巨大的负数，把整条 trace 的时长算成天文数字。
	if !end.IsZero() && !end.Before(r.StartTime) {
		r.EndTime = end
		r.DurationMS = msBetween(r.StartTime, end)
	} else {
		r.EndTime = r.StartTime
	}

	switch sp.Status().Code {
	case codes.Ok:
		r.Status = "ok"
	case codes.Error:
		r.Status = "error"
	default:
		r.Status = "unset"
	}
	r.StatusMsg = sp.Status().Description

	if attrs := sp.Attributes(); len(attrs) > 0 {
		r.Attributes = make(map[string]any, len(attrs))
		for _, kv := range attrs {
			r.Attributes[string(kv.Key)] = attrValue(kv.Value)
		}
	}
	if evs := sp.Events(); len(evs) > 0 {
		r.Events = make([]SpanEvent, 0, len(evs))
		for _, e := range evs {
			se := SpanEvent{
				Name:     e.Name,
				Time:     e.Time,
				OffsetMS: msBetween(r.StartTime, e.Time),
			}
			if len(e.Attributes) > 0 {
				se.Attributes = make(map[string]any, len(e.Attributes))
				for _, kv := range e.Attributes {
					se.Attributes[string(kv.Key)] = attrValue(kv.Value)
				}
			}
			r.Events = append(r.Events, se)
		}
	}
	return r
}

// attrValue 把属性值转成 JSON 原生类型。
//
// **不能用 `attribute.Value.Emit()`**：它在 OTel v1.46.0 里返回的是 `string`，
// 连 int64 都会走 `strconv.FormatInt`。后果是所有数值属性在查询结果里变成
// `"nebulaflow.agent.round": "1"` 而不是 `1`，调用方想做数值过滤（`round > 2`）
// 必须先解析字符串；切片则变成一段 JSON 文本，嵌套在 JSON 里再转义一层。
//
// 这个缺陷是**跑端到端脚本时才发现**的：脚本里 `round == 1` 断言失败，
// 而失败信息把值打印成 `1`——因为它是字符串 `"1"`，肉眼看不出区别。
func attrValue(v attribute.Value) any {
	switch v.Type() {
	case attribute.BOOL:
		return v.AsBool()
	case attribute.INT64:
		return v.AsInt64()
	case attribute.FLOAT64:
		return v.AsFloat64()
	case attribute.STRING:
		return v.AsString()
	case attribute.BOOLSLICE:
		return v.AsBoolSlice()
	case attribute.INT64SLICE:
		return v.AsInt64Slice()
	case attribute.FLOAT64SLICE:
		return v.AsFloat64Slice()
	case attribute.STRINGSLICE:
		return v.AsStringSlice()
	default:
		// INVALID 等未知类型：Emit 至少能给出一个可读的字符串。
		return v.Emit()
	}
}

func msBetween(a, b time.Time) float64 {
	return float64(b.Sub(a)) / float64(time.Millisecond)
}

// computeSelfTimes 就地填充每个 span 的 SelfMS。
func computeSelfTimes(spans []SpanRecord) {
	kids := make(map[string][]SpanRecord, len(spans))
	for _, s := range spans {
		if s.ParentID != "" {
			kids[s.ParentID] = append(kids[s.ParentID], s)
		}
	}
	for i := range spans {
		spans[i].SelfMS = spans[i].DurationMS - coveredMS(spans[i], kids[spans[i].SpanID])
		if spans[i].SelfMS < 0 {
			spans[i].SelfMS = 0
		}
	}
}

// coveredMS 返回子 span 在父区间内实际占用的时长（区间并集，不是简单求和）。
//
// 为什么不能直接相加：DAG 的同层节点是**并行**执行的。两个各 1 秒的子 span
// 完全可能同时开始、同时结束，占用的是同一段 1 秒。相加会算成 2 秒，
// 于是父 span 的"自耗时"变成负数——一个荒谬的结论（"某个节点比它的总耗时还长"）。
// 正确做法是把子区间裁剪到父区间内，排序后合并重叠部分再求和。
func coveredMS(parent SpanRecord, kids []SpanRecord) float64 {
	if len(kids) == 0 {
		return 0
	}
	type interval struct{ s, e float64 }
	// 所有时间都**相对父 span 的起点**来算。
	//
	// 这里踩过一个坑：第一版用 `time.Time{}`（公元 1 年）当基准，
	// 而 Go 的 Duration 是 int64 纳秒——"公元 1 年到现在"约 6.4e19 ns，
	// 超过 int64 上限 9.2e18，直接溢出成负数，于是所有区间都被判成"在父区间之外"，
	// 自耗时恒等于总时长。以父 span 起点为基准则差值只有毫秒级，永不溢出。
	p1 := parent.DurationMS
	ivs := make([]interval, 0, len(kids))
	for _, k := range kids {
		s := msBetween(parent.StartTime, k.StartTime)
		e := msBetween(parent.StartTime, k.EndTime)
		if s < 0 {
			s = 0
		}
		if e > p1 {
			e = p1
		}
		if e > s {
			ivs = append(ivs, interval{s, e})
		}
	}
	if len(ivs) == 0 {
		return 0
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].s < ivs[j].s })
	total := 0.0
	curS, curE := ivs[0].s, ivs[0].e
	for _, v := range ivs[1:] {
		if v.s <= curE {
			if v.e > curE {
				curE = v.e
			}
			continue
		}
		total += curE - curS
		curS, curE = v.s, v.e
	}
	total += curE - curS
	return total
}

// queueWaitMS 计算「提交侧完成」到「worker 开始执行」之间的空档。
//
// 这段空档不落在任何 span 里：任务在队列里排队时没有代码在跑，
// 但用户感受到的延迟包含它。不单独算出来，就会出现
// "所有 span 加起来 200ms，而用户等了 8 秒"这种看不出问题的 trace。
//
// 锚点为什么不是 `queue.enqueue` 的结束时刻 —— 这是端到端跑出来的一个真实竞态：
// 入队本身是一次写操作，写完之后 worker 就可能立刻取走并开始执行，
// 而 producer 那一边还要再走几行才调用 span.End()。两者是并发赛跑，
// 于是 `execStart - enqueueEnd` **可以是负数**（实测内存队列下就发生了，
// 结果被夹成 0，看上去像"从来没有队列等待"）。
//
// 正确的锚点是：**在 worker 开始之前、最后一个结束的非 worker 侧 span**。
// 它天然排除了上面那个还在赛跑的入队 span，同时又是"提交侧到此为止"的准确刻画。
func queueWaitMS(spans []SpanRecord) float64 {
	var exec *SpanRecord
	for i := range spans {
		if spans[i].Name == SpanTaskExecute {
			exec = &spans[i]
			break
		}
	}
	if exec == nil {
		return 0
	}
	workerSide := descendants(spans, exec.SpanID)
	var anchor time.Time
	for i := range spans {
		s := &spans[i]
		if s.SpanID == exec.SpanID || workerSide[s.SpanID] {
			continue // worker 侧自己的 span 不能当锚点
		}
		if !s.EndTime.Before(exec.StartTime) {
			continue // 还没结束，或者结束得比 worker 启动还晚（就是上面那个竞态）
		}
		if s.EndTime.After(anchor) {
			anchor = s.EndTime
		}
	}
	if anchor.IsZero() {
		return 0
	}
	return msBetween(anchor, exec.StartTime)
}

// descendants 返回 root 的全部后代 span id。
func descendants(spans []SpanRecord, root string) map[string]bool {
	kids := make(map[string][]string, len(spans))
	for _, s := range spans {
		if s.ParentID != "" {
			kids[s.ParentID] = append(kids[s.ParentID], s.SpanID)
		}
	}
	out := make(map[string]bool)
	stack := append([]string(nil), kids[root]...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		out[cur] = true
		stack = append(stack, kids[cur]...)
	}
	return out
}

// buildTree 把平铺的 span 组装成树，返回所有根节点。
func buildTree(spans []SpanRecord) []*SpanNode {
	nodes := make(map[string]*SpanNode, len(spans))
	order := make([]*SpanNode, 0, len(spans))
	for i := range spans {
		n := &SpanNode{SpanRecord: spans[i]}
		nodes[spans[i].SpanID] = n
		order = append(order, n)
	}
	var roots []*SpanNode
	for _, n := range order {
		pid := n.ParentID
		if pid == "" {
			roots = append(roots, n)
			continue
		}
		p, ok := nodes[pid]
		if !ok {
			// 父 span 不在本次快照里（被采样丢弃 / 尚未导出 / 上限截断）。
			// 提升为根而不是丢弃：否则"链路缺了一段"会被显示成"这一段不存在"。
			roots = append(roots, n)
			continue
		}
		p.Children = append(p.Children, n)
	}
	return roots
}

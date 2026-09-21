// Package tracing 提供跨 API → Task → Node → LLM/Tool 的全链路 Trace。
//
// # 为什么需要它（P2-12 的动机）
//
// Prometheus 指标是**聚合**的。它能告诉你"Agent 节点平均 2.3 轮、平均耗时 4.1 秒"，
// 但当某一个任务慢了 30 秒时，指标说不出它卡在第几轮、卡在哪个工具上——
// 所有任务都混在同一个直方图里。要回答"这一个任务为什么慢"，必须有**单次调用**的因果链。
//
// # 三层设计
//
//  1. 进程内：OTel SDK 的 TracerProvider + 各层 span（HTTP / task / node / llm / rag / tool）；
//  2. 跨进程：W3C TraceContext（`traceparent`）**随队列消息一起传递**。
//     API 侧把当前 span 的 traceparent 注入到 queue.Job 里，worker 侧取出后
//     作为父上下文继续 —— 因此 worker 上的 span 与 API 上的 span 属于**同一条链**，
//     而不是两条各自独立的链。
//  3. 导出：默认写进进程内的环形缓冲（离线可查、可断言）；
//     配了 OTLP endpoint 时**同时**批量推给 collector（Jaeger / Tempo / otel-collector）。
//
// # 两个刻意的取舍
//
//   - **默认关闭**。开启后每个节点都会多出若干 span，会改变 P0-0 / P0-4 压测基线的含义。
//     与 P2-11 的 `extra.agentic` 同理：新功能不能静默改变历史数字。
//   - **采样用 ParentBased**。采样决策只在链路入口做一次，之后所有进程都继承这个决定。
//     如果每个进程各自按比例采样，同一条链会断成好几截：一部分进程记了、一部分没记，
//     看起来像"这段代码没执行"。更糟的是链路会变成一堆互不相连的孤儿 span。
package tracing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const instrumentationName = "github.com/hoarfrost/nebulaflow"

// Span 名称常量。集中定义是为了让"查询某类 span"的代码（含端到端脚本）
// 不依赖散落各处的字符串字面量——改一处名字而漏改断言，是这类测试最常见的假绿来源。
const (
	// SpanTaskCreate 覆盖"准入预检 → 任务行落库"。
	SpanTaskCreate = "task.create"
	// SpanQueueEnqueue 是入队那一次 Redis 往返。
	SpanQueueEnqueue = "queue.enqueue"
	// SpanTaskExecute 是 worker 侧的任务执行（从出队到写终态）。
	SpanTaskExecute = "task.execute"
	// SpanNodeExecute 是单个 DAG 节点的执行。
	SpanNodeExecute = "node.execute"
	// SpanLLMChat 是一次模型调用。
	SpanLLMChat = "llm.chat"
	// SpanRAGRetrieve 是一次向量检索。
	SpanRAGRetrieve = "rag.retrieve"
	// SpanToolCall 是一次工具调用（工具节点与 Agent 自主调用共用）。
	SpanToolCall = "tool.call"
	// SpanAgentRound 是 Agent 模式的一轮对话。
	SpanAgentRound = "agent.round"
	// SpanAgentTool 是 Agent 内部的一次工具调用（与工具节点区分开）。
	SpanAgentTool = "agent.tool"
)

// traceparentHeader 是 W3C Trace Context 规定的头名（全小写）。
const traceparentHeader = "traceparent"

// maxStatusMsg 是写进 span status 的错误文本上限。
//
// trace 里会带上原始错误文本，其中可能包含 SQL 片段、表名、内网地址等内部细节。
// 截断只是防止单条 span 被一个巨大的错误串撑爆；**真正的防线是查询接口的归属校验**
// （见 SpanStore.Link / Owner）——这也是本包强制要求把 trace 关联到用户的原因之一。
const maxStatusMsg = 512

// Config 是 tracing 的启动配置。
type Config struct {
	// Enabled 关闭时装配 noop provider：所有 Start/Inject 退化为空操作。
	Enabled bool
	// ServiceName 写进 resource 的 service.name。
	ServiceName string
	// InstanceID 写进 service.instance.id，多实例时用来区分是哪个进程。
	InstanceID string
	// SampleRatio 是链路入口的采样比例，取值 (0,1]。<=0 视为 1。
	//
	// 只在**链路入口**生效（ParentBased）：后续进程一律继承入口的决定。
	SampleRatio float64
	// OTLPEndpoint 形如 "localhost:4318" 或 "http://localhost:4318"（两者都接受）。
	// 为空则只用进程内存储，不外发。
	OTLPEndpoint string
	// MaxTraces / MaxSpansPerTrace 是进程内存储的容量上界。
	MaxTraces        int
	MaxSpansPerTrace int
}

func (c Config) withDefaults() Config {
	if c.ServiceName == "" {
		c.ServiceName = "nebulaflow"
	}
	if c.InstanceID == "" {
		host, _ := os.Hostname()
		c.InstanceID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if c.SampleRatio <= 0 || c.SampleRatio > 1 {
		c.SampleRatio = 1
	}
	if c.MaxTraces <= 0 {
		c.MaxTraces = 256
	}
	if c.MaxSpansPerTrace <= 0 {
		c.MaxSpansPerTrace = 200
	}
	return c
}

// Provider 持有 tracer provider 与进程内 span 存储。
type Provider struct {
	cfg   Config
	store *SpanStore
	tp    *sdktrace.TracerProvider
	otlp  sdktrace.SpanExporter
}

// Setup 装配全局 tracer provider 与 W3C propagator，返回可查询的句柄。
//
// 它会修改 otel 的全局状态。这是 OTel 的惯例做法，好处是热路径上的
// Start/Inject 不需要把 provider 一层层传下去（scheduler、task、api 三个包
// 都得为此多一个字段，而它们当中绝大多数调用点根本不关心是谁在收集）。
func Setup(ctx context.Context, cfg Config) (*Provider, error) {
	cfg = cfg.withDefaults()
	if !cfg.Enabled {
		// 显式装 noop：不依赖"全局默认恰好是 noop"这个隐含前提，
		// 否则同一进程里先 Setup(enabled) 再 Setup(disabled) 会残留上一个 provider。
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
		return &Provider{cfg: cfg}, nil
	}

	store := NewSpanStore(cfg.MaxTraces, cfg.MaxSpansPerTrace)

	attrs := []attribute.KeyValue{attribute.String("service.name", cfg.ServiceName)}
	if cfg.InstanceID != "" {
		attrs = append(attrs, attribute.String("service.instance.id", cfg.InstanceID))
	}
	// schemaURL 传空：本项目只用自定义/字符串形式的语义约定键，
	// 声明一个 schema 版本反而会在升级 OTel 时引入无意义的合并冲突。
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes("", attrs...))
	if err != nil {
		return nil, fmt.Errorf("merge otel resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		// ParentBased 是这里唯一正确的选择，理由见包注释。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		// 进程内存储用 Simple（同步）导出器：span 一结束立刻可查。
		// 端到端断言因此是确定性的，不需要 sleep 等批量导出刷盘。
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(store)),
	}

	p := &Provider{cfg: cfg, store: store}
	if cfg.OTLPEndpoint != "" {
		expOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(normalizeEndpoint(cfg.OTLPEndpoint))}
		// scheme 决定是否启用 TLS：显式写 https:// 才走 TLS。
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(cfg.OTLPEndpoint)), "https://") {
			expOpts = append(expOpts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, expOpts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp exporter: %w", err)
		}
		p.otlp = exp
		// 网络导出必须用 Batch：请求路径上不能出现一次同步的 HTTP 往返。
		// 这与上面"内存用 Simple"并不矛盾——两者对延迟的敏感度完全不同。
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	p.tp = tp
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return p, nil
}

// Enabled 报告 tracing 是否真的在收集。
func (p *Provider) Enabled() bool { return p != nil && p.cfg.Enabled }

// Store 返回进程内 span 存储（未启用时为 nil）。
func (p *Provider) Store() *SpanStore {
	if p == nil {
		return nil
	}
	return p.store
}

// Config 返回生效后的配置（便于启动日志打印实际值）。
func (p *Provider) Config() Config {
	if p == nil {
		return Config{}
	}
	return p.cfg
}

// ForceFlush 把缓冲中的 span 推出去（Batch 处理器才需要；测试与关服前调用）。
func (p *Provider) ForceFlush(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.ForceFlush(ctx)
}

// Shutdown 刷出剩余 span 并关闭导出器。
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// ---------- 传播 ----------

// Inject 把当前 span 的上下文编码成 W3C `traceparent` 字符串。
//
// 返回空串只有一种情况：当前没有有效的 span 上下文（tracing 未启用，或不在任何 span 内）。
//
// 注意**链路未被采样时依然会返回 traceparent**（末尾 flags 是 `-00`），这是刻意的：
// 必须把"不采样"这个决定也传播下去。不传播的话，下游会当作"没有上游"而自己起一条新链，
// 于是同一次请求被拆成两条 trace，还白白采了半条链——正是 ParentBased 要避免的事。
func Inject(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get(traceparentHeader)
}

// Extract 从 `traceparent` 字符串还原出远端上下文。
//
// 刻意只带 traceparent、不带 tracestate：tracestate 是厂商自定义状态，
// 本项目没有下游厂商，多传一个字段只会让队列消息变长而没有任何消费者。
// 真接第三方 collector 时再补。
func Extract(ctx context.Context, traceparent string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(traceparent) == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx,
		propagation.MapCarrier{traceparentHeader: traceparent})
}

// TraceID 返回当前上下文所属的 trace id（无则空串）。
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// Sampled 报告当前上下文是否处于被采样的链路里。
func Sampled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	return trace.SpanContextFromContext(ctx).IsSampled()
}

// ---------- span 辅助 ----------

// StartInternal 起一个内部 span（同一进程内的调用步骤）。
func StartInternal(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return start(ctx, name, trace.SpanKindInternal, attrs)
}

// StartServer 起一个服务端 span（HTTP 入口）。
func StartServer(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return start(ctx, name, trace.SpanKindServer, attrs)
}

// StartConsumer 起一个消费者 span（从队列取出消息后开始处理）。
func StartConsumer(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return start(ctx, name, trace.SpanKindConsumer, attrs)
}

func start(ctx context.Context, name string, kind trace.SpanKind, attrs []attribute.KeyValue) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	return otel.Tracer(instrumentationName).Start(ctx, name,
		trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
}

// Finish 结束 span，并在 err != nil 时把它记进 span。
//
// 用 defer tracing.Finish(span, &err) 的形式时注意：必须传指针，
// 否则 defer 会在调用时刻就把当时的 err 值（通常是 nil）求值带走，
// 失败永远记不进去——这是 Go 里最经典的 defer 陷阱之一。
func Finish(span trace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, truncateMsg(err.Error()))
	}
	span.End()
}

// Fail 把错误记到当前 span 上（不结束它，用于循环里"某一轮失败但整体继续"）。
func Fail(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, truncateMsg(err.Error()))
}

// MarkHTTPStatus 按 HTTP 状态码给 span 定成败。
//
// **只有 5xx 算错误**。4xx 是调用方的问题（未认证、参数错、被限流），
// 把它也算成错误会让错误率彻底失去意义——一个扫描器随便打几百个 401
// 就能把面板刷红，真正的服务端故障反而被淹没在噪声里。
func MarkHTTPStatus(span trace.Span, status int) {
	if span == nil || status == 0 {
		return
	}
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	if status >= 500 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", status))
	}
}

// Attr 是 attribute.KeyValue 的别名，让调用点少一层包名。
type Attr = attribute.KeyValue

// KV 是常用的属性构造简写。
func KV(k string, v any) attribute.KeyValue {
	switch x := v.(type) {
	case string:
		return attribute.String(k, x)
	case int:
		return attribute.Int(k, x)
	case int64:
		return attribute.Int64(k, x)
	case float64:
		return attribute.Float64(k, x)
	case bool:
		return attribute.Bool(k, x)
	default:
		return attribute.String(k, fmt.Sprint(v))
	}
}

func truncateMsg(s string) string {
	if len(s) <= maxStatusMsg {
		return s
	}
	// 按字节截断，但要保证不切碎 UTF-8（错误消息里常有中文）。
	cut := s[:maxStatusMsg]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}

// normalizeEndpoint 把 OTLP endpoint 统一成 "host:port"。
//
// otlptracehttp.WithEndpoint 只接受 host[:port]，**不接受 scheme**；
// 而 OTLP 的官方环境变量 OTEL_EXPORTER_OTLP_ENDPOINT 是带 scheme 的
// （如 http://localhost:4318）。把后者直接喂给前者不会立刻报错——
// 它只会在**后台批量导出**时失败，表现为"trace 一条都看不到"，
// 和配置写错这件事看起来毫无关联。所以这里统一剥掉 scheme。
func normalizeEndpoint(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimSuffix(s, "/")
}

// ErrNotEnabled 是查询未启用的 tracing 时返回的错误。
var ErrNotEnabled = errors.New("tracing is not enabled")

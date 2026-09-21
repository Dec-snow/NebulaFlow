// Package observability 提供基于 Prometheus 的业务可观测性指标。
//
// 指标命名遵循业务语义，覆盖五类：
//   - HTTP 层：请求量 / 延迟 / 错误
//   - 任务层：workflow 任务量 / 时长 / 状态
//   - Worker 层：活跃 worker / 队列长度
//   - 队列层：stream 水位 / 背压拒绝数 / 回收条数
//   - 存储层：PostgreSQL 连接池水位与等待情况
//
// Grafana 可直接基于这些指标出面板（见 deploy/grafana/dashboards）。
package observability

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/hoarfrost/nebulaflow/internal/database"
)

type Metrics struct {
	// RunningTasks 是给 Dashboard 实时读取的原子计数（Prometheus Gauge 只可写不可读）。
	RunningTasks atomic.Int64

	// HTTP
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec
	// 任务
	WorkflowTasksTotal   *prometheus.CounterVec
	WorkflowTaskDuration *prometheus.HistogramVec
	WorkflowTasksRunning prometheus.Gauge
	// Worker
	WorkersActive   prometheus.Gauge
	WorkerQueueSize prometheus.Gauge
	// 队列水位（Redis Stream）。StreamLen 与 MaxLen 一起上报，
	// 告警规则直接算比值：nebulaflow_queue_stream_len / nebulaflow_queue_max_len >= 0.9
	// （告警线刻意高于准入线 0.8：准入是第一道防线，告警的语义是"第一道防线失效了"）
	QueueStreamLen prometheus.Gauge
	QueueMaxLen    prometheus.Gauge
	QueueDLQLen    prometheus.Gauge
	// QueueAdmitLimit 是准入阈值。它持续为 0 而 stream_len 又很高，
	// 说明背压被关掉了（QUEUE_ADMIT_RATIO<0），此时只能靠 MAXLEN 兜底。
	QueueAdmitLimit prometheus.Gauge
	// QueueRejectedTotal 是被背压拒掉的提交数（HTTP 503）。
	// 它 >0 说明系统正在过载但**没有丢任务**；如果它一直是 0
	// 而 stream_len 又贴着 max_len，说明背压没生效、MAXLEN 正在静默裁剪。
	QueueRejectedTotal prometheus.Counter
	// QueueReclaimedTotal 是 TrimConsumed 累计回收的条目数。
	// 它持续增长说明回收路径在正常工作；长期为 0 而 StreamLen 又很高，
	// 说明消费端已经停摆（没有消息被 ACK，安全前缀推进不了）。
	QueueReclaimedTotal prometheus.Counter
	// LLM
	LLMRequestsTotal   *prometheus.CounterVec
	LLMRequestDuration *prometheus.HistogramVec
	LLMTokensTotal     *prometheus.CounterVec

	// ---------- PostgreSQL 连接池 ----------
	//
	// 这一组指标回答的是「连接池到底该配多大」——不看它们就只能靠猜。
	// 判断池不够大的决定性指标是 DBPoolEmptyAcquireTotal 与 DBPoolCanceledAcquireTotal，
	// 而不是 MaxConns 本身：池满了但没人等，说明配置是够的；
	// 池没满却一直有人在等，说明瓶颈不在池上。
	DBPoolTotalConns        prometheus.Gauge
	DBPoolIdleConns         prometheus.Gauge
	DBPoolAcquiredConns     prometheus.Gauge
	DBPoolConstructingConns prometheus.Gauge
	DBPoolMaxConns          prometheus.Gauge
	// DBPoolEmptyAcquireTotal 是「池里没有空闲连接、调用方必须等待」的累计次数。
	// 它持续增长 ⇒ 池太小（或查询太慢占着连接不放）。
	DBPoolEmptyAcquireTotal prometheus.Counter
	// DBPoolCanceledAcquireTotal 是等待连接期间被 context 取消的累计次数。
	// 这就是「worker 都在跑但吞吐上不去」的直接证据：请求没失败，只是没等到连接。
	DBPoolCanceledAcquireTotal prometheus.Counter
	// DBPoolNewConnsTotal 是累计新建连接数。稳态下它应该几乎不增长；
	// 持续增长说明连接在被反复销毁重建（MaxConnLifetime 太短，或连接不稳定）。
	DBPoolNewConnsTotal prometheus.Counter

	// lastXxx 记录上一次上报的累计值，用于把池的累计量转成指标增量。
	// 只有上报 goroutine 会读写，不需要加锁。
	lastEmptyAcquire    int64
	lastCanceledAcquire int64
	lastNewConns        int64

	// ---------- 工作流读取缓存 ----------
	//
	// 命中率是判断「缓存有没有真的接上」的唯一依据：
	// 命中率长期为 0 通常意味着键不匹配、TTL 太短、或写缓存一直失败——
	// 这三种情况都不会报错，只会让缓存变成一次白跑的 Redis 往返（比不加还慢）。
	WorkflowCacheHitsTotal   prometheus.Counter
	WorkflowCacheMissesTotal prometheus.Counter
	WorkflowCacheErrorsTotal prometheus.Counter

	lastCacheHits   int64
	lastCacheMisses int64
	lastCacheErrors int64

	// ---------- Agent 工具调用（P2-11） ----------
	//
	// 这组指标回答的是「Agent 到底在干什么」。只看 LLM 调用次数是不够的：
	// 一次"调了 5 轮工具才给出答案"的请求与"直接回答"在 LLM 指标上几乎一样，
	// 成本却高一个数量级。ToolCallsTotal 的 result 维度尤其关键 ——
	// unknown_tool 是配置/提示词问题（模型要的工具没给它），
	// tool_error 是被调用外部系统的问题，两者的处置方式完全不同。
	AgentToolCallsTotal   *prometheus.CounterVec
	AgentToolCallDuration *prometheus.HistogramVec
	// AgentRounds 统计每个 Agent 节点实际用掉的轮数。
	// 它持续贴着上限（result=max_rounds）说明上限设小了或模型陷入了循环。
	AgentRounds *prometheus.HistogramVec
}

// 工具调用结果（AgentToolCallsTotal 的 result 标签取值）。
const (
	ToolResultSuccess     = "success"
	ToolResultToolError   = "tool_error"   // 工具被调用了但执行失败
	ToolResultUnknownTool = "unknown_tool" // 模型要了一个未注册的工具
)

func NewMetrics(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		HTTPRequestsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nebulaflow_http_requests_total",
			Help: "HTTP 请求总量",
		}, []string{"method", "path", "status"}),
		HTTPRequestDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nebulaflow_http_request_duration_seconds",
			Help:    "HTTP 请求耗时",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "path"}),

		WorkflowTasksTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nebulaflow_workflow_tasks_total",
			Help: "Workflow 任务总数（按最终状态）",
		}, []string{"status"}),
		WorkflowTaskDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nebulaflow_workflow_task_duration_seconds",
			Help:    "Workflow 任务执行时长",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
		}, []string{"workflow_id"}),
		WorkflowTasksRunning: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_workflow_tasks_running",
			Help: "当前执行中的任务数",
		}),

		WorkersActive: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_worker_active",
			Help: "活跃 worker 数",
		}),
		WorkerQueueSize: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_worker_queue_size",
			Help: "等待执行的节点任务数",
		}),

		QueueStreamLen: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_queue_stream_len",
			Help: "任务队列 stream 的物理长度（含已 ACK 但尚未回收的条目）",
		}),
		QueueMaxLen: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_queue_max_len",
			Help: "任务队列配置的容量上界（MAXLEN），与 stream_len 一起用于水位告警",
		}),
		QueueDLQLen: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_queue_dlq_len",
			Help: "死信队列物理长度",
		}),
		QueueAdmitLimit: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_queue_admit_limit",
			Help: "准入阈值（达到即拒绝新提交），0 表示背压已关闭",
		}),
		QueueRejectedTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_queue_rejected_total",
			Help: "被背压拒绝的提交数（HTTP 503）",
		}),
		QueueReclaimedTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_queue_reclaimed_entries_total",
			Help: "已确认前缀累计回收的条目数",
		}),

		DBPoolTotalConns: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_db_pool_total_conns",
			Help: "连接池中的连接总数（空闲 + 使用中 + 正在建立）",
		}),
		DBPoolIdleConns: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_db_pool_idle_conns",
			Help: "连接池中的空闲连接数",
		}),
		DBPoolAcquiredConns: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_db_pool_acquired_conns",
			Help: "正在被使用的连接数",
		}),
		DBPoolConstructingConns: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_db_pool_constructing_conns",
			Help: "正在建立的连接数",
		}),
		DBPoolMaxConns: f.NewGauge(prometheus.GaugeOpts{
			Name: "nebulaflow_db_pool_max_conns",
			Help: "连接池配置的连接数上限",
		}),
		DBPoolEmptyAcquireTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_db_pool_empty_acquire_total",
			Help: "池内无空闲连接而必须等待的累计次数（持续增长 ⇒ 池太小或查询占用过久）",
		}),
		DBPoolCanceledAcquireTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_db_pool_canceled_acquire_total",
			Help: "等待连接期间被 context 取消的累计次数（吞吐上不去的直接证据）",
		}),
		DBPoolNewConnsTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_db_pool_new_conns_total",
			Help: "累计新建连接数（稳态下应几乎不增长）",
		}),

		WorkflowCacheHitsTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_workflow_cache_hits_total",
			Help: "工作流读取缓存命中次数",
		}),
		WorkflowCacheMissesTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_workflow_cache_misses_total",
			Help: "工作流读取缓存未命中（回源数据库）次数",
		}),
		WorkflowCacheErrorsTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "nebulaflow_workflow_cache_errors_total",
			Help: "缓存读写或反序列化失败的次数（缓存降级但不影响请求）",
		}),

		LLMRequestsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nebulaflow_llm_requests_total",
			Help: "LLM 调用总量",
		}, []string{"provider", "model", "result"}),
		LLMRequestDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nebulaflow_llm_request_duration_seconds",
			Help:    "LLM 调用耗时",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"provider"}),
		LLMTokensTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nebulaflow_llm_tokens_total",
			Help: "LLM token 用量",
		}, []string{"provider", "direction"}), // direction: in / out

		AgentToolCallsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Name: "nebulaflow_agent_tool_calls_total",
			Help: "Agent 自主发起的工具调用数（按工具与结果）",
		}, []string{"tool", "result"}),
		AgentToolCallDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nebulaflow_agent_tool_call_duration_seconds",
			Help:    "Agent 工具调用耗时",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20},
		}, []string{"tool"}),
		AgentRounds: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "nebulaflow_agent_rounds",
			Help:    "Agent 节点实际用掉的对话轮数（result=max_rounds 表示撞上上限）",
			Buckets: []float64{1, 2, 3, 4, 6, 8, 12, 16},
		}, []string{"result"}),
	}
}

// ---------- WorkerPool MetricsSink 实现 ----------

func (m *Metrics) SetWorkersActive(n int64) { m.WorkersActive.Set(float64(n)) }
func (m *Metrics) SetQueueSize(n int64)     { m.WorkerQueueSize.Set(float64(n)) }

// ---------- 队列水位 ----------

// SetQueueDepth 上报队列物理长度、容量上界与准入阈值。
// 几个值必须成对上报：只报长度的话，告警规则无从判断"多长算长"。
func (m *Metrics) SetQueueDepth(streamLen, maxLen, dlqLen, admitLimit int64) {
	m.QueueStreamLen.Set(float64(streamLen))
	m.QueueMaxLen.Set(float64(maxLen))
	m.QueueDLQLen.Set(float64(dlqLen))
	m.QueueAdmitLimit.Set(float64(admitLimit))
}

// AddQueueRejected 累加被背压拒绝的提交数。
func (m *Metrics) AddQueueRejected() {
	if m == nil {
		return
	}
	m.QueueRejectedTotal.Inc()
}

// ---------- PostgreSQL 连接池 ----------

// SetDBPoolStats 上报连接池快照。
//
// 入参直接复用 database.PoolStats，而不是在可观测性层再定义一个同构结构体：
// 两份字段完全相同的定义迟早会漂移，而字段名相同、类型也相同的结构体之间
// 手工逐字段拷贝是编译器抓不到的一类错误（8 个 int 字段，抄错顺序完全合法）。
// 依赖方向 observability → database 是无环的，且服务端二进制本来就已经链接了 pgx，
// 因此这个耦合的实际代价为零。
func (m *Metrics) SetDBPoolStats(s database.PoolStats) {
	if m == nil {
		return
	}
	m.DBPoolTotalConns.Set(float64(s.TotalConns))
	m.DBPoolIdleConns.Set(float64(s.IdleConns))
	m.DBPoolAcquiredConns.Set(float64(s.AcquiredConns))
	m.DBPoolConstructingConns.Set(float64(s.ConstructingConns))
	m.DBPoolMaxConns.Set(float64(s.MaxConns))

	// 累计量用 Add(delta) 而不是 Set(total)：指标对象自己持有累计值，
	// 直接 Set 会在进程重启后回退（Prometheus 对 Counter 回退会判定为 counter reset，
	// 进而让 rate() 出现尖刺）。这里用「本次增量」累加，语义与 Counter 一致。
	if d := s.EmptyAcquire - m.lastEmptyAcquire; d > 0 {
		m.DBPoolEmptyAcquireTotal.Add(float64(d))
	}
	if d := s.CanceledAcquire - m.lastCanceledAcquire; d > 0 {
		m.DBPoolCanceledAcquireTotal.Add(float64(d))
	}
	if d := s.NewConns - m.lastNewConns; d > 0 {
		m.DBPoolNewConnsTotal.Add(float64(d))
	}
	m.lastEmptyAcquire = s.EmptyAcquire
	m.lastCanceledAcquire = s.CanceledAcquire
	m.lastNewConns = s.NewConns
}

// SetWorkflowCacheStats 上报工作流缓存自进程启动以来的累计命中/未命中/错误数。
//
// 参数是累计值而不是增量：调用方（缓存装饰器）本来就在维护累计计数，
// 让它再去算增量只会多一处可能算错的地方。这里用 last* 记录上一次的值来求差，
// 与连接池那组指标同一套做法。
//
// 刻意不接收 store.CacheStats 结构体：那会让 observability 反向依赖 store，
// 而三个 int64 的参数顺序在调用点一眼可辨（hits, misses, errors），不存在歧义。
func (m *Metrics) SetWorkflowCacheStats(hits, misses, errors int64) {
	if m == nil {
		return
	}
	if d := hits - m.lastCacheHits; d > 0 {
		m.WorkflowCacheHitsTotal.Add(float64(d))
	}
	if d := misses - m.lastCacheMisses; d > 0 {
		m.WorkflowCacheMissesTotal.Add(float64(d))
	}
	if d := errors - m.lastCacheErrors; d > 0 {
		m.WorkflowCacheErrorsTotal.Add(float64(d))
	}
	m.lastCacheHits, m.lastCacheMisses, m.lastCacheErrors = hits, misses, errors
}

// AddQueueReclaimed 累加已确认前缀被回收的条目数。
func (m *Metrics) AddQueueReclaimed(n int64) {
	if n > 0 {
		m.QueueReclaimedTotal.Add(float64(n))
	}
}

// ---------- 便捷记录函数 ----------

func (m *Metrics) ObserveHTTP(method, path string, status int, durSeconds float64) {
	m.HTTPRequestsTotal.WithLabelValues(method, path, httpStatusText(status)).Inc()
	m.HTTPRequestDuration.WithLabelValues(method, path).Observe(durSeconds)
}

func (m *Metrics) ObserveTaskStarted() {
	m.WorkflowTasksRunning.Inc()
	m.RunningTasks.Add(1)
}

func (m *Metrics) ObserveTaskDone(status string, durSeconds float64, workflowID string) {
	m.WorkflowTasksRunning.Dec()
	m.RunningTasks.Add(-1)
	m.WorkflowTasksTotal.WithLabelValues(status).Inc()
	m.WorkflowTaskDuration.WithLabelValues(workflowID).Observe(durSeconds)
}

// ObserveTaskDropped 归还 running 计数但不计入任务总数：
// 任务已被删除、消息被丢弃的场景下用它，避免 gauge 只增不减。
func (m *Metrics) ObserveTaskDropped() {
	m.WorkflowTasksRunning.Dec()
	m.RunningTasks.Add(-1)
}

func (m *Metrics) ObserveLLM(provider, model string, ok bool, durSeconds float64, inTokens, outTokens int) {
	result := "success"
	if !ok {
		result = "error"
	}
	m.LLMRequestsTotal.WithLabelValues(provider, model, result).Inc()
	m.LLMRequestDuration.WithLabelValues(provider).Observe(durSeconds)
	if ok {
		m.LLMTokensTotal.WithLabelValues(provider, "in").Add(float64(inTokens))
		m.LLMTokensTotal.WithLabelValues(provider, "out").Add(float64(outTokens))
	}
}

// ObserveToolCall 记录一次 Agent 自主发起的工具调用。
//
// result 只取 ToolResultSuccess / ToolResultToolError / ToolResultUnknownTool。
// 时长对失败的调用同样记录：工具失败往往正是因为超时，
// 只在成功时记录会让"慢到失败"这一类问题在面板上彻底消失。
func (m *Metrics) ObserveToolCall(tool, result string, durSeconds float64) {
	if m == nil {
		return
	}
	m.AgentToolCallsTotal.WithLabelValues(tool, result).Inc()
	m.AgentToolCallDuration.WithLabelValues(tool).Observe(durSeconds)
}

// ObserveAgentRounds 记录一个 Agent 节点用掉的轮数。
func (m *Metrics) ObserveAgentRounds(result string, rounds int) {
	if m == nil {
		return
	}
	m.AgentRounds.WithLabelValues(result).Observe(float64(rounds))
}

func httpStatusText(code int) string {
	switch {
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

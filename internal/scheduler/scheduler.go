package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/tool"
	"github.com/hoarfrost/nebulaflow/internal/worker"
)

// ErrTaskCancelled 表示任务被用户主动取消（区别于超时与进程退出）。
// 调度器据此把任务状态写成 cancelled 而不是 failed。
var ErrTaskCancelled = errors.New("task cancelled by user")

// Scheduler 是工作流执行引擎：
//   - consumeLoop：从队列取任务（Redis Stream / 内存）
//   - executeTask：解析 DAG 并驱动并发调度
//   - ExecuteNode：实现 worker.Executor，按节点类型分发执行
//
// 调度语义（面试常问的三点）：
//  1. 就绪判定：入度归零即就绪，同层节点全部提交给 Worker Pool 并发执行；
//  2. 失败传播：某节点失败后，其全部下游后代标记为 skipped 并落库/推送事件，
//     而不是继续用空输入执行下游（原实现正是这里会"明明失败了还往下跑"）；
//  3. 取消链路：API → task.Service → Scheduler.Cancel → 任务级 context，
//     正在进行的 LLM 流式调用会被真正打断。
// ---------- 存储依赖抽象 ----------
//
// Scheduler 只依赖窄接口而不是 *store.Store，好处有两个：
//  1. 单元测试可以用内存 fake 跑通整条调度链路，不需要起 PostgreSQL；
//  2. 依赖关系从"整个 store"收敛到"我真正用到的 7 个方法"，重构时更安全。

type TaskStore interface {
	GetTaskByID(ctx context.Context, id int64) (*model.Task, error)
	CountTaskNodes(ctx context.Context, taskID int64) (int, error)
	CreateTaskNodes(ctx context.Context, taskID int64, nodes []model.WorkflowNode) error
	UpdateTaskStatus(ctx context.Context, id int64, status model.TaskStatus, output, errMsg string) error
	UpdateTaskNode(ctx context.Context, n *model.TaskNode) error
	AppendLog(ctx context.Context, l *model.TaskLog) error
	RecordUsage(ctx context.Context, u *model.UsageRecord) error
}

type WorkflowStore interface {
	GetWorkflow(ctx context.Context, id, userID int64) (*model.Workflow, error)
}

type KnowledgeStore interface {
	// SearchChunks 做向量 Top-K 检索（pgvector HNSW / 内存余弦）。
	// 签名与 store.KnowledgeStore 一致：向量化后的查询向量 + k 进去，Top-K 出来。
	SearchChunks(ctx context.Context, kbID int64, queryVec []float64, k int) ([]model.DocumentChunk, error)
}

type Scheduler struct {
	tasks     TaskStore
	workflows WorkflowStore
	knowledge KnowledgeStore
	queue     queue.Queue
	pool      *worker.WorkerPool
	llm       *llm.Gateway
	rag       *rag.Service
	tools     *tool.Registry
	hub       *task.Hub
	logger    *slog.Logger
	metrics   *observability.Metrics
	// tracing 提供进程内 span 存储（用于 task → trace 反查与归属校验）。
	// 为 nil 时一切退化为空操作；span 本身的创建走包级函数，不依赖这个字段。
	tracing *tracing.Provider
	// wfLock 防止同一工作流在多个实例上并发执行。
	// 为 nil 时不加锁（单实例内存模式下的兼容行为）。
	wfLock WorkflowLock

	taskTimeout     time.Duration
	defaultMaxRetry int

	mu        sync.Mutex
	cancels   map[int64]context.CancelFunc
	cancelled map[int64]bool
	running   atomic.Int64

	// consumers 是消费循环并发度（同时推进多少个任务）。
	// <=0 表示跟随 WorkerPool 的 worker 数。见 SetConsumers 的说明。
	consumers atomic.Int64
}

type nodeOutcome struct {
	key string
	res worker.NodeResult
	err error
}

// New 创建调度器。pool 由外部创建并 Start。
func New(st *store.Store, q queue.Queue, pool *worker.WorkerPool, g *llm.Gateway,
	rg *rag.Service, tools *tool.Registry, hub *task.Hub, logger *slog.Logger,
	metrics *observability.Metrics, taskTimeout time.Duration, defaultMaxRetry int) *Scheduler {
	return newScheduler(st.Tasks, st.Workflows, st.Knowledge, q, pool, g, rg, tools, hub,
		logger, metrics, taskTimeout, defaultMaxRetry)
}

// newScheduler 是构造函数本体，测试用它注入内存实现。
func newScheduler(tasks TaskStore, workflows WorkflowStore, knowledge KnowledgeStore,
	q queue.Queue, pool *worker.WorkerPool, g *llm.Gateway,
	rg *rag.Service, tools *tool.Registry, hub *task.Hub, logger *slog.Logger,
	metrics *observability.Metrics, taskTimeout time.Duration, defaultMaxRetry int) *Scheduler {
	if taskTimeout <= 0 {
		taskTimeout = 5 * time.Minute
	}
	if defaultMaxRetry < 0 {
		defaultMaxRetry = 0
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		tasks: tasks, workflows: workflows, knowledge: knowledge,
		queue: q, pool: pool, llm: g, rag: rg, tools: tools,
		hub: hub, logger: logger, metrics: metrics,
		taskTimeout: taskTimeout, defaultMaxRetry: defaultMaxRetry,
		cancels:   map[int64]context.CancelFunc{},
		cancelled: map[int64]bool{},
	}
}

// Cancel 中断运行中的任务：设置取消标记并触发任务级 context。
// 返回 false 表示该任务不在本机执行（可能还未出队或已在其他实例上）。
//
// 注意这里的判断顺序：先查 cancels，**只有确实在本机跑才**写 cancelled 标记。
// 原实现无条件先写标记再查，而 cancelled 的唯一清理点在 executeTask 尾部——
// 一个"还没出队就被取消"的任务根本不会走到那里，标记就永久留在 map 里。
// 而取消排队中的任务是再正常不过的操作，反复调用即可让这个 map 无限增长。
func (s *Scheduler) Cancel(taskID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cancels[taskID]
	if !ok {
		return false
	}
	s.cancelled[taskID] = true
	if c != nil {
		c()
	}
	return true
}

// RunningTasks 返回本机正在执行的任务数（Dashboard 实时指标）。
func (s *Scheduler) RunningTasks() int64 { return s.running.Load() }

// SetConsumers 设置消费循环的并发度（必须在 Run 之前调用）。
//
// 为什么需要这个旋钮——这是压测实测出来的头号瓶颈：
// 原实现只有一个消费 goroutine，一次 Dequeue 取一条，同步跑完整个 DAG 才取第二条。
// 于是任务吞吐恒等于 1/关键路径耗时，实测四个档位（WORKER_COUNT=1/5/10/20）
// 都是精确的 24.0 tasks/s——WorkerCount 完全不起作用，因为 WorkerPool 并行化的是
// "单个任务内部的节点"，而单条链式 DAG 任一时刻只有一个节点就绪。
//
// n <= 0 表示跟随 WorkerPool 的 worker 数（默认）。
func (s *Scheduler) SetConsumers(n int) { s.consumers.Store(int64(n)) }

// SetTracing 注入 tracing（main 装配时调用）。
//
// 它只被用来做两件事：把 task id 与 trace id 关联起来（供 /api/tasks/:id/trace 反查），
// 以及记录 trace 的归属用户（trace 里带原始错误文本，查询接口必须做权限校验）。
// span 的创建不经过它——那走 tracing 包的包级函数，未启用时天然是空操作。
func (s *Scheduler) SetTracing(p *tracing.Provider) { s.tracing = p }

// SetWorkflowLock 注入分布式锁（main 装配时调用）。
//
// 为 nil 时不加锁——内存模式或 Redis 不可用时的兼容行为。
// 生产模式下有 Redis 就应该注入，否则同一工作流可在多实例上同时执行
// （产生脏 task_nodes、重复 LLM 调用、输出错位）。
func (s *Scheduler) SetWorkflowLock(l WorkflowLock) { s.wfLock = l }

// consumerCount 解析实际生效的消费并发度：显式设置优先，否则跟随 WorkerPool，
// 最后兜底为 1（宁可串行也不能是 0，否则 Run 会起 0 个消费者直接"静默停摆"）。
func (s *Scheduler) consumerCount() int {
	if n := int(s.consumers.Load()); n > 0 {
		return n
	}
	if s.pool != nil {
		if n := s.pool.Workers(); n > 0 {
			return n
		}
	}
	return 1
}

// Run 启动消费循环（阻塞，由 main 在 goroutine 中调用）。
//
// 并发模型是两层、互相独立的：
//
//	消费层：N 个 goroutine 共享同一队列，各自以独立 consumer name 取任务。
//	        Redis Stream 消费组天然支持竞争消费——同一条消息只会投递给一个消费者，
//	        所以 N 个消费者是"分工"而不是"重复劳动"。
//	执行层：每个任务内部由 WorkerPool 并发执行就绪节点（DAG 同层节点并行）。
//
// 启动时先做一次崩溃恢复，认领其他实例遗留的 pending 任务。
// 恢复只做一次而不是每个消费者各做一次：Recover 会把认领到的消息重新 XADD 回队列，
// N 个消费者同时恢复会把同一批消息重复投递 N 遍。
func (s *Scheduler) Run(ctx context.Context) {
	base := "scheduler-" + fmt.Sprint(time.Now().UnixNano()%100000)

	n := s.consumerCount()

	// 恢复额度随消费者数放大，避免多实例场景下一次只捞回一小批
	recoverCount := int64(n) * 8
	if recoverCount < 16 {
		recoverCount = 16
	}
	if claimed, err := s.queue.Recover(ctx, base, recoverCount); err == nil && claimed > 0 {
		s.logger.Info("recovered pending tasks", "count", claimed)
	}

	s.logger.Info("scheduler started", "worker_id", base, "consumers", n)

	var wg sync.WaitGroup

	// 队列水位维护（回收已确认前缀 + 上报水位指标）。
	// 只有 Redis Stream 实现具备这个能力，内存队列出队即删除，不需要回收。
	if stats, ok := s.queue.(queue.StreamStats); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.maintainQueue(ctx, stats)
		}()
	}

	wg.Add(n)
	for i := 0; i < n; i++ {
		// 每个消费者必须有独立的 consumer name：Redis Stream 的 PEL 按消费者归属，
		// 共用一个名字会让 XAUTOCLAIM 的 idle 判定互相干扰（A 认领了 B 正在跑的消息）。
		go func(workerID string) {
			defer wg.Done()
			s.consumeLoop(ctx, workerID)
		}(fmt.Sprintf("%s-c%d", base, i))
	}
	wg.Wait()
	s.logger.Info("scheduler stopped", "consumers", n)
}

// consumeLoop 是单个消费者的循环：取一条 → 执行完整个 DAG → 再取下一条。
//
// 这里不做任何任务级互斥：executeTask 的可变状态除 s.mu 保护的
// cancels/cancelled 与原子计数器 running 外，全部是函数局部变量
// （done / inDegree / submitted / results / remaining / firstErr / aborted），
// 天然并发安全，因此多个消费者可以放心并行。
func (s *Scheduler) consumeLoop(ctx context.Context, workerID string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, msgID, err := s.queue.Dequeue(ctx, workerID, 2*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return // 关服：立刻退出，不再空转
			}
			if !errors.Is(err, queue.ErrNoMessage) {
				s.logger.Warn("dequeue failed", "consumer", workerID, "error", err)
				// 退避，但不能睡死——关服时要能被立刻唤醒
				select {
				case <-ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
			continue
		}

		// 跨进程续接：把 API 侧注入队列消息的 traceparent 还原成远端上下文。
		// 取不到（tracing 未启用 / 链路未被采样 / 旧版本写入的消息）就退化成一条新链，
		// 而不是报错或跳过任务——可观测性不该成为任务执行的前置条件。
		taskCtx := tracing.Extract(ctx, job.TraceParent)
		if err := s.executeTask(taskCtx, job, msgID); err != nil {
			s.logger.Error("task execution failed",
				"task_id", job.TaskID, "consumer", workerID, "error", err)
		}
	}
}

// 队列水位维护的周期。10 秒是个折中：太密会白占 Redis 往返，
// 太疏则 MAXLEN 触发裁剪到告警之间会有一大段无人观测的窗口。
//
// 用 var 而非 const：测试要把它压到毫秒级来验证 Run 真的接上了维护循环，
// 否则单测得干等 10 秒。
var queueMaintainInterval = 10 * time.Second

// queueSaturationWarnRatio 是水位告警阈值（占 MaxLen 的比例）。
//
// 为什么高于准入阈值（0.8）：准入是第一道防线，告警的语义是"第一道防线失效了"。
// 两者取同一个值的话，队列会稳定停在阈值上反复穿越，告警变成噪声；
// 现在只有「准入被绕过或关掉」（例如重投回流、QUEUE_ADMIT_RATIO<0）才会响。
const queueSaturationWarnRatio = 0.9

// maintainQueue 周期性做两件事：回收「已确认前缀」，并上报水位指标。
//
// 为什么这两件事必须成对出现——它们是同一个问题的两面：
//
//	回收：XACK 不删消息本体，不回收的话 stream 只会单调增长；
//	上报：MAXLEN 裁剪掉的是**尚未被读走**的最老条目，而这个过程在 Redis 侧不留痕迹
//	      （XAUTOCLAIM 会把已被删除的 PEL 条目当成"已删除"静默移出 PEL）。
//	      也就是说，一旦水位打满，任务会开始无声无息地丢，事后查不出来。
//
// 所以护栏必须可见：StreamLen / MaxLen 两个指标一起上报，让告警能在 80% 时就响，
// 而不是等任务丢完了才从「pending 一直不减少」反推。
func (s *Scheduler) maintainQueue(ctx context.Context, stats queue.StreamStats) {
	tick := time.NewTicker(queueMaintainInterval)
	defer tick.Stop()

	warned := false // 只在跨越阈值时告警一次，避免每 10 秒刷同一条日志
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		d, err := s.maintainQueueOnce(ctx, stats)
		if err != nil {
			s.logger.Warn("queue maintenance failed", "error", err)
			continue
		}
		if d.saturated() {
			if !warned {
				// 走到这里说明准入（背压）没能把水位压住：要么被 QUEUE_ADMIT_RATIO<0 关了，
				// 要么是重投/崩溃认领回流绕过了准入。此时 MAXLEN 会开始裁剪未投递的消息。
				s.logger.Warn("task queue near capacity: MAXLEN 即将开始裁剪未消费的消息",
					"stream_len", d.streamLen, "max_len", d.maxLen, "admit_limit", d.admitLimit,
					"hint", "MAXLEN 裁掉的是最老条目，其中可能包含尚未投递的消息；"+
						"请检查 QUEUE_ADMIT_RATIO 是否被关闭，或调大 QUEUE_MAXLEN")
				warned = true
			}
		} else {
			warned = false
		}
	}
}

// queueDepth 是一轮队列维护的观测结果。
type queueDepth struct {
	streamLen  int64
	maxLen     int64
	dlqLen     int64
	admitLimit int64
	reclaimed  int64
}

// saturated 判断水位是否已达告警线。
func (d queueDepth) saturated() bool {
	return d.maxLen > 0 && float64(d.streamLen) >= float64(d.maxLen)*queueSaturationWarnRatio
}

// maintainQueueOnce 执行一轮维护：上报水位指标 → 回收已确认前缀。
//
// 刻意把「采样与回收」和「告警节流」拆开：前者是可被单测直接驱动的纯动作，
// 后者只是日志策略（每跨一次阈值告警一条），混在一起会让测试不得不等真实定时器。
func (s *Scheduler) maintainQueueOnce(ctx context.Context, stats queue.StreamStats) (queueDepth, error) {
	var d queueDepth

	// 顺序很重要：**先观测，再回收**。
	//
	// MAXLEN 是在 XADD 时刻裁剪的，它只会在「水位已经打到上界」时起作用。
	// 如果反过来先回收再读长度，读到的就是裁剪之后已经降下去的水位——
	// 正好把「刚刚发生过裁剪」这个信号抹掉，告警永远不会响。
	// 先读后删，读到的是本轮采样点上的水位高值，才是该拿去告警的那个数。
	streamLen, err := stats.StreamLen(ctx)
	if err != nil {
		return d, err
	}
	d.streamLen = streamLen
	d.maxLen = stats.MaxLen()
	d.admitLimit = stats.AdmitLimit()
	// DLQ 长度只是附带的观测项，取不到不影响主指标上报
	if dlqLen, err := stats.DLQLen(ctx); err == nil {
		d.dlqLen = dlqLen
	}
	if s.metrics != nil {
		s.metrics.SetQueueDepth(d.streamLen, d.maxLen, d.dlqLen, d.admitLimit)
	}

	reclaimed, err := stats.TrimConsumed(ctx)
	if err != nil {
		return d, err
	}
	d.reclaimed = reclaimed
	if reclaimed > 0 && s.metrics != nil {
		s.metrics.AddQueueReclaimed(reclaimed)
	}
	return d, nil
}

// detached 返回一个不受父 ctx 取消影响、但保留其 value 的短超时上下文。
// 用于"任务已被取消/超时，但仍要把最终状态写进数据库"的收尾路径——
// 否则服务优雅关闭时所有终态落库都会失败，前端永远停在 running。
func detached(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, timeout)
}

// executeTask 执行单个任务：解析 DAG → 并发调度节点 → 汇总结果。
// 队列消息在任务结束时 ACK；失败则按重试次数决定重投或进 DLQ。
//
// 返回值表示"引擎自身出了问题"（DB 抖动等，消息应留在 PEL 里重投）；
// 而**任务业务失败**时刻意返回 nil（任务已终态化、消息已 Nack 进 DLQ）。
// 因此 span 的成败不能只看返回值 —— 见下面的 spanErr。
func (s *Scheduler) executeTask(ctx context.Context, job queue.Job, msgID string) (retErr error) {
	// 父 span 在 API 侧（queue.enqueue），两者靠队列消息里的 traceparent 相连。
	// 有了这一条，链路才能从"提交"一路连到"执行完成"。
	ctx, span := tracing.StartConsumer(ctx, tracing.SpanTaskExecute,
		tracing.KV("nebulaflow.task.id", job.TaskID),
		tracing.KV("messaging.message.id", msgID),
		tracing.KV("nebulaflow.task.attempt", job.Attempt),
	)
	// spanErr 与 retErr 分开：只看返回值的话，一个**业务失败**的任务
	// 在 trace 上会显示成绿色，而失败任务恰恰是最需要看 trace 的那一类。
	var spanErr error
	defer func() {
		if spanErr != nil {
			tracing.Finish(span, spanErr)
			return
		}
		tracing.Finish(span, retErr)
	}()

	s.running.Add(1)
	defer s.running.Add(-1)
	start := time.Now()

	wfID := int64(0)
	counted := false
	finished := false
	if s.metrics != nil {
		s.metrics.ObserveTaskStarted()
		counted = true
	}
	// 兜底：任何异常 return 都必须归还 running 计数，
	// 否则 Prometheus gauge 单调上升，Dashboard 的"运行中任务"会永久虚高。
	defer func() {
		if counted && !finished && s.metrics != nil {
			s.metrics.ObserveTaskDone("abandoned", time.Since(start).Seconds(), fmt.Sprint(wfID))
			s.logger.Error("task exited without final status, metrics reconciled", "task_id", job.TaskID)
		}
	}()

	t, err := s.tasks.GetTaskByID(ctx, job.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			// 任务已被删除（通常是工作流被删后级联删除），但队列里还留着消息。
			// 原实现直接 return err：消息不 ACK，永远留在 PEL 里，
			// Recover 又不断把它重新投递回来 —— 一条死消息无限循环，
			// 还会把 abandoned 指标刷高。正确做法是确认后丢弃。
			s.logger.Warn("dropping queue message for deleted task", "task_id", job.TaskID)
			span.SetAttributes(tracing.KV("nebulaflow.task.dropped", true))
			_ = s.queue.Ack(ctx, msgID)
			// 计数已经在函数入口 +1，这里必须归还，否则每丢一条死消息
			// running gauge 就永久 +1（Dashboard 的"运行中任务"虚高）。
			if counted && s.metrics != nil {
				s.metrics.ObserveTaskDropped()
			}
			finished = true // 已明确处理，不该再计入"异常退出"的兜底指标
			return nil
		}
		// 其他错误（DB 抖动）保留消息，交给 PEL 重投
		return err
	}
	wfID = t.WorkflowID

	// 建立 task → trace 反查索引，并记录归属用户。
	// 归属校验是必须的：span 里会带上原始错误文本（可能含 SQL、表名、内网地址），
	// 查询接口不能只凭一个 trace id 就把内容交给任何人。
	if s.tracing != nil {
		s.tracing.Store().Link(tracing.TraceID(ctx), t.ID, t.UserID)
	}

	// fail 是"任务被判定失败"的唯一出口：既写终态（failTask），也把 span 标红。
	// 下面三处调用点都返回 nil，所以必须在这里显式记录 spanErr。
	fail := func(msg string) {
		spanErr = errors.New(msg)
		s.failTask(ctx, t, msg, msgID, start, &finished)
	}

	if t.Status == model.TaskCancelled {
		span.SetAttributes(tracing.KV("nebulaflow.task.status", string(model.TaskCancelled)))
		_ = s.queue.Ack(ctx, msgID)
		s.finish(ctx, t, model.TaskCancelled, "", "", msgID, start, &finished, false)
		return nil
	}
	wf, err := s.workflows.GetWorkflow(ctx, t.WorkflowID, t.UserID)
	if err != nil {
		// 工作流不存在/不可见：任务已终态化，返回 nil 避免上层误报"引擎异常"
		fail("workflow not found")
		return nil
	}
	dag, err := BuildDAG(wf.Nodes, wf.Edges)
	if err != nil {
		fail("invalid workflow: " + err.Error())
		return nil
	}

	// 分布式锁：防止同一工作流在多个实例上并发执行。
	// 获取失败时 Nack 重投（不是 DLQ——这不是业务失败，只是"还没轮到我"），
	// 让持锁实例完成后再消费这条消息。
	//
	// 为什么放在 DAG 校验之后、建节点之前：锁的粒度是工作流不是任务，
	// 放在取工作流之后就能用 wf.ID；放在建节点之前则保证 task_nodes 不会重复。
	if s.wfLock != nil {
		release, lockErr := s.wfLock.Acquire(ctx, wf.ID)
		if lockErr != nil {
			s.logger.Debug("workflow locked by another instance, requeueing",
				"task_id", t.ID, "workflow_id", wf.ID, "error", lockErr)
			// 短延迟重投：持锁实例通常很快完成（taskTimeout 5min，锁 TTL 10min），
			// 3 秒后重试是"等一下再来看看"的合理间隔。
			_ = s.queue.Nack(ctx, msgID, "workflow locked", 3)
			finished = true
			return nil
		}
		defer release()
	}

	// 任务重试（Nack 重投）时会再次进入这里，幂等建节点，避免 task_nodes 重复行
	if n, err := s.tasks.CountTaskNodes(ctx, t.ID); err == nil && n == 0 {
		if err := s.tasks.CreateTaskNodes(ctx, t.ID, wf.Nodes); err != nil {
			fail("init task nodes: " + err.Error())
			return nil
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, s.taskTimeout)
	defer cancel()
	s.mu.Lock()
	s.cancels[t.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.cancels, t.ID)
		delete(s.cancelled, t.ID)
		s.mu.Unlock()
	}()

	if err := s.tasks.UpdateTaskStatus(ctx, t.ID, model.TaskRunning, "", ""); err != nil {
		// 启动失败：拒绝消息（DLQ），避免无限重试与 PEL 泄漏
		_ = s.queue.Nack(ctx, msgID, "init running: "+err.Error(), 0)
		finished = true
		spanErr = fmt.Errorf("init running: %w", err)
		if s.metrics != nil {
			s.metrics.ObserveTaskDone("failed", time.Since(start).Seconds(), fmt.Sprint(wfID))
		}
		s.logger.Error("task failed to start", "task_id", t.ID, "error", err)
		return nil
	}
	s.hub.Publish(task.NewEvent(task.EventTaskRunning, t.ID))

	// ---- 并发调度主循环 ----
	// done 容量 = 节点数，worker 完成即投递，不会阻塞执行线程。
	done := make(chan nodeOutcome, len(dag.Nodes))

	inDegree := make(map[string]int, len(dag.InDegree))
	for k, v := range dag.InDegree {
		inDegree[k] = v
	}
	// submitted 表示"该节点已有结论（已提交/已跳过/已失败）"，不再参与就绪判定
	submitted := make(map[string]bool, len(dag.Nodes))
	results := make(map[string]string, len(dag.Nodes))
	remaining := len(dag.Nodes)
	var firstErr error
	// aborted 区分两类失败：
	//   - true：超时 / 进程退出等瞬时故障 → 允许任务级重投（换台机器大概率能成）
	//   - false：节点业务失败（节点内部已经按 maxRetry 重试过）→ 直接进 DLQ，
	//            否则一次确定性错误会把整条链路重放 N 遍，白白烧 token
	aborted := false

	// 标记节点为 skipped：落库 + 推事件 + 递减待完成计数
	markSkipped := func(key, reason string) {
		if submitted[key] {
			return
		}
		submitted[key] = true
		remaining--
		s.skipNode(runCtx, t.ID, key, dag.Nodes[key].Type, reason)
	}

	// 失败传播：把 from 的全部下游后代标记为 skipped。
	// 这是 DAG 引擎最容易写错的一处——漏掉它，失败的分支会带着空输入继续跑，
	// 用户看到的"任务失败"和"下游节点成功"会自相矛盾。
	skipDownstream := func(from, reason string) {
		stack := append([]string(nil), dag.Edges[from]...)
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			inDegree[cur]--
			markSkipped(cur, reason)
			stack = append(stack, dag.Edges[cur]...)
		}
	}

	submitReady := func() {
		for _, key := range dag.ReadyNodes(inDegree, submitted) {
			if submitted[key] {
				continue
			}
			submitted[key] = true
			input := s.assembleInput(key, dag, results, t.Input)
			key := key
			j := worker.NodeJob{
				TaskID: t.ID, UserID: t.UserID, WorkflowID: wf.ID,
				NodeKey: key, NodeType: dag.Nodes[key].Type,
				Config: dag.Nodes[key].Config, Input: input,
				Ctx: runCtx, // 任务级上下文：取消/超时真正生效
				OnDone: func(res worker.NodeResult, err error) {
					done <- nodeOutcome{key: key, res: res, err: err}
				},
			}
			if !s.pool.Submit(j) {
				s.logger.Warn("submit rejected", "task_id", t.ID, "node", key)
				remaining--
				if firstErr == nil {
					firstErr = fmt.Errorf("worker pool is shutting down")
				}
				skipDownstream(key, "worker pool unavailable")
			}
		}
	}

	for remaining > 0 {
		submitReady()
		if remaining <= 0 {
			break
		}
		select {
		case o := <-done:
			remaining--
			if o.err != nil {
				if firstErr == nil {
					firstErr = o.err
				}
				skipDownstream(o.key, "上游节点 "+o.key+" 执行失败")
			} else {
				results[o.key] = o.res.Output
				for _, target := range dag.Edges[o.key] {
					inDegree[target]--
				}
			}
		case <-runCtx.Done():
			if firstErr == nil {
				firstErr = s.classifyAbort(t.ID, runCtx)
			}
			aborted = true
			remaining = 0
			// 收口所有还没结论的节点。不做这一步，取消后下游节点会永远停在
			// pending：任务状态是 cancelled，节点却显示"排队中"，前端自相矛盾。
			// 正在执行的节点由它自己在 OnDone 里写 cancelled（见 ExecuteNode）。
			for key, node := range dag.Nodes {
				if submitted[key] {
					continue
				}
				submitted[key] = true
				s.markNode(runCtx, t.ID, key, node.Type, model.NodeCancelled, "任务已取消")
			}
		}
	}

	if firstErr != nil {
		status := model.TaskFailed
		if errors.Is(firstErr, ErrTaskCancelled) {
			status = model.TaskCancelled
		}
		span.SetAttributes(tracing.KV("nebulaflow.task.status", string(status)))
		// 用户主动取消不是"失败"：系统按预期停下来了。
		// 把它标成红色会让 trace 面板上的错误率失去意义（用户取消会变成主要噪声源）。
		if status != model.TaskCancelled {
			spanErr = firstErr
		}
		// 收尾写库必须脱离已取消的 runCtx，否则永远写不进去
		finalCtx, cancelFinal := detached(ctx, 10*time.Second)
		defer cancelFinal()
		s.finish(finalCtx, t, status, "", firstErr.Error(), msgID, start, &finished,
			aborted && !errors.Is(firstErr, ErrTaskCancelled))
		s.logger.Info("task finished", "task_id", t.ID, "status", status, "error", firstErr)
		return nil
	}

	// 成功：优先取 output 类型节点，否则取拓扑序最后一个成功结果
	final := s.finalOutput(dag, results)
	span.SetAttributes(tracing.KV("nebulaflow.task.status", string(model.TaskSucceeded)))
	finalCtx, cancelFinal := detached(ctx, 10*time.Second)
	defer cancelFinal()
	s.finish(finalCtx, t, model.TaskSucceeded, final, "", msgID, start, &finished, false)
	s.logger.Info("task finished", "task_id", t.ID, "status", "succeeded",
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

// classifyAbort 区分"用户取消 / 任务超时 / 进程退出"三种中止原因。
// 用户取消 → cancelled（终态）；超时 → failed；进程退出 → 保留 pending 语义，
// 让消息留在 PEL 里由 Recover 重新投递，避免关一次服务就丢一批任务。
func (s *Scheduler) classifyAbort(taskID int64, runCtx context.Context) error {
	s.mu.Lock()
	userCancelled := s.cancelled[taskID]
	s.mu.Unlock()
	switch {
	case userCancelled:
		return ErrTaskCancelled
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("task timeout after %s", s.taskTimeout)
	default:
		return fmt.Errorf("task aborted: %w", runCtx.Err())
	}
}

// finalOutput 决定任务的最终输出：
// 优先 output 类型节点（取拓扑序最后一个），否则回退到最后一个成功的节点。
func (s *Scheduler) finalOutput(dag *DAG, results map[string]string) string {
	for i := len(dag.Order) - 1; i >= 0; i-- {
		key := dag.Order[i]
		if dag.Nodes[key].Type == model.NodeOutput {
			if v, ok := results[key]; ok {
				return v
			}
		}
	}
	for i := len(dag.Order) - 1; i >= 0; i-- {
		if v, ok := results[dag.Order[i]]; ok {
			return v
		}
	}
	return ""
}

// finish 统一收口：落库 + SSE 事件 + 指标 + 队列 ACK/Nack。
//
// retryOnFail 决定失败时是否重新入队：
// 只有超时/进程退出这类瞬时故障才重投；节点业务失败直接进 DLQ，
// 否则一个确定性错误（比如工具参数不合法）会把整条链路重放 N 遍。
func (s *Scheduler) finish(ctx context.Context, t *model.Task, status model.TaskStatus,
	output, errMsg, msgID string, start time.Time, finished *bool, retryOnFail bool) {
	if *finished {
		return
	}
	*finished = true

	if err := s.tasks.UpdateTaskStatus(ctx, t.ID, status, output, errMsg); err != nil {
		s.logger.Error("update task status failed", "task_id", t.ID, "status", status, "error", err)
	}
	switch status {
	case model.TaskSucceeded:
		s.hub.Publish(task.NewEvent(task.EventTaskCompleted, t.ID))
	case model.TaskCancelled:
		s.hub.Publish(task.NewEvent(task.EventTaskCancelled, t.ID))
	default:
		s.hub.Publish(task.NewEvent(task.EventTaskFailed, t.ID))
	}
	if s.metrics != nil {
		s.metrics.ObserveTaskDone(string(status), time.Since(start).Seconds(), fmt.Sprint(t.WorkflowID))
	}
	if status == model.TaskSucceeded || status == model.TaskCancelled {
		_ = s.queue.Ack(ctx, msgID)
		return
	}
	retryLeft := 0
	if retryOnFail {
		retryLeft = s.defaultMaxRetry
	}
	if err := s.queue.Nack(ctx, msgID, errMsg, retryLeft); err != nil {
		s.logger.Warn("nack failed", "task_id", t.ID, "error", err)
	}
}

// failTask 处理任务启动阶段（进入调度主循环前）的致命错误。
func (s *Scheduler) failTask(ctx context.Context, t *model.Task, errMsg, msgID string,
	start time.Time, finished *bool) {
	if *finished {
		return
	}
	*finished = true
	_ = s.tasks.UpdateTaskStatus(ctx, t.ID, model.TaskFailed, "", errMsg)
	s.hub.Publish(task.NewEvent(task.EventTaskFailed, t.ID))
	if s.metrics != nil {
		s.metrics.ObserveTaskDone("failed", time.Since(start).Seconds(), fmt.Sprint(t.WorkflowID))
	}
	_ = s.queue.Nack(ctx, msgID, errMsg, 0)
	s.logger.Warn("task failed before scheduling", "task_id", t.ID, "reason", errMsg)
}

// markNode 把节点收敛到某个终态（skipped / cancelled）并落库 + 推事件。
// 收尾写库必须脱离已取消的 runCtx，否则状态永远写不进去。
func (s *Scheduler) markNode(ctx context.Context, taskID int64, key string,
	nodeType model.NodeType, status model.TaskNodeStatus, reason string) {
	n := &model.TaskNode{
		TaskID: taskID, NodeKey: key, NodeType: nodeType,
		Status: status, Error: reason,
	}
	writeCtx, cancel := writable(ctx, 5*time.Second)
	defer cancel()
	if err := s.tasks.UpdateTaskNode(writeCtx, n); err != nil {
		s.logger.Error("update task node status failed", "task_id", taskID,
			"node", key, "status", status, "error", err)
	}
	ev := task.NewEvent(eventForStatus(status), taskID)
	ev.NodeKey = key
	ev.NodeType = string(nodeType)
	ev.NodeStatus = string(status)
	ev.Message = reason
	s.hub.Publish(ev)
}

// skipNode 把节点标记为 skipped（上游失败导致的跳过）。
func (s *Scheduler) skipNode(ctx context.Context, taskID int64, key string,
	nodeType model.NodeType, reason string) {
	s.markNode(ctx, taskID, key, nodeType, model.NodeSkipped, reason)
}

// assembleInput 计算节点输入：
//   - input 节点直接取任务输入；
//   - 单依赖取上游输出；
//   - 多依赖用【上游 key】标注后拼装，避免多份上下文混在一起无法溯源
//     （原实现直接裸拼，LLM 分不清哪段来自哪个分支）。
func (s *Scheduler) assembleInput(key string, dag *DAG, results map[string]string, taskInput string) string {
	if dag.Nodes[key].Type == model.NodeInput {
		return taskInput
	}
	inputs := dag.InputsOf(key)
	if len(inputs) == 0 {
		return taskInput
	}
	if len(inputs) == 1 {
		if v, ok := results[inputs[0]]; ok && v != "" {
			return v
		}
		return taskInput
	}
	var parts []string
	for _, in := range inputs {
		v := results[in]
		if v == "" {
			continue
		}
		parts = append(parts, "【"+in+"】\n"+v)
	}
	if len(parts) == 0 {
		return taskInput
	}
	return strings.Join(parts, "\n\n")
}

// Package worker 实现带优雅退出的 Worker Pool。
//
// 设计：
//   - 固定数量 worker 从带缓冲的 job channel 并发取任务；
//   - 每个 job 由 Executor 执行（由 scheduler 注入节点执行逻辑）；
//   - Submit 在队列满时阻塞形成背压；Shutdown 先关闭接收闸门，
//     再等待在途 Submit 结束，最后 close(job channel) 让 worker 排空退出；
//   - 通过 context 支持任务级取消（NodeJob.Ctx）与全局停止（poolCtx）。
//
// 并发安全性（这里是最容易被问倒的地方）：
//   - closed 闸门用 CAS，不持锁阻塞；
//   - in-flight Submit 用 WaitGroup 计数，保证 close(jobs) 时没有任何 sender，
//     杜绝 "send on closed channel" panic；
//   - Shutdown 全程不持有互斥锁睡眠，因此不会因为某个 Submit 卡在
//     满队列上而死锁（原实现用 RWMutex 正是这个问题）。
//
// 对应文档要点：goroutine / channel / mutex / context / graceful shutdown。
package worker

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// NodeJob 是 Worker Pool 的最小执行单元：一个工作流节点。
type NodeJob struct {
	TaskID     int64
	UserID     int64
	WorkflowID int64
	NodeKey    string
	NodeType   model.NodeType
	Config     model.NodeConfig
	Input      string
	// Ctx 是任务级上下文（由 scheduler 传入，带任务超时与取消）。
	// 为 nil 时回退到 pool 级上下文。节点内部的所有 IO 都应尊重它，
	// 否则"取消任务"只能改数据库状态、实际执行却停不下来。
	Ctx context.Context
	// OnDone 在 ExecuteNode 返回后被调用（无论成败），
	// 由提交方（scheduler）注入，用于把结果送回任务调度主循环。
	OnDone func(res NodeResult, err error)
}

// NodeResult 是节点执行结果（LLM 节点附带用量信息）。
type NodeResult struct {
	Output       string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	DurationMS   int64
}

// Executor 定义节点的执行方式（scheduler 实现，串起 LLM/RAG/Tool）。
type Executor interface {
	ExecuteNode(ctx context.Context, j NodeJob) (NodeResult, error)
}

// MetricsSink 是 Worker Pool 向可观测性暴露状态的窄接口，
// 避免 worker 包反向依赖 observability 包。
type MetricsSink interface {
	SetWorkersActive(n int64)
	SetQueueSize(n int64)
}

type WorkerPool struct {
	workers int
	jobs    chan NodeJob
	exec    Executor
	logger  *slog.Logger
	metrics MetricsSink

	poolCtx  context.Context
	cancelFn context.CancelFunc
	quit     chan struct{}
	quitOnce sync.Once

	active   atomic.Int64
	wg       sync.WaitGroup
	inflight sync.WaitGroup // 正在执行 Submit 的 goroutine 计数
	closed   atomic.Bool
	mu       sync.Mutex // 保护 exec 的读写
}

func NewWorkerPool(ctx context.Context, workers int, queueCap int, exec Executor, logger *slog.Logger, metrics MetricsSink) *WorkerPool {
	if workers <= 0 {
		workers = 1
	}
	if queueCap <= 0 {
		queueCap = workers * 4
	}
	if logger == nil {
		logger = slog.Default()
	}
	poolCtx, cancel := context.WithCancel(ctx)
	return &WorkerPool{
		workers:  workers,
		jobs:     make(chan NodeJob, queueCap),
		exec:     exec,
		logger:   logger,
		metrics:  metrics,
		poolCtx:  poolCtx,
		cancelFn: cancel,
		quit:     make(chan struct{}),
	}
}

// SetExecutor 注入节点执行器（scheduler 实现），必须在 Start 前调用。
func (p *WorkerPool) SetExecutor(e Executor) {
	p.mu.Lock()
	p.exec = e
	p.mu.Unlock()
}

func (p *WorkerPool) executor() Executor {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exec
}

// Start 启动固定数量 worker goroutine。
func (p *WorkerPool) Start() {
	p.wg.Add(p.workers)
	for i := 0; i < p.workers; i++ {
		go p.runWorker()
	}
	p.updateMetrics()
}

func (p *WorkerPool) runWorker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.quit:
			return
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			p.execute(j)
		}
	}
}

func (p *WorkerPool) execute(j NodeJob) {
	p.active.Add(1)
	p.updateMetrics()
	defer func() {
		p.active.Add(-1)
		p.updateMetrics()
	}()

	// 任务级 ctx 优先，保证取消/超时能真正打断节点执行
	execCtx := j.Ctx
	if execCtx == nil {
		execCtx = p.poolCtx
	}
	exec := p.executor()
	if exec == nil {
		p.logger.Error("worker pool has no executor configured", "task_id", j.TaskID, "node", j.NodeKey)
		if j.OnDone != nil {
			j.OnDone(NodeResult{}, context.Canceled)
		}
		return
	}
	// 单个节点 panic 不能带走整个 worker（否则 worker 数逐渐归零）
	res, err := safeExecute(exec, execCtx, j, p.logger)
	if err != nil {
		p.logger.Warn("worker node failed",
			"task_id", j.TaskID, "node", j.NodeKey, "error", err)
	}
	if j.OnDone != nil {
		j.OnDone(res, err)
	}
}

// safeExecute 把节点 panic 转成 error，保证 worker 长驻。
func safeExecute(e Executor, ctx context.Context, j NodeJob, logger *slog.Logger) (res NodeResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("node execution panicked", "task_id", j.TaskID, "node", j.NodeKey, "panic", r)
			err = context.Canceled
		}
	}()
	return e.ExecuteNode(ctx, j)
}

func (p *WorkerPool) updateMetrics() {
	if p.metrics != nil {
		p.metrics.SetWorkersActive(p.active.Load())
		p.metrics.SetQueueSize(int64(len(p.jobs)))
	}
}

// Submit 提交节点任务。队列满时阻塞等待空位（背压），
// Shutdown 开始后拒绝新任务并返回 false。
func (p *WorkerPool) Submit(j NodeJob) bool {
	if p.closed.Load() {
		return false
	}
	// 在途计数：Shutdown 依赖它判断"是否还有 sender"
	p.inflight.Add(1)
	defer p.inflight.Done()
	if p.closed.Load() {
		return false
	}

	select {
	case p.jobs <- j:
		p.updateMetrics()
		return true
	case <-p.quit:
		return false
	case <-p.poolCtx.Done():
		return false
	}
}

// Shutdown 优雅关闭：
//  1. CAS 关闭接收闸门，后续 Submit 立即失败；
//  2. 等待在途 Submit 结束（worker 会持续消费而排空队列）；
//  3. close(job channel)，worker 处理完剩余任务后退出；
//  4. 超时则取消执行上下文并强制唤醒所有阻塞方。
//
// 全程不持锁睡眠，因此不会与阻塞在满队列上的 Submit 互相等待。
func (p *WorkerPool) Shutdown(timeout time.Duration) {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// --- 阶段一：等所有 sender 退出，确保 close(jobs) 安全 ---
	inflightDone := make(chan struct{})
	go func() {
		p.inflight.Wait()
		close(inflightDone)
	}()
	select {
	case <-inflightDone:
	case <-time.After(timeout):
		p.logger.Warn("worker pool: submitters still blocked, forcing quit")
		p.quitOnce.Do(func() { close(p.quit) })
		<-inflightDone
	}
	close(p.jobs)

	// --- 阶段二：等 worker 排空 ---
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		p.logger.Warn("worker pool drain timeout, cancelling executions")
		p.quitOnce.Do(func() { close(p.quit) })
		p.cancelFn()
		<-done
	}

	p.updateMetrics()
	if p.metrics != nil {
		p.metrics.SetWorkersActive(0)
		p.metrics.SetQueueSize(0)
	}
	// 释放 poolCtx 关联资源（此时已无在途执行）
	p.cancelFn()
}

// Stats 返回当前活跃（正在执行节点）的 worker 数与真实排队任务数。
// 排队数直接读 channel 长度，而不是统计 Submit 的调用次数。
func (p *WorkerPool) Stats() (active, queued int64) {
	return p.active.Load(), int64(len(p.jobs))
}

// Workers 返回配置的 worker 总数。
func (p *WorkerPool) Workers() int { return p.workers }

// Capacity 返回 job channel 容量（背压上限）。
func (p *WorkerPool) Capacity() int { return cap(p.jobs) }

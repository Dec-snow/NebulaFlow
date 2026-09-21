package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/ratelimit"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// Canceller 由 scheduler 实现，用于真正中断运行中的任务。
// 这里用窄接口解耦：task 包不反向依赖 scheduler 包，避免循环 import。
type Canceller interface {
	// Cancel 中断任务执行（撤销任务级 context）；返回 false 表示任务不在本机执行。
	Cancel(taskID int64) bool
}

// Service 是任务领域服务：创建/查询/取消 + 入队 + 提交限流。
type Service struct {
	store     *store.Store
	queue     queue.Queue
	hub       *Hub
	limiter   ratelimit.Limiter
	canceller Canceller
}

func NewService(st *store.Store, q queue.Queue, hub *Hub) *Service {
	return &Service{store: st, queue: q, hub: hub}
}

// SetLimiter 注入限流器（main 装配时调用）。
func (s *Service) SetLimiter(l ratelimit.Limiter) { s.limiter = l }

// SetCanceller 注入取消器（main 在 scheduler 创建后调用）。
func (s *Service) SetCanceller(c Canceller) { s.canceller = c }

// RateAllowed 检查用户每分钟任务提交上限。
func (s *Service) RateAllowed(ctx context.Context, userID int64, perMin int) bool {
	if s.limiter == nil || perMin <= 0 {
		return true
	}
	res, err := s.limiter.Allow(ctx, fmt.Sprintf("user:%d:tasks", userID), perMin, time.Minute)
	return err == nil && res.Allowed
}

var (
	ErrTaskNotCancellable = errors.New("task is not in a cancellable state")
	ErrTaskNotRunnable    = errors.New("workflow has no valid executable nodes")
)

// Admit 在落库之前做一次队列准入预检。
//
// 为什么要有这一步：Enqueue 里的准入检查发生在任务行已经落库之后，
// 于是每个被拒绝的请求仍然留下一条 failed 记录——过载时拒绝得越多，
// 往数据库压的写就越多，而背压的目的恰恰是保护数据库。
// 预检把拒绝挡在建行之前，拒绝路径只剩一次 XLEN。
//
// 队列不支持预检（内存队列、或准入被关闭）时返回 nil，行为与改造前一致。
func (s *Service) Admit(ctx context.Context) error {
	if a, ok := s.queue.(queue.Admitter); ok {
		return a.Admit(ctx)
	}
	return nil
}

// CreateTask 创建任务记录并入队。
// 返回 task；入队失败时任务标记为 failed（不丢数据）。
//
// 这是全链路 trace 的**起点**：`task.create` 覆盖准入预检与落库，
// `queue.enqueue` 覆盖入队那一次 Redis 往返，并把当前 span 的 traceparent
// 写进队列消息——worker 侧据此把 `task.execute` 接成它的子 span，
// 而不是另起一条互不相连的链。
func (s *Service) CreateTask(ctx context.Context, userID, workflowID int64, input string) (t *model.Task, err error) {
	ctx, span := tracing.StartInternal(ctx, tracing.SpanTaskCreate,
		tracing.KV("nebulaflow.user.id", userID),
		tracing.KV("nebulaflow.workflow.id", workflowID),
		tracing.KV("nebulaflow.task.input_length", len(input)),
	)
	defer func() {
		// 任务 ID 是落库之后才有的，只能在收尾时补上属性。
		if t != nil {
			span.SetAttributes(tracing.KV("nebulaflow.task.id", t.ID))
		}
		tracing.Finish(span, err)
	}()

	// 先问队列还能不能收，再落库。
	// 这只是优化而非正确性保证：预检与入队之间仍可能被别的实例填满，
	// 那个窗口由 Enqueue 内部的检查兜底（届时任务会被标 failed）。
	if err := s.Admit(ctx); err != nil {
		return nil, err
	}

	t = &model.Task{
		WorkflowID: workflowID,
		UserID:     userID,
		Status:     model.TaskPending,
		Input:      input,
	}
	if err := s.store.Tasks.CreateTask(ctx, t); err != nil {
		return nil, err
	}
	s.hub.Publish(NewEvent(EventTaskCreated, t.ID))

	// 入队单独一个 span。它既是一次跨进程的 Redis 往返，
	// 也是 queue_wait_ms 的计时起点（从它结束到 task.execute 开始之间的空档）。
	enqCtx, enqSpan := tracing.StartInternal(ctx, tracing.SpanQueueEnqueue,
		tracing.KV("messaging.system", "redis"),
		tracing.KV("messaging.operation", "publish"),
		tracing.KV("nebulaflow.task.id", t.ID),
	)
	err = s.queue.Enqueue(enqCtx, queue.Job{
		TaskID: t.ID,
		// 传播点：把当前 span 编码成 traceparent 塞进消息体。
		// tracing 未启用时返回空串，行为与改造前完全一致。
		TraceParent: tracing.Inject(enqCtx),
	})
	tracing.Finish(enqSpan, err)
	if err != nil {
		_ = s.store.Tasks.UpdateTaskStatus(ctx, t.ID, model.TaskFailed, "", "enqueue failed: "+err.Error())
		return t, err
	}
	return t, nil
}

// CancelTask 取消任务。
//
// 两段式：先让 scheduler 撤销任务级 context（真正打断 LLM 流式调用等长耗时 IO），
// 再把数据库状态推进到 cancelled。只改数据库是不够的——原实现就是只改库，
// 结果用户点了"取消"，节点还在后台跑完，前端却已经显示已取消。
// 未在执行的任务（pending 或跑在别的实例上）仅靠数据库状态即可，
// scheduler 出队时会检查到 cancelled 并直接 ACK。
func (s *Service) CancelTask(ctx context.Context, taskID, userID int64) error {
	t, err := s.store.Tasks.GetTask(ctx, taskID, userID)
	if err != nil {
		return err
	}
	if t.Status != model.TaskPending && t.Status != model.TaskRunning {
		return ErrTaskNotCancellable
	}

	// 1) 中断执行（若任务在本机运行）
	cancelledInPlace := false
	if s.canceller != nil {
		cancelledInPlace = s.canceller.Cancel(taskID)
	}

	// 2) 落库终态；运行中的任务若已被引擎感知，引擎也会写一次，结果一致
	if err := s.store.Tasks.UpdateTaskStatus(ctx, taskID, model.TaskCancelled, t.Output, "cancelled by user"); err != nil {
		return err
	}
	s.hub.Publish(NewEvent(EventTaskCancelled, taskID))
	_ = cancelledInPlace
	return nil
}

func (s *Service) GetTask(ctx context.Context, taskID, userID int64) (*model.Task, error) {
	return s.store.Tasks.GetTask(ctx, taskID, userID)
}

func (s *Service) ListTasks(ctx context.Context, userID int64, limit, offset int) ([]model.Task, error) {
	return s.store.Tasks.ListTasks(ctx, userID, limit, offset)
}

func (s *Service) GetLogs(ctx context.Context, taskID, userID int64, limit int) ([]model.TaskLog, error) {
	if _, err := s.store.Tasks.GetTask(ctx, taskID, userID); err != nil {
		return nil, err
	}
	return s.store.Tasks.ListLogs(ctx, taskID, limit)
}

// Hub 暴露给 scheduler / API 使用。
func (s *Service) Hub() *Hub { return s.hub }

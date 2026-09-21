package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/queue"
)

// --- MemoryWorkflowLock ---

func TestMemoryLockAcquireAndRelease(t *testing.T) {
	l := NewMemoryWorkflowLock()
	ctx := context.Background()

	release, err := l.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	release()

	// 释放后可以再次获取
	release2, err := l.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("second acquire after release failed: %v", err)
	}
	release2()
}

func TestMemoryLockRejectsConcurrentAcquire(t *testing.T) {
	l := NewMemoryWorkflowLock()
	ctx := context.Background()

	release, err := l.Acquire(ctx, 42)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release()

	// 同一工作流第二次获取必须失败
	_, err = l.Acquire(ctx, 42)
	if err != ErrWorkflowLocked {
		t.Fatalf("second acquire should return ErrWorkflowLocked, got: %v", err)
	}
}

func TestMemoryLockDifferentWorkflowsIndependent(t *testing.T) {
	l := NewMemoryWorkflowLock()
	ctx := context.Background()

	release1, err := l.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("acquire wf 1 failed: %v", err)
	}
	defer release1()

	// 不同工作流不受影响
	release2, err := l.Acquire(ctx, 2)
	if err != nil {
		t.Fatalf("acquire wf 2 should succeed while wf 1 is held, got: %v", err)
	}
	release2()
}

func TestMemoryLockReleaseIsIdempotent(t *testing.T) {
	l := NewMemoryWorkflowLock()
	ctx := context.Background()

	release, _ := l.Acquire(ctx, 1)
	release()
	release() // 不应 panic

	// 释放两次后仍可重新获取
	release2, err := l.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("acquire after double release failed: %v", err)
	}
	release2()
}

func TestMemoryLockDoesNotBlockAcrossWorkflows(t *testing.T) {
	l := NewMemoryWorkflowLock()
	ctx := context.Background()

	// 获取 10 个不同工作流的锁，全部应该成功
	var releases []func()
	for i := 0; i < 10; i++ {
		r, err := l.Acquire(ctx, int64(i))
		if err != nil {
			t.Fatalf("acquire wf %d failed: %v", i, err)
		}
		releases = append(releases, r)
	}
	for _, r := range releases {
		r()
	}
}

// --- 装置自检 ---

// 确保测试装置本身正确：空锁从不拒绝。
func TestMemoryLockEmptyStartHasNoLocks(t *testing.T) {
	l := NewMemoryWorkflowLock()
	ctx := context.Background()

	// 初始状态：任何工作流都应该能获取
	for i := int64(1); i <= 5; i++ {
		release, err := l.Acquire(ctx, i)
		if err != nil {
			t.Fatalf("fresh lock should acquire wf %d, got: %v", i, err)
		}
		release()
	}
}

// --- RedisWorkflowLock（需要 Redis，用 miniredis 跑） ---

// 由于本机 Redis 是 miniredis，我们用一个最小化的方式验证：
// 如果 Redis 不可用，Acquire 退化为不加锁（不阻断执行）。
func TestRedisLockDegradesGracefullyWhenRedisDown(t *testing.T) {
	// 构造一个指向不存在端口的 client
	l := NewRedisWorkflowLock(nil, time.Minute)
	ctx := context.Background()

	// rdb == nil 时 Acquire 应该退化为不加锁
	release, err := l.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("acquire with nil rdb should degrade to no-op, got: %v", err)
	}
	if release == nil {
		t.Fatal("release function should not be nil")
	}
	release() // 不应 panic
}

func TestRedisLockLocalMutexRejectsDoubleAcquire(t *testing.T) {
	l := NewRedisWorkflowLock(nil, time.Minute)
	ctx := context.Background()

	// 第一次：rdb == nil 退化成功
	release1, err := l.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	// 但 local map 已标记，第二次应该被本地互斥拦住
	_, err = l.Acquire(ctx, 1)
	if err != ErrWorkflowLocked {
		t.Fatalf("second acquire should be rejected by local mutex, got: %v", err)
	}
	release1()

	// 释放后可以再次获取
	release2, err := l.Acquire(ctx, 1)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	release2()
}

// --- 调度器集成：executeTask 正确获取和释放锁 ---

// trackingLock 是一个可观测的锁，记录 Acquire / Release 的调用次数。
type trackingLock struct {
	mu         sync.Mutex
	acquired   map[int64]int // workflowID → acquire 次数
	released   map[int64]int
	inner      WorkflowLock // 真正的锁实现
	acquireErr error        // 非 nil 时 Acquire 返回这个错误
}

func newTrackingLock(inner WorkflowLock) *trackingLock {
	return &trackingLock{
		acquired: map[int64]int{},
		released: map[int64]int{},
		inner:    inner,
	}
}

func (t *trackingLock) Acquire(ctx context.Context, workflowID int64) (func(), error) {
	t.mu.Lock()
	t.acquired[workflowID]++
	if t.acquireErr != nil {
		err := t.acquireErr
		t.mu.Unlock()
		return nil, err
	}
	t.mu.Unlock()
	rel, err := t.inner.Acquire(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	return func() {
		t.mu.Lock()
		t.released[workflowID]++
		t.mu.Unlock()
		rel()
	}, nil
}

func (t *trackingLock) acquireCount(wfID int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acquired[wfID]
}

func (t *trackingLock) releaseCount(wfID int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.released[wfID]
}

// TestExecuteTaskAcquiresAndReleasesWorkflowLock
// 验证 executeTask 在执行期间持有工作流锁，执行完成后释放。
func TestExecuteTaskAcquiresAndReleasesWorkflowLock(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "llm", NodeType: model.NodeLLM, Config: model.NodeConfig{Model: "mock-chat"}},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "llm"},
		{SourceNode: "llm", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))

	lock := newTrackingLock(NewMemoryWorkflowLock())
	rig.sched.SetWorkflowLock(lock)

	tk := &model.Task{ID: 200, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "hello"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}

	// 装置自检：任务必须成功
	if tk.Status != model.TaskSucceeded {
		t.Fatalf("task should succeed, got %s", tk.Status)
	}

	// 核心断言：锁被获取且恰好一次
	if acq := lock.acquireCount(1); acq != 1 {
		t.Fatalf("lock should be acquired exactly once for wf 1, got %d", acq)
	}
	// 核心断言：锁被释放且恰好一次
	if rel := lock.releaseCount(1); rel != 1 {
		t.Fatalf("lock should be released exactly once for wf 1, got %d", rel)
	}
}

// TestExecuteTaskRequeuesWhenWorkflowLocked
// 验证锁被持有时 executeTask 会 Nack 重投（而不是 DLQ 或丢弃）。
func TestExecuteTaskRequeuesWhenWorkflowLocked(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))

	// 用一个总是返回 ErrWorkflowLocked 的锁
	lock := newTrackingLock(NewMemoryWorkflowLock())
	lock.acquireErr = ErrWorkflowLocked
	rig.sched.SetWorkflowLock(lock)

	// 先入队，这样 Nack 才有消息可操作
	tk := &model.Task{ID: 201, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)
	if err := rig.queue.Enqueue(context.Background(), queue.Job{TaskID: tk.ID}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 真正走 consumeLoop → Dequeue → executeTask，这样 msgID 存在于 inflight 里
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rig.sched.consumeLoop(ctx, "wf-lock-test")

	// 等 consumeLoop 消费并尝试获取锁
	time.Sleep(200 * time.Millisecond)

	// 装置自检：锁确实被尝试获取了（至少一次）
	if acq := lock.acquireCount(1); acq < 1 {
		t.Fatalf("lock should be attempted at least once, got %d", acq)
	}

	// 核心断言：任务状态不应变成 succeeded（它没真正执行）
	if tk.Status == model.TaskSucceeded {
		t.Fatal("locked task must not succeed")
	}

	// 核心断言：锁被尝试多次（消息被 Nack 重投后会重新进入 consumeLoop，
	// 所以 acquireCount 应 > 1，证明消息在被反复重投而不是被丢弃或 Ack）
	if acq := lock.acquireCount(1); acq < 2 {
		t.Fatalf("locked task should be requeued and attempted multiple times (acquireCount >= 2), got %d", acq)
	}
}

// TestExecuteTaskDifferentWorkflowsLockIndependently
// 验证不同工作流的锁互不影响。
func TestExecuteTaskDifferentWorkflowsLockIndependently(t *testing.T) {
	// wf 1
	wf1Nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	wf1Edges := []model.WorkflowEdge{{SourceNode: "in", TargetNode: "out"}}

	rig := newTestRig(t, wfOf(1, 1, wf1Nodes, wf1Edges))
	lock := newTrackingLock(NewMemoryWorkflowLock())
	rig.sched.SetWorkflowLock(lock)

	// wf 1 的任务
	tk1 := &model.Task{ID: 210, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "a"}
	rig.tasks.seed(tk1)
	if err := rig.run(t, tk1.ID); err != nil {
		t.Fatalf("executeTask 1: %v", err)
	}

	// wf 2 的任务（需要另一份工作流）
	rig2 := newTestRig(t, wfOf(2, 1, wf1Nodes, wf1Edges))
	rig2.sched.SetWorkflowLock(lock) // 共用同一个锁实例
	tk2 := &model.Task{ID: 211, WorkflowID: 2, UserID: 1, Status: model.TaskPending, Input: "b"}
	rig2.tasks.seed(tk2)
	if err := rig2.run(t, tk2.ID); err != nil {
		t.Fatalf("executeTask 2: %v", err)
	}

	// 两个工作流的锁各被获取一次
	if acq := lock.acquireCount(1); acq != 1 {
		t.Fatalf("wf 1 lock acquired %d times, want 1", acq)
	}
	if acq := lock.acquireCount(2); acq != 1 {
		t.Fatalf("wf 2 lock acquired %d times, want 1", acq)
	}
	// 两个工作流的锁各被释放一次
	if rel := lock.releaseCount(1); rel != 1 {
		t.Fatalf("wf 1 lock released %d times, want 1", rel)
	}
	if rel := lock.releaseCount(2); rel != 1 {
		t.Fatalf("wf 2 lock released %d times, want 1", rel)
	}
}

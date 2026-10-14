package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/queue"
)

// ---------- 锁粒度 ----------

// 默认（零值）必须是 task 粒度。
//
// 回归背景：锁最初按工作流加，把"防止同一任务被两个实例重复执行"退化成了
// "整条工作流串行"。同一二进制只改锁的受控压测（loadgen 1200 任务 / 20 worker /
// 单工作流）：按工作流加锁 8.9 tasks/s、p50 端到端 97.9s、227 个任务未完成；
// 不加锁 493.5 tasks/s、p50 1.31s、100% 落地。
func TestLockScopeDefaultIsTask(t *testing.T) {
	var s Scheduler
	if s.lockScope != LockScopeTask {
		t.Fatalf("zero-value scope should be LockScopeTask, got %v", s.lockScope)
	}
	if got := s.lockKeyFor(200, 1); got != 200 {
		t.Fatalf("task scope must key on task id, got %d", got)
	}
	s.SetLockScope(LockScopeWorkflow)
	if got := s.lockKeyFor(200, 1); got != 1 {
		t.Fatalf("workflow scope must key on workflow id, got %d", got)
	}
}

func TestParseLockScope(t *testing.T) {
	cases := map[string]LockScope{
		"":          LockScopeTask,
		"task":      LockScopeTask,
		" TASK ":    LockScopeTask,
		"workflow":  LockScopeWorkflow,
		"wf":        LockScopeWorkflow,
		"off":       LockScopeOff,
		"disabled":  LockScopeOff,
		"nonsense":  LockScopeTask, // 拼错时退回安全默认值
	}
	for in, want := range cases {
		got, ok := ParseLockScope(in)
		if got != want {
			t.Fatalf("ParseLockScope(%q) = %v, want %v", in, got, want)
		}
		if !ok && in != "nonsense" {
			t.Fatalf("ParseLockScope(%q) reported not-ok", in)
		}
	}
}

func linearRigWorkflow() *model.Workflow {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{{SourceNode: "in", TargetNode: "out"}}
	return wfOf(1, 1, nodes, edges)
}

// task 粒度下，占住"任务 300"的锁不能让同一工作流的"任务 301"被拒绝执行。
func TestTaskLockScopeDoesNotSerializeSameWorkflowTasks(t *testing.T) {
	rig := newTestRig(t, linearRigWorkflow())

	inner := NewMemoryWorkflowLock()
	held, err := inner.Acquire(context.Background(), 300) // 模拟"任务 300 正在别的实例上跑"
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer held()

	rig.sched.SetWorkflowLock(newTrackingLock(inner)) // 默认 task 粒度

	tk := &model.Task{ID: 301, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)
	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}

	// 装置自检：确认被占住的那个键确实是"另一个任务"，而不是本任务
	if tk.Status != model.TaskSucceeded {
		t.Fatalf("task 301 must not be blocked by task 300's lock (scope=task), got status %s", tk.Status)
	}
}

// workflow 粒度下（显式打开），占住工作流锁后同一工作流的任务必须被重投而不是执行。
func TestWorkflowLockScopeStillSerializes(t *testing.T) {
	rig := newTestRig(t, linearRigWorkflow())

	inner := NewMemoryWorkflowLock()
	held, err := inner.Acquire(context.Background(), 1) // 工作流 1 被占住
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer held()

	lock := newTrackingLock(inner)
	rig.sched.SetWorkflowLock(lock)
	rig.sched.SetLockScope(LockScopeWorkflow)

	tk := &model.Task{ID: 302, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)
	if err := rig.queue.Enqueue(context.Background(), queue.Job{TaskID: tk.ID}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 走真实的 consumeLoop，让消息真的进入 inflight，重投才有对象
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rig.sched.consumeLoop(ctx, "scope-test")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := rig.tasks.nodeStatus(tk.ID, "in"); st == model.NodeSucceeded {
			t.Fatal("task must not execute while its workflow lock is held (scope=workflow)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 消息必须被反复重投（回到队列），而不是被丢弃或进 DLQ。
	// 断言"锁被尝试了多次"而不是"队列此刻非空"：固定延迟期间消息停在
	// 延迟队列里，任何一个瞬间采样队列长度都会抖动。
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if lock.acquireCount(1) >= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("contended message must be requeued and retried, but the lock was attempted only %d time(s)",
		lock.acquireCount(1))
}

// 锁竞争必须归还 running 计数：原实现直接 finished = true 返回、从不调用
// ObserveTaskDone，每次竞争都让 gauge 永久 +1（Dashboard"运行中任务"只增不减）。
func TestLockContentionReturnsRunningGauge(t *testing.T) {
	rig := newTestRig(t, linearRigWorkflow())

	lock := newTrackingLock(NewMemoryWorkflowLock())
	lock.acquireErr = ErrWorkflowLocked
	rig.sched.SetWorkflowLock(lock)
	rig.sched.SetLockScope(LockScopeWorkflow)

	tk := &model.Task{ID: 303, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)
	if err := rig.queue.Enqueue(context.Background(), queue.Job{TaskID: tk.ID}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rig.sched.consumeLoop(ctx, "gauge-test")

	time.Sleep(300 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)

	if got := rig.metrics.RunningTasks.Load(); got != 0 {
		t.Fatalf("running gauge must be returned after lock requeue, got %d", got)
	}
}

// ---------- 审批（Human-in-the-loop） ----------

// 回归：审批挂起不是失败，绝不能把下游落库为 skipped。
//
// 原实现把 errApprovalWaiting 交给失败分支处理 → skipDownstream 把审批节点的全部
// 下游标成 skipped；审批通过后恢复逻辑把 skipped 当终态 → remaining 立刻归零 →
// 任务报"成功"而下游一个节点都没跑（有副作用的后续步骤被静默跳过）。
func TestApprovalPauseKeepsDownstreamPendingAndResumes(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "appr", NodeType: model.NodeApproval},
		{NodeKey: "llm", NodeType: model.NodeLLM, Config: model.NodeConfig{Model: "mock-chat"}},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "appr"},
		{SourceNode: "appr", TargetNode: "llm"},
		{SourceNode: "llm", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))

	tk := &model.Task{ID: 400, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "需要审批的输入"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if tk.Status != model.TaskWaiting {
		t.Fatalf("task should be waiting for approval, got %s", tk.Status)
	}
	// 装置自检：审批节点确实是 waiting 而不是 failed
	if st, _ := rig.tasks.nodeStatus(tk.ID, "appr"); st != model.NodeWaiting {
		t.Fatalf("approval node should be waiting (not failed), got %q", st)
	}
	// 核心断言：下游不得被标成 skipped
	for _, key := range []string{"llm", "out"} {
		if st, _ := rig.tasks.nodeStatus(tk.ID, key); st == model.NodeSkipped {
			t.Fatalf("downstream node %s was skipped while approval is pending (status=%q)", key, st)
		}
	}

	// 审批通过 → 任务重新入队 → 调度器必须真正跑下游
	if err := rig.sched.ApproveNode(context.Background(), tk.ID, "appr", true, "ok"); err != nil {
		t.Fatalf("ApproveNode: %v", err)
	}
	ctx := context.Background()
	job, msgID, err := rig.queue.Dequeue(ctx, "approval-test", 2*time.Second)
	if err != nil {
		t.Fatalf("dequeue resumed task: %v", err)
	}
	if err := rig.sched.executeTask(ctx, job, msgID); err != nil {
		t.Fatalf("executeTask (resumed): %v", err)
	}

	if tk.Status != model.TaskSucceeded {
		t.Fatalf("task should succeed after approval, got %s", tk.Status)
	}
	if st, _ := rig.tasks.nodeStatus(tk.ID, "llm"); st != model.NodeSucceeded {
		t.Fatalf("downstream llm node must actually run after approval, got %q", st)
	}
	// 装置自检：下游真的调用了 LLM（有 usage 记录），不是"跳过式成功"
	if len(rig.tasks.usage) == 0 {
		t.Fatal("resumed run must call the LLM node (expected usage records)")
	}
}

// 审批被拒绝：下游跳过，任务失败。
func TestApprovalRejectedSkipsDownstream(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "appr", NodeType: model.NodeApproval},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "appr"},
		{SourceNode: "appr", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))
	tk := &model.Task{ID: 401, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if err := rig.sched.ApproveNode(context.Background(), tk.ID, "appr", false, "不同意"); err != nil {
		t.Fatalf("ApproveNode(reject): %v", err)
	}
	if tk.Status != model.TaskFailed {
		t.Fatalf("rejected approval should fail the task, got %s", tk.Status)
	}
}

// ---------- 恢复（checkpoint）记账 ----------

// 回归：恢复时 remaining 被重复递减 → 主循环一次都没进，可运行节点永远不执行。
func TestRecoveryDoesNotDoubleCountRemaining(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "a", NodeType: model.NodeInput},
		{NodeKey: "b", NodeType: model.NodeLLM, Config: model.NodeConfig{Model: "mock-chat"}},
		{NodeKey: "c", NodeType: model.NodeOutput},
		{NodeKey: "d", NodeType: model.NodeInput},
		{NodeKey: "e", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "a", TargetNode: "b"},
		{SourceNode: "b", TargetNode: "c"},
		{SourceNode: "d", TargetNode: "e"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))
	tk := &model.Task{ID: 500, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	// 先跑一轮，让 task_nodes 建立起来
	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// 构造"上一轮留下的库状态"：分支 1 失败并被跳过，分支 2 因超时被取消（待重跑）
	rig.tasks.forceNode(tk.ID, "a", model.NodeInput, model.NodeFailed)
	rig.tasks.forceNode(tk.ID, "b", model.NodeLLM, model.NodeSkipped)
	rig.tasks.forceNode(tk.ID, "c", model.NodeOutput, model.NodeSkipped)
	rig.tasks.forceNode(tk.ID, "d", model.NodeInput, model.NodeCancelled)
	rig.tasks.forceNode(tk.ID, "e", model.NodeOutput, model.NodeCancelled)
	tk.Status = model.TaskPending

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	// 核心断言：被取消的分支必须被重新调度，而不是停在 cancelled
	if st, _ := rig.tasks.nodeStatus(tk.ID, "d"); st == model.NodeCancelled || st == "" {
		t.Fatalf("node d must be rescheduled on resume, got %q", st)
	}
	if st, _ := rig.tasks.nodeStatus(tk.ID, "e"); st != model.NodeSucceeded {
		t.Fatalf("node e must run to completion on resume, got %q", st)
	}
	// 任务仍然失败（分支 1 是确定性失败），但不能以"什么都没跑"的方式失败
	if tk.Status != model.TaskFailed {
		t.Fatalf("task should still be failed because branch a failed, got %s", tk.Status)
	}
}

// 回归：skipped 节点不递减后继入度 → 后继入度永不归零，白等到任务超时。
func TestRecoverySkippedNodeDoesNotHang(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "a", NodeType: model.NodeInput},
		{NodeKey: "b", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{{SourceNode: "a", TargetNode: "b"}}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))
	tk := &model.Task{ID: 501, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	rig.tasks.forceNode(tk.ID, "a", model.NodeInput, model.NodeSkipped)
	rig.tasks.forceNode(tk.ID, "b", model.NodeOutput, model.NodePending)
	tk.Status = model.TaskPending

	done := make(chan error, 1)
	go func() { done <- rig.run(t, tk.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resumed run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resume hung: a skipped node left its successor's in-degree above zero")
	}
}

// ---------- 引擎级错误 ----------

// errTaskStore 让 GetTaskByID 返回基础设施错误（DB 抖动）。
type errTaskStore struct {
	*fakeTaskStore
	err error
}

func (e *errTaskStore) GetTaskByID(context.Context, int64) (*model.Task, error) {
	return nil, e.err
}

// 回归：引擎级错误的消息必须回到队列。
//
// 原实现只打一行日志就 continue（注释说"交给 PEL 重投"，但运行期没有任何地方
// 会认领 PEL）→ 任务在本进程生命周期内永远不会再执行，而且它长期 pending 会把
// TrimConsumed 的安全位点钉死，队列再也回收不掉已确认前缀。
func TestEngineErrorRequeuesMessage(t *testing.T) {
	rig := newTestRig(t, linearRigWorkflow())
	rig.sched.tasks = &errTaskStore{fakeTaskStore: rig.tasks, err: errors.New("pg: connection refused")}

	rig.tasks.seed(&model.Task{ID: 600, WorkflowID: 1, UserID: 1, Status: model.TaskPending})
	if err := rig.queue.Enqueue(context.Background(), queue.Job{TaskID: 600}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rig.sched.consumeLoop(ctx, "engine-err-test")

	// 消息应在一轮退避（内存队列首次重投 100ms）后回到队列
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := rig.queue.Len(context.Background()); n > 0 {
			return // 已重投
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("message was neither acked nor requeued after an engine-level error (task would be lost)")
}

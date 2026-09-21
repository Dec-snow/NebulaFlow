package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/tool"
	"github.com/hoarfrost/nebulaflow/internal/worker"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// ---------- 内存 fake 存储 ----------

type fakeTaskStore struct {
	mu    sync.Mutex
	tasks map[int64]*model.Task
	nodes map[int64][]*model.TaskNode
	logs  []*model.TaskLog
	usage []*model.UsageRecord
}

func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{tasks: map[int64]*model.Task{}, nodes: map[int64][]*model.TaskNode{}}
}

func (f *fakeTaskStore) seed(t *model.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[t.ID] = t
}

func (f *fakeTaskStore) GetTaskByID(_ context.Context, id int64) (*model.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		// 必须返回与真实 store 相同的哨兵，否则调度器无法区分"任务已删除"与"DB 抖动"
		return nil, store.ErrTaskNotFound
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTaskStore) CountTaskNodes(_ context.Context, taskID int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.nodes[taskID]), nil
}

func (f *fakeTaskStore) CreateTaskNodes(_ context.Context, taskID int64, nodes []model.WorkflowNode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range nodes {
		f.nodes[taskID] = append(f.nodes[taskID], &model.TaskNode{
			TaskID: taskID, NodeKey: n.NodeKey, NodeType: n.NodeType, Status: model.NodePending,
		})
	}
	return nil
}

func (f *fakeTaskStore) UpdateTaskStatus(_ context.Context, id int64, status model.TaskStatus, output, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return errors.New("task not found")
	}
	t.Status, t.Output, t.Error = status, output, errMsg
	now := time.Now()
	if status == model.TaskRunning && t.StartedAt == nil {
		t.StartedAt = &now
	}
	if status == model.TaskSucceeded || status == model.TaskFailed || status == model.TaskCancelled {
		t.FinishedAt = &now
	}
	return nil
}

func (f *fakeTaskStore) UpdateTaskNode(_ context.Context, n *model.TaskNode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.nodes[n.TaskID]
	for _, existing := range list {
		if existing.NodeKey == n.NodeKey {
			existing.Status = n.Status
			existing.Input = n.Input
			existing.Output = n.Output
			existing.Error = n.Error
			existing.DurationMS = n.DurationMS
			existing.TokensIn = n.TokensIn
			existing.TokensOut = n.TokensOut
			return nil
		}
	}
	cp := *n
	f.nodes[n.TaskID] = append(list, &cp)
	return nil
}

func (f *fakeTaskStore) AppendLog(_ context.Context, l *model.TaskLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, l)
	return nil
}

func (f *fakeTaskStore) RecordUsage(_ context.Context, u *model.UsageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, u)
	return nil
}

func (f *fakeTaskStore) nodeStatus(taskID int64, key string) (model.TaskNodeStatus, *model.TaskNode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.nodes[taskID] {
		if n.NodeKey == key {
			return n.Status, n
		}
	}
	return "", nil
}

// pendingNodes 返回仍处于非终态（pending / running）的节点，用于验证收敛性。
func (f *fakeTaskStore) pendingNodes(taskID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, n := range f.nodes[taskID] {
		if n.Status == model.NodePending || n.Status == model.NodeRunning {
			out = append(out, n.NodeKey)
		}
	}
	return out
}

type fakeWorkflowStore struct{ wf *model.Workflow }

func (f *fakeWorkflowStore) GetWorkflow(_ context.Context, id, userID int64) (*model.Workflow, error) {
	if f.wf == nil || f.wf.ID != id || f.wf.UserID != userID {
		return nil, errors.New("workflow not found")
	}
	return f.wf, nil
}

// fakeKnowledgeStore 是 KnowledgeStore 的内存实现：固定语料 + 余弦排序。
//
// 语义必须与 store 的内存后端一致（相似度降序 + Top-K 截断），
// 否则调度器的 RAG 节点测试会跑在一个与生产不同的契约上。
type fakeKnowledgeStore struct {
	chunks []model.DocumentChunk
	gotK   int // 最后一次收到的 k，用于断言 Top-K 被原样透传
}

func (f *fakeKnowledgeStore) SearchChunks(_ context.Context, _ int64, queryVec []float64, k int) ([]model.DocumentChunk, error) {
	f.gotK = k
	if len(queryVec) == 0 || len(f.chunks) == 0 {
		return nil, nil
	}
	out := make([]model.DocumentChunk, 0, len(f.chunks))
	for _, c := range f.chunks {
		c.Score = llm.Cosine(queryVec, c.Embedding)
		c.Embedding = nil
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// ---------- 测试用工具 ----------

// barrierTool 只有在两个节点"真正同时执行"时才能通过：
// 若调度器串行执行并行分支，屏障会超时，节点失败 → 任务失败。
// 这比"比较耗时"的并发测试更确定，不会在高负载机器上误判。
type barrierTool struct {
	peers    int
	timeout  time.Duration
	arrived  chan struct{}
	once     sync.Once
	failFast bool
}

func (b *barrierTool) Name() string { return "barrier" }
func (b *barrierTool) Description() string {
	return "测试用：验证并行节点被真正并发执行"
}

func (b *barrierTool) Call(ctx context.Context, _ string) (string, error) {
	b.arrived <- struct{}{}
	deadline := time.After(b.timeout)
	for i := 0; i < b.peers; i++ {
		select {
		case <-b.arrived:
		case <-deadline:
			return "", errors.New("barrier timeout: nodes were not executed concurrently")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "barrier-passed", nil
}

// failTool 总是失败，用于验证失败传播。
type failTool struct{}

func (failTool) Name() string        { return "always_fail" }
func (failTool) Description() string { return "测试用：恒定失败" }
func (failTool) Call(context.Context, string) (string, error) {
	return "", errors.New("tool failed on purpose")
}

// blockTool 进入执行后阻塞直到 context 取消，用于验证取消链路。
type blockTool struct{ started chan struct{} }

func (b *blockTool) Name() string        { return "block" }
func (b *blockTool) Description() string { return "测试用：阻塞直到取消" }
func (b *blockTool) Call(ctx context.Context, _ string) (string, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", ctx.Err()
}

// ---------- 装配 ----------

type testRig struct {
	sched     *Scheduler
	tasks     *fakeTaskStore
	hub       *task.Hub
	queue     *queue.InMemoryQueue
	pool      *worker.WorkerPool
	knowledge *fakeKnowledgeStore
	cancel    context.CancelFunc
}

func newTestRig(t *testing.T, wf *model.Workflow, extraTools ...tool.Tool) *testRig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	ts := newFakeTaskStore()
	hub := task.NewHub()
	q := queue.NewInMemoryQueue(64)
	metrics := observability.NewMetrics(prometheus.NewRegistry())

	mock := llm.NewMockProvider("mock")
	mock.Latency = 5 * time.Millisecond
	gateway := llm.NewGateway([]llm.Provider{mock}, slog.Default(), 10*time.Second, nil)

	tools := append([]tool.Tool{tool.Calculator{}}, extraTools...)
	registry := tool.NewRegistry(tools...)

	ks := &fakeKnowledgeStore{}
	pool := worker.NewWorkerPool(ctx, 8, 64, nil, slog.Default(), metrics)
	sched := newScheduler(ts, &fakeWorkflowStore{wf: wf}, ks, q, pool,
		gateway, rag.NewService(llm.NewEmbeddingGateway(nil, llm.NewLocalHashEmbedder(0))),
		registry, hub, slog.Default(), metrics, 30*time.Second, 0)
	pool.SetExecutor(sched)
	pool.Start()
	t.Cleanup(func() {
		pool.Shutdown(5 * time.Second)
		cancel()
	})
	return &testRig{sched: sched, tasks: ts, hub: hub, queue: q, pool: pool, knowledge: ks, cancel: cancel}
}

func (r *testRig) run(t *testing.T, taskID int64) error {
	t.Helper()
	return r.sched.executeTask(context.Background(), queue.Job{TaskID: taskID}, "msg-1")
}

func wfOf(id, userID int64, nodes []model.WorkflowNode, edges []model.WorkflowEdge) *model.Workflow {
	return &model.Workflow{ID: id, UserID: userID, Name: "test", Status: model.WorkflowPublished,
		Nodes: nodes, Edges: edges}
}

// ---------- 用例 ----------

// 线性 DAG：input → llm → output，全部成功且最终输出来自 output 节点。
func TestExecuteTask_Linear(t *testing.T) {
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

	tk := &model.Task{ID: 100, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "帮我分析这份简历"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if tk.Status != model.TaskSucceeded {
		t.Fatalf("expected succeeded, got %s (err=%s)", tk.Status, tk.Error)
	}
	if !strings.Contains(tk.Output, "简历") {
		t.Fatalf("output should carry task input downstream, got %q", tk.Output)
	}
	for _, key := range []string{"in", "llm", "out"} {
		if st, _ := rig.tasks.nodeStatus(tk.ID, key); st != model.NodeSucceeded {
			t.Fatalf("node %s expected succeeded, got %q", key, st)
		}
	}
	// token 用量应被记录（LLM 节点）
	if len(rig.tasks.usage) == 0 {
		t.Fatal("expected usage record for llm node")
	}
}

// 并行 DAG：A → B, A → C, B/C → D。
// 用屏障工具证明 B 与 C 真的并发执行（串行会超时失败）。
func TestExecuteTask_ParallelBranches(t *testing.T) {
	barrier := &barrierTool{peers: 1, timeout: 3 * time.Second, arrived: make(chan struct{}, 4)}
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "b", NodeType: model.NodeTool, Config: model.NodeConfig{Tool: "barrier"}},
		{NodeKey: "c", NodeType: model.NodeTool, Config: model.NodeConfig{Tool: "barrier"}},
		{NodeKey: "d", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "b"},
		{SourceNode: "in", TargetNode: "c"},
		{SourceNode: "b", TargetNode: "d"},
		{SourceNode: "c", TargetNode: "d"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges), barrier)

	tk := &model.Task{ID: 101, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if tk.Status != model.TaskSucceeded {
		t.Fatalf("expected succeeded, got %s (err=%s)", tk.Status, tk.Error)
	}
	// D 有两个上游，输入应带来源标签，否则 LLM 无法区分两段上下文
	if _, d := rig.tasks.nodeStatus(tk.ID, "d"); d != nil {
		if !strings.Contains(d.Input, "【b】") || !strings.Contains(d.Input, "【c】") {
			t.Fatalf("multi-input should be labelled by upstream key, got %q", d.Input)
		}
	} else {
		t.Fatal("node d missing")
	}
}

// 失败传播：中间节点失败后，其全部下游必须被 skipped，而不是继续用空输入执行。
func TestExecuteTask_FailureSkipsDownstream(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "boom", NodeType: model.NodeTool, Config: model.NodeConfig{Tool: "always_fail"}},
		{NodeKey: "llm", NodeType: model.NodeLLM, Config: model.NodeConfig{Model: "mock-chat"}},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "boom"},
		{SourceNode: "boom", TargetNode: "llm"},
		{SourceNode: "llm", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges), failTool{})

	tk := &model.Task{ID: 102, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if tk.Status != model.TaskFailed {
		t.Fatalf("expected failed, got %s", tk.Status)
	}
	if st, _ := rig.tasks.nodeStatus(tk.ID, "boom"); st != model.NodeFailed {
		t.Fatalf("boom expected failed, got %q", st)
	}
	for _, key := range []string{"llm", "out"} {
		if st, _ := rig.tasks.nodeStatus(tk.ID, key); st != model.NodeSkipped {
			t.Fatalf("downstream node %s must be skipped, got %q", key, st)
		}
	}
	// 跳过的节点不应该产生任何 LLM 用量
	if len(rig.tasks.usage) != 0 {
		t.Fatalf("skipped nodes must not call LLM, got %d usage records", len(rig.tasks.usage))
	}
}

// 取消：用户取消后正在执行的节点必须被真正打断（context 生效），
// 而不是继续跑完——这是"只改数据库状态"方案做不到的。
func TestExecuteTask_CancellationInterruptsRunningNode(t *testing.T) {
	block := &blockTool{started: make(chan struct{}, 1)}
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "slow", NodeType: model.NodeTool, Config: model.NodeConfig{Tool: "block"}},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "slow"},
		{SourceNode: "slow", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges), block)

	tk := &model.Task{ID: 103, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	done := make(chan error, 1)
	go func() { done <- rig.run(t, tk.ID) }()

	select {
	case <-block.started:
	case <-time.After(3 * time.Second):
		t.Fatal("slow node never started")
	}

	if !rig.sched.Cancel(tk.ID) {
		t.Fatal("Cancel should report the task as locally running")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executeTask: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt the running node (context not propagated)")
	}
	if tk.Status != model.TaskCancelled {
		t.Fatalf("expected cancelled, got %s", tk.Status)
	}
	// 取消后不应把下游当成成功执行
	if st, _ := rig.tasks.nodeStatus(tk.ID, "out"); st == model.NodeSucceeded {
		t.Fatal("downstream must not succeed after cancellation")
	}
	// 取消是终态操作：所有节点都必须收敛，不能留下 pending/running 的僵尸节点
	// （正在执行的节点由 worker 自己写 cancelled，所以要轮询等它落库）
	deadline := time.Now().Add(3 * time.Second)
	for {
		pending := rig.tasks.pendingNodes(tk.ID)
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nodes stuck non-terminal after cancellation: %v", pending)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, _ := rig.tasks.nodeStatus(tk.ID, "out"); st != model.NodeCancelled {
		t.Fatalf("downstream node should be cancelled, got %q", st)
	}
}

// 任务已被删除（工作流级联删除）但队列里还留着消息：必须确认丢弃，
// 否则消息永远留在 PEL 里被反复重投，形成一条死消息死循环。
func TestExecuteTask_DeletedTaskIsAcked(t *testing.T) {
	rig := newTestRig(t, wfOf(1, 1, nil, nil))
	ctx := context.Background()

	// 注意：不 seed 任何任务，模拟"消息还在、任务行已被级联删除"
	if err := rig.sched.executeTask(ctx, queue.Job{TaskID: 999}, "msg-gone"); err != nil {
		t.Fatalf("deleted task must not surface as an engine error: %v", err)
	}
	if n, err := rig.queue.Len(ctx); err != nil || n != 0 {
		t.Fatalf("message for deleted task must be acked, queue len=%d err=%v", n, err)
	}
	// 入口处 +1 的 running 计数必须归还，否则每丢一条消息 gauge 就永久 +1
	if got := rig.sched.RunningTasks(); got != 0 {
		t.Fatalf("running counter leaked after dropping message: %d", got)
	}
}

// 非法 DAG（环）：任务直接失败并给出可读原因，不进入调度循环。
func TestExecuteTask_CycleFailsFast(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "a", NodeType: model.NodeInput},
		{NodeKey: "b", NodeType: model.NodeLLM},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "a", TargetNode: "b"},
		{SourceNode: "b", TargetNode: "a"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))
	tk := &model.Task{ID: 104, WorkflowID: 1, UserID: 1, Status: model.TaskPending}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask should not return error for handled failure: %v", err)
	}
	if tk.Status != model.TaskFailed {
		t.Fatalf("expected failed, got %s", tk.Status)
	}
	if !strings.Contains(tk.Error, "cycle") {
		t.Fatalf("expected cycle error, got %q", tk.Error)
	}
}

// 运行计数不能泄漏：无论任务成功还是异常退出，running 都必须回到 0。
func TestExecuteTask_RunningGaugeDoesNotLeak(t *testing.T) {
	nodes := []model.WorkflowNode{{NodeKey: "in", NodeType: model.NodeInput}}
	rig := newTestRig(t, wfOf(1, 1, nodes, nil))

	// 任务不存在（已被删除）：消息应被确认丢弃，既不报错也不计入异常退出
	if err := rig.sched.executeTask(context.Background(), queue.Job{TaskID: 999}, "msg"); err != nil {
		t.Fatalf("deleted task should be dropped silently, got %v", err)
	}
	if got := rig.sched.RunningTasks(); got != 0 {
		t.Fatalf("running counter leaked: %d", got)
	}

	tk := &model.Task{ID: 105, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)
	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if got := rig.sched.RunningTasks(); got != 0 {
		t.Fatalf("running counter leaked after success: %d", got)
	}
}

// 拓扑序必须稳定：同一 DAG 多次构建的 Order 完全一致（不依赖 map 迭代顺序）。
func TestBuildDAG_OrderIsDeterministic(t *testing.T) {
	build := func() []string {
		nodes := []model.WorkflowNode{
			{NodeKey: "d", NodeType: model.NodeOutput},
			{NodeKey: "a", NodeType: model.NodeInput},
			{NodeKey: "c", NodeType: model.NodeLLM},
			{NodeKey: "b", NodeType: model.NodeLLM},
		}
		edges := []model.WorkflowEdge{
			{SourceNode: "a", TargetNode: "b"},
			{SourceNode: "a", TargetNode: "c"},
			{SourceNode: "b", TargetNode: "d"},
			{SourceNode: "c", TargetNode: "d"},
		}
		dag, err := BuildDAG(nodes, edges)
		if err != nil {
			t.Fatalf("BuildDAG: %v", err)
		}
		return dag.Order
	}
	first := build()
	for i := 0; i < 50; i++ {
		got := build()
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("topological order not deterministic: %v vs %v", got, first)
		}
	}
	if strings.Join(first, ",") != "a,b,c,d" {
		t.Fatalf("unexpected order: %v", first)
	}
}

// 重复 node_key / 未知节点类型 / 重复边都必须在构建阶段被拒绝。
func TestBuildDAG_RejectsInvalidGraph(t *testing.T) {
	in := model.WorkflowNode{NodeKey: "a", NodeType: model.NodeInput}
	cases := []struct {
		name  string
		nodes []model.WorkflowNode
		edges []model.WorkflowEdge
		want  error
	}{
		{
			name:  "duplicate node key",
			nodes: []model.WorkflowNode{in, {NodeKey: "a", NodeType: model.NodeLLM}},
			want:  ErrDuplicateNode,
		},
		{
			name:  "unknown node type",
			nodes: []model.WorkflowNode{{NodeKey: "a", NodeType: model.NodeType("magic")}},
			want:  ErrInvalidNode,
		},
		{
			name:  "empty node key",
			nodes: []model.WorkflowNode{{NodeKey: "", NodeType: model.NodeInput}},
			want:  ErrInvalidNode,
		},
		{
			name:  "duplicate edge",
			nodes: []model.WorkflowNode{in, {NodeKey: "b", NodeType: model.NodeLLM}},
			edges: []model.WorkflowEdge{{SourceNode: "a", TargetNode: "b"}, {SourceNode: "a", TargetNode: "b"}},
			want:  ErrInvalidNode,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildDAG(tc.nodes, tc.edges)
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
}

// 取消一个不在本机执行的任务不应 panic，也不应误伤其他任务，
// 更不能在 cancelled 里留下永不清理的条目——这个 map 只在 executeTask 尾部清理，
// 而"还没出队就被取消"的任务压根不会走到那里。
func TestScheduler_CancelUnknownTask(t *testing.T) {
	nodes := []model.WorkflowNode{{NodeKey: "in", NodeType: model.NodeInput}}
	rig := newTestRig(t, wfOf(1, 1, nodes, nil))
	for i := 0; i < 100; i++ {
		if rig.sched.Cancel(424242) {
			t.Fatal("cancelling unknown task should report false")
		}
	}
	rig.sched.mu.Lock()
	leaked := len(rig.sched.cancelled)
	rig.sched.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("取消非本机任务后 cancelled 应保持为空，实际残留 %d 条", leaked)
	}
}

// 任务已被标记为 cancelled 时，出队后应直接确认并不执行。
func TestExecuteTask_AlreadyCancelled(t *testing.T) {
	nodes := []model.WorkflowNode{{NodeKey: "in", NodeType: model.NodeInput}}
	rig := newTestRig(t, wfOf(1, 1, nodes, nil))
	tk := &model.Task{ID: 106, WorkflowID: 1, UserID: 1, Status: model.TaskCancelled}
	rig.tasks.seed(tk)
	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if st, _ := rig.tasks.nodeStatus(tk.ID, "in"); st == model.NodeSucceeded {
		t.Fatal("cancelled task must not execute nodes")
	}
}

// 节点执行 panic 不应把 worker 打死：后续节点仍能被执行。
func TestExecuteTask_NodePanicDoesNotKillWorker(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "p", NodeType: model.NodeTool, Config: model.NodeConfig{Tool: "barrier"}},
	}
	edges := []model.WorkflowEdge{{SourceNode: "in", TargetNode: "p"}}
	barrier := &barrierTool{peers: 0, timeout: time.Second, arrived: make(chan struct{}, 2)}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges), barrier)

	tk := &model.Task{ID: 107, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)
	_ = rig.run(t, tk.ID)

	// 再跑一个任务，确认 worker 池仍然可用
	tk2 := &model.Task{ID: 108, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "y"}
	rig.tasks.seed(tk2)
	if err := rig.run(t, tk2.ID); err != nil {
		t.Fatalf("second task failed: %v", err)
	}
	if tk2.Status != model.TaskSucceeded {
		t.Fatalf("worker pool should still work, got %s (%s)", tk2.Status, tk2.Error)
	}
}

// ---------- RAG 节点（P0-1 改造后的检索链路） ----------

// RAG 节点应把 query 交给 rag.Service 向量化，再由存储层做 Top-K 检索。
// 关键断言是 k 被原样下推为 5——这证明调度器不再自己持有候选集。
func TestExecuteTask_RAGNodeRetrievesTopK(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "rag", NodeType: model.NodeRAG, Config: model.NodeConfig{KnowledgeBaseID: 7}},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "rag"},
		{SourceNode: "rag", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))

	// 语料：与查询向量最接近的一条应排在最前
	rig.knowledge.chunks = []model.DocumentChunk{
		{ID: 1, DocumentID: 11, ChunkIndex: 0, Content: "Redis Stream 支持消费者组与崩溃恢复", Filename: "queue.md",
			Embedding: []float64{1, 0, 0}},
		{ID: 2, DocumentID: 12, ChunkIndex: 0, Content: "PostgreSQL 存储用户与工作流", Filename: "db.md",
			Embedding: []float64{0, 1, 0}},
	}

	tk := &model.Task{ID: 500, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "消费者组"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if tk.Status != model.TaskSucceeded {
		t.Fatalf("expected succeeded, got %s (err=%s)", tk.Status, tk.Error)
	}
	if rig.knowledge.gotK != 5 {
		t.Fatalf("调度器应以 k=5 检索，实际 k=%d", rig.knowledge.gotK)
	}
	if !strings.Contains(tk.Output, "Redis Stream") {
		t.Fatalf("输出应包含检索到的资料，实际 %q", tk.Output)
	}
	// 输出里的 [检索命中] 摘要要带上文档与分块号，便于前端定位引用来源
	if !strings.Contains(tk.Output, "doc#11") {
		t.Fatalf("输出应标注命中来源，实际 %q", tk.Output)
	}
}

// RAG 节点未配置 knowledge_base_id 时必须失败，而不是静默返回空上下文。
func TestExecuteTask_RAGNodeRequiresKB(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "rag", NodeType: model.NodeRAG}, // 故意不配 KnowledgeBaseID
	}
	edges := []model.WorkflowEdge{{SourceNode: "in", TargetNode: "rag"}}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges))

	tk := &model.Task{ID: 501, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if tk.Status != model.TaskFailed {
		t.Fatalf("未配置知识库应失败，实际 %s", tk.Status)
	}
	if !strings.Contains(tk.Error, "knowledge_base_id") {
		t.Fatalf("错误信息应指出缺失的配置项，实际 %q", tk.Error)
	}
}

// 知识库为空（检索无命中）时应给出明确提示，且任务仍然成功——
// 空知识库是合法的业务状态，不该被当成执行失败。
func TestExecuteTask_RAGNodeEmptyKnowledgeBase(t *testing.T) {
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "rag", NodeType: model.NodeRAG, Config: model.NodeConfig{KnowledgeBaseID: 7}},
	}
	edges := []model.WorkflowEdge{{SourceNode: "in", TargetNode: "rag"}}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges)) // knowledge.chunks 为空

	tk := &model.Task{ID: 502, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "x"}
	rig.tasks.seed(tk)

	if err := rig.run(t, tk.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if tk.Status != model.TaskSucceeded {
		t.Fatalf("空知识库不应导致任务失败，实际 %s (err=%s)", tk.Status, tk.Error)
	}
	if !strings.Contains(tk.Output, "未检索到相关内容") {
		t.Fatalf("应提示未检索到内容，实际 %q", tk.Output)
	}
}

// ---------- P0-0：消费循环并发化 ----------

// gateTool 是一个 N 路汇合闸门：必须凑够 target 个并发执行体抵达才一起放行。
//
// 用它来证明"任务级并发"是确定性可行的，而不是靠计时碰运气：
// 如果消费循环还是串行的（一次只跑一个 DAG），第一个任务会独自卡在闸门里，
// 等不到同伴，必然超时失败——测试直接红掉，不存在"偶尔通过"。
type gateTool struct {
	target  int
	timeout time.Duration

	mu      sync.Mutex
	n       int
	release chan struct{}
	once    sync.Once
}

func (g *gateTool) Name() string { return "gate" }
func (g *gateTool) Description() string {
	return "测试用：N 路汇合闸门，证明任务被真正并发消费"
}

func (g *gateTool) Call(ctx context.Context, _ string) (string, error) {
	g.mu.Lock()
	g.n++
	reached := g.n
	if reached >= g.target {
		g.once.Do(func() { close(g.release) })
	}
	g.mu.Unlock()

	select {
	case <-g.release:
		return "gate-passed", nil
	case <-time.After(g.timeout):
		return "", fmt.Errorf("gate timeout: 只有 %d/%d 个任务并发抵达，消费循环疑似仍在串行",
			reached, g.target)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// countTool 统计每个输入被执行的次数，用于验证多消费者不会重复消费同一条消息。
type countTool struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *countTool) Name() string        { return "count" }
func (c *countTool) Description() string { return "测试用：统计执行次数" }
func (c *countTool) Call(_ context.Context, in string) (string, error) {
	c.mu.Lock()
	c.n[in]++
	c.mu.Unlock()
	return in, nil
}
func (c *countTool) hits(in string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[in]
}

// waitStatus 轮询任务终态。必须走 GetTaskByID（内部加锁并返回副本），
// 直接读 seed 进去的那个指针会和调度器 goroutine 形成数据竞争。
func (r *testRig) waitStatus(t *testing.T, id int64, want model.TaskStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last model.TaskStatus
	for time.Now().Before(deadline) {
		if tk, err := r.tasks.GetTaskByID(context.Background(), id); err == nil && tk != nil {
			last = tk.Status
			if tk.Status == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %d 在 %s 内没有到达 %s（当前 %s）", id, timeout, want, last)
}

// 消费并发度的解析顺序：显式设置 > 跟随 WorkerPool > 兜底 1。
func TestScheduler_ConsumerCountResolution(t *testing.T) {
	nodes := []model.WorkflowNode{{NodeKey: "in", NodeType: model.NodeInput}}
	rig := newTestRig(t, wfOf(1, 1, nodes, nil))

	if got, want := rig.sched.consumerCount(), rig.pool.Workers(); got != want {
		t.Fatalf("未显式设置时应跟随 WorkerPool(%d)，实际 %d", want, got)
	}
	rig.sched.SetConsumers(3)
	if got := rig.sched.consumerCount(); got != 3 {
		t.Fatalf("显式设置应优先生效，实际 %d", got)
	}
	// 非正数不能真的起 0 个消费者（那等于调度器静默停摆），必须回退
	rig.sched.SetConsumers(-5)
	if got, want := rig.sched.consumerCount(), rig.pool.Workers(); got != want {
		t.Fatalf("非正数应回退到 WorkerPool(%d)，实际 %d", want, got)
	}
}

// Run 必须并发消费：4 个任务同时卡在闸门上，任何一个等不到同伴就会失败。
// 这是 P0-0 的回归测试——修复前 Run 是单 goroutine 串行消费，此用例必红。
func TestRun_ConsumersExecuteTasksConcurrently(t *testing.T) {
	const n = 4

	gate := &gateTool{target: n, timeout: 5 * time.Second, release: make(chan struct{})}
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "gate", NodeType: model.NodeTool, Config: model.NodeConfig{Tool: "gate"}},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "gate"},
		{SourceNode: "gate", TargetNode: "out"},
	}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges), gate)
	rig.sched.SetConsumers(n)

	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id := int64(300 + i)
		ids = append(ids, id)
		rig.tasks.seed(&model.Task{
			ID: id, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: fmt.Sprint("in-", i),
		})
		if err := rig.queue.Enqueue(context.Background(), queue.Job{TaskID: id}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); rig.sched.Run(runCtx) }()

	for _, id := range ids {
		rig.waitStatus(t, id, model.TaskSucceeded, 15*time.Second)
	}

	// Run 必须在 ctx 取消后及时退出，否则优雅关闭会挂住
	stopRun()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Run 没有退出")
	}
}

// 多消费者是"分工"不是"重复劳动"：每条消息只能被一个消费者取走。
func TestRun_ConsumersDoNotDuplicateMessages(t *testing.T) {
	const n = 6

	counter := &countTool{n: map[string]int{}}
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "c", NodeType: model.NodeTool, Config: model.NodeConfig{Tool: "count"}},
	}
	edges := []model.WorkflowEdge{{SourceNode: "in", TargetNode: "c"}}
	rig := newTestRig(t, wfOf(1, 1, nodes, edges), counter)
	rig.sched.SetConsumers(n)

	inputs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := int64(400 + i)
		in := fmt.Sprint("payload-", i)
		inputs = append(inputs, in)
		rig.tasks.seed(&model.Task{
			ID: id, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: in,
		})
		if err := rig.queue.Enqueue(context.Background(), queue.Job{TaskID: id}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	runDone := make(chan struct{})
	go func() { defer close(runDone); rig.sched.Run(runCtx) }()

	for i := 0; i < n; i++ {
		rig.waitStatus(t, int64(400+i), model.TaskSucceeded, 15*time.Second)
	}

	for _, in := range inputs {
		if got := counter.hits(in); got != 1 {
			t.Fatalf("输入 %q 被执行了 %d 次，期望恰好 1 次（消息被重复消费）", in, got)
		}
	}

	stopRun()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Run 没有退出")
	}
}

// ---------- 队列水位维护（P0-2） ----------
//
// Redis Stream 的 XACK 只把消息移出 PEL、不删消息本体，因此队列需要两件事：
//  1. 周期性回收「已投递且已确认」的前缀（queue.StreamStats.TrimConsumed）；
//  2. 把物理长度与容量上界一起上报，让 MAXLEN 触发的裁剪可被告警捕捉——
//     裁剪丢的是尚未投递的消息，Redis 侧不留痕迹，不上报就完全查不出来。
//
// 下面两个用例分别覆盖「一轮维护的逻辑」和「Run 真的把它接上了」。

// fakeStreamStatsQueue 在内存队列之上补出 StreamStats，
// 真实实现见 queue.RedisStreamQueue（其回收逻辑由 queue 包的集成测试覆盖）。
type fakeStreamStatsQueue struct {
	*queue.InMemoryQueue
	streamLen, maxLen, dlqLen int64
	admitLimit                int64
	trimmed                   int64
	trimErr                   error

	mu        sync.Mutex
	trimCalls int
}

func (f *fakeStreamStatsQueue) StreamLen(context.Context) (int64, error) { return f.streamLen, nil }
func (f *fakeStreamStatsQueue) DLQLen(context.Context) (int64, error)    { return f.dlqLen, nil }
func (f *fakeStreamStatsQueue) MaxLen() int64                            { return f.maxLen }
func (f *fakeStreamStatsQueue) AdmitLimit() int64                        { return f.admitLimit }

func (f *fakeStreamStatsQueue) TrimConsumed(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trimCalls++
	return f.trimmed, f.trimErr
}

func (f *fakeStreamStatsQueue) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trimCalls
}

func TestSchedulerMaintainQueueOnceReclaimsAndReports(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	fq := &fakeStreamStatsQueue{
		InMemoryQueue: queue.NewInMemoryQueue(8),
		streamLen:     8500, maxLen: 10000, dlqLen: 7, admitLimit: 8000, trimmed: 120,
	}
	// 前三个参数（存储依赖）不参与队列维护，传 nil 即可
	s := newScheduler(nil, nil, nil, fq, nil, nil, nil, nil, nil,
		slog.Default(), m, time.Minute, 0)

	d, err := s.maintainQueueOnce(context.Background(), fq)
	if err != nil {
		t.Fatalf("maintainQueueOnce: %v", err)
	}
	if fq.calls() != 1 {
		t.Fatalf("应触发 1 次回收，实际 %d 次", fq.calls())
	}
	if d.reclaimed != 120 || d.streamLen != 8500 || d.maxLen != 10000 || d.dlqLen != 7 ||
		d.admitLimit != 8000 {
		t.Fatalf("观测值不对: %+v", d)
	}
	// 8500/10000 = 85% 已越过 90% 告警线？没有——准入线在 80%，告警线在 90%，
	// 85% 属于"准入已在拦截、但护栏还没到危险区"，这一档不该告警。
	if d.saturated() {
		t.Fatalf("8500/10000 = 85%% 未到 90%% 告警线，saturated 应为 false")
	}

	// 指标必须成对上报：只报长度的话，告警规则无从判断"多长算长"
	if got := testutil.ToFloat64(m.QueueStreamLen); got != 8500 {
		t.Fatalf("queue_stream_len 应为 8500，实际 %v", got)
	}
	if got := testutil.ToFloat64(m.QueueMaxLen); got != 10000 {
		t.Fatalf("queue_max_len 应为 10000，实际 %v", got)
	}
	if got := testutil.ToFloat64(m.QueueDLQLen); got != 7 {
		t.Fatalf("queue_dlq_len 应为 7，实际 %v", got)
	}
	if got := testutil.ToFloat64(m.QueueReclaimedTotal); got != 120 {
		t.Fatalf("reclaimed_total 应为 120，实际 %v", got)
	}
	if got := testutil.ToFloat64(m.QueueAdmitLimit); got != 8000 {
		t.Fatalf("queue_admit_limit 应为 8000，实际 %v", got)
	}
}

// 水位告警线：达到 MaxLen 的 90% 才响。它比准入线（80%）高，语义是"准入失效了"——
// 准入正常工作时水位应该稳定在 80% 附近，根本到不了这里。
func TestSchedulerQueueDepthSaturation(t *testing.T) {
	cases := []struct {
		name      string
		streamLen int64
		maxLen    int64
		want      bool
	}{
		{"准入线附近不告警", 8500, 10000, false},
		{"略低于告警线", 8999, 10000, false},
		{"正好 90%", 9000, 10000, true},
		{"打满", 10000, 10000, true},
		{"未配置上界时不告警", 1_000_000, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := queueDepth{streamLen: tc.streamLen, maxLen: tc.maxLen}
			if got := d.saturated(); got != tc.want {
				t.Fatalf("streamLen=%d maxLen=%d 期望 saturated=%v，实际 %v",
					tc.streamLen, tc.maxLen, tc.want, got)
			}
		})
	}
}

// Run 必须真的把维护循环接上——只写一个没人调用的方法等于没修。
func TestSchedulerRunStartsQueueMaintenance(t *testing.T) {
	old := queueMaintainInterval
	queueMaintainInterval = 5 * time.Millisecond
	defer func() { queueMaintainInterval = old }()

	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	fq := &fakeStreamStatsQueue{
		InMemoryQueue: queue.NewInMemoryQueue(8),
		streamLen:     100, maxLen: 1000,
	}
	s := newScheduler(nil, nil, nil, fq, nil, nil, nil, nil, nil,
		slog.Default(), m, time.Minute, 0)
	s.SetConsumers(1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fq.calls() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run 在 ctx 取消后没有退出")
	}

	if fq.calls() == 0 {
		t.Fatal("Run 没有启动队列维护循环（回收不会被触发）")
	}
	if got := testutil.ToFloat64(m.QueueStreamLen); got != 100 {
		t.Fatalf("水位指标未上报，queue_stream_len=%v", got)
	}
}

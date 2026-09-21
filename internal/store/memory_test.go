package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

func newMem(t *testing.T) (*MemoryStore, context.Context) {
	t.Helper()
	return NewMemory(), context.Background()
}

func mkUser(t *testing.T, m *MemoryStore, username string) *model.User {
	t.Helper()
	u := &model.User{Username: username, Email: username + "@x.com", PasswordHash: "h"}
	if err := m.Users.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	return u
}

// ---------- 用户 ----------

func TestMemoryUser_UniqueViolation(t *testing.T) {
	m, ctx := newMem(t)
	mkUser(t, m, "alice")

	// 同 username 重复注册 → ErrUserExists（对齐 PostgreSQL 23505）
	if err := m.Users.CreateUser(ctx, &model.User{Username: "alice", Email: "other@x.com"}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate username: want ErrUserExists, got %v", err)
	}
	// 同 email 重复注册 → ErrUserExists
	if err := m.Users.CreateUser(ctx, &model.User{Username: "bob", Email: "alice@x.com"}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate email: want ErrUserExists, got %v", err)
	}
}

func TestMemoryUser_NotFound(t *testing.T) {
	m, ctx := newMem(t)
	if _, err := m.Users.GetUserByUsername(ctx, "nobody"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("want ErrUserNotFound, got %v", err)
	}
	if _, err := m.Users.GetUserByID(ctx, 999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("want ErrUserNotFound, got %v", err)
	}
}

// 内存实现必须模拟数据库的值语义：调用方改写读出的对象，不能污染库内数据。
func TestMemoryUser_ReadIsCopy(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	got, err := m.Users.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Username = "hacked"

	again, _ := m.Users.GetUserByID(ctx, u.ID)
	if again.Username != "alice" {
		t.Fatalf("mutating a read result leaked into the store: got %q", again.Username)
	}
}

// ---------- 工作流 ----------

func sampleWorkflow(userID int64) *model.Workflow {
	return &model.Workflow{
		UserID: userID,
		Name:   "wf",
		Status: model.WorkflowDraft,
		Nodes: []model.WorkflowNode{
			{NodeKey: "a", NodeType: model.NodeInput},
			{NodeKey: "b", NodeType: model.NodeLLM, Config: model.NodeConfig{System: "你是分析师"}},
		},
		Edges: []model.WorkflowEdge{{SourceNode: "a", TargetNode: "b"}},
	}
}

func TestMemoryWorkflow_CreateAndGet(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	wf := sampleWorkflow(u.ID)
	if err := m.Workflows.CreateWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}

	got, err := m.Workflows.GetWorkflow(ctx, wf.ID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 2 || len(got.Edges) != 1 {
		t.Fatalf("want 2 nodes + 1 edge, got %d + %d", len(got.Nodes), len(got.Edges))
	}
	// 节点 ID/WorkflowID 必须被回填（对齐 PostgreSQL 的插入行为）
	for _, n := range got.Nodes {
		if n.ID == 0 {
			t.Fatalf("node %q has zero ID", n.NodeKey)
		}
		if n.WorkflowID != wf.ID {
			t.Fatalf("node %q workflow_id = %d, want %d", n.NodeKey, n.WorkflowID, wf.ID)
		}
	}
	// 关键回归：system 等非空配置不能被清空（历史 bug #16）
	var b *model.WorkflowNode
	for i := range got.Nodes {
		if got.Nodes[i].NodeKey == "b" {
			b = &got.Nodes[i]
		}
	}
	if b == nil || b.Config.System != "你是分析师" {
		t.Fatalf("node config lost: %+v", b)
	}
}

func TestMemoryWorkflow_OwnershipIsolation(t *testing.T) {
	m, ctx := newMem(t)
	alice := mkUser(t, m, "alice")
	bob := mkUser(t, m, "bob")

	wf := sampleWorkflow(alice.ID)
	if err := m.Workflows.CreateWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}

	// bob 读 alice 的工作流 → ErrWorkflowNotFound（不泄露存在性）
	if _, err := m.Workflows.GetWorkflow(ctx, wf.ID, bob.ID); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("cross-user read: want ErrWorkflowNotFound, got %v", err)
	}
	if err := m.Workflows.DeleteWorkflow(ctx, wf.ID, bob.ID); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("cross-user delete: want ErrWorkflowNotFound, got %v", err)
	}
	// 列表也隔离
	list, _ := m.Workflows.ListWorkflows(ctx, bob.ID)
	if len(list) != 0 {
		t.Fatalf("bob should see 0 workflows, got %d", len(list))
	}
	list, _ = m.Workflows.ListWorkflows(ctx, alice.ID)
	if len(list) != 1 {
		t.Fatalf("alice should see 1 workflow, got %d", len(list))
	}
}

func TestMemoryWorkflow_UpdateReplacesNodes(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	wf := sampleWorkflow(u.ID)
	if err := m.Workflows.CreateWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}

	// 替换成单节点（先删后插语义）
	wf.Nodes = []model.WorkflowNode{{NodeKey: "x", NodeType: model.NodeOutput}}
	wf.Edges = nil
	wf.Name = "renamed"
	if err := m.Workflows.UpdateWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}

	got, _ := m.Workflows.GetWorkflow(ctx, wf.ID, u.ID)
	if got.Name != "renamed" {
		t.Fatalf("name not updated: %q", got.Name)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].NodeKey != "x" {
		t.Fatalf("nodes not replaced: %+v", got.Nodes)
	}
	if len(got.Edges) != 0 {
		t.Fatalf("edges not cleared: %+v", got.Edges)
	}
}

// ---------- 任务 ----------

func TestMemoryTask_StatusTimestamps(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")
	tk := &model.Task{WorkflowID: 1, UserID: u.ID, Input: "hi"}
	if err := m.Tasks.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}

	// running：写入 started_at，finished_at 仍为空
	if err := m.Tasks.UpdateTaskStatus(ctx, tk.ID, model.TaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	cur, _ := m.Tasks.GetTask(ctx, tk.ID, u.ID)
	if cur.StartedAt == nil {
		t.Fatal("started_at should be set on running")
	}
	if cur.FinishedAt != nil {
		t.Fatal("finished_at should be nil while running")
	}
	firstStart := *cur.StartedAt

	// 再次 running 不应覆盖 started_at（COALESCE 语义）
	time.Sleep(2 * time.Millisecond)
	if err := m.Tasks.UpdateTaskStatus(ctx, tk.ID, model.TaskRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	cur, _ = m.Tasks.GetTask(ctx, tk.ID, u.ID)
	if !cur.StartedAt.Equal(firstStart) {
		t.Fatalf("started_at was overwritten: %v -> %v", firstStart, *cur.StartedAt)
	}

	// 终态：写 finished_at
	if err := m.Tasks.UpdateTaskStatus(ctx, tk.ID, model.TaskSucceeded, "out", ""); err != nil {
		t.Fatal(err)
	}
	cur, _ = m.Tasks.GetTask(ctx, tk.ID, u.ID)
	if cur.FinishedAt == nil {
		t.Fatal("finished_at should be set on terminal status")
	}
	if cur.Output != "out" {
		t.Fatalf("output = %q, want %q", cur.Output, "out")
	}
}

func TestMemoryTask_ListPagination(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	for i := 0; i < 5; i++ {
		if err := m.Tasks.CreateTask(ctx, &model.Task{WorkflowID: 1, UserID: u.ID, Input: "t"}); err != nil {
			t.Fatal(err)
		}
	}

	// 默认 limit=20，按 id DESC
	list, err := m.Tasks.ListTasks(ctx, u.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("want 5 tasks, got %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID <= list[i].ID {
			t.Fatalf("tasks not ordered by id DESC: %d then %d", list[i-1].ID, list[i].ID)
		}
	}

	// limit=2 offset=1
	page, err := m.Tasks.ListTasks(ctx, u.ID, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("want 2 tasks on page, got %d", len(page))
	}
	if page[0].ID != list[1].ID || page[1].ID != list[2].ID {
		t.Fatalf("pagination mismatch: got %d,%d", page[0].ID, page[1].ID)
	}

	// offset 越界 → 空切片而非 error
	empty, err := m.Tasks.ListTasks(ctx, u.ID, 10, 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("want empty page, got %d", len(empty))
	}
}

// 重投幂等：调度器靠 CountTaskNodes 判断是否需要初始化节点。
func TestMemoryTaskNodes_IdempotentCreate(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")
	tk := &model.Task{WorkflowID: 1, UserID: u.ID}
	_ = m.Tasks.CreateTask(ctx, tk)

	nodes := []model.WorkflowNode{
		{ID: 11, NodeKey: "a", NodeType: model.NodeInput},
		{ID: 12, NodeKey: "b", NodeType: model.NodeLLM},
	}

	if n, _ := m.Tasks.CountTaskNodes(ctx, tk.ID); n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
	if err := m.Tasks.CreateTaskNodes(ctx, tk.ID, nodes); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Tasks.CountTaskNodes(ctx, tk.ID); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	// 空切片是 no-op
	if err := m.Tasks.CreateTaskNodes(ctx, tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Tasks.CountTaskNodes(ctx, tk.ID); n != 2 {
		t.Fatalf("count after empty insert = %d, want 2", n)
	}
}

func TestMemoryTaskNode_UpdateByNodeKey(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")
	tk := &model.Task{WorkflowID: 1, UserID: u.ID}
	_ = m.Tasks.CreateTask(ctx, tk)
	_ = m.Tasks.CreateTaskNodes(ctx, tk.ID, []model.WorkflowNode{{ID: 1, NodeKey: "a", NodeType: model.NodeLLM}})

	// 调度器 ExecuteNode 的真实顺序：先 publishNode(running)，再 publishNode(终态)
	if err := m.Tasks.UpdateTaskNode(ctx, &model.TaskNode{
		TaskID: tk.ID, NodeKey: "a", Status: model.NodeRunning,
	}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := m.Tasks.GetTaskNodes(ctx, tk.ID)
	started := nodes[0].StartedAt
	if started == nil {
		t.Fatal("started_at should be set after running")
	}
	if nodes[0].FinishedAt != nil {
		t.Fatal("finished_at should be nil while running")
	}

	// 更新终态字段
	if err := m.Tasks.UpdateTaskNode(ctx, &model.TaskNode{
		TaskID: tk.ID, NodeKey: "a", Status: model.NodeSucceeded,
		Output: "result", TokensIn: 7, TokensOut: 9, DurationMS: 42, Retries: 1,
	}); err != nil {
		t.Fatal(err)
	}

	nodes, _ = m.Tasks.GetTaskNodes(ctx, tk.ID)
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(nodes))
	}
	n := nodes[0]
	if n.Status != model.NodeSucceeded || n.Output != "result" {
		t.Fatalf("status/output not updated: %+v", n)
	}
	if n.TokensIn != 7 || n.TokensOut != 9 || n.DurationMS != 42 || n.Retries != 1 {
		t.Fatalf("metrics not persisted: %+v", n)
	}
	if n.FinishedAt == nil {
		t.Fatal("finished_at should be set on terminal status")
	}
	// started_at 必须保留最初 running 时的时间，不被终态覆盖
	if n.StartedAt == nil || !n.StartedAt.Equal(*started) {
		t.Fatalf("started_at changed on terminal update: %v -> %v", started, n.StartedAt)
	}

	// 不存在的 node_key → 报错（对齐 PostgreSQL 的 RowsAffected 语义）
	if err := m.Tasks.UpdateTaskNode(ctx, &model.TaskNode{TaskID: tk.ID, NodeKey: "ghost"}); err == nil {
		t.Fatal("updating a nonexistent node_key should fail")
	}
}

// COALESCE 语义保真：节点未经 running 直接进终态时，started_at 保持 NULL。
// 这是 PostgreSQL 版 SQL 的真实行为，内存实现必须一致（不能"补"一个值），
// 否则两种后端在边界场景下会给出不同结果。
func TestMemoryTaskNode_TerminalWithoutRunningLeavesStartedAtNull(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")
	tk := &model.Task{WorkflowID: 1, UserID: u.ID}
	_ = m.Tasks.CreateTask(ctx, tk)
	_ = m.Tasks.CreateTaskNodes(ctx, tk.ID, []model.WorkflowNode{{ID: 1, NodeKey: "a", NodeType: model.NodeLLM}})

	if err := m.Tasks.UpdateTaskNode(ctx, &model.TaskNode{
		TaskID: tk.ID, NodeKey: "a", Status: model.NodeFailed, Error: "boom",
	}); err != nil {
		t.Fatal(err)
	}

	nodes, _ := m.Tasks.GetTaskNodes(ctx, tk.ID)
	if nodes[0].StartedAt != nil {
		t.Fatalf("started_at should stay NULL per COALESCE semantics, got %v", nodes[0].StartedAt)
	}
	if nodes[0].FinishedAt == nil {
		t.Fatal("finished_at should be set on failed")
	}
}

// 取消链路：NodeCancelled 也必须写 finished_at（否则前端看到节点悬空）。
func TestMemoryTaskNode_CancelledClosesNode(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")
	tk := &model.Task{WorkflowID: 1, UserID: u.ID}
	_ = m.Tasks.CreateTask(ctx, tk)
	_ = m.Tasks.CreateTaskNodes(ctx, tk.ID, []model.WorkflowNode{{ID: 1, NodeKey: "slow", NodeType: model.NodeLLM}})

	if err := m.Tasks.UpdateTaskNode(ctx, &model.TaskNode{
		TaskID: tk.ID, NodeKey: "slow", Status: model.NodeCancelled,
	}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := m.Tasks.GetTaskNodes(ctx, tk.ID)
	if nodes[0].Status != model.NodeCancelled {
		t.Fatalf("status = %q, want cancelled", nodes[0].Status)
	}
	if nodes[0].FinishedAt == nil {
		t.Fatal("cancelled node must have finished_at")
	}
}

// ---------- 日志 ----------

// 语义对齐：取最近 limit 条，按时间正序返回。
func TestMemoryLogs_TakeLatestThenOrderAsc(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")
	tk := &model.Task{WorkflowID: 1, UserID: u.ID}
	_ = m.Tasks.CreateTask(ctx, tk)

	for i := 0; i < 5; i++ {
		if err := m.Tasks.AppendLog(ctx, &model.TaskLog{
			TaskID: tk.ID, Level: "info", Message: string(rune('a' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	logs, err := m.Tasks.ListLogs(ctx, tk.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Fatalf("want 3 logs, got %d", len(logs))
	}
	// 最近 3 条是 c,d,e，正序返回
	want := []string{"c", "d", "e"}
	for i, w := range want {
		if logs[i].Message != w {
			t.Fatalf("log[%d] = %q, want %q", i, logs[i].Message, w)
		}
	}
	if logs[0].ID >= logs[1].ID {
		t.Fatalf("logs should be ascending by id: %d, %d", logs[0].ID, logs[1].ID)
	}
}

// ---------- 用量聚合 ----------

func TestMemoryUsageSummary(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	recs := []model.UsageRecord{
		{UserID: u.ID, TaskID: 1, Provider: "deepseek", InputTokens: 100, OutputTokens: 50},
		{UserID: u.ID, TaskID: 1, Provider: "deepseek", InputTokens: 20, OutputTokens: 10},
		{UserID: u.ID, TaskID: 2, Provider: "ollama", InputTokens: 5, OutputTokens: 5},
	}
	for i := range recs {
		if err := m.Tasks.RecordUsage(ctx, &recs[i]); err != nil {
			t.Fatal(err)
		}
	}

	in, out, reqs, per, err := m.Tasks.UsageSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in != 125 || out != 65 || reqs != 3 {
		t.Fatalf("totals wrong: in=%d out=%d reqs=%d (want 125/65/3)", in, out, reqs)
	}
	if len(per) != 2 {
		t.Fatalf("want 2 providers, got %d", len(per))
	}
	// 按 tokens 总量倒序：deepseek(180) 在 ollama(10) 前面
	if per[0].Provider != "deepseek" {
		t.Fatalf("providers not sorted by volume: %+v", per)
	}
	if per[0].InputTokens != 120 || per[0].OutputTokens != 60 || per[0].TotalRequests != 2 {
		t.Fatalf("deepseek aggregate wrong: %+v", per[0])
	}
}

// ---------- 知识库 ----------

func TestMemoryKnowledge_Ownership(t *testing.T) {
	m, ctx := newMem(t)
	alice := mkUser(t, m, "alice")
	bob := mkUser(t, m, "bob")

	kb := &model.KnowledgeBase{UserID: alice.ID, Name: "docs"}
	if err := m.Knowledge.CreateKB(ctx, kb); err != nil {
		t.Fatal(err)
	}

	ok, err := m.Knowledge.BelongsToUser(ctx, kb.ID, alice.ID)
	if err != nil || !ok {
		t.Fatalf("alice should own kb: ok=%v err=%v", ok, err)
	}
	ok, err = m.Knowledge.BelongsToUser(ctx, kb.ID, bob.ID)
	if err != nil || ok {
		t.Fatalf("bob should not own kb: ok=%v err=%v", ok, err)
	}
	// 删除他人 KB → ErrKBNotFound
	if err := m.Knowledge.DeleteKB(ctx, kb.ID, bob.ID); !errors.Is(err, ErrKBNotFound) {
		t.Fatalf("want ErrKBNotFound, got %v", err)
	}
	// 不存在的 KB → false 而不是 error
	ok, err = m.Knowledge.BelongsToUser(ctx, 9999, alice.ID)
	if err != nil || ok {
		t.Fatalf("unknown kb should be (false, nil): ok=%v err=%v", ok, err)
	}
}

// DeleteKB 必须级联清理文档与分块（对齐 ON DELETE CASCADE）。
func TestMemoryKnowledge_DeleteCascades(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	kb := &model.KnowledgeBase{UserID: u.ID, Name: "docs"}
	_ = m.Knowledge.CreateKB(ctx, kb)

	doc := &model.Document{KnowledgeBaseID: kb.ID, Filename: "f.md", Status: model.DocIndexed, Content: "c"}
	_ = m.Knowledge.CreateDocument(ctx, doc)
	_ = m.Knowledge.BatchInsertChunks(ctx, []model.DocumentChunk{
		{DocumentID: doc.ID, Content: "chunk1", Embedding: []float64{1, 0}},
		{DocumentID: doc.ID, Content: "chunk2", Embedding: []float64{0, 1}},
	})

	hits, _ := m.Knowledge.SearchChunks(ctx, kb.ID, []float64{1, 0}, 10)
	if len(hits) != 2 {
		t.Fatalf("want 2 chunks before delete, got %d", len(hits))
	}

	if err := m.Knowledge.DeleteKB(ctx, kb.ID, u.ID); err != nil {
		t.Fatal(err)
	}

	if docs, _ := m.Knowledge.ListDocuments(ctx, kb.ID); len(docs) != 0 {
		t.Fatalf("documents not cascaded: %d left", len(docs))
	}
	if _, err := m.Knowledge.GetDocument(ctx, doc.ID); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("document should be gone, got %v", err)
	}
	if hits, _ := m.Knowledge.SearchChunks(ctx, kb.ID, []float64{1, 0}, 10); len(hits) != 0 {
		t.Fatalf("chunks not cascaded: %d left", len(hits))
	}
}

// SearchChunks 只返回 indexed 文档，且必须填充 Filename。
func TestMemorySearchChunks_OnlyIndexedWithFilename(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	kb := &model.KnowledgeBase{UserID: u.ID, Name: "docs"}
	_ = m.Knowledge.CreateKB(ctx, kb)

	indexed := &model.Document{KnowledgeBaseID: kb.ID, Filename: "good.md", Status: model.DocIndexed}
	pending := &model.Document{KnowledgeBaseID: kb.ID, Filename: "bad.md", Status: model.DocPending}
	_ = m.Knowledge.CreateDocument(ctx, indexed)
	_ = m.Knowledge.CreateDocument(ctx, pending)

	_ = m.Knowledge.BatchInsertChunks(ctx, []model.DocumentChunk{
		{DocumentID: indexed.ID, Content: "ok", Embedding: []float64{1, 2, 3}},
		{DocumentID: pending.ID, Content: "not yet", Embedding: []float64{4, 5, 6}},
	})

	hits, err := m.Knowledge.SearchChunks(ctx, kb.ID, []float64{1, 2, 3}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("want only indexed doc's chunks, got %d", len(hits))
	}
	if hits[0].Filename != "good.md" {
		t.Fatalf("filename not populated: %q", hits[0].Filename)
	}
	// 检索结果不携带向量：PostgreSQL 版按列不回传 embedding_v，
	// 内存版必须一致（详见 SearchChunks 的注释）。
	if len(hits[0].Embedding) != 0 {
		t.Fatalf("search result should not carry the embedding, got %v", hits[0].Embedding)
	}
	if hits[0].Score < 0.99 {
		t.Fatalf("identical vector should score ~1, got %f", hits[0].Score)
	}
}

// SearchChunks 的接口契约：按相似度降序、按 k 截断、Score 量纲与余弦一致。
// 这是 PostgreSQL（pgvector）与内存两个后端都必须满足的共同语义。
func TestMemorySearchChunks_TopKAndRanking(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")

	kb := &model.KnowledgeBase{UserID: u.ID, Name: "docs"}
	_ = m.Knowledge.CreateKB(ctx, kb)
	doc := &model.Document{KnowledgeBaseID: kb.ID, Filename: "f.md", Status: model.DocIndexed}
	_ = m.Knowledge.CreateDocument(ctx, doc)

	// 与查询向量 [1,0,0] 的余弦依次为 1 / 0.707 / 0（已按相似度降序排列）
	_ = m.Knowledge.BatchInsertChunks(ctx, []model.DocumentChunk{
		{DocumentID: doc.ID, Content: "exact", Embedding: []float64{1, 0, 0}},
		{DocumentID: doc.ID, Content: "close", Embedding: []float64{1, 1, 0}},
		{DocumentID: doc.ID, Content: "orthogonal", Embedding: []float64{0, 0, 1}},
	})

	hits, err := m.Knowledge.SearchChunks(ctx, kb.ID, []float64{1, 0, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("k=2 应只回传 2 条，实际 %d", len(hits))
	}
	if hits[0].Content != "exact" || hits[1].Content != "close" {
		t.Fatalf("排序错误：期望 [exact close]，实际 [%s %s]", hits[0].Content, hits[1].Content)
	}
	if hits[0].Score < 0.999 {
		t.Fatalf("完全相同的向量余弦应为 1，实际 %f", hits[0].Score)
	}
	if hits[1].Score < 0.70 || hits[1].Score > 0.72 {
		t.Fatalf("45° 夹角余弦应约 0.707，实际 %f", hits[1].Score)
	}

	// 空查询向量应安全返回空，而不是把全库倒出来
	empty, err := m.Knowledge.SearchChunks(ctx, kb.ID, nil, 5)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空向量应返回空，实际 %d 条 err=%v", len(empty), err)
	}
}

// ---------- Provider ----------

func TestMemoryProvider_UpsertSemantics(t *testing.T) {
	m, ctx := newMem(t)

	p := &model.LLMProvider{Name: "deepseek", BaseURL: "https://api.deepseek.com", Enabled: true, Priority: 1}
	if err := m.Providers.UpsertProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	firstID := p.ID

	// 同名再 upsert → 更新而非新增（ON CONFLICT (name) DO UPDATE）
	p2 := &model.LLMProvider{Name: "deepseek", BaseURL: "https://new.example.com", Enabled: false, Priority: 5}
	if err := m.Providers.UpsertProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if p2.ID != firstID {
		t.Fatalf("upsert created a new row: %d -> %d", firstID, p2.ID)
	}

	list, _ := m.Providers.ListProviders(ctx)
	if len(list) != 1 {
		t.Fatalf("want 1 provider, got %d", len(list))
	}
	if list[0].BaseURL != "https://new.example.com" || list[0].Enabled || list[0].Priority != 5 {
		t.Fatalf("upsert did not update: %+v", list[0])
	}
}

func TestMemoryProvider_ListOrder(t *testing.T) {
	m, ctx := newMem(t)

	for _, p := range []*model.LLMProvider{
		{Name: "c", Priority: 3},
		{Name: "a", Priority: 1},
		{Name: "b", Priority: 2},
	} {
		if err := m.Providers.UpsertProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	list, _ := m.Providers.ListProviders(ctx)
	want := []string{"a", "b", "c"} // ORDER BY priority ASC
	for i, w := range want {
		if list[i].Name != w {
			t.Fatalf("order wrong at %d: got %q want %q", i, list[i].Name, w)
		}
	}
}

func TestMemoryProvider_DeleteCascadesModels(t *testing.T) {
	m, ctx := newMem(t)

	p := &model.LLMProvider{Name: "ollama", Priority: 1}
	_ = m.Providers.UpsertProvider(ctx, p)
	_ = m.Providers.UpsertModel(ctx, &model.LLMModel{ProviderID: p.ID, Name: "qwen2.5", MaxTokens: 4096})

	models, _ := m.Providers.ListModels(ctx, p.ID)
	if len(models) != 1 {
		t.Fatalf("want 1 model, got %d", len(models))
	}

	if err := m.Providers.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Providers.GetProvider(ctx, p.ID); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("provider should be gone, got %v", err)
	}
	if models, _ := m.Providers.ListModels(ctx, p.ID); len(models) != 0 {
		t.Fatalf("models not cascaded: %d left", len(models))
	}
}

func TestMemoryProvider_UpsertModel(t *testing.T) {
	m, ctx := newMem(t)
	p := &model.LLMProvider{Name: "ollama", Priority: 1}
	_ = m.Providers.UpsertProvider(ctx, p)

	mo := &model.LLMModel{ProviderID: p.ID, Name: "qwen2.5", MaxTokens: 2048}
	_ = m.Providers.UpsertModel(ctx, mo)
	firstID := mo.ID

	// 同 (provider_id, name) → 更新 max_tokens
	mo2 := &model.LLMModel{ProviderID: p.ID, Name: "qwen2.5", MaxTokens: 8192}
	_ = m.Providers.UpsertModel(ctx, mo2)

	if mo2.ID != firstID {
		t.Fatalf("model upsert created new row: %d -> %d", firstID, mo2.ID)
	}
	models, _ := m.Providers.ListModels(ctx, p.ID)
	if len(models) != 1 || models[0].MaxTokens != 8192 {
		t.Fatalf("model not updated: %+v", models)
	}
}

// ---------- Reset ----------

func TestMemoryReset(t *testing.T) {
	m, ctx := newMem(t)
	u := mkUser(t, m, "alice")
	_ = m.Tasks.CreateTask(ctx, &model.Task{WorkflowID: 1, UserID: u.ID})

	m.Reset()

	if _, err := m.Users.GetUserByID(ctx, u.ID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("users not cleared: %v", err)
	}
	if list, _ := m.Tasks.ListTasks(ctx, u.ID, 10, 0); len(list) != 0 {
		t.Fatalf("tasks not cleared: %d", len(list))
	}
	// 重置后 ID 从 1 重新开始，且能正常写入
	nu := mkUser(t, m, "bob")
	if nu.ID != 1 {
		t.Fatalf("sequence not reset: got id %d", nu.ID)
	}
}

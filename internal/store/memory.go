package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// MemoryStore 是纯内存数据访问层，语义对齐 PostgreSQL 实现。
//
// 用途：
//   - STORAGE_MODE=memory 时零依赖启动完整服务（演示 / 面试现场 / 离线开发）
//   - 单元测试里替代数据库
//
// 一致性保证：
//   - 全局一把 sync.RWMutex 保护所有 map（演示场景优先正确性，不追求分片并发）
//   - 自增 ID 与 SERIAL 行为一致（单调递增，删除后不复用）
//   - 复制语义：读出的对象是深拷贝，调用方修改不会污染"库"内数据
//     （PostgreSQL 天然如此，内存实现必须显式模拟，否则会出现诡异的共享 bug）
type MemoryStore struct {
	*Store

	mu sync.RWMutex

	seq int64 // 全局自增 ID

	users     map[int64]*model.User
	userNames map[string]int64 // username -> id
	userMails map[string]int64 // email -> id

	workflows map[int64]*model.Workflow
	wfNodes   map[int64][]model.WorkflowNode
	wfEdges   map[int64][]model.WorkflowEdge

	tasks     map[int64]*model.Task
	taskNodes map[int64][]model.TaskNode
	taskLogs  map[int64][]model.TaskLog
	usageRecs []model.UsageRecord

	kbs    map[int64]*model.KnowledgeBase
	docs   map[int64]*model.Document
	chunks map[int64][]model.DocumentChunk

	providers map[int64]*model.LLMProvider
	provName  map[string]int64
	models    map[int64][]model.LLMModel
}

func newMemoryStore() *MemoryStore {
	m := &MemoryStore{
		users:     map[int64]*model.User{},
		userNames: map[string]int64{},
		userMails: map[string]int64{},
		workflows: map[int64]*model.Workflow{},
		wfNodes:   map[int64][]model.WorkflowNode{},
		wfEdges:   map[int64][]model.WorkflowEdge{},
		tasks:     map[int64]*model.Task{},
		taskNodes: map[int64][]model.TaskNode{},
		taskLogs:  map[int64][]model.TaskLog{},
		kbs:       map[int64]*model.KnowledgeBase{},
		docs:      map[int64]*model.Document{},
		chunks:    map[int64][]model.DocumentChunk{},
		providers: map[int64]*model.LLMProvider{},
		provName:  map[string]int64{},
		models:    map[int64][]model.LLMModel{},
	}
	// 内嵌的 *Store 字段使用同一批内存实现，接口类型让上层无感。
	m.Store = &Store{
		db:        nil, // 无数据库
		Users:     &memUserRepo{m: m},
		Workflows: &memWorkflowRepo{m: m},
		Tasks:     &memTaskRepo{m: m},
		Knowledge: &memKnowledgeRepo{m: m},
		Providers: &memProviderRepo{m: m},
	}
	return m
}

// nextID 必须在持有写锁时调用。
func (m *MemoryStore) nextID() int64 {
	m.seq++
	return m.seq
}

// Reset 清空全部数据（测试用）。
// 注意：不能整体赋值 *m = *fresh —— MemoryStore 内含 sync.RWMutex，
// 复制锁会触发 go vet 告警，且语义上也会丢失并发安全性。
// 逐个替换受保护的 map 即可。
func (m *MemoryStore) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq = 0
	m.users = map[int64]*model.User{}
	m.userNames = map[string]int64{}
	m.userMails = map[string]int64{}
	m.workflows = map[int64]*model.Workflow{}
	m.wfNodes = map[int64][]model.WorkflowNode{}
	m.wfEdges = map[int64][]model.WorkflowEdge{}
	m.tasks = map[int64]*model.Task{}
	m.taskNodes = map[int64][]model.TaskNode{}
	m.taskLogs = map[int64][]model.TaskLog{}
	m.usageRecs = nil
	m.kbs = map[int64]*model.KnowledgeBase{}
	m.docs = map[int64]*model.Document{}
	m.chunks = map[int64][]model.DocumentChunk{}
	m.providers = map[int64]*model.LLMProvider{}
	m.provName = map[string]int64{}
	m.models = map[int64][]model.LLMModel{}
}

// ---------- 深拷贝辅助 ----------
//
// 内存实现必须模拟数据库的"值语义"：读出去的对象被调用方改写时，
// 不能影响库内数据；否则会出现"下游改了上游状态"的隐蔽 bug。

func copyUser(u *model.User) *model.User {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

func copyWorkflow(w *model.Workflow) *model.Workflow {
	if w == nil {
		return nil
	}
	c := *w
	c.Nodes = append([]model.WorkflowNode(nil), w.Nodes...)
	c.Edges = append([]model.WorkflowEdge(nil), w.Edges...)
	return &c
}

func copyTask(t *model.Task) *model.Task {
	if t == nil {
		return nil
	}
	c := *t
	c.Nodes = append([]model.TaskNode(nil), t.Nodes...)
	return &c
}

func copyTaskNode(n model.TaskNode) model.TaskNode {
	c := n
	if n.StartedAt != nil {
		v := *n.StartedAt
		c.StartedAt = &v
	}
	if n.FinishedAt != nil {
		v := *n.FinishedAt
		c.FinishedAt = &v
	}
	return c
}

func copyDoc(d *model.Document) *model.Document {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

func copyChunk(c model.DocumentChunk) model.DocumentChunk {
	out := c
	out.Embedding = append([]float64(nil), c.Embedding...)
	return out
}

// ---------- 用户 ----------

type memUserRepo struct{ m *MemoryStore }

func (r *memUserRepo) CreateUser(_ context.Context, u *model.User) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.userNames[u.Username]; ok {
		return ErrUserExists
	}
	if _, ok := m.userMails[u.Email]; ok {
		return ErrUserExists
	}
	now := time.Now()
	u.ID = m.nextID()
	u.CreatedAt, u.UpdatedAt = now, now
	m.users[u.ID] = copyUser(u)
	m.userNames[u.Username] = u.ID
	m.userMails[u.Email] = u.ID
	return nil
}

func (r *memUserRepo) GetUserByUsername(_ context.Context, username string) (*model.User, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	id, ok := m.userNames[username]
	if !ok {
		return nil, ErrUserNotFound
	}
	return copyUser(m.users[id]), nil
}

func (r *memUserRepo) GetUserByID(_ context.Context, id int64) (*model.User, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	u, ok := m.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return copyUser(u), nil
}

// ---------- 工作流 ----------

type memWorkflowRepo struct{ m *MemoryStore }

func (r *memWorkflowRepo) CreateWorkflow(_ context.Context, wf *model.Workflow) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	wf.ID = m.nextID()
	wf.CreatedAt, wf.UpdatedAt = now, now

	stored := copyWorkflow(wf)
	stored.Nodes, stored.Edges = nil, nil
	m.workflows[wf.ID] = stored

	// 节点/边各自分配 ID，并回填 workflow_id（对齐 PostgreSQL 的插入行为）
	nodes := make([]model.WorkflowNode, 0, len(wf.Nodes))
	for _, n := range wf.Nodes {
		n.ID = m.nextID()
		n.WorkflowID = wf.ID
		n.CreatedAt = now
		nodes = append(nodes, n)
	}
	m.wfNodes[wf.ID] = nodes

	edges := make([]model.WorkflowEdge, 0, len(wf.Edges))
	for _, e := range wf.Edges {
		e.ID = m.nextID()
		e.WorkflowID = wf.ID
		e.CreatedAt = now
		edges = append(edges, e)
	}
	m.wfEdges[wf.ID] = edges
	return nil
}

func (r *memWorkflowRepo) UpdateWorkflow(_ context.Context, wf *model.Workflow) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	cur, ok := m.workflows[wf.ID]
	if !ok || cur.UserID != wf.UserID {
		return ErrWorkflowNotFound
	}
	now := time.Now()
	updated := copyWorkflow(wf)
	updated.UserID = cur.UserID
	updated.CreatedAt = cur.CreatedAt
	updated.UpdatedAt = now
	updated.Nodes, updated.Edges = nil, nil
	m.workflows[wf.ID] = updated

	// 先删后插，与 PostgreSQL 的 tx 语义一致
	delete(m.wfNodes, wf.ID)
	delete(m.wfEdges, wf.ID)

	nodes := make([]model.WorkflowNode, 0, len(wf.Nodes))
	for _, n := range wf.Nodes {
		n.ID = m.nextID()
		n.WorkflowID = wf.ID
		n.CreatedAt = now
		nodes = append(nodes, n)
	}
	m.wfNodes[wf.ID] = nodes

	edges := make([]model.WorkflowEdge, 0, len(wf.Edges))
	for _, e := range wf.Edges {
		e.ID = m.nextID()
		e.WorkflowID = wf.ID
		e.CreatedAt = now
		edges = append(edges, e)
	}
	m.wfEdges[wf.ID] = edges
	return nil
}

func (r *memWorkflowRepo) DeleteWorkflow(_ context.Context, id, userID int64) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	cur, ok := m.workflows[id]
	if !ok || cur.UserID != userID {
		return ErrWorkflowNotFound
	}
	delete(m.workflows, id)
	delete(m.wfNodes, id)
	delete(m.wfEdges, id)
	return nil
}

func (r *memWorkflowRepo) GetWorkflow(_ context.Context, id, userID int64) (*model.Workflow, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	cur, ok := m.workflows[id]
	if !ok || cur.UserID != userID {
		return nil, ErrWorkflowNotFound
	}
	out := copyWorkflow(cur)
	out.Nodes = append([]model.WorkflowNode(nil), m.wfNodes[id]...)
	out.Edges = append([]model.WorkflowEdge(nil), m.wfEdges[id]...)
	return out, nil
}

func (r *memWorkflowRepo) ListWorkflows(_ context.Context, userID int64) ([]model.Workflow, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.Workflow, 0, len(m.workflows))
	for _, w := range m.workflows {
		if w.UserID == userID {
			c := copyWorkflow(w)
			c.Nodes, c.Edges = nil, nil
			out = append(out, *c)
		}
	}
	// ORDER BY updated_at DESC
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (r *memWorkflowRepo) GetWorkflowNodes(_ context.Context, workflowID int64) ([]model.WorkflowNode, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]model.WorkflowNode(nil), m.wfNodes[workflowID]...), nil
}

func (r *memWorkflowRepo) GetWorkflowEdges(_ context.Context, workflowID int64) ([]model.WorkflowEdge, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]model.WorkflowEdge(nil), m.wfEdges[workflowID]...), nil
}

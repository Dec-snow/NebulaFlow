package store

import (
	"context"
	"sort"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ---------- 任务 ----------

type memTaskRepo struct{ m *MemoryStore }

func (r *memTaskRepo) CreateTask(_ context.Context, t *model.Task) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	t.ID = m.nextID()
	t.CreatedAt = time.Now()
	if t.Status == "" {
		t.Status = model.TaskPending
	}
	m.tasks[t.ID] = copyTask(t)
	return nil
}

func (r *memTaskRepo) GetTask(_ context.Context, id, userID int64) (*model.Task, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.tasks[id]
	if !ok || t.UserID != userID {
		return nil, ErrTaskNotFound
	}
	out := copyTask(t)
	out.Nodes = append([]model.TaskNode(nil), m.taskNodes[id]...)
	return out, nil
}

// GetTaskByID 供调度器使用（不校验 user_id）。
func (r *memTaskRepo) GetTaskByID(_ context.Context, id int64) (*model.Task, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.tasks[id]
	if !ok {
		return nil, ErrTaskNotFound
	}
	return copyTask(t), nil
}

func (r *memTaskRepo) ListTasks(_ context.Context, userID int64, limit, offset int) ([]model.Task, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	out := make([]model.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if t.UserID == userID {
			out = append(out, *copyTask(t))
		}
	}
	// ORDER BY id DESC
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })

	if offset >= len(out) {
		return []model.Task{}, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// UpdateTaskStatus 对齐 PostgreSQL 版本的状态机语义：
//   - started_at 只在首次进入 running 时写入（COALESCE 语义）
//   - finished_at 只在进入终态时写入
//
// 注意 PostgreSQL 版每次都会整列覆盖 output/error，此处保持一致。
func (r *memTaskRepo) UpdateTaskStatus(_ context.Context, id int64, status model.TaskStatus, output, errMsg string) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[id]
	if !ok {
		return ErrTaskNotFound
	}
	now := time.Now()
	t.Status = status
	t.Output = output
	t.Error = errMsg
	if t.Status == model.TaskRunning && t.StartedAt == nil {
		v := now
		t.StartedAt = &v
	}
	switch status {
	case model.TaskSucceeded, model.TaskFailed, model.TaskCancelled:
		v := now
		t.FinishedAt = &v
	}
	return nil
}

// ---------- 任务节点 ----------

func (r *memTaskRepo) GetTaskNodes(_ context.Context, taskID int64) ([]model.TaskNode, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	src := m.taskNodes[taskID]
	out := make([]model.TaskNode, 0, len(src))
	for _, n := range src {
		out = append(out, copyTaskNode(n))
	}
	return out, nil
}

// CreateTaskNodes 批量创建节点，status 固定为 pending（对齐 PostgreSQL 版 SQL）。
func (r *memTaskRepo) CreateTaskNodes(_ context.Context, taskID int64, nodes []model.WorkflowNode) error {
	if len(nodes) == 0 {
		return nil
	}
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.taskNodes[taskID]
	for _, n := range nodes {
		cur = append(cur, model.TaskNode{
			ID:       m.nextID(),
			TaskID:   taskID,
			NodeID:   n.ID,
			NodeKey:  n.NodeKey,
			NodeType: n.NodeType,
			Status:   model.NodePending,
		})
	}
	m.taskNodes[taskID] = cur
	return nil
}

func (r *memTaskRepo) CountTaskNodes(_ context.Context, taskID int64) (int, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.taskNodes[taskID]), nil
}

// UpdateTaskNode 按 (task_id, node_key) 定位更新，对齐 PostgreSQL 版的 WHERE 条件。
//
// 时间戳语义严格照抄 PostgreSQL 的 SQL：
//
//	started_at  = COALESCE(started_at, CASE WHEN status='running' THEN now() END)
//	finished_at = CASE WHEN status IN ('succeeded','failed','skipped') THEN now() END
//
// 注意 started_at 只在"本次变更状态为 running 且此前为空"时才写入。
// 若某节点未经 running 直接进入终态，started_at 保持 NULL —— 这是 SQL 的
// 真实行为，内存实现必须一致，不能"自作聪明"地补上一个值。
// （实践中调度器 ExecuteNode 一定先 publishNode(running)，所以正常链路不受影响。）
func (r *memTaskRepo) UpdateTaskNode(_ context.Context, n *model.TaskNode) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.taskNodes[n.TaskID]
	idx := -1
	for i := range cur {
		if cur[i].NodeKey == n.NodeKey {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrTaskNotFound
	}
	now := time.Now()
	old := cur[idx]

	old.Status = n.Status
	old.Input = n.Input
	old.Output = n.Output
	old.Error = n.Error
	old.Retries = n.Retries
	old.TokensIn = n.TokensIn
	old.TokensOut = n.TokensOut
	old.DurationMS = n.DurationMS

	// COALESCE(started_at, CASE WHEN status='running' THEN now() END)
	if n.Status == model.NodeRunning && old.StartedAt == nil {
		v := now
		old.StartedAt = &v
	}
	// 调用方显式带了时间戳时以调用方为准（对齐 SQL 中的显式赋值场景）
	if n.StartedAt != nil {
		v := *n.StartedAt
		old.StartedAt = &v
	}
	if n.FinishedAt != nil {
		v := *n.FinishedAt
		old.FinishedAt = &v
	} else if n.Status == model.NodeSucceeded || n.Status == model.NodeFailed ||
		n.Status == model.NodeSkipped || n.Status == model.NodeCancelled {
		v := now
		old.FinishedAt = &v
	}
	cur[idx] = old
	return nil
}

// ---------- 日志 ----------

func (r *memTaskRepo) AppendLog(_ context.Context, l *model.TaskLog) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	l.ID = m.nextID()
	l.CreatedAt = time.Now()
	m.taskLogs[l.TaskID] = append(m.taskLogs[l.TaskID], *l)
	return nil
}

// ListLogs 对齐 PostgreSQL 版：按 id 倒序取 limit 条，再翻转成正序返回。
// 即"取最近的 N 条，按时间正序展示"。
func (r *memTaskRepo) ListLogs(_ context.Context, taskID int64, limit int) ([]model.TaskLog, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 || limit > 200 {
		limit = 100
	}
	src := m.taskLogs[taskID]
	sorted := append([]model.TaskLog(nil), src...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID > sorted[j].ID })
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	// 正序返回
	for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
		sorted[i], sorted[j] = sorted[j], sorted[i]
	}
	return sorted, nil
}

// ---------- 用量 ----------

func (r *memTaskRepo) RecordUsage(_ context.Context, u *model.UsageRecord) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	u.ID = m.nextID()
	u.CreatedAt = time.Now()
	m.usageRecs = append(m.usageRecs, *u)
	return nil
}

// UsageSummary 对齐 PostgreSQL 版的聚合语义：
// 全局求和 + 按 provider 分组，并按 tokens 总量倒序。
func (r *memTaskRepo) UsageSummary(_ context.Context) (int64, int64, int64, []ProviderUsage, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	var totalIn, totalOut, totalReq int64
	agg := map[string]*ProviderUsage{}
	for _, u := range m.usageRecs {
		totalIn += int64(u.InputTokens)
		totalOut += int64(u.OutputTokens)
		totalReq++
		p, ok := agg[u.Provider]
		if !ok {
			p = &ProviderUsage{Provider: u.Provider}
			agg[u.Provider] = p
		}
		p.TotalRequests++
		p.InputTokens += int64(u.InputTokens)
		p.OutputTokens += int64(u.OutputTokens)
	}

	out := make([]ProviderUsage, 0, len(agg))
	for _, p := range agg {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		si := out[i].InputTokens + out[i].OutputTokens
		sj := out[j].InputTokens + out[j].OutputTokens
		if si != sj {
			return si > sj
		}
		return out[i].Provider < out[j].Provider
	})
	return totalIn, totalOut, totalReq, out, nil
}

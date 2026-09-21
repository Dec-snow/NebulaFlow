package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/jackc/pgx/v5"
)

type TaskRepo struct{ db *database.DB }

func (r *TaskRepo) CreateTask(ctx context.Context, t *model.Task) error {
	err := r.db.Pool.QueryRow(ctx,
		`INSERT INTO tasks (workflow_id, user_id, status, input) VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at`, t.WorkflowID, t.UserID, t.Status, t.Input,
	).Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return err
	}
	return nil
}

func (r *TaskRepo) GetTask(ctx context.Context, id, userID int64) (*model.Task, error) {
	t := &model.Task{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, workflow_id, user_id, status, input, output, error, started_at, finished_at, created_at
		 FROM tasks WHERE id=$1 AND user_id=$2`, id, userID,
	).Scan(&t.ID, &t.WorkflowID, &t.UserID, &t.Status, &t.Input, &t.Output, &t.Error,
		&t.StartedAt, &t.FinishedAt, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	nodes, err := r.GetTaskNodes(ctx, id)
	if err != nil {
		return nil, err
	}
	t.Nodes = nodes
	return t, nil
}

// GetTaskByID 供调度器使用（不校验 user_id，内部系统调用）。
func (r *TaskRepo) GetTaskByID(ctx context.Context, id int64) (*model.Task, error) {
	t := &model.Task{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, workflow_id, user_id, status, input, output, error, started_at, finished_at, created_at
		 FROM tasks WHERE id=$1`, id,
	).Scan(&t.ID, &t.WorkflowID, &t.UserID, &t.Status, &t.Input, &t.Output, &t.Error,
		&t.StartedAt, &t.FinishedAt, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (r *TaskRepo) ListTasks(ctx context.Context, userID int64, limit, offset int) ([]model.Task, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, workflow_id, user_id, status, input, output, error, started_at, finished_at, created_at
		 FROM tasks WHERE user_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`,
		userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		var t model.Task
		if err := rows.Scan(&t.ID, &t.WorkflowID, &t.UserID, &t.Status, &t.Input, &t.Output, &t.Error,
			&t.StartedAt, &t.FinishedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTaskStatus 原子推进任务状态（含运行时间戳），返回受影响行数。
// 注意：status 列是 VARCHAR(16)，同一参数 $2 多处出现时类型推断会冲突
// （列赋值→varchar，与 unknown 字面量比较→text），因此显式 ::text 统一。
func (r *TaskRepo) UpdateTaskStatus(ctx context.Context, id int64, status model.TaskStatus, output, errMsg string) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE tasks SET status=$2::text, output=$3::text, error=$4::text,
		 started_at = COALESCE(started_at, CASE WHEN $2::text='running' THEN now() END),
		 finished_at = CASE WHEN $2::text IN ('succeeded','failed','cancelled') THEN now() END
		 WHERE id=$1`, id, status, output, errMsg)
	return err
}

func (r *TaskRepo) GetTaskNodes(ctx context.Context, taskID int64) ([]model.TaskNode, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, task_id, node_id, node_key, node_type, status, input, output, error,
		        retries, tokens_in, tokens_out, duration_ms, started_at, finished_at
		 FROM task_nodes WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TaskNode
	for rows.Next() {
		var n model.TaskNode
		if err := rows.Scan(&n.ID, &n.TaskID, &n.NodeID, &n.NodeKey, &n.NodeType, &n.Status,
			&n.Input, &n.Output, &n.Error, &n.Retries, &n.TokensIn, &n.TokensOut,
			&n.DurationMS, &n.StartedAt, &n.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateTaskNodes 批量插入任务节点（单条 SQL，减少 round-trip）。
// 原实现逐行 INSERT，对大工作流会产生 N 次 DB 往返。
func (r *TaskRepo) CreateTaskNodes(ctx context.Context, taskID int64, nodes []model.WorkflowNode) error {
	if len(nodes) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO task_nodes (task_id, node_id, node_key, node_type, status) VALUES ")
	args := make([]any, 0, len(nodes)*4)
	for i, n := range nodes {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,'pending')", i*4+1, i*4+2, i*4+3, i*4+4)
		args = append(args, taskID, n.ID, n.NodeKey, n.NodeType)
	}
	_, err := r.db.Pool.Exec(ctx, sb.String(), args...)
	return err
}

// CountTaskNodes 返回任务已初始化的节点数，用于任务重投（Nack 重投）时
// 保证 CreateTaskNodes 幂等，避免 task_nodes 出现重复行。
func (r *TaskRepo) CountTaskNodes(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := r.db.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM task_nodes WHERE task_id=$1`, taskID).Scan(&n)
	return n, err
}

// UpdateTaskNode 更新节点执行明细。
//
// 时间戳语义：
//   - started_at 只在首次进入 running 时写入（COALESCE 保证不被终态覆盖）
//   - finished_at 在所有终态写入：succeeded / failed / skipped / cancelled
//
// 注意 cancelled 必须在场：任务被取消时 scheduler 会把这个状态写给正在执行
// 的节点（executor.go: errors.Is(err, context.Canceled) → NodeCancelled）。
// 若这里漏掉 cancelled，被取消的节点会出现"状态已终止但 finished_at 为空"
// 的自相矛盾记录（任务级 UpdateTaskStatus 的列表里是有 cancelled 的）。
func (r *TaskRepo) UpdateTaskNode(ctx context.Context, n *model.TaskNode) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE task_nodes SET status=$3::text, input=$4::text, output=$5::text, error=$6::text, retries=$7,
		        tokens_in=$8, tokens_out=$9, duration_ms=$10,
		        started_at = COALESCE(started_at, CASE WHEN $3::text='running' THEN now() END),
		        finished_at = CASE WHEN $3::text IN ('succeeded','failed','skipped','cancelled') THEN now() END
		 WHERE task_id=$1 AND node_key=$2`,
		n.TaskID, n.NodeKey, n.Status, n.Input, n.Output, n.Error, n.Retries,
		n.TokensIn, n.TokensOut, n.DurationMS)
	return err
}

func (r *TaskRepo) AppendLog(ctx context.Context, l *model.TaskLog) error {
	_, err := r.db.Pool.Exec(ctx,
		`INSERT INTO task_logs (task_id, node_key, level, message) VALUES ($1, $2, $3, $4)`,
		l.TaskID, l.NodeKey, l.Level, l.Message)
	return err
}

func (r *TaskRepo) ListLogs(ctx context.Context, taskID int64, limit int) ([]model.TaskLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, task_id, node_key, level, message, created_at
		 FROM task_logs WHERE task_id=$1 ORDER BY id DESC LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TaskLog
	for rows.Next() {
		var l model.TaskLog
		if err := rows.Scan(&l.ID, &l.TaskID, &l.NodeKey, &l.Level, &l.Message, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	// 正序返回
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// RecordUsage 写入 token 用量记录。
func (r *TaskRepo) RecordUsage(ctx context.Context, u *model.UsageRecord) error {
	_, err := r.db.Pool.Exec(ctx,
		`INSERT INTO usage_records (user_id, task_id, node_key, provider, model, input_tokens, output_tokens, latency_ms, error)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		u.UserID, u.TaskID, u.NodeKey, u.Provider, u.Model, u.InputTokens, u.OutputTokens, u.LatencyMS, u.Error)
	return err
}

// UsageByProvider 按 provider 聚合 token 用量。
type ProviderUsage struct {
	Provider      string `json:"provider"`
	TotalRequests int64  `json:"total_requests"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
}

// UsageSummary 返回系统级 token 用量汇总（供 Dashboard 展示）。
func (r *TaskRepo) UsageSummary(ctx context.Context) (totalInput, totalOutput, totalRequests int64, perProvider []ProviderUsage, err error) {
	// 1. 全局汇总
	err = r.db.Pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COUNT(*) FROM usage_records`).Scan(
		&totalInput, &totalOutput, &totalRequests)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	// 2. 按 provider 分组
	rows, err := r.db.Pool.Query(ctx,
		`SELECT provider, COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		 FROM usage_records GROUP BY provider ORDER BY COALESCE(SUM(input_tokens),0)+COALESCE(SUM(output_tokens),0) DESC`)
	if err != nil {
		return totalInput, totalOutput, totalRequests, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pu ProviderUsage
		if e := rows.Scan(&pu.Provider, &pu.TotalRequests, &pu.InputTokens, &pu.OutputTokens); e != nil {
			err = e
			return
		}
		perProvider = append(perProvider, pu)
	}
	return totalInput, totalOutput, totalRequests, perProvider, rows.Err()
}

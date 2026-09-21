package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/jackc/pgx/v5"
)

type WorkflowRepo struct{ db *database.DB }

// CreateWorkflow 在同一事务中写入 workflow + nodes + edges。
func (r *WorkflowRepo) CreateWorkflow(ctx context.Context, wf *model.Workflow) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`INSERT INTO workflows (user_id, name, description, status)
		 VALUES ($1, $2, $3, $4) RETURNING id, created_at, updated_at`,
		wf.UserID, wf.Name, wf.Description, wf.Status,
	).Scan(&wf.ID, &wf.CreatedAt, &wf.UpdatedAt)
	if err != nil {
		return err
	}
	if err := r.insertNodesTx(ctx, tx, wf.ID, wf.Nodes); err != nil {
		return err
	}
	if err := r.insertEdgesTx(ctx, tx, wf.ID, wf.Edges); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateWorkflow 替换整个 workflow（nodes/edges 先删后插）。
func (r *WorkflowRepo) UpdateWorkflow(ctx context.Context, wf *model.Workflow) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE workflows SET name=$1, description=$2, status=$3, updated_at=now()
		 WHERE id=$4 AND user_id=$5`,
		wf.Name, wf.Description, wf.Status, wf.ID, wf.UserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWorkflowNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workflow_nodes WHERE workflow_id=$1`, wf.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workflow_edges WHERE workflow_id=$1`, wf.ID); err != nil {
		return err
	}
	if err := r.insertNodesTx(ctx, tx, wf.ID, wf.Nodes); err != nil {
		return err
	}
	if err := r.insertEdgesTx(ctx, tx, wf.ID, wf.Edges); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *WorkflowRepo) DeleteWorkflow(ctx context.Context, id, userID int64) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM workflows WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWorkflowNotFound
	}
	return nil
}

func (r *WorkflowRepo) GetWorkflow(ctx context.Context, id, userID int64) (*model.Workflow, error) {
	wf := &model.Workflow{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, user_id, name, description, status, created_at, updated_at
		 FROM workflows WHERE id=$1 AND user_id=$2`, id, userID,
	).Scan(&wf.ID, &wf.UserID, &wf.Name, &wf.Description, &wf.Status, &wf.CreatedAt, &wf.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkflowNotFound
	}
	if err != nil {
		return nil, err
	}
	nodes, err := r.GetWorkflowNodes(ctx, id)
	if err != nil {
		return nil, err
	}
	edges, err := r.GetWorkflowEdges(ctx, id)
	if err != nil {
		return nil, err
	}
	wf.Nodes, wf.Edges = nodes, edges
	return wf, nil
}

func (r *WorkflowRepo) ListWorkflows(ctx context.Context, userID int64) ([]model.Workflow, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, user_id, name, description, status, created_at, updated_at
		 FROM workflows WHERE user_id=$1 ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Workflow
	for rows.Next() {
		var wf model.Workflow
		if err := rows.Scan(&wf.ID, &wf.UserID, &wf.Name, &wf.Description, &wf.Status, &wf.CreatedAt, &wf.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, wf)
	}
	return out, rows.Err()
}

func (r *WorkflowRepo) GetWorkflowNodes(ctx context.Context, workflowID int64) ([]model.WorkflowNode, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, workflow_id, node_key, node_type, config, position_x, position_y, created_at
		 FROM workflow_nodes WHERE workflow_id=$1 ORDER BY id`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.WorkflowNode
	for rows.Next() {
		var n model.WorkflowNode
		var cfg json.RawMessage
		if err := rows.Scan(&n.ID, &n.WorkflowID, &n.NodeKey, &n.NodeType, &cfg, &n.PositionX, &n.PositionY, &n.CreatedAt); err != nil {
			return nil, err
		}
		if len(cfg) > 0 {
			_ = json.Unmarshal(cfg, &n.Config)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (r *WorkflowRepo) GetWorkflowEdges(ctx context.Context, workflowID int64) ([]model.WorkflowEdge, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, workflow_id, source_node, target_node, created_at
		 FROM workflow_edges WHERE workflow_id=$1 ORDER BY id`, workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.WorkflowEdge
	for rows.Next() {
		var e model.WorkflowEdge
		if err := rows.Scan(&e.ID, &e.WorkflowID, &e.SourceNode, &e.TargetNode, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// insertNodesTx 批量插入工作流节点（单条 SQL，减少 round-trip）。
// 原实现逐行 INSERT，对大工作流会产生 N 次事务内往返。
//
// 原实现在这里还有一段"若 Model/Tool/Prompt/KB/Extra 都为空则重置为 NodeConfig{}"
// 的逻辑，本意大概是想存一个干净的 {}，但它连带把 System、MaxRetry、
// TimeoutSec 一起清掉了——只配了 system 提示词的节点保存后配置丢失。
// 直接序列化即可，NodeConfig.String() 的 omitempty 已经会产出紧凑 JSON。
func (r *WorkflowRepo) insertNodesTx(ctx context.Context, tx pgx.Tx, wfID int64, nodes []model.WorkflowNode) error {
	if len(nodes) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO workflow_nodes (workflow_id, node_key, node_type, config, position_x, position_y) VALUES ")
	args := make([]any, 0, len(nodes)*6)
	for i, n := range nodes {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*6 + 1
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4, base+5)
		args = append(args, wfID, n.NodeKey, n.NodeType, n.Config.String(), n.PositionX, n.PositionY)
	}
	_, err := tx.Exec(ctx, sb.String(), args...)
	return err
}

func (r *WorkflowRepo) insertEdgesTx(ctx context.Context, tx pgx.Tx, wfID int64, edges []model.WorkflowEdge) error {
	if len(edges) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO workflow_edges (workflow_id, source_node, target_node) VALUES ")
	args := make([]any, 0, len(edges)*3)
	for i, e := range edges {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*3 + 1
		fmt.Fprintf(&sb, "($%d,$%d,$%d)", base, base+1, base+2)
		args = append(args, wfID, e.SourceNode, e.TargetNode)
	}
	_, err := tx.Exec(ctx, sb.String(), args...)
	return err
}

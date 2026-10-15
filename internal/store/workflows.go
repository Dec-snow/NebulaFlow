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

// ---------- 系统模板的归属：0 <-> NULL ----------
//
// workflows.user_id 有外键指向 users(id)，而 users.id 是 BIGSERIAL（从 1 开始），
// 所以 0 不是合法用户 —— 直接把"系统模板"的 0 写进库会被外键拒绝
// （SQLSTATE 23503 workflows_user_id_fkey）。库里的正确形态是 NULL：
// 系统模板没有归属用户，而 FK 不校验 NULL。
//
// 对外（model / API JSON / 内存实现 / 前端）继续用 model.SystemTemplateUserID = 0
// 作哨兵，翻译只发生在这个文件里：写 0 → NULL，读 NULL → 0。

// ownerParam 把 Go 侧的 UserID 翻译成 SQL 参数：0 → NULL，其余原样。
//
// 非模板工作流不允许用 0：那会静默产出一个"谁都不属于"的普通工作流，
// 既不出现在任何人的列表里，又占着一个 workflow ID。宁可这里显式报错。
func ownerParam(wf *model.Workflow) (any, error) {
	if wf.UserID != model.SystemTemplateUserID {
		return wf.UserID, nil
	}
	if !wf.IsTemplate {
		return nil, fmt.Errorf(
			"workflow %q: user_id=%d 是系统模板专用的哨兵值，非模板工作流必须属于一个真实用户",
			wf.Name, model.SystemTemplateUserID)
	}
	return nil, nil
}

// CreateWorkflow 在同一事务中写入 workflow + nodes + edges。
func (r *WorkflowRepo) CreateWorkflow(ctx context.Context, wf *model.Workflow) error {
	owner, err := ownerParam(wf)
	if err != nil {
		return err
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`INSERT INTO workflows (user_id, name, description, status, is_template, category, icon)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at, updated_at`,
		owner, wf.Name, wf.Description, wf.Status,
		wf.IsTemplate, wf.Category, wf.Icon,
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
	owner, err := ownerParam(wf)
	if err != nil {
		return err
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// IS NOT DISTINCT FROM 而不是 = ：系统模板的 owner 在库里是 NULL，
	// 而 `user_id = NULL` 恒为 UNKNOWN（永远匹配不到），会让自己人都改不了模板。
	// 这个写法同时把归属校验保持得同样严：传真实用户 ID 只能改到该用户自己的行，
	// 传 NULL 只能改到无归属的系统模板，不存在跨用户写入。
	tag, err := tx.Exec(ctx,
		`UPDATE workflows SET name=$1, description=$2, status=$3, is_template=$4, category=$5, icon=$6, updated_at=now()
		 WHERE id=$7 AND user_id IS NOT DISTINCT FROM $8`,
		wf.Name, wf.Description, wf.Status, wf.IsTemplate, wf.Category, wf.Icon,
		wf.ID, owner)
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

// workflowColumns 是 workflows 的统一 SELECT 列表。
// user_id 一律 COALESCE 成 0：库里的 NULL 表示系统模板（见文件头说明），
// Go 侧与 API JSON 的契约仍然是 0，这样内存实现和前端都不用跟着改。
const workflowColumns = `id, COALESCE(user_id, 0), name, description, status, is_template, category, icon, created_at, updated_at`

// DeleteWorkflow 删除工作流。
//
// 归属条件刻意保持严格（不允许传 0 删系统模板）：系统模板由 seed 维护，
// 不应该被任何一个普通用户的请求删掉。
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

// GetWorkflow 按 (id, owner) 取工作流。
//
// 注意这里**故意**不接受"系统模板"：系统模板的 user_id 是 NULL，
// 而 `user_id = $2` 与 NULL 比较恒为 UNKNOWN，所以谁也拿不到它 ——
// 这正是我们要的：模板只能通过 GetTemplate / CreateFromTemplate 使用，
// 不能被人当成自己的 workflow 去改。归属判断因此不需要任何额外分支。
func (r *WorkflowRepo) GetWorkflow(ctx context.Context, id, userID int64) (*model.Workflow, error) {
	wf := &model.Workflow{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT `+workflowColumns+`
		 FROM workflows WHERE id=$1 AND user_id=$2`, id, userID,
	).Scan(&wf.ID, &wf.UserID, &wf.Name, &wf.Description, &wf.Status,
		&wf.IsTemplate, &wf.Category, &wf.Icon, &wf.CreatedAt, &wf.UpdatedAt)
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

// ListWorkflows 列出某个用户自己的工作流（不含模板）。
// 系统模板的 user_id 是 NULL，`user_id=$1` 自然把它排除在外，无需额外条件。
func (r *WorkflowRepo) ListWorkflows(ctx context.Context, userID int64) ([]model.Workflow, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT `+workflowColumns+`
		 FROM workflows WHERE user_id=$1 AND is_template=false ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Workflow
	for rows.Next() {
		var wf model.Workflow
		if err := rows.Scan(&wf.ID, &wf.UserID, &wf.Name, &wf.Description, &wf.Status,
			&wf.IsTemplate, &wf.Category, &wf.Icon, &wf.CreatedAt, &wf.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, wf)
	}
	return out, rows.Err()
}

// ---------- 模板相关 ----------

// ListTemplates 列出所有系统模板（库里 user_id IS NULL）+ 用户自己的模板。
// 分类过滤：category 非空时只返回该分类。
func (r *WorkflowRepo) ListTemplates(ctx context.Context, userID int64, category string) ([]model.Workflow, error) {
	var rows pgx.Rows
	var err error

	// 系统模板排在前面：user_id IS NULL 求值成 boolean，DESC 让 true 先出。
	if category != "" {
		rows, err = r.db.Pool.Query(ctx,
			`SELECT `+workflowColumns+`
			 FROM workflows
			 WHERE is_template=true
			   AND (user_id IS NULL OR user_id=$1)
			   AND category=$2
			 ORDER BY user_id IS NULL DESC, created_at DESC`, userID, category)
	} else {
		rows, err = r.db.Pool.Query(ctx,
			`SELECT `+workflowColumns+`
			 FROM workflows
			 WHERE is_template=true AND (user_id IS NULL OR user_id=$1)
			 ORDER BY user_id IS NULL DESC, created_at DESC`, userID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Workflow
	for rows.Next() {
		var wf model.Workflow
		if err := rows.Scan(&wf.ID, &wf.UserID, &wf.Name, &wf.Description, &wf.Status,
			&wf.IsTemplate, &wf.Category, &wf.Icon, &wf.CreatedAt, &wf.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, wf)
	}
	return out, rows.Err()
}

// ListTemplateCategories 返回用户可见模板的去重分类列表。
// 用 SELECT DISTINCT 在数据库层去重，避免全表加载到内存再去重。
func (r *WorkflowRepo) ListTemplateCategories(ctx context.Context, userID int64) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT DISTINCT category
		 FROM workflows
		 WHERE is_template=true
		   AND (user_id IS NULL OR user_id=$1)
		   AND category IS NOT NULL
		   AND category <> ''
		 ORDER BY category`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var cat string
		if err := rows.Scan(&cat); err != nil {
			return nil, err
		}
		out = append(out, cat)
	}
	return out, rows.Err()
}

// GetTemplate 获取模板详情。
// 系统模板（库里 user_id IS NULL）和用户自己的模板可见；普通用户不能看别人的私有模板。
func (r *WorkflowRepo) GetTemplate(ctx context.Context, id, userID int64) (*model.Workflow, error) {
	wf := &model.Workflow{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT `+workflowColumns+`
		 FROM workflows WHERE id=$1 AND is_template=true AND (user_id IS NULL OR user_id=$2)`, id, userID,
	).Scan(&wf.ID, &wf.UserID, &wf.Name, &wf.Description, &wf.Status,
		&wf.IsTemplate, &wf.Category, &wf.Icon, &wf.CreatedAt, &wf.UpdatedAt)
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

// CreateFromTemplate 从模板复制一个新工作流到目标用户。
// 在一个事务中：读模板 → 复制 workflow + nodes + edges → 返回新 workflow。
func (r *WorkflowRepo) CreateFromTemplate(ctx context.Context, templateID int64, targetUserID int64, newName string) (*model.Workflow, error) {
	// 读模板（含 nodes/edges）
	tpl, err := r.GetTemplate(ctx, templateID, targetUserID)
	if err != nil {
		return nil, fmt.Errorf("template not found: %w", err)
	}
	if !tpl.IsTemplate {
		return nil, fmt.Errorf("workflow %d is not a template", templateID)
	}

	// 名称为空则用模板名 + "副本"
	name := newName
	if name == "" {
		name = tpl.Name + " 副本"
	}

	// 构造新 workflow（去掉模板属性）
	newWF := &model.Workflow{
		UserID:      targetUserID,
		Name:        name,
		Description: tpl.Description,
		Status:      model.WorkflowDraft,
		IsTemplate:  false,
		Category:    "",
		Icon:        "",
		Nodes:       tpl.Nodes,
		Edges:       tpl.Edges,
	}
	// 清空节点 ID（新插入的会重新生成）
	for i := range newWF.Nodes {
		newWF.Nodes[i].ID = 0
		newWF.Nodes[i].WorkflowID = 0
	}
	for i := range newWF.Edges {
		newWF.Edges[i].ID = 0
		newWF.Edges[i].WorkflowID = 0
	}

	if err := r.CreateWorkflow(ctx, newWF); err != nil {
		return nil, fmt.Errorf("create workflow from template: %w", err)
	}
	return newWF, nil
}

// ---------- 节点 / 边 ----------

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

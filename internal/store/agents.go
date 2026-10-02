package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ErrAgentNotFound 表示指定的 Agent 不存在。
var ErrAgentNotFound = errors.New("agent not found")

// AgentRepo 是 Agent 注册中心的 PostgreSQL 实现。
//
// agent_registry 表存储所有已注册的 Agent，支持：
//   - 按 ID / 名称查询
//   - 按 Runtime 类型、能力过滤
//   - 用户级隔离 + 系统内置 Agent（user_id = 0）
type AgentRepo struct{ db *database.DB }

func NewAgentRepo(db *database.DB) *AgentRepo { return &AgentRepo{db: db} }

// CreateAgent 插入一条 Agent 注册记录。
func (r *AgentRepo) CreateAgent(ctx context.Context, a *model.AgentRegistry) error {
	now := time.Now()
	caps := a.Capabilities
	if caps == nil {
		caps = []string{}
	}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO agent_registry
			(user_id, name, description, runtime_type, endpoint, model,
			 capabilities, status, version, timeout_sec, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::text[], $8, $9, $10, $11, $12)
		RETURNING id`,
		a.UserID, a.Name, a.Description, string(a.RuntimeType), a.Endpoint, a.Model,
		caps, string(a.Status), a.Version, a.TimeoutSec, now, now,
	).Scan(&a.ID)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	a.CreatedAt = now
	a.UpdatedAt = now
	return nil
}

// GetAgentByID 按 ID 查询 Agent。
func (r *AgentRepo) GetAgentByID(ctx context.Context, id int64) (*model.AgentRegistry, error) {
	var a model.AgentRegistry
	var caps []string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, user_id, name, description, runtime_type, endpoint, model,
		       capabilities, status, version, timeout_sec, created_at, updated_at
		FROM agent_registry WHERE id = $1`, id,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.Description, (*string)(&a.RuntimeType),
		&a.Endpoint, &a.Model, &caps, (*string)(&a.Status),
		&a.Version, &a.TimeoutSec, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get agent %d: %w", id, err)
	}
	a.Capabilities = caps
	return &a, nil
}

// GetAgentByName 按名称查询（同一用户下名称唯一）。
func (r *AgentRepo) GetAgentByName(ctx context.Context, userID int64, name string) (*model.AgentRegistry, error) {
	var a model.AgentRegistry
	var caps []string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, user_id, name, description, runtime_type, endpoint, model,
		       capabilities, status, version, timeout_sec, created_at, updated_at
		FROM agent_registry
		WHERE (user_id = $1 OR user_id = 0) AND name = $2
		ORDER BY user_id DESC
		LIMIT 1`, userID, name,
	).Scan(&a.ID, &a.UserID, &a.Name, &a.Description, (*string)(&a.RuntimeType),
		&a.Endpoint, &a.Model, &caps, (*string)(&a.Status),
		&a.Version, &a.TimeoutSec, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get agent by name %q: %w", name, err)
	}
	a.Capabilities = caps
	return &a, nil
}

// ListAgents 列出 Agent，支持按 Runtime 类型和能力过滤。
// user_id = 0 的系统内置 Agent 对所有用户可见。
func (r *AgentRepo) ListAgents(ctx context.Context, userID int64, runtimeType model.AgentRuntimeType, capability string) ([]model.AgentRegistry, error) {
	var args []any
	var conds []string

	conds = append(conds, "(user_id = $1 OR user_id = 0)")
	args = append(args, userID)

	if runtimeType != "" {
		conds = append(conds, fmt.Sprintf("runtime_type = $%d", len(args)+1))
		args = append(args, string(runtimeType))
	}
	if capability != "" {
		conds = append(conds, fmt.Sprintf("$%d = ANY(capabilities)", len(args)+1))
		args = append(args, capability)
	}

	query := `
		SELECT id, user_id, name, description, runtime_type, endpoint, model,
		       capabilities, status, version, timeout_sec, created_at, updated_at
		FROM agent_registry
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY created_at DESC`

	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()

	var agents []model.AgentRegistry
	for rows.Next() {
		var a model.AgentRegistry
		var caps []string
		if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.Description, (*string)(&a.RuntimeType),
			&a.Endpoint, &a.Model, &caps, (*string)(&a.Status),
			&a.Version, &a.TimeoutSec, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Capabilities = caps
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// UpdateAgent 更新 Agent 信息。
func (r *AgentRepo) UpdateAgent(ctx context.Context, a *model.AgentRegistry) error {
	now := time.Now()
	caps := a.Capabilities
	if caps == nil {
		caps = []string{}
	}
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE agent_registry SET
			name = $1, description = $2, runtime_type = $3, endpoint = $4,
			model = $5, capabilities = $6::text[], status = $7, version = $8,
			timeout_sec = $9, updated_at = $10
		WHERE id = $11`,
		a.Name, a.Description, string(a.RuntimeType), a.Endpoint,
		a.Model, caps, string(a.Status), a.Version,
		a.TimeoutSec, now, a.ID,
	)
	if err != nil {
		return fmt.Errorf("update agent %d: %w", a.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentNotFound
	}
	a.UpdatedAt = now
	return nil
}

// DeleteAgent 删除 Agent。
func (r *AgentRepo) DeleteAgent(ctx context.Context, id int64) error {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM agent_registry WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete agent %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// ListMarketplaceAgents 列出系统内置 Agent（user_id=0），用于 Agent 市场。
func (r *AgentRepo) ListMarketplaceAgents(ctx context.Context, runtimeType model.AgentRuntimeType, capability string) ([]model.AgentRegistry, error) {
	var args []any
	var conds []string

	conds = append(conds, "user_id = 0 AND status = 'active'")

	if runtimeType != "" {
		conds = append(conds, fmt.Sprintf("runtime_type = $%d", len(args)+1))
		args = append(args, string(runtimeType))
	}
	if capability != "" {
		conds = append(conds, fmt.Sprintf("$%d = ANY(capabilities)", len(args)+1))
		args = append(args, capability)
	}

	query := `
		SELECT id, user_id, name, description, runtime_type, endpoint, model,
		       capabilities, status, version, timeout_sec, created_at, updated_at
		FROM agent_registry
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY created_at DESC`

	rows, err := r.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list marketplace agents: %w", err)
	}
	defer rows.Close()

	var agents []model.AgentRegistry
	for rows.Next() {
		var a model.AgentRegistry
		var caps []string
		if err := rows.Scan(&a.ID, &a.UserID, &a.Name, &a.Description, (*string)(&a.RuntimeType),
			&a.Endpoint, &a.Model, &caps, (*string)(&a.Status),
			&a.Version, &a.TimeoutSec, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Capabilities = caps
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// InstallAgent 从系统 Agent 复制一份到指定用户的注册中心。
// 如果用户已有同名 Agent，会在名称后加数字后缀避免冲突。
func (r *AgentRepo) InstallAgent(ctx context.Context, agentID int64, userID int64) (*model.AgentRegistry, error) {
	// 1. 获取源 Agent（必须是系统内置）
	src, err := r.GetAgentByID(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if src.UserID != 0 {
		return nil, fmt.Errorf("only system agents can be installed")
	}

	// 2. 检查用户是否已有同名 Agent，如有则加后缀
	name := src.Name
	suffix := 1
	for {
		existing, err := r.GetAgentByName(ctx, userID, name)
		if errors.Is(err, ErrAgentNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		// 如果已存在且来源就是同一个系统 agent，则返回已存在的
		// 否则改名重试
		suffix++
		name = fmt.Sprintf("%s-%d", src.Name, suffix)
		if suffix > 100 {
			return nil, fmt.Errorf("too many copies of this agent")
		}
		_ = existing
	}

	// 3. 创建副本
	copy := &model.AgentRegistry{
		UserID:       userID,
		Name:         name,
		Description:  src.Description,
		RuntimeType:  src.RuntimeType,
		Endpoint:     src.Endpoint,
		Model:        src.Model,
		Capabilities: append([]string{}, src.Capabilities...),
		Status:       model.AgentActive,
		Version:      src.Version,
		TimeoutSec:   src.TimeoutSec,
	}

	if err := r.CreateAgent(ctx, copy); err != nil {
		return nil, fmt.Errorf("install agent: %w", err)
	}

	return copy, nil
}

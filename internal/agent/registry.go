package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// AgentStore 是 Agent 注册中心的数据持久化接口。
// 由 store 包实现（PostgreSQL / 内存），Registry 服务只依赖这个接口，
// 避免循环依赖（agent → store → agent）。
type AgentStore interface {
	// CreateAgent 创建一个新的 Agent 注册记录。
	CreateAgent(ctx context.Context, a *model.AgentRegistry) error
	// GetAgentByID 根据 ID 获取 Agent。
	GetAgentByID(ctx context.Context, id int64) (*model.AgentRegistry, error)
	// GetAgentByName 根据名称获取 Agent（同一用户下名称唯一）。
	GetAgentByName(ctx context.Context, userID int64, name string) (*model.AgentRegistry, error)
	// ListAgents 列出用户可见的 Agent（含系统内置）。
	ListAgents(ctx context.Context, userID int64, runtimeType model.AgentRuntimeType, capability string) ([]model.AgentRegistry, error)
	// UpdateAgent 更新 Agent 信息。
	UpdateAgent(ctx context.Context, a *model.AgentRegistry) error
	// DeleteAgent 删除 Agent。
	DeleteAgent(ctx context.Context, id int64) error
}

// RuntimeFactory 是根据 Agent 注册记录创建 Runtime 实例的工厂函数。
// 不同 RuntimeType 对应不同的工厂实现。
type RuntimeFactory func(ctx context.Context, a *model.AgentRegistry) (Runtime, error)

// Registry 是 Agent 注册中心服务。
//
// 职责：
//  1. 管理 Agent 的注册 / 注销 / 查询（持久化到 DB）
//  2. 根据 Agent 记录创建对应的 Runtime 实例
//  3. 提供按能力检索 Agent 的能力发现
//
// 架构类似 Kubernetes 的 Service Discovery + Service Broker：
// 工作流节点指定 agent_id → Registry 查 DB → 创建 Runtime → 执行
//
// 这样 Agent 可以独立部署和版本管理，工作流不需要知道具体实现。
type Registry struct {
	store    AgentStore
	factories map[model.AgentRuntimeType]RuntimeFactory
	cache    sync.Map // agent_id → Runtime 缓存，避免重复创建
}

// NewRegistry 创建一个 Agent 注册中心。
func NewRegistry(store AgentStore) *Registry {
	r := &Registry{
		store:     store,
		factories: make(map[model.AgentRuntimeType]RuntimeFactory),
	}
	return r
}

// RegisterFactory 注册一个 Runtime 工厂。
// 每种 RuntimeType 对应一个工厂，Registry 不直接依赖具体实现。
func (r *Registry) RegisterFactory(rt model.AgentRuntimeType, factory RuntimeFactory) {
	r.factories[rt] = factory
}

// Register 注册一个新的 Agent。
func (r *Registry) Register(ctx context.Context, a *model.AgentRegistry) error {
	if a.Name == "" {
		return fmt.Errorf("agent name is required")
	}
	if a.RuntimeType == "" {
		return fmt.Errorf("agent runtime_type is required")
	}
	if a.Status == "" {
		a.Status = model.AgentActive
	}
	if a.Version == "" {
		a.Version = "1.0.0"
	}
	if _, ok := r.factories[a.RuntimeType]; !ok {
		return fmt.Errorf("unsupported runtime type: %s", a.RuntimeType)
	}
	return r.store.CreateAgent(ctx, a)
}

// Get 获取 Agent 注册信息。
func (r *Registry) Get(ctx context.Context, id int64) (*model.AgentRegistry, error) {
	return r.store.GetAgentByID(ctx, id)
}

// GetByName 根据名称查找 Agent。
func (r *Registry) GetByName(ctx context.Context, userID int64, name string) (*model.AgentRegistry, error) {
	return r.store.GetAgentByName(ctx, userID, name)
}

// List 列出 Agent，支持按 Runtime 类型和能力过滤。
func (r *Registry) List(ctx context.Context, userID int64, runtimeType model.AgentRuntimeType, capability string) ([]model.AgentRegistry, error) {
	return r.store.ListAgents(ctx, userID, runtimeType, capability)
}

// Update 更新 Agent 信息，同时清空缓存（下次获取时重新创建 Runtime）。
func (r *Registry) Update(ctx context.Context, a *model.AgentRegistry) error {
	if err := r.store.UpdateAgent(ctx, a); err != nil {
		return err
	}
	r.cache.Delete(a.ID)
	return nil
}

// Delete 删除 Agent，同时清理缓存。
func (r *Registry) Delete(ctx context.Context, id int64) error {
	if err := r.store.DeleteAgent(ctx, id); err != nil {
		return err
	}
	r.cache.Delete(id)
	return nil
}

// GetRuntime 根据 agent_id 获取（或创建）对应的 Runtime 实例。
//
// 这是注册中心最核心的方法：工作流节点只知道 agent_id，
// 通过这个方法拿到可执行的 Runtime 实例，完全解耦"是什么 Agent"和"怎么用 Agent"。
//
// Runtime 实例会被缓存，避免每次都重新创建（HTTP 客户端、连接池等是有代价的）。
func (r *Registry) GetRuntime(ctx context.Context, agentID int64) (Runtime, error) {
	// 先查缓存
	if cached, ok := r.cache.Load(agentID); ok {
		if rt, ok := cached.(Runtime); ok {
			return rt, nil
		}
	}

	// 从 DB 读取 Agent 信息
	a, err := r.store.GetAgentByID(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("get agent %d: %w", agentID, err)
	}
	if a.Status != model.AgentActive {
		return nil, fmt.Errorf("agent %d is not active (status: %s)", agentID, a.Status)
	}

	// 找对应工厂
	factory, ok := r.factories[a.RuntimeType]
	if !ok {
		return nil, fmt.Errorf("no factory for runtime type: %s", a.RuntimeType)
	}

	// 创建 Runtime 实例
	rt, err := factory(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("create runtime for agent %d: %w", agentID, err)
	}

	// 缓存起来
	r.cache.Store(agentID, rt)
	return rt, nil
}

// FindByCapability 按能力查找可用的 Agent（能力发现）。
// 返回所有具备指定能力的活跃 Agent。
func (r *Registry) FindByCapability(ctx context.Context, userID int64, capability string) ([]model.AgentRegistry, error) {
	return r.store.ListAgents(ctx, userID, "", capability)
}

// InvalidateCache 清空某个 Agent 的 Runtime 缓存（配置变更后调用）。
func (r *Registry) InvalidateCache(agentID int64) {
	r.cache.Delete(agentID)
}

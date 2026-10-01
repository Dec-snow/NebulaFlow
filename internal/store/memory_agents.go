package store

import (
	"context"
	"sort"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ---------- Agent 注册中心 ----------

type memAgentRepo struct{ m *MemoryStore }

func copyAgent(a *model.AgentRegistry) *model.AgentRegistry {
	if a == nil {
		return nil
	}
	c := *a
	c.Capabilities = append([]string(nil), a.Capabilities...)
	return &c
}

func (r *memAgentRepo) CreateAgent(_ context.Context, a *model.AgentRegistry) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	a.ID = m.nextID()
	if a.Status == "" {
		a.Status = model.AgentActive
	}
	if a.Version == "" {
		a.Version = "1.0.0"
	}
	a.CreatedAt, a.UpdatedAt = now, now
	m.agents[a.ID] = copyAgent(a)
	return nil
}

func (r *memAgentRepo) GetAgentByID(_ context.Context, id int64) (*model.AgentRegistry, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	a, ok := m.agents[id]
	if !ok {
		return nil, ErrAgentNotFound
	}
	return copyAgent(a), nil
}

func (r *memAgentRepo) GetAgentByName(_ context.Context, userID int64, name string) (*model.AgentRegistry, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 用户自己的优先于系统内置
	var found *model.AgentRegistry
	for _, a := range m.agents {
		if a.Name != name {
			continue
		}
		if a.UserID == userID {
			return copyAgent(a), nil
		}
		if a.UserID == 0 {
			found = a
		}
	}
	if found != nil {
		return copyAgent(found), nil
	}
	return nil, ErrAgentNotFound
}

func (r *memAgentRepo) ListAgents(_ context.Context, userID int64, runtimeType model.AgentRuntimeType, capability string) ([]model.AgentRegistry, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []model.AgentRegistry
	for _, a := range m.agents {
		// 用户可见：自己的 + 系统内置
		if a.UserID != userID && a.UserID != 0 {
			continue
		}
		if runtimeType != "" && a.RuntimeType != runtimeType {
			continue
		}
		if capability != "" && !containsString(a.Capabilities, capability) {
			continue
		}
		out = append(out, *copyAgent(a))
	}
	// ORDER BY created_at DESC
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (r *memAgentRepo) UpdateAgent(_ context.Context, a *model.AgentRegistry) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	cur, ok := m.agents[a.ID]
	if !ok {
		return ErrAgentNotFound
	}
	now := time.Now()
	updated := copyAgent(a)
	updated.UserID = cur.UserID
	updated.CreatedAt = cur.CreatedAt
	updated.UpdatedAt = now
	m.agents[a.ID] = updated
	return nil
}

func (r *memAgentRepo) DeleteAgent(_ context.Context, id int64) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.agents[id]; !ok {
		return ErrAgentNotFound
	}
	delete(m.agents, id)
	return nil
}

// containsString 判断字符串切片是否包含指定元素。
func containsString(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}

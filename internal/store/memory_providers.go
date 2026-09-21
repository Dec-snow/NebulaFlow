package store

import (
	"context"
	"sort"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ---------- LLM Provider ----------

type memProviderRepo struct{ m *MemoryStore }

// UpsertProvider 对齐 PostgreSQL 的 ON CONFLICT (name) DO UPDATE 语义。
func (r *memProviderRepo) UpsertProvider(_ context.Context, p *model.LLMProvider) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	if id, ok := m.provName[p.Name]; ok {
		cur := m.providers[id]
		cur.BaseURL = p.BaseURL
		cur.APIKey = p.APIKey
		cur.IsDefault = p.IsDefault
		cur.Enabled = p.Enabled
		cur.Priority = p.Priority
		p.ID = cur.ID
		p.CreatedAt = cur.CreatedAt
		return nil
	}
	p.ID = m.nextID()
	p.CreatedAt = time.Now()
	c := *p
	m.providers[p.ID] = &c
	m.provName[p.Name] = p.ID
	return nil
}

// ListProviders 对齐 ORDER BY priority ASC, id ASC。
func (r *memProviderRepo) ListProviders(_ context.Context) ([]model.LLMProvider, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.LLMProvider, 0, len(m.providers))
	for _, p := range m.providers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (r *memProviderRepo) GetProvider(_ context.Context, id int64) (*model.LLMProvider, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	p, ok := m.providers[id]
	if !ok {
		return nil, ErrProviderNotFound
	}
	c := *p
	return &c, nil
}

// DeleteProvider 级联删除其下模型（对齐 ON DELETE CASCADE）。
func (r *memProviderRepo) DeleteProvider(_ context.Context, id int64) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.providers[id]
	if !ok {
		return ErrProviderNotFound
	}
	delete(m.provName, p.Name)
	delete(m.providers, id)
	delete(m.models, id)
	return nil
}

// ---------- LLM Model ----------

func (r *memProviderRepo) ListModels(_ context.Context, providerID int64) ([]model.LLMModel, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	src := m.models[providerID]
	out := make([]model.LLMModel, 0, len(src))
	out = append(out, src...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// UpsertModel 对齐 ON CONFLICT (provider_id, name) DO UPDATE。
func (r *memProviderRepo) UpsertModel(_ context.Context, mo *model.LLMModel) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	cur := m.models[mo.ProviderID]
	for i := range cur {
		if cur[i].Name == mo.Name {
			cur[i].MaxTokens = mo.MaxTokens
			mo.ID = cur[i].ID
			mo.CreatedAt = cur[i].CreatedAt
			m.models[mo.ProviderID] = cur
			return nil
		}
	}
	mo.ID = m.nextID()
	mo.CreatedAt = time.Now()
	m.models[mo.ProviderID] = append(cur, *mo)
	return nil
}

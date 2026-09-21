package store

import (
	"context"
	"errors"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/jackc/pgx/v5"
)

type ProviderRepo struct{ db *database.DB }

func (r *ProviderRepo) UpsertProvider(ctx context.Context, p *model.LLMProvider) error {
	err := r.db.Pool.QueryRow(ctx,
		`INSERT INTO llm_providers (name, base_url, api_key, is_default, enabled, priority)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (name) DO UPDATE SET
		   base_url=EXCLUDED.base_url, api_key=EXCLUDED.api_key,
		   is_default=EXCLUDED.is_default, enabled=EXCLUDED.enabled,
		   priority=EXCLUDED.priority
		 RETURNING id, created_at`,
		p.Name, p.BaseURL, p.APIKey, p.IsDefault, p.Enabled, p.Priority,
	).Scan(&p.ID, &p.CreatedAt)
	return err
}

func (r *ProviderRepo) ListProviders(ctx context.Context) ([]model.LLMProvider, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, name, base_url, api_key, is_default, enabled, priority, created_at
		 FROM llm_providers ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LLMProvider
	for rows.Next() {
		var p model.LLMProvider
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.IsDefault,
			&p.Enabled, &p.Priority, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *ProviderRepo) GetProvider(ctx context.Context, id int64) (*model.LLMProvider, error) {
	p := &model.LLMProvider{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, name, base_url, api_key, is_default, enabled, priority, created_at
		 FROM llm_providers WHERE id=$1`, id,
	).Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.IsDefault, &p.Enabled, &p.Priority, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProviderRepo) DeleteProvider(ctx context.Context, id int64) error {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM llm_providers WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderNotFound
	}
	return nil
}

func (r *ProviderRepo) ListModels(ctx context.Context, providerID int64) ([]model.LLMModel, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, provider_id, name, max_tokens, created_at
		 FROM llm_models WHERE provider_id=$1 ORDER BY id`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LLMModel
	for rows.Next() {
		var m model.LLMModel
		if err := rows.Scan(&m.ID, &m.ProviderID, &m.Name, &m.MaxTokens, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *ProviderRepo) UpsertModel(ctx context.Context, m *model.LLMModel) error {
	err := r.db.Pool.QueryRow(ctx,
		`INSERT INTO llm_models (provider_id, name, max_tokens) VALUES ($1, $2, $3)
		 ON CONFLICT (provider_id, name) DO UPDATE SET max_tokens=EXCLUDED.max_tokens
		 RETURNING id, created_at`,
		m.ProviderID, m.Name, m.MaxTokens,
	).Scan(&m.ID, &m.CreatedAt)
	return err
}

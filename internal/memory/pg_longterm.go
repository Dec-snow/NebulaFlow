package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hoarfrost/nebulaflow/internal/database"
)

// PgLongTermStore 是基于 PostgreSQL + pgvector 的长期记忆实现。
//
// 表结构：agent_memory
//   - user_id: 用户隔离
//   - content: 记忆内容（文本）
//   - category: 分类（preference / fact / task_summary）
//   - source: 来源
//   - embedding_v: 向量（pgvector）
//   - metadata: JSONB 附加信息
//
// 检索方式：余弦相似度 <=>  （pgvector 算子）
type PgLongTermStore struct {
	db *database.DB
}

// NewPgLongTermStore 创建 PostgreSQL 长期记忆存储。
func NewPgLongTermStore(db *database.DB) *PgLongTermStore {
	return &PgLongTermStore{db: db}
}

// Store 保存一条长期记忆。
func (s *PgLongTermStore) Store(ctx context.Context, item MemoryItem, embedding []float32) error {
	now := time.Now()
	if item.Timestamp.IsZero() {
		item.Timestamp = now
	}

	// 把 float32 切片转成 pgvector 格式字符串：[0.1,0.2,...]
	vecStr := vectorToString(embedding)

	var id string
	err := s.db.Pool.QueryRow(ctx, `
		INSERT INTO agent_memory
			(id, user_id, content, category, source, embedding_v, metadata, created_at)
		VALUES (COALESCE(NULLIF($1, ''), gen_random_uuid()::text), $2, $3, $4, $5, $6::vector, $7::jsonb, $8)
		RETURNING id`,
		item.ID, item.UserID, item.Content, item.Category, item.Source,
		vecStr, mapToJSONB(item.Metadata), item.Timestamp,
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("insert agent_memory: %w", err)
	}
	item.ID = id
	return nil
}

// Search 按语义相似度检索记忆（余弦相似度）。
func (s *PgLongTermStore) Search(ctx context.Context, userID int64, queryEmbedding []float32, topK int) ([]MemoryItem, error) {
	if topK <= 0 {
		topK = 5
	}
	vecStr := vectorToString(queryEmbedding)

	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, user_id, content, category, source, metadata, created_at,
		       1 - (embedding_v <=> $1::vector) AS score
		FROM agent_memory
		WHERE user_id = $2
		ORDER BY embedding_v <=> $1::vector
		LIMIT $3`,
		vecStr, userID, topK,
	)
	if err != nil {
		return nil, fmt.Errorf("search agent_memory: %w", err)
	}
	defer rows.Close()

	var items []MemoryItem
	for rows.Next() {
		var item MemoryItem
		var meta map[string]string
		if err := rows.Scan(&item.ID, &item.UserID, &item.Content, &item.Category,
			&item.Source, &meta, &item.Timestamp, &item.Score); err != nil {
			return nil, err
		}
		item.Metadata = meta
		items = append(items, item)
	}
	return items, rows.Err()
}

// Delete 删除一条记忆。
func (s *PgLongTermStore) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM agent_memory WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete agent_memory: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("memory not found")
	}
	return nil
}

// ListByCategory 按分类列出用户的记忆。
func (s *PgLongTermStore) ListByCategory(ctx context.Context, userID int64, category string, limit int) ([]MemoryItem, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	if category == "" {
		rows, err = s.db.Pool.Query(ctx, `
			SELECT id, user_id, content, category, source, metadata, created_at, 0 AS score
			FROM agent_memory
			WHERE user_id = $1
			ORDER BY created_at DESC
			LIMIT $2`, userID, limit,
		)
	} else {
		rows, err = s.db.Pool.Query(ctx, `
			SELECT id, user_id, content, category, source, metadata, created_at, 0 AS score
			FROM agent_memory
			WHERE user_id = $1 AND category = $2
			ORDER BY created_at DESC
			LIMIT $3`, userID, category, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list agent_memory: %w", err)
	}
	defer rows.Close()

	var items []MemoryItem
	for rows.Next() {
		var item MemoryItem
		var meta map[string]string
		if err := rows.Scan(&item.ID, &item.UserID, &item.Content, &item.Category,
			&item.Source, &meta, &item.Timestamp, &item.Score); err != nil {
			return nil, err
		}
		item.Metadata = meta
		items = append(items, item)
	}
	return items, rows.Err()
}

// vectorToString 把 float32 切片转成 pgvector 格式的字符串。
func vectorToString(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%g", f)
	}
	sb.WriteByte(']')
	return sb.String()
}

// mapToJSONB 把 map[string]string 转成 JSONB 字符串。
// 简单实现，用 strings 自己拼，避免额外依赖。
func mapToJSONB(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	var sb strings.Builder
	sb.WriteByte('{')
	first := true
	for k, v := range m {
		if !first {
			sb.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&sb, "%q:%q", k, v)
	}
	sb.WriteByte('}')
	return sb.String()
}

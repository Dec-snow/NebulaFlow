// Package store 提供统一的数据访问层。
//
// 支持两种后端，由 STORAGE_MODE 选择：
//   - postgres（默认）：PostgreSQL，生产形态
//   - memory：纯内存 map + RWMutex，无需任何外部依赖，用于演示 / 本地开发 / 测试
//
// 上层（api / task / auth / scheduler）依赖本文件暴露的窄接口，
// 不关心底层到底是哪种实现。
package store

import (
	"context"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// Store 聚合 5 个仓储。字段是接口类型，因此 postgres 与 memory 后端可互换。
type Store struct {
	// db 仅在 postgres 模式下非 nil；memory 模式下为 nil。
	db *database.DB

	Users     UserStore
	Workflows WorkflowStore
	Tasks     TaskStore
	Knowledge KnowledgeStore
	Providers ProviderStore
}

// New 构造基于 PostgreSQL 的 Store。
func New(db *database.DB) *Store {
	return &Store{
		db:        db,
		Users:     &UserRepo{db: db},
		Workflows: &WorkflowRepo{db: db},
		Tasks:     &TaskRepo{db: db},
		Knowledge: &KnowledgeRepo{db: db},
		Providers: &ProviderRepo{db: db},
	}
}

// NewMemory 构造纯内存 Store（无需 PostgreSQL）。
// 见 memory.go。返回 *MemoryStore，其中也内嵌了 *Store 便于直接注入。
func NewMemory() *MemoryStore {
	return newMemoryStore()
}

// DB 返回底层数据库句柄；memory 模式下为 nil。
// 调用方必须先判空（例如 API 层判断是否挂载 /metrics 之外的 DB 依赖能力）。
func (s *Store) DB() *database.DB { return s.db }

// HasDB 报告当前是否为数据库后端。
func (s *Store) HasDB() bool { return s != nil && s.db != nil }

var _ = context.Background

// ensureModel 仅用于编译期确认 model 包被引用（避免未使用导入）。
var _ = model.NodeInput

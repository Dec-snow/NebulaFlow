// Package memory 实现 Agent Memory 管理系统。
//
// 架构：
//
//	Agent
//	  │
//	MemoryManager  ← 统一入口
//	  │
//	┌─┴───────────┐
//	│             │
//	ShortTerm    LongTerm
//	(Redis)      (pgvector)
//	会话上下文    用户偏好/历史任务
//
// 设计理念：
//   - 短期记忆：对话历史，Redis List 存，TTL 自动过期，读快写快
//   - 长期记忆：重要信息、用户偏好、历史任务摘要，pgvector 存，语义检索
//   - MemoryManager：对外统一接口，Agent 不需要知道底层是什么
//
// 为什么 Memory 重要：
//   没有记忆的 Agent = 每次对话都是"第一次见面"
//   有记忆的 Agent = 能记住你是谁、你喜欢什么、上次聊了什么
//
// 简历卖点：
//   > 实现 Agent Memory 管理机制，支持会话上下文保持与长期知识检索。
package memory

import (
	"context"
	"fmt"
	"time"
)

// MemoryMessage 是一条记忆消息。
type MemoryMessage struct {
	Role      string    `json:"role"`      // user / assistant / system / tool
	Content   string    `json:"content"`   // 消息内容
	Timestamp time.Time `json:"timestamp"` // 时间戳
	// Metadata 是附加信息（如 tool_call_id、token 数等）。
	Metadata map[string]string `json:"metadata,omitempty"`
}

// MemoryItem 是长期记忆的一条记录。
type MemoryItem struct {
	ID        string            `json:"id"`
	UserID    int64             `json:"user_id"`
	Content   string            `json:"content"`    // 记忆内容
	Category  string            `json:"category"`   // 分类：preference / fact / task_summary
	Source    string            `json:"source"`     // 来源：conversation / explicit / import
	Score     float64           `json:"score"`      // 检索时的相似度分数
	Timestamp time.Time         `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// ShortTermStore 是短期记忆（会话上下文）的存储接口。
//
// 典型实现：Redis List + TTL
// 特点：快、有过期时间、按会话隔离
type ShortTermStore interface {
	// Append 追加一条消息到会话记忆。
	Append(ctx context.Context, sessionID string, msg MemoryMessage) error
	// GetAll 获取会话的全部消息（按时间顺序）。
	GetAll(ctx context.Context, sessionID string) ([]MemoryMessage, error)
	// GetLastN 获取最近的 N 条消息。
	GetLastN(ctx context.Context, sessionID string, n int) ([]MemoryMessage, error)
	// Clear 清空会话记忆。
	Clear(ctx context.Context, sessionID string) error
	// SetTTL 设置会话的过期时间（多久不活动就清除）。
	SetTTL(ctx context.Context, sessionID string, ttl time.Duration) error
}

// LongTermStore 是长期记忆（用户知识）的存储接口。
//
// 典型实现：PostgreSQL + pgvector
// 特点：持久化、语义检索、按用户隔离
type LongTermStore interface {
	// Store 保存一条长期记忆。
	Store(ctx context.Context, item MemoryItem, embedding []float32) error
	// Search 按语义相似度检索记忆。
	Search(ctx context.Context, userID int64, queryEmbedding []float32, topK int) ([]MemoryItem, error)
	// Delete 删除一条记忆。
	Delete(ctx context.Context, id string) error
	// ListByCategory 按分类列出用户的记忆。
	ListByCategory(ctx context.Context, userID int64, category string, limit int) ([]MemoryItem, error)
}

// EmbedFunc 是生成文本向量的函数类型。
// 由调用方注入（避免 memory 包直接依赖 llm 包造成循环依赖）。
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

// Manager 是 Memory 的统一入口。
//
// Agent 只跟 Manager 打交道，不需要知道底层是 Redis 还是 pgvector。
// Manager 负责：
//   - 读写短期记忆（对话历史）
//   - 检索长期记忆（用户偏好、历史知识）
//   - 决定哪些记忆该注入到 Prompt 里
type Manager struct {
	shortTerm ShortTermStore
	longTerm  LongTermStore
	embed     EmbedFunc
	// DefaultTTL 是短期记忆的默认过期时间。
	DefaultTTL time.Duration
	// MaxContextMessages 是注入到 Prompt 的最大历史消息数。
	MaxContextMessages int
	// MaxLongTermItems 是注入到 Prompt 的最大长期记忆数。
	MaxLongTermItems int
}

// ManagerConfig 是 Manager 的配置。
type ManagerConfig struct {
	ShortTerm          ShortTermStore
	LongTerm           LongTermStore
	Embed              EmbedFunc
	DefaultTTL         time.Duration
	MaxContextMessages int
	MaxLongTermItems   int
}

// NewManager 创建一个 Memory Manager。
func NewManager(cfg ManagerConfig) *Manager {
	ttl := cfg.DefaultTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	maxCtx := cfg.MaxContextMessages
	if maxCtx == 0 {
		maxCtx = 20
	}
	maxLT := cfg.MaxLongTermItems
	if maxLT == 0 {
		maxLT = 5
	}
	return &Manager{
		shortTerm:          cfg.ShortTerm,
		longTerm:           cfg.LongTerm,
		embed:              cfg.Embed,
		DefaultTTL:         ttl,
		MaxContextMessages: maxCtx,
		MaxLongTermItems:   maxLT,
	}
}

// AppendMessage 追加一条消息到短期记忆。
func (m *Manager) AppendMessage(ctx context.Context, sessionID string, msg MemoryMessage) error {
	if m.shortTerm == nil {
		return nil
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	if err := m.shortTerm.Append(ctx, sessionID, msg); err != nil {
		return err
	}
	// 刷新 TTL
	return m.shortTerm.SetTTL(ctx, sessionID, m.DefaultTTL)
}

// GetContext 获取会话上下文（最近 N 条消息）。
func (m *Manager) GetContext(ctx context.Context, sessionID string) ([]MemoryMessage, error) {
	if m.shortTerm == nil {
		return nil, nil
	}
	return m.shortTerm.GetLastN(ctx, sessionID, m.MaxContextMessages)
}

// ClearContext 清空会话上下文。
func (m *Manager) ClearContext(ctx context.Context, sessionID string) error {
	if m.shortTerm == nil {
		return nil
	}
	return m.shortTerm.Clear(ctx, sessionID)
}

// SaveLongTerm 保存一条长期记忆。
func (m *Manager) SaveLongTerm(ctx context.Context, item MemoryItem) error {
	if m.longTerm == nil || m.embed == nil {
		return nil
	}
	embedding, err := m.embed(ctx, item.Content)
	if err != nil {
		return err
	}
	return m.longTerm.Store(ctx, item, embedding)
}

// Recall 从长期记忆中检索相关内容。
// query 是当前的问题/上下文，返回最相关的若干条记忆。
func (m *Manager) Recall(ctx context.Context, userID int64, query string) ([]MemoryItem, error) {
	if m.longTerm == nil || m.embed == nil {
		return nil, nil
	}
	embedding, err := m.embed(ctx, query)
	if err != nil {
		return nil, err
	}
	return m.longTerm.Search(ctx, userID, embedding, m.MaxLongTermItems)
}

// BuildSystemPrompt 把长期记忆注入到系统提示词里。
//
// 这是 Memory 系统最核心的"用法"：Agent 每次推理前，
// 从长期记忆里检索相关内容，拼到系统提示词中，
// 让 Agent"好像记得"用户的偏好和历史。
func (m *Manager) BuildSystemPrompt(ctx context.Context, userID int64, query string, baseSystem string) (string, error) {
	memories, err := m.Recall(ctx, userID, query)
	if err != nil || len(memories) == 0 {
		return baseSystem, nil
	}

	// 把记忆格式化成提示词
	var memText string
	memText += "\n\n【关于用户的记忆】\n"
	for i, mem := range memories {
		memText += fmt.Sprintf("%d. [%s] %s\n", i+1, mem.Category, mem.Content)
	}
	memText += "\n请结合以上记忆来回答用户的问题。"

	if baseSystem == "" {
		return memText, nil
	}
	return baseSystem + memText, nil
}

// Package model 定义 NebulaFlow 的核心领域模型。
// 这些结构体与 PostgreSQL 表一一对应（见 migrations/001_init.sql）。
package model

import (
	"encoding/json"
	"time"
)

// ---------- 用户与认证 ----------

type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ---------- Workflow ----------

// NodeType 定义工作流节点类型。
type NodeType string

const (
	NodeInput     NodeType = "input"
	NodeLLM       NodeType = "llm"
	NodeRAG       NodeType = "rag"
	NodeTool      NodeType = "tool"
	NodeCondition NodeType = "condition"
	NodeOutput    NodeType = "output"
)

// TaskStatus 定义任务状态机。
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

type WorkflowStatus string

const (
	WorkflowDraft     WorkflowStatus = "draft"
	WorkflowPublished WorkflowStatus = "published"
)

type Workflow struct {
	ID          int64          `json:"id"`
	UserID      int64          `json:"user_id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Status      WorkflowStatus `json:"status"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Nodes       []WorkflowNode `json:"nodes,omitempty"`
	Edges       []WorkflowEdge `json:"edges,omitempty"`
}

// NodeConfig 是节点配置的通用容器（模型/提示词/工具名等）。
type NodeConfig struct {
	Model           string         `json:"model,omitempty"`
	Prompt          string         `json:"prompt,omitempty"`
	System          string         `json:"system,omitempty"`
	Tool            string         `json:"tool,omitempty"`
	KnowledgeBaseID int64          `json:"knowledge_base_id,omitempty"`
	MaxRetry        int            `json:"max_retry,omitempty"`
	TimeoutSec      int            `json:"timeout_sec,omitempty"`
	Extra           map[string]any `json:"extra,omitempty"`
}

func (c *NodeConfig) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}

type WorkflowNode struct {
	ID         int64      `json:"id"`
	WorkflowID int64      `json:"workflow_id"`
	NodeKey    string     `json:"node_key"` // 前端稳定标识，如 "analyst"
	NodeType   NodeType   `json:"node_type"`
	Config     NodeConfig `json:"config"`
	PositionX  float64    `json:"position_x"`
	PositionY  float64    `json:"position_y"`
	CreatedAt  time.Time  `json:"created_at"`
}

type WorkflowEdge struct {
	ID         int64     `json:"id"`
	WorkflowID int64     `json:"workflow_id"`
	SourceNode string    `json:"source_node"` // node_key
	TargetNode string    `json:"target_node"` // node_key
	CreatedAt  time.Time `json:"created_at"`
}

// ---------- 任务执行 ----------

type Task struct {
	ID         int64      `json:"id"`
	WorkflowID int64      `json:"workflow_id"`
	UserID     int64      `json:"user_id"`
	Status     TaskStatus `json:"status"`
	Input      string     `json:"input"`
	Output     string     `json:"output"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	// 运行时字段（不入库）
	Nodes []TaskNode `json:"nodes,omitempty"`
}

type TaskNodeStatus string

const (
	NodePending   TaskNodeStatus = "pending"
	NodeRunning   TaskNodeStatus = "running"
	NodeSucceeded TaskNodeStatus = "succeeded"
	NodeFailed    TaskNodeStatus = "failed"
	NodeSkipped   TaskNodeStatus = "skipped"
	// NodeCancelled：任务被取消时，未执行完的节点统一收口到这个状态。
	// 没有它，取消后下游节点会永远停在 pending，前端看到"任务已取消但节点还在排队"的自相矛盾状态。
	NodeCancelled TaskNodeStatus = "cancelled"
)

type TaskNode struct {
	ID         int64          `json:"id"`
	TaskID     int64          `json:"task_id"`
	NodeID     int64          `json:"node_id"`
	NodeKey    string         `json:"node_key"`
	NodeType   NodeType       `json:"node_type"`
	Status     TaskNodeStatus `json:"status"`
	Input      string         `json:"input,omitempty"`
	Output     string         `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Retries    int            `json:"retries"`
	TokensIn   int            `json:"tokens_in,omitempty"`
	TokensOut  int            `json:"tokens_out,omitempty"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
}

// TaskLog 记录节点执行过程中的日志（含 LLM token 流、错误、重试）。
type TaskLog struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	NodeKey   string    `json:"node_key,omitempty"`
	Level     string    `json:"level"` // info / warn / error / token
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// ---------- 知识库 / RAG ----------

type KnowledgeBase struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type DocumentStatus string

const (
	DocPending DocumentStatus = "pending"
	DocIndexed DocumentStatus = "indexed"
	DocFailed  DocumentStatus = "failed"
)

type Document struct {
	ID              int64          `json:"id"`
	KnowledgeBaseID int64          `json:"knowledge_base_id"`
	Filename        string         `json:"filename"`
	Status          DocumentStatus `json:"status"`
	Content         string         `json:"content,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
}

// DocumentChunk 是向量检索的基本单元。
//
// 向量存两处，各有明确职责：
//   - embedding_v：pgvector 的 vector 列，检索走 HNSW 索引（`ORDER BY embedding_v <=> $1`）；
//   - Embedding：进程内的 float64 切片，只在写入路径与内存后端使用，不作为查询依据。
//
// 检索时数据库只回传 Top-K 行，因此 Embedding 在检索结果里通常为空——
// 这正是本次改造的目的：不再把整个知识库的分块拉进进程算余弦。
type DocumentChunk struct {
	ID         int64     `json:"id"`
	DocumentID int64     `json:"document_id"`
	Content    string    `json:"content"`
	Embedding  []float64 `json:"embedding,omitempty"`
	ChunkIndex int       `json:"chunk_index"`
	CreatedAt  time.Time `json:"created_at"`
	// Filename 只在检索时填充（联表查出），用于前端展示引用来源。
	Filename string `json:"filename,omitempty"`
	// Score 是检索相似度（余弦，越大越相似），只在检索结果里填充。
	// 由数据库侧计算（1 - 余弦距离），保证与应用层实现的量纲一致。
	Score float64 `json:"score,omitempty"`
}

// ---------- LLM ----------

type LLMProvider struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"` // deepseek / openai / ollama / mimo
	BaseURL   string    `json:"base_url"`
	APIKey    string    `json:"api_key,omitempty"`
	IsDefault bool      `json:"is_default"`
	Enabled   bool      `json:"enabled"`
	Priority  int       `json:"priority"` // fallback 顺序，越小越优先
	CreatedAt time.Time `json:"created_at"`
}

type LLMModel struct {
	ID         int64     `json:"id"`
	ProviderID int64     `json:"provider_id"`
	Name       string    `json:"name"` // deepseek-chat / gpt-4o-mini / qwen2.5 / ...
	MaxTokens  int       `json:"max_tokens"`
	CreatedAt  time.Time `json:"created_at"`
}

// ---------- 用量 ----------

type UsageRecord struct {
	ID           int64     `json:"id"`
	UserID       int64     `json:"user_id"`
	TaskID       int64     `json:"task_id"`
	NodeKey      string    `json:"node_key,omitempty"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	LatencyMS    int64     `json:"latency_ms"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

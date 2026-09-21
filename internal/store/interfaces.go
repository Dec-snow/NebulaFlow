package store

import (
	"context"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// 本文件定义 5 个仓储接口。
//
// 设计意图：让上层（api / task / auth / scheduler）依赖"接口"而不是"具体的
// PostgreSQL 实现"，从而获得两个能力：
//
//  1. MemoryMode —— STORAGE_MODE=memory 时装配纯内存实现，
//     无需 PostgreSQL / Redis 即可启动完整服务（演示、本地开发、面试现场）。
//  2. 可测试性 —— 单测可用内存实现跑通整条链路，不必起数据库。
//
// 接口只暴露真实被调用的方法，不做"预留式"设计。
// 具体实现（*UserRepo 等）在各自文件中，编译期用断言确保满足接口。

// UserStore 用户仓储。
type UserStore interface {
	CreateUser(ctx context.Context, u *model.User) error
	GetUserByUsername(ctx context.Context, username string) (*model.User, error)
	GetUserByID(ctx context.Context, id int64) (*model.User, error)
}

// WorkflowStore 工作流仓储（含节点与边）。
type WorkflowStore interface {
	CreateWorkflow(ctx context.Context, wf *model.Workflow) error
	UpdateWorkflow(ctx context.Context, wf *model.Workflow) error
	DeleteWorkflow(ctx context.Context, id, userID int64) error
	GetWorkflow(ctx context.Context, id, userID int64) (*model.Workflow, error)
	ListWorkflows(ctx context.Context, userID int64) ([]model.Workflow, error)
	GetWorkflowNodes(ctx context.Context, workflowID int64) ([]model.WorkflowNode, error)
	GetWorkflowEdges(ctx context.Context, workflowID int64) ([]model.WorkflowEdge, error)
}

// TaskStore 任务仓储（任务 / 节点 / 日志 / 用量）。
type TaskStore interface {
	CreateTask(ctx context.Context, t *model.Task) error
	GetTask(ctx context.Context, id, userID int64) (*model.Task, error)
	GetTaskByID(ctx context.Context, id int64) (*model.Task, error)
	ListTasks(ctx context.Context, userID int64, limit, offset int) ([]model.Task, error)
	UpdateTaskStatus(ctx context.Context, id int64, status model.TaskStatus, output, errMsg string) error

	GetTaskNodes(ctx context.Context, taskID int64) ([]model.TaskNode, error)
	CreateTaskNodes(ctx context.Context, taskID int64, nodes []model.WorkflowNode) error
	CountTaskNodes(ctx context.Context, taskID int64) (int, error)
	UpdateTaskNode(ctx context.Context, n *model.TaskNode) error

	AppendLog(ctx context.Context, l *model.TaskLog) error
	ListLogs(ctx context.Context, taskID int64, limit int) ([]model.TaskLog, error)

	RecordUsage(ctx context.Context, u *model.UsageRecord) error
	UsageSummary(ctx context.Context) (totalInput, totalOutput, totalRequests int64, perProvider []ProviderUsage, err error)
}

// KnowledgeStore 知识库仓储（知识库 / 文档 / 分块）。
type KnowledgeStore interface {
	CreateKB(ctx context.Context, kb *model.KnowledgeBase) error
	ListKBs(ctx context.Context, userID int64) ([]model.KnowledgeBase, error)
	DeleteKB(ctx context.Context, id, userID int64) error
	BelongsToUser(ctx context.Context, kbID, userID int64) (bool, error)

	CreateDocument(ctx context.Context, d *model.Document) error
	ListDocuments(ctx context.Context, kbID int64) ([]model.Document, error)
	GetDocument(ctx context.Context, docID int64) (*model.Document, error)
	UpdateDocumentStatus(ctx context.Context, docID int64, status model.DocumentStatus) error

	DeleteChunksByDocument(ctx context.Context, docID int64) error
	InsertChunk(ctx context.Context, c *model.DocumentChunk) error
	BatchInsertChunks(ctx context.Context, chunks []model.DocumentChunk) error
	// SearchChunks 在知识库内做向量 Top-K 检索：传入查询向量，返回最相似的 k 个分块
	// （按相似度降序，Score 为余弦相似度）。
	//
	// 注意这里没有"列出全部分块"的方法——这是刻意的。
	// 原签名 SearchChunks(ctx, kbID) 会把整个知识库拉进进程，见优化任务清单 P0-1。
	SearchChunks(ctx context.Context, kbID int64, queryVec []float64, k int) ([]model.DocumentChunk, error)
}

// ProviderStore LLM Provider / Model 仓储。
type ProviderStore interface {
	UpsertProvider(ctx context.Context, p *model.LLMProvider) error
	ListProviders(ctx context.Context) ([]model.LLMProvider, error)
	GetProvider(ctx context.Context, id int64) (*model.LLMProvider, error)
	DeleteProvider(ctx context.Context, id int64) error

	ListModels(ctx context.Context, providerID int64) ([]model.LLMModel, error)
	UpsertModel(ctx context.Context, m *model.LLMModel) error
}

// 编译期断言：PostgreSQL 实现必须满足对应接口。
// 这样一旦实现与接口漂移，编译立刻失败，而不是等运行时。
var (
	_ UserStore      = (*UserRepo)(nil)
	_ WorkflowStore  = (*WorkflowRepo)(nil)
	_ TaskStore      = (*TaskRepo)(nil)
	_ KnowledgeStore = (*KnowledgeRepo)(nil)
	_ ProviderStore  = (*ProviderRepo)(nil)
)

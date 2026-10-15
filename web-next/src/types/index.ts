/**
 * 领域类型定义。
 *
 * 命名约定：前端一律 camelCase（API 层负责 snake_case → camelCase）。
 *
 * 字段可用性约定（重要）：
 *   `?` 标注的字段表示**后端不一定下发**。Go 侧的 json tag 大量使用
 *   `omitempty`，列表接口也不会 join 出关联数据（例如 workflow 列表不带
 *   nodes/edges）。凡是后端没有的指标，要么按需拉详情算出来，要么由 UI
 *   隐藏 —— 不允许编造，也不允许渲染成 undefined / NaN。
 */

/* ============================================================
 *  通用
 * ========================================================== */

export type Tone = "brand" | "mint" | "sky" | "violet" | "rose" | "amber";

export interface ListResponse<T> {
  items: T[];
  total: number;
  page: number;
  pageSize: number;
}

export interface PaginationParams {
  page?: number;
  pageSize?: number;
}

/* ============================================================
 *  认证
 * ========================================================== */

export interface LoginPayload {
  username: string;
  password: string;
}

export interface RegisterPayload extends LoginPayload {
  email: string;
}

/** POST /api/auth/{login,register} 的响应体（原样 camelize 后）。 */
export interface AuthResult {
  token: string;
  username: string;
  userId: number | null;
}

/* ============================================================
 *  KPI & 指标
 * ========================================================== */

export interface Kpi {
  key: string;
  label: string;
  value: number | null;
  unit?: string;
  /** 环比变化百分比。后端不提供时间序列聚合时为 undefined（UI 隐藏该行） */
  delta?: number;
  /** "up" 表示上升是好事，"down" 表示下降是好事 */
  deltaGood?: "up" | "down";
  tone: Tone;
  /** 迷你趋势数据；没有真实序列时留空，不编造 */
  spark?: number[];
}

/* ============================================================
 *  工作流
 * ========================================================== */

export type WorkflowStatus = "published" | "draft" | "archived";

export interface WorkflowSummary {
  id: number;
  name: string;
  description: string;
  status: WorkflowStatus;
  createdAt?: string;
  updatedAt?: string;
  /**
   * 下列字段**不在 workflow 列表接口里**：`model.Workflow` 的 nodes/edges 是
   * `omitempty`，也没有 node_count / edge_count / runs / success_rate。
   * API 层会为列表前若干条拉详情补齐 nodeCount/edgeCount/mini；runs 与
   * successRate 后端根本没有对应数据，始终为 undefined，UI 必须隐藏。
   */
  nodeCount?: number;
  edgeCount?: number;
  runs?: number;
  successRate?: number;
  avgDurationMs?: number;
  /** 缩略图节点位置（0–100 的相对坐标），由详情里的 position_x/y 归一化得到 */
  mini?: { id: string; x: number; y: number }[];
  miniEdges?: [string, string][];
  /** 模板字段（is_template=true 的记录） */
  category?: string;
  icon?: string;
  isTemplate?: boolean;
}

export type NodeKind = "input" | "output" | "llm" | "tool" | "rag" | "condition";

export interface EditorNode {
  id: string;
  type: NodeKind;
  x: number;
  y: number;
  config: NodeConfig;
}

export interface NodeConfig {
  // 通用
  label?: string;
  maxRetry?: number;
  timeoutSec?: number;

  // LLM
  model?: string;
  system?: string;
  prompt?: string;
  temperature?: number;

  // RAG
  knowledgeBaseId?: number;
  topK?: number;
  similarityThreshold?: number;

  // Tool
  tool?: string;

  // Condition
  expression?: string;

  /** 后端 NodeConfig.Extra：前端扩展键在保存时统一塞进这里，避免被后端丢弃 */
  extra?: Record<string, unknown>;
}

/** 工作流详情：GET /api/workflows/:id（含 nodes / edges）。 */
export interface WorkflowDetail {
  id: number;
  name: string;
  description: string;
  status: WorkflowStatus;
  updatedAt?: string;
  nodes: EditorNode[];
  edges: [string, string][];
}

/** 创建工作流 / 保存工作流的请求体。 */
export interface WorkflowPayload {
  name: string;
  description?: string;
  status?: WorkflowStatus;
  nodes: EditorNode[];
  edges: [string, string][];
}

/* ============================================================
 *  任务
 * ========================================================== */

export type TaskStatus =
  | "pending"
  | "running"
  | "waiting"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "skipped";

export interface TaskRow {
  id: number;
  workflow: string;
  workflowId: number;
  status: TaskStatus;
  /** started_at → finished_at 的差值；未跑完时为 undefined */
  durationMs?: number;
  /** 后端 task 上没有 token 汇总字段（只有 task_node 才有），因此通常为 undefined */
  tokens?: number;
  startedAt?: string;
  createdAt?: string;
  input: string;
  output?: string;
  error?: string;
  priority?: string;
  nodes: TaskNodeRow[];
}

export interface TaskNodeRow {
  key: string;
  type: NodeKind;
  status: TaskStatus;
  durationMs?: number;
  tokensIn?: number;
  tokensOut?: number;
  /** 已重试次数（后端 TaskNode.retries，非 omitempty） */
  retries?: number;
  input?: string;
  output?: string;
  message?: string;
}

/** 任务日志：GET /api/tasks/:id/logs */
export interface TaskLog {
  id: number;
  nodeKey?: string;
  level: string;
  message: string;
  createdAt?: string;
}

/** 执行时间线：GET /api/tasks/:id/timeline */
export interface TaskTimeline {
  taskId: number;
  status: string;
  totalMs?: number;
  startedAt?: string;
  finishedAt?: string;
  nodes: TaskTimelineNode[];
  tokenUsage: { input?: number; output?: number; total?: number; nodeCount?: number };
  toolCalls?: number;
}

export interface TaskTimelineNode {
  nodeKey: string;
  nodeType: string;
  status: string;
  startMs?: number;
  durationMs?: number;
  tokensIn?: number;
  tokensOut?: number;
  error?: string;
  events?: { type: string; name?: string; offsetMs?: number; durationMs?: number; detail?: string }[];
}

/* ============================================================
 *  Provider / 模型
 * ========================================================== */

export type ProviderStatus = "enabled" | "degraded" | "disabled";

export interface ProviderModel {
  id?: number;
  name: string;
  maxTokens?: number;
}

export interface Provider {
  id: number;
  name: string;
  baseUrl: string;
  /** 由后端的 enabled 布尔值映射而来（enabled ? "enabled" : "disabled"） */
  status: ProviderStatus;
  priority?: number;
  /** 后端 LLMProvider 没有 latency_ms：不编造，UI 显示占位符 */
  latencyMs?: number;
  isDefault: boolean;
  models: ProviderModel[];
}

export interface ProviderPayload {
  name: string;
  baseUrl: string;
  apiKey?: string;
  priority?: number;
}

/* ============================================================
 *  知识库
 * ========================================================== */

export type KbDocStatus = "pending" | "indexing" | "indexed" | "failed";

export interface KbDoc {
  id: number;
  filename: string;
  status: KbDocStatus;
  chunks?: number;
  sizeKb?: number;
  errorMessage?: string;
  createdAt?: string;
}

export interface KnowledgeBase {
  id: number;
  name: string;
  description: string;
  createdAt?: string;
  /** 文档明细来自 GET /api/knowledge-bases/:id/documents，列表接口不下发 */
  docs?: KbDoc[];
}

export interface RetrievalHit {
  id: number;
  /** 后端 DocumentChunk 的来源字段就叫 filename（不是 source） */
  filename?: string;
  score?: number;
  content: string;
}

/* ============================================================
 *  活动流
 * ========================================================== */

export type ActivityKind = "task" | "node" | "system" | "error";

export interface ActivityItem {
  id: string;
  at: string;
  kind: ActivityKind;
  text: string;
  meta?: string;
}

/* ============================================================
 *  用量 / 成本
 * ========================================================== */

export interface ProviderUsage {
  provider: string;
  totalRequests?: number;
  inputTokens?: number;
  outputTokens?: number;
}

export interface UsageSummary {
  totalInputTokens?: number;
  totalOutputTokens?: number;
  totalRequests?: number;
  perProvider: ProviderUsage[];
  timestamp?: string;
}

export interface WorkflowUsage {
  id: number;
  name: string;
  tasks: number;
}

/* ============================================================
 *  Dashboard
 * ========================================================== */

export interface DashboardStats {
  queueLength?: number;
  workerQueued?: number;
  activeWorkers?: number;
  totalWorkers?: number;
  runningTasks?: number;
  sseSubscribers?: number;
  sseUsers?: number;
  sseDropped?: number;
  timestamp?: string;
}

export interface WorkerStats {
  total?: number;
  active?: number;
  queued?: number;
  utilization?: number;
  queueCapacity?: number;
}

/* ============================================================
 *  Agent
 * ========================================================== */

export type AgentRuntimeType = "native" | "langchain" | "http";
export type AgentStatus = "active" | "inactive" | "error";

export interface Agent {
  id: number;
  userId: number;
  name: string;
  description: string;
  runtimeType: AgentRuntimeType;
  endpoint?: string;
  model?: string;
  capabilities: string[];
  status: AgentStatus;
  version: string;
  timeoutSec?: number;
  createdAt?: string;
  updatedAt?: string;
}

export interface AgentMarketplaceResponse {
  items: Agent[];
  installedNames: string[];
}

/** GET /api/agents/:id/health */
export interface AgentHealth {
  agentId: number;
  healthy: boolean;
  error?: string;
  metadata?: Record<string, unknown>;
}

/* ============================================================
 *  工作流模板
 * ========================================================== */

export interface WorkflowTemplate {
  id: number;
  name: string;
  description: string;
  category: string;
  icon?: string;
  /** 后端模板列表不下发节点数 / 使用次数，缺失时 UI 隐藏 */
  nodes?: number;
  usedCount?: number;
  createdAt?: string;
}

/* ============================================================
 *  Prompt 模板
 * ========================================================== */

export type PromptCategory = "通用" | "分析" | "写作" | "翻译" | "代码";

export interface PromptTemplate {
  id: string;
  name: string;
  category: PromptCategory;
  prompt: string;
  system?: string;
}

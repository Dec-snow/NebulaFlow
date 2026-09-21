/**
 * 领域类型定义。
 *
 * 所有核心数据结构集中在这里，前后端共享同一套类型契约。
 * mock 数据和 API 返回值都应该实现这些接口。
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
 *  KPI & 指标
 * ========================================================== */

export interface Kpi {
  key: string;
  label: string;
  value: number;
  unit?: string;
  /** 环比变化百分比，正数为上升，负数为下降 */
  delta: number;
  /** "up" 表示上升是好事，"down" 表示下降是好事 */
  deltaGood: "up" | "down";
  tone: Tone;
  spark: number[];
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
  nodes: number;
  edges: number;
  runs: number;
  successRate: number;
  avgDurationMs: number;
  updatedAt: string;
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
}

/* ============================================================
 *  任务
 * ========================================================== */

export type TaskStatus = "pending" | "running" | "succeeded" | "failed" | "cancelled";

export interface TaskRow {
  id: number;
  workflow: string;
  workflowId: number;
  status: TaskStatus;
  durationMs: number;
  totalTokens: number;
  startedAt: string;
  input: string;
}

export interface TaskNodeRow {
  key: string;
  type: NodeKind;
  status: TaskStatus;
  durationMs: number;
  tokensIn: number;
  tokensOut: number;
  message?: string;
}

/* ============================================================
 *  Provider / 模型
 * ========================================================== */

export type ProviderStatus = "enabled" | "degraded" | "disabled";

export interface ProviderModel {
  name: string;
  maxTokens: number;
}

export interface Provider {
  id: number;
  name: string;
  baseUrl: string;
  status: ProviderStatus;
  priority: number;
  latencyMs: number;
  isDefault: boolean;
  models: ProviderModel[];
}

/* ============================================================
 *  知识库
 * ========================================================== */

export interface KnowledgeBase {
  id: number;
  name: string;
  docs: number;
  chunks: number;
  updatedAt: string;
}

export interface RetrievalHit {
  id: string;
  source: string;
  score: number;
  snippet: string;
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
 *  成本分析
 * ========================================================== */

export interface CostByModel {
  model: string;
  provider: string;
  tokensIn: number;
  tokensOut: number;
  cost: number;
  calls: number;
}

export interface CostByWorkflow {
  id: number;
  name: string;
  cost: number;
  tasks: number;
  avgCost: number;
  trend: number;
}

export interface ModelPricing {
  input: number;
  output: number;
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

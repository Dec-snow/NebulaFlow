/**
 * API 服务层：统一数据获取入口。
 *
 * 设计原则：
 * 1. 所有页面通过 import { api } from "@/api" 获取数据，不直接 import mock
 * 2. 通过 VITE_USE_MOCK 环境变量切换真实 API / Mock API
 * 3. 所有方法返回 Promise，模拟真实网络请求延迟
 * 4. 类型定义集中在 @/types，前后端联调时只改实现不改类型
 */

import {
  KPIS,
  THROUGHPUT,
  THROUGHPUT_LABELS,
  TOKEN_BY_PROVIDER,
  WORKER_STATS,
  ACTIVITY,
  WORKFLOWS,
  TASKS,
  PROVIDERS,
  KNOWLEDGE_BASES,
  RETRIEVE_HITS,
  EDITOR_NODES,
  EDITOR_EDGES,
  NODE_SETS,
  AGENTS,
  MARKETPLACE_AGENTS,
  WORKFLOW_TEMPLATES,
  COST_BY_MODEL,
  COST_BY_WORKFLOW,
  COST_TREND,
  COST_TREND_LABELS,
} from "@/lib/mock";

import type {
  WorkflowSummary,
  TaskRow,
  Provider,
  KnowledgeBase,
  ActivityItem,
  Kpi,
  EditorNode,
  TaskNodeRow,
  Agent,
  WorkflowTemplate,
  PaginationParams,
  ListResponse,
  RetrievalHit,
  CostByModel,
  CostByWorkflow,
} from "@/types";

export type { PaginationParams, ListResponse } from "@/types";

import { includesFold } from "@/lib/utils";

/* ============================================================
 *  响应转换工具：snake_case → camelCase
 * ========================================================== */

/**
 * 将 snake_case 键名转为 camelCase。
 * 后端（Go）通常返回 snake_case，前端统一用 camelCase，
 * 在 API 层做一次转换，组件层无需感知命名差异。
 */
function camelize(str: string): string {
  return str.replace(/_([a-z])/g, (_, c) => c.toUpperCase());
}

/** 递归转换对象的所有 snake_case 键为 camelCase。 */
function camelizeKeys<T = unknown>(obj: unknown): T {
  if (Array.isArray(obj)) {
    return obj.map((item) => camelizeKeys(item)) as unknown as T;
  }
  if (obj !== null && typeof obj === "object") {
    const result: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(obj as Record<string, unknown>)) {
      result[camelize(key)] = camelizeKeys(value);
    }
    return result as T;
  }
  return obj as T;
}

/* ============================================================
 * 复合数据类型
 * ========================================================== */

export interface DashboardData {
  kpis: Kpi[];
  throughput: number[];
  throughputLabels: string[];
  workerStats: typeof WORKER_STATS;
  tokenByProvider: typeof TOKEN_BY_PROVIDER;
  activity: ActivityItem[];
  recentTasks: TaskRow[];
}

export interface CostData {
  byModel: CostByModel[];
  byWorkflow: CostByWorkflow[];
  trend: number[];
  trendLabels: string[];
}

export interface AgentListParams extends PaginationParams {
  runtimeType?: string;
  q?: string;
}

export interface AgentMarketplaceResponse {
  items: Agent[];
  installedNames: string[];
}

export interface TemplateListParams extends PaginationParams {
  category?: string;
  q?: string;
}

/* ============================================================
 * Mock 实现（带延迟）
 * ========================================================== */

const MOCK_DELAY = 300; // mock 延迟 ms，模拟网络请求

function delay<T>(data: T, ms = MOCK_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(data), ms));
}

/* ------------------------------------------------ Dashboard */

async function getDashboard(range: string = "24h"): Promise<DashboardData> {
  // 不同 range 有不同的数据（简单模拟）
  const multiplier = range === "7d" ? 7 : range === "30d" ? 30 : 1;
  return delay({
    kpis: KPIS.map((k) => ({ ...k, value: k.value * multiplier })),
    throughput: THROUGHPUT,
    throughputLabels: THROUGHPUT_LABELS,
    workerStats: WORKER_STATS,
    tokenByProvider: TOKEN_BY_PROVIDER,
    activity: ACTIVITY,
    recentTasks: TASKS.slice(0, 5),
  });
}

/* ------------------------------------------------ Workflows */

async function listWorkflows(
  params: { status?: string; q?: string } & PaginationParams = {},
): Promise<ListResponse<WorkflowSummary>> {
  const { status, q, page = 1, pageSize = 10 } = params;
  let list = WORKFLOWS.filter(
    (w) =>
      (!status || w.status === status) && includesFold(w.name, q),
  );
  const total = list.length;
  const start = (page - 1) * pageSize;
  list = list.slice(start, start + pageSize);
  return delay({ items: list, total, page, pageSize });
}

async function getWorkflow(id: number): Promise<{
  nodes: EditorNode[];
  edges: [string, string][];
  meta: WorkflowSummary;
} | null> {
  const meta = WORKFLOWS.find((w) => w.id === id) ?? null;
  if (!meta) return delay(null);
  return delay({
    nodes: EDITOR_NODES,
    edges: EDITOR_EDGES,
    meta,
  });
}

async function saveWorkflow(
  id: number,
  data: { nodes: EditorNode[]; edges: [string, string][] },
): Promise<{ success: boolean }> {
  console.log("[api] save workflow", id, data);
  return delay({ success: true }, 500);
}

/* ------------------------------------------------ Tasks */

async function listTasks(
  params: { status?: string; q?: string } & PaginationParams = {},
): Promise<ListResponse<TaskRow>> {
  const { status, q, page = 1, pageSize = 10 } = params;
  let list = TASKS.filter(
    (t) =>
      (!status || t.status === status) &&
      (!q || String(t.id).includes(q) || includesFold(t.workflow, q)),
  );
  const total = list.length;
  const start = (page - 1) * pageSize;
  list = list.slice(start, start + pageSize);
  return delay({ items: list, total, page, pageSize });
}

async function getTask(id: number): Promise<{
  task: TaskRow;
  nodes: TaskNodeRow[];
} | null> {
  const task = TASKS.find((t) => t.id === id) ?? null;
  if (!task) return delay(null);
  const nodes = NODE_SETS[id as keyof typeof NODE_SETS] ?? NODE_SETS[1];
  return delay({ task, nodes });
}

/* ------------------------------------------------ Providers / Models */

async function listProviders(): Promise<Provider[]> {
  return delay(PROVIDERS);
}

/* ------------------------------------------------ Knowledge */

async function listKnowledgeBases(): Promise<KnowledgeBase[]> {
  return delay(KNOWLEDGE_BASES);
}

async function searchKnowledge(
  baseId: number,
  query: string,
): Promise<RetrievalHit[]> {
  console.log("[api] search knowledge", baseId, query);
  return delay(RETRIEVE_HITS, 400);
}

/* ------------------------------------------------ Cost */

async function getCostAnalysis(range: string = "30d"): Promise<CostData> {
  const multiplier = range === "7d" ? 0.25 : range === "90d" ? 3 : 1;
  return delay({
    byModel: COST_BY_MODEL.map((m) => ({
      ...m,
      tokensIn: Math.round(m.tokensIn * multiplier),
      tokensOut: Math.round(m.tokensOut * multiplier),
      cost: +(m.cost * multiplier).toFixed(2),
      calls: Math.round(m.calls * multiplier),
    })),
    byWorkflow: COST_BY_WORKFLOW.map((w) => ({
      ...w,
      cost: +(w.cost * multiplier).toFixed(2),
      tasks: Math.round(w.tasks * multiplier),
      avgCost: +(w.avgCost * multiplier).toFixed(4),
    })),
    trend: COST_TREND.map((v) => +(v * multiplier).toFixed(2)),
    trendLabels: COST_TREND_LABELS,
  });
}

/* ------------------------------------------------ Agents */

async function listAgents(
  params: AgentListParams = {},
): Promise<ListResponse<Agent>> {
  const { runtimeType, q, page = 1, pageSize = 20 } = params;
  let list = AGENTS.filter(
    (a) =>
      (!runtimeType || a.runtimeType === runtimeType) &&
      (includesFold(a.name, q) || includesFold(a.description, q)),
  );
  const total = list.length;
  const start = (page - 1) * pageSize;
  list = list.slice(start, start + pageSize);
  return delay({ items: list, total, page, pageSize });
}

async function listMarketplaceAgents(
  params: AgentListParams = {},
): Promise<AgentMarketplaceResponse> {
  const { runtimeType, q } = params;
  const installedNames = AGENTS.map((a) => a.name);
  let items = MARKETPLACE_AGENTS.filter(
    (a) =>
      (!runtimeType || a.runtimeType === runtimeType) &&
      (includesFold(a.name, q) ||
        includesFold(a.description, q) ||
        // capabilities 在 Agent 上可能是空/缺失（后端未配置能力时），
        // 直接 .some 会抛 TypeError。
        (Array.isArray(a.capabilities) &&
          a.capabilities.some((c) => includesFold(c, q)))),
  );
  return delay({ items, installedNames });
}

async function installAgent(id: number): Promise<{ success: boolean }> {
  console.log("[api] install agent", id);
  return delay({ success: true }, 500);
}

async function deleteAgent(id: number): Promise<{ success: boolean }> {
  console.log("[api] delete agent", id);
  return delay({ success: true }, 400);
}

async function createAgent(payload: Partial<Agent>): Promise<Agent> {
  console.log("[api] create agent", payload);
  const newAgent: Agent = {
    id: AGENTS.length + 100,
    userId: 1,
    name: payload.name ?? "New Agent",
    description: payload.description ?? "",
    runtimeType: (payload.runtimeType as Agent["runtimeType"]) ?? "native",
    endpoint: payload.endpoint,
    model: payload.model,
    capabilities: payload.capabilities ?? [],
    status: "active",
    version: payload.version ?? "0.1.0",
    timeoutSec: payload.timeoutSec ?? 60,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
  return delay(newAgent, 500);
}

/* ------------------------------------------------ Templates */

async function listTemplates(
  params: TemplateListParams = {},
): Promise<ListResponse<WorkflowTemplate>> {
  const { category, q, page = 1, pageSize = 20 } = params;
  let list = WORKFLOW_TEMPLATES.filter(
    (t) =>
      (!category || category === "all" || t.category === category) &&
      (includesFold(t.name, q) || includesFold(t.description, q)),
  );
  const total = list.length;
  const start = (page - 1) * pageSize;
  list = list.slice(start, start + pageSize);
  return delay({ items: list, total, page, pageSize });
}

async function listTemplateCategories(): Promise<{ name: string; count: number }[]> {
  const counts = new Map<string, number>();
  WORKFLOW_TEMPLATES.forEach((t) => {
    counts.set(t.category, (counts.get(t.category) ?? 0) + 1);
  });
  const categories = [
    { name: "全部", count: WORKFLOW_TEMPLATES.length },
    ...Array.from(counts.entries()).map(([name, count]) => ({ name, count })),
  ];
  return delay(categories);
}

async function useTemplate(id: number): Promise<{ workflowId: number }> {
  console.log("[api] use template", id);
  // 模拟创建新工作流，返回一个新的 workflow id
  const newId = Math.floor(Math.random() * 1000) + 100;
  return delay({ workflowId: newId }, 500);
}

/* ============================================================
 * 导出 API 服务
 * ========================================================== */

export const api = {
  dashboard: { get: getDashboard },
  workflows: { list: listWorkflows, get: getWorkflow, save: saveWorkflow },
  tasks: { list: listTasks, get: getTask },
  providers: { list: listProviders },
  knowledge: { list: listKnowledgeBases, search: searchKnowledge },
  cost: { get: getCostAnalysis },
  agents: {
    list: listAgents,
    marketplace: listMarketplaceAgents,
    install: installAgent,
    delete: deleteAgent,
    create: createAgent,
  },
  templates: {
    list: listTemplates,
    categories: listTemplateCategories,
    use: useTemplate,
  },
};

/**
 * API 服务层：统一数据获取入口。
 *
 * 设计原则：
 * 1. 所有页面通过 import { xxxApi } from "@/api" 获取数据，不直接 import mock
 * 2. 通过 VITE_USE_MOCK 环境变量切换真实 API / Mock API
 * 3. 所有方法返回 Promise，模拟真实网络请求延迟
 * 4. 类型定义集中在这里，前后端联调时只改实现不改类型
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
  type WorkflowSummary,
  type TaskRow,
  type Provider,
  type KnowledgeBase,
  type ActivityItem,
  type Kpi,
  type EditorNode,
  type TaskNodeRow,
  type Agent,
  type WorkflowTemplate,
} from "@/lib/mock";

/* ============================================================
 * 类型定义（前后端契约）
 * ========================================================== */

export interface PaginationParams {
  page?: number;
  pageSize?: number;
}

export interface ListResponse<T> {
  items: T[];
  total: number;
  page: number;
  pageSize: number;
}

export interface DashboardData {
  kpis: Kpi[];
  throughput: number[];
  throughputLabels: string[];
  workerStats: typeof WORKER_STATS;
  tokenByProvider: typeof TOKEN_BY_PROVIDER;
  activity: ActivityItem[];
  recentTasks: TaskRow[];
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
      (!status || w.status === status) &&
      (!q || w.name.toLowerCase().includes(q.toLowerCase())),
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
      (!q ||
        String(t.id).includes(q) ||
        t.workflow.toLowerCase().includes(q.toLowerCase())),
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

/* ------------------------------------------------ Providers */

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
): Promise<typeof RETRIEVE_HITS> {
  console.log("[api] search knowledge", baseId, query);
  return delay(RETRIEVE_HITS, 400);
}

/* ------------------------------------------------ Agents */

export interface AgentListParams extends PaginationParams {
  runtime_type?: string;
  q?: string;
}

export interface AgentMarketplaceResponse {
  items: Agent[];
  installed_names: string[];
}

async function listAgents(
  params: AgentListParams = {},
): Promise<ListResponse<Agent>> {
  const { runtime_type, q, page = 1, pageSize = 20 } = params;
  let list = AGENTS.filter(
    (a) =>
      (!runtime_type || a.runtime_type === runtime_type) &&
      (!q ||
        a.name.toLowerCase().includes(q.toLowerCase()) ||
        a.description.toLowerCase().includes(q.toLowerCase())),
  );
  const total = list.length;
  const start = (page - 1) * pageSize;
  list = list.slice(start, start + pageSize);
  return delay({ items: list, total, page, pageSize });
}

async function listMarketplaceAgents(
  params: AgentListParams = {},
): Promise<AgentMarketplaceResponse> {
  const { runtime_type, q } = params;
  const installed_names = AGENTS.map((a) => a.name);
  let items = MARKETPLACE_AGENTS.filter(
    (a) =>
      (!runtime_type || a.runtime_type === runtime_type) &&
      (!q ||
        a.name.toLowerCase().includes(q.toLowerCase()) ||
        a.description.toLowerCase().includes(q.toLowerCase()) ||
        a.capabilities.some((c) => c.toLowerCase().includes(q.toLowerCase()))),
  );
  return delay({ items, installed_names });
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
    user_id: 1,
    name: payload.name ?? "New Agent",
    description: payload.description ?? "",
    runtime_type: (payload.runtime_type as Agent["runtime_type"]) ?? "native",
    endpoint: payload.endpoint,
    model: payload.model,
    capabilities: payload.capabilities ?? [],
    status: "active",
    version: payload.version ?? "0.1.0",
    timeout_sec: payload.timeout_sec ?? 60,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
  return delay(newAgent, 500);
}

/* ------------------------------------------------ Templates */

export interface TemplateListParams extends PaginationParams {
  category?: string;
  q?: string;
}

async function listTemplates(
  params: TemplateListParams = {},
): Promise<ListResponse<WorkflowTemplate>> {
  const { category, q, page = 1, pageSize = 20 } = params;
  let list = WORKFLOW_TEMPLATES.filter(
    (t) =>
      (!category || category === "all" || t.category === category) &&
      (!q ||
        t.name.toLowerCase().includes(q.toLowerCase()) ||
        t.description.toLowerCase().includes(q.toLowerCase())),
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

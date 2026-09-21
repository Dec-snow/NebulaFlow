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
  type WorkflowSummary,
  type TaskRow,
  type Provider,
  type KnowledgeBase,
  type ActivityItem,
  type Kpi,
  type EditorNode,
  type TaskNodeRow,
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

/* ============================================================
 * 导出 API 服务
 * ========================================================== */

export const api = {
  dashboard: { get: getDashboard },
  workflows: { list: listWorkflows, get: getWorkflow, save: saveWorkflow },
  tasks: { list: listTasks, get: getTask },
  providers: { list: listProviders },
  knowledge: { list: listKnowledgeBases, search: searchKnowledge },
};

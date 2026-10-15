/**
 * Mock 版 API 实现（`VITE_USE_MOCK=true` 时启用）。
 *
 * 用途：无后端时演示前端（`npm run dev` 直接看页面），以及视觉回归。
 * **默认不启用** —— 不设 `VITE_USE_MOCK` 就走真实 HTTP（见 api/index.ts）。
 *
 * 与真实实现共用同一份 `ApiService` 契约（api/service.ts），
 * 所以任何一侧漏掉方法都会在 `tsc` 阶段暴露。
 */

import {
  ACTIVITY,
  AGENTS,
  EDITOR_EDGES,
  EDITOR_NODES,
  KPIS,
  KNOWLEDGE_BASES,
  MARKETPLACE_AGENTS,
  MOCK_WORKFLOW_USAGE,
  NODE_SETS,
  PROVIDERS,
  RETRIEVE_HITS,
  TASKS,
  THROUGHPUT,
  THROUGHPUT_LABELS,
  TOKEN_BY_PROVIDER,
  USAGE_BY_PROVIDER,
  WORKER_STATS,
  WORKFLOW_TEMPLATES,
  WORKFLOWS,
} from "@/lib/mock";
import { includesFold } from "@/lib/utils";
import type {
  Agent,
  DashboardStats,
  KbDoc,
  KnowledgeBase,
  ListResponse,
  Provider,
  RetrievalHit,
  TaskLog,
  TaskStatus,
  UsageSummary,
  WorkflowDetail,
} from "@/types";
import type {
  AgentListParams,
  ApiService,
  CostData,
  DashboardData,
  TaskDetailResponse,
  TaskListParams,
  TemplateListParams,
  WorkflowListParams,
} from "./service";

const MOCK_DELAY = 250;

function delay<T>(data: T, ms = MOCK_DELAY): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(data), ms));
}

function paginate<T>(list: T[], page = 1, pageSize = 10): ListResponse<T> {
  const start = (page - 1) * pageSize;
  return {
    items: list.slice(start, start + pageSize),
    total: list.length,
    page,
    pageSize,
  };
}

const MOCK_USAGE: UsageSummary = {
  totalInputTokens: USAGE_BY_PROVIDER.reduce((s, p) => s + (p.inputTokens ?? 0), 0),
  totalOutputTokens: USAGE_BY_PROVIDER.reduce((s, p) => s + (p.outputTokens ?? 0), 0),
  totalRequests: USAGE_BY_PROVIDER.reduce((s, p) => s + (p.totalRequests ?? 0), 0),
  perProvider: USAGE_BY_PROVIDER,
  timestamp: new Date().toISOString(),
};

const MOCK_STATS: DashboardStats = {
  queueLength: 37,
  workerQueued: 12,
  activeWorkers: WORKER_STATS.active,
  totalWorkers: WORKER_STATS.total,
  runningTasks: 3,
  sseSubscribers: 1,
  sseUsers: 1,
  sseDropped: 0,
  timestamp: new Date().toISOString(),
};

export const mockApi: ApiService = {
  auth: {
    login: async ({ username }) =>
      delay({ token: "mock-token", username, userId: 1 }, 200),
    register: async ({ username }) =>
      delay({ token: "mock-token", username, userId: 1 }, 200),
    me: async () => delay({ id: 1, username: "demo" }),
  },

  dashboard: {
    stats: async (): Promise<DashboardStats> => delay({ ...MOCK_STATS }),

    get: async (range): Promise<DashboardData> => {
      const multiplier = range === "7d" ? 7 : range === "30d" ? 30 : 1;
      return delay({
        range,
        kpis: KPIS.map((k) => ({ ...k, value: (k.value ?? 0) * multiplier })),
        throughput: THROUGHPUT,
        throughputLabels: THROUGHPUT_LABELS,
        workerStats: WORKER_STATS,
        tokenByProvider: TOKEN_BY_PROVIDER,
        activity: ACTIVITY,
        recentTasks: TASKS.slice(0, 5),
        stats: MOCK_STATS,
        usage: MOCK_USAGE,
      });
    },
  },

  workflows: {
    list: async (params: WorkflowListParams = {}) => {
      const { status, q, page = 1, pageSize = 10 } = params;
      const list = WORKFLOWS.filter(
        (w) => (!status || w.status === status) && includesFold(w.name, q),
      );
      return delay(paginate(list, page, pageSize));
    },
    get: async (id): Promise<WorkflowDetail | null> => {
      const meta = WORKFLOWS.find((w) => w.id === id);
      if (!meta) return delay(null);
      return delay({
        id: meta.id,
        name: meta.name,
        description: meta.description,
        status: meta.status,
        updatedAt: meta.updatedAt,
        nodes: EDITOR_NODES,
        edges: EDITOR_EDGES,
      });
    },
    create: async (payload) =>
      delay({
        id: Math.floor(Math.random() * 900) + 100,
        name: payload.name,
        description: payload.description ?? "",
        status: payload.status ?? "draft",
        updatedAt: new Date().toISOString(),
        nodeCount: payload.nodes.length,
        edgeCount: payload.edges.length,
      }),
    save: async () => delay({ success: true }, 300),
    remove: async () => delay({ success: true }, 200),
  },

  tasks: {
    list: async (params: TaskListParams = {}) => {
      const { status, q, page = 1, pageSize = 10 } = params;
      const list = TASKS.filter(
        (t) =>
          (!status || t.status === status) &&
          (!q || String(t.id).includes(q) || includesFold(t.workflow, q)),
      );
      return delay(paginate(list, page, pageSize));
    },
    get: async (id): Promise<TaskDetailResponse | null> => {
      const task = TASKS.find((t) => t.id === id);
      if (!task) return delay(null);
      const nodes = NODE_SETS[id] ?? NODE_SETS[1] ?? [];
      return delay({ task, nodes });
    },
    create: async ({ workflowId, input }) =>
      delay({
        id: Math.floor(Math.random() * 9000) + 1000,
        workflowId,
        workflow: WORKFLOWS.find((w) => w.id === workflowId)?.name ?? `工作流 #${workflowId}`,
        status: "pending" as TaskStatus,
        startedAt: new Date().toISOString(),
        input,
        nodes: [],
      }),
    cancel: async () => delay({ success: true }, 200),
    logs: async (): Promise<TaskLog[]> => delay([]),
    timeline: async () => delay(null),
    trace: async () => delay(null),
  },

  providers: {
    list: async (): Promise<Provider[]> => delay(PROVIDERS),
    create: async (payload) =>
      delay({
        id: PROVIDERS.length + 100,
        name: payload.name,
        baseUrl: payload.baseUrl,
        status: "enabled" as const,
        priority: payload.priority ?? 100,
        isDefault: false,
        models: [],
      }),
    update: async (id) => {
      const found = PROVIDERS.find((p) => p.id === id);
      return delay(found ?? PROVIDERS[0]);
    },
    remove: async () => delay({ success: true }, 200),
    addModel: async (_id, payload) =>
      delay({ name: payload.name, maxTokens: payload.maxTokens ?? 4096 }),
  },

  knowledge: {
    list: async (): Promise<KnowledgeBase[]> => delay(KNOWLEDGE_BASES),
    create: async (payload) =>
      delay({
        id: KNOWLEDGE_BASES.length + 10,
        name: payload.name,
        description: payload.description ?? "",
        createdAt: new Date().toISOString(),
        docs: [],
      }),
    remove: async () => delay({ success: true }, 200),
    documents: async (kbId): Promise<KbDoc[]> =>
      delay(KNOWLEDGE_BASES.find((k) => k.id === kbId)?.docs ?? []),
    uploadDocument: async (_kbId, payload) =>
      delay({
        id: Math.floor(Math.random() * 900) + 100,
        filename: payload.filename,
        status: "indexing" as const,
        chunks: 0,
        sizeKb: Math.max(1, Math.round(payload.content.length / 1024)),
        createdAt: new Date().toISOString(),
      }),
    search: async (): Promise<RetrievalHit[]> => delay(RETRIEVE_HITS, 300),
  },

  cost: {
    get: async (range): Promise<CostData> =>
      delay({
        range,
        totals: MOCK_USAGE,
        byProvider: USAGE_BY_PROVIDER,
        byWorkflow: MOCK_WORKFLOW_USAGE,
      }),
  },

  agents: {
    list: async (params: AgentListParams = {}) => {
      const { runtimeType, q, page = 1, pageSize = 20 } = params;
      const list = AGENTS.filter(
        (a) =>
          (!runtimeType || a.runtimeType === runtimeType) &&
          (includesFold(a.name, q) || includesFold(a.description, q)),
      );
      return delay(paginate(list, page, pageSize));
    },
    get: async (id): Promise<Agent | null> => delay(AGENTS.find((a) => a.id === id) ?? null),
    create: async (payload) =>
      delay({
        id: AGENTS.length + 100,
        userId: 1,
        name: payload.name,
        description: payload.description ?? "",
        runtimeType: (payload.runtimeType as Agent["runtimeType"]) ?? "native",
        endpoint: payload.endpoint,
        model: payload.model,
        capabilities: payload.capabilities ?? [],
        status: "active",
        version: payload.version ?? "0.1.0",
        timeoutSec: payload.timeoutSec ?? 60,
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString(),
      }),
    remove: async () => delay({ success: true }, 200),
    health: async (id) => delay({ agentId: id, healthy: true, error: "" }),
    marketplace: async (params: AgentListParams = {}) => {
      const { runtimeType, q } = params;
      const items = MARKETPLACE_AGENTS.filter(
        (a) =>
          (!runtimeType || a.runtimeType === runtimeType) &&
          (includesFold(a.name, q) ||
            includesFold(a.description, q) ||
            (Array.isArray(a.capabilities) && a.capabilities.some((c) => includesFold(c, q)))),
      );
      return delay({ items, installedNames: AGENTS.map((a) => a.name) });
    },
    install: async (id) => {
      void id;
      return delay({ agentId: id, name: "已安装 Agent" }, 300);
    },
  },

  templates: {
    list: async (params: TemplateListParams = {}) => {
      const { category, q, page = 1, pageSize = 20 } = params;
      const list = WORKFLOW_TEMPLATES.filter(
        (t) =>
          (!category || category === "all" || t.category === category) &&
          (includesFold(t.name, q) || includesFold(t.description, q)),
      );
      return delay(paginate(list, page, pageSize));
    },
    categories: async () => {
      const counts = new Map<string, number>();
      WORKFLOW_TEMPLATES.forEach((t) => counts.set(t.category, (counts.get(t.category) ?? 0) + 1));
      return delay([
        { name: "全部", count: WORKFLOW_TEMPLATES.length },
        ...Array.from(counts.entries()).map(([name, count]) => ({ name, count })),
      ]);
    },
    use: async (id) =>
      delay({ workflowId: Math.floor(Math.random() * 900) + 100, name: `模板 ${id} 副本` }, 300),
  },
};

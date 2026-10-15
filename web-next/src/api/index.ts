/**
 * API 服务层：统一数据获取入口。
 *
 * 默认走**真实后端 HTTP**（`VITE_USE_MOCK !== "true"`）：
 *   - 所有请求经 `apiClient`，自动带上 `Authorization: Bearer <token>`；
 *   - 响应统一 snake_case → camelCase；
 *   - 后端不存在的字段一律留空（undefined），由 UI 决定隐藏还是占位，
 *     **绝不编造数字**。
 *
 * 打开 mock（无后端演示）：`VITE_USE_MOCK=true npm run dev|build`，
 * 实现见 `api/mock-api.ts`。
 *
 * 唯一事实来源是 Go 侧：路由见 `internal/api/server.go`，
 * 请求/响应结构见 `internal/api/handlers_*.go` 与 `internal/model/model.go`。
 */

import { ApiError, apiClient } from "./client";
import { USE_MOCK } from "./config";
import { mockApi } from "./mock-api";
import { asArray, asNumber, asString, durationBetween, query } from "./transform";
import { includesFold } from "@/lib/utils";
import type {
  ActivityItem,
  Agent,
  AgentHealth,
  AgentMarketplaceResponse,
  AuthResult,
  DashboardStats,
  EditorNode,
  KbDoc,
  KnowledgeBase,
  Kpi,
  ListResponse,
  NodeConfig,
  NodeKind,
  Provider,
  ProviderModel,
  ProviderUsage,
  RetrievalHit,
  TaskLog,
  TaskNodeRow,
  TaskRow,
  TaskStatus,
  TaskTimeline,
  UsageSummary,
  WorkflowDetail,
  WorkflowPayload,
  WorkflowStatus,
  WorkflowSummary,
  WorkflowTemplate,
} from "@/types";
import type {
  AgentListParams,
  ApiService,
  CostData,
  DashboardData,
  TaskDetailResponse,
  TaskListParams,
  TemplateCategory,
  TemplateListParams,
  TokenUsageBar,
  WorkflowListParams,
} from "./service";

export { ApiError } from "./client";
export { USE_MOCK } from "./config";
export { subscribeTaskStream, TERMINAL_EVENTS, TERMINAL_STATUSES } from "./sse";
export type { TaskStreamEvent, TaskStreamState } from "./sse";
export type {
  ApiService,
  AgentListParams,
  CostData,
  DashboardData,
  TaskDetailResponse,
  TaskListParams,
  TemplateCategory,
  TemplateListParams,
  TokenUsageBar,
  WorkflowListParams,
} from "./service";

/* ============================================================
 *  小工具
 * ========================================================== */

const WORKFLOW_STATUSES: WorkflowStatus[] = ["published", "draft", "archived"];
const TASK_STATUSES: TaskStatus[] = [
  "pending",
  "running",
  "waiting",
  "succeeded",
  "failed",
  "cancelled",
  "skipped",
];
const NODE_KINDS: NodeKind[] = ["input", "output", "llm", "tool", "rag", "condition"];

function asWorkflowStatus(v: unknown): WorkflowStatus {
  return WORKFLOW_STATUSES.includes(v as WorkflowStatus) ? (v as WorkflowStatus) : "draft";
}

function asTaskStatus(v: unknown): TaskStatus {
  return TASK_STATUSES.includes(v as TaskStatus) ? (v as TaskStatus) : "pending";
}

function asNodeKind(v: unknown): NodeKind {
  return NODE_KINDS.includes(v as NodeKind) ? (v as NodeKind) : "llm";
}

/** 列表接口最多为多少条记录补齐详情（避免 N+1 无上限地放大请求数）。 */
const DETAIL_ENRICH_LIMIT = 24;
/** 任务列表一次拉取的条数（后端用 limit/offset，没有 status/q 过滤参数）。 */
const TASK_FETCH_LIMIT = 200;

/* ============================================================
 *  映射：后端 model → 前端类型
 * ========================================================== */

function toEditorNode(raw: unknown): EditorNode {
  const n = (raw ?? {}) as Record<string, unknown>;
  return {
    id: asString(n.nodeKey),
    type: asNodeKind(n.nodeType),
    x: asNumber(n.positionX) ?? 0,
    y: asNumber(n.positionY) ?? 0,
    config: toNodeConfig(n.config),
  };
}

/**
 * 后端 NodeConfig → 前端 NodeConfig。
 *
 * 后端只认 model/prompt/system/tool/knowledge_base_id/max_retry/timeout_sec
 * 六个键，其余前端扩展键在保存时被塞进 `Extra`；
 * 这里把它们摊回扁平结构，保证编辑器里改过的字段往返不丢。
 */
function toNodeConfig(raw: unknown): NodeConfig {
  const cfg: NodeConfig = {};
  if (!raw || typeof raw !== "object") return cfg;
  const { extra, ...rest } = raw as Record<string, unknown>;
  Object.assign(cfg, rest);
  if (extra && typeof extra === "object" && !Array.isArray(extra)) {
    for (const [k, v] of Object.entries(extra as Record<string, unknown>)) {
      if (!(k in cfg)) (cfg as Record<string, unknown>)[k] = v;
    }
  }
  return cfg;
}

/**
 * 前端 NodeConfig 键 → 后端 `model.NodeConfig` 的 json tag。
 * 后端只认下列六个键，其余前端扩展键在保存时进 `extra`，避免被 Go 侧丢弃。
 */
const CONFIG_KEY_MAP: Record<string, string> = {
  model: "model",
  prompt: "prompt",
  system: "system",
  tool: "tool",
  knowledgeBaseId: "knowledge_base_id",
  maxRetry: "max_retry",
  timeoutSec: "timeout_sec",
};

/** 前端 NodeConfig → 后端 payload（扩展键进 extra，避免被 Go 的 json 解码丢弃）。 */
function packNodeConfig(cfg: NodeConfig = {}): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const extra: Record<string, unknown> = { ...(cfg.extra ?? {}) };
  for (const [k, v] of Object.entries(cfg)) {
    if (k === "extra" || v === undefined || v === null || v === "") continue;
    const target = CONFIG_KEY_MAP[k];
    if (target) out[target] = v;
    else extra[k] = v;
  }
  if (Object.keys(extra).length > 0) out.extra = extra;
  return out;
}

function toWorkflowSummary(raw: unknown): WorkflowSummary {
  const w = (raw ?? {}) as Record<string, unknown>;
  return {
    id: asNumber(w.id) ?? 0,
    name: asString(w.name),
    description: asString(w.description),
    status: asWorkflowStatus(w.status),
    createdAt: asString(w.createdAt) || undefined,
    updatedAt: asString(w.updatedAt) || undefined,
    category: asString(w.category) || undefined,
    icon: asString(w.icon) || undefined,
    isTemplate: w.isTemplate === true,
  };
}

function toWorkflowDetail(raw: unknown): WorkflowDetail {
  const w = (raw ?? {}) as Record<string, unknown>;
  const nodes = asArray<unknown>(w.nodes).map(toEditorNode);
  const edges = asArray<Record<string, unknown>>(w.edges)
    .map((e) => [asString(e.sourceNode), asString(e.targetNode)] as [string, string])
    .filter(([a, b]) => a !== "" && b !== "");
  return {
    id: asNumber(w.id) ?? 0,
    name: asString(w.name),
    description: asString(w.description),
    status: asWorkflowStatus(w.status),
    updatedAt: asString(w.updatedAt) || undefined,
    nodes,
    edges,
  };
}

/** 把绝对坐标归一化成 0–100 的相对坐标（列表缩略图用）。 */
function miniFromNodes(nodes: EditorNode[]): { id: string; x: number; y: number }[] {
  if (nodes.length === 0) return [];
  const xs = nodes.map((n) => n.x);
  const ys = nodes.map((n) => n.y);
  const minX = Math.min(...xs);
  const minY = Math.min(...ys);
  const spanX = Math.max(...xs) - minX || 1;
  const spanY = Math.max(...ys) - minY || 1;
  return nodes.map((n) => ({
    id: n.id,
    x: ((n.x - minX) / spanX) * 100,
    y: ((n.y - minY) / spanY) * 100,
  }));
}

function toProvider(raw: unknown): Provider {
  const p = (raw ?? {}) as Record<string, unknown>;
  return {
    id: asNumber(p.id) ?? 0,
    name: asString(p.name),
    baseUrl: asString(p.baseUrl),
    status: p.enabled === false ? "disabled" : "enabled",
    priority: asNumber(p.priority) ?? undefined,
    isDefault: p.isDefault === true,
    models: asArray<Record<string, unknown>>(p.models).map((m) => ({
      id: asNumber(m.id) ?? undefined,
      name: asString(m.name),
      maxTokens: asNumber(m.maxTokens) ?? undefined,
    })),
  };
}

function toKbDoc(raw: unknown): KbDoc {
  const d = (raw ?? {}) as Record<string, unknown>;
  return {
    id: asNumber(d.id) ?? 0,
    filename: asString(d.filename),
    status: (asString(d.status) || "pending") as KbDoc["status"],
    chunks: asNumber(d.chunks) ?? undefined,
    sizeKb: asNumber(d.sizeKb) ?? undefined,
    errorMessage: asString(d.errorMessage) || undefined,
    createdAt: asString(d.createdAt) || undefined,
  };
}

function toKnowledgeBase(raw: unknown): KnowledgeBase {
  const k = (raw ?? {}) as Record<string, unknown>;
  return {
    id: asNumber(k.id) ?? 0,
    name: asString(k.name),
    description: asString(k.description),
    createdAt: asString(k.createdAt) || undefined,
  };
}

function toTaskNodeRow(raw: unknown): TaskNodeRow {
  const n = (raw ?? {}) as Record<string, unknown>;
  const output = asString(n.output);
  const error = asString(n.error);
  return {
    key: asString(n.nodeKey),
    type: asNodeKind(n.nodeType),
    status: asTaskStatus(n.status),
    durationMs: asNumber(n.durationMs) ?? undefined,
    tokensIn: asNumber(n.tokensIn) ?? undefined,
    tokensOut: asNumber(n.tokensOut) ?? undefined,
    retries: asNumber(n.retries) ?? undefined,
    input: asString(n.input) || undefined,
    output: output || undefined,
    message: error || (output ? output.slice(0, 120) : undefined),
  };
}

function toTaskRow(raw: unknown, workflowNames: Map<number, string>): TaskRow {
  const t = (raw ?? {}) as Record<string, unknown>;
  const workflowId = asNumber(t.workflowId) ?? 0;
  return {
    id: asNumber(t.id) ?? 0,
    workflowId,
    workflow:
      workflowNames.get(workflowId) ??
      (workflowId > 0 ? `工作流 #${workflowId}` : "未知工作流"),
    status: asTaskStatus(t.status),
    // model.Task 没有 duration_ms：只有 started_at / finished_at
    durationMs: durationBetween(t.startedAt, t.finishedAt) ?? undefined,
    startedAt: asString(t.startedAt) || asString(t.createdAt) || undefined,
    createdAt: asString(t.createdAt) || undefined,
    input: asString(t.input),
    output: asString(t.output) || undefined,
    error: asString(t.error) || undefined,
    priority: asString(t.priority) || undefined,
    nodes: asArray<unknown>(t.nodes).map(toTaskNodeRow),
  };
}

/** id → name（任务列表要显示工作流名，而后端 task 只带 workflow_id）。 */
async function fetchWorkflowNames(): Promise<Map<number, string>> {
  const map = new Map<number, string>();
  try {
    const raw = await apiClient.get<{ workflows?: unknown[] }>("/api/workflows");
    asArray<Record<string, unknown>>(raw?.workflows).forEach((w) => {
      const id = asNumber(w.id);
      if (id !== null) map.set(id, asString(w.name));
    });
  } catch {
    /* 拉不到就退化成 "工作流 #id"，不影响列表渲染 */
  }
  return map;
}

/* ============================================================
 *  真实实现
 * ========================================================== */

async function enrichWorkflowSummaries(items: WorkflowSummary[]): Promise<void> {
  const targets = items.slice(0, DETAIL_ENRICH_LIMIT);
  await Promise.all(
    targets.map(async (w) => {
      if (w.isTemplate) return;
      try {
        const raw = await apiClient.get<unknown>(`/api/workflows/${w.id}`);
        const detail = toWorkflowDetail(raw);
        w.nodeCount = detail.nodes.length;
        w.edgeCount = detail.edges.length;
        w.mini = miniFromNodes(detail.nodes);
        w.miniEdges = detail.edges;
        if (!w.description) w.description = detail.description;
      } catch {
        // 详情拉不到就保持字段缺失：UI 会隐藏"节点数/连线数"并提示去详情看拓扑
      }
    }),
  );
}

const STATUS_TEXT: Record<string, string> = {
  pending: "排队中",
  running: "开始执行",
  waiting: "等待中",
  succeeded: "执行成功",
  failed: "执行失败",
  cancelled: "已取消",
  skipped: "已跳过",
};

/** 用真实任务时间戳分桶出吞吐量序列（24h 按小时 / 7d、30d 按天）。 */
function buildThroughput(
  tasks: TaskRow[],
  range: string,
): { values: number[]; labels: string[] } {
  const now = Date.now();
  const day = 86_400_000;
  const hour = 3_600_000;

  if (range === "7d" || range === "30d") {
    const days = range === "7d" ? 7 : 30;
    const values = new Array<number>(days).fill(0);
    const labels: string[] = [];
    for (let i = days - 1; i >= 0; i--) {
      const d = new Date(now - i * day);
      labels.push(`${d.getMonth() + 1}/${d.getDate()}`);
    }
    for (const t of tasks) {
      const ts = Date.parse(t.startedAt ?? "");
      if (!Number.isFinite(ts)) continue;
      const diff = Math.floor((now - ts) / day);
      if (diff < 0 || diff >= days) continue;
      values[days - 1 - diff] += 1;
    }
    return { values, labels };
  }

  const values = new Array<number>(24).fill(0);
  const labels: string[] = [];
  for (let i = 23; i >= 0; i--) {
    const d = new Date(now - i * hour);
    labels.push(`${String(d.getHours()).padStart(2, "0")}:00`);
  }
  for (const t of tasks) {
    const ts = Date.parse(t.startedAt ?? "");
    if (!Number.isFinite(ts)) continue;
    const diff = Math.floor((now - ts) / hour);
    if (diff < 0 || diff >= 24) continue;
    values[23 - diff] += 1;
  }
  return { values, labels };
}

function activityFromTasks(tasks: TaskRow[]): ActivityItem[] {
  return tasks.slice(0, 40).map((t) => ({
    id: `task-${t.id}`,
    at: t.startedAt ?? new Date().toISOString(),
    kind: t.status === "failed" ? "error" : "task",
    text: `任务 #${t.id} ${STATUS_TEXT[t.status] ?? t.status}`,
    meta: t.workflow,
  }));
}

function toProviderUsage(raw: unknown): ProviderUsage {
  const p = (raw ?? {}) as Record<string, unknown>;
  return {
    provider: asString(p.provider) || "unknown",
    totalRequests: asNumber(p.totalRequests) ?? undefined,
    inputTokens: asNumber(p.inputTokens) ?? undefined,
    outputTokens: asNumber(p.outputTokens) ?? undefined,
  };
}

function toUsageSummary(raw: unknown): UsageSummary {
  const u = (raw ?? {}) as Record<string, unknown>;
  return {
    totalInputTokens: asNumber(u.totalInputTokens) ?? undefined,
    totalOutputTokens: asNumber(u.totalOutputTokens) ?? undefined,
    totalRequests: asNumber(u.totalRequests) ?? undefined,
    perProvider: asArray<unknown>(u.perProvider).map(toProviderUsage),
    timestamp: asString(u.timestamp) || undefined,
  };
}

const BAR_TONES: TokenUsageBar["tone"][] = ["brand", "mint", "sky", "violet", "amber"];

function usageBars(perProvider: ProviderUsage[]): TokenUsageBar[] {
  return perProvider.map((p, i) => ({
    label: p.provider,
    value: (p.inputTokens ?? 0) + (p.outputTokens ?? 0),
    tone: BAR_TONES[i % BAR_TONES.length],
  }));
}

function toAgent(raw: unknown): Agent {
  const a = (raw ?? {}) as Record<string, unknown>;
  const runtime = asString(a.runtimeType);
  return {
    id: asNumber(a.id) ?? 0,
    userId: asNumber(a.userId) ?? 0,
    name: asString(a.name),
    description: asString(a.description),
    runtimeType: (["native", "langchain", "http"].includes(runtime)
      ? runtime
      : "native") as Agent["runtimeType"],
    endpoint: asString(a.endpoint) || undefined,
    model: asString(a.model) || undefined,
    capabilities: asArray<string>(a.capabilities).map((c) => String(c)),
    status: (asString(a.status) || "active") as Agent["status"],
    version: asString(a.version) || "—",
    timeoutSec: asNumber(a.timeoutSec) ?? undefined,
    createdAt: asString(a.createdAt) || undefined,
    updatedAt: asString(a.updatedAt) || undefined,
  };
}

function toTemplate(raw: unknown): WorkflowTemplate {
  const t = (raw ?? {}) as Record<string, unknown>;
  const nodes = asNumber(t.nodeCount);
  return {
    id: asNumber(t.id) ?? 0,
    name: asString(t.name),
    description: asString(t.description),
    category: asString(t.category),
    icon: asString(t.icon) || undefined,
    nodes: nodes ?? undefined,
    createdAt: asString(t.createdAt) || undefined,
  };
}

/**
 * `GET /api/dashboard/stats` → DashboardStats。
 *
 * 抽成函数是因为侧边栏徽标与 Dashboard 都要用它：后端字段全是数字且
 * 不带 omitempty，但缺字段时一律留 undefined（不编造 0，否则"没拿到"和
 * "真的是 0"在前端看起来一样）。
 */
function toDashboardStats(raw: unknown): DashboardStats {
  const s = (raw ?? {}) as Record<string, unknown>;
  return {
    queueLength: asNumber(s.queueLength) ?? undefined,
    workerQueued: asNumber(s.workerQueued) ?? undefined,
    activeWorkers: asNumber(s.activeWorkers) ?? undefined,
    totalWorkers: asNumber(s.totalWorkers) ?? undefined,
    runningTasks: asNumber(s.runningTasks) ?? undefined,
    sseSubscribers: asNumber(s.sseSubscribers) ?? undefined,
    sseUsers: asNumber(s.sseUsers) ?? undefined,
    sseDropped: asNumber(s.sseDropped) ?? undefined,
    timestamp: asString(s.timestamp) || undefined,
  };
}

export const realApi: ApiService = {
  auth: {
    async login(payload): Promise<AuthResult> {
      const raw = await apiClient.post<Record<string, unknown>>("/api/auth/login", {
        username: payload.username,
        password: payload.password,
      }, { auth: false });
      return {
        token: asString(raw?.token),
        username: asString(raw?.username) || payload.username,
        userId: asNumber(raw?.userId),
      };
    },
    async register(payload): Promise<AuthResult> {
      const raw = await apiClient.post<Record<string, unknown>>(
        "/api/auth/register",
        { username: payload.username, email: payload.email, password: payload.password },
        { auth: false },
      );
      return {
        token: asString(raw?.token),
        username: asString(raw?.username) || payload.username,
        userId: asNumber(raw?.userId),
      };
    },
    async me() {
      const raw = await apiClient.get<Record<string, unknown>>("/api/auth/me");
      return { id: asNumber(raw?.id) ?? 0, username: asString(raw?.username) };
    },
  },

  dashboard: {
    async stats(): Promise<DashboardStats> {
      const raw = await apiClient.get<Record<string, unknown>>("/api/dashboard/stats");
      return toDashboardStats(raw);
    },

    async get(range: string): Promise<DashboardData> {
      const [statsRaw, usageRaw, tasksRaw] = await Promise.all([
        apiClient.get<Record<string, unknown>>("/api/dashboard/stats"),
        apiClient.get<Record<string, unknown>>("/api/dashboard/usage"),
        apiClient.get<Record<string, unknown>>(`/api/tasks${query({ limit: TASK_FETCH_LIMIT, offset: 0 })}`),
      ]);

      const stats = toDashboardStats(statsRaw);
      const usage = toUsageSummary(usageRaw);
      const names = await fetchWorkflowNames();
      const tasks = asArray<unknown>(tasksRaw?.tasks).map((t) => toTaskRow(t, names));

      const throughput = buildThroughput(tasks, range);
      const hasUsage =
        usage.totalInputTokens !== undefined || usage.totalOutputTokens !== undefined;
      const totalTokens = hasUsage
        ? (usage.totalInputTokens ?? 0) + (usage.totalOutputTokens ?? 0)
        : null;

      const kpis: Kpi[] = [
        {
          key: "running",
          label: "运行中任务",
          value: stats.runningTasks ?? null,
          tone: "brand",
          spark: throughput.values,
        },
        { key: "queued", label: "队列待消费", value: stats.queueLength ?? null, tone: "sky" },
        {
          key: "workers",
          label: "活跃 Worker",
          value: stats.activeWorkers ?? null,
          unit: stats.totalWorkers !== undefined ? `/ ${stats.totalWorkers}` : undefined,
          tone: "mint",
        },
        { key: "tokens", label: "Token 消耗", value: totalTokens, tone: "violet" },
      ];

      const totalWorkers = stats.totalWorkers ?? null;
      const activeWorkers = stats.activeWorkers ?? null;

      return {
        range,
        kpis,
        throughput: throughput.values,
        throughputLabels: throughput.labels,
        workerStats: {
          total: totalWorkers ?? undefined,
          active: activeWorkers ?? undefined,
          queued: stats.workerQueued ?? undefined,
          utilization:
            totalWorkers && activeWorkers !== null ? activeWorkers / totalWorkers : undefined,
        },
        tokenByProvider: usageBars(usage.perProvider),
        activity: activityFromTasks(tasks),
        recentTasks: tasks.slice(0, 5),
        stats,
        usage,
      };
    },
  },

  workflows: {
    async list(params: WorkflowListParams = {}): Promise<ListResponse<WorkflowSummary>> {
      const { page = 1, pageSize = 10 } = params;
      const raw = await apiClient.get<{ workflows?: unknown[] }>("/api/workflows");
      // 后端 /api/workflows 没有 status / q 参数：过滤在客户端做
      const filtered = asArray<unknown>(raw?.workflows)
        .map(toWorkflowSummary)
        .filter(
          (w) =>
            (!params.status || w.status === params.status) && includesFold(w.name, params.q),
        );
      const start = (page - 1) * pageSize;
      const items = filtered.slice(start, start + pageSize);
      // 列表不下发 nodes/edges：为当前页补详情，拿到真实的节点数/连线数与缩略拓扑
      await enrichWorkflowSummaries(items);
      return { items, total: filtered.length, page, pageSize };
    },

    async get(id: number): Promise<WorkflowDetail | null> {
      try {
        const raw = await apiClient.get<unknown>(`/api/workflows/${id}`);
        return toWorkflowDetail(raw);
      } catch (err) {
        if (err instanceof ApiError && err.status === 404) return null;
        throw err;
      }
    },

    async create(payload: WorkflowPayload): Promise<WorkflowSummary> {
      const raw = await apiClient.post<unknown>("/api/workflows", {
        name: payload.name,
        description: payload.description ?? "",
        status: payload.status ?? "draft",
        nodes: payload.nodes.map((n) => ({
          key: n.id,
          type: n.type,
          config: packNodeConfig(n.config),
          x: n.x,
          y: n.y,
        })),
        edges: payload.edges.map(([source, target]) => ({ source, target })),
      });
      return toWorkflowSummary(raw);
    },

    async save(id: number, payload: WorkflowPayload): Promise<{ success: boolean }> {
      await apiClient.put<unknown>(`/api/workflows/${id}`, {
        name: payload.name,
        description: payload.description ?? "",
        status: payload.status ?? "draft",
        nodes: payload.nodes.map((n) => ({
          key: n.id,
          type: n.type,
          config: packNodeConfig(n.config),
          x: n.x,
          y: n.y,
        })),
        edges: payload.edges.map(([source, target]) => ({ source, target })),
      });
      return { success: true };
    },

    async remove(id: number): Promise<{ success: boolean }> {
      await apiClient.delete<unknown>(`/api/workflows/${id}`);
      return { success: true };
    },
  },

  tasks: {
    async list(params: TaskListParams = {}): Promise<ListResponse<TaskRow>> {
      const { page = 1, pageSize = 10 } = params;
      const [raw, names] = await Promise.all([
        apiClient.get<Record<string, unknown>>(
          `/api/tasks${query({ limit: TASK_FETCH_LIMIT, offset: 0 })}`,
        ),
        fetchWorkflowNames(),
      ]);
      const rows = asArray<unknown>(raw?.tasks).map((t) => toTaskRow(t, names));
      // 后端列表不接受 status/q：按当前页数据客户端过滤
      const filtered = rows.filter(
        (t) =>
          (!params.status || t.status === params.status) &&
          (!params.q || String(t.id).includes(params.q) || includesFold(t.workflow, params.q)),
      );
      const start = (page - 1) * pageSize;
      return {
        items: filtered.slice(start, start + pageSize),
        total: filtered.length,
        page,
        pageSize,
      };
    },

    async get(id: number): Promise<TaskDetailResponse | null> {
      try {
        const raw = await apiClient.get<unknown>(`/api/tasks/${id}`);
        const base = toTaskRow(raw, new Map());
        let workflow = base.workflow;
        if (base.workflowId > 0) {
          try {
            const wf = await apiClient.get<Record<string, unknown>>(`/api/workflows/${base.workflowId}`);
            workflow = asString(wf?.name) || workflow;
          } catch {
            /* 工作流详情拉不到时保留 "工作流 #id" */
          }
        }
        const task: TaskRow = { ...base, workflow };
        return { task, nodes: task.nodes };
      } catch (err) {
        if (err instanceof ApiError && err.status === 404) return null;
        throw err;
      }
    },

    async create(payload): Promise<TaskRow> {
      const raw = await apiClient.post<unknown>("/api/tasks", {
        workflow_id: payload.workflowId,
        input: payload.input,
      });
      const names = await fetchWorkflowNames();
      return toTaskRow(raw, names);
    },

    async cancel(id: number): Promise<{ success: boolean }> {
      await apiClient.post<unknown>(`/api/tasks/${id}/cancel`);
      return { success: true };
    },

    async logs(id: number, limit = 100): Promise<TaskLog[]> {
      const raw = await apiClient.get<{ logs?: unknown[] }>(
        `/api/tasks/${id}/logs${query({ limit })}`,
      );
      return asArray<Record<string, unknown>>(raw?.logs).map((l) => ({
        id: asNumber(l.id) ?? 0,
        nodeKey: asString(l.nodeKey) || undefined,
        level: asString(l.level) || "info",
        message: asString(l.message),
        createdAt: asString(l.createdAt) || undefined,
      }));
    },

    async timeline(id: number): Promise<TaskTimeline | null> {
      try {
        const raw = await apiClient.get<Record<string, unknown>>(`/api/tasks/${id}/timeline`);
        const tu = (raw?.tokenUsage ?? {}) as Record<string, unknown>;
        return {
          taskId: asNumber(raw?.taskId) ?? id,
          status: asString(raw?.status),
          totalMs: asNumber(raw?.totalMs) ?? undefined,
          startedAt: asString(raw?.startedAt) || undefined,
          finishedAt: asString(raw?.finishedAt) || undefined,
          toolCalls: asNumber(raw?.toolCalls) ?? undefined,
          tokenUsage: {
            input: asNumber(tu.input) ?? undefined,
            output: asNumber(tu.output) ?? undefined,
            total: asNumber(tu.total) ?? undefined,
            nodeCount: asNumber(tu.nodeCount) ?? undefined,
          },
          nodes: asArray<Record<string, unknown>>(raw?.nodes).map((n) => ({
            nodeKey: asString(n.nodeKey),
            nodeType: asString(n.nodeType),
            status: asString(n.status),
            startMs: asNumber(n.startMs) ?? undefined,
            durationMs: asNumber(n.durationMs) ?? undefined,
            tokensIn: asNumber(n.tokensIn) ?? undefined,
            tokensOut: asNumber(n.tokensOut) ?? undefined,
            error: asString(n.error) || undefined,
            events: asArray<Record<string, unknown>>(n.events).map((e) => ({
              type: asString(e.type),
              name: asString(e.name) || undefined,
              offsetMs: asNumber(e.offsetMs) ?? undefined,
              durationMs: asNumber(e.durationMs) ?? undefined,
              detail: asString(e.detail) || undefined,
            })),
          })),
        };
      } catch (err) {
        // 501 = tracing/时间线未启用；404 = 任务不存在。都不是"页面错误"
        if (err instanceof ApiError && (err.status === 501 || err.status === 404)) return null;
        throw err;
      }
    },

    async trace(id: number): Promise<unknown | null> {
      try {
        return await apiClient.get<unknown>(`/api/tasks/${id}/trace`, { raw: true });
      } catch (err) {
        if (err instanceof ApiError && (err.status === 501 || err.status === 404)) return null;
        throw err;
      }
    },
  },

  providers: {
    async list(): Promise<Provider[]> {
      const raw = await apiClient.get<{ providers?: unknown[] }>("/api/providers");
      return asArray<unknown>(raw?.providers).map(toProvider);
    },
    async create(payload): Promise<Provider> {
      const raw = await apiClient.post<unknown>("/api/providers", {
        name: payload.name,
        base_url: payload.baseUrl,
        api_key: payload.apiKey ?? "",
        priority: payload.priority ?? 0,
      });
      return toProvider(raw);
    },
    async update(id, payload): Promise<Provider> {
      const raw = await apiClient.put<unknown>(`/api/providers/${id}`, {
        base_url: payload.baseUrl ?? "",
        api_key: payload.apiKey ?? "",
        is_default: false,
        enabled: payload.enabled,
        priority: payload.priority ?? 0,
      });
      return toProvider(raw);
    },
    async remove(id): Promise<{ success: boolean }> {
      await apiClient.delete<unknown>(`/api/providers/${id}`);
      return { success: true };
    },
    async addModel(id, payload): Promise<ProviderModel> {
      const raw = await apiClient.post<Record<string, unknown>>(`/api/providers/${id}/models`, {
        name: payload.name,
        max_tokens: payload.maxTokens ?? 0,
      });
      return {
        id: asNumber(raw?.id) ?? undefined,
        name: asString(raw?.name),
        maxTokens: asNumber(raw?.maxTokens) ?? undefined,
      };
    },
  },

  knowledge: {
    async list(): Promise<KnowledgeBase[]> {
      const raw = await apiClient.get<{ knowledgeBases?: unknown[] }>("/api/knowledge-bases");
      return asArray<unknown>(raw?.knowledgeBases).map(toKnowledgeBase);
    },
    async create(payload): Promise<KnowledgeBase> {
      const raw = await apiClient.post<unknown>("/api/knowledge-bases", {
        name: payload.name,
        description: payload.description ?? "",
      });
      return toKnowledgeBase(raw);
    },
    async remove(id): Promise<{ success: boolean }> {
      await apiClient.delete<unknown>(`/api/knowledge-bases/${id}`);
      return { success: true };
    },
    async documents(kbId: number): Promise<KbDoc[]> {
      const raw = await apiClient.get<{ documents?: unknown[] }>(
        `/api/knowledge-bases/${kbId}/documents`,
      );
      return asArray<unknown>(raw?.documents).map(toKbDoc);
    },
    async uploadDocument(kbId, payload): Promise<KbDoc> {
      const raw = await apiClient.post<unknown>(
        `/api/knowledge-bases/${kbId}/documents`,
        { content: payload.content, filename: payload.filename },
      );
      return toKbDoc(raw);
    },
    async search(kbId, queryText, k = 5): Promise<RetrievalHit[]> {
      const raw = await apiClient.get<{ hits?: unknown[] }>(
        `/api/knowledge-bases/${kbId}/retrieve${query({ q: queryText, k })}`,
      );
      return asArray<Record<string, unknown>>(raw?.hits).map((h) => ({
        id: asNumber(h.id) ?? 0,
        // 后端 DocumentChunk 的来源字段是 filename
        filename: asString(h.filename) || undefined,
        score: asNumber(h.score) ?? undefined,
        content: asString(h.content),
      }));
    },
  },

  cost: {
    async get(range: string): Promise<CostData> {
      const [usageRaw, tasksRaw, wfRaw] = await Promise.all([
        apiClient.get<Record<string, unknown>>("/api/dashboard/usage"),
        apiClient.get<Record<string, unknown>>(
          `/api/tasks${query({ limit: TASK_FETCH_LIMIT, offset: 0 })}`,
        ),
        apiClient.get<{ workflows?: unknown[] }>("/api/workflows"),
      ]);
      const usage = toUsageSummary(usageRaw);
      const names = new Map<number, string>();
      asArray<Record<string, unknown>>(wfRaw?.workflows).forEach((w) => {
        const id = asNumber(w.id);
        if (id !== null) names.set(id, asString(w.name));
      });

      const perWorkflow = new Map<number, number>();
      asArray<Record<string, unknown>>(tasksRaw?.tasks).forEach((t) => {
        const id = asNumber(t.workflowId);
        if (id === null) return;
        perWorkflow.set(id, (perWorkflow.get(id) ?? 0) + 1);
      });

      return {
        range,
        totals: usage,
        byProvider: usage.perProvider,
        byWorkflow: Array.from(perWorkflow.entries())
          .map(([id, tasks]) => ({ id, name: names.get(id) ?? `工作流 #${id}`, tasks }))
          .sort((a, b) => b.tasks - a.tasks),
      };
    },
  },

  agents: {
    async list(params: AgentListParams = {}): Promise<ListResponse<Agent>> {
      const { page = 1, pageSize = 20 } = params;
      const raw = await apiClient.get<Record<string, unknown>>(
        `/api/agents${query({ runtime_type: params.runtimeType, capability: params.capability })}`,
      );
      const rows = asArray<unknown>(raw?.agents)
        .map(toAgent)
        .filter((a) => includesFold(a.name, params.q) || includesFold(a.description, params.q));
      const start = (page - 1) * pageSize;
      return {
        items: rows.slice(start, start + pageSize),
        total: rows.length,
        page,
        pageSize,
      };
    },

    async get(id: number): Promise<Agent | null> {
      try {
        return toAgent(await apiClient.get<unknown>(`/api/agents/${id}`));
      } catch (err) {
        if (err instanceof ApiError && err.status === 404) return null;
        throw err;
      }
    },

    async create(payload): Promise<Agent> {
      const raw = await apiClient.post<unknown>("/api/agents", {
        name: payload.name,
        description: payload.description ?? "",
        runtime_type: payload.runtimeType,
        endpoint: payload.endpoint ?? "",
        model: payload.model ?? "",
        capabilities: payload.capabilities ?? [],
        version: payload.version ?? "",
        timeout_sec: payload.timeoutSec ?? 0,
        status: payload.status ?? "active",
      });
      return toAgent(raw);
    },

    async remove(id: number): Promise<{ success: boolean }> {
      await apiClient.delete<unknown>(`/api/agents/${id}`);
      return { success: true };
    },

    async health(id: number): Promise<AgentHealth> {
      const raw = await apiClient.get<Record<string, unknown>>(`/api/agents/${id}/health`);
      return {
        agentId: asNumber(raw?.agentId) ?? id,
        healthy: raw?.healthy === true,
        error: asString(raw?.error) || undefined,
        metadata: (raw?.metadata ?? undefined) as Record<string, unknown> | undefined,
      };
    },

    async marketplace(params: AgentListParams = {}): Promise<AgentMarketplaceResponse> {
      const raw = await apiClient.get<Record<string, unknown>>(
        `/api/agents/marketplace${query({ runtime_type: params.runtimeType, capability: params.capability })}`,
      );
      // 后端 installed_names 是 map[string]bool（JSON 对象），这里转成名字数组
      const installedRaw = raw?.installedNames;
      const installedNames =
        installedRaw && typeof installedRaw === "object" && !Array.isArray(installedRaw)
          ? Object.keys(installedRaw as Record<string, unknown>)
          : asArray<string>(installedRaw);
      // 后端 marketplace 不接受 q：名称/描述/能力标签的模糊匹配在客户端做
      const items = asArray<unknown>(raw?.agents)
        .map(toAgent)
        .filter(
          (a) =>
            includesFold(a.name, params.q) ||
            includesFold(a.description, params.q) ||
            a.capabilities.some((c) => includesFold(c, params.q)),
        );
      return { items, installedNames };
    },

    async install(id: number): Promise<{ agentId: number | null; name: string }> {
      const raw = await apiClient.post<Record<string, unknown>>(
        `/api/agents/marketplace/${id}/install`,
      );
      return { agentId: asNumber(raw?.agentId), name: asString(raw?.name) };
    },
  },

  templates: {
    async list(params: TemplateListParams = {}): Promise<ListResponse<WorkflowTemplate>> {
      const { page = 1, pageSize = 20 } = params;
      const raw = await apiClient.get<{ templates?: unknown[] }>(
        `/api/templates${query({ category: params.category })}`,
      );
      const rows = asArray<unknown>(raw?.templates)
        .map(toTemplate)
        .filter((t) => includesFold(t.name, params.q) || includesFold(t.description, params.q));
      const start = (page - 1) * pageSize;
      return {
        items: rows.slice(start, start + pageSize),
        total: rows.length,
        page,
        pageSize,
      };
    },

    async categories(): Promise<TemplateCategory[]> {
      const [catsRaw, listRaw] = await Promise.all([
        apiClient.get<{ categories?: unknown[] }>("/api/templates/categories"),
        apiClient.get<{ templates?: unknown[] }>("/api/templates"),
      ]);
      const templates = asArray<unknown>(listRaw?.templates).map(toTemplate);
      const counts = new Map<string, number>();
      templates.forEach((t) => counts.set(t.category, (counts.get(t.category) ?? 0) + 1));
      const names = asArray<string>(catsRaw?.categories).map((c) => String(c));
      return [
        { name: "全部", count: templates.length },
        ...names.map((name) => ({ name, count: counts.get(name) ?? 0 })),
      ];
    },

    async use(id: number, name?: string): Promise<{ workflowId: number | null; name: string }> {
      const raw = await apiClient.post<Record<string, unknown>>(`/api/templates/${id}/use`, {
        name: name ?? "",
      });
      return { workflowId: asNumber(raw?.workflowId), name: asString(raw?.name) };
    },
  },
};

/* ============================================================
 *  导出：按开关选择实现（默认真实 API）
 * ========================================================== */

export const api: ApiService = USE_MOCK ? mockApi : realApi;

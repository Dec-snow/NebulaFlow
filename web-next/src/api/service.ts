/**
 * API 服务层的对外契约。
 *
 * `api/index.ts`（真实 HTTP）与 `api/mock-api.ts`（本地演示数据）都实现
 * 这个接口，页面只依赖 `api` 对象、不关心数据来源。这样切换
 * `VITE_USE_MOCK` 时不存在"某一侧漏实现某个方法"的可能 —— 编译期就报错。
 */

import type {
  ActivityItem,
  Agent,
  AgentHealth,
  AgentMarketplaceResponse,
  AuthResult,
  DashboardStats,
  KbDoc,
  KnowledgeBase,
  Kpi,
  ListResponse,
  LoginPayload,
  PaginationParams,
  Provider,
  ProviderModel,
  ProviderPayload,
  ProviderUsage,
  RegisterPayload,
  RetrievalHit,
  TaskLog,
  TaskNodeRow,
  TaskRow,
  TaskTimeline,
  UsageSummary,
  WorkerStats,
  WorkflowDetail,
  WorkflowPayload,
  WorkflowSummary,
  WorkflowTemplate,
  WorkflowUsage,
} from "@/types";

export type { PaginationParams, ListResponse } from "@/types";

/** BarSeries 用的分布行。 */
export interface TokenUsageBar {
  label: string;
  value: number;
  tone?: "brand" | "mint" | "sky" | "violet" | "amber";
}

/** 控制台一屏所需的全部数据。 */
export interface DashboardData {
  range: string;
  kpis: Kpi[];
  /** 吞吐量序列（按 range 决定小时/天粒度），由真实任务时间戳分桶得到 */
  throughput: number[];
  throughputLabels: string[];
  workerStats: WorkerStats;
  tokenByProvider: TokenUsageBar[];
  /** 活动流：由真实任务记录派生（不是假事件） */
  activity: ActivityItem[];
  recentTasks: TaskRow[];
  stats: DashboardStats;
  usage: UsageSummary;
}

/** 成本页数据。后端不提供计费字段，这里只有真实的 token 用量与任务数。 */
export interface CostData {
  range: string;
  totals: UsageSummary;
  byProvider: ProviderUsage[];
  byWorkflow: WorkflowUsage[];
}

export interface WorkflowListParams extends PaginationParams {
  status?: string;
  q?: string;
}

export interface TaskListParams extends PaginationParams {
  status?: string;
  q?: string;
}

export interface AgentListParams extends PaginationParams {
  runtimeType?: string;
  q?: string;
  capability?: string;
}

export interface TemplateListParams extends PaginationParams {
  category?: string;
  q?: string;
}

export interface TaskDetailResponse {
  task: TaskRow;
  nodes: TaskNodeRow[];
}

export interface TemplateCategory {
  name: string;
  count: number;
}

export interface ApiService {
  auth: {
    login(payload: LoginPayload): Promise<AuthResult>;
    register(payload: RegisterPayload): Promise<AuthResult>;
    me(): Promise<{ id: number; username: string }>;
  };
  dashboard: {
    get(range: string): Promise<DashboardData>;
    /**
     * 只取 `GET /api/dashboard/stats` 的运行时计数。
     *
     * 侧边栏徽标这类"全局但轻量"的位置用它，避免为了一个数字
     * 把 usage + tasks + workflows 一起拉下来（dashboard.get 会）。
     */
    stats(): Promise<DashboardStats>;
  };
  workflows: {
    list(params?: WorkflowListParams): Promise<ListResponse<WorkflowSummary>>;
    get(id: number): Promise<WorkflowDetail | null>;
    create(payload: WorkflowPayload): Promise<WorkflowSummary>;
    save(id: number, payload: WorkflowPayload): Promise<{ success: boolean }>;
    remove(id: number): Promise<{ success: boolean }>;
  };
  tasks: {
    list(params?: TaskListParams): Promise<ListResponse<TaskRow>>;
    get(id: number): Promise<TaskDetailResponse | null>;
    create(payload: { workflowId: number; input: string }): Promise<TaskRow>;
    cancel(id: number): Promise<{ success: boolean }>;
    logs(id: number, limit?: number): Promise<TaskLog[]>;
    timeline(id: number): Promise<TaskTimeline | null>;
    trace(id: number): Promise<unknown | null>;
  };
  providers: {
    list(): Promise<Provider[]>;
    create(payload: ProviderPayload): Promise<Provider>;
    update(id: number, payload: Partial<ProviderPayload> & { enabled?: boolean }): Promise<Provider>;
    remove(id: number): Promise<{ success: boolean }>;
    addModel(id: number, payload: { name: string; maxTokens?: number }): Promise<ProviderModel>;
  };
  knowledge: {
    list(): Promise<KnowledgeBase[]>;
    create(payload: { name: string; description?: string }): Promise<KnowledgeBase>;
    remove(id: number): Promise<{ success: boolean }>;
    documents(kbId: number): Promise<KbDoc[]>;
    uploadDocument(kbId: number, payload: { filename: string; content: string }): Promise<KbDoc>;
    search(kbId: number, query: string, k?: number): Promise<RetrievalHit[]>;
  };
  cost: {
    get(range: string): Promise<CostData>;
  };
  agents: {
    list(params?: AgentListParams): Promise<ListResponse<Agent>>;
    get(id: number): Promise<Agent | null>;
    create(payload: Partial<Agent> & { name: string; runtimeType: string }): Promise<Agent>;
    remove(id: number): Promise<{ success: boolean }>;
    health(id: number): Promise<AgentHealth>;
    marketplace(params?: AgentListParams): Promise<AgentMarketplaceResponse>;
    install(id: number): Promise<{ agentId: number | null; name: string }>;
  };
  templates: {
    list(params?: TemplateListParams): Promise<ListResponse<WorkflowTemplate>>;
    categories(): Promise<TemplateCategory[]>;
    use(id: number, name?: string): Promise<{ workflowId: number | null; name: string }>;
  };
}

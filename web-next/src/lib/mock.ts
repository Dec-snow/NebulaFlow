/**
 * 框架演示数据。
 *
 * 全部为前端 mock，目的是让框架脱离后端即可完整预览。
 * 接入真实接口时，把这些常量替换为 fetch 结果即可 —— 页面只消费这些类型，
 * 不关心数据来源。
 */

/** 确定性伪随机，保证每次刷新数据一致（避免视觉抖动）。 */
function seeded(seed: number) {
  let s = seed;
  return () => {
    s = (s * 1103515245 + 12345) % 2147483648;
    return s / 2147483648;
  };
}

/* ------------------------------------------------------------------ 指标 */
export interface Kpi {
  key: string;
  label: string;
  value: number;
  unit?: string;
  delta: number;
  deltaGood: "up" | "down";
  tone: "brand" | "mint" | "sky" | "violet" | "amber";
  spark: number[];
}

const rnd = seeded(42);
const series = (n: number, base: number, jitter: number) =>
  Array.from({ length: n }, (_, i) =>
    Math.round(base + Math.sin(i / 2.2) * jitter * 0.55 + rnd() * jitter),
  );

export const KPIS: Kpi[] = [
  {
    key: "tasks",
    label: "今日任务",
    value: 1284,
    delta: 12.4,
    deltaGood: "up",
    tone: "brand",
    spark: series(24, 46, 22),
  },
  {
    key: "success",
    label: "成功率",
    value: 98.6,
    unit: "%",
    delta: 0.8,
    deltaGood: "up",
    tone: "mint",
    spark: series(24, 96, 4),
  },
  {
    key: "p95",
    label: "P95 延迟",
    value: 842,
    unit: "ms",
    delta: -6.2,
    deltaGood: "down",
    tone: "sky",
    spark: series(24, 900, 180),
  },
  {
    key: "tokens",
    label: "Token 消耗",
    value: 3.4,
    unit: "M",
    delta: 8.3,
    deltaGood: "down",
    tone: "violet",
    spark: series(24, 130, 60),
  },
];

/** 24 小时吞吐量（按小时）。 */
export const THROUGHPUT: number[] = series(24, 48, 30);
export const THROUGHPUT_LABELS = Array.from({ length: 24 }, (_, i) => `${i}:00`);

export const WORKER_STATS = {
  total: 20,
  active: 14,
  queued: 37,
  utilization: 0.7,
  queueCapacity: 160,
};

export const TOKEN_BY_PROVIDER = [
  { label: "DeepSeek", value: 1_820_400, tone: "brand" as const },
  { label: "OpenAI", value: 940_200, tone: "mint" as const },
  { label: "智谱 AI", value: 512_800, tone: "sky" as const },
  { label: "Anthropic", value: 148_600, tone: "violet" as const },
];

/* ------------------------------------------------------------------ 活动流 */
export type ActivityKind = "task" | "node" | "system" | "error";

export interface ActivityItem {
  id: string;
  at: string;
  kind: ActivityKind;
  text: string;
  meta?: string;
}

const now = Date.now();
const ago = (min: number) => new Date(now - min * 60_000).toISOString();

export const ACTIVITY: ActivityItem[] = [
  { id: "a1", at: ago(0.2), kind: "node", text: "writer 节点完成", meta: "842ms · 1.2k tok" },
  { id: "a2", at: ago(0.4), kind: "task", text: "任务 #1284 执行成功", meta: "技术分析工作流" },
  { id: "a3", at: ago(1.1), kind: "node", text: "rag 节点命中 3 条上下文", meta: "top score 0.82" },
  { id: "a4", at: ago(2.3), kind: "system", text: "Worker Pool 扩容检查通过", meta: "20/20 健康" },
  { id: "a5", at: ago(3.8), kind: "error", text: "任务 #1279 失败", meta: "tool 参数校验不通过" },
  { id: "a6", at: ago(5.2), kind: "node", text: "analyst 节点完成", meta: "1.4s · 890 tok" },
  { id: "a7", at: ago(7.6), kind: "task", text: "任务 #1278 执行成功", meta: "知识库问答流" },
  { id: "a8", at: ago(9.4), kind: "system", text: "Provider 故障转移", meta: "deepseek → ollama" },
];

/* ------------------------------------------------------------------ 工作流 */
export interface WorkflowSummary {
  id: number;
  name: string;
  description: string;
  status: "published" | "draft";
  nodeCount: number;
  edgeCount: number;
  runs: number;
  successRate: number;
  updatedAt: string;
  /** 缩略图节点位置（0–100 的相对坐标） */
  mini: { id: string; x: number; y: number }[];
  miniEdges: [string, string][];
}

export const WORKFLOWS: WorkflowSummary[] = [
  {
    id: 1,
    name: "技术分析工作流",
    description: "解析输入 → 并行 RAG 检索与 LLM 分析 → 汇总技术报告",
    status: "published",
    nodeCount: 6,
    edgeCount: 7,
    runs: 482,
    successRate: 98.8,
    updatedAt: ago(12),
    mini: [
      { id: "parse", x: 8, y: 50 },
      { id: "rag", x: 34, y: 18 },
      { id: "analyst", x: 34, y: 50 },
      { id: "clock", x: 34, y: 82 },
      { id: "writer", x: 64, y: 50 },
      { id: "out", x: 90, y: 50 },
    ],
    miniEdges: [
      ["parse", "rag"],
      ["parse", "analyst"],
      ["parse", "clock"],
      ["rag", "writer"],
      ["analyst", "writer"],
      ["clock", "writer"],
      ["writer", "out"],
    ],
  },
  {
    id: 2,
    name: "知识库问答",
    description: "RAG 检索 → LLM 生成答案 → 附带引用来源",
    status: "published",
    nodeCount: 4,
    edgeCount: 3,
    runs: 1206,
    successRate: 99.4,
    updatedAt: ago(48),
    mini: [
      { id: "q", x: 10, y: 50 },
      { id: "ret", x: 40, y: 50 },
      { id: "gen", x: 68, y: 50 },
      { id: "out", x: 92, y: 50 },
    ],
    miniEdges: [
      ["q", "ret"],
      ["ret", "gen"],
      ["gen", "out"],
    ],
  },
  {
    id: 3,
    name: "多模型对比评测",
    description: "同一输入分发给三个 Provider，聚合评分后输出对比表",
    status: "published",
    nodeCount: 7,
    edgeCount: 7,
    runs: 156,
    successRate: 96.2,
    updatedAt: ago(120),
    mini: [
      { id: "in", x: 8, y: 50 },
      { id: "ds", x: 36, y: 20 },
      { id: "ol", x: 36, y: 50 },
      { id: "oa", x: 36, y: 80 },
      { id: "judge", x: 68, y: 50 },
      { id: "out", x: 92, y: 50 },
    ],
    miniEdges: [
      ["in", "ds"],
      ["in", "ol"],
      ["in", "oa"],
      ["ds", "judge"],
      ["ol", "judge"],
      ["oa", "judge"],
      ["judge", "out"],
    ],
  },
  {
    id: 4,
    name: "定时数据巡检",
    description: "每日 02:00 触发，HTTP 拉取指标 → 阈值判断 → 异常告警",
    status: "draft",
    nodeCount: 5,
    edgeCount: 5,
    runs: 62,
    successRate: 100,
    updatedAt: ago(320),
    mini: [
      { id: "t", x: 10, y: 50 },
      { id: "http", x: 38, y: 50 },
      { id: "calc", x: 64, y: 30 },
      { id: "alert", x: 88, y: 50 },
    ],
    miniEdges: [
      ["t", "http"],
      ["http", "calc"],
      ["calc", "alert"],
    ],
  },
];

/* ------------------------------------------------------------------ 任务 */
export type NodeStatus =
  | "succeeded"
  | "running"
  | "failed"
  | "pending"
  | "skipped"
  | "cancelled";

export interface TaskNodeRow {
  key: string;
  type: "input" | "llm" | "rag" | "tool" | "output";
  status: NodeStatus;
  durationMs: number;
  tokensIn: number;
  tokensOut: number;
  message?: string;
}

export interface TaskRow {
  id: number;
  workflow: string;
  workflowId: number;
  status: NodeStatus;
  durationMs: number;
  tokens: number;
  startedAt: string;
  input: string;
  nodes: TaskNodeRow[];
}

const NODE_SETS: Record<number, TaskNodeRow[]> = {
  1: [
    { key: "parse", type: "input", status: "succeeded", durationMs: 2, tokensIn: 0, tokensOut: 0 },
    { key: "rag", type: "rag", status: "succeeded", durationMs: 34, tokensIn: 0, tokensOut: 0, message: "命中 3 条上下文" },
    { key: "analyst", type: "llm", status: "succeeded", durationMs: 1420, tokensIn: 486, tokensOut: 890, message: "deepseek-v3" },
    { key: "clock", type: "tool", status: "succeeded", durationMs: 3, tokensIn: 0, tokensOut: 0 },
    { key: "writer", type: "llm", status: "succeeded", durationMs: 842, tokensIn: 1240, tokensOut: 1180, message: "deepseek-v3" },
    { key: "out", type: "output", status: "succeeded", durationMs: 1, tokensIn: 0, tokensOut: 0 },
  ],
  2: [
    { key: "q", type: "input", status: "succeeded", durationMs: 1, tokensIn: 0, tokensOut: 0 },
    { key: "ret", type: "rag", status: "succeeded", durationMs: 28, tokensIn: 0, tokensOut: 0, message: "命中 5 条上下文" },
    { key: "gen", type: "llm", status: "succeeded", durationMs: 1180, tokensIn: 920, tokensOut: 640 },
    { key: "out", type: "output", status: "succeeded", durationMs: 1, tokensIn: 0, tokensOut: 0 },
  ],
  3: [
    { key: "in", type: "input", status: "succeeded", durationMs: 1, tokensIn: 0, tokensOut: 0 },
    { key: "ds", type: "llm", status: "succeeded", durationMs: 1240, tokensIn: 320, tokensOut: 410 },
    { key: "ol", type: "llm", status: "running", durationMs: 0, tokensIn: 320, tokensOut: 180, message: "流式输出中" },
    { key: "oa", type: "llm", status: "succeeded", durationMs: 1620, tokensIn: 320, tokensOut: 380 },
    { key: "judge", type: "llm", status: "pending", durationMs: 0, tokensIn: 0, tokensOut: 0 },
    { key: "out", type: "output", status: "pending", durationMs: 0, tokensIn: 0, tokensOut: 0 },
  ],
};

export const TASKS: TaskRow[] = [
  {
    id: 1284,
    workflow: "技术分析工作流",
    workflowId: 1,
    status: "succeeded",
    durationMs: 2302,
    tokens: 3796,
    startedAt: ago(0.3),
    input: "请分析 NebulaFlow 调度引擎与 Worker Pool 的设计取舍",
    nodes: NODE_SETS[1],
  },
  {
    id: 1283,
    workflow: "多模型对比评测",
    workflowId: 3,
    status: "running",
    durationMs: 3180,
    tokens: 1930,
    startedAt: ago(1.2),
    input: "对比 deepseek / openai / 智谱在代码解释任务上的表现",
    nodes: NODE_SETS[3],
  },
  {
    id: 1282,
    workflow: "知识库问答",
    workflowId: 2,
    status: "succeeded",
    durationMs: 1210,
    tokens: 1560,
    startedAt: ago(4.5),
    input: "Redis Stream 崩溃后如何保证消息不丢？",
    nodes: NODE_SETS[2],
  },
  {
    id: 1279,
    workflow: "定时数据巡检",
    workflowId: 4,
    status: "failed",
    durationMs: 640,
    tokens: 0,
    startedAt: ago(18),
    input: "拉取过去 24 小时错误率",
    nodes: [
      { key: "t", type: "input", status: "succeeded", durationMs: 1, tokensIn: 0, tokensOut: 0 },
      { key: "http", type: "tool", status: "failed", durationMs: 638, tokensIn: 0, tokensOut: 0, message: "connect timeout after 600ms" },
      { key: "calc", type: "tool", status: "skipped", durationMs: 0, tokensIn: 0, tokensOut: 0 },
      { key: "alert", type: "output", status: "skipped", durationMs: 0, tokensIn: 0, tokensOut: 0 },
    ],
  },
  {
    id: 1278,
    workflow: "知识库问答",
    workflowId: 2,
    status: "succeeded",
    durationMs: 980,
    tokens: 1420,
    startedAt: ago(26),
    input: "首 token 门闩的作用是什么？",
    nodes: NODE_SETS[2],
  },
  {
    id: 1277,
    workflow: "技术分析工作流",
    workflowId: 1,
    status: "cancelled",
    durationMs: 412,
    tokens: 210,
    startedAt: ago(41),
    input: "分析超长文档（用户中途取消）",
    nodes: [
      { key: "parse", type: "input", status: "succeeded", durationMs: 2, tokensIn: 0, tokensOut: 0 },
      { key: "rag", type: "rag", status: "succeeded", durationMs: 31, tokensIn: 0, tokensOut: 0 },
      { key: "analyst", type: "llm", status: "cancelled", durationMs: 379, tokensIn: 210, tokensOut: 0, message: "用户取消" },
      { key: "clock", type: "tool", status: "cancelled", durationMs: 0, tokensIn: 0, tokensOut: 0 },
      { key: "writer", type: "llm", status: "cancelled", durationMs: 0, tokensIn: 0, tokensOut: 0 },
      { key: "out", type: "output", status: "cancelled", durationMs: 0, tokensIn: 0, tokensOut: 0 },
    ],
  },
];

/* ------------------------------------------------------------------ 知识库 */
export interface KbDoc {
  id: number;
  filename: string;
  status: "indexed" | "indexing" | "failed";
  chunks: number;
  sizeKb: number;
  createdAt: string;
}

export interface KnowledgeBase {
  id: number;
  name: string;
  description: string;
  docs: KbDoc[];
}

export const KNOWLEDGE_BASES: KnowledgeBase[] = [
  {
    id: 1,
    name: "平台技术文档",
    description: "架构说明、设计决策与运维手册",
    docs: [
      { id: 11, filename: "architecture.md", status: "indexed", chunks: 42, sizeKb: 18, createdAt: ago(60) },
      { id: 12, filename: "scheduler-design.md", status: "indexed", chunks: 28, sizeKb: 12, createdAt: ago(180) },
      { id: 13, filename: "queue-reliability.md", status: "indexed", chunks: 35, sizeKb: 15, createdAt: ago(320) },
    ],
  },
  {
    id: 2,
    name: "产品需求库",
    description: "历史 PRD 与用户反馈归集",
    docs: [
      { id: 21, filename: "prd-v2.pdf", status: "indexed", chunks: 96, sizeKb: 420, createdAt: ago(1400) },
      { id: 22, filename: "user-feedback.csv", status: "indexing", chunks: 0, sizeKb: 76, createdAt: ago(2) },
    ],
  },
];

export interface RetrieveHit {
  id: number;
  content: string;
  score: number;
  source: string;
}

export const RETRIEVE_HITS: RetrieveHit[] = [
  {
    id: 1,
    content:
      "Worker Pool 采用固定 worker 数量与带缓冲 channel，队列满时 Submit 阻塞形成背压，避免无界堆积导致内存膨胀。",
    score: 0.82,
    source: "architecture.md",
  },
  {
    id: 2,
    content:
      "优雅关闭采用两阶段：先关闭提交入口并等待在途任务结束，再排空 worker，最后超时兜底强制退出。",
    score: 0.74,
    source: "architecture.md",
  },
  {
    id: 3,
    content:
      "Redis Stream 使用 Consumer Group 实现多消费者负载均衡，XAUTOCLAIM 负责认领崩溃 worker 遗留的 pending 消息。",
    score: 0.61,
    source: "queue-reliability.md",
  },
];

/* ------------------------------------------------------------------ Provider */
export interface ProviderModel {
  name: string;
  maxTokens: number;
}

export interface Provider {
  id: number;
  name: string;
  baseUrl: string;
  status: "enabled" | "degraded" | "disabled";
  priority: number;
  latencyMs: number;
  isDefault: boolean;
  models: ProviderModel[];
}

export const PROVIDERS: Provider[] = [
  {
    id: 1,
    name: "DeepSeek",
    baseUrl: "https://api.deepseek.com/v1",
    status: "enabled",
    priority: 1,
    latencyMs: 680,
    isDefault: true,
    models: [
      { name: "deepseek-v3", maxTokens: 65536 },
      { name: "deepseek-r1", maxTokens: 65536 },
    ],
  },
  {
    id: 2,
    name: "OpenAI",
    baseUrl: "https://api.openai.com/v1",
    status: "enabled",
    priority: 2,
    latencyMs: 950,
    isDefault: false,
    models: [
      { name: "gpt-4o-mini", maxTokens: 128000 },
      { name: "gpt-4o", maxTokens: 128000 },
    ],
  },
  {
    id: 3,
    name: "智谱 AI",
    baseUrl: "https://open.bigmodel.cn/api/paas/v4",
    status: "enabled",
    priority: 3,
    latencyMs: 720,
    isDefault: false,
    models: [
      { name: "glm-4-plus", maxTokens: 128000 },
      { name: "glm-4-flash", maxTokens: 128000 },
    ],
  },
  {
    id: 4,
    name: "Anthropic",
    baseUrl: "https://api.anthropic.com/v1",
    status: "degraded",
    priority: 4,
    latencyMs: 1820,
    isDefault: false,
    models: [
      { name: "claude-3.5-sonnet", maxTokens: 200000 },
    ],
  },
];

/* ------------------------------------------------------------------ 编辑器 */
export interface EditorNode {
  id: string;
  type: "input" | "llm" | "rag" | "tool" | "output";
  x: number;
  y: number;
  config: {
    model?: string;
    prompt?: string;
    system?: string;
    tool?: string;
    knowledgeBaseId?: number;
    maxRetry?: number;
    timeoutSec?: number;
  };
}

// 编辑器初始布局。坐标刻意排得紧凑（列距 210 / 行距 150）：
// 画布在 1440 视口下只有约 700px 可用宽，图越宽 fitView 缩得越狠、字越小。
export const EDITOR_NODES: EditorNode[] = [
  { id: "parse", type: "input", x: 20, y: 170, config: {} },
  {
    id: "rag",
    type: "rag",
    x: 230,
    y: 20,
    config: { knowledgeBaseId: 1, prompt: "检索平台架构资料" },
  },
  {
    id: "analyst",
    type: "llm",
    x: 230,
    y: 170,
    config: {
      model: "deepseek-v3",
      prompt: "分析输入内容",
      system: "你是资深架构师，回答要具体到实现细节。",
      maxRetry: 2,
      timeoutSec: 60,
    },
  },
  { id: "clock", type: "tool", x: 230, y: 320, config: { tool: "time" } },
  {
    id: "writer",
    type: "llm",
    x: 440,
    y: 170,
    config: { model: "deepseek-v3", prompt: "汇总成技术报告" },
  },
  { id: "out", type: "output", x: 650, y: 170, config: {} },
];

export const EDITOR_EDGES: [string, string][] = [
  ["parse", "rag"],
  ["parse", "analyst"],
  ["parse", "clock"],
  ["rag", "writer"],
  ["analyst", "writer"],
  ["clock", "writer"],
  ["writer", "out"],
];

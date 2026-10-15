/**
 * 框架演示数据。
 *
 * 全部为前端 mock，目的是让框架脱离后端即可完整预览。
 * 接入真实接口时，把这些常量替换为 fetch 结果即可 —— 页面只消费这些类型，
 * 不关心数据来源。
 *
 * 类型定义统一从 @/types 导入。
 */

import type {
  Kpi,
  ActivityItem,
  WorkflowSummary,
  TaskRow,
  TaskNodeRow,
  Provider,
  KnowledgeBase,
  RetrievalHit,
  EditorNode,
  Agent,
  WorkflowTemplate,
  PromptTemplate,
  ProviderUsage,
  WorkflowUsage,
} from "@/types";

/** 确定性伪随机，保证每次刷新数据一致（避免视觉抖动）。 */
function seeded(seed: number) {
  let s = seed;
  return () => {
    s = (s * 1103515245 + 12345) % 2147483648;
    return s / 2147483648;
  };
}

/* ------------------------------------------------------------------ 指标 */

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

/* --------------------------------------------------------------- 成本分析 */

/**
 * 按 Provider 聚合的 token 用量。
 *
 * 与后端 `GET /api/dashboard/usage` 的 `per_provider` 同构：
 * 真实后端只提供 token 用量，**不提供任何计费字段**（没有单价表），
 * 所以 mock 侧也不再编造 cost —— 保持 demo 与线上同一套字段。
 */
export const USAGE_BY_PROVIDER: ProviderUsage[] = [
  { provider: "DeepSeek", totalRequests: 1280, inputTokens: 1_200_000, outputTokens: 620_400 },
  { provider: "OpenAI", totalRequests: 1193, inputTokens: 740_000, outputTokens: 282_200 },
  { provider: "智谱 AI", totalRequests: 320, inputTokens: 350_000, outputTokens: 162_800 },
  { provider: "Anthropic", totalRequests: 70, inputTokens: 160_000, outputTokens: 70_600 },
];

/** 每个工作流的任务数（由任务列表聚合，后端没有单独的聚合接口）。 */
export const MOCK_WORKFLOW_USAGE: WorkflowUsage[] = [
  { id: 1, name: "技术分析工作流", tasks: 3 },
  { id: 2, name: "多模型对比评测", tasks: 1 },
  { id: 3, name: "知识库问答", tasks: 1 },
];

/* ------------------------------------------------------------------ 活动流 */

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
  { id: "a8", at: ago(9.4), kind: "system", text: "Provider 故障转移", meta: "deepseek → openai" },
];

/* ------------------------------------------------------------------ 工作流 */

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
    avgDurationMs: 2302,
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
    avgDurationMs: 1210,
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
    avgDurationMs: 3180,
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
    avgDurationMs: 640,
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

export const NODE_SETS: Record<number, TaskNodeRow[]> = {
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

export const RETRIEVE_HITS: RetrievalHit[] = [
  {
    id: 1,
    content:
      "Worker Pool 采用固定 worker 数量与带缓冲 channel，队列满时 Submit 阻塞形成背压，避免无界堆积导致内存膨胀。",
    score: 0.82,
    filename: "architecture.md",
  },
  {
    id: 2,
    content:
      "优雅关闭采用两阶段：先关闭提交入口并等待在途任务结束，再排空 worker，最后超时兜底强制退出。",
    score: 0.74,
    filename: "architecture.md",
  },
  {
    id: 3,
    content:
      "Redis Stream 使用 Consumer Group 实现多消费者负载均衡，XAUTOCLAIM 负责认领崩溃 worker 遗留的 pending 消息。",
    score: 0.61,
    filename: "queue-reliability.md",
  },
];

/* ------------------------------------------------------------------ Provider */

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

/* ------------------------------------------------------------------ Agent */

export const AGENTS: Agent[] = [
  {
    id: 1,
    userId: 1,
    name: "代码评审专家",
    description: "基于 deepseek-r1 的深度代码分析 Agent，擅长发现隐藏的性能与安全问题。",
    runtimeType: "native",
    model: "deepseek-r1",
    capabilities: ["代码审查", "性能优化", "安全审计", "重构建议"],
    status: "active",
    version: "1.2.0",
    timeoutSec: 120,
    createdAt: ago(60 * 24 * 7),
    updatedAt: ago(60 * 24 * 2),
  },
  {
    id: 2,
    userId: 1,
    name: "文档摘要助手",
    description: "快速处理长文档，提取核心观点与行动项，支持多格式输入。",
    runtimeType: "langchain",
    model: "gpt-4o-mini",
    capabilities: ["文档摘要", "信息提取", "多格式支持", "结构化输出"],
    status: "active",
    version: "2.0.1",
    timeoutSec: 60,
    createdAt: ago(60 * 24 * 14),
    updatedAt: ago(60 * 24 * 1),
  },
  {
    id: 3,
    userId: 1,
    name: "数据分析师",
    description: "HTTP 服务型 Agent，连接内部数据平台，提供 SQL 查询与可视化建议。",
    runtimeType: "http",
    endpoint: "https://agents.internal.example.com/analyst",
    capabilities: ["SQL 生成", "数据分析", "图表建议", "趋势预测"],
    status: "inactive",
    version: "0.9.3",
    timeoutSec: 180,
    createdAt: ago(60 * 24 * 30),
    updatedAt: ago(60 * 24 * 5),
  },
  {
    id: 4,
    userId: 1,
    name: "翻译专员",
    description: "专业级中英互译 Agent，保留技术术语准确性，支持批量翻译。",
    runtimeType: "native",
    model: "glm-4-plus",
    capabilities: ["中英互译", "技术术语", "批量处理", "格式保留"],
    status: "error",
    version: "1.0.0",
    timeoutSec: 30,
    createdAt: ago(60 * 24 * 20),
    updatedAt: ago(60 * 12),
  },
];

export const MARKETPLACE_AGENTS: Agent[] = [
  ...AGENTS.map((a) => ({ ...a })),
  {
    id: 101,
    userId: 0,
    name: "产品需求拆解",
    description: "将模糊的产品想法拆解为结构化的 PRD 文档，包含用户故事与验收标准。",
    runtimeType: "langchain",
    model: "gpt-4o",
    capabilities: ["需求分析", "PRD 生成", "用户故事", "验收标准"],
    status: "active",
    version: "1.5.0",
    timeoutSec: 90,
    createdAt: ago(60 * 24 * 60),
    updatedAt: ago(60 * 24 * 3),
  },
  {
    id: 102,
    userId: 0,
    name: "测试用例生成器",
    description: "根据代码或需求自动生成全面的测试用例，覆盖边界条件与异常路径。",
    runtimeType: "native",
    model: "deepseek-v3",
    capabilities: ["单元测试", "集成测试", "边界用例", "代码覆盖率"],
    status: "active",
    version: "2.1.0",
    timeoutSec: 120,
    createdAt: ago(60 * 24 * 45),
    updatedAt: ago(60 * 24 * 7),
  },
  {
    id: 103,
    userId: 0,
    name: "客服机器人",
    description: "HTTP 接入的智能客服 Agent，支持多轮对话与工单自动创建。",
    runtimeType: "http",
    endpoint: "https://service-agents.example.com/customer-bot",
    capabilities: ["多轮对话", "FAQ 问答", "工单创建", "情绪识别"],
    status: "active",
    version: "3.0.0",
    timeoutSec: 60,
    createdAt: ago(60 * 24 * 90),
    updatedAt: ago(60 * 24 * 1),
  },
];

/* --------------------------------------------------------- Prompt 模板 */

export const PROMPT_TEMPLATES: PromptTemplate[] = [
  {
    id: "tpl-analyst",
    name: "技术分析专家",
    category: "分析",
    system:
      "你是一名资深技术分析师，擅长从复杂信息中提取关键结论。回答要求结构清晰、逻辑严谨，优先使用 bullet points。",
    prompt: "请分析以下内容，给出：1) 核心结论  2) 关键数据  3) 风险与建议\n\n{{input}}",
  },
  {
    id: "tpl-summarize",
    name: "内容摘要",
    category: "通用",
    prompt: "请用不超过 300 字总结以下内容，保留关键信息和数据：\n\n{{input}}",
  },
  {
    id: "tpl-translate-en",
    name: "中英互译",
    category: "翻译",
    prompt:
      "请将以下内容翻译成中文（如果已是中文则翻译成英文），保持专业术语准确：\n\n{{input}}",
  },
  {
    id: "tpl-code-review",
    name: "代码评审",
    category: "代码",
    system: "你是一名严格的代码评审专家，关注正确性、性能、可读性和安全性。",
    prompt: "请评审以下代码，从正确性、性能、可读性、安全性四个维度给出反馈：\n\n```\n{{input}}\n```",
  },
  {
    id: "tpl-technical-report",
    name: "技术报告",
    category: "写作",
    prompt:
      "请根据以下信息撰写一份结构化技术报告，包含：摘要、背景、方法、结果、结论五个部分：\n\n{{input}}",
  },
  {
    id: "tpl-rag-answer",
    name: "知识库问答",
    category: "通用",
    system: "请基于提供的参考资料回答用户问题。如果资料中没有答案，请明确说明。",
    prompt:
      "参考资料：\n{{context}}\n\n用户问题：{{query}}\n\n请基于参考资料回答：",
  },
];

/* ---------------------------------------------------------- 工作流模板 */

export const WORKFLOW_TEMPLATES: WorkflowTemplate[] = [
  {
    id: 1,
    name: "智能文章写作助手",
    description: "输入主题和关键词，自动生成结构完整、逻辑清晰的专业文章，支持多种文风调整。",
    category: "内容创作",
    icon: "📝",
    nodes: 5,
    usedCount: 1284,
    createdAt: "2024-10-15T08:00:00Z",
  },
  {
    id: 2,
    name: "社交媒体内容生成",
    description: "一键生成小红书、微博、公众号等多平台文案，自动适配各平台风格与字数限制。",
    category: "内容创作",
    icon: "📱",
    nodes: 4,
    usedCount: 956,
    createdAt: "2024-11-02T10:30:00Z",
  },
  {
    id: 3,
    name: "销售数据洞察分析",
    description: "上传销售报表，自动识别趋势异常、计算同比环比、生成可视化分析报告。",
    category: "数据分析",
    icon: "📊",
    nodes: 6,
    usedCount: 2103,
    createdAt: "2024-09-20T14:00:00Z",
  },
  {
    id: 4,
    name: "用户反馈情感分析",
    description: "批量处理用户评论和反馈，自动分类情感倾向，提取关键词和改进建议。",
    category: "客户服务",
    icon: "💬",
    nodes: 5,
    usedCount: 1567,
    createdAt: "2024-10-08T09:15:00Z",
  },
  {
    id: 5,
    name: "智能客服工单处理",
    description: "自动分类客户工单、提取关键信息、生成标准回复建议，大幅提升客服效率。",
    category: "客户服务",
    icon: "🎧",
    nodes: 7,
    usedCount: 3421,
    createdAt: "2024-08-25T11:45:00Z",
  },
  {
    id: 6,
    name: "代码审查自动化",
    description: "自动扫描代码变更，检测潜在 Bug、性能问题和安全漏洞，生成审查报告。",
    category: "研发提效",
    icon: "🔍",
    nodes: 6,
    usedCount: 1876,
    createdAt: "2024-11-10T16:20:00Z",
  },
  {
    id: 7,
    name: "API 文档自动生成",
    description: "解析代码注释和类型定义，自动生成结构化的 API 文档，支持多种输出格式。",
    category: "研发提效",
    icon: "📚",
    nodes: 4,
    usedCount: 892,
    createdAt: "2024-12-01T08:30:00Z",
  },
  {
    id: 8,
    name: "智能题库生成器",
    description: "根据知识点和难度要求，自动生成选择题、判断题、简答题等多种题型的试卷。",
    category: "教育学习",
    icon: "📖",
    nodes: 5,
    usedCount: 2345,
    createdAt: "2024-09-15T13:00:00Z",
  },
  {
    id: 9,
    name: "外语学习助手",
    description: "智能生成词汇练习、语法讲解、情景对话和写作批改，个性化提升语言能力。",
    category: "教育学习",
    icon: "🌍",
    nodes: 6,
    usedCount: 1678,
    createdAt: "2024-10-28T10:00:00Z",
  },
  {
    id: 10,
    name: "财务报表智能分析",
    description: "上传财务数据，自动生成利润分析、现金流预测、风险评估等多维度分析报告。",
    category: "数据分析",
    icon: "💰",
    nodes: 7,
    usedCount: 1123,
    createdAt: "2024-11-20T15:30:00Z",
  },
];

/**
 * 中文（简体）翻译字典。
 *
 * 键名采用「语义路径」而不是「原文」，避免改动中文文案时所有语言文件都要改。
 * 嵌套结构方便按模块分组查找。
 */
export const zhCN = {
  common: {
    save: "保存",
    cancel: "取消",
    delete: "删除",
    edit: "编辑",
    confirm: "确认",
    loading: "加载中…",
    error: "出错了",
    retry: "重试",
    search: "搜索",
    refresh: "刷新",
    export: "导出",
    total: "共",
    items: "条",
  },
  nav: {
    overview: "总览",
    dashboard: "控制台",
    orchestration: "编排",
    workflows: "工作流",
    tasks: "任务",
    data: "数据",
    knowledge: "知识库",
    settings: "配置",
    models: "模型与供应商",
    analytics: "分析",
    cost: "成本分析",
  },
  topbar: {
    notifications: "通知",
    theme: "主题",
  },
  sidebar: {
    allSystemsNormal: "所有系统正常",
    engine: "工作流引擎 · DAG 调度 · 模型路由",
    logout: "退出登录",
  },
  dashboard: {
    title: "系统概览",
    subtitle: "实时指标",
    lastUpdated: "最后更新",
    ranges: {
      "24h": "24 小时",
      "7d": "7 天",
      "30d": "30 天",
    },
    throughput: "任务吞吐量",
    workerUtil: "Worker 利用率",
    activity: "实时活动",
    tokenDistribution: "Token 消耗分布",
    recentTasks: "最近任务",
    viewAll: "查看全部",
  },
  cost: {
    title: "成本分析",
    subtitle: "Token 消耗与费用明细",
    totalCost: "总费用",
    totalTokens: "Token 消耗",
    totalCalls: "调用次数",
    avgCostPerTask: "单次均价",
    trend: "成本趋势",
    byModel: "按模型分布",
    modelDetail: "模型费用明细",
    byWorkflow: "工作流费用排行",
    perTask: "/次",
  },
} as const;

export type TranslationDict = typeof zhCN;

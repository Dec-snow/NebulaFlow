import type { TranslationDict } from "./zh-CN";

/**
 * 英文翻译字典。
 *
 * 保持与 zh-CN 相同的嵌套结构，类型一致确保不会漏翻译。
 */
export const enUS: TranslationDict = {
  common: {
    save: "Save",
    cancel: "Cancel",
    delete: "Delete",
    edit: "Edit",
    confirm: "Confirm",
    loading: "Loading…",
    error: "Something went wrong",
    retry: "Retry",
    search: "Search",
    refresh: "Refresh",
    export: "Export",
    total: "Total",
    items: "items",
  },
  nav: {
    overview: "Overview",
    dashboard: "Dashboard",
    orchestration: "Orchestration",
    workflows: "Workflows",
    tasks: "Tasks",
    data: "Data",
    knowledge: "Knowledge",
    settings: "Settings",
    models: "Models & Providers",
    analytics: "Analytics",
    cost: "Cost Analysis",
  },
  topbar: {
    notifications: "Notifications",
    theme: "Theme",
  },
  sidebar: {
    allSystemsNormal: "All systems normal",
    engine: "Workflow Engine · DAG Scheduling · Model Routing",
    logout: "Sign out",
  },
  dashboard: {
    title: "System Overview",
    subtitle: "Real-time metrics",
    lastUpdated: "Last updated",
    ranges: {
      "24h": "24 hours",
      "7d": "7 days",
      "30d": "30 days",
    },
    throughput: "Task Throughput",
    workerUtil: "Worker Utilization",
    activity: "Live Activity",
    tokenDistribution: "Token Distribution",
    recentTasks: "Recent Tasks",
    viewAll: "View all",
  },
  cost: {
    title: "Cost Analysis",
    subtitle: "Token consumption & expenses",
    totalCost: "Total Cost",
    totalTokens: "Token Usage",
    totalCalls: "Total Calls",
    avgCostPerTask: "Avg per Task",
    trend: "Cost Trend",
    byModel: "By Model",
    modelDetail: "Model Cost Detail",
    byWorkflow: "Workflow Cost Ranking",
    perTask: "/task",
  },
};

import { lazy, Suspense } from "react";
import { Route, Routes } from "react-router-dom";
import { AppShell } from "@/components/shell/AppShell";
import { PageLoader } from "@/components/ui";
import Login from "@/pages/Login";

// 懒加载页面组件（首屏不需要的页面）
const Dashboard = lazy(() => import("@/pages/Dashboard"));
const Workflows = lazy(() => import("@/pages/Workflows"));
const WorkflowEditor = lazy(() => import("@/pages/WorkflowEditor"));
const Templates = lazy(() => import("@/pages/Templates"));
const Tasks = lazy(() => import("@/pages/Tasks"));
const TaskDetail = lazy(() => import("@/pages/TaskDetail"));
const Agents = lazy(() => import("@/pages/Agents"));
const AgentMarketplace = lazy(() => import("@/pages/AgentMarketplace"));
const Knowledge = lazy(() => import("@/pages/Knowledge"));
const Models = lazy(() => import("@/pages/Models"));
const CostAnalysis = lazy(() => import("@/pages/CostAnalysis"));
const NotFound = lazy(() => import("@/pages/NotFound"));

/**
 * 路由表。
 *
 * 布局分两类：
 * - /login 独立全屏页（静态导入，首屏需要）
 * - 其余挂在 AppShell 之下，共享侧边栏与顶栏（懒加载，按需加载）
 */
export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />

      <Route
        path="/"
        element={
          <Suspense fallback={<PageLoader text="加载中…" />}>
            <AppShell />
          </Suspense>
        }
      >
        <Route index element={<Dashboard />} />
        <Route path="workflows" element={<Workflows />} />
        <Route path="workflows/:id" element={<WorkflowEditor />} />
        <Route path="templates" element={<Templates />} />
        <Route path="tasks" element={<Tasks />} />
        <Route path="tasks/:id" element={<TaskDetail />} />
        <Route path="agents" element={<Agents />} />
        <Route path="agents/marketplace" element={<AgentMarketplace />} />
        <Route path="knowledge" element={<Knowledge />} />
        <Route path="models" element={<Models />} />
        <Route path="cost" element={<CostAnalysis />} />
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}

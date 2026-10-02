import { Route, Routes } from "react-router-dom";
import { AppShell } from "@/components/shell/AppShell";
import AgentMarketplace from "@/pages/AgentMarketplace";
import Agents from "@/pages/Agents";
import CostAnalysis from "@/pages/CostAnalysis";
import Dashboard from "@/pages/Dashboard";
import Knowledge from "@/pages/Knowledge";
import Login from "@/pages/Login";
import Models from "@/pages/Models";
import NotFound from "@/pages/NotFound";
import TaskDetail from "@/pages/TaskDetail";
import Tasks from "@/pages/Tasks";
import Templates from "@/pages/Templates";
import WorkflowEditor from "@/pages/WorkflowEditor";
import Workflows from "@/pages/Workflows";

/**
 * 路由表。
 *
 * 布局分两类：
 * - /login 独立全屏页
 * - 其余挂在 AppShell 之下，共享侧边栏与顶栏
 */
export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />

      <Route path="/" element={<AppShell />}>
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

import { Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "@/components/shell/AppShell";
import Dashboard from "@/pages/Dashboard";
import Knowledge from "@/pages/Knowledge";
import Login from "@/pages/Login";
import Models from "@/pages/Models";
import TaskDetail from "@/pages/TaskDetail";
import Tasks from "@/pages/Tasks";
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
        <Route path="tasks" element={<Tasks />} />
        <Route path="tasks/:id" element={<TaskDetail />} />
        <Route path="knowledge" element={<Knowledge />} />
        <Route path="models" element={<Models />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}

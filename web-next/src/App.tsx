import { lazy, Suspense, useEffect, type ReactNode } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { AppShell } from "@/components/shell/AppShell";
import { PageLoader } from "@/components/ui";
import Login from "@/pages/Login";
import { setUnauthorizedHandler } from "@/lib/auth";
import { useSession } from "@/lib/session";

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
 * 登录守卫。
 *
 * 没有 token 时不渲染控制台，直接跳登录页 —— 否则页面会先发一轮
 * 必然 401 的请求，再被"401 → 清理会话"弹回登录页，用户看到的是
 * 闪一下的空白页 + 一串 console 报错。
 */
function RequireAuth({ children }: { children: ReactNode }) {
  const loggedIn = useSession((s) => s.loggedIn);
  const location = useLocation();
  if (!loggedIn) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }
  return <>{children}</>;
}

/**
 * 路由表。
 *
 * 布局分两类：
 * - /login 独立全屏页（静态导入，首屏需要）
 * - 其余挂在 AppShell 之下，共享侧边栏与顶栏（懒加载，按需加载）
 */
export default function App() {
  const navigate = useNavigate();
  const clearLocal = useSession((s) => s.clearLocal);

  // API 客户端拿到 401 时（token 过期 / 被吊销）：清会话并回登录页
  useEffect(() => {
    setUnauthorizedHandler(() => {
      clearLocal();
      navigate("/login", { replace: true });
    });
    return () => setUnauthorizedHandler(null);
  }, [clearLocal, navigate]);

  return (
    <Routes>
      <Route path="/login" element={<Login />} />

      <Route
        path="/"
        element={
          <RequireAuth>
            <Suspense fallback={<PageLoader text="加载中…" />}>
              <AppShell />
            </Suspense>
          </RequireAuth>
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

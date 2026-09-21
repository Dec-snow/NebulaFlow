import { Outlet, useNavigate } from "react-router-dom";
import { useSession } from "@/lib/session";
import { ErrorBoundary } from "@/components/ui";
import { Sidebar } from "./Sidebar";
import { Topbar } from "./Topbar";

/**
 * 应用外壳：侧边栏 + 顶栏 + 内容区。
 *
 * 布局要点：
 * - 整屏高度锁定（h-screen + overflow-hidden），滚动只发生在内容区，
 *   这样侧边栏与顶栏始终可见。
 * - app-ambient 提供极淡的双色环境光，是"清新"观感的主要来源。
 * - 内容最大宽度 1400px 居中，避免超宽屏下文字行过长。
 */
export function AppShell() {
  const navigate = useNavigate();
  const username = useSession((s) => s.username);
  const logout = useSession((s) => s.logout);

  return (
    <div className="app-ambient flex h-screen overflow-hidden">
      <Sidebar
        username={username || "访客"}
        onLogout={() => {
          logout();
          navigate("/login");
        }}
      />

      <div className="relative z-10 flex min-w-0 flex-1 flex-col">
        <Topbar username={username || "访客"} />

        <main className="scrollbar-none flex-1 overflow-y-auto">
          <div className="mx-auto w-full max-w-[1400px] px-6 py-6">
            <ErrorBoundary
              title="页面加载失败"
              description="当前页面出现了错误，侧边栏和顶栏仍然可用，你可以切换到其他页面继续操作。"
            >
              <Outlet />
            </ErrorBoundary>
          </div>
        </main>
      </div>
    </div>
  );
}

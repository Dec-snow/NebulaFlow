import { NavLink, Outlet, useNavigate } from "react-router-dom";
import { Boxes, Database, Gauge, GitBranch, LayoutDashboard, LogOut, Zap } from "lucide-react";
import { useAuth } from "../store/auth";

const nav = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard, end: true },
  { to: "/workflows", label: "Workflows", icon: GitBranch },
  { to: "/tasks", label: "Tasks", icon: Zap },
  { to: "/knowledge", label: "Knowledge", icon: Database },
  { to: "/models", label: "Models", icon: Boxes },
];

export default function Layout() {
  const { username, logout } = useAuth();
  const navigate = useNavigate();

  return (
    <div className="flex h-screen">
      {/* 侧边栏 */}
      <aside className="w-56 shrink-0 bg-nebula-900 border-r border-nebula-800 flex flex-col">
        <div className="px-5 py-5 border-b border-nebula-800">
          <div className="flex items-center gap-2">
            <div className="w-2 h-2 rounded-full bg-nebula-400 animate-pulse" />
            <span className="text-slate-100 font-semibold tracking-wide">NebulaFlow</span>
          </div>
          <p className="text-[10px] text-slate-500 mt-1">AI Agent Workflow Platform</p>
        </div>

        <nav className="flex-1 py-4 px-3 space-y-1">
          {nav.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                `flex items-center gap-3 px-3 py-2 rounded-lg text-sm transition-colors ${
                  isActive
                    ? "bg-nebula-800 text-nebula-300 border border-nebula-700"
                    : "text-slate-400 hover:bg-nebula-800/60 hover:text-slate-200"
                }`
              }
            >
              <Icon size={16} />
              {label}
            </NavLink>
          ))}
        </nav>

        <div className="p-4 border-t border-nebula-800">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2 min-w-0">
              <div className="w-7 h-7 rounded-full bg-nebula-700 flex items-center justify-center text-xs font-bold text-nebula-300 shrink-0">
                {username?.[0]?.toUpperCase()}
              </div>
              <span className="text-xs text-slate-300 truncate">{username}</span>
            </div>
            <button
              title="退出登录"
              onClick={() => {
                logout();
                navigate("/login");
              }}
              className="text-slate-500 hover:text-red-400 transition-colors"
            >
              <LogOut size={15} />
            </button>
          </div>
        </div>
      </aside>

      {/* 主内容 */}
      <main className="flex-1 overflow-y-auto">
        <div className="max-w-7xl mx-auto p-6">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

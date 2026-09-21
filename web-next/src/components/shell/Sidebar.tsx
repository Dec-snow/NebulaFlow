import { useEffect, useState } from "react";
import { NavLink } from "react-router-dom";
import {
  Activity,
  Boxes,
  ChevronsLeft,
  ChevronsRight,
  Database,
  GitBranch,
  LayoutDashboard,
  LogOut,
  type LucideIcon,
} from "lucide-react";
import { Avatar } from "@/components/ui";
import { cn } from "@/lib/utils";
import { BrandMark } from "./Brand";

interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
  /** 右侧计数徽标 */
  badge?: number;
  badgeTone?: "brand" | "mint" | "sky";
}

interface NavGroup {
  title: string;
  items: NavItem[];
}

const GROUPS: NavGroup[] = [
  {
    title: "总览",
    items: [{ to: "/", label: "控制台", icon: LayoutDashboard, end: true }],
  },
  {
    title: "编排",
    items: [
      { to: "/workflows", label: "工作流", icon: GitBranch },
      { to: "/tasks", label: "任务", icon: Activity, badge: 2, badgeTone: "sky" },
    ],
  },
  {
    title: "数据",
    items: [{ to: "/knowledge", label: "知识库", icon: Database }],
  },
  {
    title: "配置",
    items: [{ to: "/models", label: "模型与供应商", icon: Boxes }],
  },
];

const BADGE_TONE: Record<string, string> = {
  brand: "bg-brand-soft text-brand",
  mint: "bg-mint/12 text-mint",
  sky: "bg-sky/12 text-sky",
};

const COLLAPSE_KEY = "nf-sidebar-collapsed";

export function Sidebar({
  username,
  onLogout,
}: {
  username: string;
  onLogout: () => void;
}) {
  const [collapsed, setCollapsed] = useState(() => {
    try {
      return localStorage.getItem(COLLAPSE_KEY) === "1";
    } catch {
      return false;
    }
  });

  useEffect(() => {
    try {
      localStorage.setItem(COLLAPSE_KEY, collapsed ? "1" : "0");
    } catch {
      /* 忽略 */
    }
  }, [collapsed]);

  return (
    <aside
      className={cn(
        "relative z-10 flex shrink-0 flex-col border-r border-line bg-surface/60",
        "transition-[width] duration-300 ease-smooth",
        collapsed ? "w-[68px]" : "w-[236px]",
      )}
    >
      {/* 品牌 */}
      <div
        className={cn(
          "flex h-16 items-center border-b border-line px-4",
          collapsed && "justify-center px-0",
        )}
      >
        <NavLink to="/" className="flex min-w-0 items-center gap-2.5">
          <BrandMark size={30} />
          {!collapsed && (
            <div className="min-w-0 leading-tight">
              <div className="truncate text-sm font-semibold tracking-tight text-fg">
                Nebula<span className="text-gradient">Flow</span>
              </div>
              <div className="truncate text-2xs text-fg-subtle">AI Agent Platform</div>
            </div>
          )}
        </NavLink>
      </div>

      {/* 导航 */}
      <nav className="scrollbar-none flex-1 overflow-y-auto px-3 py-4">
        {GROUPS.map((group, gi) => (
          <div key={group.title} className={cn(gi > 0 && "mt-5")}>
            {!collapsed && (
              <div className="mb-2 px-2 text-2xs font-semibold uppercase tracking-widest text-fg-subtle">
                {group.title}
              </div>
            )}
            <div className="space-y-0.5">
              {group.items.map((item) => (
                <SidebarLink key={item.to} item={item} collapsed={collapsed} />
              ))}
            </div>
          </div>
        ))}
      </nav>

      {/* 系统状态 */}
      {!collapsed && (
        <div className="mx-3 mb-3 rounded-lg border border-line bg-surface-2/60 px-3 py-2.5">
          <div className="flex items-center gap-2">
            <span className="dot dot-mint" />
            <span className="text-2xs font-medium text-fg">所有系统正常</span>
          </div>
          <p className="mt-1 text-2xs leading-relaxed text-fg-subtle">
            工作流引擎 · DAG 调度 · 模型路由
          </p>
        </div>
      )}

      {/* 用户 + 折叠开关 */}
      <div className="border-t border-line p-3">
        <div className={cn("flex items-center gap-2.5", collapsed && "justify-center")}>
          <Avatar name={username} size={30} />
          {!collapsed && (
            <>
              <div className="min-w-0 flex-1">
                <div className="truncate text-xs font-medium text-fg">{username}</div>
                <div className="truncate text-2xs text-fg-subtle">开发者</div>
              </div>
              <button
                onClick={onLogout}
                title="退出登录"
                aria-label="退出登录"
                className="rounded-md p-1.5 text-fg-subtle transition-colors hover:bg-rose/10 hover:text-rose"
              >
                <LogOut size={15} />
              </button>
            </>
          )}
        </div>

        <button
          onClick={() => setCollapsed((c) => !c)}
          title={collapsed ? "展开侧边栏" : "收起侧边栏"}
          aria-label={collapsed ? "展开侧边栏" : "收起侧边栏"}
          className={cn(
            "mt-3 flex w-full items-center gap-2 rounded-md px-2 py-1.5",
            "text-2xs text-fg-subtle transition-colors hover:bg-surface-2 hover:text-fg",
            collapsed && "justify-center",
          )}
        >
          {collapsed ? <ChevronsRight size={14} /> : <ChevronsLeft size={14} />}
          {!collapsed && <span>收起</span>}
        </button>
      </div>
    </aside>
  );
}

function SidebarLink({ item, collapsed }: { item: NavItem; collapsed: boolean }) {
  const { to, label, icon: Icon, end, badge, badgeTone = "brand" } = item;

  return (
    <NavLink
      to={to}
      end={end}
      title={collapsed ? label : undefined}
      className={({ isActive }) =>
        cn(
          "group relative flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm",
          "transition-all duration-150 ease-smooth",
          collapsed && "justify-center px-0",
          isActive
            ? "bg-brand-soft font-medium text-brand"
            : "text-fg-muted hover:bg-surface-2 hover:text-fg",
        )
      }
    >
      {({ isActive }) => (
        <>
          {isActive && (
            <span className="absolute left-0 top-1/2 h-4 w-[3px] -translate-y-1/2 rounded-r-full bg-brand" />
          )}
          <Icon size={16.5} strokeWidth={2} className="shrink-0" />
          {!collapsed && (
            <>
              <span className="flex-1 truncate">{label}</span>
              {badge != null && (
                <span
                  className={cn(
                    "tnum rounded px-1.5 py-px text-2xs font-medium",
                    BADGE_TONE[badgeTone],
                  )}
                >
                  {badge}
                </span>
              )}
            </>
          )}
        </>
      )}
    </NavLink>
  );
}

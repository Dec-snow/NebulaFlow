import { useEffect, useState } from "react";
import { NavLink } from "react-router-dom";
import {
  Activity,
  BarChart3,
  Boxes,
  ChevronsLeft,
  ChevronsRight,
  Database,
  GitBranch,
  Globe,
  LayoutDashboard,
  LogOut,
  type LucideIcon,
} from "lucide-react";
import { Avatar } from "@/components/ui";
import { cn } from "@/lib/utils";
import { useI18n, type Locale } from "@/i18n";
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

const BADGE_TONE: Record<string, string> = {
  brand: "bg-brand-soft text-brand",
  mint: "bg-mint/12 text-mint",
  sky: "bg-sky/12 text-sky",
};

const COLLAPSE_KEY = "nf-sidebar-collapsed";

const LANGS: { locale: Locale; label: string; short: string }[] = [
  { locale: "zh-CN", label: "简体中文", short: "中" },
  { locale: "en-US", label: "English", short: "EN" },
];

/**
 * 根据 i18n 字典生成导航组配置。
 * 图标与路径是结构化数据，label / title 走翻译。
 */
function buildNavGroups(t: (key: string) => string): NavGroup[] {
  return [
    {
      title: t("nav.overview"),
      items: [{ to: "/", label: t("nav.dashboard"), icon: LayoutDashboard, end: true }],
    },
    {
      title: t("nav.orchestration"),
      items: [
        { to: "/workflows", label: t("nav.workflows"), icon: GitBranch },
        { to: "/tasks", label: t("nav.tasks"), icon: Activity, badge: 2, badgeTone: "sky" },
      ],
    },
    {
      title: t("nav.data"),
      items: [{ to: "/knowledge", label: t("nav.knowledge"), icon: Database }],
    },
    {
      title: t("nav.settings"),
      items: [{ to: "/models", label: t("nav.models"), icon: Boxes }],
    },
    {
      title: t("nav.analytics"),
      items: [{ to: "/cost", label: t("nav.cost"), icon: BarChart3 }],
    },
  ];
}

export function Sidebar({
  username,
  onLogout,
}: {
  username: string;
  onLogout: () => void;
}) {
  const { locale, setLocale, t } = useI18n();
  const groups = buildNavGroups(t);
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
        {groups.map((group, gi) => (
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
            <span className="text-2xs font-medium text-fg">{t("sidebar.allSystemsNormal")}</span>
          </div>
          <p className="mt-1 text-2xs leading-relaxed text-fg-subtle">
            {t("sidebar.engine")}
          </p>
        </div>
      )}

      {/* 用户 + 语言切换 + 折叠开关 */}
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
                title={t("sidebar.logout")}
                aria-label={t("sidebar.logout")}
                className="rounded-md p-1.5 text-fg-subtle transition-colors hover:bg-rose/10 hover:text-rose"
              >
                <LogOut size={15} />
              </button>
            </>
          )}
        </div>

        {/* 语言切换 */}
        {!collapsed && (
          <div className="mt-3 flex items-center gap-1 rounded-lg border border-line bg-surface-2/60 p-1">
            {LANGS.map((lang) => (
              <button
                key={lang.locale}
                onClick={() => setLocale(lang.locale)}
                title={lang.label}
                className={cn(
                  "flex flex-1 items-center justify-center gap-1 rounded-md px-2 py-1 text-2xs font-medium transition-colors",
                  locale === lang.locale
                    ? "bg-brand text-white shadow-sm"
                    : "text-fg-subtle hover:bg-surface hover:text-fg",
                )}
              >
                <Globe size={12} />
                {lang.short}
              </button>
            ))}
          </div>
        )}
        {collapsed && (
          <button
            onClick={() => {
              const idx = LANGS.findIndex((l) => l.locale === locale);
              const next = LANGS[(idx + 1) % LANGS.length];
              setLocale(next.locale);
            }}
            title="切换语言 / Toggle language"
            className="mt-3 flex w-full items-center justify-center rounded-md p-1.5 text-fg-subtle transition-colors hover:bg-surface-2 hover:text-fg"
          >
            <Globe size={15} />
          </button>
        )}

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

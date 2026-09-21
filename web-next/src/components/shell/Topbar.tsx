import { useLocation } from "react-router-dom";
import { Bell, Command, Search } from "lucide-react";
import { Avatar, IconButton } from "@/components/ui";
import { cn } from "@/lib/utils";
import { ThemeToggle } from "./ThemeToggle";

/** 路由 → 标题/说明。用于顶栏展示当前位置。 */
const TITLES: Record<string, { title: string; subtitle: string }> = {
  "/": { title: "Dashboard", subtitle: "系统实时概览" },
  "/workflows": { title: "工作流", subtitle: "编排与发布 DAG" },
  "/tasks": { title: "任务", subtitle: "执行记录与实时日志" },
  "/knowledge": { title: "知识库", subtitle: "文档索引与 RAG 检索" },
  "/models": { title: "模型与供应商", subtitle: "LLM Provider 配置" },
};

function resolveTitle(pathname: string) {
  if (TITLES[pathname]) return TITLES[pathname];
  if (pathname.startsWith("/workflows/"))
    return { title: "工作流编辑器", subtitle: "拖拽编排节点与连线" };
  if (pathname.startsWith("/tasks/"))
    return { title: "任务详情", subtitle: "节点执行明细与事件流" };
  return { title: "NebulaFlow", subtitle: "" };
}

export function Topbar({ username }: { username: string }) {
  const { pathname } = useLocation();
  const { title, subtitle } = resolveTitle(pathname);

  return (
    <header className="glass sticky top-0 z-20 border-b border-line">
      <div className="flex h-16 items-center gap-4 px-6">
        {/* 当前页面 */}
        <div className="min-w-0">
          <h1 className="truncate text-[15px] font-semibold tracking-tight text-fg">
            {title}
          </h1>
          {subtitle && (
            <p className="truncate text-2xs text-fg-subtle">{subtitle}</p>
          )}
        </div>

        {/* 搜索（占位，演示视觉） */}
        <div className="ml-auto hidden max-w-xs flex-1 md:block">
          <button
            className={cn(
              "group flex h-9 w-full items-center gap-2 rounded-lg border border-line",
              "bg-surface/70 px-3 text-left text-xs text-fg-subtle",
              "transition-colors hover:border-line-strong hover:bg-surface",
            )}
          >
            <Search size={14} className="shrink-0" />
            <span className="flex-1">搜索工作流、任务…</span>
            <span className="kbd">
              <Command size={10} />
              <span className="ml-0.5">K</span>
            </span>
          </button>
        </div>

        <div className="ml-auto flex items-center gap-2 md:ml-0">
          <ThemeToggle />
          <div className="relative">
            <IconButton label="通知" variant="ghost">
              <Bell size={16.5} />
            </IconButton>
            <span className="absolute right-1.5 top-1.5 h-1.5 w-1.5 rounded-full bg-rose ring-2 ring-surface" />
          </div>
          <div className="mx-1 h-5 w-px bg-line" />
          <Avatar name={username} size={30} />
        </div>
      </div>
    </header>
  );
}

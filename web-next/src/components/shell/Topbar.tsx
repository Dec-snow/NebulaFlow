import { useLocation } from "react-router-dom";
import { Bell } from "lucide-react";
import { Avatar, IconButton } from "@/components/ui";
import { ThemeToggle } from "./ThemeToggle";

/** 路由 → 标题/说明。用于顶栏展示当前位置。 */
const TITLES: Record<string, { title: string; subtitle: string }> = {
  "/": { title: "控制台", subtitle: "系统实时概览" },
  "/workflows": { title: "工作流", subtitle: "编排与发布 DAG" },
  "/tasks": { title: "任务", subtitle: "执行记录与实时日志" },
  "/knowledge": { title: "知识库", subtitle: "文档索引与 RAG 检索" },
  "/models": { title: "模型与供应商", subtitle: "LLM Provider 配置" },
  "/cost": { title: "成本分析", subtitle: "Token 消耗与费用明细" },
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

        <div className="ml-auto flex items-center gap-2">
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

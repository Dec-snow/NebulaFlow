import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Activity, Cpu, GitBranch, Layers, Zap } from "lucide-react";
import { api } from "../api/client";
import type { DashboardStats, Workflow } from "../types";

const formatTime = (iso: string) => new Date(iso).toLocaleString("zh-CN");

export default function Dashboard() {
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [tasks, setTasks] = useState<{ id: number; workflow_id: number; status: string; created_at: string }[]>([]);

  useEffect(() => {
    const load = () => {
      api.dashboardStats().then(setStats).catch(() => {});
      api.listWorkflows().then((r) => setWorkflows(r.workflows)).catch(() => {});
      api.listTasks(8).then((r) => setTasks(r.tasks)).catch(() => {});
    };
    load();
    const timer = setInterval(load, 5000); // 实时刷新
    return () => clearInterval(timer);
  }, []);

  const cards = [
    { label: "Running Tasks", value: stats?.running_tasks ?? "-", icon: Activity, hint: "执行中的任务" },
    { label: "Active Workers", value: stats?.active_workers ?? "-", icon: Cpu, hint: "Worker Pool" },
    { label: "Queue Length", value: stats?.queue_length ?? "-", icon: Layers, hint: "Redis Stream 待消费" },
    { label: "Workflows", value: workflows.length, icon: GitBranch, hint: "已保存的工作流" },
  ];

  const statusColor: Record<string, string> = {
    pending: "text-yellow-400 bg-yellow-400/10 border-yellow-400/30",
    running: "text-nebula-300 bg-nebula-400/10 border-nebula-400/30",
    succeeded: "text-emerald-400 bg-emerald-400/10 border-emerald-400/30",
    failed: "text-red-400 bg-red-400/10 border-red-400/30",
    cancelled: "text-slate-400 bg-slate-400/10 border-slate-400/30",
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold text-slate-100">Dashboard</h2>
          <p className="text-xs text-slate-500 mt-1">系统实时状态 · 每 5s 自动刷新</p>
        </div>
        <div className="flex items-center gap-2 text-xs text-emerald-400">
          <div className="w-2 h-2 rounded-full bg-emerald-400 animate-pulse" />
          Connected
        </div>
      </div>

      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        {cards.map(({ label, value, icon: Icon, hint }) => (
          <div key={label} className="card">
            <div className="flex items-center justify-between">
              <span className="text-xs text-slate-500">{label}</span>
              <Icon size={15} className="text-nebula-400" />
            </div>
            <div className="text-3xl font-bold text-slate-100 mt-2 tabular-nums">{value}</div>
            <div className="text-[10px] text-slate-600 mt-1">{hint}</div>
          </div>
        ))}
      </div>

      <div className="grid lg:grid-cols-2 gap-6">
        <div className="card">
          <div className="flex items-center justify-between mb-4">
            <h3 className="text-sm font-medium text-slate-200">最近工作流</h3>
            <Link to="/workflows" className="text-xs text-nebula-400 hover:text-nebula-300">
              查看全部 →
            </Link>
          </div>
          <div className="space-y-2">
            {workflows.length === 0 && <p className="text-xs text-slate-600">暂无工作流，去创建第一个吧</p>}
            {workflows.slice(0, 5).map((w) => (
              <Link
                key={w.id}
                to={`/workflows/${w.id}`}
                className="flex items-center justify-between px-3 py-2 rounded-lg bg-nebula-950 border border-nebula-800 hover:border-nebula-600 transition-colors"
              >
                <div className="min-w-0">
                  <div className="text-sm text-slate-200 truncate">{w.name}</div>
                  <div className="text-[10px] text-slate-500">{w.nodes?.length ?? 0} 个节点 · {formatTime(w.updated_at)}</div>
                </div>
                <span className={`text-[10px] px-2 py-0.5 rounded-full border ${statusColor[w.status] || statusColor.pending}`}>
                  {w.status}
                </span>
              </Link>
            ))}
          </div>
        </div>

        <div className="card">
          <div className="flex items-center justify-between mb-4">
            <h3 className="text-sm font-medium text-slate-200">最近任务</h3>
            <Link to="/tasks" className="text-xs text-nebula-400 hover:text-nebula-300">
              查看全部 →
            </Link>
          </div>
          <div className="space-y-2">
            {tasks.length === 0 && <p className="text-xs text-slate-600">暂无任务</p>}
            {tasks.map((t) => (
              <Link
                key={t.id}
                to={`/tasks/${t.id}`}
                className="flex items-center justify-between px-3 py-2 rounded-lg bg-nebula-950 border border-nebula-800 hover:border-nebula-600 transition-colors"
              >
                <div className="flex items-center gap-2">
                  <Zap size={13} className="text-nebula-400" />
                  <span className="text-sm text-slate-200">Task #{t.id}</span>
                  <span className="text-[10px] text-slate-600">wf#{t.workflow_id}</span>
                </div>
                <div className="flex items-center gap-3">
                  <span className="text-[10px] text-slate-500">{formatTime(t.created_at)}</span>
                  <span className={`text-[10px] px-2 py-0.5 rounded-full border ${statusColor[t.status]}`}>{t.status}</span>
                </div>
              </Link>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}

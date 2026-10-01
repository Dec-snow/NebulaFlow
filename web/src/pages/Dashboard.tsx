import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Activity, Cpu, GitBranch, Layers, Zap, TrendingUp, CheckCircle, AlertCircle, Clock, Database, Server } from "lucide-react";
import { api } from "../api/client";
import type { DashboardStats, Workflow, Task } from "../types";

const formatTime = (iso: string) => new Date(iso).toLocaleString("zh-CN");
const formatDuration = (ms: number) => {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
};

// 迷你折线图 - 纯 SVG 实现（自适应宽度）
function Sparkline({ data, color = "#818cf8", height = 32 }: { data: number[]; color?: string; height?: number }) {
  if (data.length < 2) return <div style={{ height }} />;
  const w = 200;
  const max = Math.max(...data, 1);
  const min = Math.min(...data, 0);
  const range = max - min || 1;
  const step = w / (data.length - 1);
  const points = data.map((v, i) => {
    const x = i * step;
    const y = height - ((v - min) / range) * (height - 4) - 2;
    return `${x},${y}`;
  }).join(" ");
  const areaPoints = `0,${height} ${points} ${w},${height}`;
  return (
    <svg viewBox={`0 0 ${w} ${height}`} preserveAspectRatio="none" className="w-full overflow-visible">
      <defs>
        <linearGradient id={`grad-${color.replace("#", "")}`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity="0.3" />
          <stop offset="100%" stopColor={color} stopOpacity="0" />
        </linearGradient>
      </defs>
      <polygon points={areaPoints} fill={`url(#grad-${color.replace("#", "")})`} />
      <polyline points={points} fill="none" stroke={color} strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
      <circle cx={(data.length - 1) * step} cy={height - ((data[data.length - 1] - min) / range) * (height - 4) - 2} r="3" fill={color} />
    </svg>
  );
}

// 环形进度图
function DonutChart({ value, max, color = "#10b981", size = 80, strokeWidth = 6, label }: { value: number; max: number; color?: string; size?: number; strokeWidth?: number; label?: string }) {
  const radius = (size - strokeWidth) / 2;
  const circumference = 2 * Math.PI * radius;
  const percent = Math.min(value / max, 1);
  const offset = circumference * (1 - percent);
  return (
    <div className="relative inline-flex items-center justify-center" style={{ width: size, height: size }}>
      <svg width={size} height={size} className="-rotate-90">
        <circle cx={size / 2} cy={size / 2} r={radius} fill="none" stroke="rgba(255,255,255,0.08)" strokeWidth={strokeWidth} />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke={color}
          strokeWidth={strokeWidth}
          strokeLinecap="round"
          strokeDasharray={circumference}
          strokeDashoffset={offset}
          style={{ transition: "stroke-dashoffset 0.6s ease-out" }}
        />
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        <span className="text-sm font-bold text-slate-100 tabular-nums">{value}</span>
        {label && <span className="text-[9px] text-slate-500 mt-0.5">{label}</span>}
      </div>
    </div>
  );
}

// 状态条
function StatusBar({ items }: { items: { label: string; value: number; color: string }[] }) {
  const total = items.reduce((s, i) => s + i.value, 0) || 1;
  return (
    <div className="w-full">
      <div className="h-2 rounded-full overflow-hidden flex bg-nebula-950">
        {items.map((item, idx) => (
          <div
            key={item.label}
            className="h-full transition-all duration-700 ease-out"
            style={{
              width: `${(item.value / total) * 100}%`,
              backgroundColor: item.color,
              marginLeft: idx > 0 ? "1px" : 0,
            }}
          />
        ))}
      </div>
      <div className="flex flex-wrap gap-x-4 gap-y-1 mt-2">
        {items.map((item) => (
          <div key={item.label} className="flex items-center gap-1.5">
            <div className="w-2 h-2 rounded-full" style={{ backgroundColor: item.color }} />
            <span className="text-[10px] text-slate-500">{item.label}</span>
            <span className="text-[10px] text-slate-400 font-medium tabular-nums">{item.value}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

export default function Dashboard() {
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [history, setHistory] = useState<{ running: number[]; queue: number[]; workers: number[] }>({
    running: [],
    queue: [],
    workers: [],
  });
  const historyRef = useRef({ running: [] as number[], queue: [] as number[], workers: [] as number[] });
  const MAX_HISTORY = 20;

  useEffect(() => {
    const load = async () => {
      try {
        const [s, w, t] = await Promise.all([
          api.dashboardStats(),
          api.listWorkflows().catch(() => ({ workflows: [] as Workflow[] })),
          api.listTasks(8).catch(() => ({ tasks: [] as Task[] })),
        ]);
        setStats(s);
        setWorkflows(w.workflows || []);
        setTasks(t.tasks || []);

        // 更新历史数据
        const h = historyRef.current;
        h.running.push(s.running_tasks);
        h.queue.push(s.queue_length);
        h.workers.push(s.active_workers);
        if (h.running.length > MAX_HISTORY) {
          h.running.shift();
          h.queue.shift();
          h.workers.shift();
        }
        setHistory({ ...h });
      } catch {
        /* ignore */
      }
    };
    load();
    const timer = setInterval(load, 5000);
    return () => clearInterval(timer);
  }, []);

  const taskStats = useMemo(() => {
    const counts = { succeeded: 0, running: 0, failed: 0, pending: 0, cancelled: 0 };
    tasks.forEach((t) => {
      if (t.status in counts) counts[t.status as keyof typeof counts]++;
    });
    return counts;
  }, [tasks]);

  const totalWorkers = stats?.total_workers ?? 20;
  const workerUtilization = stats ? Math.round((stats.active_workers / (stats.total_workers || 20)) * 100) : 0;

  const cards = [
    { label: "Running Tasks", value: stats?.running_tasks ?? 0, icon: Activity, hint: "执行中的任务", color: "#818cf8", data: history.running },
    { label: "Active Workers", value: stats?.active_workers ?? 0, icon: Cpu, hint: `Worker Pool · ${workerUtilization}%`, color: "#34d399", data: history.workers },
    { label: "Queue Length", value: stats?.queue_length ?? 0, icon: Layers, hint: "待消费任务", color: "#fbbf24", data: history.queue },
    { label: "Workflows", value: workflows.length, icon: GitBranch, hint: "已保存的工作流", color: "#f472b6", data: [] },
  ];

  const statusColor: Record<string, string> = {
    pending: "text-yellow-400 bg-yellow-400/10 border-yellow-400/30",
    running: "text-nebula-300 bg-nebula-400/10 border-nebula-400/30",
    succeeded: "text-emerald-400 bg-emerald-400/10 border-emerald-400/30",
    failed: "text-red-400 bg-red-400/10 border-red-400/30",
    cancelled: "text-slate-400 bg-slate-400/10 border-slate-400/30",
    skipped: "text-slate-500 bg-slate-500/10 border-slate-500/30",
  };

  const statusBarItems = [
    { label: "成功", value: taskStats.succeeded, color: "#10b981" },
    { label: "运行中", value: taskStats.running, color: "#818cf8" },
    { label: "等待中", value: taskStats.pending, color: "#fbbf24" },
    { label: "失败", value: taskStats.failed, color: "#ef4444" },
    { label: "已取消", value: taskStats.cancelled, color: "#64748b" },
  ];

  return (
    <div className="space-y-6">
      {/* 顶部标题 */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold text-slate-100">Dashboard</h2>
          <p className="text-xs text-slate-500 mt-1">系统实时状态 · 每 5s 自动刷新</p>
        </div>
        <div className="flex items-center gap-2">
          <div className="flex items-center gap-2 text-xs text-emerald-400">
            <div className="relative">
              <div className="w-2 h-2 rounded-full bg-emerald-400 animate-pulse" />
              <div className="absolute inset-0 w-2 h-2 rounded-full bg-emerald-400 animate-ping opacity-40" />
            </div>
            Healthy
          </div>
          <div className="text-[10px] text-slate-600 ml-2">
            {stats ? formatTime(stats.timestamp) : "加载中..."}
          </div>
        </div>
      </div>

      {/* 指标卡片 */}
      <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
        {cards.map(({ label, value, icon: Icon, hint, color, data }) => (
          <div key={label} className="card group hover:border-nebula-700 transition-all duration-300 hover:shadow-lg hover:shadow-nebula-500/5">
            <div className="flex items-start justify-between">
              <div>
                <span className="text-xs text-slate-500">{label}</span>
                <div className="text-3xl font-bold text-slate-100 mt-2 tabular-nums transition-all duration-300 group-hover:scale-105 origin-left">
                  {value}
                </div>
                <div className="text-[10px] text-slate-600 mt-1">{hint}</div>
              </div>
              <div className="p-2 rounded-lg transition-all duration-300 group-hover:scale-110" style={{ backgroundColor: `${color}15` }}>
                <Icon size={18} style={{ color }} />
              </div>
            </div>
            {data && data.length > 1 && (
              <div className="mt-3 -mx-1">
                <Sparkline data={data} color={color} height={32} />
              </div>
            )}
          </div>
        ))}
      </div>

      {/* 中部：系统健康 + 任务概览 */}
      <div className="grid lg:grid-cols-3 gap-6">
        {/* 系统健康 */}
        <div className="card">
          <div className="flex items-center justify-between mb-4">
            <h3 className="text-sm font-medium text-slate-200 flex items-center gap-2">
              <Server size={14} className="text-nebula-400" />
              系统健康
            </h3>
            <span className="text-[10px] text-emerald-400 px-2 py-0.5 rounded-full bg-emerald-400/10 border border-emerald-400/30">
              正常
            </span>
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="flex flex-col items-center">
              <DonutChart
                value={stats?.active_workers ?? 0}
                max={totalWorkers}
                color="#34d399"
                label="活跃 Worker"
              />
            </div>
            <div className="flex flex-col items-center">
              <DonutChart
                value={stats ? Math.min(stats.queue_length, 100) : 0}
                max={100}
                color={stats && stats.queue_length > 50 ? "#fbbf24" : "#818cf8"}
                label="队列水位"
              />
            </div>
          </div>
          <div className="mt-4 space-y-2">
            <div className="flex items-center justify-between text-xs">
              <div className="flex items-center gap-2 text-slate-400">
                <Database size={12} className="text-emerald-400" />
                数据库
              </div>
              <span className="text-emerald-400 flex items-center gap-1">
                <CheckCircle size={12} /> 可用
              </span>
            </div>
            <div className="flex items-center justify-between text-xs">
              <div className="flex items-center gap-2 text-slate-400">
                <Zap size={12} className="text-nebula-400" />
                Worker Pool
              </div>
              <span className="text-emerald-400 flex items-center gap-1">
                <CheckCircle size={12} /> {stats?.total_workers ?? 20} workers
              </span>
            </div>
            <div className="flex items-center justify-between text-xs">
              <div className="flex items-center gap-2 text-slate-400">
                <Clock size={12} className="text-yellow-400" />
                队列
              </div>
              <span className={stats && stats.queue_length > 0 ? "text-yellow-400" : "text-emerald-400"}>
                {stats?.queue_length ?? 0} pending
              </span>
            </div>
          </div>
        </div>

        {/* 任务状态概览 */}
        <div className="card lg:col-span-2">
          <div className="flex items-center justify-between mb-4">
            <h3 className="text-sm font-medium text-slate-200 flex items-center gap-2">
              <TrendingUp size={14} className="text-nebula-400" />
              任务状态概览
            </h3>
            <Link to="/tasks" className="text-xs text-nebula-400 hover:text-nebula-300 transition-colors">
              查看全部 →
            </Link>
          </div>
          <StatusBar items={statusBarItems} />
          <div className="mt-5 grid grid-cols-2 md:grid-cols-4 gap-3">
            <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
              <div className="text-2xl font-bold text-emerald-400 tabular-nums">{taskStats.succeeded}</div>
              <div className="text-[10px] text-slate-500 mt-1">成功</div>
            </div>
            <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
              <div className="text-2xl font-bold text-nebula-400 tabular-nums">{taskStats.running}</div>
              <div className="text-[10px] text-slate-500 mt-1">运行中</div>
            </div>
            <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
              <div className="text-2xl font-bold text-yellow-400 tabular-nums">{taskStats.pending}</div>
              <div className="text-[10px] text-slate-500 mt-1">等待中</div>
            </div>
            <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
              <div className="text-2xl font-bold text-red-400 tabular-nums">{taskStats.failed}</div>
              <div className="text-[10px] text-slate-500 mt-1">失败</div>
            </div>
          </div>
        </div>
      </div>

      {/* 底部：工作流 + 任务列表 */}
      <div className="grid lg:grid-cols-2 gap-6">
        <div className="card">
          <div className="flex items-center justify-between mb-4">
            <h3 className="text-sm font-medium text-slate-200 flex items-center gap-2">
              <GitBranch size={14} className="text-nebula-400" />
              最近工作流
            </h3>
            <Link to="/workflows" className="text-xs text-nebula-400 hover:text-nebula-300 transition-colors">
              查看全部 →
            </Link>
          </div>
          <div className="space-y-2">
            {workflows.length === 0 && (
              <div className="text-center py-8">
                <GitBranch size={32} className="mx-auto text-slate-700 mb-2" />
                <p className="text-xs text-slate-600">暂无工作流</p>
                <Link to="/workflows/new" className="text-xs text-nebula-400 hover:text-nebula-300 mt-2 inline-block">
                  创建第一个工作流 →
                </Link>
              </div>
            )}
            {workflows.slice(0, 5).map((w) => (
              <Link
                key={w.id}
                to={`/workflows/${w.id}`}
                className="flex items-center justify-between px-3 py-2.5 rounded-lg bg-nebula-950 border border-nebula-800 hover:border-nebula-600 transition-all duration-200 group"
              >
                <div className="min-w-0 flex-1">
                  <div className="text-sm text-slate-200 truncate group-hover:text-nebula-300 transition-colors">{w.name}</div>
                  <div className="text-[10px] text-slate-500 mt-0.5">
                    {w.nodes?.length ?? 0} 个节点 · {formatTime(w.updated_at)}
                  </div>
                </div>
                <span className={`text-[10px] px-2 py-0.5 rounded-full border ml-2 ${statusColor[w.status] || statusColor.pending}`}>
                  {w.status}
                </span>
              </Link>
            ))}
          </div>
        </div>

        <div className="card">
          <div className="flex items-center justify-between mb-4">
            <h3 className="text-sm font-medium text-slate-200 flex items-center gap-2">
              <Zap size={14} className="text-nebula-400" />
              最近任务
            </h3>
            <Link to="/tasks" className="text-xs text-nebula-400 hover:text-nebula-300 transition-colors">
              查看全部 →
            </Link>
          </div>
          <div className="space-y-2">
            {tasks.length === 0 && (
              <div className="text-center py-8">
                <Zap size={32} className="mx-auto text-slate-700 mb-2" />
                <p className="text-xs text-slate-600">暂无任务</p>
              </div>
            )}
            {tasks.map((t) => (
              <Link
                key={t.id}
                to={`/tasks/${t.id}`}
                className="flex items-center justify-between px-3 py-2.5 rounded-lg bg-nebula-950 border border-nebula-800 hover:border-nebula-600 transition-all duration-200 group"
              >
                <div className="flex items-center gap-2.5 min-w-0 flex-1">
                  <div className={`w-1.5 h-1.5 rounded-full flex-shrink-0 ${
                    t.status === "running" ? "bg-nebula-400 animate-pulse" :
                    t.status === "succeeded" ? "bg-emerald-400" :
                    t.status === "failed" ? "bg-red-400" :
                    "bg-yellow-400"
                  }`} />
                  <div className="min-w-0 flex-1">
                    <div className="text-sm text-slate-200 truncate group-hover:text-nebula-300 transition-colors">
                      Task #{t.id}
                      <span className="text-[10px] text-slate-600 ml-2">wf#{t.workflow_id}</span>
                    </div>
                    <div className="text-[10px] text-slate-500 mt-0.5">
                      {formatTime(t.created_at)}
                      {t.started_at && t.finished_at
                        ? ` · ${formatDuration(new Date(t.finished_at).getTime() - new Date(t.started_at).getTime())}`
                        : ""}
                    </div>
                  </div>
                </div>
                <span className={`text-[10px] px-2 py-0.5 rounded-full border ml-2 flex-shrink-0 ${statusColor[t.status]}`}>
                  {t.status}
                </span>
              </Link>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}

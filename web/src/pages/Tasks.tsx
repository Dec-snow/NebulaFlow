import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { RefreshCw } from "lucide-react";
import { api } from "../api/client";
import type { Task } from "../types";

const statusColor: Record<string, string> = {
  pending: "text-yellow-400 bg-yellow-400/10 border-yellow-400/30",
  running: "text-nebula-300 bg-nebula-400/10 border-nebula-400/30",
  succeeded: "text-emerald-400 bg-emerald-400/10 border-emerald-400/30",
  failed: "text-red-400 bg-red-400/10 border-red-400/30",
  cancelled: "text-slate-400 bg-slate-400/10 border-slate-400/30",
};

export default function Tasks() {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [loading, setLoading] = useState(true);

  const load = () =>
    api.listTasks(50).then((r) => setTasks(r.tasks)).finally(() => setLoading(false));

  useEffect(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold text-slate-100">Tasks</h2>
          <p className="text-xs text-slate-500 mt-1">任务执行记录 · 点击查看实时执行流</p>
        </div>
        <button className="btn-ghost" onClick={load}>
          <RefreshCw size={14} /> 刷新
        </button>
      </div>

      <div className="card overflow-hidden p-0">
        <table className="w-full text-sm">
          <thead>
            <tr className="text-left text-[11px] text-slate-500 border-b border-nebula-800">
              <th className="px-4 py-3 font-medium">ID</th>
              <th className="px-4 py-3 font-medium">工作流</th>
              <th className="px-4 py-3 font-medium">状态</th>
              <th className="px-4 py-3 font-medium">节点</th>
              <th className="px-4 py-3 font-medium">耗时</th>
              <th className="px-4 py-3 font-medium">创建时间</th>
            </tr>
          </thead>
          <tbody>
            {loading && (
              <tr>
                <td colSpan={6} className="px-4 py-8 text-center text-slate-500">
                  加载中…
                </td>
              </tr>
            )}
            {!loading && tasks.length === 0 && (
              <tr>
                <td colSpan={6} className="px-4 py-8 text-center text-slate-500">
                  暂无任务
                </td>
              </tr>
            )}
            {tasks.map((t) => {
              const done = t.finished_at ? new Date(t.finished_at).getTime() : Date.now();
              const start = t.started_at ? new Date(t.started_at).getTime() : done;
              const dur = t.finished_at || t.started_at ? Math.max(0, done - start) : null;
              return (
                <tr key={t.id} className="border-b border-nebula-800/50 hover:bg-nebula-800/30">
                  <td className="px-4 py-3">
                    <Link to={`/tasks/${t.id}`} className="text-nebula-400 hover:text-nebula-300">
                      #{t.id}
                    </Link>
                  </td>
                  <td className="px-4 py-3 text-slate-400">wf#{t.workflow_id}</td>
                  <td className="px-4 py-3">
                    <span className={`text-[10px] px-2 py-0.5 rounded-full border ${statusColor[t.status]}`}>
                      {t.status}
                    </span>
                  </td>
                  <td className="px-4 py-3 text-slate-400 tabular-nums">{t.nodes?.length ?? "-"}</td>
                  <td className="px-4 py-3 text-slate-400 tabular-nums">
                    {dur !== null ? `${(dur / 1000).toFixed(1)}s` : "-"}
                  </td>
                  <td className="px-4 py-3 text-slate-500 text-xs">
                    {new Date(t.created_at).toLocaleString("zh-CN")}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

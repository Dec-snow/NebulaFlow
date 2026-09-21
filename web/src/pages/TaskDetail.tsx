import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Ban, CheckCircle2, Circle, Loader2, XCircle } from "lucide-react";
import { api } from "../api/client";
import type { SSEvent, Task, TaskNode } from "../types";

const nodeStatusIcon: Record<string, React.ElementType> = {
  pending: Circle,
  running: Loader2,
  succeeded: CheckCircle2,
  failed: XCircle,
  skipped: Circle,
  cancelled: Ban,
};

const nodeStatusColor: Record<string, string> = {
  pending: "text-slate-500",
  running: "text-nebula-300 animate-spin",
  succeeded: "text-emerald-400",
  failed: "text-red-400",
  skipped: "text-slate-600",
  cancelled: "text-amber-400",
};

export default function TaskDetail() {
  const { id } = useParams();
  const taskId = Number(id);
  const [task, setTask] = useState<Task | null>(null);
  const [nodes, setNodes] = useState<Record<string, TaskNode>>({});
  const [logs, setLogs] = useState<SSEvent[]>([]);
  const [streamText, setStreamText] = useState<Record<string, string>>({});
  const [connected, setConnected] = useState(false);
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    api.getTask(taskId).then((t) => {
      setTask(t);
      const m: Record<string, TaskNode> = {};
      for (const n of t.nodes || []) m[n.node_key] = n;
      setNodes(m);
    }).catch(() => {});

    // SSE 实时流
    const es = new EventSource(`/api/tasks/${taskId}/stream`);
    esRef.current = es;
    const listeners: Record<string, (e: MessageEvent) => void> = {};

    const attach = (type: string, fn: (ev: SSEvent) => void) => {
      listeners[type] = (e: MessageEvent) => {
        try {
          fn(JSON.parse(e.data) as SSEvent);
        } catch {
          /* ignore */
        }
      };
      es.addEventListener(type, listeners[type]);
    };

    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false);

    attach("snapshot", (ev) => {
      const t = ev as unknown as Task;
      setTask(t);
      const m: Record<string, TaskNode> = {};
      for (const n of t.nodes || []) m[n.node_key] = n;
      setNodes(m);
    });
    attach("task_running", (ev) => setTask((prev) => (prev ? { ...prev, status: "running" } : prev)));
    attach("task_completed", (ev) => setTask((prev) => (prev ? { ...prev, status: "succeeded", output: ev.content || prev.output } : prev)));
    attach("task_failed", (ev) => setTask((prev) => (prev ? { ...prev, status: "failed", error: ev.message || prev.error } : prev)));
    attach("task_cancelled", () => setTask((prev) => (prev ? { ...prev, status: "cancelled" } : prev)));
    attach("node_started", (ev) =>
      setNodes((prev) => ({ ...prev, [ev.node_key!]: { ...(prev[ev.node_key!] || {}), node_key: ev.node_key!, node_type: ev.node_type as any, status: "running" } }))
    );
    attach("node_completed", (ev) =>
      setNodes((prev) => ({
        ...prev,
        [ev.node_key!]: {
          ...(prev[ev.node_key!] || {}),
          node_key: ev.node_key!,
          status: "succeeded",
          output: ev.content,
          duration_ms: ev.duration_ms || 0,
        },
      }))
    );
    attach("node_failed", (ev) =>
      setNodes((prev) => ({ ...prev, [ev.node_key!]: { ...(prev[ev.node_key!] || {}), node_key: ev.node_key!, status: "failed", error: ev.message } }))
    );
    attach("token", (ev) => {
      if (!ev.node_key) return;
      setStreamText((prev) => ({ ...prev, [ev.node_key!]: (prev[ev.node_key!] || "") + (ev.content || "") }));
      setLogs((prev) => [...prev.slice(-199), ev]);
    });
    attach("fallback", (ev) => setLogs((prev) => [...prev.slice(-199), ev]));
    attach("log", (ev) => setLogs((prev) => [...prev.slice(-199), ev]));

    return () => {
      es.close();
      Object.entries(listeners).forEach(([t, fn]) => es.removeEventListener(t, fn));
    };
  }, [taskId]);

  const cancel = async () => {
    await api.cancelTask(taskId);
    setTask((prev) => (prev ? { ...prev, status: "cancelled" } : prev));
  };

  const nodeKeys = Object.keys(nodes);
  const isDone = task && ["succeeded", "failed", "cancelled"].includes(task.status);

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-3">
            <h2 className="text-xl font-semibold text-slate-100">Task #{taskId}</h2>
            <span
              className={`text-[11px] px-2.5 py-0.5 rounded-full border ${
                task?.status === "succeeded"
                  ? "text-emerald-400 border-emerald-400/30 bg-emerald-400/10"
                  : task?.status === "failed"
                  ? "text-red-400 border-red-400/30 bg-red-400/10"
                  : task?.status === "running"
                  ? "text-nebula-300 border-nebula-400/30 bg-nebula-400/10"
                  : "text-slate-400 border-slate-400/30 bg-slate-400/10"
              }`}
            >
              {task?.status || "loading"}
            </span>
          </div>
          <p className="text-xs text-slate-500 mt-1">
            <Link to={`/workflows/${task?.workflow_id}`} className="text-nebula-400 hover:text-nebula-300">
              工作流 #{task?.workflow_id}
            </Link>
            {" · "}创建于 {task ? new Date(task.created_at).toLocaleString("zh-CN") : "-"}
          </p>
        </div>
        <div className="flex items-center gap-3">
          <span className={`text-[10px] flex items-center gap-1.5 ${connected ? "text-emerald-400" : "text-slate-500"}`}>
            <span className={`w-1.5 h-1.5 rounded-full ${connected ? "bg-emerald-400 animate-pulse" : "bg-slate-600"}`} />
            {connected ? "SSE Connected" : "SSE Disconnected"}
          </span>
          {task?.status === "running" && (
            <button className="btn-danger" onClick={cancel}>
              <Ban size={13} /> 取消任务
            </button>
          )}
        </div>
      </div>

      <div className="grid lg:grid-cols-2 gap-6">
        {/* 左侧：节点执行状态 */}
        <div className="space-y-4">
          <div className="card">
            <h3 className="text-sm font-medium text-slate-200 mb-4">节点执行状态</h3>
            <div className="space-y-2">
              {nodeKeys.length === 0 && <p className="text-xs text-slate-600">等待调度器初始化节点…</p>}
              {nodeKeys.map((k) => {
                const n = nodes[k];
                const Icon = nodeStatusIcon[n?.status || "pending"] || Circle;
                const color = nodeStatusColor[n?.status || "pending"];
                return (
                  <div key={k} className="flex items-center gap-3 px-3 py-2 rounded-lg bg-nebula-950 border border-nebula-800">
                    <Icon size={15} className={color} />
                    <div className="flex-1 min-w-0">
                      <div className="text-xs text-slate-200">
                        {k} <span className="text-slate-600">· {n?.node_type || "?"}</span>
                      </div>
                      {n?.error && <div className="text-[10px] text-red-400 truncate">{n.error}</div>}
                      {n?.duration_ms ? (
                        <div className="text-[10px] text-slate-500">{(n.duration_ms / 1000).toFixed(2)}s</div>
                      ) : null}
                    </div>
                    {n?.retries ? <span className="text-[9px] text-yellow-400">重试 {n.retries}</span> : null}
                  </div>
                );
              })}
            </div>
          </div>

          {/* LLM 流式输出 */}
          {Object.keys(streamText).length > 0 && (
            <div className="card">
              <h3 className="text-sm font-medium text-slate-200 mb-3">LLM 实时输出</h3>
              <div className="space-y-3">
                {Object.entries(streamText).map(([k, v]) => (
                  <div key={k}>
                    <div className="text-[10px] text-nebula-400 mb-1">{k}</div>
                    <div className="text-xs text-slate-300 whitespace-pre-wrap bg-nebula-950 rounded-lg p-3 border border-nebula-800 max-h-40 overflow-y-auto">
                      {v}
                      {!isDone && <span className="inline-block w-1.5 h-3 bg-nebula-400 animate-pulse ml-0.5 align-middle" />}
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* 最终输出 */}
          {task?.output && (
            <div className="card">
              <h3 className="text-sm font-medium text-slate-200 mb-3">最终输出</h3>
              <div className="text-xs text-slate-300 whitespace-pre-wrap bg-nebula-950 rounded-lg p-3 border border-nebula-800 max-h-60 overflow-y-auto">
                {task.output}
              </div>
            </div>
          )}
          {task?.error && (
            <div className="card border-red-500/30">
              <h3 className="text-sm font-medium text-red-300 mb-2">错误</h3>
              <div className="text-xs text-red-400 whitespace-pre-wrap">{task.error}</div>
            </div>
          )}
        </div>

        {/* 右侧：实时事件流 */}
        <div className="card h-fit">
          <h3 className="text-sm font-medium text-slate-200 mb-3">实时事件流</h3>
          <div className="bg-nebula-950 rounded-lg border border-nebula-800 p-3 h-96 overflow-y-auto space-y-1.5">
            {logs.length === 0 && (
              <p className="text-xs text-slate-600">
                等待事件…（节点启动、LLM token、fallback、日志会实时显示在这里）
              </p>
            )}
            {logs.map((l, i) => (
              <div key={i} className="text-[11px] leading-relaxed">
                <span className="text-slate-600">{new Date(l.timestamp).toLocaleTimeString("zh-CN")}</span>{" "}
                <span
                  className={
                    l.type === "token"
                      ? "text-slate-300"
                      : l.type === "fallback"
                      ? "text-yellow-400"
                      : l.type === "log" && l.message?.includes("error")
                      ? "text-red-400"
                      : "text-nebula-300"
                  }
                >
                  [{l.type}]
                </span>{" "}
                {l.node_key && <span className="text-slate-500">{l.node_key}</span>}{" "}
                <span className="text-slate-300">{l.type === "token" ? l.content : l.message || l.content || ""}</span>
              </div>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}

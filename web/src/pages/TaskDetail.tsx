import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Ban, Clock, GitBranch, Activity } from "lucide-react";
import { api } from "../api/client";
import type { SSEvent, Task, TaskNode, Workflow } from "../types";
import TaskTimeline from "../components/TaskTimeline";

export default function TaskDetail() {
  const { id } = useParams();
  const taskId = Number(id);
  const [task, setTask] = useState<Task | null>(null);
  const [workflow, setWorkflow] = useState<Workflow | null>(null);
  const [nodes, setNodes] = useState<Record<string, TaskNode>>({});
  const [logs, setLogs] = useState<SSEvent[]>([]);
  const [streamText, setStreamText] = useState<Record<string, string>>({});
  const [connected, setConnected] = useState(false);
  const esRef = useRef<EventSource | null>(null);

  useEffect(() => {
    // 并行获取任务和工作流
    Promise.all([
      api.getTask(taskId).catch(() => null),
      api.getWorkflow(0).catch(() => null), // 占位，实际用 task.workflow_id
    ]).then(([t]) => {
      if (t) {
        setTask(t);
        const m: Record<string, TaskNode> = {};
        for (const n of t.nodes || []) m[n.node_key] = n;
        setNodes(m);
        // 获取工作流信息
        api.getWorkflow(t.workflow_id).then((wf) => setWorkflow(wf)).catch(() => {});
      }
    });

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
      setNodes((prev) => ({ ...prev, [ev.node_key!]: { ...(prev[ev.node_key!] || {}), node_key: ev.node_key!, node_type: ev.node_type as any, status: "running", duration_ms: 0, tokens_in: 0, tokens_out: 0, retries: 0 } }))
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

  const nodeList = useMemo(() => Object.values(nodes), [nodes]);
  const isDone = task && ["succeeded", "failed", "cancelled"].includes(task.status);

  // 计算总耗时
  const totalDuration = useMemo(() => {
    if (task?.started_at && task.finished_at) {
      return new Date(task.finished_at).getTime() - new Date(task.started_at).getTime();
    }
    const maxDur = Math.max(...nodeList.map((n) => n.duration_ms || 0), 0);
    return maxDur;
  }, [task, nodeList]);

  // 工作流节点和边（用于 Timeline）
  const wfNodes = useMemo(() => {
    if (workflow?.nodes && workflow.nodes.length > 0) {
      return workflow.nodes.map((n) => ({ node_key: n.node_key, node_type: n.node_type }));
    }
    // fallback：从 task nodes 构造
    return nodeList.map((n) => ({ node_key: n.node_key, node_type: n.node_type }));
  }, [workflow, nodeList]);

  const wfEdges = useMemo(() => {
    return workflow?.edges || [];
  }, [workflow]);

  const statusBadgeClass =
    task?.status === "succeeded"
      ? "text-emerald-400 border-emerald-400/30 bg-emerald-400/10"
      : task?.status === "failed"
      ? "text-red-400 border-red-400/30 bg-red-400/10"
      : task?.status === "running"
      ? "text-nebula-300 border-nebula-400/30 bg-nebula-400/10"
      : "text-slate-400 border-slate-400/30 bg-slate-400/10";

  return (
    <div className="space-y-6">
      {/* 顶部标题栏 */}
      <div className="flex items-center justify-between">
        <div>
          <div className="flex items-center gap-3">
            <h2 className="text-xl font-semibold text-slate-100">Task #{taskId}</h2>
            <span className={`text-[11px] px-2.5 py-0.5 rounded-full border ${statusBadgeClass}`}>
              {task?.status || "loading"}
            </span>
          </div>
          <p className="text-xs text-slate-500 mt-1">
            <Link to={`/workflows/${task?.workflow_id}`} className="text-nebula-400 hover:text-nebula-300">
              <GitBranch size={11} className="inline mr-1" />
              {workflow?.name || `工作流 #${task?.workflow_id}`}
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

      {/* 统计卡片 */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
        <div className="card py-3">
          <div className="flex items-center gap-2 text-[10px] text-slate-500 mb-1">
            <Clock size={11} className="text-nebula-400" />
            总耗时
          </div>
          <div className="text-lg font-bold text-slate-100 tabular-nums">
            {totalDuration > 0 ? `${(totalDuration / 1000).toFixed(2)}s` : "—"}
          </div>
        </div>
        <div className="card py-3">
          <div className="flex items-center gap-2 text-[10px] text-slate-500 mb-1">
            <Activity size={11} className="text-emerald-400" />
            节点数
          </div>
          <div className="text-lg font-bold text-slate-100 tabular-nums">
            {nodeList.length}
          </div>
        </div>
        <div className="card py-3">
          <div className="flex items-center gap-2 text-[10px] text-slate-500 mb-1">
            <Ban size={11} className="text-emerald-400" />
            成功节点
          </div>
          <div className="text-lg font-bold text-emerald-400 tabular-nums">
            {nodeList.filter((n) => n.status === "succeeded").length}
          </div>
        </div>
        <div className="card py-3">
          <div className="flex items-center gap-2 text-[10px] text-slate-500 mb-1">
            <Ban size={11} className="text-red-400" />
            失败节点
          </div>
          <div className="text-lg font-bold text-red-400 tabular-nums">
            {nodeList.filter((n) => n.status === "failed").length}
          </div>
        </div>
      </div>

      {/* Timeline + 详情 */}
      <div className="grid lg:grid-cols-3 gap-6">
        {/* 左侧：Timeline（占 2 列） */}
        <div className="lg:col-span-2 space-y-4">
          <div className="card">
            <h3 className="text-sm font-medium text-slate-200 mb-4 flex items-center gap-2">
              <Activity size={14} className="text-nebula-400" />
              执行时间线
            </h3>
            {wfNodes.length === 0 ? (
              <p className="text-xs text-slate-600 text-center py-8">等待调度器初始化节点…</p>
            ) : (
              <TaskTimeline
                nodes={nodeList}
                workflowNodes={wfNodes}
                workflowEdges={wfEdges}
                totalDuration={totalDuration}
                taskStartedAt={task?.started_at}
                taskFinishedAt={task?.finished_at}
              />
            )}
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
              <div className="text-xs text-slate-300 whitespace-pre-wrap bg-nebula-950 rounded-lg p-3 border border-emerald-500/20 max-h-60 overflow-y-auto">
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
          <div className="bg-nebula-950 rounded-lg border border-nebula-800 p-3 h-[500px] overflow-y-auto space-y-1.5">
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

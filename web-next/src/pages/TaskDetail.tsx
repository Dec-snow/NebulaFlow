import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  Ban,
  ChevronLeft,
  Clock,
  Copy,
  Database,
  LogIn,
  LogOut,
  Radio,
  RotateCcw,
  Sparkles,
  Wrench,
  type LucideIcon,
} from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorState,
  MiniStat,
  PageLoader,
  Segmented,
  StatusBadge,
  StatusDot,
  ToastContainer,
  useToast,
} from "@/components/ui";
import {
  api,
  subscribeTaskStream,
  type TaskStreamEvent,
  type TaskStreamState,
} from "@/api";
import { useFetch } from "@/hooks";
import { clockOf, cn, fmtDuration, numberOrNull, timeAgo } from "@/lib/utils";

const TYPE_ICON: Record<string, LucideIcon> = {
  input: LogIn,
  llm: Sparkles,
  rag: Database,
  tool: Wrench,
  output: LogOut,
};

const TYPE_LABEL: Record<string, string> = {
  input: "输入",
  llm: "LLM",
  rag: "RAG",
  tool: "工具",
  output: "输出",
  condition: "条件",
};

/** 事件流一行。由后端 SSE 事件映射而来（不再是本地 mock）。 */
interface StreamRow {
  id: number;
  at: string;
  type: string;
  text: string;
}

let streamSeq = 0;

/**
 * SSE 事件 → 事件流文案。
 *
 * 事件名与 `internal/task/hub.go` 的常量一一对应：snapshot / task_running /
 * task_completed / task_failed / task_cancelled / task_waiting / node_started /
 * node_completed / node_failed / node_waiting / node_skipped / node_cancelled /
 * token / log / fallback / tool_call / tool_result。
 */
function toStreamRow(ev: TaskStreamEvent): StreamRow {
  const d = ev.data ?? {};
  const str = (v: unknown) => (typeof v === "string" ? v : "");
  const node = str(d.nodeKey);
  const message = str(d.message);
  const content = str(d.content);
  const provider = str(d.provider);
  const tool = str(d.toolName);
  const duration = numberOrNull(d.durationMs);
  const nodeKeys = Array.isArray(d.nodes)
    ? d.nodes
        .map((n) => (n && typeof n === "object" ? str((n as Record<string, unknown>).nodeKey) : ""))
        .filter(Boolean)
    : [];

  let text = message || content;
  switch (ev.event) {
    case "snapshot":
      text = `任务快照 · 状态 ${str(d.status) || "—"} · 节点 [${nodeKeys.join(", ") || "无"}]`;
      break;
    case "task_created":
      text = "任务已创建";
      break;
    case "task_running":
      text = "调度器开始执行";
      break;
    case "task_completed":
      text = "任务执行成功";
      break;
    case "task_failed":
      text = `任务执行失败${message ? ` · ${message}` : ""}`;
      break;
    case "task_cancelled":
      text = "任务已取消";
      break;
    case "task_waiting":
      text = "任务进入等待（人工审批）";
      break;
    case "node_started":
      text = `${node} 开始执行`;
      break;
    case "node_completed":
      text = `${node} 执行完成${duration !== null ? ` · ${fmtDuration(duration)}` : ""}`;
      break;
    case "node_failed":
      text = `${node} 执行失败${message ? ` · ${message}` : ""}`;
      break;
    case "node_skipped":
      text = `${node} 已跳过`;
      break;
    case "node_cancelled":
      text = `${node} 已取消`;
      break;
    case "node_waiting":
      text = `${node} 等待审批`;
      break;
    case "token":
      text = `${node ? `${node} ` : ""}${content}`;
      break;
    case "log":
      text = `${node ? `${node} ` : ""}${message || content}`;
      break;
    case "fallback":
      text = `Provider 故障转移${provider ? ` → ${provider}` : ""}`;
      break;
    case "tool_call":
      text = `${node} 调用工具 ${tool}`;
      break;
    case "tool_result":
      text = `${node} 工具 ${tool} 返回`;
      break;
    default:
      break;
  }

  const at = str(d.timestamp) || ev.receivedAt;
  streamSeq += 1;
  return { id: streamSeq, at, type: ev.event, text: text || ev.event };
}

const STREAM_BADGE: Record<TaskStreamState, { label: string; tone: "mint" | "sky" | "amber" | "rose" | "neutral" }> = {
  connecting: { label: "连接中", tone: "sky" },
  live: { label: "已连接", tone: "mint" },
  reconnecting: { label: "重连中", tone: "amber" },
  closed: { label: "已结束", tone: "neutral" },
  failed: { label: "连接失败", tone: "rose" },
};

export default function TaskDetail() {
  const { id } = useParams();
  const navigate = useNavigate();
  const taskId = Number(id);
  const [tab, setTab] = useState<"events" | "output">("events");
  const [events, setEvents] = useState<StreamRow[]>([]);
  const [streamState, setStreamState] = useState<TaskStreamState>("connecting");
  const [streamDetail, setStreamDetail] = useState("");
  const [busy, setBusy] = useState(false);
  const { toasts, toast } = useToast();

  const { data, loading, error, refresh } = useFetch(
    () => api.tasks.get(taskId),
    [taskId],
  );

  const refreshRef = useRef(refresh);
  refreshRef.current = refresh;
  /** 终态只刷新一次详情：snapshot 与 task_completed 都会命中终态判定 */
  const terminalRefreshedRef = useRef(false);

  /* ---------- SSE 实时事件流 ---------- */
  useEffect(() => {
    if (!Number.isFinite(taskId) || taskId <= 0) return;
    setEvents([]);
    setStreamState("connecting");
    setStreamDetail("");
    terminalRefreshedRef.current = false;

    const handle = subscribeTaskStream(taskId, {
      onEvent: (ev) => {
        setEvents((prev) => [...prev.slice(-499), toStreamRow(ev)]);
        // 任务终态 / 快照显示终态时，刷新一次详情拿最终节点状态与 output
        const status = typeof ev.data?.status === "string" ? ev.data.status : undefined;
        const taskStatus = typeof ev.data?.taskStatus === "string" ? ev.data.taskStatus : undefined;
        const terminal = new Set(["succeeded", "failed", "cancelled"]);
        if (
          ["task_completed", "task_failed", "task_cancelled"].includes(ev.event) ||
          (status !== undefined && terminal.has(status)) ||
          (taskStatus !== undefined && terminal.has(taskStatus))
        ) {
          if (!terminalRefreshedRef.current) {
            terminalRefreshedRef.current = true;
            refreshRef.current();
          }
        }
      },
      onStateChange: (state, detail) => {
        setStreamState(state);
        setStreamDetail(detail ?? "");
      },
    });
    return () => handle.close();
  }, [taskId]);

  if (loading || !data) return <PageLoader text="加载任务详情…" />;

  if (error) return <ErrorState title="加载任务详情失败" description={error.message} onRetry={refresh} />;

  const { task, nodes: rawNodes } = data;

  // 节点明细来自接口（后端的 task.nodes 是 `nodes,omitempty`），缺失时按空表渲染；
  // duration_ms / tokens_in / tokens_out 同样是 omitempty，缺失会污染 reduce/Math.max 的结果
  const nodes = Array.isArray(rawNodes) ? rawNodes : [];

  const tokensIn = nodes.reduce((s, n) => s + (numberOrNull(n.tokensIn) ?? 0), 0);
  const tokensOut = nodes.reduce((s, n) => s + (numberOrNull(n.tokensOut) ?? 0), 0);
  const retries = nodes.reduce((s, n) => s + (numberOrNull(n.retries) ?? 0), 0);
  const maxDur = Math.max(...nodes.map((n) => numberOrNull(n.durationMs) ?? 0), 1);
  const hasTokens = nodes.some((n) => (numberOrNull(n.tokensIn) ?? 0) > 0 || (numberOrNull(n.tokensOut) ?? 0) > 0);

  const statusTone: Record<string, string> = {
    succeeded: "bg-mint",
    running: "bg-sky",
    failed: "bg-rose",
    pending: "bg-line-strong",
    skipped: "bg-line-strong",
    cancelled: "bg-line-strong",
  };

  const handleCancel = async () => {
    setBusy(true);
    try {
      await api.tasks.cancel(taskId);
      toast("已提交取消请求", "success");
      refresh();
    } catch (e) {
      toast(`取消失败：${(e as Error).message}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const handleRerun = async () => {
    setBusy(true);
    try {
      const created = await api.tasks.create({ workflowId: task.workflowId, input: task.input });
      toast(`已重新提交为任务 #${created.id}`, "success");
      navigate(`/tasks/${created.id}`);
    } catch (e) {
      toast(`重新运行失败：${(e as Error).message}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const badge = STREAM_BADGE[streamState];

  return (
    <div className="space-y-6">
      {/* 页头 */}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Link
            to="/tasks"
            className="mb-2 inline-flex items-center gap-1 text-xs text-fg-muted transition-colors hover:text-fg"
          >
            <ChevronLeft size={14} />
            返回任务列表
          </Link>
          <div className="flex flex-wrap items-center gap-2.5">
            <h2 className="tnum text-lg font-semibold tracking-tight text-fg">
              任务 #{task.id}
            </h2>
            <StatusBadge status={task.status} />
            <Badge tone="neutral">{task.workflow}</Badge>
          </div>
          <p className="mt-1.5 max-w-2xl text-xs leading-relaxed text-fg-subtle">
            {task.input || "（无输入）"}
          </p>
        </div>

        <div className="flex items-center gap-2">
          {task.status === "running" || task.status === "pending" || task.status === "waiting" ? (
            <Button
              variant="danger"
              size="sm"
              icon={<Ban size={13} />}
              onClick={handleCancel}
              disabled={busy}
            >
              取消任务
            </Button>
          ) : (
            <Button
              variant="outline"
              size="sm"
              icon={<RotateCcw size={13} />}
              onClick={handleRerun}
              disabled={busy}
            >
              重新运行
            </Button>
          )}
        </div>
      </div>

      {/* 指标 */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Card>
          {/* 后端 task 没有 duration_ms：api 层用 finished_at - started_at 计算 */}
          <MiniStat label="总耗时" value={fmtDuration(task.durationMs)} tone="brand" />
          <MiniStat label="开始时间" value={clockOf(task.startedAt)} />
        </Card>
        <Card>
          <MiniStat
            label="输入 token"
            value={hasTokens ? tokensIn.toLocaleString() : "—"}
          />
          <MiniStat
            label="输出 token"
            value={hasTokens ? tokensOut.toLocaleString() : "—"}
            tone="sky"
          />
        </Card>
        <Card>
          <MiniStat label="节点总数" value={nodes.length} />
          <MiniStat
            label="成功节点"
            value={nodes.filter((n) => n.status === "succeeded").length}
            tone="mint"
          />
        </Card>
        <Card>
          <MiniStat label="重试次数" value={retries} />
          <MiniStat label="创建于" value={timeAgo(task.createdAt ?? task.startedAt)} />
        </Card>
      </div>

      <div className="grid gap-4 xl:grid-cols-5">
        {/* 节点时间线 */}
        <Card className="xl:col-span-3">
          <CardHeader
            title="节点执行明细"
            subtitle="按执行顺序排列 · 入度归零即并行触发"
            action={
              <Badge tone="sky">
                <StatusDot tone="sky" pulse={task.status === "running"} />
                {nodes.length} 节点
              </Badge>
            }
          />

          <div>
            {nodes.length === 0 && (
              <p className="py-4 text-xs text-fg-subtle">
                该任务还没有节点明细（任务可能仍在排队）。
              </p>
            )}
            {nodes.map((n, i) => {
              const Icon = TYPE_ICON[n.type] ?? Sparkles;
              const isLast = i === nodes.length - 1;
              const dur = numberOrNull(n.durationMs);
              const tIn = numberOrNull(n.tokensIn);
              const tOut = numberOrNull(n.tokensOut);
              return (
                <div key={n.key || i} className="flex gap-3">
                  {/* 左侧时间线轨道 */}
                  <div className="flex flex-col items-center pt-1">
                    <span
                      className={cn(
                        "h-2.5 w-2.5 shrink-0 rounded-full ring-4",
                        n.status === "succeeded" && "bg-mint ring-mint/15",
                        n.status === "running" && "animate-breathe bg-sky ring-sky/15",
                        n.status === "failed" && "bg-rose ring-rose/15",
                        (n.status === "pending" ||
                          n.status === "skipped" ||
                          n.status === "cancelled" ||
                          n.status === "waiting") &&
                          "bg-line-strong ring-line/40",
                      )}
                    />
                    {!isLast && <span className="my-1 w-px flex-1 bg-line" />}
                  </div>

                  {/* 内容 */}
                  <div className={cn("min-w-0 flex-1", !isLast && "pb-5")}>
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-xs font-medium text-fg">
                        {n.key || "—"}
                      </span>
                      <Badge tone="neutral">
                        <Icon size={10} />
                        {TYPE_LABEL[n.type] ?? n.type}
                      </Badge>
                      <StatusBadge status={n.status} />
                      <span className="tnum ml-auto text-2xs text-fg-subtle">
                        {dur !== null && dur > 0 ? fmtDuration(dur) : "—"}
                      </span>
                    </div>

                    <div className="mt-1.5 flex flex-wrap items-center gap-3 text-2xs text-fg-subtle">
                      {tIn !== null && tIn > 0 && (
                        <span className="tnum font-mono">
                          {tIn.toLocaleString()} → {tOut?.toLocaleString() ?? "—"} tok
                        </span>
                      )}
                      {n.message && (
                        <span className="font-mono text-fg-muted">{n.message}</span>
                      )}
                    </div>

                    {/* 耗时条 */}
                    {dur !== null && dur > 0 && (
                      <div className="mt-2 h-1 overflow-hidden rounded-full bg-surface-2">
                        <div
                          className={cn("h-full rounded-full", statusTone[n.status])}
                          style={{ width: `${(dur / maxDur) * 100}%` }}
                        />
                      </div>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        </Card>

        {/* 右侧：事件流 / 输出 */}
        <div className="space-y-4 xl:col-span-2">
          <Card pad={false}>
            <div className="flex items-center justify-between px-5 pb-3 pt-5">
              <Segmented
                value={tab}
                onChange={setTab}
                options={[
                  { value: "events", label: "事件流" },
                  { value: "output", label: "最终输出" },
                ]}
              />
              {tab === "events" && (
                <Badge tone={badge.tone}>
                  <Radio size={10} />
                  {badge.label}
                </Badge>
              )}
            </div>

            <div className="px-5 pb-5">
              {tab === "events" ? (
                <>
                  <p className="mb-2 font-mono text-2xs text-fg-subtle">
                    GET /api/tasks/{taskId}/stream{streamDetail ? ` · ${streamDetail}` : ""}
                  </p>
                  <div className="scrollbar-none max-h-[420px] space-y-1 overflow-y-auto">
                    {events.length === 0 ? (
                      <p className="py-3 text-xs text-fg-subtle">等待服务端事件…</p>
                    ) : (
                      events.map((e) => (
                        <div
                          key={e.id}
                          className="flex items-start gap-2.5 rounded-md px-2 py-1.5 transition-colors hover:bg-surface-2"
                        >
                          <span className="tnum shrink-0 font-mono text-2xs text-fg-subtle">
                            {clockOf(e.at)}
                          </span>
                          <span
                            className={cn(
                              "shrink-0 rounded px-1.5 py-px font-mono text-2xs",
                              e.type === "node_failed" || e.type === "task_failed"
                                ? "bg-rose/12 text-rose"
                                : e.type === "node_completed" || e.type === "task_completed"
                                  ? "bg-mint/12 text-mint"
                                  : e.type.startsWith("task_")
                                    ? "bg-brand-soft text-brand"
                                    : "bg-surface-3 text-fg-muted",
                            )}
                          >
                            {e.type}
                          </span>
                          <span className="min-w-0 flex-1 break-words text-2xs text-fg-muted">
                            {e.text}
                          </span>
                        </div>
                      ))
                    )}
                  </div>
                </>
              ) : task.output ? (
                <div>
                  {/* 真实输出：model.Task.output */}
                  <pre className="scrollbar-none max-h-[380px] overflow-auto whitespace-pre-wrap rounded-lg border border-line bg-surface-2/60 p-3.5 font-mono text-2xs leading-relaxed text-fg-muted">
                    {task.output}
                  </pre>
                  <Button
                    variant="outline"
                    size="sm"
                    block
                    className="mt-3"
                    icon={<Copy size={13} />}
                    onClick={() => {
                      void navigator.clipboard?.writeText(task.output ?? "");
                      toast("已复制到剪贴板", "success");
                    }}
                  >
                    复制输出
                  </Button>
                </div>
              ) : (
                <EmptyState
                  icon={<Clock size={18} />}
                  title="暂无输出"
                  description={
                    task.status === "running"
                      ? "任务仍在执行，完成后此处显示最终结果。"
                      : task.error
                        ? `任务失败：${task.error}`
                        : "该任务没有产生最终输出。"
                  }
                />
              )}
            </div>
          </Card>
        </div>
      </div>

      <ToastContainer toasts={toasts} />
    </div>
  );
}

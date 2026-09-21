import { useState } from "react";
import { Link, useParams } from "react-router-dom";
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
  MiniStat,
  Segmented,
  StatusBadge,
  StatusDot,
  statusMeta,
} from "@/components/ui";
import { TASKS, type TaskNodeRow } from "@/lib/mock";
import { clockOf, cn, fmtDuration, timeAgo } from "@/lib/utils";

const TYPE_ICON: Record<TaskNodeRow["type"], LucideIcon> = {
  input: LogIn,
  llm: Sparkles,
  rag: Database,
  tool: Wrench,
  output: LogOut,
};

const TYPE_LABEL: Record<TaskNodeRow["type"], string> = {
  input: "输入",
  llm: "LLM",
  rag: "RAG",
  tool: "工具",
  output: "输出",
};

/** 事件流（mock）：真实实现由 SSE 逐条追加。 */
function buildEvents(taskId: number, nodes: TaskNodeRow[]) {
  const base = Date.now() - 2300;
  const out: { at: string; type: string; text: string }[] = [
    { at: new Date(base).toISOString(), type: "task_created", text: `任务 #${taskId} 已创建` },
    { at: new Date(base + 40).toISOString(), type: "task_running", text: "调度器开始执行" },
  ];
  let t = base + 60;
  nodes.forEach((n) => {
    out.push({
      at: new Date(t).toISOString(),
      type: "node_started",
      text: `${n.key} 开始执行`,
    });
    t += Math.max(n.durationMs, 8);
    out.push({
      at: new Date(t).toISOString(),
      type:
        n.status === "failed"
          ? "node_failed"
          : n.status === "cancelled"
            ? "node_cancelled"
            : n.status === "skipped"
              ? "node_skipped"
              : "node_completed",
      text: `${n.key} ${statusMeta(n.status).label}${
        n.message ? ` · ${n.message}` : ""
      }`,
    });
  });
  return out;
}

export default function TaskDetail() {
  const { id } = useParams();
  const taskId = Number(id);
  const task = TASKS.find((t) => t.id === taskId) ?? TASKS[0];
  const [tab, setTab] = useState<"events" | "output">("events");

  const events = buildEvents(task.id, task.nodes);
  const tokensIn = task.nodes.reduce((s, n) => s + n.tokensIn, 0);
  const tokensOut = task.nodes.reduce((s, n) => s + n.tokensOut, 0);
  const maxDur = Math.max(...task.nodes.map((n) => n.durationMs), 1);

  const statusTone: Record<string, string> = {
    succeeded: "bg-mint",
    running: "bg-sky",
    failed: "bg-rose",
    pending: "bg-line-strong",
    skipped: "bg-line-strong",
    cancelled: "bg-line-strong",
  };

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
            {task.input}
          </p>
        </div>

        <div className="flex items-center gap-2">
          {task.status === "running" ? (
            <Button variant="danger" size="sm" icon={<Ban size={13} />}>
              取消任务
            </Button>
          ) : (
            <Button variant="outline" size="sm" icon={<RotateCcw size={13} />}>
              重新运行
            </Button>
          )}
        </div>
      </div>

      {/* 指标 */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Card>
          <MiniStat label="总耗时" value={fmtDuration(task.durationMs)} tone="brand" />
          <MiniStat label="开始时间" value={clockOf(task.startedAt)} />
        </Card>
        <Card>
          <MiniStat label="输入 token" value={tokensIn.toLocaleString()} />
          <MiniStat label="输出 token" value={tokensOut.toLocaleString()} tone="sky" />
        </Card>
        <Card>
          <MiniStat label="节点总数" value={task.nodes.length} />
          <MiniStat
            label="成功节点"
            value={task.nodes.filter((n) => n.status === "succeeded").length}
            tone="mint"
          />
        </Card>
        <Card>
          <MiniStat label="重试次数" value={0} />
          <MiniStat label="创建于" value={timeAgo(task.startedAt)} />
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
                {task.nodes.length} 节点
              </Badge>
            }
          />

          <div>
            {task.nodes.map((n, i) => {
              const Icon = TYPE_ICON[n.type];
              const meta = statusMeta(n.status);
              const isLast = i === task.nodes.length - 1;
              return (
                <div key={n.key} className="flex gap-3">
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
                          n.status === "cancelled") &&
                          "bg-line-strong ring-line/40",
                      )}
                    />
                    {!isLast && <span className="my-1 w-px flex-1 bg-line" />}
                  </div>

                  {/* 内容 */}
                  <div className={cn("min-w-0 flex-1", !isLast && "pb-5")}>
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-xs font-medium text-fg">
                        {n.key}
                      </span>
                      <Badge tone="neutral">
                        <Icon size={10} />
                        {TYPE_LABEL[n.type]}
                      </Badge>
                      <StatusBadge status={n.status} />
                      <span className="tnum ml-auto text-2xs text-fg-subtle">
                        {n.durationMs > 0 ? fmtDuration(n.durationMs) : "—"}
                      </span>
                    </div>

                    <div className="mt-1.5 flex flex-wrap items-center gap-3 text-2xs text-fg-subtle">
                      {n.tokensIn > 0 && (
                        <span className="tnum font-mono">
                          {n.tokensIn.toLocaleString()} →{" "}
                          {n.tokensOut.toLocaleString()} tok
                        </span>
                      )}
                      {n.message && (
                        <span className="font-mono text-fg-muted">{n.message}</span>
                      )}
                    </div>

                    {/* 耗时条 */}
                    {n.durationMs > 0 && (
                      <div className="mt-2 h-1 overflow-hidden rounded-full bg-surface-2">
                        <div
                          className={cn("h-full rounded-full", statusTone[n.status])}
                          style={{ width: `${(n.durationMs / maxDur) * 100}%` }}
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
                <Badge tone="mint">
                  <Radio size={10} />
                  已连接
                </Badge>
              )}
            </div>

            <div className="px-5 pb-5">
              {tab === "events" ? (
                <div className="scrollbar-none max-h-[420px] space-y-1 overflow-y-auto">
                  {events.map((e, i) => (
                    <div
                      key={i}
                      className="flex items-start gap-2.5 rounded-md px-2 py-1.5 transition-colors hover:bg-surface-2"
                    >
                      <span className="tnum shrink-0 font-mono text-2xs text-fg-subtle">
                        {clockOf(e.at)}
                      </span>
                      <span
                        className={cn(
                          "shrink-0 rounded px-1.5 py-px font-mono text-2xs",
                          e.type === "node_failed"
                            ? "bg-rose/12 text-rose"
                            : e.type === "node_completed"
                              ? "bg-mint/12 text-mint"
                              : e.type.startsWith("task_")
                                ? "bg-brand-soft text-brand"
                                : "bg-surface-3 text-fg-muted",
                        )}
                      >
                        {e.type}
                      </span>
                      <span className="min-w-0 flex-1 text-2xs text-fg-muted">
                        {e.text}
                      </span>
                    </div>
                  ))}
                </div>
              ) : task.status === "succeeded" ? (
                <div>
                  <pre className="scrollbar-none max-h-[380px] overflow-auto whitespace-pre-wrap rounded-lg border border-line bg-surface-2/60 p-3.5 font-mono text-2xs leading-relaxed text-fg-muted">
{`【技术分析报告】

## 1. 调度引擎
采用 Kahn 拓扑排序做环检测，运行期以入度计数驱动就绪节点。
同层节点全部提交给 Worker Pool 并发执行，失败时递归跳过全部下游。

## 2. 并发控制
固定 20 个 worker，队列满时 Submit 阻塞形成背压，
避免无界堆积；优雅关闭采用两阶段排空。

## 3. 可靠投递
Redis Stream + Consumer Group，PEL 与 XAUTOCLAIM
处理崩溃恢复，ZSET 做延迟重投，重试用尽进 DLQ。`}
                  </pre>
                  <Button
                    variant="outline"
                    size="sm"
                    block
                    className="mt-3"
                    icon={<Copy size={13} />}
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
                      : "该任务未产生最终输出。"
                  }
                />
              )}
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}

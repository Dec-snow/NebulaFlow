import { useState } from "react";
import { Link } from "react-router-dom";
import {
  Activity as ActivityIcon,
  AlertTriangle,
  ArrowUpRight,
  CircleDot,
  RefreshCw,
  Server,
  Zap,
} from "lucide-react";
import {
  AreaChart,
  BarSeries,
  Badge,
  Button,
  Card,
  CardHeader,
  ErrorState,
  MiniStat,
  PageLoader,
  RingProgress,
  Segmented,
  Stat,
  StatusBadge,
  StatusDot,
} from "@/components/ui";
import { api } from "@/api";
import { useFetch } from "@/hooks";
import type { ActivityKind } from "@/types";
import { clockOf, cn, numberOrNull } from "@/lib/utils";

/** 活动流列表最大保留条数，超出截断（避免长时间运行后内存膨胀） */
const ACTIVITY_MAX_ITEMS = 200;

const KIND_ICON: Record<ActivityKind, typeof Zap> = {
  task: Zap,
  node: CircleDot,
  system: Server,
  error: AlertTriangle,
};

const KIND_TONE: Record<ActivityKind, string> = {
  task: "bg-brand-soft text-brand",
  node: "bg-sky/12 text-sky",
  system: "bg-surface-3 text-fg-muted",
  error: "bg-rose/12 text-rose",
};

export default function Dashboard() {
  const [range, setRange] = useState<"24h" | "7d" | "30d">("24h");
  const { data, loading, error, refresh } = useFetch(
    () => api.dashboard.get(range),
    [range],
  );

  if (loading || !data) return <PageLoader text="加载控制台数据…" />;

  if (error) return <ErrorState title="加载控制台数据失败" description={error.message} onRetry={refresh} />;

  const {
    kpis: rawKpis,
    throughput: rawThroughput,
    throughputLabels: rawThroughputLabels,
    workerStats: rawWorkerStats,
    tokenByProvider: rawTokenByProvider,
    activity: rawActivityData,
    recentTasks: rawRecentTasks,
  } = data;

  // 接口可能整个不下发某个数组（后端的 nodes / documents / tokens 都是 omitempty），
  // 直接 .map/.slice/.reduce 会抛 TypeError，把整页交给 ErrorBoundary —— 这正是
  // 线上 Dashboard 白屏的同一类成因。数组一律先收成数组。
  const kpis = Array.isArray(rawKpis) ? rawKpis : [];
  const throughput = Array.isArray(rawThroughput) ? rawThroughput : [];
  const throughputLabels = Array.isArray(rawThroughputLabels) ? rawThroughputLabels : [];
  const tokenByProvider = Array.isArray(rawTokenByProvider) ? rawTokenByProvider : [];
  const rawActivity = Array.isArray(rawActivityData) ? rawActivityData : [];
  const recentTasks = Array.isArray(rawRecentTasks) ? rawRecentTasks : [];
  const workerStats = rawWorkerStats ?? {};

  // 活动流最多保留 200 条，超出截断
  const activity = rawActivity.slice(0, ACTIVITY_MAX_ITEMS);

  // 统计数值缺失时渲染占位符，而不是 "undefined" / "NaN" / "-Infinity"
  const activeWorkers = numberOrNull(workerStats.active);
  const totalWorkers = numberOrNull(workerStats.total);
  const utilization = numberOrNull(workerStats.utilization);
  const queued = numberOrNull(workerStats.queued);
  const queueCapacity = numberOrNull(workerStats.queueCapacity);
  const peak = throughput.length > 0 ? Math.max(...throughput) : null;

  return (
    <div className="space-y-7">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">系统概览</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            实时指标 · 最后更新 {clockOf(new Date().toISOString())}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Segmented
            value={range}
            onChange={(v) => setRange(v as "24h" | "7d" | "30d")}
            options={[
              { value: "24h", label: "24 小时" },
              { value: "7d", label: "7 天" },
              { value: "30d", label: "30 天" },
            ]}
          />
          <Button variant="outline" size="sm" icon={<RefreshCw size={13} />} onClick={refresh}>
            刷新
          </Button>
        </div>
      </div>

      {/* KPI */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {kpis.map((k) => (
          <Stat
            key={k.key}
            label={k.label}
            value={numberOrNull(k.value)?.toLocaleString() ?? "—"}
            unit={k.unit}
            delta={k.delta}
            deltaGood={k.deltaGood}
            spark={k.spark}
            sparkTone={k.tone}
            icon={<ActivityIcon size={13} />}
          />
        ))}
      </div>

      {/* 趋势 + Worker 利用率 */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader
            title="任务吞吐量"
            subtitle={`按小时统计 · 峰值 ${peak ?? "—"} 个任务`}
            action={
              <Badge tone="mint">
                <StatusDot tone="mint" />
                运行正常
              </Badge>
            }
          />
          <AreaChart data={throughput} labels={throughputLabels} tone="brand" />
        </Card>

        <Card>
          <CardHeader title="Worker 利用率" subtitle={`${activeWorkers ?? "—"} / ${totalWorkers ?? "—"} 活跃`} />
          <div className="flex flex-col items-center gap-5 py-2">
            <RingProgress
              value={utilization ?? 0}
              tone="brand"
              label={utilization !== null ? `${Math.round(utilization * 100)}%` : "—"}
              sublabel="已占用"
            />
            <div className="w-full space-y-0.5">
              <MiniStat label="活跃 worker" value={activeWorkers ?? "—"} tone="brand" />
              <MiniStat label="池内排队" value={queued ?? "—"} tone="sky" />
              <MiniStat label="队列容量" value={queueCapacity ?? "—"} />
              <MiniStat label="P95 排队等待" value="18ms" tone="mint" />
            </div>
          </div>
        </Card>
      </div>

      {/* 实时活动 + Token 分布 */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader
            title="实时活动"
            subtitle="SSE 推送 · 节点状态与 token 流"
            action={
              <Badge tone="sky">
                <StatusDot tone="sky" pulse />
                已连接
              </Badge>
            }
          />
          <div className="scrollbar-none max-h-[268px] space-y-1 overflow-y-auto pr-1">
            {activity.map((item) => {
              // kind 可能不在枚举内：没有兜底时 <Icon/> 的 type 是 undefined，
              // React 会直接抛 "Element type is invalid"。
              const Icon = KIND_ICON[item.kind] ?? Zap;
              return (
                <div
                  key={item.id}
                  className="flex items-start gap-3 rounded-lg px-2 py-2 transition-colors hover:bg-surface-2"
                >
                  <div
                    className={cn(
                      "mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-md",
                      KIND_TONE[item.kind] ?? "bg-surface-3 text-fg-muted",
                    )}
                  >
                    <Icon size={12.5} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-xs text-fg">{item.text}</div>
                    {item.meta && (
                      <div className="mt-0.5 truncate font-mono text-2xs text-fg-subtle">
                        {item.meta}
                      </div>
                    )}
                  </div>
                  <span className="tnum shrink-0 font-mono text-2xs text-fg-subtle">
                    {clockOf(item.at)}
                  </span>
                </div>
              );
            })}
          </div>
        </Card>

        <Card>
          <CardHeader title="Token 消耗分布" subtitle="按 Provider 聚合" />
          <BarSeries items={tokenByProvider} />
          <div className="divider my-4" />
          <div className="space-y-0.5">
            <MiniStat label="总消耗" value="3.42M" tone="brand" />
            <MiniStat label="输入 / 输出" value="1.9M / 1.5M" />
            <MiniStat label="缓存命中率" value="41%" tone="mint" />
          </div>
        </Card>
      </div>

      {/* 最近任务 */}
      <Card pad={false}>
        <div className="flex items-center justify-between px-5 pb-3.5 pt-5">
          <div>
            <h3 className="text-sm font-semibold text-fg">最近任务</h3>
            <p className="mt-0.5 text-xs text-fg-subtle">最新 5 条执行记录</p>
          </div>
          <Link
            to="/tasks"
            className="inline-flex items-center gap-1 text-xs text-brand hover:underline"
          >
            查看全部
            <ArrowUpRight size={12} />
          </Link>
        </div>

        <div className="px-2 pb-2">
          {recentTasks.map((t) => {
            // 后端的 task 没有 duration_ms / tokens 这类汇总字段（只有 task_node 上有），
            // 缺失时渲染占位符，而不是 "undefinedms" / "NaN"。
            const dur = numberOrNull(t.durationMs);
            const tokens = numberOrNull(t.tokens);
            return (
              <Link
                key={t.id}
                to={`/tasks/${t.id}`}
                className="flex items-center gap-3 rounded-lg px-3 py-2.5 transition-colors hover:bg-surface-2"
              >
                <span className="tnum w-14 shrink-0 font-mono text-xs text-fg-subtle">
                  #{t.id}
                </span>
                <span className="min-w-0 flex-1 truncate text-xs text-fg">{t.workflow}</span>
                <StatusBadge status={t.status} />
                <span className="tnum hidden w-16 shrink-0 text-right text-2xs text-fg-subtle sm:block">
                  {dur === null ? "—" : dur >= 1000 ? `${(dur / 1000).toFixed(1)}s` : `${dur}ms`}
                </span>
                <span className="tnum hidden w-16 shrink-0 text-right text-2xs text-fg-subtle sm:block">
                  {tokens?.toLocaleString() ?? "—"} tok
                </span>
              </Link>
            );
          })}
        </div>
      </Card>
    </div>
  );
}

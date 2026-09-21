import { useState } from "react";
import { Link } from "react-router-dom";
import {
  Activity as ActivityIcon,
  AlertTriangle,
  ArrowUpRight,
  Boxes,
  CircleDot,
  Clock,
  Cpu,
  Layers,
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
  MiniStat,
  RingProgress,
  Segmented,
  Stat,
  StatusBadge,
  StatusDot,
} from "@/components/ui";
import {
  ACTIVITY,
  KPIS,
  THROUGHPUT,
  THROUGHPUT_LABELS,
  TOKEN_BY_PROVIDER,
  WORKER_STATS,
  type ActivityKind,
} from "@/lib/mock";
import { clockOf, cn, fmtDuration } from "@/lib/utils";

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

  return (
    <div className="space-y-6">
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
            onChange={setRange}
            options={[
              { value: "24h", label: "24 小时" },
              { value: "7d", label: "7 天" },
              { value: "30d", label: "30 天" },
            ]}
          />
          <Button variant="outline" size="sm" icon={<RefreshCw size={13} />}>
            刷新
          </Button>
        </div>
      </div>

      {/* KPI */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {KPIS.map((k) => (
          <Stat
            key={k.key}
            label={k.label}
            value={k.value.toLocaleString()}
            unit={k.unit}
            delta={k.delta}
            deltaGood={k.deltaGood}
            spark={k.spark}
            sparkTone={k.tone}
            icon={<ActivityIcon size={13} />}
          />
        ))}
      </div>

      {/* 趋势 + 资源 */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader
            title="任务吞吐量"
            subtitle={`按小时统计 · 峰值 ${Math.max(...THROUGHPUT)} 个任务`}
            action={
              <Badge tone="mint">
                <StatusDot tone="mint" />
                运行正常
              </Badge>
            }
          />
          <AreaChart data={THROUGHPUT} labels={THROUGHPUT_LABELS} tone="brand" />
        </Card>

        <Card>
          <CardHeader title="Worker 利用率" subtitle={`${WORKER_STATS.active} / ${WORKER_STATS.total} 活跃`} />
          <div className="flex flex-col items-center gap-5 py-2">
            <RingProgress
              value={WORKER_STATS.utilization}
              tone="brand"
              label={`${Math.round(WORKER_STATS.utilization * 100)}%`}
              sublabel="已占用"
            />
            <div className="w-full space-y-0.5">
              <MiniStat label="活跃 worker" value={WORKER_STATS.active} tone="brand" />
              <MiniStat label="池内排队" value={WORKER_STATS.queued} tone="sky" />
              <MiniStat label="队列容量" value={WORKER_STATS.queueCapacity} />
            </div>
          </div>
        </Card>
      </div>

      {/* 队列 / 活动流 */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card>
          <CardHeader
            title="Worker Pool"
            subtitle="固定 20 worker · 背压已生效"
            action={<Badge tone="sky">队列 {WORKER_STATS.queued}</Badge>}
          />
          <div className="space-y-4">
            {/* worker 槽位可视化 */}
            <div>
              <div className="mb-2 flex items-center justify-between text-2xs text-fg-subtle">
                <span>槽位占用</span>
                <span className="tnum">
                  {WORKER_STATS.active}/{WORKER_STATS.total}
                </span>
              </div>
              <div className="grid grid-cols-10 gap-1.5">
                {Array.from({ length: WORKER_STATS.total }).map((_, i) => (
                  <div
                    key={i}
                    title={`worker-${String(i + 1).padStart(2, "0")}`}
                    className={cn(
                      "h-6 rounded-[5px] transition-colors duration-300",
                      i < WORKER_STATS.active
                        ? "bg-brand"
                        : "bg-surface-3",
                    )}
                  />
                ))}
              </div>
            </div>

            <div className="divider" />

            <div className="space-y-0.5">
              <MiniStat label="P95 排队等待" value="18ms" tone="mint" />
              <MiniStat label="P95 节点执行" value="1.4s" />
              <MiniStat label="背压触发次数" value="3" tone="amber" />
            </div>
          </div>
        </Card>

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
            {ACTIVITY.map((item) => {
              const Icon = KIND_ICON[item.kind];
              return (
                <div
                  key={item.id}
                  className="flex items-start gap-3 rounded-lg px-2 py-2 transition-colors hover:bg-surface-2"
                >
                  <div
                    className={cn(
                      "mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-md",
                      KIND_TONE[item.kind],
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
      </div>

      {/* Token 分布 + 最近任务 */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card>
          <CardHeader title="Token 消耗分布" subtitle="按 Provider 聚合" />
          <BarSeries items={TOKEN_BY_PROVIDER} />
          <div className="divider my-4" />
          <div className="space-y-0.5">
            <MiniStat label="总消耗" value="3.42M" tone="brand" />
            <MiniStat label="输入 / 输出" value="1.9M / 1.5M" />
            <MiniStat label="缓存命中率" value="41%" tone="mint" />
          </div>
        </Card>

        <Card className="lg:col-span-2" pad={false}>
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
            {[
              { id: 1284, wf: "技术分析工作流", st: "succeeded", ms: 2302, tok: 3796 },
              { id: 1283, wf: "多模型对比评测", st: "running", ms: 3180, tok: 1930 },
              { id: 1282, wf: "知识库问答", st: "succeeded", ms: 1210, tok: 1560 },
              { id: 1279, wf: "定时数据巡检", st: "failed", ms: 640, tok: 0 },
              { id: 1278, wf: "知识库问答", st: "succeeded", ms: 980, tok: 1420 },
            ].map((t) => (
              <Link
                key={t.id}
                to={`/tasks/${t.id}`}
                className="flex items-center gap-3 rounded-lg px-3 py-2.5 transition-colors hover:bg-surface-2"
              >
                <span className="tnum w-14 shrink-0 font-mono text-xs text-fg-subtle">
                  #{t.id}
                </span>
                <span className="min-w-0 flex-1 truncate text-xs text-fg">{t.wf}</span>
                <StatusBadge status={t.st} />
                <span className="tnum hidden w-16 shrink-0 text-right text-2xs text-fg-subtle sm:block">
                  {fmtDuration(t.ms)}
                </span>
                <span className="tnum hidden w-16 shrink-0 text-right text-2xs text-fg-subtle sm:block">
                  {t.tok.toLocaleString()} tok
                </span>
              </Link>
            ))}
          </div>
        </Card>
      </div>

      {/* 底部：系统资源 */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {[
          { icon: Cpu, label: "CPU", value: "38%", sub: "8 vCPU" },
          { icon: Layers, label: "内存", value: "1.2 GB", sub: "上限 2 GB" },
          { icon: Boxes, label: "Postgres 连接", value: "6 / 20", sub: "池使用率 30%" },
          { icon: Clock, label: "Redis PEL", value: "0", sub: "无遗留消息" },
        ].map(({ icon: Icon, label, value, sub }) => (
          <Card key={label} className="flex items-center gap-3.5">
            <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-surface-2 text-fg-muted">
              <Icon size={15} />
            </div>
            <div className="min-w-0">
              <div className="text-2xs text-fg-subtle">{label}</div>
              <div className="tnum text-sm font-semibold text-fg">{value}</div>
              <div className="truncate text-2xs text-fg-subtle">{sub}</div>
            </div>
          </Card>
        ))}
      </div>
    </div>
  );
}

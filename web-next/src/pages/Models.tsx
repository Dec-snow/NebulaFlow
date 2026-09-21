import {
  ArrowRight,
  Boxes,
  Check,
  Gauge,
  MoreHorizontal,
  Plus,
  Server,
  Zap,
} from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  IconButton,
  StatusBadge,
} from "@/components/ui";
import { PROVIDERS, type Provider } from "@/lib/mock";
import { cn } from "@/lib/utils";

function ProviderCard({ p }: { p: Provider }) {
  const isDown = p.status === "disabled";
  return (
    <Card
      className={cn(
        "flex h-full flex-col transition-all duration-200 ease-smooth",
        isDown ? "opacity-70" : "hover:-translate-y-0.5 hover:shadow-md",
      )}
    >
      <div className="mb-3.5 flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <span
            className={cn(
              "flex h-9 w-9 shrink-0 items-center justify-center rounded-lg",
              isDown ? "bg-surface-2 text-fg-subtle" : "bg-brand-soft text-brand",
            )}
          >
            <Server size={16} />
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-1.5">
              <span className="truncate text-sm font-semibold text-fg">{p.name}</span>
              {p.isDefault && (
                <span title="默认 Provider">
                  <Check size={12} className="text-mint" />
                </span>
              )}
            </div>
            <div className="truncate font-mono text-2xs text-fg-subtle">
              {p.baseUrl}
            </div>
          </div>
        </div>
        <IconButton label="更多操作" variant="ghost">
          <MoreHorizontal size={15} />
        </IconButton>
      </div>

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <StatusBadge status={p.status} />
        <Badge tone="neutral">优先级 {p.priority}</Badge>
        {!isDown && (
          <Badge tone={p.latencyMs > 1500 ? "amber" : "mint"}>
            <Gauge size={10} />
            {p.latencyMs}ms
          </Badge>
        )}
      </div>

      <div className="divider my-1" />

      <div className="mt-3 flex-1">
        <div className="mb-2 text-2xs font-medium uppercase tracking-wider text-fg-subtle">
          可用模型
        </div>
        <div className="space-y-1.5">
          {p.models.map((m) => (
            <div
              key={m.name}
              className="flex items-center justify-between rounded-md border border-line bg-surface-2/40 px-2.5 py-1.5"
            >
              <span className="truncate font-mono text-2xs text-fg">{m.name}</span>
              <span className="tnum shrink-0 text-2xs text-fg-subtle">
                {m.maxTokens.toLocaleString()} tok
              </span>
            </div>
          ))}
        </div>
      </div>

      <div className="mt-4 flex gap-2">
        <Button variant="outline" size="sm" block>
          配置
        </Button>
        <Button variant="ghost" size="sm" block>
          {p.status === "enabled" ? "停用" : "启用"}
        </Button>
      </div>
    </Card>
  );
}

export default function Models() {
  const enabled = PROVIDERS.filter((p) => p.status !== "disabled");

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">
            模型与供应商
          </h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            统一 Provider 抽象 · 按优先级自动故障转移
          </p>
        </div>
        <Button variant="brand" icon={<Plus size={15} />}>
          添加 Provider
        </Button>
      </div>

      {/* 故障转移链路 */}
      <Card>
        <CardHeader
          title="故障转移顺序"
          subtitle="调用失败时按优先级依次降级，首 token 之前可安全切换"
          action={
            <Badge tone="mint">
              <Zap size={10} />
              链路健康
            </Badge>
          }
        />
        <div className="flex flex-wrap items-center gap-2">
          {enabled.map((p, i) => (
            <div key={p.id} className="flex items-center gap-2">
              <div
                className={cn(
                  "flex items-center gap-2 rounded-lg border px-3 py-2",
                  i === 0
                    ? "border-transparent bg-brand-soft ring-1 ring-brand/30"
                    : "border-line bg-surface-2/50",
                )}
              >
                <span
                  className={cn(
                    "h-1.5 w-1.5 rounded-full",
                    p.status === "degraded" ? "bg-amber" : "bg-mint",
                  )}
                />
                <span className="text-xs font-medium text-fg">{p.name}</span>
                <span className="tnum text-2xs text-fg-subtle">{p.latencyMs}ms</span>
              </div>
              {i < enabled.length - 1 && (
                <ArrowRight size={13} className="shrink-0 text-fg-subtle" />
              )}
            </div>
          ))}
          {enabled.length === 0 && (
            <p className="text-xs text-fg-subtle">暂无启用的 Provider</p>
          )}
        </div>
        <div className="mt-4 flex items-center gap-2.5 rounded-lg border border-line bg-surface-2/50 px-3.5 py-2.5">
          <Boxes size={14} className="shrink-0 text-fg-subtle" />
          <p className="text-2xs leading-relaxed text-fg-muted">
            全部 Provider 不可用时自动回退到 Mock Provider，保证离线仍可演示完整链路。
          </p>
        </div>
      </Card>

      {/* Provider 卡片 */}
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
        {PROVIDERS.map((p) => (
          <ProviderCard key={p.id} p={p} />
        ))}
      </div>
    </div>
  );
}

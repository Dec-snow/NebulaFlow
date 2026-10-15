import { useState } from "react";
import {
  Activity as ActivityIcon,
  ArrowUpRight,
  BarChart3,
  Download,
  LineChart,
  TrendingUp,
} from "lucide-react";
import {
  Badge,
  BarSeries,
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorState,
  MiniStat,
  PageLoader,
  Segmented,
  Stat,
} from "@/components/ui";
import { api } from "@/api";
import { useFetch } from "@/hooks";
import { compact, numberOrNull } from "@/lib/utils";

/** Provider 名称 → 统一色调。 */
const TONE_MAP: Record<string, "brand" | "mint" | "sky" | "violet" | "amber"> = {
  deepseek: "brand",
  openai: "mint",
  "智谱 ai": "sky",
  anthropic: "violet",
};

function toneOf(provider: string) {
  return TONE_MAP[provider.toLowerCase()] ?? "brand";
}

export default function CostAnalysis() {
  const [range, setRange] = useState<"7d" | "30d" | "90d">("30d");

  const { data, loading, error, refresh } = useFetch(() => api.cost.get(range), [range]);

  if (loading || !data) return <PageLoader text="加载用量数据…" />;

  if (error) return <ErrorState title="加载用量数据失败" description={error.message} onRetry={refresh} />;

  const totals = data.totals ?? {};
  const byProvider = Array.isArray(data.byProvider) ? data.byProvider : [];
  const byWorkflow = Array.isArray(data.byWorkflow) ? data.byWorkflow : [];

  const totalInput = numberOrNull(totals.totalInputTokens);
  const totalOutput = numberOrNull(totals.totalOutputTokens);
  const totalRequests = numberOrNull(totals.totalRequests);
  const totalTokens =
    totalInput !== null || totalOutput !== null ? (totalInput ?? 0) + (totalOutput ?? 0) : null;
  const hasUsage = byProvider.length > 0;

  const providerBars = byProvider.map((p) => ({
    label: p.provider,
    value: (numberOrNull(p.inputTokens) ?? 0) + (numberOrNull(p.outputTokens) ?? 0),
    tone: toneOf(p.provider),
  }));

  return (
    <div className="space-y-7">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">成本与用量</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            数据来自 /api/dashboard/usage（usage_records 聚合）与任务列表
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Segmented
            value={range}
            onChange={(v) => setRange(v as "7d" | "30d" | "90d")}
            options={[
              { value: "7d", label: "7 天" },
              { value: "30d", label: "30 天" },
              { value: "90d", label: "90 天" },
            ]}
          />
          <Button
            variant="outline"
            size="sm"
            icon={<Download size={13} />}
            onClick={() => {
              // 导出真实数据（CSV），不是摆设按钮
              const header = "provider,requests,input_tokens,output_tokens";
              const rows = byProvider.map((p) =>
                [
                  `"${p.provider.replace(/"/g, '""')}"`,
                  p.totalRequests ?? "",
                  p.inputTokens ?? "",
                  p.outputTokens ?? "",
                ].join(","),
              );
              const blob = new Blob([[header, ...rows].join("\n")], {
                type: "text/csv;charset=utf-8",
              });
              const url = URL.createObjectURL(blob);
              const a = document.createElement("a");
              a.href = url;
              a.download = `nebulaflow-usage-${range}.csv`;
              a.click();
              URL.revokeObjectURL(url);
            }}
          >
            导出 CSV
          </Button>
        </div>
      </div>

      {/* 真实性说明：后端没有单价表，费用不可推算 */}
      <Card className="flex items-start gap-2.5">
        <Badge tone="amber">字段</Badge>
        <p className="text-xs leading-relaxed text-fg-muted">
          后端（model.UsageRecord / GET /api/dashboard/usage）只提供 token 数量与调用次数，
          没有任何计费字段，前端无法在不编造单价的前提下算出金额 ——
          因此本页不展示"费用 / 单价"这类指标，只展示可核对的真实用量。
        </p>
      </Card>

      {/* KPI */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Stat
          label="总 Token"
          value={totalTokens !== null ? compact(totalTokens) : "—"}
          sparkTone="brand"
          icon={<ActivityIcon size={13} />}
        />
        <Stat
          label="输入 Token"
          value={totalInput !== null ? compact(totalInput) : "—"}
          unit="tok"
          sparkTone="violet"
          icon={<LineChart size={13} />}
        />
        <Stat
          label="输出 Token"
          value={totalOutput !== null ? compact(totalOutput) : "—"}
          unit="tok"
          sparkTone="mint"
          icon={<TrendingUp size={13} />}
        />
        <Stat
          label="调用次数"
          value={totalRequests?.toLocaleString() ?? "—"}
          unit="次"
          sparkTone="sky"
          icon={<BarChart3 size={13} />}
        />
      </div>

      {/* 按 Provider 分布 */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader title="Token 用量分布" subtitle="按 Provider 聚合（输入 + 输出）" />
          {providerBars.length > 0 ? (
            <BarSeries items={providerBars} />
          ) : (
            <EmptyState
              icon={<BarChart3 size={18} />}
              title="暂无用量记录"
              description="还没有 LLM 调用产生 usage_records。用带 LLM 节点的工作流跑一个任务即可看到数据。"
            />
          )}
        </Card>

        <Card>
          <CardHeader title="概览" subtitle="usage_records 聚合结果" />
          <div className="space-y-0.5">
            <MiniStat
              label="总消耗"
              value={totalTokens !== null ? compact(totalTokens) : "—"}
              tone="brand"
            />
            <MiniStat
              label="输入 / 输出"
              value={
                totalInput !== null || totalOutput !== null
                  ? `${compact(totalInput ?? 0)} / ${compact(totalOutput ?? 0)}`
                  : "—"
              }
            />
            <MiniStat label="调用次数" value={totalRequests?.toLocaleString() ?? "—"} />
            <MiniStat label="Provider 数" value={byProvider.length} tone="mint" />
            <MiniStat label="覆盖工作流" value={byWorkflow.length} tone="sky" />
          </div>
        </Card>
      </div>

      {/* Provider 明细表 */}
      <Card pad={false}>
        <div className="px-5 pb-3.5 pt-5">
          <h3 className="text-sm font-semibold text-fg">Provider 用量明细</h3>
          <p className="mt-0.5 text-xs text-fg-subtle">输入 / 输出 token 与调用次数</p>
        </div>

        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead>
              <tr className="border-y border-line text-2xs uppercase tracking-wider text-fg-subtle">
                <th className="px-5 py-2.5 font-medium">Provider</th>
                <th className="px-4 py-2.5 text-right font-medium">调用次数</th>
                <th className="px-4 py-2.5 text-right font-medium">输入 Token</th>
                <th className="px-4 py-2.5 text-right font-medium">输出 Token</th>
                <th className="px-5 py-2.5 text-right font-medium">合计</th>
              </tr>
            </thead>
            <tbody>
              {byProvider.map((p) => {
                const input = numberOrNull(p.inputTokens);
                const output = numberOrNull(p.outputTokens);
                return (
                  <tr
                    key={p.provider}
                    className="border-b border-line/60 transition-colors hover:bg-surface-2"
                  >
                    <td className="px-5 py-2.5">
                      <span className="font-mono text-fg">{p.provider}</span>
                    </td>
                    <td className="tnum px-4 py-2.5 text-right text-fg-subtle">
                      {numberOrNull(p.totalRequests)?.toLocaleString() ?? "—"}
                    </td>
                    <td className="tnum px-4 py-2.5 text-right text-fg-subtle">
                      {input?.toLocaleString() ?? "—"}
                    </td>
                    <td className="tnum px-4 py-2.5 text-right text-fg-subtle">
                      {output?.toLocaleString() ?? "—"}
                    </td>
                    <td className="tnum px-5 py-2.5 text-right font-medium text-fg">
                      {input !== null || output !== null
                        ? ((input ?? 0) + (output ?? 0)).toLocaleString()
                        : "—"}
                    </td>
                  </tr>
                );
              })}
              {!hasUsage && (
                <tr>
                  <td colSpan={5} className="px-5 py-4 text-center text-2xs text-fg-subtle">
                    暂无用量记录
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </Card>

      {/* 按工作流任务数排行（token 无法按工作流拆分：usage_records 只记 provider/model） */}
      <Card pad={false}>
        <div className="flex items-center justify-between px-5 pb-3.5 pt-5">
          <div>
            <h3 className="text-sm font-semibold text-fg">工作流任务数排行</h3>
            <p className="mt-0.5 text-xs text-fg-subtle">
              按执行次数排序 · token 维度后端不按工作流聚合，故不展示
            </p>
          </div>
        </div>

        <div className="px-2 pb-2">
          {byWorkflow.length === 0 ? (
            <p className="px-3 py-3 text-xs text-fg-subtle">还没有任务记录</p>
          ) : (
            byWorkflow.map((w) => (
              <div
                key={w.id}
                className="flex items-center gap-3 rounded-lg px-3 py-2.5 transition-colors hover:bg-surface-2"
              >
                <span className="tnum w-10 shrink-0 font-mono text-xs text-fg-subtle">
                  #{w.id}
                </span>
                <span className="min-w-0 flex-1 truncate text-xs text-fg">{w.name}</span>
                <span className="tnum shrink-0 text-right text-xs text-fg-subtle">
                  {w.tasks.toLocaleString()} 次
                </span>
                <ArrowUpRight size={12} className="text-fg-subtle" />
              </div>
            ))
          )}
        </div>
      </Card>
    </div>
  );
}

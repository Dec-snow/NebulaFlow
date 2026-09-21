import { useState } from "react";
import {
  Activity as ActivityIcon,
  ArrowUpRight,
  BarChart3,
  DollarSign,
  LineChart,
  TrendingDown,
  TrendingUp,
} from "lucide-react";
import {
  AreaChart,
  Badge,
  BarSeries,
  Button,
  Card,
  CardHeader,
  MiniStat,
  Segmented,
  Stat,
} from "@/components/ui";
import {
  COST_BY_MODEL,
  COST_BY_WORKFLOW,
  COST_TREND,
  COST_TREND_LABELS,
} from "@/lib/mock";
import { cn } from "@/lib/utils";

const TONE_MAP: Record<string, string> = {
  DeepSeek: "brand",
  OpenAI: "mint",
  "智谱 AI": "sky",
  Anthropic: "violet",
};

export default function CostAnalysis() {
  const [range, setRange] = useState<"7d" | "30d" | "90d">("30d");

  const totalCost = COST_BY_MODEL.reduce((sum, m) => sum + m.cost, 0);
  const totalTokens = COST_BY_MODEL.reduce((sum, m) => sum + m.tokensIn + m.tokensOut, 0);
  const totalCalls = COST_BY_MODEL.reduce((sum, m) => sum + m.calls, 0);
  const avgCostPerTask = totalCost / COST_BY_WORKFLOW.reduce((s, w) => s + w.tasks, 0);

  const modelCostItems = COST_BY_MODEL.map((m) => ({
    label: m.model,
    value: m.cost,
    tone: (TONE_MAP[m.provider] ?? "brand") as "brand" | "mint" | "sky" | "violet",
  }));

  return (
    <div className="space-y-7">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">成本分析</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            Token 消耗与费用明细 · 按模型 / 工作流聚合
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
          <Button variant="outline" size="sm" icon={<BarChart3 size={13} />}>
            导出报表
          </Button>
        </div>
      </div>

      {/* KPI */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Stat
          label="总费用"
          value={`¥${totalCost.toFixed(2)}`}
          delta={8.3}
          deltaGood="down"
          spark={COST_TREND.slice(-12)}
          sparkTone="brand"
          icon={<DollarSign size={13} />}
        />
        <Stat
          label="Token 消耗"
          value={(totalTokens / 1_000_000).toFixed(2)}
          unit="M"
          delta={12.1}
          deltaGood="down"
          tone="violet"
          icon={<ActivityIcon size={13} />}
        />
        <Stat
          label="调用次数"
          value={totalCalls.toLocaleString()}
          unit="次"
          delta={5.7}
          deltaGood="up"
          tone="mint"
          icon={<LineChart size={13} />}
        />
        <Stat
          label="单次均价"
          value={`¥${avgCostPerTask.toFixed(4)}`}
          delta={-2.4}
          deltaGood="down"
          tone="sky"
          icon={<TrendingDown size={13} />}
        />
      </div>

      {/* 成本趋势 + 按模型分布 */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader
            title="成本趋势"
            subtitle="每日总费用（元）"
            action={<Badge tone="brand">近 14 天</Badge>}
          />
          <AreaChart data={COST_TREND} labels={COST_TREND_LABELS} tone="brand" />
        </Card>

        <Card>
          <CardHeader title="按模型分布" subtitle="费用占比 Top 6" />
          <BarSeries items={modelCostItems} />
          <div className="divider my-4" />
          <div className="space-y-0.5">
            <MiniStat label="最贵模型" value="deepseek-r1" tone="brand" />
            <MiniStat label="调用最多" value="deepseek-v3" tone="mint" />
            <MiniStat label="性价比最优" value="glm-4-flash" tone="sky" />
          </div>
        </Card>
      </div>

      {/* 按模型明细表 */}
      <Card pad={false}>
        <div className="px-5 pb-3.5 pt-5">
          <h3 className="text-sm font-semibold text-fg">模型费用明细</h3>
          <p className="mt-0.5 text-xs text-fg-subtle">
            输入 / 输出 token 与对应费用
          </p>
        </div>

        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead>
              <tr className="border-y border-line text-2xs uppercase tracking-wider text-fg-subtle">
                <th className="px-5 py-2.5 font-medium">模型</th>
                <th className="px-4 py-2.5 font-medium">Provider</th>
                <th className="px-4 py-2.5 text-right font-medium">输入 Token</th>
                <th className="px-4 py-2.5 text-right font-medium">输出 Token</th>
                <th className="px-4 py-2.5 text-right font-medium">调用次数</th>
                <th className="px-5 py-2.5 text-right font-medium">费用</th>
              </tr>
            </thead>
            <tbody>
              {COST_BY_MODEL.map((m) => (
                <tr
                  key={m.model}
                  className="border-b border-line/60 transition-colors hover:bg-surface-2"
                >
                  <td className="px-5 py-2.5">
                    <span className="font-mono text-fg">{m.model}</span>
                  </td>
                  <td className="px-4 py-2.5">
                    <Badge tone={TONE_MAP[m.provider] as any} size="sm">
                      {m.provider}
                    </Badge>
                  </td>
                  <td className="tnum px-4 py-2.5 text-right text-fg-subtle">
                    {m.tokensIn.toLocaleString()}
                  </td>
                  <td className="tnum px-4 py-2.5 text-right text-fg-subtle">
                    {m.tokensOut.toLocaleString()}
                  </td>
                  <td className="tnum px-4 py-2.5 text-right text-fg-subtle">
                    {m.calls.toLocaleString()}
                  </td>
                  <td className="tnum px-5 py-2.5 text-right font-medium text-fg">
                    ¥{m.cost.toFixed(2)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>

      {/* 按工作流排行 */}
      <Card pad={false}>
        <div className="flex items-center justify-between px-5 pb-3.5 pt-5">
          <div>
            <h3 className="text-sm font-semibold text-fg">工作流费用排行</h3>
            <p className="mt-0.5 text-xs text-fg-subtle">按费用从高到低</p>
          </div>
        </div>

        <div className="px-2 pb-2">
          {COST_BY_WORKFLOW.map((w) => (
            <div
              key={w.id}
              className="flex items-center gap-3 rounded-lg px-3 py-2.5 transition-colors hover:bg-surface-2"
            >
              <span className="tnum w-10 shrink-0 font-mono text-xs text-fg-subtle">
                #{w.id}
              </span>
              <span className="min-w-0 flex-1 truncate text-xs text-fg">{w.name}</span>
              <span className="tnum hidden w-20 shrink-0 text-right text-2xs text-fg-subtle sm:block">
                {w.tasks.toLocaleString()} 次
              </span>
              <span className="tnum hidden w-20 shrink-0 text-right text-2xs text-fg-subtle sm:block">
                ¥{w.avgCost.toFixed(3)}/次
              </span>
              <span
                className={cn(
                  "tnum flex w-16 shrink-0 items-center justify-end gap-1 text-2xs",
                  w.trend >= 0 ? "text-rose" : "text-mint",
                )}
              >
                {w.trend >= 0 ? <TrendingUp size={11} /> : <TrendingDown size={11} />}
                {Math.abs(w.trend).toFixed(1)}%
              </span>
              <span className="tnum w-20 shrink-0 text-right text-xs font-medium text-fg">
                ¥{w.cost.toFixed(2)}
              </span>
              <ArrowUpRight size={12} className="text-fg-subtle" />
            </div>
          ))}
        </div>
      </Card>
    </div>
  );
}

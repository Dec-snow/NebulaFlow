import type { ReactNode } from "react";
import { ArrowDownRight, ArrowUpRight } from "lucide-react";
import { cn } from "@/lib/utils";
import { Sparkline } from "./Charts";

export interface StatProps {
  label: string;
  value: ReactNode;
  /** 单位后缀，如 "ms" / "tok" */
  unit?: string;
  /** 环比变化百分比，正数向上、负数向下 */
  delta?: number;
  /** 环比语义：升是好事还是坏事（决定配色） */
  deltaGood?: "up" | "down";
  icon?: ReactNode;
  /** 迷你趋势数据 */
  spark?: number[];
  sparkTone?: "brand" | "mint" | "sky" | "violet" | "amber";
  className?: string;
}

export function Stat({
  label,
  value,
  unit,
  delta,
  deltaGood = "up",
  icon,
  spark,
  sparkTone = "brand",
  className,
}: StatProps) {
  const positive = (delta ?? 0) >= 0;
  const isGood = deltaGood === "up" ? positive : !positive;

  return (
    <div className={cn("card card-pad flex flex-col gap-3", className)}>
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium text-fg-muted">{label}</span>
        {icon && (
          <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-surface-2 text-fg-subtle">
            {icon}
          </span>
        )}
      </div>

      <div className="flex items-baseline gap-1.5">
        <span className="tnum text-2xl font-semibold tracking-tight text-fg">{value}</span>
        {unit && <span className="text-xs text-fg-subtle">{unit}</span>}
      </div>

      {spark && spark.length > 1 && <Sparkline data={spark} tone={sparkTone} />}

      {delta != null && (
        <div className="flex items-center gap-1.5">
          <span
            className={cn(
              "badge",
              isGood ? "bg-mint/12 text-mint" : "bg-rose/12 text-rose",
            )}
          >
            {positive ? <ArrowUpRight size={11} /> : <ArrowDownRight size={11} />}
            <span className="tnum">{Math.abs(delta).toFixed(1)}%</span>
          </span>
          <span className="text-2xs text-fg-subtle">较上周期</span>
        </div>
      )}
    </div>
  );
}

/** 紧凑指标：用于侧栏或卡内嵌的小数据行。 */
export function MiniStat({
  label,
  value,
  tone = "neutral",
}: {
  label: string;
  value: ReactNode;
  tone?: "neutral" | "mint" | "sky" | "amber" | "rose" | "brand";
}) {
  const toneMap: Record<string, string> = {
    neutral: "text-fg",
    mint: "text-mint",
    sky: "text-sky",
    amber: "text-amber",
    rose: "text-rose",
    brand: "text-brand",
  };
  return (
    <div className="flex items-baseline justify-between gap-3 py-1.5">
      <span className="text-xs text-fg-subtle">{label}</span>
      <span className={cn("tnum text-sm font-medium", toneMap[tone])}>{value}</span>
    </div>
  );
}

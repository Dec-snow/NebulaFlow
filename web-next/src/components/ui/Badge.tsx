import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export type Tone = "mint" | "sky" | "amber" | "rose" | "violet" | "neutral";

const BADGE_TONE: Record<Tone, string> = {
  mint: "badge-mint",
  sky: "badge-sky",
  amber: "badge-amber",
  rose: "badge-rose",
  violet: "badge-violet",
  neutral: "badge-neutral",
};

const DOT_TONE: Record<Tone, string> = {
  mint: "dot-mint",
  sky: "dot-sky",
  amber: "dot-amber",
  rose: "dot-rose",
  violet: "dot-sky",
  neutral: "dot-subtle",
};

export function Badge({
  tone = "neutral",
  className,
  children,
}: {
  tone?: Tone;
  className?: string;
  children: ReactNode;
}) {
  return <span className={cn(BADGE_TONE[tone], className)}>{children}</span>;
}

/** 状态圆点。pulse 用于"进行中"状态，产生呼吸感。 */
export function StatusDot({ tone = "neutral", pulse }: { tone?: Tone; pulse?: boolean }) {
  return <span className={cn(DOT_TONE[tone], pulse && "animate-breathe")} />;
}

/* -------------------------------------------------------------------------
   领域状态 → 视觉语义 的统一映射。
   放在这里是为了让全站状态配色一致：同一个 succeeded 在任何页面都是同一种绿。
   ---------------------------------------------------------------------- */

interface StatusMeta {
  label: string;
  tone: Tone;
  pulse?: boolean;
}

const STATUS: Record<string, StatusMeta> = {
  // 任务 / 节点
  succeeded: { label: "成功", tone: "mint" },
  running: { label: "运行中", tone: "sky", pulse: true },
  pending: { label: "排队中", tone: "neutral" },
  queued: { label: "排队中", tone: "neutral" },
  failed: { label: "失败", tone: "rose" },
  cancelled: { label: "已取消", tone: "neutral" },
  skipped: { label: "已跳过", tone: "neutral" },

  // 工作流
  published: { label: "已发布", tone: "mint" },
  draft: { label: "草稿", tone: "neutral" },

  // 文档 / 知识库
  indexed: { label: "已索引", tone: "mint" },
  indexing: { label: "索引中", tone: "sky", pulse: true },

  // Provider
  enabled: { label: "启用", tone: "mint" },
  disabled: { label: "停用", tone: "neutral" },
  degraded: { label: "降级", tone: "amber" },
  unhealthy: { label: "异常", tone: "rose" },
};

export function statusMeta(status: string): StatusMeta {
  return STATUS[status] ?? { label: status, tone: "neutral" };
}

/** 状态徽章：圆点 + 文案。 */
export function StatusBadge({ status, className }: { status: string; className?: string }) {
  const meta = statusMeta(status);
  return (
    <Badge tone={meta.tone} className={className}>
      <StatusDot tone={meta.tone} pulse={meta.pulse} />
      {meta.label}
    </Badge>
  );
}

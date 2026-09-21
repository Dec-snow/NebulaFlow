import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/* ------------------------------------------------------------------ 空状态 */
export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
}: {
  icon?: ReactNode;
  title: string;
  description?: string;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center rounded-xl border border-dashed border-line",
        "bg-surface/40 px-6 py-14 text-center",
        className,
      )}
    >
      {icon && (
        <div className="mb-3.5 flex h-11 w-11 items-center justify-center rounded-xl bg-brand-soft text-brand">
          {icon}
        </div>
      )}
      <h3 className="text-sm font-semibold text-fg">{title}</h3>
      {description && (
        <p className="mt-1.5 max-w-sm text-xs leading-relaxed text-fg-subtle">
          {description}
        </p>
      )}
      {action && <div className="mt-5">{action}</div>}
    </div>
  );
}

/* ------------------------------------------------------------------ 进度条 */
export function Progress({
  value,
  tone = "brand",
  className,
  height = 6,
}: {
  /** 0–1 */
  value: number;
  tone?: "brand" | "mint" | "sky" | "amber" | "rose";
  className?: string;
  height?: number;
}) {
  const clamped = Math.max(0, Math.min(1, value));
  const map: Record<string, string> = {
    brand: "bg-brand",
    mint: "bg-mint",
    sky: "bg-sky",
    amber: "bg-amber",
    rose: "bg-rose",
  };
  return (
    <div
      className={cn("w-full overflow-hidden rounded-full bg-surface-2", className)}
      style={{ height }}
      role="progressbar"
      aria-valuenow={Math.round(clamped * 100)}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div
        className={cn("h-full rounded-full transition-[width] duration-500 ease-smooth", map[tone])}
        style={{ width: `${clamped * 100}%` }}
      />
    </div>
  );
}

/* ------------------------------------------------------------------ 骨架屏 */
export function Skeleton({ className }: { className?: string }) {
  return <div className={cn("skeleton", className)} />;
}

/** 卡片骨架：用于列表加载态。 */
export function CardSkeleton({ rows = 3 }: { rows?: number }) {
  return (
    <div className="card card-pad space-y-3">
      <Skeleton className="h-4 w-1/3" />
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className="h-3 w-full" />
      ))}
    </div>
  );
}

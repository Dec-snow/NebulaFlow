import type { HTMLAttributes, ReactNode } from "react";
import { cn } from "@/lib/utils";

export interface CardProps extends HTMLAttributes<HTMLDivElement> {
  /** 悬浮时轻微上浮 + 阴影加深 */
  hover?: boolean;
  /** 内边距（默认 p-5） */
  pad?: boolean;
}

export function Card({ hover, pad = true, className, ...rest }: CardProps) {
  return (
    <div
      className={cn(hover ? "card-hover" : "card", pad && "card-pad", className)}
      {...rest}
    />
  );
}

export interface CardHeaderProps {
  title: ReactNode;
  /** 标题下方的说明文字 */
  subtitle?: ReactNode;
  /** 右侧操作区 */
  action?: ReactNode;
  className?: string;
}

/** 卡片头部：标题 + 说明 + 右侧操作，自带下分隔线。 */
export function CardHeader({ title, subtitle, action, className }: CardHeaderProps) {
  return (
    <div
      className={cn(
        "mb-4 flex items-start justify-between gap-4 border-b border-line pb-3.5",
        className,
      )}
    >
      <div className="min-w-0">
        <h3 className="truncate text-sm font-semibold text-fg">{title}</h3>
        {subtitle && <p className="mt-0.5 text-xs text-fg-subtle">{subtitle}</p>}
      </div>
      {action && <div className="shrink-0">{action}</div>}
    </div>
  );
}

/** 区块标题（页面内的大分组）。 */
export function SectionTitle({
  title,
  subtitle,
  action,
  className,
}: CardHeaderProps) {
  return (
    <div className={cn("mb-4 flex items-end justify-between gap-4", className)}>
      <div>
        <h2 className="text-base font-semibold text-fg">{title}</h2>
        {subtitle && <p className="mt-0.5 text-xs text-fg-subtle">{subtitle}</p>}
      </div>
      {action}
    </div>
  );
}

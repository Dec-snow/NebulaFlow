import { cn } from "@/lib/utils";

/** 品牌标记：渐变圆角方块 + DAG 扇出字形（点 + 连线）。 */
export function BrandMark({
  size = 30,
  className,
}: {
  size?: number;
  className?: string;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 32 32"
      className={cn("shrink-0", className)}
      aria-hidden
    >
      <defs>
        <linearGradient id="nf-brand-grad" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" stopColor="rgb(var(--c-brand))" />
          <stop offset="100%" stopColor="rgb(var(--c-sky))" />
        </linearGradient>
      </defs>
      <rect width="32" height="32" rx="9" fill="url(#nf-brand-grad)" />
      <g
        stroke="#fff"
        strokeWidth="1.6"
        strokeLinecap="round"
        fill="none"
        opacity="0.92"
      >
        <path d="M10.5 16h5" />
        <path d="M15.5 16 21.5 10.5" />
        <path d="M15.5 16 21.5 21.5" />
      </g>
      <g fill="#fff">
        <circle cx="10.5" cy="16" r="2.2" />
        <circle cx="15.5" cy="16" r="2.2" />
        <circle cx="21.5" cy="10.5" r="2.2" />
        <circle cx="21.5" cy="21.5" r="2.2" />
      </g>
    </svg>
  );
}

/** 品牌字标 + 副标题。 */
export function BrandWordmark({ collapsed }: { collapsed?: boolean }) {
  if (collapsed) return null;
  return (
    <div className="min-w-0 leading-tight">
      <div className="truncate text-sm font-semibold tracking-tight text-fg">
        Nebula<span className="text-gradient">Flow</span>
      </div>
      <div className="truncate text-2xs text-fg-subtle">AI Agent Platform</div>
    </div>
  );
}

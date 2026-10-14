import { useId, useState, type MouseEvent } from "react";
import { cn, numberOrNull } from "@/lib/utils";

type Tone = "brand" | "mint" | "sky" | "violet" | "amber";

const TONE_VAR: Record<Tone, string> = {
  brand: "--c-brand",
  mint: "--c-mint",
  sky: "--c-sky",
  violet: "--c-violet",
  amber: "--c-amber",
};

const rgb = (tone: Tone, alpha = 1) =>
  `rgb(var(${TONE_VAR[tone]})${alpha === 1 ? "" : ` / ${alpha}`})`;

/** 把数值序列映射到 SVG 坐标点。 */
function toPoints(values: number[], w: number, h: number, padY: number) {
  if (!Array.isArray(values) || values.length === 0) return [];
  const max = Math.max(...values);
  const min = Math.min(...values);
  const span = max - min || 1;
  const stepX = values.length > 1 ? w / (values.length - 1) : w;
  return values.map((v, i) => {
    const x = i * stepX;
    const y = h - padY - ((v - min) / span) * (h - padY * 2);
    return [x, y] as const;
  });
}

/** 用水平控制点的三次贝塞尔做平滑，避免样条过冲。 */
function smoothPath(pts: ReadonlyArray<readonly [number, number]>): string {
  if (pts.length === 0) return "";
  let d = `M ${pts[0][0]} ${pts[0][1]}`;
  for (let i = 1; i < pts.length; i++) {
    const [x0, y0] = pts[i - 1];
    const [x1, y1] = pts[i];
    const mx = (x0 + x1) / 2;
    d += ` C ${mx} ${y0}, ${mx} ${y1}, ${x1} ${y1}`;
  }
  return d;
}

/* ------------------------------------------------------------------ 迷你趋势 */
export function Sparkline({
  data,
  tone = "brand",
  className,
}: {
  data: number[];
  tone?: Tone;
  className?: string;
}) {
  const id = useId();
  const W = 100;
  const H = 28;
  // 趋势数组来自接口，可能整个缺失：空数组时渲染空图，不要 Math.max(...[]) → -Infinity
  const values = Array.isArray(data) ? data : [];
  const pts = toPoints(values, W, H, 4);
  const line = smoothPath(pts);
  // 空序列时不要拼出 " L 100 28 L 0 28 Z" 这种非法 path：浏览器会记一条
  // "Expected moveto path command" 控制台错误。没有点时直接不渲染 path。
  const area = line ? `${line} L ${W} ${H} L 0 ${H} Z` : "";

  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
      className={cn("h-7 w-full", className)}
      aria-hidden
    >
      <defs>
        <linearGradient id={`sp-${id}`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={rgb(tone, 0.22)} />
          <stop offset="100%" stopColor={rgb(tone, 0)} />
        </linearGradient>
      </defs>
      {area && <path d={area} fill={`url(#sp-${id})`} />}
      {line && (
        <path
          d={line}
          fill="none"
          stroke={rgb(tone)}
          strokeWidth={1.75}
          strokeLinecap="round"
          vectorEffect="non-scaling-stroke"
        />
      )}
    </svg>
  );
}

/* -------------------------------------------------------------------- 面积图 */
export function AreaChart({
  data,
  labels,
  tone = "brand",
  height = 176,
  className,
}: {
  data: number[];
  labels?: string[];
  tone?: Tone;
  height?: number;
  className?: string;
}) {
  const id = useId();
  const [hover, setHover] = useState<number | null>(null);

  const W = 600;
  const H = 180;
  const padY = 16;
  // 接口未下发吞吐/成本序列时退化成空图，避免 Math.max(...undefined) 抛错与 "-Infinity" 刻度
  const values = Array.isArray(data) ? data : [];
  const labelsList = Array.isArray(labels) ? labels : [];
  const pts = toPoints(values, W, H, padY);
  const line = smoothPath(pts);
  // 同 Sparkline：空序列不渲染非法 path（否则浏览器控制台会出现 path 解析错误）
  const area = line ? `${line} L ${W} ${H} L 0 ${H} Z` : "";
  const max = values.length > 0 ? Math.max(...values) : null;

  const onMove = (e: MouseEvent<SVGSVGElement>) => {
    if (values.length === 0) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const ratio = (e.clientX - rect.left) / rect.width;
    const i = Math.round(ratio * (values.length - 1));
    setHover(Math.max(0, Math.min(values.length - 1, i)));
  };

  const hp = hover != null ? pts[hover] : null;

  return (
    <div className={cn("relative", className)} style={{ height }}>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        className="h-full w-full"
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
        role="img"
        aria-label="趋势图"
      >
        <defs>
          <linearGradient id={`ar-${id}`} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={rgb(tone, 0.24)} />
            <stop offset="100%" stopColor={rgb(tone, 0.01)} />
          </linearGradient>
        </defs>

        {/* 横向网格 */}
        {[0, 1, 2, 3].map((i) => (
          <line
            key={i}
            x1={0}
            x2={W}
            y1={padY + ((H - padY * 2) / 3) * i}
            y2={padY + ((H - padY * 2) / 3) * i}
            stroke="rgb(var(--c-line))"
            strokeWidth={1}
            strokeDasharray="4 6"
            vectorEffect="non-scaling-stroke"
          />
        ))}

        {area && <path d={area} fill={`url(#ar-${id})`} />}
        {line && (
          <path
            d={line}
            fill="none"
            stroke={rgb(tone)}
            strokeWidth={2}
            strokeLinecap="round"
            vectorEffect="non-scaling-stroke"
          />
        )}

        {/* 悬停竖向指示线 */}
        {hp && (
          <line
            x1={hp[0]}
            x2={hp[0]}
            y1={0}
            y2={H}
            stroke={rgb(tone, 0.45)}
            strokeWidth={1}
            strokeDasharray="3 4"
            vectorEffect="non-scaling-stroke"
          />
        )}
        {hp && <circle cx={hp[0]} cy={hp[1]} r={4} fill={rgb(tone)} />}
        {hp && (
          <circle
            cx={hp[0]}
            cy={hp[1]}
            r={8}
            fill="none"
            stroke={rgb(tone, 0.3)}
            strokeWidth={1.5}
            vectorEffect="non-scaling-stroke"
          />
        )}
      </svg>

      {/* 悬停数值气泡（用 HTML 定位，避免 SVG 缩放导致文字变形） */}
      {hover != null && hp && (
        <div
          className="pointer-events-none absolute -translate-x-1/2 -translate-y-full rounded-lg border border-line bg-surface px-2.5 py-1.5 shadow-md"
          style={{
            left: `${(hp[0] / W) * 100}%`,
            top: `${(hp[1] / H) * 100}%`,
            marginTop: -10,
          }}
        >
          <div className="tnum text-sm font-semibold text-fg">{values[hover]}</div>
          {labelsList[hover] && (
            <div className="text-2xs text-fg-subtle">{labelsList[hover]}</div>
          )}
        </div>
      )}

      {/* 顶部刻度 + 底部标签 */}
      <div className="pointer-events-none absolute right-0 top-0 text-2xs text-fg-subtle tnum">
        {max ?? "—"}
      </div>
      {labelsList.length > 0 && (
        <div className="mt-2 flex justify-between text-2xs text-fg-subtle">
          <span>{labelsList[0]}</span>
          <span>{labelsList[labelsList.length - 1]}</span>
        </div>
      )}
    </div>
  );
}

/* -------------------------------------------------------------------- 环形图 */
export function RingProgress({
  value,
  size = 108,
  stroke = 10,
  tone = "brand",
  label,
  sublabel,
}: {
  /** 0–1 */
  value: number;
  size?: number;
  stroke?: number;
  tone?: Tone;
  label?: string;
  sublabel?: string;
}) {
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  // utilization 缺失时兜底为 0，否则 clamped 变 NaN、label 渲染成 "NaN%"
  const clamped = Math.max(0, Math.min(1, numberOrNull(value) ?? 0));

  return (
    <div className="relative inline-flex items-center justify-center" style={{ width: size, height: size }}>
      <svg width={size} height={size} className="-rotate-90">
        <circle
          cx={size / 2}
          cy={size / 2}
          r={r}
          fill="none"
          stroke="rgb(var(--c-line))"
          strokeWidth={stroke}
        />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={r}
          fill="none"
          stroke={rgb(tone)}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={c}
          strokeDashoffset={c * (1 - clamped)}
          style={{ transition: "stroke-dashoffset 0.6s cubic-bezier(0.32,0.72,0,1)" }}
        />
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        <span className="tnum text-xl font-semibold text-fg">
          {label ?? `${Math.round(clamped * 100)}%`}
        </span>
        {sublabel && <span className="text-2xs text-fg-subtle">{sublabel}</span>}
      </div>
    </div>
  );
}

/* -------------------------------------------------------------------- 柱状图 */
export function BarSeries({
  items,
  className,
}: {
  items: { label: string; value: number; tone?: Tone }[];
  className?: string;
}) {
  // items 来自接口（token 分布 / 成本分布），可能缺失或数值字段为空
  const list = Array.isArray(items) ? items : [];
  const max = Math.max(...list.map((i) => numberOrNull(i.value) ?? 0), 1);
  return (
    <div className={cn("space-y-3", className)}>
      {list.map((it) => (
        <div key={it.label} className="flex items-center gap-3">
          <span className="w-24 shrink-0 truncate text-xs text-fg-muted">{it.label}</span>
          <div className="h-2 flex-1 overflow-hidden rounded-full bg-surface-2">
            <div
              className="h-full rounded-full transition-[width] duration-700 ease-smooth"
              style={{
                width: `${((numberOrNull(it.value) ?? 0) / max) * 100}%`,
                background: rgb(it.tone ?? "brand"),
              }}
            />
          </div>
          <span className="tnum w-14 shrink-0 text-right text-xs text-fg">
            {numberOrNull(it.value)?.toLocaleString() ?? "—"}
          </span>
        </div>
      ))}
    </div>
  );
}

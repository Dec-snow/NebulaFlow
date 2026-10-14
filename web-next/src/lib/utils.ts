/** 轻量 className 组合器（避免为此引入 clsx 依赖）。 */
export function cn(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(" ");
}

/** 从字符串派生一个稳定的 0–359 色相，用于头像/标签的确定性配色。 */
export function hueOf(seed: string): number {
  let h = 0;
  for (let i = 0; i < seed.length; i++) {
    h = (h * 31 + seed.charCodeAt(i)) % 360;
  }
  return h;
}

/**
 * 大小写无关的包含判断（过滤 / 搜索统一走这里）。
 *
 * 后端的字符串字段可能缺失：`omitempty` 未填、老数据为 NULL、
 * 或者字段名与前端约定不一致。直接 `w.name.toLowerCase()` 会抛
 * TypeError（"Cannot read properties of undefined"），渲染期异常会让
 * 整页被 ErrorBoundary 兜住或白屏。所以这里把 haystack 收成 string，
 * needle 为空视为"不过滤"。
 */
export function includesFold(
  haystack: unknown,
  needle: string | null | undefined,
): boolean {
  if (!needle) return true;
  const h =
    typeof haystack === "string" ? haystack : haystack == null ? "" : String(haystack);
  return h.toLowerCase().includes(needle.toLowerCase());
}

/**
 * 数值型统计字段的安全读取。
 *
 * 列表接口不一定下发统计字段（后端的 workflow 列表是 `nodes,omitempty`，
 * 且没有 nodeCount / usedCount 这类字段），此时渲染 `{undefined} 节点`
 * 或直接 `.toLocaleString()` 都会出问题。返回 null 让 UI 自己决定
 * 是隐藏还是显示占位。
 */
export function numberOrNull(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/** 数字千分位；大数自动收敛为 1.2k / 3.4M。 */
export function compact(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return (n / 1000).toFixed(n < 10_000 ? 1 : 0) + "k";
  return (n / 1_000_000).toFixed(1) + "M";
}

/**
 * 毫秒 → 人类可读（842ms / 1.2s / 2m18s）。
 *
 * 后端 `duration_ms` 带 `omitempty`，耗时字段可能整个缺失（老数据、节点未跑完），
 * 直接参与运算会得到 "NaNmNaNs"。非有限值统一降级为占位符。
 */
export function fmtDuration(ms: unknown): string {
  const n = numberOrNull(ms);
  if (n === null) return "—";
  if (n < 1000) return `${Math.round(n)}ms`;
  if (n < 60_000) return `${(n / 1000).toFixed(n < 10_000 ? 2 : 1)}s`;
  const m = Math.floor(n / 60_000);
  const s = Math.round((n % 60_000) / 1000);
  return `${m}m${String(s).padStart(2, "0")}s`;
}

/** 相对时间：刚刚 / 3 分钟前 / 2 小时前 / 3 天前。字段缺失/非法时返回占位符。 */
export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return "—";
  const diff = Date.now() - t;
  const min = Math.floor(diff / 60_000);
  if (min < 1) return "刚刚";
  if (min < 60) return `${min} 分钟前`;
  const h = Math.floor(min / 60);
  if (h < 24) return `${h} 小时前`;
  return `${Math.floor(h / 24)} 天前`;
}

/**
 * 时钟格式 HH:MM:SS，用于日志与事件流。
 *
 * 时间戳缺失（`started_at` 也是 omitempty 的指针字段）时会得到 Invalid Date，
 * 原样格式化会渲染出 "NaN:NaN:NaN"，所以这里与 timeAgo 保持一致：降级为占位符。
 */
export function clockOf(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((x) => String(x).padStart(2, "0"))
    .join(":");
}

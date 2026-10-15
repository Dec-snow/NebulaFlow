/**
 * snake_case → camelCase 的响应键名转换。
 *
 * 后端（Go）的 json tag 一律 snake_case；前端组件统一用 camelCase，
 * 转换只在这一层做一次，组件与类型定义都不需要感知命名差异。
 *
 * 注意：`NodeConfig.Extra`（map[string]any）里的用户数据也会被一并转换，
 * 这是已知取舍 —— 换取的是全站一套命名，而不是每个字段手写映射。
 */
export function camelize(str: string): string {
  return str.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());
}

/** 递归转换对象的所有 snake_case 键为 camelCase。数组逐项处理。 */
export function camelizeKeys<T = unknown>(obj: unknown): T {
  if (Array.isArray(obj)) {
    return obj.map((item) => camelizeKeys(item)) as unknown as T;
  }
  if (obj !== null && typeof obj === "object") {
    const result: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(obj as Record<string, unknown>)) {
      result[camelize(key)] = camelizeKeys(value);
    }
    return result as T;
  }
  return obj as T;
}

/** 安全数组：后端 `omitempty` 字段缺失时返回 []，避免调用方 .map 抛异常。 */
export function asArray<T = unknown>(v: unknown): T[] {
  return Array.isArray(v) ? (v as T[]) : [];
}

/** 安全数字：非有限数返回 null，让 UI 决定隐藏还是显示占位符。 */
export function asNumber(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/** 安全字符串：缺失/类型不符时返回空串。 */
export function asString(v: unknown): string {
  return typeof v === "string" ? v : "";
}

/** 安全布尔。 */
export function asBool(v: unknown): boolean {
  return v === true;
}

/**
 * 两个 ISO 时间戳之差（毫秒）。
 *
 * 后端 `model.Task` 没有 duration_ms：只有 started_at / finished_at，
 * 未跑完的任务（指针字段为 nil → JSON 里整个缺失）返回 null。
 */
export function durationBetween(start?: unknown, end?: unknown): number | null {
  if (typeof start !== "string" || typeof end !== "string") return null;
  const a = new Date(start).getTime();
  const b = new Date(end).getTime();
  if (!Number.isFinite(a) || !Number.isFinite(b) || b < a) return null;
  return b - a;
}

/** 拼接 baseURL + path（baseURL 为空时即同源相对路径）。 */
export function joinUrl(base: string, path: string): string {
  if (/^https?:\/\//.test(path)) return path;
  const b = base.replace(/\/$/, "");
  const p = path.replace(/^\//, "");
  return `${b}/${p}`;
}

/** 构造查询串，跳过 undefined / null / 空串。 */
export function query(params: Record<string, string | number | undefined | null>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "") continue;
    sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : "";
}

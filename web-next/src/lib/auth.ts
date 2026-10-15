/**
 * 认证凭据持久化 + 全局 401 通知。
 *
 * 键名沿用现有 `nf-*` 风格：
 *   - `nf-token`   JWT（API 客户端从这里取，注入 Authorization: Bearer）
 *   - `nf-user`    用户名（顶栏/侧栏展示）
 *   - `nf-user-id` 用户 ID（后端 /api/auth/* 返回的 user_id）
 *
 * 单独抽成一个模块（而不是塞进 zustand store）是为了让 API 客户端在
 * 不依赖 React / store 的情况下也能读到 token —— 否则 client.ts 与
 * session.ts 之间会形成循环依赖。
 */

export const TOKEN_KEY = "nf-token";
export const USER_KEY = "nf-user";
export const USER_ID_KEY = "nf-user-id";

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function write(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* 隐私模式下 localStorage 可能抛异常：内存态仍然可用 */
  }
}

function remove(key: string): void {
  try {
    localStorage.removeItem(key);
  } catch {
    /* 忽略 */
  }
}

/** 当前 token；未登录返回 null。 */
export function getToken(): string | null {
  const t = read(TOKEN_KEY);
  return t && t.length > 0 ? t : null;
}

export function getStoredUsername(): string {
  return read(USER_KEY) ?? "";
}

export function getStoredUserId(): number | null {
  const raw = read(USER_ID_KEY);
  if (!raw) return null;
  const n = Number(raw);
  return Number.isFinite(n) ? n : null;
}

/** 登入成功后写入凭据。 */
export function saveCredentials(token: string, username: string, userId: number | null): void {
  write(TOKEN_KEY, token);
  write(USER_KEY, username);
  if (userId !== null) write(USER_ID_KEY, String(userId));
  else remove(USER_ID_KEY);
}

/** 退出登录 / 收到 401 时清理本地凭据。 */
export function clearCredentials(): void {
  remove(TOKEN_KEY);
  remove(USER_KEY);
  remove(USER_ID_KEY);
}

/* ------------------------------------------------------------------ 401 */

type UnauthorizedHandler = () => void;

let handler: UnauthorizedHandler | null = null;
let lastNotifiedAt = 0;

/**
 * 注册 401 处理函数（由 App 在路由树内注册，这样能拿到 navigate）。
 *
 * 并发请求同时 401 是常态（一个页面会并行发好几个请求），所以这里做了
 * 去抖：1.5 秒内只通知一次，避免同一轮里反复跳转登录页。
 */
export function setUnauthorizedHandler(next: UnauthorizedHandler | null): void {
  handler = next;
}

export function notifyUnauthorized(): void {
  const now = Date.now();
  if (now - lastNotifiedAt < 1500) return;
  lastNotifiedAt = now;
  clearCredentials();
  handler?.();
}

import { create } from "zustand";
import { api } from "@/api";
import {
  clearCredentials,
  getStoredUserId,
  getStoredUsername,
  getToken,
  saveCredentials,
} from "@/lib/auth";

/**
 * 会话状态。
 *
 * 凭据（JWT）落在 localStorage 的 `nf-token`，用户名 / 用户 ID 分别是
 * `nf-user` / `nf-user-id`（与框架原有的 `nf-*` 风格一致）。
 * store 只维护内存态，真正的持久化在 `lib/auth.ts`，
 * 这样 API 客户端（非 React 上下文）也能读到 token。
 *
 * 安全性说明：JWT 放 localStorage 会被 XSS 读到，这是明确的取舍 ——
 * 后端用 Authorization 头（而不是 cookie），所以接不了 HttpOnly Cookie
 * 的防 XSS 优势；换来的是完全不引入 CSRF 面。演示/内网部署可接受。
 */
interface SessionState {
  username: string;
  userId: number | null;
  token: string | null;
  loggedIn: boolean;
  login: (username: string, password: string) => Promise<void>;
  register: (payload: { username: string; email: string; password: string }) => Promise<void>;
  logout: () => void;
  /** 凭据在别处被清理（例如 401）后同步内存态 */
  clearLocal: () => void;
}

function initialToken(): string | null {
  return getToken();
}

export const useSession = create<SessionState>((set) => ({
  username: getStoredUsername(),
  userId: getStoredUserId(),
  token: initialToken(),
  loggedIn: initialToken() !== null,

  login: async (username, password) => {
    const res = await api.auth.login({ username, password });
    saveCredentials(res.token, res.username, res.userId);
    set({
      username: res.username,
      userId: res.userId,
      token: res.token,
      loggedIn: res.token !== "",
    });
  },

  register: async (payload) => {
    const res = await api.auth.register(payload);
    saveCredentials(res.token, res.username, res.userId);
    set({
      username: res.username,
      userId: res.userId,
      token: res.token,
      loggedIn: res.token !== "",
    });
  },

  logout: () => {
    clearCredentials();
    set({ username: "", userId: null, token: null, loggedIn: false });
  },

  clearLocal: () => {
    clearCredentials();
    set({ username: "", userId: null, token: null, loggedIn: false });
  },
}));

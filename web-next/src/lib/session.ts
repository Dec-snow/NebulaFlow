import { create } from "zustand";

const KEY = "nf-user";

/**
 * 会话状态（框架层）。
 *
 * 当前是纯前端 mock：默认用户 demo，登录只是写入用户名。
 * 接入真实后端时，把 login() 换成调用 /api/auth/login、
 * 把 token 存进来、并在 api 客户端注入 Authorization 头即可，
 * 组件层无需改动。
 */
interface SessionState {
  username: string;
  loggedIn: boolean;
  login: (username: string) => void;
  logout: () => void;
}

function readUser(): string {
  try {
    return localStorage.getItem(KEY) || "demo";
  } catch {
    return "demo";
  }
}

export const useSession = create<SessionState>((set) => ({
  username: readUser(),
  loggedIn: true,

  login: (username) => {
    try {
      localStorage.setItem(KEY, username);
    } catch {
      /* 忽略 */
    }
    set({ username, loggedIn: true });
  },

  logout: () => {
    try {
      localStorage.removeItem(KEY);
    } catch {
      /* 忽略 */
    }
    set({ username: "", loggedIn: false });
  },
}));

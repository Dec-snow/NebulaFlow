import { create } from "zustand";

export type ThemeMode = "light" | "dark" | "system";

const STORAGE_KEY = "nf-theme";

function systemPrefersDark(): boolean {
  return (
    typeof window !== "undefined" &&
    window.matchMedia("(prefers-color-scheme: dark)").matches
  );
}

/** 把模式解析成"当前是否深色"。 */
export function resolveDark(mode: ThemeMode): boolean {
  if (mode === "dark") return true;
  if (mode === "light") return false;
  return systemPrefersDark();
}

function applyToDom(dark: boolean) {
  document.documentElement.classList.toggle("dark", dark);
}

function readStored(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {
    /* 隐私模式下 localStorage 可能抛错 */
  }
  return "system";
}

interface ThemeState {
  mode: ThemeMode;
  /** 当前实际渲染的深色态（system 模式下会跟随系统变化）。 */
  dark: boolean;
  setMode: (mode: ThemeMode) => void;
  /** 在浅色 / 深色之间直接切换（跳过 system，交互更可预期）。 */
  toggle: () => void;
}

export const useTheme = create<ThemeState>((set, get) => ({
  mode: readStored(),
  dark: resolveDark(readStored()),

  setMode: (mode) => {
    try {
      localStorage.setItem(STORAGE_KEY, mode);
    } catch {
      /* 忽略写入失败，本次会话仍生效 */
    }
    const dark = resolveDark(mode);
    applyToDom(dark);
    set({ mode, dark });
  },

  toggle: () => {
    get().setMode(resolveDark(get().mode) ? "light" : "dark");
  },
}));

// 首次挂载即同步一次 DOM（index.html 的脚本已处理首帧，这里保证一致性）
applyToDom(useTheme.getState().dark);

// system 模式下跟随操作系统主题变化
if (typeof window !== "undefined" && window.matchMedia) {
  window
    .matchMedia("(prefers-color-scheme: dark)")
    .addEventListener("change", () => {
      if (useTheme.getState().mode !== "system") return;
      const dark = systemPrefersDark();
      applyToDom(dark);
      useTheme.setState({ dark });
    });
}

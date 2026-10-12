import { create } from "zustand";
import { zhCN, type TranslationDict } from "./locales/zh-CN";
import { enUS } from "./locales/en-US";

export type Locale = "zh-CN" | "en-US";

const LOCALE_KEY = "nf-locale";

const DICTS: Record<Locale, TranslationDict> = {
  "zh-CN": zhCN,
  "en-US": enUS,
};

/* ============================================================
 *  类型工具：推导嵌套字典的所有键路径（例如 "nav.dashboard"）
 * ========================================================== */

type NestedKeys<T, Prefix extends string = ""> = T extends object
  ? {
      [K in keyof T & string]: T[K] extends string
        ? `${Prefix}${K}`
        : NestedKeys<T[K], `${Prefix}${K}.`>;
    }[keyof T & string]
  : never;

/** 所有合法的 i18n 键路径，由字典类型自动推导 */
export type I18nKey = NestedKeys<TranslationDict>;

/* ============================================================
 *  运行时：键路径解析
 * ========================================================== */

/**
 * 从嵌套字典中按键路径取值，例如 "nav.dashboard"。
 * 找不到时返回 key 本身，避免页面空白。
 */
function resolvePath(dict: TranslationDict, path: string): string {
  const parts = path.split(".");
  let cur: unknown = dict;
  for (const p of parts) {
    if (cur == null || typeof cur !== "object") return path;
    cur = (cur as Record<string, unknown>)[p];
  }
  return typeof cur === "string" ? cur : path;
}

function detectLocale(): Locale {
  try {
    const saved = localStorage.getItem(LOCALE_KEY) as Locale | null;
    if (saved && DICTS[saved]) return saved;
  } catch {
    /* 忽略 localStorage 读取失败 */
  }
  const lang = navigator.language;
  if (lang.startsWith("zh")) return "zh-CN";
  return "en-US";
}

/* ============================================================
 *  Zustand Store（替代原 Context 方案）
 * ========================================================== */

interface I18nState {
  locale: Locale;
  dict: TranslationDict;
  setLocale: (l: Locale) => void;
  t: (key: I18nKey) => string;
}

export const useI18n = create<I18nState>((set, get) => {
  const initialLocale = detectLocale();

  // 初始化时设置 html lang
  if (typeof document !== "undefined") {
    document.documentElement.lang = initialLocale;
  }

  return {
    locale: initialLocale,
    dict: DICTS[initialLocale],

    setLocale: (l: Locale) => {
      try {
        localStorage.setItem(LOCALE_KEY, l);
      } catch {
        /* 忽略 */
      }
      if (typeof document !== "undefined") {
        document.documentElement.lang = l;
      }
      set({ locale: l, dict: DICTS[l] });
    },

    t: (key: I18nKey) => {
      const { dict } = get();
      return resolvePath(dict, key);
    },
  };
});

/**
 * 非组件内使用的 t 函数（工具函数等场景）。
 * 注意：不会响应 locale 变化，只取当前值。
 */
export function t(key: I18nKey): string {
  return resolvePath(DICTS[useI18n.getState().locale], key);
}

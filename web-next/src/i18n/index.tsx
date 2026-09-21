import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { zhCN, type TranslationDict } from "./locales/zh-CN";
import { enUS } from "./locales/en-US";

export type Locale = "zh-CN" | "en-US";

const LOCALE_KEY = "nf-locale";

const DICTS: Record<Locale, TranslationDict> = {
  "zh-CN": zhCN,
  "en-US": enUS,
};

interface I18nContextValue {
  locale: Locale;
  setLocale: (l: Locale) => void;
  t: (key: string) => string;
  dict: TranslationDict;
}

const I18nContext = createContext<I18nContextValue | null>(null);

/**
 * 从嵌套字典中按键路径取值，例如 "nav.dashboard"。
 * 找不到时返回 key 本身，避免页面空白。
 */
function resolvePath(dict: TranslationDict, path: string): string {
  const parts = path.split(".");
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let cur: any = dict;
  for (const p of parts) {
    if (cur == null || typeof cur !== "object") return path;
    cur = cur[p];
  }
  return typeof cur === "string" ? cur : path;
}

function detectLocale(): Locale {
  const saved = localStorage.getItem(LOCALE_KEY) as Locale | null;
  if (saved && DICTS[saved]) return saved;
  const lang = navigator.language;
  if (lang.startsWith("zh")) return "zh-CN";
  return "en-US";
}

export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(() => detectLocale());

  const setLocale = useCallback((l: Locale) => {
    setLocaleState(l);
    localStorage.setItem(LOCALE_KEY, l);
    document.documentElement.lang = l;
  }, []);

  useEffect(() => {
    document.documentElement.lang = locale;
  }, [locale]);

  const t = useCallback(
    (key: string) => resolvePath(DICTS[locale], key),
    [locale],
  );

  const value = useMemo<I18nContextValue>(
    () => ({ locale, setLocale, t, dict: DICTS[locale] }),
    [locale, setLocale, t],
  );

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18nContextValue {
  const ctx = useContext(I18nContext);
  if (!ctx) throw new Error("useI18n must be used within I18nProvider");
  return ctx;
}

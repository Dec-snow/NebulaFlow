import { useCallback, useEffect, useRef, useState } from "react";

interface FetchState<T> {
  data: T | null;
  loading: boolean;
  error: Error | null;
}

/**
 * 通用数据获取 hook。
 *
 * - 自动处理 loading / error / data 状态
 * - AbortController 竞态处理：新请求发起时自动取消上一次请求，避免过期结果覆盖
 * - 组件卸载时取消请求并忽略结果，防止内存泄漏
 * - 支持手动刷新
 * - 依赖数组变化时自动重新获取
 *
 * @param fetcher 数据获取函数
 * @param deps 依赖数组，变化时重新获取
 */
export function useFetch<T>(
  fetcher: () => Promise<T>,
  deps: unknown[] = [],
): FetchState<T> & { refresh: () => void } {
  const [state, setState] = useState<FetchState<T>>({
    data: null,
    loading: true,
    error: null,
  });

  const abortControllerRef = useRef<AbortController | null>(null);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  const run = useCallback(() => {
    // 取消上一次请求（竞态处理）
    if (abortControllerRef.current) {
      abortControllerRef.current.abort();
    }

    // 创建新的 AbortController
    const controller = new AbortController();
    abortControllerRef.current = controller;
    const signal = controller.signal;

    setState((s) => ({ ...s, loading: true, error: null }));

    fetcherRef
      .current()
      .then((data) => {
        // 如果请求已被取消（新请求已发起或组件已卸载），忽略结果
        if (signal.aborted) return;
        setState({ data, loading: false, error: null });
      })
      .catch((error) => {
        // 如果请求已被取消，忽略错误
        if (signal.aborted) return;
        setState({ data: null, loading: false, error });
      });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => {
    run();
    return () => {
      // 组件卸载时取消当前请求
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
        abortControllerRef.current = null;
      }
    };
  }, [run]);

  return { ...state, refresh: run };
}

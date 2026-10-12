import { useEffect, useState } from "react";

/**
 * 防抖 hook：返回值会在 value 停止变化 delay 毫秒后更新。
 *
 * 常用于搜索输入框，避免快速输入产生大量无效请求。
 *
 * @param value 需要防抖的值
 * @param delay 延迟时间（毫秒），默认 300ms
 */
export function useDebounce<T>(value: T, delay = 300): T {
  const [debouncedValue, setDebouncedValue] = useState(value);

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDebouncedValue(value);
    }, delay);

    return () => {
      window.clearTimeout(timer);
    };
  }, [value, delay]);

  return debouncedValue;
}

/**
 * API 层的运行时开关。
 *
 * - `VITE_USE_MOCK === "true"` → 使用 `src/api/mock-api.ts` 的本地演示数据；
 * - 其它情况（含未设置）→ **真实 API**，这是默认行为。
 *
 * 这样"没配环境变量就打到真后端"是默认，mock 必须显式打开，
 * 避免"以为在联调、其实在看假数据"。
 */
export const USE_MOCK = import.meta.env.VITE_USE_MOCK === "true";

/** 后端基址；留空表示同源（生产形态下由 Go 进程托管 dist，走同一个端口）。 */
export const API_BASE_URL = import.meta.env.VITE_API_BASE_URL ?? "";

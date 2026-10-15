/**
 * API 客户端基类。
 *
 * 封装统一的 fetch 行为：
 * - baseURL（默认同源，即由 Go 进程托管 dist 的单端口形态）
 * - 超时控制
 * - token 注入：所有请求带 `Authorization: Bearer <token>`
 * - 401 统一处理：清理会话并跳转登录页（由 lib/auth 注册的 handler 完成）
 * - 统一错误处理（ApiError）
 *
 * SSE 不走这里：浏览器 EventSource 无法设置请求头，改用 fetch+reader
 * 的实现在 `src/api/sse.ts`，这里只导出它需要的 `authHeaders()`。
 */

import { getToken, notifyUnauthorized } from "@/lib/auth";
import { API_BASE_URL } from "./config";
import { camelizeKeys, joinUrl } from "./transform";

export interface ApiClientOptions {
  baseURL?: string;
  timeout?: number;
  /** 获取 token 的函数，默认从 localStorage（nf-token）读取 */
  getToken?: () => string | null;
}

export interface RequestOptions extends RequestInit {
  /** 单次请求超时时间（ms），覆盖默认值 */
  timeout?: number;
  /** 是否需要认证（自动注入 token） */
  auth?: boolean;
  /** 响应体不做 snake_case → camelCase 转换（默认会转） */
  raw?: boolean;
}

export class ApiError extends Error {
  status: number;
  data?: unknown;

  constructor(message: string, status: number, data?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.data = data;
  }
}

const DEFAULT_TIMEOUT = 15_000;

/**
 * 认证请求头。SSE 客户端需要自己拼 fetch 请求头，所以抽出来复用。
 */
export function authHeaders(extra?: Record<string, string>): Record<string, string> {
  const headers: Record<string, string> = { ...(extra ?? {}) };
  const token = getToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;
  return headers;
}

export class ApiClient {
  private baseURL: string;
  private timeout: number;
  private getToken?: () => string | null;

  constructor(options: ApiClientOptions = {}) {
    this.baseURL = options.baseURL ?? API_BASE_URL;
    this.timeout = options.timeout ?? DEFAULT_TIMEOUT;
    this.getToken = options.getToken ?? getToken;
  }

  /**
   * 发起请求。
   *
   * @template T 响应体类型
   * @param path 请求路径（相对 baseURL）
   * @param options 请求选项
   * @throws {ApiError} HTTP 错误或网络错误
   */
  async request<T = unknown>(path: string, options: RequestOptions = {}): Promise<T> {
    const {
      timeout = this.timeout,
      auth = true,
      headers: customHeaders,
      raw = false,
      ...rest
    } = options;

    const headers: Record<string, string> = {
      ...(customHeaders as Record<string, string>),
    };
    // FormData 由浏览器自己带 boundary，不要覆盖 Content-Type
    if (!(rest.body instanceof FormData) && !headers["Content-Type"]) {
      headers["Content-Type"] = "application/json";
    }

    if (auth && this.getToken) {
      const token = this.getToken();
      if (token) headers["Authorization"] = `Bearer ${token}`;
    }

    const url = joinUrl(this.baseURL, path);
    const controller = new AbortController();
    const timer = window.setTimeout(() => controller.abort(), timeout);

    try {
      const response = await fetch(url, { ...rest, headers, signal: controller.signal });

      if (response.status === 401) {
        // 会话失效：清 token 并跳登录页（handler 由 App 注册）
        notifyUnauthorized();
        throw new ApiError("登录状态已失效，请重新登录", 401);
      }

      const data = await this.parseResponse(response);

      if (!response.ok) {
        throw new ApiError(
          (data as { error?: string })?.error ?? response.statusText,
          response.status,
          data,
        );
      }

      return (raw ? data : camelizeKeys<T>(data)) as T;
    } catch (err) {
      if (err instanceof ApiError) throw err;
      if ((err as Error).name === "AbortError") {
        throw new ApiError("请求超时", 408);
      }
      throw new ApiError((err as Error).message || "网络错误", 0);
    } finally {
      window.clearTimeout(timer);
    }
  }

  private async parseResponse(response: Response): Promise<unknown> {
    if (response.status === 204) return null;
    const contentType = response.headers.get("content-type") ?? "";
    if (contentType.includes("application/json")) {
      try {
        return await response.json();
      } catch {
        return null;
      }
    }
    return response.text();
  }

  // 便捷方法
  get<T = unknown>(path: string, options?: Omit<RequestOptions, "method">) {
    return this.request<T>(path, { ...options, method: "GET" });
  }

  post<T = unknown>(path: string, body?: unknown, options?: Omit<RequestOptions, "method" | "body">) {
    return this.request<T>(path, {
      ...options,
      method: "POST",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  }

  put<T = unknown>(path: string, body?: unknown, options?: Omit<RequestOptions, "method" | "body">) {
    return this.request<T>(path, {
      ...options,
      method: "PUT",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  }

  delete<T = unknown>(path: string, options?: Omit<RequestOptions, "method">) {
    return this.request<T>(path, { ...options, method: "DELETE" });
  }
}

/** 默认客户端实例（单例）。baseURL 默认同源。 */
export const apiClient = new ApiClient();

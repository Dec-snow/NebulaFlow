/**
 * API 客户端基类。
 *
 * 封装统一的 fetch 行为：
 * - 超时控制
 * - token 注入预留（当前为 mock 阶段，接入真实后端时启用）
 * - 统一错误处理
 *
 * 设计为轻量封装，不引入 axios 等第三方依赖。
 * 接入真实后端时，把 api/index.ts 中的 mock 实现替换为调用本客户端即可。
 */

export interface ApiClientOptions {
  baseURL?: string;
  timeout?: number;
  /** 获取 token 的函数，接入真实后端时注入 */
  getToken?: () => string | null;
}

export interface RequestOptions extends RequestInit {
  /** 单次请求超时时间（ms），覆盖默认值 */
  timeout?: number;
  /** 是否需要认证（自动注入 token） */
  auth?: boolean;
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

export class ApiClient {
  private baseURL: string;
  private timeout: number;
  private getToken?: () => string | null;

  constructor(options: ApiClientOptions = {}) {
    this.baseURL = options.baseURL ?? import.meta.env.VITE_API_BASE_URL ?? "";
    this.timeout = options.timeout ?? DEFAULT_TIMEOUT;
    this.getToken = options.getToken;
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
      ...rest
    } = options;

    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      ...(customHeaders as Record<string, string>),
    };

    // token 注入预留：接入真实后端时启用
    if (auth && this.getToken) {
      const token = this.getToken();
      if (token) {
        headers["Authorization"] = `Bearer ${token}`;
      }
    }

    const url = this.buildURL(path);
    const controller = new AbortController();
    const timer = window.setTimeout(() => controller.abort(), timeout);

    try {
      const response = await fetch(url, {
        ...rest,
        headers,
        signal: controller.signal,
      });

      const data = await this.parseResponse(response);

      if (!response.ok) {
        throw new ApiError(
          (data as { error?: string })?.error ?? response.statusText,
          response.status,
          data,
        );
      }

      return data as T;
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

  private buildURL(path: string): string {
    if (/^https?:\/\//.test(path)) return path;
    const base = this.baseURL.replace(/\/$/, "");
    const p = path.replace(/^\//, "");
    return `${base}/${p}`;
  }

  private async parseResponse(response: Response): Promise<unknown> {
    const contentType = response.headers.get("content-type") ?? "";
    if (contentType.includes("application/json")) {
      return response.json();
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
      body: body ? JSON.stringify(body) : undefined,
    });
  }

  put<T = unknown>(path: string, body?: unknown, options?: Omit<RequestOptions, "method" | "body">) {
    return this.request<T>(path, {
      ...options,
      method: "PUT",
      body: body ? JSON.stringify(body) : undefined,
    });
  }

  delete<T = unknown>(path: string, options?: Omit<RequestOptions, "method">) {
    return this.request<T>(path, { ...options, method: "DELETE" });
  }
}

/**
 * 默认客户端实例（单例）。
 * 当前为 mock 阶段，baseURL 为空；接入真实后端时在 main.tsx 初始化。
 */
export const apiClient = new ApiClient();

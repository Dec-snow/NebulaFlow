/**
 * 任务实时事件流（SSE）客户端。
 *
 * 为什么不用 `EventSource`：`GET /api/tasks/:id/stream` 需要
 * `Authorization: Bearer <token>` 请求头，而 `EventSource` 构造函数
 * **无法设置自定义请求头**（这是规范限制，不是配置问题）。把它换成
 * cookie 认证又会把 CSRF 面打开。所以这里用 fetch + ReadableStream：
 *
 *   - `fetch(url, { headers, signal })` 自己带 token；
 *   - `response.body.getReader()` 逐块读，按 `\n\n` 切帧；
 *   - 解析 `event:` / `data:` 字段（多行 data 用 `\n` 拼接，`:` 开头是注释/心跳）。
 *
 * 连接治理：
 *   - 断线重连带**指数退避 + 抖动**（1s→2s→4s→8s→15s→30s 封顶），
 *     而不是每 3 秒无脑重连（那会把后端打成一个 429/连接数超限的放大器；
 *     后端 Hub 每用户并发 SSE 上限是 10，重连风暴会直接把用户挡在门外）；
 *   - 收到任务终态事件后**停止重连**（终态不会再有新事件，重连只是浪费连接）；
 *   - `close()` 会 abort 当前请求，组件卸载时必须调用。
 */

import { getToken, notifyUnauthorized } from "@/lib/auth";
import { API_BASE_URL } from "./config";
import { camelizeKeys, joinUrl } from "./transform";

export interface TaskStreamEvent {
  /** SSE 事件名：snapshot / task_running / node_completed / token / ... */
  event: string;
  /** 事件体（已 camelize）。`snapshot` 是完整 Task 对象，其余见 task.Hub.Event */
  data: Record<string, unknown>;
  /** 客户端收到该帧的时间（ISO），用于事件流行首列时间 */
  receivedAt: string;
}

export type TaskStreamState = "connecting" | "live" | "reconnecting" | "closed" | "failed";

export interface TaskStreamOptions {
  onEvent?: (ev: TaskStreamEvent) => void;
  onStateChange?: (state: TaskStreamState, detail?: string) => void;
}

/** 指数退避序列（毫秒），最后一项为封顶值。 */
const BACKOFF_MS = [1_000, 2_000, 4_000, 8_000, 15_000, 30_000];

/**
 * 收到终态事件后继续读流的宽限期（毫秒）。
 *
 * 为什么需要它：连接建立时服务端会先补发当前任务快照，再补发它保留的
 * 近期事件（`internal/task/hub.go` 的 replay 缓冲，最多 64 条、不含 token）。
 * 如果一看到快照里的终态就立刻 abort，**排在后面的 node_started /
 * node_completed 回放会被丢掉** —— 用户点开一个已经跑完的任务，事件流里
 * 只剩一行 snapshot，看不到它是怎么一步步跑完的。
 * 因此终态时先停止"重连"，留 1 秒把已经在管道里的帧读完，再断开。
 */
const DRAIN_MS = 1_000;

/** 任务终态事件：收到即停止重连。与 internal/task/hub.go 的事件常量对齐。 */
export const TERMINAL_EVENTS = new Set(["task_completed", "task_failed", "task_cancelled"]);

/** 任务终态状态（Event.task_status 或 snapshot.status）。 */
export const TERMINAL_STATUSES = new Set(["succeeded", "failed", "cancelled", "skipped"]);

export interface TaskStreamHandle {
  close: () => void;
}

/** 解析一个 SSE 帧（不含结尾空行）。返回 null 表示该帧没有 data。 */
function parseFrame(frame: string): { event: string; data: unknown } | null {
  let event = "message";
  const dataLines: string[] = [];

  for (const rawLine of frame.split("\n")) {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    if (line === "" || line.startsWith(":")) continue; // 空行 / 心跳注释
    const idx = line.indexOf(":");
    const field = idx === -1 ? line : line.slice(0, idx);
    let value = idx === -1 ? "" : line.slice(idx + 1);
    if (value.startsWith(" ")) value = value.slice(1);

    if (field === "event") event = value;
    else if (field === "data") dataLines.push(value);
  }

  if (dataLines.length === 0) return null;
  const text = dataLines.join("\n");
  let data: unknown = text;
  try {
    data = JSON.parse(text);
  } catch {
    /* 非 JSON 就按原始文本透传 */
  }
  return { event, data };
}

/**
 * 订阅某个任务的实时事件流。
 *
 * @param taskId 任务 ID
 * @param options 事件回调
 * @returns 句柄，组件卸载时调用 `close()`
 */
export function subscribeTaskStream(
  taskId: number,
  options: TaskStreamOptions = {},
): TaskStreamHandle {
  /** 组件卸载 / 主动 close() */
  let userClosed = false;
  /** 收到终态事件：停止重连（但先排空已到达的帧） */
  let sawTerminal = false;
  let attempt = 0;
  let controller: AbortController | null = null;
  let retryTimer: number | null = null;
  let drainTimer: number | null = null;
  let state: TaskStreamState | null = null;

  const setState = (next: TaskStreamState, detail?: string) => {
    if (state === next) return;
    state = next;
    options.onStateChange?.(next, detail);
  };

  const cleanup = () => {
    if (retryTimer !== null) {
      window.clearTimeout(retryTimer);
      retryTimer = null;
    }
    if (drainTimer !== null) {
      window.clearTimeout(drainTimer);
      drainTimer = null;
    }
    controller?.abort();
    controller = null;
  };

  /** 终态：停掉重连，给回放帧留一个宽限期后断开。 */
  const finishAfterDrain = (detail: string) => {
    if (sawTerminal) return;
    sawTerminal = true;
    setState("closed", detail);
    if (drainTimer !== null) window.clearTimeout(drainTimer);
    drainTimer = window.setTimeout(() => {
      drainTimer = null;
      controller?.abort();
      controller = null;
    }, DRAIN_MS);
  };

  const scheduleRetry = (detail: string) => {
    if (userClosed || sawTerminal) return;
    const base = BACKOFF_MS[Math.min(attempt, BACKOFF_MS.length - 1)];
    const delay = Math.round(base * (0.8 + Math.random() * 0.4)); // ±20% 抖动
    attempt += 1;
    setState("reconnecting", `${detail}；${delay}ms 后第 ${attempt} 次重连`);
    retryTimer = window.setTimeout(() => {
      retryTimer = null;
      void connect();
    }, delay);
  };

  const handleFrame = (frame: string) => {
    const parsed = parseFrame(frame);
    if (!parsed) return;
    const data = camelizeKeys<Record<string, unknown>>(parsed.data);
    options.onEvent?.({ event: parsed.event, data, receivedAt: new Date().toISOString() });

    const status = typeof data.status === "string" ? data.status : undefined;
    const taskStatus = typeof data.taskStatus === "string" ? data.taskStatus : undefined;
    if (
      TERMINAL_EVENTS.has(parsed.event) ||
      (status !== undefined && TERMINAL_STATUSES.has(status)) ||
      (taskStatus !== undefined && TERMINAL_STATUSES.has(taskStatus))
    ) {
      finishAfterDrain("任务已进入终态，事件流已停止重连");
    }
  };

  const connect = async (): Promise<void> => {
    if (userClosed || sawTerminal) return;
    controller = new AbortController();
    const url = joinUrl(API_BASE_URL, `/api/tasks/${taskId}/stream`);

    let response: Response;
    try {
      response = await fetch(url, {
        method: "GET",
        headers: {
          Accept: "text/event-stream",
          ...(getToken() ? { Authorization: `Bearer ${getToken()}` } : {}),
        },
        signal: controller.signal,
        cache: "no-store",
      });
    } catch (err) {
      if (!userClosed) scheduleRetry((err as Error)?.name === "AbortError" ? "连接已中断" : "网络错误");
      return;
    }

    if (userClosed) {
      response.body?.cancel().catch(() => undefined);
      return;
    }

    if (response.status === 401) {
      notifyUnauthorized();
      finishAfterDrain("登录状态已失效");
      return;
    }
    if (response.status === 404) {
      // 任务不存在 / 不属于当前用户 —— 重试没有意义
      finishAfterDrain("任务不存在或无权限访问");
      return;
    }
    if (!response.ok || !response.body) {
      scheduleRetry(`事件流连接失败（HTTP ${response.status}）`);
      return;
    }

    attempt = 0;
    setState("live");

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        let sep = buffer.indexOf("\n\n");
        while (sep >= 0) {
          const frame = buffer.slice(0, sep);
          buffer = buffer.slice(sep + 2);
          handleFrame(frame);
          if (userClosed) {
            await reader.cancel().catch(() => undefined);
            return;
          }
          sep = buffer.indexOf("\n\n");
        }
      }
    } catch (err) {
      if (userClosed || sawTerminal) return; // 主动 abort / 终态排空后断开，不算错误
      scheduleRetry((err as Error)?.name === "AbortError" ? "连接已中断" : "读取事件流失败");
      return;
    }

    if (userClosed || sawTerminal) return;
    scheduleRetry("事件流被服务端关闭");
  };

  setState("connecting");
  void connect();

  return {
    close: () => {
      if (userClosed) return;
      userClosed = true;
      cleanup();
      setState("closed", "已主动断开");
    },
  };
}

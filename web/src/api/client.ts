import { useAuth } from "../store/auth";

// 统一 API 客户端：自动附加 JWT，401 时登出。
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, options: RequestInit = {}): Promise<T> {
  const { token } = useAuth.getState();
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(options.headers as Record<string, string>),
  };
  if (token) headers.Authorization = `Bearer ${token}`;

  const res = await fetch(path, { ...options, headers });
  if (res.status === 401) {
    useAuth.getState().logout();
    throw new ApiError(401, "登录已过期，请重新登录");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      msg = body.error || msg;
    } catch {
      /* ignore */
    }
    throw new ApiError(res.status, msg);
  }
  return res.json() as Promise<T>;
}

export const api = {
  // auth
  login: (username: string, password: string) =>
    request<{ token: string; username: string; user_id: number }>("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username, password }),
    }),
  register: (username: string, email: string, password: string) =>
    request<{ token: string; username: string; user_id: number }>("/api/auth/register", {
      method: "POST",
      body: JSON.stringify({ username, email, password }),
    }),

  // workflows
  listWorkflows: () => request<{ workflows: Workflow[] }>("/api/workflows"),
  getWorkflow: (id: number) => request<Workflow>(`/api/workflows/${id}`),
  createWorkflow: (payload: unknown) =>
    request<Workflow>("/api/workflows", { method: "POST", body: JSON.stringify(payload) }),
  updateWorkflow: (id: number, payload: unknown) =>
    request<Workflow>(`/api/workflows/${id}`, { method: "PUT", body: JSON.stringify(payload) }),
  deleteWorkflow: (id: number) =>
    request<{ deleted: number }>(`/api/workflows/${id}`, { method: "DELETE" }),

  // tasks
  createTask: (workflowId: number, input: string) =>
    request<Task>("/api/tasks", { method: "POST", body: JSON.stringify({ workflow_id: workflowId, input }) }),
  listTasks: (limit = 20) => request<{ tasks: Task[] }>(`/api/tasks?limit=${limit}`),
  getTask: (id: number) => request<Task>(`/api/tasks/${id}`),
  cancelTask: (id: number) => request<{ cancelled: number }>(`/api/tasks/${id}/cancel`, { method: "POST" }),
  taskLogs: (id: number) => request<{ logs: unknown[] }>(`/api/tasks/${id}/logs`),

  // knowledge
  listKBs: () => request<{ knowledge_bases: KnowledgeBase[] }>("/api/knowledge-bases"),
  createKB: (name: string, description: string) =>
    request<KnowledgeBase>("/api/knowledge-bases", {
      method: "POST",
      body: JSON.stringify({ name, description }),
    }),
  deleteKB: (id: number) => request<{ deleted: number }>(`/api/knowledge-bases/${id}`, { method: "DELETE" }),
  listDocs: (kbId: number) => request<{ documents: Document[] }>(`/api/knowledge-bases/${kbId}/documents`),
  uploadDoc: (kbId: number, content: string, filename: string) =>
    request<Document>(`/api/knowledge-bases/${kbId}/documents?filename=${encodeURIComponent(filename)}`, {
      method: "POST",
      headers: { "Content-Type": "text/plain" },
      body: content,
    }),
  retrieve: (kbId: number, q: string, k = 5) =>
    request<{ hits: { content: string; score: number; document_id: number; chunk_index: number }[] }>(
      `/api/knowledge-bases/${kbId}/retrieve?q=${encodeURIComponent(q)}&k=${k}`
    ),

  // providers
  listProviders: () => request<{ providers: Provider[] }>("/api/providers"),

  // dashboard
  dashboardStats: () => request<DashboardStats>("/api/dashboard/stats"),
};

import type { Workflow, Task, KnowledgeBase, Document, Provider, DashboardStats } from "../types";

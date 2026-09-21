/// <reference types="vite/client" />

export type NodeType = "input" | "llm" | "rag" | "tool" | "output";

export interface NodeConfig {
  model?: string;
  prompt?: string;
  system?: string;
  tool?: string;
  knowledge_base_id?: number;
  max_retry?: number;
  timeout_sec?: number;
  extra?: Record<string, unknown>;
}

export interface WorkflowNode {
  id?: number;
  workflow_id?: number;
  key: string;
  type: NodeType;
  config?: NodeConfig;
  x?: number;
  y?: number;
  position_x?: number;
  position_y?: number;
}

export interface WorkflowEdge {
  id?: number;
  source: string;
  target: string;
}

export interface Workflow {
  id: number;
  user_id: number;
  name: string;
  description: string;
  status: string;
  created_at: string;
  updated_at: string;
  nodes?: WorkflowNode[];
  edges?: WorkflowEdge[];
}

export type TaskStatus = "pending" | "running" | "succeeded" | "failed" | "cancelled";
export type NodeStatus =
  | "pending"
  | "running"
  | "succeeded"
  | "failed"
  | "skipped"
  | "cancelled";

export interface TaskNode {
  id: number;
  task_id: number;
  node_id: number;
  node_key: string;
  node_type: NodeType;
  status: NodeStatus;
  input?: string;
  output?: string;
  error?: string;
  retries: number;
  tokens_in: number;
  tokens_out: number;
  duration_ms: number;
}

export interface Task {
  id: number;
  workflow_id: number;
  user_id: number;
  status: TaskStatus;
  input: string;
  output: string;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  nodes?: TaskNode[];
}

export interface SSEvent {
  type: string;
  task_id: number;
  task_status?: string;
  node_key?: string;
  node_type?: string;
  node_status?: string;
  content?: string;
  message?: string;
  provider?: string;
  duration_ms?: number;
  timestamp: string;
}

export interface KnowledgeBase {
  id: number;
  user_id: number;
  name: string;
  description: string;
  created_at: string;
}

export interface Document {
  id: number;
  knowledge_base_id: number;
  filename: string;
  status: string;
  created_at: string;
}

export interface Provider {
  id: number;
  name: string;
  base_url: string;
  api_key?: string;
  is_default: boolean;
  enabled: boolean;
  priority: number;
  models?: { id: number; name: string; max_tokens: number }[];
}

export interface DashboardStats {
  queue_length: number;
  active_workers: number;
  running_tasks: number;
  timestamp: string;
}

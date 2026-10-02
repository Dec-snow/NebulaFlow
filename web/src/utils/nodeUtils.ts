import type { NodeType } from "../types";

// ---------- 拓扑排序相关类型 ----------
export interface WorkflowEdgeLike {
  source_node: string;
  target_node: string;
}

export interface WorkflowNodeLike {
  node_key: string;
  node_type: NodeType;
}

// ---------- 上游节点计算（BFS） ----------
/**
 * 计算目标节点的所有上游节点 key（BFS 遍历）
 * @param edges 边列表
 * @param targetKey 目标节点 key
 * @returns 所有上游节点 key 的数组
 */
export function computeUpstreamNodes(
  edges: WorkflowEdgeLike[],
  targetKey: string
): string[] {
  const result: string[] = [];
  const visited = new Set<string>();
  const queue: string[] = [];

  // 找到所有直接指向目标节点的源节点
  edges.forEach((e) => {
    if (e.target_node === targetKey && !visited.has(e.source_node)) {
      queue.push(e.source_node);
      visited.add(e.source_node);
    }
  });

  // BFS 找所有上游
  while (queue.length > 0) {
    const cur = queue.shift()!;
    result.push(cur);
    edges.forEach((e) => {
      if (e.target_node === cur && !visited.has(e.source_node)) {
        queue.push(e.source_node);
        visited.add(e.source_node);
      }
    });
  }

  return result;
}

// ---------- 拓扑排序（Kahn 算法） ----------
/**
 * 对节点进行拓扑排序（Kahn 算法）
 * @param nodes 节点列表
 * @param edges 边列表
 * @returns 拓扑排序后的节点 key 数组
 */
export function topologicalSort(
  nodes: WorkflowNodeLike[],
  edges: WorkflowEdgeLike[]
): string[] {
  const inDegree = new Map<string, number>();
  const adj = new Map<string, string[]>();

  nodes.forEach((n) => {
    inDegree.set(n.node_key, 0);
    adj.set(n.node_key, []);
  });

  edges.forEach((e) => {
    if (inDegree.has(e.target_node)) {
      inDegree.set(e.target_node, (inDegree.get(e.target_node) || 0) + 1);
    }
    if (adj.has(e.source_node)) {
      adj.get(e.source_node)!.push(e.target_node);
    }
  });

  // Kahn 算法
  const queue: string[] = [];
  const order: string[] = [];
  inDegree.forEach((deg, key) => {
    if (deg === 0) queue.push(key);
  });

  while (queue.length > 0) {
    const cur = queue.shift()!;
    order.push(cur);
    adj.get(cur)?.forEach((next) => {
      const d = (inDegree.get(next) || 0) - 1;
      inDegree.set(next, d);
      if (d === 0) queue.push(next);
    });
  }

  return order;
}

// ---------- 格式化耗时 ----------
/**
 * 格式化毫秒耗时为可读字符串
 * @param ms 毫秒数
 * @returns 格式化后的字符串，如 "500ms"、"1.50s"、"2m 30s"
 */
export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(2)}s`;
  const sec = Math.floor(ms / 1000);
  const min = Math.floor(sec / 60);
  const s = sec % 60;
  return `${min}m ${s}s`;
}

// ---------- 节点名校验 ----------
export interface ValidationResult {
  valid: boolean;
  error?: string;
}

/**
 * 验证节点名合法性
 * - 不能为空或纯空格
 * - 不能与已有节点重名
 * - 长度不超过 50 字符
 * - 只能包含字母、数字、下划线、连字符
 * @param name 待验证的节点名
 * @param existingNames 已有节点名列表
 * @returns 验证结果
 */
export function validateNodeName(
  name: string,
  existingNames: string[]
): ValidationResult {
  const trimmed = name.trim();

  if (!trimmed) {
    return { valid: false, error: "节点名称不能为空" };
  }

  if (trimmed.length > 50) {
    return { valid: false, error: "节点名称不能超过 50 个字符" };
  }

  if (!/^[a-zA-Z0-9_-]+$/.test(trimmed)) {
    return { valid: false, error: "节点名称只能包含字母、数字、下划线和连字符" };
  }

  if (existingNames.includes(trimmed)) {
    return { valid: false, error: `节点名称 "${trimmed}" 已存在` };
  }

  return { valid: true };
}

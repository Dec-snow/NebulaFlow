import { useMemo, useState } from "react";
import {
  CheckCircle2,
  Circle,
  Loader2,
  XCircle,
  Ban,
  Clock,
  ChevronDown,
  ChevronRight,
  Copy,
  Check,
  Zap,
  AlertCircle,
  Hash,
} from "lucide-react";
import type { NodeType, TaskNode } from "../types";
import { nodeMeta } from "./FlowNodes";

const statusIconMap: Record<string, React.ElementType> = {
  pending: Circle,
  running: Loader2,
  succeeded: CheckCircle2,
  failed: XCircle,
  skipped: Circle,
  cancelled: Ban,
};

const statusColorMap: Record<string, { text: string; bg: string; bar: string; border: string }> = {
  pending: { text: "text-slate-500", bg: "bg-slate-500/10", bar: "bg-slate-600", border: "border-slate-600/30" },
  running: { text: "text-nebula-300", bg: "bg-nebula-400/10", bar: "bg-nebula-400", border: "border-nebula-400/30" },
  succeeded: { text: "text-emerald-400", bg: "bg-emerald-400/10", bar: "bg-emerald-400", border: "border-emerald-400/30" },
  failed: { text: "text-red-400", bg: "bg-red-400/10", bar: "bg-red-400", border: "border-red-400/30" },
  skipped: { text: "text-slate-600", bg: "bg-slate-600/10", bar: "bg-slate-700", border: "border-slate-600/30" },
  cancelled: { text: "text-amber-400", bg: "bg-amber-400/10", bar: "bg-amber-400", border: "border-amber-400/30" },
};

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(2)}s`;
  const sec = Math.floor(ms / 1000);
  const min = Math.floor(sec / 60);
  const s = sec % 60;
  return `${min}m ${s}s`;
}

function formatTokens(n: number): string {
  if (n < 1000) return `${n}`;
  if (n < 1000000) return `${(n / 1000).toFixed(1)}K`;
  return `${(n / 1000000).toFixed(2)}M`;
}

interface TimelineNode {
  key: string;
  type: NodeType;
  node?: TaskNode;
  status: string;
  startOffset: number; // 相对起始的偏移量（ms）
  duration: number;
  dependencies: string[]; // 上游节点 key
}

interface TaskTimelineProps {
  nodes: TaskNode[];
  workflowNodes: { node_key: string; node_type: NodeType }[];
  workflowEdges: { source_node: string; target_node: string }[];
  totalDuration?: number;
  taskStartedAt?: string;
  taskFinishedAt?: string;
}

export default function TaskTimeline({
  nodes,
  workflowNodes,
  workflowEdges,
  totalDuration,
  taskStartedAt,
  taskFinishedAt,
}: TaskTimelineProps) {
  const [expandedKey, setExpandedKey] = useState<string | null>(null);
  const [copiedKey, setCopiedKey] = useState<string | null>(null);

  // 构建节点映射
  const nodeMap = useMemo(() => {
    const m = new Map<string, TaskNode>();
    nodes.forEach((n) => m.set(n.node_key, n));
    return m;
  }, [nodes]);

  // 计算拓扑排序（基于工作流边）
  const timelineNodes = useMemo<TimelineNode[]>(() => {
    // 构建图
    const inDegree = new Map<string, number>();
    const adj = new Map<string, string[]>();

    workflowNodes.forEach((n) => {
      inDegree.set(n.node_key, 0);
      adj.set(n.node_key, []);
    });

    workflowEdges.forEach((e) => {
      if (inDegree.has(e.target_node)) {
        inDegree.set(e.target_node, (inDegree.get(e.target_node) || 0) + 1);
      }
      if (adj.has(e.source_node)) {
        adj.get(e.source_node)!.push(e.target_node);
      }
    });

    // Kahn 算法拓扑排序
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

    // 计算每个节点的开始时间和持续时间
    // 策略：节点在所有上游完成后才能开始（DAG 关键路径）
    const startTimes = new Map<string, number>();
    const durations = new Map<string, number>();

    order.forEach((key) => {
      const tn = nodeMap.get(key);
      const duration = tn?.duration_ms || 0;
      durations.set(key, duration);

      // 找所有上游
      const upstream = workflowEdges.filter((e) => e.target_node === key).map((e) => e.source_node);
      if (upstream.length === 0) {
        startTimes.set(key, 0);
      } else {
        const maxUpstreamEnd = Math.max(
          ...upstream.map((u) => (startTimes.get(u) || 0) + (durations.get(u) || 0)),
          0
        );
        startTimes.set(key, maxUpstreamEnd);
      }
    });

    // 计算总时长
    const maxEnd = Math.max(
      ...order.map((k) => (startTimes.get(k) || 0) + (durations.get(k) || 0)),
      1
    );

    // 转换为百分比偏移
    const result: TimelineNode[] = order.map((key) => {
      const wfNode = workflowNodes.find((n) => n.node_key === key);
      const tn = nodeMap.get(key);
      return {
        key,
        type: wfNode?.node_type || "llm",
        node: tn,
        status: tn?.status || "pending",
        startOffset: (startTimes.get(key) || 0) / maxEnd,
        duration: (durations.get(key) || 0) / maxEnd,
        dependencies: workflowEdges.filter((e) => e.target_node === key).map((e) => e.source_node),
      };
    });

    return result;
  }, [workflowNodes, workflowEdges, nodeMap]);

  // 实际总时长（用于显示）
  const actualTotalDuration = useMemo(() => {
    if (totalDuration !== undefined) return totalDuration;
    if (taskStartedAt && taskFinishedAt) {
      return new Date(taskFinishedAt).getTime() - new Date(taskStartedAt).getTime();
    }
    const maxEnd = Math.max(...timelineNodes.map((n) => (n.node?.duration_ms || 0) + 0), 0);
    return maxEnd;
  }, [totalDuration, taskStartedAt, taskFinishedAt, timelineNodes]);

  const copyToClipboard = (text: string, key: string) => {
    navigator.clipboard.writeText(text);
    setCopiedKey(key);
    setTimeout(() => setCopiedKey(null), 1500);
  };

  return (
    <div className="space-y-2">
      {/* 头部统计 */}
      <div className="flex items-center justify-between px-1 mb-3">
        <div className="flex items-center gap-3 text-[10px] text-slate-500">
          <span className="flex items-center gap-1">
            <Clock size={11} className="text-nebula-400" />
            总耗时：<span className="text-slate-300 font-medium">{formatDuration(actualTotalDuration)}</span>
          </span>
          <span className="flex items-center gap-1">
            <Zap size={11} className="text-emerald-400" />
            节点：<span className="text-slate-300 font-medium">{timelineNodes.length}</span>
          </span>
        </div>
        <div className="flex items-center gap-3 text-[9px]">
          {Object.entries(statusColorMap).slice(0, 4).map(([s, c]) => (
            <div key={s} className="flex items-center gap-1">
              <div className={`w-2 h-2 rounded-full ${c.bar}`} />
              <span className="text-slate-500 capitalize">{s}</span>
            </div>
          ))}
        </div>
      </div>

      {/* 时间轴 */}
      <div className="relative">
        {/* 时间刻度 */}
        <div className="flex justify-between text-[9px] text-slate-600 mb-2 px-0.5">
          <span>0s</span>
          <span>{(actualTotalDuration / 1000 / 4).toFixed(1)}s</span>
          <span>{(actualTotalDuration / 1000 / 2).toFixed(1)}s</span>
          <span>{(actualTotalDuration / 1000 * 3 / 4).toFixed(1)}s</span>
          <span>{(actualTotalDuration / 1000).toFixed(1)}s</span>
        </div>

        {/* 节点列表 */}
        <div className="space-y-1.5">
          {timelineNodes.map((tn) => {
            const meta = nodeMeta[tn.type] || nodeMeta.llm;
            const StatusIcon = statusIconMap[tn.status] || Circle;
            const colors = statusColorMap[tn.status] || statusColorMap.pending;
            const isExpanded = expandedKey === tn.key;
            const isRunning = tn.status === "running";
            const node = tn.node;

            return (
              <div key={tn.key} className="group">
                {/* 时间线条目 */}
                <div
                  className={`relative flex items-center gap-3 px-3 py-2 rounded-lg border transition-all cursor-pointer ${
                    isExpanded
                      ? "bg-nebula-800/50 border-nebula-600"
                      : "bg-nebula-900/50 border-nebula-800 hover:border-nebula-700"
                  }`}
                  onClick={() => setExpandedKey(isExpanded ? null : tn.key)}
                >
                  {/* 状态图标 */}
                  <div className={`shrink-0 ${colors.text}`}>
                    <StatusIcon size={14} className={isRunning ? "animate-spin" : ""} />
                  </div>

                  {/* 节点信息 */}
                  <div className="w-28 shrink-0 min-w-0">
                    <div className="text-xs text-slate-200 font-medium truncate" title={tn.key}>
                      {tn.key}
                    </div>
                    <div className="text-[9px] text-slate-500 flex items-center gap-1">
                      <span
                        className="w-1.5 h-1.5 rounded-full"
                        style={{ backgroundColor: meta.color }}
                      />
                      {meta.label}
                    </div>
                  </div>

                  {/* 进度条轨道 */}
                  <div className="flex-1 relative h-6">
                    <div className="absolute inset-y-1 inset-x-0 bg-nebula-950 rounded-full border border-nebula-800" />

                    {/* 进度条 */}
                    {tn.duration > 0 || tn.status === "running" ? (
                      <div
                        className={`absolute top-1 bottom-1 rounded-full ${colors.bar} transition-all duration-500 ${
                          isRunning ? "animate-pulse" : ""
                        }`}
                        style={{
                          left: `${tn.startOffset * 100}%`,
                          width: `${Math.max(tn.duration * 100, 2)}%`,
                          minWidth: "8px",
                        }}
                      >
                        {/* 渐变光晕效果 */}
                        <div className="absolute inset-0 rounded-full bg-gradient-to-r from-transparent via-white/20 to-transparent" />
                      </div>
                    ) : (
                      <div
                        className="absolute top-1/2 -translate-y-1/2 w-2 h-2 rounded-full bg-slate-700 border border-slate-600"
                        style={{ left: `${tn.startOffset * 100}%` }}
                      />
                    )}

                    {/* 依赖连接线（简化：从左侧指向进度条起点） */}
                    {tn.dependencies.length > 0 && (
                      <div
                        className="absolute -left-3 top-1/2 -translate-y-1/2 w-3 h-px bg-nebula-700"
                        style={{ left: `-${12 + tn.startOffset * 0}px` }}
                      />
                    )}
                  </div>

                  {/* 耗时 */}
                  <div className="w-16 text-right shrink-0">
                    <div className={`text-[11px] font-mono tabular-nums ${colors.text}`}>
                      {node?.duration_ms ? formatDuration(node.duration_ms) : tn.status === "pending" ? "—" : "0ms"}
                    </div>
                  </div>

                  {/* token 统计 */}
                  {node && (node.tokens_in > 0 || node.tokens_out > 0) && (
                    <div className="w-16 text-right shrink-0 hidden sm:block">
                      <div className="text-[9px] text-slate-500">
                        <span className="text-slate-400">{formatTokens(node.tokens_in)}</span>
                        <span className="text-slate-600"> → </span>
                        <span className="text-emerald-400">{formatTokens(node.tokens_out)}</span>
                      </div>
                    </div>
                  )}

                  {/* 展开箭头 */}
                  <div className="shrink-0 text-slate-500 group-hover:text-slate-400 transition-colors">
                    {isExpanded ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
                  </div>
                </div>

                {/* 展开详情 */}
                {isExpanded && node && (
                  <div className="ml-11 mt-1 mb-2 p-3 rounded-lg bg-nebula-950 border border-nebula-800 space-y-3">
                    {/* 状态 + 重试 */}
                    <div className="flex items-center gap-4 text-[11px]">
                      <span className={`flex items-center gap-1.5 ${colors.text}`}>
                        <StatusIcon size={12} className={isRunning ? "animate-spin" : ""} />
                        状态：{tn.status}
                      </span>
                      {node.retries > 0 && (
                        <span className="flex items-center gap-1.5 text-yellow-400">
                          <AlertCircle size={12} />
                          重试 {node.retries} 次
                        </span>
                      )}
                      {node.tokens_in > 0 && (
                        <span className="flex items-center gap-1.5 text-slate-400">
                          <Hash size={12} />
                          {formatTokens(node.tokens_in)} in / {formatTokens(node.tokens_out)} out
                        </span>
                      )}
                    </div>

                    {/* 输入 */}
                    {node.input && (
                      <div>
                        <div className="flex items-center justify-between mb-1">
                          <span className="text-[10px] text-slate-500 font-medium">输入</span>
                          <button
                            onClick={(e) => {
                              e.stopPropagation();
                              copyToClipboard(node.input!, `${tn.key}-input`);
                            }}
                            className="text-[9px] text-slate-500 hover:text-slate-300 flex items-center gap-1"
                          >
                            {copiedKey === `${tn.key}-input` ? (
                              <><Check size={10} className="text-emerald-400" /> 已复制</>
                            ) : (
                              <><Copy size={10} /> 复制</>
                            )}
                          </button>
                        </div>
                        <div className="text-[11px] text-slate-400 bg-nebula-900 rounded-md p-2 max-h-32 overflow-y-auto whitespace-pre-wrap border border-nebula-800">
                          {node.input}
                        </div>
                      </div>
                    )}

                    {/* 输出 */}
                    {node.output && (
                      <div>
                        <div className="flex items-center justify-between mb-1">
                          <span className="text-[10px] text-emerald-400/80 font-medium">输出</span>
                          <button
                            onClick={(e) => {
                              e.stopPropagation();
                              copyToClipboard(node.output!, `${tn.key}-output`);
                            }}
                            className="text-[9px] text-slate-500 hover:text-slate-300 flex items-center gap-1"
                          >
                            {copiedKey === `${tn.key}-output` ? (
                              <><Check size={10} className="text-emerald-400" /> 已复制</>
                            ) : (
                              <><Copy size={10} /> 复制</>
                            )}
                          </button>
                        </div>
                        <div className="text-[11px] text-slate-300 bg-nebula-900 rounded-md p-2 max-h-40 overflow-y-auto whitespace-pre-wrap border border-emerald-500/20">
                          {node.output}
                        </div>
                      </div>
                    )}

                    {/* 错误 */}
                    {node.error && (
                      <div>
                        <div className="flex items-center gap-1.5 mb-1">
                          <XCircle size={10} className="text-red-400" />
                          <span className="text-[10px] text-red-400 font-medium">错误</span>
                        </div>
                        <div className="text-[11px] text-red-300 bg-red-500/10 rounded-md p-2 max-h-32 overflow-y-auto whitespace-pre-wrap border border-red-500/20">
                          {node.error}
                        </div>
                      </div>
                    )}

                    {/* 依赖关系 */}
                    {tn.dependencies.length > 0 && (
                      <div className="pt-2 border-t border-nebula-800">
                        <div className="text-[10px] text-slate-500 mb-1.5">上游依赖</div>
                        <div className="flex flex-wrap gap-1.5">
                          {tn.dependencies.map((dep) => (
                            <span
                              key={dep}
                              className="px-2 py-0.5 rounded text-[10px] bg-nebula-800 text-slate-300 border border-nebula-700 font-mono"
                            >
                              {dep}
                            </span>
                          ))}
                        </div>
                      </div>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}

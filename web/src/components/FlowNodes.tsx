import { memo } from "react";
import { Handle, Position } from "@xyflow/react";
import { Bot, Calculator, Database, GitBranch, TerminalSquare, Boxes } from "lucide-react";
import type { NodeConfig, NodeType } from "../types";

// 节点外观配置
export const nodeMeta: Record<
  NodeType,
  { label: string; color: string; icon: React.ElementType; description: string }
> = {
  input: { label: "Input", color: "#8aa3ff", icon: Boxes, description: "任务输入" },
  llm: { label: "LLM", color: "#5b7cff", icon: Bot, description: "大模型调用" },
  rag: { label: "RAG", color: "#34d399", icon: Database, description: "知识库检索" },
  tool: { label: "Tool", color: "#fbbf24", icon: Calculator, description: "工具调用" },
  output: { label: "Output", color: "#f472b6", icon: TerminalSquare, description: "结果输出" },
};

interface NodeData {
  label: string;
  nodeType: NodeType;
  config: NodeConfig;
  onChange: (config: NodeConfig) => void;
  onConfigOpen: (key: string) => void;
}

// 通用节点壳：连接点 + 状态色
function NodeShell({
  data,
  children,
}: {
  data: NodeData;
  children?: React.ReactNode;
}) {
  const meta = nodeMeta[data.nodeType];
  const Icon = meta.icon;
  return (
    <div
      className="w-48 rounded-xl border bg-nebula-900 shadow-lg"
      style={{ borderColor: `${meta.color}66` }}
    >
      <Handle type="target" position={Position.Left} />
      <div
        className="flex items-center gap-2 px-3 py-2 rounded-t-xl border-b"
        style={{ borderColor: `${meta.color}33`, background: `${meta.color}14` }}
      >
        <Icon size={13} style={{ color: meta.color }} />
        <span className="text-xs font-medium text-slate-200">{data.label}</span>
      </div>
      <div className="px-3 py-2 text-[10px] text-slate-500">{meta.description}</div>
      {children}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

export const FlowInputNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <button
        className="w-full px-3 pb-2 text-left text-[10px] text-nebula-300 hover:text-nebula-400"
        onClick={() => data.onConfigOpen(data.label)}
      >
        ⚙ 节点设置
      </button>
    </NodeShell>
  );
});

export const FlowLLMNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-2 space-y-1">
        <div className="flex items-center gap-1.5 text-[10px] text-slate-400">
          <GitBranch size={9} className="text-nebula-400" />
          模型：<span className="text-slate-200">{data.config.model || "未选择"}</span>
        </div>
        <button
          className="w-full text-left text-[10px] text-nebula-300 hover:text-nebula-400"
          onClick={() => data.onConfigOpen(data.label)}
        >
          ⚙ 配置提示词…
        </button>
      </div>
    </NodeShell>
  );
});

export const FlowRAGNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-2">
        <div className="text-[10px] text-slate-400">
          知识库：<span className="text-slate-200">{data.config.knowledge_base_id ? `#${data.config.knowledge_base_id}` : "未选择"}</span>
        </div>
        <button
          className="mt-1 w-full text-left text-[10px] text-nebula-300 hover:text-nebula-400"
          onClick={() => data.onConfigOpen(data.label)}
        >
          ⚙ 选择知识库…
        </button>
      </div>
    </NodeShell>
  );
});

export const FlowToolNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-2">
        <div className="text-[10px] text-slate-400">
          工具：<span className="text-slate-200">{data.config.tool || "未选择"}</span>
        </div>
        <button
          className="mt-1 w-full text-left text-[10px] text-nebula-300 hover:text-nebula-400"
          onClick={() => data.onConfigOpen(data.label)}
        >
          ⚙ 选择工具…
        </button>
      </div>
    </NodeShell>
  );
});

export const FlowOutputNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-2 text-[10px] text-slate-500">聚合输出最终结果</div>
    </NodeShell>
  );
});

export const nodeTypes = {
  input: FlowInputNode,
  llm: FlowLLMNode,
  rag: FlowRAGNode,
  tool: FlowToolNode,
  output: FlowOutputNode,
};

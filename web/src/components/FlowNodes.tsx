import { memo } from "react";
import { Handle, Position } from "@xyflow/react";
import { Bot, Calculator, Database, GitBranch, TerminalSquare, Boxes, Settings } from "lucide-react";
import type { NodeConfig, NodeType } from "../types";

// 节点外观配置
export const nodeMeta: Record<
  NodeType,
  { label: string; color: string; icon: React.ElementType; description: string; gradient: string }
> = {
  input: {
    label: "Input",
    color: "#8aa3ff",
    icon: Boxes,
    description: "任务输入入口",
    gradient: "from-indigo-500/20 to-transparent",
  },
  llm: {
    label: "LLM",
    color: "#5b7cff",
    icon: Bot,
    description: "大语言模型调用",
    gradient: "from-blue-500/20 to-transparent",
  },
  rag: {
    label: "RAG",
    color: "#34d399",
    icon: Database,
    description: "知识库检索增强",
    gradient: "from-emerald-500/20 to-transparent",
  },
  tool: {
    label: "Tool",
    color: "#fbbf24",
    icon: Calculator,
    description: "工具函数调用",
    gradient: "from-amber-500/20 to-transparent",
  },
  output: {
    label: "Output",
    color: "#f472b6",
    icon: TerminalSquare,
    description: "结果输出节点",
    gradient: "from-pink-500/20 to-transparent",
  },
};

interface NodeData {
  label: string;
  nodeType: NodeType;
  config: NodeConfig;
  onChange: (config: NodeConfig) => void;
  onConfigOpen: (key: string) => void;
}

// 通用节点壳：连接点 + 状态色 + 渐变头部
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
      className="w-48 rounded-xl border bg-nebula-900 shadow-lg transition-all duration-200 hover:shadow-xl hover:scale-[1.02] group"
      style={{
        borderColor: `${meta.color}55`,
        boxShadow: `0 4px 20px -8px ${meta.color}20`,
      }}
    >
      <Handle
        type="target"
        position={Position.Left}
        className="!w-3 !h-3 !border-2 !border-nebula-900 transition-transform duration-200 group-hover:scale-125"
        style={{ background: meta.color }}
      />
      <div
        className={`relative flex items-center gap-2 px-3 py-2.5 rounded-t-xl border-b overflow-hidden bg-gradient-to-r ${meta.gradient}`}
        style={{ borderColor: `${meta.color}33` }}
      >
        {/* 顶部光晕效果 */}
        <div
          className="absolute top-0 left-1/2 -translate-x-1/2 w-16 h-px opacity-60"
          style={{ background: `linear-gradient(90deg, transparent, ${meta.color}, transparent)` }}
        />
        <div
          className="flex items-center justify-center w-6 h-6 rounded-md transition-transform duration-200 group-hover:scale-110"
          style={{ backgroundColor: `${meta.color}20` }}
        >
          <Icon size={13} style={{ color: meta.color }} />
        </div>
        <div className="flex-1 min-w-0">
          <span className="text-xs font-semibold text-slate-200 block truncate">{data.label}</span>
          <span className="text-[9px] text-slate-500">{meta.label} Node</span>
        </div>
      </div>
      <div className="px-3 py-2 text-[10px] text-slate-500">{meta.description}</div>
      {children}
      <Handle
        type="source"
        position={Position.Right}
        className="!w-3 !h-3 !border-2 !border-nebula-900 transition-transform duration-200 group-hover:scale-125"
        style={{ background: meta.color }}
      />
    </div>
  );
}

// 配置按钮
function ConfigButton({ onClick, label }: { onClick: () => void; label: string }) {
  return (
    <button
      className="w-full flex items-center gap-1.5 px-2 py-1.5 mx-1 mb-2 rounded-md text-[10px] text-nebula-300 hover:text-nebula-200 hover:bg-nebula-800/50 transition-colors group/btn"
      onClick={onClick}
    >
      <Settings size={10} className="transition-transform duration-200 group-hover/btn:rotate-45" />
      {label}
    </button>
  );
}

export const FlowInputNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <ConfigButton onClick={() => data.onConfigOpen(data.label)} label="配置输入" />
    </NodeShell>
  );
});

export const FlowLLMNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-1 space-y-1">
        <div className="flex items-center gap-1.5 text-[10px] text-slate-400">
          <GitBranch size={9} className="text-nebula-400 flex-shrink-0" />
          <span className="truncate">
            模型：<span className="text-slate-200 font-medium">{data.config.model || "未选择"}</span>
          </span>
        </div>
        {data.config.system && (
          <div className="text-[9px] text-slate-500 truncate" title={data.config.system}>
            system: {data.config.system.slice(0, 30)}...
          </div>
        )}
      </div>
      <ConfigButton onClick={() => data.onConfigOpen(data.label)} label="配置提示词" />
    </NodeShell>
  );
});

export const FlowRAGNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-1">
        <div className="text-[10px] text-slate-400 truncate">
          知识库：<span className="text-slate-200 font-medium">{data.config.knowledge_base_id ? `#${data.config.knowledge_base_id}` : "未选择"}</span>
        </div>
      </div>
      <ConfigButton onClick={() => data.onConfigOpen(data.label)} label="选择知识库" />
    </NodeShell>
  );
});

export const FlowToolNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-1">
        <div className="text-[10px] text-slate-400 truncate">
          工具：<span className="text-slate-200 font-medium">{data.config.tool || "未选择"}</span>
        </div>
      </div>
      <ConfigButton onClick={() => data.onConfigOpen(data.label)} label="选择工具" />
    </NodeShell>
  );
});

export const FlowOutputNode = memo(({ data }: { data: NodeData }) => {
  return (
    <NodeShell data={data}>
      <div className="px-3 pb-3 text-[10px] text-slate-500">聚合输出最终结果</div>
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

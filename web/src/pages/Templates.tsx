import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  LayoutGrid,
  Search,
  Sparkles,
  X,
  Play,
  GitBranch,
  ArrowRight,
  Check,
  Bot,
  Database,
  TerminalSquare,
  Boxes,
  Calculator,
} from "lucide-react";
import { api } from "../api/client";
import type { Workflow, NodeType } from "../types";
import { nodeMeta } from "../components/FlowNodes";

// 节点类型图标映射
const nodeIconMap: Record<string, React.ElementType> = {
  input: Boxes,
  llm: Bot,
  rag: Database,
  tool: Calculator,
  output: TerminalSquare,
};

// 分类颜色
const categoryColors: Record<string, string> = {
  客服: "#f472b6",
  数据分析: "#34d399",
  代码助手: "#818cf8",
  内容创作: "#fbbf24",
  自动化: "#22d3ee",
  通用: "#94a3b8",
};

function getCategoryColor(category: string): string {
  return categoryColors[category] || "#94a3b8";
}

// 模板迷你预览图 — SVG 绘制简化的工作流拓扑
function TemplatePreview({ workflow, size = "sm" }: { workflow: Workflow; size?: "sm" | "lg" }) {
  const nodes = workflow.nodes || [];
  const edges = workflow.edges || [];

  if (nodes.length === 0) {
    return (
      <div className={`flex items-center justify-center ${size === "lg" ? "h-40" : "h-20"} text-slate-600`}>
        <GitBranch size={size === "lg" ? 32 : 20} />
      </div>
    );
  }

  // 计算画布尺寸
  const isLg = size === "lg";
  const W = isLg ? 480 : 200;
  const H = isLg ? 160 : 70;

  // 按节点位置计算布局
  const posXs = nodes.map((n) => n.position_x ?? n.x ?? 0);
  const posYs = nodes.map((n) => n.position_y ?? n.y ?? 0);
  const minX = Math.min(...posXs, 0);
  const maxX = Math.max(...posXs, 300);
  const minY = Math.min(...posYs, 0);
  const maxY = Math.max(...posYs, 200);
  const rangeX = maxX - minX || 1;
  const rangeY = maxY - minY || 1;

  const scaleX = (W - 40) / rangeX;
  const scaleY = (H - 20) / rangeY;
  const scale = Math.min(scaleX, scaleY, 1);

  const offsetX = (W - rangeX * scale) / 2 - minX * scale;
  const offsetY = (H - rangeY * scale) / 2 - minY * scale;

  const nodeW = isLg ? 70 : 28;
  const nodeH = isLg ? 36 : 14;

  const getNodePos = (key: string) => {
    const node = nodes.find((n) => n.node_key === key);
    if (!node) return { x: 0, y: 0 };
    const x = (node.position_x ?? node.x ?? 0) * scale + offsetX;
    const y = (node.position_y ?? node.y ?? 0) * scale + offsetY;
    return { x, y };
  };

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full h-full overflow-visible">
      {/* 边 */}
      {edges.map((e, i) => {
        const src = getNodePos(e.source_node);
        const tgt = getNodePos(e.target_node);
        const sx = src.x + nodeW;
        const sy = src.y + nodeH / 2;
        const tx = tgt.x;
        const ty = tgt.y + nodeH / 2;
        const mx = (sx + tx) / 2;
        const srcNode = nodes.find((n) => n.node_key === e.source_node);
        const color = srcNode ? nodeMeta[srcNode.node_type as NodeType]?.color || "#5b7cff" : "#5b7cff";
        return (
          <path
            key={i}
            d={`M ${sx} ${sy} C ${mx} ${sy}, ${mx} ${ty}, ${tx} ${ty}`}
            fill="none"
            stroke={color}
            strokeOpacity={0.4}
            strokeWidth={isLg ? 1.5 : 1}
          />
        );
      })}
      {/* 节点 */}
      {nodes.map((n) => {
        const pos = getNodePos(n.node_key);
        const meta = nodeMeta[n.node_type as NodeType] || nodeMeta.llm;
        const Icon = nodeIconMap[n.node_type] || Boxes;
        const iconSize = isLg ? 14 : 7;
        return (
          <g key={n.node_key}>
            <rect
              x={pos.x}
              y={pos.y}
              width={nodeW}
              height={nodeH}
              rx={isLg ? 6 : 3}
              fill="#0f172a"
              stroke={meta.color}
              strokeOpacity={0.5}
              strokeWidth={1}
            />
            {isLg && (
              <>
                <foreignObject x={pos.x + 6} y={pos.y + (nodeH - iconSize) / 2} width={iconSize} height={iconSize}>
                  <div style={{ color: meta.color }}>
                    <Icon size={iconSize} />
                  </div>
                </foreignObject>
                <text
                  x={pos.x + nodeW / 2 + 6}
                  y={pos.y + nodeH / 2 + 3}
                  fill="#cbd5e1"
                  fontSize={9}
                  fontWeight={500}
                  textAnchor="middle"
                >
                  {meta.label}
                </text>
              </>
            )}
          </g>
        );
      })}
    </svg>
  );
}

// 模板详情弹窗
function TemplateDetailModal({
  template,
  onClose,
  onUse,
}: {
  template: Workflow;
  onClose: () => void;
  onUse: (name: string) => Promise<number>;
}) {
  const [customName, setCustomName] = useState(template.name + "（副本）");
  const [using, setUsing] = useState(false);
  const navigate = useNavigate();

  const handleUse = async () => {
    setUsing(true);
    try {
      const wfId = await onUse(customName);
      navigate(`/workflows/${wfId}`);
    } catch {
      /* error handled by caller */
    } finally {
      setUsing(false);
    }
  };

  const nodes = template.nodes || [];
  const nodeTypeCount: Record<string, number> = {};
  nodes.forEach((n) => {
    nodeTypeCount[n.node_type] = (nodeTypeCount[n.node_type] || 0) + 1;
  });

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm p-4"
      onClick={onClose}
    >
      <div
        className="card w-full max-w-3xl max-h-[90vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部 */}
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-4">
            <div
              className="w-14 h-14 rounded-xl flex items-center justify-center text-2xl shrink-0"
              style={{ backgroundColor: `${getCategoryColor(template.category || "通用")}15` }}
            >
              {template.icon || "📋"}
            </div>
            <div className="min-w-0">
              <h3 className="text-lg font-semibold text-slate-100">{template.name}</h3>
              <div className="flex items-center gap-2 mt-1">
                <span
                  className="text-[10px] px-2 py-0.5 rounded-full border"
                  style={{
                    color: getCategoryColor(template.category || "通用"),
                    borderColor: `${getCategoryColor(template.category || "通用")}40`,
                    backgroundColor: `${getCategoryColor(template.category || "通用")}10`,
                  }}
                >
                  {template.category || "通用"}
                </span>
                <span className="text-[10px] text-slate-500">
                  {nodes.length} 节点 · {template.edges?.length || 0} 边
                </span>
              </div>
            </div>
          </div>
          <button
            onClick={onClose}
            className="text-slate-500 hover:text-slate-300 transition-colors p-1"
          >
            <X size={18} />
          </button>
        </div>

        {/* 描述 */}
        <p className="text-sm text-slate-400 mt-4">{template.description}</p>

        {/* 预览图 */}
        <div className="mt-5 p-4 rounded-xl bg-nebula-950 border border-nebula-800">
          <div className="text-xs text-slate-500 mb-2 flex items-center gap-1.5">
            <LayoutGrid size={12} /> 工作流预览
          </div>
          <div className="h-40">
            <TemplatePreview workflow={template} size="lg" />
          </div>
        </div>

        {/* 节点组成 */}
        <div className="mt-5">
          <div className="text-xs text-slate-500 mb-2">节点组成</div>
          <div className="flex flex-wrap gap-2">
            {Object.entries(nodeTypeCount).map(([type, count]) => {
              const meta = nodeMeta[type as NodeType] || nodeMeta.llm;
              const Icon = nodeIconMap[type] || Boxes;
              return (
                <div
                  key={type}
                  className="flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg bg-nebula-950 border border-nebula-800"
                >
                  <Icon size={12} style={{ color: meta.color }} />
                  <span className="text-xs text-slate-300">{meta.label}</span>
                  <span className="text-[10px] text-slate-500">×{count}</span>
                </div>
              );
            })}
          </div>
        </div>

        {/* 使用区域 */}
        <div className="mt-6 p-4 rounded-xl bg-nebula-800/50 border border-nebula-700">
          <label className="label">工作流名称</label>
          <input
            type="text"
            className="input"
            value={customName}
            onChange={(e) => setCustomName(e.target.value)}
            placeholder="输入工作流名称"
          />
          <div className="flex items-center gap-3 mt-3">
            <button
              onClick={handleUse}
              disabled={using || !customName.trim()}
              className="btn-primary flex-1 justify-center"
            >
              {using ? (
                <>
                  <div className="w-3 h-3 border-2 border-white/30 border-t-white rounded-full animate-spin" />
                  创建中...
                </>
              ) : (
                <>
                  <Play size={14} /> 使用此模板
                </>
              )}
            </button>
            <button onClick={onClose} className="btn-ghost">
              取消
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default function Templates() {
  const [templates, setTemplates] = useState<Workflow[]>([]);
  const [categories, setCategories] = useState<string[]>([]);
  const [activeCategory, setActiveCategory] = useState<string>("全部");
  const [searchQuery, setSearchQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const [selectedTemplate, setSelectedTemplate] = useState<Workflow | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  const load = async () => {
    setLoading(true);
    try {
      const [tplRes, catRes] = await Promise.all([
        api.listTemplates(),
        api.listTemplateCategories(),
      ]);
      setTemplates(tplRes.templates || []);
      setCategories(catRes.categories || []);
    } catch (err) {
      console.error("Failed to load templates:", err);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
  }, []);

  const filteredTemplates = templates.filter((t) => {
    const matchCategory = activeCategory === "全部" || t.category === activeCategory;
    const matchSearch =
      !searchQuery ||
      t.name.toLowerCase().includes(searchQuery.toLowerCase()) ||
      t.description.toLowerCase().includes(searchQuery.toLowerCase());
    return matchCategory && matchSearch;
  });

  const handleUseTemplate = async (id: number, name: string): Promise<number> => {
    const result = await api.useTemplate(id, name);
    setToast(`已创建：${result.name}`);
    setTimeout(() => setToast(null), 2500);
    return result.workflow_id;
  };

  const openDetail = async (tpl: Workflow) => {
    // 如果没有节点详情，先获取完整模板
    if (!tpl.nodes || tpl.nodes.length === 0) {
      try {
        const detail = await api.getTemplate(tpl.id);
        setSelectedTemplate(detail);
      } catch {
        setSelectedTemplate(tpl);
      }
    } else {
      setSelectedTemplate(tpl);
    }
  };

  return (
    <div className="space-y-6">
      {/* 顶部标题 */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold text-slate-100 flex items-center gap-2">
            <Sparkles className="text-nebula-400" size={20} />
            模板市场
          </h2>
          <p className="text-xs text-slate-500 mt-1">精选工作流模板 · 一键使用，快速上手</p>
        </div>
      </div>

      {/* 搜索 + 分类过滤 */}
      <div className="flex flex-col sm:flex-row gap-3 items-stretch sm:items-center">
        <div className="relative flex-1 max-w-md">
          <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-slate-500" />
          <input
            type="text"
            className="input pl-9"
            placeholder="搜索模板..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
          />
        </div>
        <div className="flex items-center gap-1.5 overflow-x-auto pb-1">
          <button
            onClick={() => setActiveCategory("全部")}
            className={`px-3 py-1.5 rounded-lg text-xs font-medium whitespace-nowrap transition-all ${
              activeCategory === "全部"
                ? "bg-nebula-500 text-white"
                : "bg-nebula-800 text-slate-400 hover:bg-nebula-700 hover:text-slate-200 border border-nebula-700"
            }`}
          >
            全部
          </button>
          {categories.map((cat) => (
            <button
              key={cat}
              onClick={() => setActiveCategory(cat)}
              className={`px-3 py-1.5 rounded-lg text-xs font-medium whitespace-nowrap transition-all ${
                activeCategory === cat
                  ? "bg-nebula-500 text-white"
                  : "bg-nebula-800 text-slate-400 hover:bg-nebula-700 hover:text-slate-200 border border-nebula-700"
              }`}
            >
              {cat}
            </button>
          ))}
        </div>
      </div>

      {/* 模板列表 */}
      {loading ? (
        <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
          {[1, 2, 3, 4, 5, 6].map((i) => (
            <div key={i} className="card animate-pulse">
              <div className="h-20 bg-nebula-800 rounded-lg mb-4" />
              <div className="h-4 bg-nebula-800 rounded w-2/3 mb-2" />
              <div className="h-3 bg-nebula-800 rounded w-full mb-1" />
              <div className="h-3 bg-nebula-800 rounded w-1/2" />
            </div>
          ))}
        </div>
      ) : filteredTemplates.length === 0 ? (
        <div className="card text-center py-16">
          <LayoutGrid className="mx-auto text-slate-600 mb-3" size={32} />
          <p className="text-sm text-slate-400">
            {searchQuery ? "没有找到匹配的模板" : "暂无模板"}
          </p>
        </div>
      ) : (
        <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
          {filteredTemplates.map((tpl) => (
            <div
              key={tpl.id}
              className="card hover:border-nebula-500 transition-all duration-200 group cursor-pointer flex flex-col"
              onClick={() => openDetail(tpl)}
            >
              {/* 图标 + 分类 */}
              <div className="flex items-start justify-between mb-3">
                <div
                  className="w-11 h-11 rounded-lg flex items-center justify-center text-xl shrink-0 transition-transform duration-200 group-hover:scale-110"
                  style={{
                    backgroundColor: `${getCategoryColor(tpl.category || "通用")}15`,
                  }}
                >
                  {tpl.icon || "📋"}
                </div>
                <span
                  className="text-[10px] px-2 py-0.5 rounded-full border"
                  style={{
                    color: getCategoryColor(tpl.category || "通用"),
                    borderColor: `${getCategoryColor(tpl.category || "通用")}40`,
                    backgroundColor: `${getCategoryColor(tpl.category || "通用")}10`,
                  }}
                >
                  {tpl.category || "通用"}
                </span>
              </div>

              {/* 名称 + 描述 */}
              <h3 className="font-medium text-slate-100 group-hover:text-nebula-300 transition-colors">
                {tpl.name}
              </h3>
              <p className="text-xs text-slate-500 mt-1 line-clamp-2 min-h-[2rem]">
                {tpl.description || "暂无描述"}
              </p>

              {/* 预览图 */}
              <div className="mt-3 h-20 rounded-lg bg-nebula-950 border border-nebula-800 overflow-hidden">
                <TemplatePreview workflow={tpl} size="sm" />
              </div>

              {/* 底部：节点统计 + 使用按钮 */}
              <div className="flex items-center justify-between mt-3 pt-3 border-t border-nebula-800">
                <div className="flex items-center gap-1.5 text-[10px] text-slate-500">
                  <GitBranch size={11} />
                  {tpl.nodes?.length ?? 0} 节点 · {tpl.edges?.length ?? 0} 边
                </div>
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    openDetail(tpl);
                  }}
                  className="flex items-center gap-1 text-xs text-nebula-400 hover:text-nebula-300 font-medium transition-colors"
                >
                  使用 <ArrowRight size={12} />
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* 模板详情弹窗 */}
      {selectedTemplate && (
        <TemplateDetailModal
          template={selectedTemplate}
          onClose={() => setSelectedTemplate(null)}
          onUse={(name) => handleUseTemplate(selectedTemplate.id, name)}
        />
      )}

      {/* Toast 提示 */}
      {toast && (
        <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-50 flex items-center gap-2 px-4 py-2.5 rounded-lg bg-emerald-500/20 border border-emerald-500/40 text-emerald-300 text-sm shadow-lg backdrop-blur-sm animate-fade-in">
          <Check size={14} />
          {toast}
        </div>
      )}
    </div>
  );
}

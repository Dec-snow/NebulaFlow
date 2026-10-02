import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Bot,
  Search,
  Sparkles,
  X,
  Download,
  Cpu,
  Server,
  Globe,
  Clock,
  Check,
  Shield,
  Zap,
  Tag,
  Activity,
  ArrowRight,
} from "lucide-react";
import { api } from "../api/client";
import type { Agent, AgentRuntimeType } from "../types";

// 能力标签颜色
const capColors: Record<string, string> = {
  tool_call: "bg-blue-500/15 text-blue-300 border-blue-500/30",
  rag: "bg-purple-500/15 text-purple-300 border-purple-500/30",
  memory: "bg-amber-500/15 text-amber-300 border-amber-500/30",
  streaming: "bg-emerald-500/15 text-emerald-300 border-emerald-500/30",
  code: "bg-cyan-500/15 text-cyan-300 border-cyan-500/30",
  search: "bg-orange-500/15 text-orange-300 border-orange-500/30",
  multimodal: "bg-pink-500/15 text-pink-300 border-pink-500/30",
};

const capLabels: Record<string, string> = {
  tool_call: "工具调用",
  rag: "知识检索",
  memory: "记忆",
  streaming: "流式输出",
  code: "代码生成",
  search: "联网搜索",
  multimodal: "多模态",
};

const runtimeLabels: Record<AgentRuntimeType, string> = {
  native: "Native",
  langchain: "LangChain",
  http: "HTTP",
};

const runtimeIcons: Record<AgentRuntimeType, typeof Bot> = {
  native: Cpu,
  langchain: Server,
  http: Globe,
};

const runtimeColors: Record<AgentRuntimeType, string> = {
  native: "#34d399",
  langchain: "#818cf8",
  http: "#fbbf24",
};

function AgentDetailModal({
  agent,
  installed,
  onClose,
  onInstall,
}: {
  agent: Agent;
  installed: boolean;
  onClose: () => void;
  onInstall: () => Promise<number>;
}) {
  const [installing, setInstalling] = useState(false);
  const navigate = useNavigate();
  const Icon = runtimeIcons[agent.runtime_type as AgentRuntimeType] || Bot;
  const color = runtimeColors[agent.runtime_type as AgentRuntimeType] || "#94a3b8";

  const handleInstall = async () => {
    setInstalling(true);
    try {
      const agentId = await onInstall();
      navigate(`/agents`);
    } catch {
      /* error handled by caller */
    } finally {
      setInstalling(false);
    }
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm p-4"
      onClick={onClose}
    >
      <div
        className="card w-full max-w-2xl max-h-[90vh] overflow-y-auto"
        onClick={(e) => e.stopPropagation()}
      >
        {/* 头部 */}
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-4">
            <div
              className="w-14 h-14 rounded-xl flex items-center justify-center shrink-0"
              style={{ backgroundColor: `${color}15` }}
            >
              <Icon size={28} style={{ color }} />
            </div>
            <div className="min-w-0">
              <h3 className="text-lg font-semibold text-slate-100">{agent.name}</h3>
              <div className="flex items-center gap-2 mt-1 flex-wrap">
                <span
                  className="text-[10px] px-2 py-0.5 rounded-full border"
                  style={{ color, borderColor: `${color}40`, backgroundColor: `${color}10` }}
                >
                  {runtimeLabels[agent.runtime_type as AgentRuntimeType]}
                </span>
                <span className="text-[10px] text-slate-500">v{agent.version}</span>
                <span className="text-[10px] text-slate-500">
                  <Clock size={10} className="inline mr-1" />
                  {agent.timeout_sec}s 超时
                </span>
              </div>
            </div>
          </div>
          <button
            onClick={onClose}
            className="text-slate-500 hover:text-slate-300 p-1 transition-colors"
          >
            <X size={18} />
          </button>
        </div>

        {/* 描述 */}
        <p className="text-sm text-slate-400 mt-4 leading-relaxed">{agent.description}</p>

        {/* 能力标签 */}
        <div className="mt-5">
          <div className="text-xs text-slate-500 mb-2 flex items-center gap-1.5">
            <Zap size={12} className="text-nebula-400" /> 能力标签
          </div>
          <div className="flex flex-wrap gap-1.5">
            {agent.capabilities?.map((c) => (
              <span
                key={c}
                className={`text-[10px] px-2 py-1 rounded-full border ${
                  capColors[c] || "bg-slate-500/15 text-slate-400 border-slate-500/30"
                }`}
              >
                {capLabels[c] || c}
              </span>
            ))}
          </div>
        </div>

        {/* 详细信息 */}
        <div className="mt-5 grid grid-cols-2 gap-3">
          <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
            <div className="text-[10px] text-slate-500 mb-1">默认模型</div>
            <div className="text-sm text-slate-200 font-mono">{agent.model || "未指定"}</div>
          </div>
          <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
            <div className="text-[10px] text-slate-500 mb-1">Runtime 类型</div>
            <div className="text-sm text-slate-200 flex items-center gap-1.5">
              <Icon size={13} style={{ color }} />
              {runtimeLabels[agent.runtime_type as AgentRuntimeType]}
            </div>
          </div>
          <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
            <div className="text-[10px] text-slate-500 mb-1">Endpoint</div>
            <div className="text-sm text-slate-200 font-mono truncate" title={agent.endpoint}>
              {agent.endpoint || "— (Native)"}
            </div>
          </div>
          <div className="p-3 rounded-lg bg-nebula-950 border border-nebula-800">
            <div className="text-[10px] text-slate-500 mb-1">版本</div>
            <div className="text-sm text-slate-200 font-mono">v{agent.version}</div>
          </div>
        </div>

        {/* 安全提示 */}
        <div className="mt-4 p-3 rounded-lg bg-nebula-800/50 border border-nebula-700 flex items-start gap-2.5">
          <Shield size={14} className="text-nebula-400 shrink-0 mt-0.5" />
          <div className="text-[11px] text-slate-400 leading-relaxed">
            <span className="text-slate-300 font-medium">系统内置 Agent</span>
            <br />
            安装后会复制到你的个人注册中心，你可以自由修改配置。系统 Agent 更新不会影响已安装的副本。
          </div>
        </div>

        {/* 安装按钮 */}
        <div className="mt-5 flex items-center gap-3">
          <button
            onClick={handleInstall}
            disabled={installing || installed}
            className="btn-primary flex-1 justify-center"
          >
            {installing ? (
              <>
                <div className="w-4 h-4 border-2 border-white/30 border-t-white rounded-full animate-spin" />
                安装中...
              </>
            ) : installed ? (
              <>
                <Check size={15} /> 已安装
              </>
            ) : (
              <>
                <Download size={15} /> 一键安装
              </>
            )}
          </button>
          <button onClick={onClose} className="btn-ghost">
            关闭
          </button>
        </div>
      </div>
    </div>
  );
}

export default function AgentMarketplace() {
  const [agents, setAgents] = useState<Agent[]>([]);
  const [installedNames, setInstalledNames] = useState<Record<string, boolean>>({});
  const [capOptions, setCapOptions] = useState<{ value: string; label: string }[]>([]);
  const [runtimeOptions, setRuntimeOptions] = useState<{ value: string; label: string }[]>([]);
  const [search, setSearch] = useState("");
  const [filterRuntime, setFilterRuntime] = useState("");
  const [filterCap, setFilterCap] = useState("");
  const [loading, setLoading] = useState(true);
  const [selectedAgent, setSelectedAgent] = useState<Agent | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  const load = async () => {
    setLoading(true);
    try {
      const [mktRes, capsRes, rtRes] = await Promise.all([
        api.listMarketplaceAgents({ runtime_type: filterRuntime || undefined, capability: filterCap || undefined }),
        api.listCapabilities().catch(() => ({ capabilities: [] })),
        api.listRuntimeTypes().catch(() => ({ runtime_types: [] })),
      ]);
      setAgents(mktRes.agents || []);
      setInstalledNames(mktRes.installed_names || {});
      setCapOptions(capsRes.capabilities || []);
      setRuntimeOptions(rtRes.runtime_types || []);
    } catch (err) {
      console.error("Failed to load marketplace:", err);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
  }, [filterRuntime, filterCap]);

  const filtered = agents.filter(
    (a) =>
      !search ||
      a.name.toLowerCase().includes(search.toLowerCase()) ||
      a.description.toLowerCase().includes(search.toLowerCase())
  );

  const handleInstall = async (id: number): Promise<number> => {
    const result = await api.installAgent(id);
    setToast(`已安装：${result.name}`);
    setInstalledNames((prev) => ({ ...prev, [result.name]: true }));
    setTimeout(() => setToast(null), 2500);
    return result.agent_id;
  };

  const openDetail = async (agent: Agent) => {
    // 已经有完整数据，直接打开
    setSelectedAgent(agent);
  };

  return (
    <div className="space-y-6">
      {/* 顶部标题 */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold text-slate-100 flex items-center gap-2">
            <Sparkles className="text-nebula-400" size={20} />
            Agent 市场
          </h2>
          <p className="text-xs text-slate-500 mt-1">
            精选系统 Agent · 一键安装到你的注册中心
          </p>
        </div>
      </div>

      {/* 搜索 + 筛选 */}
      <div className="flex flex-col sm:flex-row gap-3 items-stretch sm:items-center">
        <div className="relative flex-1 max-w-md">
          <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-slate-500" />
          <input
            type="text"
            className="input pl-9"
            placeholder="搜索 Agent..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </div>
        <div className="flex items-center gap-1.5 overflow-x-auto pb-1">
          <button
            onClick={() => setFilterRuntime("")}
            className={`px-3 py-1.5 rounded-lg text-xs font-medium whitespace-nowrap transition-all ${
              filterRuntime === ""
                ? "bg-nebula-500 text-white"
                : "bg-nebula-800 text-slate-400 hover:bg-nebula-700 hover:text-slate-200 border border-nebula-700"
            }`}
          >
            全部类型
          </button>
          {runtimeOptions.map((o) => (
            <button
              key={o.value}
              onClick={() => setFilterRuntime(filterRuntime === o.value ? "" : o.value)}
              className={`px-3 py-1.5 rounded-lg text-xs font-medium whitespace-nowrap transition-all ${
                filterRuntime === o.value
                  ? "bg-nebula-500 text-white"
                  : "bg-nebula-800 text-slate-400 hover:bg-nebula-700 hover:text-slate-200 border border-nebula-700"
              }`}
            >
              {o.label.split("（")[0]}
            </button>
          ))}
        </div>
        <select
          value={filterCap}
          onChange={(e) => setFilterCap(e.target.value)}
          className="input max-w-[180px] text-xs"
        >
          <option value="">所有能力</option>
          {capOptions.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      </div>

      {/* Agent 卡片网格 */}
      {loading ? (
        <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
          {[1, 2, 3, 4, 5, 6].map((i) => (
            <div key={i} className="card animate-pulse">
              <div className="h-12 w-12 rounded-lg bg-nebula-800 mb-3" />
              <div className="h-4 w-1/2 bg-nebula-800 rounded mb-2" />
              <div className="h-3 w-full bg-nebula-800/60 rounded mb-1" />
              <div className="h-3 w-2/3 bg-nebula-800/60 rounded" />
            </div>
          ))}
        </div>
      ) : filtered.length === 0 ? (
        <div className="card text-center py-16">
          <Bot className="mx-auto text-slate-600 mb-3" size={32} />
          <p className="text-sm text-slate-400">
            {search ? "没有找到匹配的 Agent" : "暂无可用的系统 Agent"}
          </p>
        </div>
      ) : (
        <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
          {filtered.map((a) => {
            const Icon = runtimeIcons[a.runtime_type as AgentRuntimeType] || Bot;
            const color = runtimeColors[a.runtime_type as AgentRuntimeType] || "#94a3b8";
            const isInstalled = installedNames[a.name];

            return (
              <div
                key={a.id}
                className="card hover:border-nebula-500 transition-all duration-200 group cursor-pointer flex flex-col"
                onClick={() => openDetail(a)}
              >
                {/* 卡片头 */}
                <div className="flex items-start justify-between mb-3">
                  <div
                    className="w-11 h-11 rounded-lg flex items-center justify-center shrink-0 transition-transform duration-200 group-hover:scale-110"
                    style={{ backgroundColor: `${color}15` }}
                  >
                    <Icon size={20} style={{ color }} />
                  </div>
                  <div className="flex items-center gap-1.5">
                    {isInstalled && (
                      <span className="text-[9px] px-1.5 py-0.5 rounded-full bg-emerald-500/15 text-emerald-400 border border-emerald-500/30 flex items-center gap-1">
                        <Check size={9} /> 已安装
                      </span>
                    )}
                    <span
                      className="text-[9px] px-1.5 py-0.5 rounded-full border"
                      style={{ color, borderColor: `${color}40`, backgroundColor: `${color}10` }}
                    >
                      {runtimeLabels[a.runtime_type as AgentRuntimeType]}
                    </span>
                  </div>
                </div>

                {/* 名称 + 描述 */}
                <h3 className="font-medium text-slate-100 group-hover:text-nebula-300 transition-colors">
                  {a.name}
                </h3>
                <p className="text-xs text-slate-500 mt-1 line-clamp-2 min-h-[2rem]">
                  {a.description || "暂无描述"}
                </p>

                {/* 能力标签 */}
                <div className="mt-3 flex flex-wrap gap-1">
                  {(a.capabilities || []).slice(0, 4).map((c) => (
                    <span
                      key={c}
                      className={`text-[9px] px-1.5 py-0.5 rounded-full border ${
                        capColors[c] || "bg-slate-500/15 text-slate-400 border-slate-500/30"
                      }`}
                    >
                      {capLabels[c] || c}
                    </span>
                  ))}
                  {(a.capabilities || []).length > 4 && (
                    <span className="text-[9px] px-1.5 py-0.5 rounded-full border border-slate-600/30 text-slate-500">
                      +{(a.capabilities || []).length - 4}
                    </span>
                  )}
                </div>

                {/* 底部：版本 + 安装按钮 */}
                <div className="flex items-center justify-between mt-4 pt-3 border-t border-nebula-800">
                  <div className="flex items-center gap-1.5 text-[10px] text-slate-500">
                    <Tag size={10} />
                    v{a.version}
                  </div>
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      openDetail(a);
                    }}
                    className="flex items-center gap-1 text-xs text-nebula-400 hover:text-nebula-300 font-medium transition-colors"
                  >
                    {isInstalled ? "查看详情" : "安装"} <ArrowRight size={12} />
                  </button>
                </div>
              </div>
            );
          })}
        </div>
      )}

      {/* 详情弹窗 */}
      {selectedAgent && (
        <AgentDetailModal
          agent={selectedAgent}
          installed={installedNames[selectedAgent.name]}
          onClose={() => setSelectedAgent(null)}
          onInstall={() => handleInstall(selectedAgent.id)}
        />
      )}

      {/* Toast 提示 */}
      {toast && (
        <div className="fixed bottom-6 left-1/2 -translate-x-1/2 z-50 flex items-center gap-2 px-4 py-2.5 rounded-lg bg-emerald-500/20 border border-emerald-500/40 text-emerald-300 text-sm shadow-lg backdrop-blur-sm">
          <Check size={14} />
          {toast}
        </div>
      )}
    </div>
  );
}

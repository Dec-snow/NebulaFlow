import { useEffect, useState } from "react";
import {
  Bot,
  Plus,
  Search,
  Activity,
  X,
  Pencil,
  Trash2,
  RefreshCw,
  Cpu,
  Server,
  Globe,
  CheckCircle2,
  AlertCircle,
  Power,
  Tag,
} from "lucide-react";
import { api } from "../api/client";
import type { Agent, AgentRuntimeType, AgentStatus } from "../types";

// 能力标签颜色映射
const capColors: Record<string, string> = {
  tool_call: "bg-blue-500/15 text-blue-300 border-blue-500/30",
  rag: "bg-purple-500/15 text-purple-300 border-purple-500/30",
  memory: "bg-amber-500/15 text-amber-300 border-amber-500/30",
  streaming: "bg-emerald-500/15 text-emerald-300 border-emerald-500/30",
  code: "bg-cyan-500/15 text-cyan-300 border-cyan-500/30",
  search: "bg-orange-500/15 text-orange-300 border-orange-500/30",
  multimodal: "bg-pink-500/15 text-pink-300 border-pink-500/30",
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

function StatusBadge({ status }: { status: AgentStatus }) {
  const map = {
    active: "bg-emerald-500/15 text-emerald-300 border-emerald-500/30",
    inactive: "bg-slate-500/15 text-slate-400 border-slate-500/30",
    error: "bg-red-500/15 text-red-300 border-red-500/30",
  };
  return (
    <span className={`text-[10px] px-1.5 py-0.5 rounded border ${map[status]}`}>
      {status}
    </span>
  );
}

export default function Agents() {
  const [agents, setAgents] = useState<Agent[]>([]);
  const [loading, setLoading] = useState(true);
  const [search, setSearch] = useState("");
  const [filterRuntime, setFilterRuntime] = useState("");
  const [filterCap, setFilterCap] = useState("");
  const [capOptions, setCapOptions] = useState<{ value: string; label: string }[]>([]);
  const [runtimeOptions, setRuntimeOptions] = useState<{ value: string; label: string }[]>([]);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<Agent | null>(null);
  const [healthMap, setHealthMap] = useState<Record<number, boolean | null>>({});

  const fetchAgents = () => {
    setLoading(true);
    api
      .listAgents({
        runtime_type: filterRuntime || undefined,
        capability: filterCap || undefined,
      })
      .then((r) => setAgents(r.agents))
      .catch(() => setAgents([]))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    fetchAgents();
    api.listCapabilities().then((r) => setCapOptions(r.capabilities)).catch(() => {});
    api.listRuntimeTypes().then((r) => setRuntimeOptions(r.runtime_types)).catch(() => {});
  }, [filterRuntime, filterCap]);

  const filtered = agents.filter((a) =>
    search
      ? a.name.toLowerCase().includes(search.toLowerCase()) ||
        a.description.toLowerCase().includes(search.toLowerCase())
      : true
  );

  const checkHealth = async (id: number) => {
    setHealthMap((m) => ({ ...m, [id]: null }));
    try {
      const h = await api.agentHealth(id);
      setHealthMap((m) => ({ ...m, [id]: h.healthy }));
    } catch {
      setHealthMap((m) => ({ ...m, [id]: false }));
    }
  };

  const handleDelete = async (id: number) => {
    if (!confirm("确认删除这个 Agent 吗？")) return;
    try {
      await api.deleteAgent(id);
      setAgents((prev) => prev.filter((a) => a.id !== id));
    } catch (e) {
      alert("删除失败");
    }
  };

  const openCreate = () => {
    setEditing(null);
    setModalOpen(true);
  };

  const openEdit = (a: Agent) => {
    setEditing(a);
    setModalOpen(true);
  };

  const handleSaved = (a: Agent) => {
    setModalOpen(false);
    if (editing) {
      setAgents((prev) => prev.map((x) => (x.id === a.id ? a : x)));
    } else {
      setAgents((prev) => [a, ...prev]);
    }
  };

  return (
    <div className="space-y-6">
      {/* 顶部 */}
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-xl font-semibold text-slate-100 flex items-center gap-2">
            <Bot size={20} className="text-nebula-400" />
            Agent Registry
          </h2>
          <p className="text-xs text-slate-500 mt-1">
            注册和管理你的 AI Agent，支持 Native 自研 Agent 和 LangChain 远程 Agent
          </p>
        </div>
        <button
          onClick={openCreate}
          className="flex items-center gap-1.5 px-3 py-2 bg-nebula-500 hover:bg-nebula-400 text-white text-sm rounded-lg transition-colors"
        >
          <Plus size={15} />
          新建 Agent
        </button>
      </div>

      {/* 筛选栏 */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="relative flex-1 min-w-[200px] max-w-sm">
          <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-slate-500" />
          <input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="搜索 Agent 名称 / 描述..."
            className="w-full pl-9 pr-3 py-2 bg-nebula-900 border border-nebula-800 rounded-lg text-sm text-slate-200 placeholder:text-slate-600 focus:outline-none focus:border-nebula-600"
          />
        </div>
        <select
          value={filterRuntime}
          onChange={(e) => setFilterRuntime(e.target.value)}
          className="px-3 py-2 bg-nebula-900 border border-nebula-800 rounded-lg text-sm text-slate-300 focus:outline-none focus:border-nebula-600"
        >
          <option value="">所有类型</option>
          {runtimeOptions.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <select
          value={filterCap}
          onChange={(e) => setFilterCap(e.target.value)}
          className="px-3 py-2 bg-nebula-900 border border-nebula-800 rounded-lg text-sm text-slate-300 focus:outline-none focus:border-nebula-600"
        >
          <option value="">所有能力</option>
          {capOptions.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <button
          onClick={fetchAgents}
          className="p-2 text-slate-400 hover:text-slate-200 hover:bg-nebula-800 rounded-lg transition-colors"
          title="刷新"
        >
          <RefreshCw size={15} className={loading ? "animate-spin" : ""} />
        </button>
      </div>

      {/* Agent 卡片网格 */}
      {loading ? (
        <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
          {[1, 2, 3].map((i) => (
            <div key={i} className="card animate-pulse">
              <div className="h-5 w-1/3 bg-nebula-800 rounded" />
              <div className="mt-4 h-4 w-full bg-nebula-800/60 rounded" />
              <div className="mt-2 h-4 w-2/3 bg-nebula-800/60 rounded" />
            </div>
          ))}
        </div>
      ) : filtered.length === 0 ? (
        <div className="card text-center py-12">
          <Bot size={32} className="mx-auto text-slate-600 mb-3" />
          <p className="text-sm text-slate-500">暂无 Agent</p>
          <button
            onClick={openCreate}
            className="mt-4 text-xs text-nebula-400 hover:text-nebula-300"
          >
            点击创建第一个 Agent
          </button>
        </div>
      ) : (
        <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
          {filtered.map((a) => {
            const Icon = runtimeIcons[a.runtime_type] || Bot;
            const healthy = healthMap[a.id];
            return (
              <div key={a.id} className="card group">
                {/* 卡片头 */}
                <div className="flex items-start justify-between">
                  <div className="flex items-center gap-2 min-w-0">
                    <div className="w-8 h-8 rounded-lg bg-nebula-800 flex items-center justify-center text-nebula-400 shrink-0">
                      <Icon size={16} />
                    </div>
                    <div className="min-w-0">
                      <div className="flex items-center gap-1.5">
                        <span className="font-medium text-slate-100 truncate">{a.name}</span>
                        <StatusBadge status={a.status} />
                      </div>
                      <p className="text-[10px] text-slate-500 mt-0.5">
                        {runtimeLabels[a.runtime_type]} · v{a.version}
                      </p>
                    </div>
                  </div>
                  <div className="flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity">
                    <button
                      onClick={() => checkHealth(a.id)}
                      className="p-1.5 text-slate-400 hover:text-emerald-400 hover:bg-nebula-800 rounded transition-colors"
                      title="健康检查"
                    >
                      <Activity size={13} />
                    </button>
                    <button
                      onClick={() => openEdit(a)}
                      className="p-1.5 text-slate-400 hover:text-nebula-300 hover:bg-nebula-800 rounded transition-colors"
                      title="编辑"
                    >
                      <Pencil size={13} />
                    </button>
                    <button
                      onClick={() => handleDelete(a.id)}
                      className="p-1.5 text-slate-400 hover:text-red-400 hover:bg-nebula-800 rounded transition-colors"
                      title="删除"
                    >
                      <Trash2 size={13} />
                    </button>
                  </div>
                </div>

                {/* 描述 */}
                <p className="mt-3 text-xs text-slate-400 line-clamp-2 min-h-[2rem]">
                  {a.description || "暂无描述"}
                </p>

                {/* 能力标签 */}
                <div className="mt-3 flex flex-wrap gap-1">
                  {a.capabilities?.slice(0, 4).map((c) => (
                    <span
                      key={c}
                      className={`text-[9px] px-1.5 py-0.5 rounded border ${
                        capColors[c] || "bg-slate-500/15 text-slate-400 border-slate-500/30"
                      }`}
                    >
                      {c}
                    </span>
                  ))}
                  {a.capabilities?.length > 4 && (
                    <span className="text-[9px] px-1.5 py-0.5 rounded border border-slate-600/30 text-slate-500">
                      +{a.capabilities.length - 4}
                    </span>
                  )}
                </div>

                {/* 底部信息 */}
                <div className="mt-3 pt-3 border-t border-nebula-800/50 flex items-center justify-between text-[10px] text-slate-500">
                  <div className="flex items-center gap-1">
                    <Tag size={10} />
                    <span>ID: {a.id}</span>
                  </div>
                  {healthy !== undefined && healthy !== null && (
                    <div className="flex items-center gap-1">
                      {healthy ? (
                        <>
                          <CheckCircle2 size={10} className="text-emerald-400" />
                          <span className="text-emerald-400">健康</span>
                        </>
                      ) : (
                        <>
                          <AlertCircle size={10} className="text-red-400" />
                          <span className="text-red-400">不可用</span>
                        </>
                      )}
                    </div>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}

      {/* 底部说明 */}
      <div className="card text-xs text-slate-500 leading-relaxed">
        <p className="font-medium text-slate-300 mb-2">动态路由</p>
        <pre className="bg-nebula-950 p-3 rounded-lg border border-nebula-800 text-slate-400 text-[11px]">
{`工作流节点 → Router 策略 → 注册中心 → 最合适的 Agent
              ↓
         SmartRouter: AgentID → 名称 → 能力+标签 → 健康优先 → 兜底
         RoundRobin: 轮询负载均衡

Multi-Agent Supervisor 通过 Router 为每个子任务动态分配 Agent。`}
        </pre>
      </div>

      {/* 新建/编辑弹窗 */}
      {modalOpen && (
        <AgentModal
          agent={editing}
          runtimeOptions={runtimeOptions}
          capOptions={capOptions}
          onClose={() => setModalOpen(false)}
          onSaved={handleSaved}
        />
      )}
    </div>
  );
}

// ---------- 新建/编辑 Agent 弹窗 ----------

interface AgentModalProps {
  agent: Agent | null;
  runtimeOptions: { value: string; label: string }[];
  capOptions: { value: string; label: string }[];
  onClose: () => void;
  onSaved: (a: Agent) => void;
}

function AgentModal({ agent, runtimeOptions, capOptions, onClose, onSaved }: AgentModalProps) {
  const [name, setName] = useState(agent?.name || "");
  const [description, setDescription] = useState(agent?.description || "");
  const [runtimeType, setRuntimeType] = useState<AgentRuntimeType>(
    (agent?.runtime_type as AgentRuntimeType) || "native"
  );
  const [endpoint, setEndpoint] = useState(agent?.endpoint || "");
  const [model, setModel] = useState(agent?.model || "");
  const [version, setVersion] = useState(agent?.version || "1.0.0");
  const [timeoutSec, setTimeoutSec] = useState(agent?.timeout_sec || 120);
  const [status, setStatus] = useState<AgentStatus>(agent?.status || "active");
  const [capabilities, setCapabilities] = useState<string[]>(agent?.capabilities || []);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  const toggleCap = (c: string) => {
    setCapabilities((prev) =>
      prev.includes(c) ? prev.filter((x) => x !== c) : [...prev, c]
    );
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    setSubmitting(true);
    try {
      const payload = {
        name,
        description,
        runtime_type: runtimeType,
        endpoint,
        model,
        version,
        timeout_sec: Number(timeoutSec) || 120,
        status,
        capabilities,
      };
      let result: Agent;
      if (agent) {
        result = await api.updateAgent(agent.id, payload);
      } else {
        result = await api.createAgent(payload as any);
      }
      onSaved(result);
    } catch (e: any) {
      setError(e?.message || "保存失败");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm p-4">
      <div className="w-full max-w-lg bg-nebula-900 border border-nebula-700 rounded-xl shadow-2xl max-h-[90vh] overflow-y-auto">
        <div className="flex items-center justify-between px-5 py-4 border-b border-nebula-800">
          <h3 className="text-base font-semibold text-slate-100">
            {agent ? "编辑 Agent" : "新建 Agent"}
          </h3>
          <button
            onClick={onClose}
            className="text-slate-400 hover:text-slate-200 transition-colors"
          >
            <X size={16} />
          </button>
        </div>

        <form onSubmit={handleSubmit} className="p-5 space-y-4">
          {/* 名称 */}
          <div>
            <label className="block text-xs text-slate-400 mb-1.5">Agent 名称 *</label>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              placeholder="my-awesome-agent"
              className="w-full px-3 py-2 bg-nebula-950 border border-nebula-800 rounded-lg text-sm text-slate-200 placeholder:text-slate-600 focus:outline-none focus:border-nebula-600"
            />
          </div>

          {/* 描述 */}
          <div>
            <label className="block text-xs text-slate-400 mb-1.5">描述</label>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={2}
              placeholder="这个 Agent 擅长什么..."
              className="w-full px-3 py-2 bg-nebula-950 border border-nebula-800 rounded-lg text-sm text-slate-200 placeholder:text-slate-600 focus:outline-none focus:border-nebula-600 resize-none"
            />
          </div>

          {/* Runtime 类型 + 版本 */}
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-xs text-slate-400 mb-1.5">Runtime 类型</label>
              <select
                value={runtimeType}
                onChange={(e) => setRuntimeType(e.target.value as AgentRuntimeType)}
                className="w-full px-3 py-2 bg-nebula-950 border border-nebula-800 rounded-lg text-sm text-slate-200 focus:outline-none focus:border-nebula-600"
              >
                {runtimeOptions.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </div>
            <div>
              <label className="block text-xs text-slate-400 mb-1.5">版本号</label>
              <input
                value={version}
                onChange={(e) => setVersion(e.target.value)}
                placeholder="1.0.0"
                className="w-full px-3 py-2 bg-nebula-950 border border-nebula-800 rounded-lg text-sm text-slate-200 placeholder:text-slate-600 focus:outline-none focus:border-nebula-600"
              />
            </div>
          </div>

          {/* Endpoint（非 native 才显示） */}
          {runtimeType !== "native" && (
            <div>
              <label className="block text-xs text-slate-400 mb-1.5">Endpoint URL *</label>
              <input
                value={endpoint}
                onChange={(e) => setEndpoint(e.target.value)}
                placeholder="http://langserve-agent:8000"
                className="w-full px-3 py-2 bg-nebula-950 border border-nebula-800 rounded-lg text-sm text-slate-200 placeholder:text-slate-600 focus:outline-none focus:border-nebula-600 font-mono"
              />
            </div>
          )}

          {/* Model + Timeout */}
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className="block text-xs text-slate-400 mb-1.5">默认模型</label>
              <input
                value={model}
                onChange={(e) => setModel(e.target.value)}
                placeholder="deepseek-chat"
                className="w-full px-3 py-2 bg-nebula-950 border border-nebula-800 rounded-lg text-sm text-slate-200 placeholder:text-slate-600 focus:outline-none focus:border-nebula-600"
              />
            </div>
            <div>
              <label className="block text-xs text-slate-400 mb-1.5">超时（秒）</label>
              <input
                type="number"
                value={timeoutSec}
                onChange={(e) => setTimeoutSec(Number(e.target.value))}
                min={1}
                className="w-full px-3 py-2 bg-nebula-950 border border-nebula-800 rounded-lg text-sm text-slate-200 focus:outline-none focus:border-nebula-600"
              />
            </div>
          </div>

          {/* 状态 */}
          <div>
            <label className="block text-xs text-slate-400 mb-1.5">状态</label>
            <div className="flex gap-2">
              {(["active", "inactive"] as const).map((s) => (
                <button
                  key={s}
                  type="button"
                  onClick={() => setStatus(s)}
                  className={`flex items-center gap-1.5 px-3 py-1.5 text-xs rounded-lg border transition-colors ${
                    status === s
                      ? "bg-nebula-600/30 border-nebula-500 text-nebula-200"
                      : "bg-nebula-950 border-nebula-800 text-slate-400 hover:border-nebula-700"
                  }`}
                >
                  <Power size={11} />
                  {s}
                </button>
              ))}
            </div>
          </div>

          {/* 能力标签 */}
          <div>
            <label className="block text-xs text-slate-400 mb-2">能力标签</label>
            <div className="flex flex-wrap gap-1.5">
              {capOptions.map((o) => (
                <button
                  key={o.value}
                  type="button"
                  onClick={() => toggleCap(o.value)}
                  className={`text-[10px] px-2 py-1 rounded-full border transition-colors ${
                    capabilities.includes(o.value)
                      ? capColors[o.value] || "bg-nebula-500/20 text-nebula-200 border-nebula-500/40"
                      : "bg-nebula-950 border-nebula-800 text-slate-500 hover:border-nebula-700"
                  }`}
                >
                  {o.label}
                </button>
              ))}
            </div>
          </div>

          {/* 错误提示 */}
          {error && (
            <div className="text-xs text-red-400 bg-red-500/10 border border-red-500/30 rounded-lg px-3 py-2">
              {error}
            </div>
          )}

          {/* 操作按钮 */}
          <div className="flex items-center justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={onClose}
              className="px-4 py-2 text-sm text-slate-400 hover:text-slate-200 transition-colors"
            >
              取消
            </button>
            <button
              type="submit"
              disabled={submitting || !name}
              className="px-4 py-2 bg-nebula-500 hover:bg-nebula-400 disabled:opacity-50 disabled:cursor-not-allowed text-white text-sm rounded-lg transition-colors"
            >
              {submitting ? "保存中..." : agent ? "保存修改" : "创建 Agent"}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

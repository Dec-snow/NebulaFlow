import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import {
  addEdge,
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  ReactFlow,
  useEdgesState,
  useNodesState,
  type Connection,
  type Edge,
  type Node,
} from "@xyflow/react";
import {
  Play,
  Save,
  X,
  Trash2,
  Copy,
  ChevronDown,
  ChevronRight,
  Sparkles,
  Variable,
  Settings,
  Zap,
  Info,
  CheckCheck,
} from "lucide-react";
import { api } from "../api/client";
import { nodeMeta, nodeTypes } from "../components/FlowNodes";
import type { KnowledgeBase, NodeConfig, NodeType, Provider, Workflow } from "../types";
import { computeUpstreamNodes } from "../utils/nodeUtils";

const PALETTE: NodeType[] = ["input", "llm", "rag", "tool", "output"];

interface FlowNodeData extends Record<string, unknown> {
  label: string;
  nodeType: NodeType;
  config: NodeConfig;
  onChange: (config: NodeConfig) => void;
  onConfigOpen: (key: string) => void;
}

export default function WorkflowEditor() {
  const { id } = useParams();
  const navigate = useNavigate();
  const isNew = id === "new";

  const [wf, setWf] = useState<Workflow | null>(null);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [nodes, setNodes, onNodesChange] = useNodesState<Node<FlowNodeData>>([]);
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>([]);
  const [providers, setProviders] = useState<Provider[]>([]);
  const [kbs, setKbs] = useState<KnowledgeBase[]>([]);
  const [configNodeKey, setConfigNodeKey] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState("");

  // 加载工作流 / 元数据
  useEffect(() => {
    api.listProviders().then((r) => setProviders(r.providers)).catch(() => {});
    api.listKBs().then((r) => setKbs(r.knowledge_bases)).catch(() => {});
    if (!isNew && id) {
      api.getWorkflow(Number(id)).then((w) => {
        setWf(w);
        setName(w.name);
        setDescription(w.description);
        setNodes(
          (w.nodes || []).map((n) => ({
            id: n.node_key,
            position: { x: n.position_x ?? n.x ?? 0, y: n.position_y ?? n.y ?? 0 },
            type: n.node_type,
            data: {
              label: n.node_key,
              nodeType: n.node_type,
              config: n.config || {},
              onChange: () => {},
              onConfigOpen: (key: string) => setConfigNodeKey(key),
            },
          }))
        );
        setEdges(
          (w.edges || []).map((e) => ({
            id: `${e.source_node}-${e.target_node}`,
            source: e.source_node,
            target: e.target_node,
            animated: true,
          }))
        );
      });
    }
  }, [id]);

  // 更新节点 data 的 onChange 绑定
  useEffect(() => {
    setNodes((nds) =>
      nds.map((n) => ({
        ...n,
        data: { ...n.data, onChange: (c: NodeConfig) => updateNodeConfig(n.id, c) },
      }))
    );
  }, []);

  const updateNodeConfig = useCallback(
    (nodeKey: string, cfg: NodeConfig) => {
      setNodes((nds) =>
        nds.map((n) =>
          n.id === nodeKey ? { ...n, data: { ...n.data, config: cfg } } : n
        )
      );
    },
    [setNodes]
  );

  const onConnect = useCallback(
    (conn: Connection) => setEdges((eds) => addEdge({ ...conn, animated: true }, eds)),
    [setEdges]
  );

  const addNode = useCallback(
    (type: NodeType) => {
      const key = `${type}${Date.now() % 10000}`;
      const node: Node<FlowNodeData> = {
        id: key,
        type,
        position: { x: 60 + Math.random() * 300, y: 80 + Math.random() * 200 },
        data: {
          label: key,
          nodeType: type,
          config: type === "llm" ? { model: "", max_retry: 2, timeout_sec: 90 } : {},
          onChange: (c) => updateNodeConfig(key, c),
          onConfigOpen: (k) => setConfigNodeKey(k),
        },
      };
      setNodes((nds) => [...nds, node]);
    },
    [setNodes, updateNodeConfig]
  );

  // 节点重命名
  const renameNode = useCallback(
    (oldKey: string, newKey: string) => {
      if (!newKey.trim() || newKey === oldKey) return;
      // 检查是否重名
      if (nodes.some((n) => n.id === newKey.trim())) {
        alert(`节点名称 "${newKey}" 已存在`);
        return;
      }
      const key = newKey.trim();
      setNodes((nds) =>
        nds.map((n) =>
          n.id === oldKey
            ? { ...n, id: key, data: { ...n.data, label: key, onChange: (c) => updateNodeConfig(key, c) } }
            : n
        )
      );
      // 同步更新边的 source/target
      setEdges((eds) =>
        eds.map((e) => ({
          ...e,
          id: e.id.replace(oldKey, key),
          source: e.source === oldKey ? key : e.source,
          target: e.target === oldKey ? key : e.target,
        }))
      );
      setConfigNodeKey(key);
    },
    [nodes, setNodes, setEdges, updateNodeConfig]
  );

  // 删除节点
  const deleteNode = useCallback(
    (key: string) => {
      if (!confirm(`确定删除节点 "${key}" 吗？`)) return;
      setNodes((nds) => nds.filter((n) => n.id !== key));
      setEdges((eds) => eds.filter((e) => e.source !== key && e.target !== key));
      setConfigNodeKey(null);
    },
    [setNodes, setEdges]
  );

  // 复制节点
  const duplicateNode = useCallback(
    (key: string) => {
      const node = nodes.find((n) => n.id === key);
      if (!node) return;
      const newKey = `${key}_copy`;
      const newNode: Node<FlowNodeData> = {
        ...node,
        id: newKey,
        position: { x: node.position.x + 40, y: node.position.y + 40 },
        selected: false,
        data: {
          ...node.data,
          label: newKey,
          onChange: (c) => updateNodeConfig(newKey, c),
          onConfigOpen: (k) => setConfigNodeKey(k),
        },
      };
      setNodes((nds) => [...nds, newNode]);
      setConfigNodeKey(newKey);
    },
    [nodes, setNodes, updateNodeConfig]
  );

  // 计算当前节点的所有上游节点（用于变量提示）
  const upstreamNodes = useMemo(() => {
    if (!configNodeKey) return [];
    // 将 React Flow 的 edge 格式转换为工具函数需要的格式
    const edgeLikes = edges.map((e) => ({ source_node: e.source, target_node: e.target }));
    return computeUpstreamNodes(edgeLikes, configNodeKey);
  }, [configNodeKey, edges]);

  // 可折叠面板分组状态
  const [collapsedSections, setCollapsedSections] = useState<Record<string, boolean>>({});
  const toggleSection = (key: string) => {
    setCollapsedSections((prev) => ({ ...prev, [key]: !prev[key] }));
  };

  // 折叠分组组件
  const Section = ({
    title,
    icon: Icon,
    defaultOpen = true,
    children,
  }: {
    title: string;
    icon?: React.ElementType;
    defaultOpen?: boolean;
    children: React.ReactNode;
  }) => {
    const isOpen = collapsedSections[title] !== undefined ? !collapsedSections[title] : defaultOpen;
    return (
      <div className="border-t border-nebula-800 -mx-5 first:border-t-0">
        <button
          onClick={() => toggleSection(title)}
          className="w-full flex items-center gap-2 px-5 py-2.5 text-xs font-medium text-slate-300 hover:bg-nebula-800/50 transition-colors"
        >
          {isOpen ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
          {Icon && <Icon size={12} className="text-nebula-400" />}
          {title}
        </button>
        {isOpen && <div className="px-5 pb-4 space-y-3">{children}</div>}
      </div>
    );
  };

  // LLM 快捷提示词模板
  const promptTemplates = [
    {
      name: "专业客服",
      system: "你是一位专业、耐心的客服代表，请用友好、专业的语气回答用户问题。",
      prompt: "请根据以下信息回答用户问题：\n{rag_output}\n\n用户问题：{input}",
    },
    {
      name: "代码助手",
      system: "你是一位资深软件工程师，擅长编写清晰、高效、可维护的代码。",
      prompt: "请帮我解决以下编程问题：\n{input}\n\n请提供完整的代码和解释。",
    },
    {
      name: "翻译官",
      system: "你是一位专业翻译，精通中英双语，翻译准确、自然。",
      prompt: "请将以下内容翻译成中文：\n{input}",
    },
    {
      name: "数据分析",
      system: "你是一位数据分析师，擅长从数据中提取洞察并给出建议。",
      prompt: "请分析以下数据并给出洞察：\n{input}",
    },
  ];

  // 插入变量到文本框
  const insertVariable = (varName: string, field: "prompt" | "system") => {
    const current = (cfg[field] as string) || "";
    setCfg({ [field]: current + `{${varName}}` });
  };

  // 保存：节点/边 → 后端 payload
  const save = async (): Promise<Workflow | null> => {
    setSaving(true);
    setError("");
    try {
      const payload = {
        name,
        description,
        status: "published",
        nodes: nodes.map((n) => ({
          key: n.id,
          type: (n.data as FlowNodeData).nodeType,
          config: (n.data as FlowNodeData).config || {},
          x: n.position.x,
          y: n.position.y,
        })),
        edges: edges.map((e) => ({ source: e.source, target: e.target })),
      };
      const saved = isNew
        ? await api.createWorkflow(payload)
        : await api.updateWorkflow(Number(id), payload);
      setWf(saved);
      return saved;
    } catch (err: any) {
      setError(err.message || "保存失败");
      return null;
    } finally {
      setSaving(false);
    }
  };

  const run = async () => {
    const saved = wf || (await save());
    if (!saved) return;
    setRunning(true);
    try {
      const task = await api.createTask(saved.id, "");
      navigate(`/tasks/${task.id}`);
    } catch (err: any) {
      setError(err.message || "运行失败");
    } finally {
      setRunning(false);
    }
  };

  const configNode = nodes.find((n) => n.id === configNodeKey);
  const configData = configNode?.data as FlowNodeData | undefined;
  const cfg = configData?.config || {};
  const setCfg = (patch: Partial<NodeConfig>) => {
    if (configNodeKey) updateNodeConfig(configNodeKey, { ...cfg, ...patch });
  };

  const modelOptions = useMemo(() => {
    const out: { provider: string; models: string[] }[] = [];
    for (const p of providers) {
      const models = (p.models || []).map((m) => m.name);
      if (models.length) out.push({ provider: p.name, models });
    }
    return out;
  }, [providers]);

  return (
    <div className="h-[calc(100vh-3rem)] flex flex-col">
      {/* 顶部工具条 */}
      <div className="flex items-center gap-3 pb-4">
        <input
          className="input max-w-xs font-medium"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="工作流名称"
        />
        <input
          className="input max-w-md text-xs"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder="描述（可选）"
        />
        <div className="flex-1" />
        <button className="btn-ghost" onClick={save} disabled={saving}>
          <Save size={14} /> {saving ? "保存中..." : "保存"}
        </button>
        <button className="btn-primary" onClick={run} disabled={running}>
          <Play size={14} /> {running ? "运行中..." : "Run Workflow"}
        </button>
      </div>
      {error && <p className="text-xs text-red-400 mb-2">{error}</p>}

      <div className="flex-1 flex gap-4 min-h-0">
        {/* 节点面板 */}
        <div className="w-40 shrink-0 space-y-2 overflow-y-auto">
          <p className="text-[10px] text-slate-500 uppercase tracking-wider px-1">节点库</p>
          {PALETTE.map((t) => {
            const meta = nodeMeta[t];
            const Icon = meta.icon;
            return (
              <button
                key={t}
                onClick={() => addNode(t)}
                className="w-full flex items-center gap-2.5 px-3 py-2.5 rounded-lg bg-nebula-900 border border-nebula-800 hover:border-nebula-500 text-left transition-colors"
              >
                <Icon size={14} style={{ color: meta.color }} />
                <div>
                  <div className="text-xs text-slate-200">{meta.label}</div>
                  <div className="text-[9px] text-slate-600">{meta.description}</div>
                </div>
              </button>
            );
          })}
        </div>

        {/* 画布 */}
        <div className="flex-1 rounded-xl border border-nebula-800 overflow-hidden bg-nebula-900/50 relative">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            nodeTypes={nodeTypes}
            fitView
            defaultEdgeOptions={{ animated: true, style: { strokeWidth: 1.5 } }}
          >
            <Background variant={BackgroundVariant.Dots} gap={24} size={1} color="#1a2440" />
            <Controls />
            <MiniMap
              className="bg-nebula-900"
              nodeColor={(n) => nodeMeta[(n.data as FlowNodeData).nodeType].color}
            />
          </ReactFlow>
          {nodes.length === 0 && (
            <div className="absolute inset-0 flex items-center justify-center pointer-events-none">
              <p className="text-sm text-slate-600">从左侧拖入节点，连接它们构成工作流</p>
            </div>
          )}
        </div>

        {/* 配置面板 */}
        {configNode && configData && (
          <div className="w-80 shrink-0 card overflow-y-auto p-0 flex flex-col">
            {/* 头部：节点信息 + 操作按钮 */}
            <div className="p-4 border-b border-nebula-800">
              <div className="flex items-center justify-between mb-3">
                <div className="flex items-center gap-2 min-w-0">
                  <div
                    className="w-8 h-8 rounded-lg flex items-center justify-center shrink-0"
                    style={{ backgroundColor: `${nodeMeta[configData.nodeType].color}20` }}
                  >
                    {(() => {
                      const Icon = nodeMeta[configData.nodeType].icon;
                      return <Icon size={14} style={{ color: nodeMeta[configData.nodeType].color }} />;
                    })()}
                  </div>
                  <div className="min-w-0">
                    <div className="text-xs font-semibold text-slate-200">
                      {nodeMeta[configData.nodeType].label} 节点
                    </div>
                    <div className="text-[10px] text-slate-500">
                      {nodeMeta[configData.nodeType].description}
                    </div>
                  </div>
                </div>
                <button
                  onClick={() => setConfigNodeKey(null)}
                  className="text-slate-500 hover:text-slate-300 p-1 transition-colors"
                >
                  <X size={14} />
                </button>
              </div>

              {/* 节点重命名 */}
              <div className="flex items-center gap-2">
                <input
                  type="text"
                  className="input text-xs flex-1"
                  value={configNode.id}
                  onChange={(e) => {
                    // 实时更新但不立即提交
                    const val = e.target.value;
                    if (val && !nodes.some((n) => n.id === val && n.id !== configNodeKey)) {
                      renameNode(configNodeKey!, val);
                    }
                  }}
                  onBlur={(e) => renameNode(configNodeKey!, e.target.value)}
                  placeholder="节点名称"
                />
              </div>

              {/* 操作按钮组 */}
              <div className="flex items-center gap-2 mt-3">
                <button
                  onClick={() => duplicateNode(configNodeKey!)}
                  className="flex-1 flex items-center justify-center gap-1.5 px-2 py-1.5 rounded-lg text-[11px] bg-nebula-800 hover:bg-nebula-700 text-slate-300 border border-nebula-700 transition-colors"
                >
                  <Copy size={11} /> 复制
                </button>
                <button
                  onClick={() => deleteNode(configNodeKey!)}
                  className="flex-1 flex items-center justify-center gap-1.5 px-2 py-1.5 rounded-lg text-[11px] bg-red-500/10 hover:bg-red-500/20 text-red-400 border border-red-500/20 transition-colors"
                >
                  <Trash2 size={11} /> 删除
                </button>
              </div>
            </div>

            {/* 配置内容区 */}
            <div className="flex-1 overflow-y-auto">
              {configData.nodeType === "input" && (
                <Section title="基本信息" icon={Info}>
                  <p className="text-xs text-slate-400 leading-relaxed">
                    输入节点是工作流的起点。任务运行时，用户的输入会通过此节点流入下游节点。
                  </p>
                  <div className="p-2.5 rounded-lg bg-nebula-950 border border-nebula-800">
                    <div className="text-[10px] text-slate-500 mb-1">节点输出变量</div>
                    <code className="text-[11px] text-nebula-300">{"{input}"}</code>
                  </div>
                </Section>
              )}

              {configData.nodeType === "llm" && (
                <>
                  <Section title="模型选择" icon={Zap}>
                    <div>
                      <label className="label">模型</label>
                      <select
                        className="input"
                        value={cfg.model || ""}
                        onChange={(e) => setCfg({ model: e.target.value })}
                      >
                        <option value="">选择模型…</option>
                        {modelOptions.map((g) => (
                          <optgroup key={g.provider} label={g.provider}>
                            {g.models.map((m) => (
                              <option key={m} value={m}>
                                {m}
                              </option>
                            ))}
                          </optgroup>
                        ))}
                        <option value="mock-chat">mock-chat（离线演示）</option>
                      </select>
                    </div>
                  </Section>

                  <Section title="提示词配置" icon={Sparkles}>
                    {/* 快捷模板 */}
                    <div>
                      <div className="flex items-center justify-between mb-1.5">
                        <label className="label !mb-0">快捷模板</label>
                        <span className="text-[9px] text-slate-600">点击填充</span>
                      </div>
                      <div className="flex flex-wrap gap-1.5">
                        {promptTemplates.map((t) => (
                          <button
                            key={t.name}
                            onClick={() => setCfg({ system: t.system, prompt: t.prompt })}
                            className="px-2 py-1 rounded-md text-[10px] bg-nebula-800 hover:bg-nebula-700 text-slate-300 border border-nebula-700 transition-colors"
                          >
                            {t.name}
                          </button>
                        ))}
                      </div>
                    </div>

                    <div>
                      <label className="label">System Prompt</label>
                      <textarea
                        className="input h-20 resize-none text-xs"
                        value={cfg.system || ""}
                        onChange={(e) => setCfg({ system: e.target.value })}
                        placeholder="角色设定…"
                      />
                    </div>

                    <div>
                      <div className="flex items-center justify-between">
                        <label className="label">Prompt</label>
                        <span className="text-[9px] text-slate-600">
                          {((cfg.prompt as string) || "").length} 字
                        </span>
                      </div>
                      <textarea
                        className="input h-28 resize-none text-xs"
                        value={cfg.prompt || ""}
                        onChange={(e) => setCfg({ prompt: e.target.value })}
                        placeholder="任务描述，可用 {node_key} 引用上游节点输出…"
                      />
                    </div>

                    {/* 变量提示 */}
                    {upstreamNodes.length > 0 && (
                      <div>
                        <div className="flex items-center gap-1.5 mb-1.5">
                          <Variable size={10} className="text-nebula-400" />
                          <span className="text-[10px] text-slate-400">可用变量（点击插入）</span>
                        </div>
                        <div className="flex flex-wrap gap-1.5">
                          {upstreamNodes.map((n) => (
                            <button
                              key={n}
                              onClick={() => insertVariable(n, "prompt")}
                              className="px-2 py-0.5 rounded text-[10px] bg-nebula-800 hover:bg-nebula-700 text-nebula-300 border border-nebula-700 font-mono transition-colors"
                              title={`插入 {${n}} 到 Prompt`}
                            >
                              {"{"}
                              {n}
                              {"}"}
                            </button>
                          ))}
                        </div>
                      </div>
                    )}
                  </Section>

                  <Section title="高级设置" icon={Settings} defaultOpen={false}>
                    <div className="grid grid-cols-2 gap-2">
                      <div>
                        <label className="label">重试次数</label>
                        <input
                          className="input text-xs"
                          type="number"
                          min={0}
                          max={5}
                          value={cfg.max_retry ?? 2}
                          onChange={(e) => setCfg({ max_retry: Number(e.target.value) })}
                        />
                      </div>
                      <div>
                        <label className="label">超时(秒)</label>
                        <input
                          className="input text-xs"
                          type="number"
                          min={5}
                          value={cfg.timeout_sec ?? 90}
                          onChange={(e) => setCfg({ timeout_sec: Number(e.target.value) })}
                        />
                      </div>
                    </div>
                    <p className="text-[10px] text-slate-600">
                      节点失败时自动重试，最多重试指定次数后判定失败。
                    </p>
                  </Section>
                </>
              )}

              {configData.nodeType === "rag" && (
                <>
                  <Section title="知识库配置" icon={Settings}>
                    <div>
                      <label className="label">选择知识库</label>
                      <select
                        className="input"
                        value={cfg.knowledge_base_id || ""}
                        onChange={(e) =>
                          setCfg({ knowledge_base_id: e.target.value ? Number(e.target.value) : undefined })
                        }
                      >
                        <option value="">选择知识库…</option>
                        {kbs.map((k) => (
                          <option key={k.id} value={k.id}>
                            {k.name}
                          </option>
                        ))}
                      </select>
                    </div>
                  </Section>

                  <Section title="说明" icon={Info} defaultOpen={false}>
                    <p className="text-xs text-slate-400 leading-relaxed">
                      RAG 节点会根据输入内容从知识库中检索 Top-5 最相关的文档片段，注入下游 LLM 节点。
                    </p>
                    <div className="p-2.5 rounded-lg bg-nebula-950 border border-nebula-800 mt-2">
                      <div className="text-[10px] text-slate-500 mb-1">节点输出变量</div>
                      <code className="text-[11px] text-emerald-300">{"{node_key}"}</code>
                      <p className="text-[10px] text-slate-500 mt-1">下游使用时替换 node_key 为当前节点名</p>
                    </div>
                  </Section>
                </>
              )}

              {configData.nodeType === "tool" && (
                <>
                  <Section title="工具选择" icon={Zap}>
                    <div>
                      <label className="label">工具</label>
                      <select
                        className="input"
                        value={cfg.tool || ""}
                        onChange={(e) => setCfg({ tool: e.target.value })}
                      >
                        <option value="">选择工具…</option>
                        <option value="calculator">calculator · 四则运算</option>
                        <option value="http_request">http_request · 抓取 URL</option>
                        <option value="time">time · 当前时间</option>
                        <option value="code_runner">code_runner · 统计计算</option>
                      </select>
                    </div>
                  </Section>

                  {upstreamNodes.length > 0 && (
                    <Section title="可用变量" icon={Variable} defaultOpen={false}>
                      <div className="flex flex-wrap gap-1.5">
                        {upstreamNodes.map((n) => (
                          <span
                            key={n}
                            className="px-2 py-0.5 rounded text-[10px] bg-nebula-800 text-nebula-300 border border-nebula-700 font-mono"
                          >
                            {"{"}
                            {n}
                            {"}"}
                          </span>
                        ))}
                      </div>
                      <p className="text-[10px] text-slate-600 mt-2">工具节点会自动接收上游节点输出作为输入。</p>
                    </Section>
                  )}
                </>
              )}

              {configData.nodeType === "output" && (
                <Section title="基本信息" icon={Info}>
                  <p className="text-xs text-slate-400 leading-relaxed">
                    输出节点是工作流的终点。所有上游节点的最终结果会在此聚合，作为任务的输出返回。
                  </p>
                  <div className="p-2.5 rounded-lg bg-nebula-950 border border-nebula-800 mt-2">
                    <div className="flex items-center gap-1.5 text-[10px] text-emerald-400 mb-1">
                      <CheckCheck size={11} /> 自动聚合
                    </div>
                    <p className="text-[10px] text-slate-500">
                      连接到输出节点的所有上游结果会自动合并。
                    </p>
                  </div>
                </Section>
              )}
            </div>

            {/* 底部：节点位置信息 */}
            <div className="px-4 py-2.5 border-t border-nebula-800 text-[10px] text-slate-600 flex items-center justify-between">
              <span>
                位置：({Math.round(configNode.position.x)}, {Math.round(configNode.position.y)})
              </span>
              <span>ID: {configNode.id.slice(0, 12)}…</span>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

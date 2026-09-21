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
import { Play, Save, X } from "lucide-react";
import { api } from "../api/client";
import { nodeMeta, nodeTypes } from "../components/FlowNodes";
import type { KnowledgeBase, NodeConfig, NodeType, Provider, Workflow } from "../types";

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
            id: n.key,
            position: { x: n.position_x ?? n.x ?? 0, y: n.position_y ?? n.y ?? 0 },
            type: n.type,
            data: {
              label: n.key,
              nodeType: n.type,
              config: n.config || {},
              onChange: () => {},
              onConfigOpen: (key: string) => setConfigNodeKey(key),
            },
          }))
        );
        setEdges(
          (w.edges || []).map((e) => ({
            id: `${e.source}-${e.target}`,
            source: e.source,
            target: e.target,
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
          <div className="w-72 shrink-0 card overflow-y-auto">
            <div className="flex items-center justify-between mb-4">
              <h3 className="text-sm font-medium text-slate-200">
                {nodeMeta[configData.nodeType].label} · {configNode.id}
              </h3>
              <button onClick={() => setConfigNodeKey(null)} className="text-slate-500 hover:text-slate-300">
                <X size={14} />
              </button>
            </div>

            {configData.nodeType === "input" && (
              <p className="text-xs text-slate-500">输入节点：任务运行时的用户输入会流入此处。</p>
            )}

            {configData.nodeType === "llm" && (
              <div className="space-y-3">
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
                <div>
                  <label className="label">System Prompt</label>
                  <textarea
                    className="input h-20 resize-none"
                    value={cfg.system || ""}
                    onChange={(e) => setCfg({ system: e.target.value })}
                    placeholder="角色设定…"
                  />
                </div>
                <div>
                  <label className="label">Prompt</label>
                  <textarea
                    className="input h-24 resize-none"
                    value={cfg.prompt || ""}
                    onChange={(e) => setCfg({ prompt: e.target.value })}
                    placeholder="任务描述，可用 {input} 引用上游…"
                  />
                </div>
                <div className="grid grid-cols-2 gap-2">
                  <div>
                    <label className="label">重试次数</label>
                    <input
                      className="input"
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
                      className="input"
                      type="number"
                      min={5}
                      value={cfg.timeout_sec ?? 90}
                      onChange={(e) => setCfg({ timeout_sec: Number(e.target.value) })}
                    />
                  </div>
                </div>
              </div>
            )}

            {configData.nodeType === "rag" && (
              <div className="space-y-3">
                <div>
                  <label className="label">知识库</label>
                  <select
                    className="input"
                    value={cfg.knowledge_base_id || ""}
                    onChange={(e) => setCfg({ knowledge_base_id: e.target.value ? Number(e.target.value) : undefined })}
                  >
                    <option value="">选择知识库…</option>
                    {kbs.map((k) => (
                      <option key={k.id} value={k.id}>
                        {k.name}
                      </option>
                    ))}
                  </select>
                </div>
                <p className="text-[10px] text-slate-600">RAG 节点会检索 Top-5 相关片段注入下游 LLM。</p>
              </div>
            )}

            {configData.nodeType === "tool" && (
              <div className="space-y-3">
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
              </div>
            )}

            {configData.nodeType === "output" && (
              <p className="text-xs text-slate-500">输出节点：工作流最终结果。</p>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

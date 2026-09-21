import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  addEdge,
  Background,
  BackgroundVariant,
  Controls,
  Handle,
  MiniMap,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useEdgesState,
  useNodesInitialized,
  useNodesState,
  useReactFlow,
  type Connection,
  type Edge,
  type Node,
  type NodeProps,
  type NodeTypes,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import {
  AlertTriangle,
  ChevronLeft,
  Database,
  GitBranch,
  LayoutGrid,
  LogIn,
  LogOut,
  Maximize2,
  Play,
  Save,
  Settings2,
  Sparkles,
  Trash2,
  Wrench,
  type LucideIcon,
} from "lucide-react";
import { Undo2, Redo2 } from "lucide-react";
import {
  Badge,
  Button,
  Field,
  IconButton,
  Input,
  Select,
  Textarea,
} from "@/components/ui";
import { EDITOR_EDGES, EDITOR_NODES, PROMPT_TEMPLATES, type EditorNode } from "@/lib/mock";
import { cn } from "@/lib/utils";

/* ------------------------------------------------------------------ 类型 */
type NodeKind = EditorNode["type"];

/** 节点携带的数据：类型 + 配置，与后端 workflow node 的 payload 同构。 */
interface WFData extends Record<string, unknown> {
  kind: NodeKind;
  config: EditorNode["config"];
}

type WFNode = Node<WFData, "wf">;

/* ------------------------------------------------------------ 节点元信息 */
interface NodeMeta {
  label: string;
  icon: LucideIcon;
  chip: string;
  ring: string;
  accent: string;
  /** MiniMap 中的填充色（SVG attribute 不支持 var()，所以给具体值） */
  map: string;
}

const NODE_META: Record<NodeKind, NodeMeta> = {
  input: {
    label: "输入",
    icon: LogIn,
    chip: "bg-mint/12 text-mint",
    ring: "ring-mint/40",
    accent: "bg-mint",
    map: "rgb(16 185 129 / 0.65)",
  },
  llm: {
    label: "LLM",
    icon: Sparkles,
    chip: "bg-brand-soft text-brand",
    ring: "ring-brand/45",
    accent: "bg-brand",
    map: "rgb(108 71 255 / 0.65)",
  },
  rag: {
    label: "RAG",
    icon: Database,
    chip: "bg-sky/12 text-sky",
    ring: "ring-sky/40",
    accent: "bg-sky",
    map: "rgb(14 165 233 / 0.65)",
  },
  tool: {
    label: "工具",
    icon: Wrench,
    chip: "bg-amber/14 text-amber",
    ring: "ring-amber/40",
    accent: "bg-amber",
    map: "rgb(245 158 11 / 0.65)",
  },
  condition: {
    label: "条件",
    icon: GitBranch,
    chip: "bg-rose/12 text-rose",
    ring: "ring-rose/40",
    accent: "bg-rose",
    map: "rgb(244 63 94 / 0.65)",
  },
  output: {
    label: "输出",
    icon: LogOut,
    chip: "bg-violet/12 text-violet",
    ring: "ring-violet/40",
    accent: "bg-violet",
    map: "rgb(139 92 246 / 0.65)",
  },
};

const PALETTE: { type: NodeKind; hint: string }[] = [
  { type: "input", hint: "接收用户输入" },
  { type: "llm", hint: "调用大模型生成" },
  { type: "rag", hint: "知识库检索增强" },
  { type: "condition", hint: "IF / ELSE 分支" },
  { type: "tool", hint: "计算 / HTTP / 时间" },
  { type: "output", hint: "汇总最终结果" },
];

/** 新节点的默认配置，避免拖进来就是空表单。 */
const DEFAULT_CONFIG: Record<NodeKind, EditorNode["config"]> = {
  input: {},
  llm: { model: "deepseek-v3", prompt: "", system: "", maxRetry: 2, timeoutSec: 60 },
  rag: { knowledgeBaseId: 1, prompt: "", maxRetry: 2, timeoutSec: 60 },
  condition: { expression: "{{input}} != ''" },
  tool: { tool: "time", maxRetry: 2, timeoutSec: 60 },
  output: {},
};

const DRAG_MIME = "application/nf-node";

/* ---------------------------------------------------------------- 工具函数 */
/** 节点卡片上显示的一行配置摘要。 */
function nodeSummary(kind: NodeKind, c: EditorNode["config"]): string {
  switch (kind) {
    case "llm":
      return c.model ?? "未选择模型";
    case "rag":
      return c.knowledgeBaseId ? `知识库 #${c.knowledgeBaseId}` : "未绑定知识库";
    case "tool":
      return c.tool ?? "未选择工具";
    case "condition":
      return c.expression ? c.expression.slice(0, 24) + (c.expression.length > 24 ? "…" : "") : "无条件";
    case "input":
      return "用户输入";
    case "output":
      return "最终输出";
  }
}

/**
 * 判断新增 source → target 这条边是否会形成环。
 *
 * 与后端调度引擎同一套语义：建工作流时会做拓扑排序，非法 DAG（如 A→B→C→A）
 * 直接拒绝。前端在连线当下就拦住，避免用户存了个永远跑不起来的图。
 * 做法：从 target 出发做 DFS，若能走回 source 说明成环。
 */
function createsCycle(edges: Edge[], source: string, target: string): boolean {
  if (source === target) return true;
  const adj = new Map<string, string[]>();
  for (const e of edges) {
    const list = adj.get(e.source);
    if (list) list.push(e.target);
    else adj.set(e.source, [e.target]);
  }
  const stack = [target];
  const seen = new Set<string>();
  while (stack.length) {
    const cur = stack.pop() as string;
    if (cur === source) return true;
    if (seen.has(cur)) continue;
    seen.add(cur);
    for (const next of adj.get(cur) ?? []) stack.push(next);
  }
  return false;
}

/** 生成不与现有节点冲突的 id（kind-2、kind-3 …）。 */
function nextNodeId(kind: NodeKind, taken: Set<string>): string {
  let i = 2;
  while (taken.has(`${kind}-${i}`)) i += 1;
  return `${kind}-${i}`;
}

/**
 * 分层自动布局：按拓扑层级决定 x，同层按出现顺序决定 y。
 * 只依赖入度，不需要额外图算法依赖，对 DAG 稳定可用。
 */
function autoLayout(nodes: WFNode[], edges: Edge[]): WFNode[] {
  const indeg = new Map<string, number>();
  const adj = new Map<string, string[]>();
  for (const n of nodes) {
    indeg.set(n.id, 0);
    adj.set(n.id, []);
  }
  for (const e of edges) {
    if (!indeg.has(e.target) || !adj.has(e.source)) continue;
    adj.get(e.source)?.push(e.target);
    indeg.set(e.target, (indeg.get(e.target) ?? 0) + 1);
  }

  const level = new Map<string, number>();
  let frontier = nodes.filter((n) => (indeg.get(n.id) ?? 0) === 0).map((n) => n.id);
  frontier.forEach((id) => level.set(id, 0));

  while (frontier.length) {
    const next: string[] = [];
    for (const id of frontier) {
      const lv = level.get(id) ?? 0;
      for (const t of adj.get(id) ?? []) {
        if ((level.get(t) ?? -1) < lv + 1) {
          level.set(t, lv + 1);
          next.push(t);
        }
      }
    }
    frontier = next;
  }

  const byLevel = new Map<number, string[]>();
  for (const n of nodes) {
    const lv = level.get(n.id) ?? 0;
    const bucket = byLevel.get(lv);
    if (bucket) bucket.push(n.id);
    else byLevel.set(lv, [n.id]);
  }

  // 与 mock 里的初始布局保持同一套间距，自动整理后观感一致
  const X0 = 20;
  const DX = 210;
  const Y0 = 20;
  const DY = 150;

  return nodes.map((n) => {
    const lv = level.get(n.id) ?? 0;
    const idx = byLevel.get(lv)?.indexOf(n.id) ?? 0;
    return { ...n, position: { x: X0 + lv * DX, y: Y0 + idx * DY } };
  });
}

/* -------------------------------------------------------------- 自定义节点 */
function WorkflowNodeView({ id, data, selected }: NodeProps<WFNode>) {
  const meta = NODE_META[data.kind];
  const Icon = meta.icon;

  return (
    <div
      className={cn(
        "w-[168px] rounded-xl border bg-surface px-3 py-2.5 shadow-sm",
        "transition-[box-shadow,border-color] duration-200 ease-smooth",
        selected
          ? cn("border-transparent ring-2 shadow-md", meta.ring)
          : "border-line hover:border-line-strong hover:shadow-md",
      )}
    >
      {/* 输入节点没有上游，不渲染 target 手柄 */}
      {data.kind !== "input" && (
        <Handle type="target" position={Position.Left} isConnectable />
      )}

      <div className="flex items-center gap-2.5">
        <span
          className={cn(
            "flex h-8 w-8 shrink-0 items-center justify-center rounded-lg",
            meta.chip,
          )}
        >
          <Icon size={15} />
        </span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5">
            <span className="truncate font-mono text-xs font-medium text-fg">{id}</span>
            <span className={cn("h-1.5 w-1.5 shrink-0 rounded-full", meta.accent)} />
          </span>
          <span className="mt-0.5 block truncate text-2xs text-fg-subtle">
            {nodeSummary(data.kind, data.config)}
          </span>
        </span>
      </div>

      {/* 输出节点没有下游 */}
      {data.kind !== "output" && data.kind !== "condition" && (
        <Handle type="source" position={Position.Right} isConnectable />
      )}

      {/* 条件节点：两个输出 handle（true / false） */}
      {data.kind === "condition" && (
        <>
          <Handle
            type="source"
            position={Position.Right}
            id="true"
            isConnectable
            style={{ top: "32%" }}
          />
          <Handle
            type="source"
            position={Position.Right}
            id="false"
            isConnectable
            style={{ top: "68%" }}
          />
          <span
            className="pointer-events-none absolute right-3 top-[24%] text-[10px] font-medium text-mint"
          >
            T
          </span>
          <span
            className="pointer-events-none absolute right-3 top-[60%] text-[10px] font-medium text-rose"
          >
            F
          </span>
        </>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ 主体 */
function EditorInner() {
  const { id } = useParams();
  const { screenToFlowPosition, deleteElements, fitView } = useReactFlow();
  const wrapRef = useRef<HTMLDivElement>(null);

  const initialNodes = useMemo<WFNode[]>(
    () =>
      EDITOR_NODES.map((n) => ({
        id: n.id,
        type: "wf" as const,
        position: { x: n.x, y: n.y },
        data: { kind: n.type, config: n.config },
      })),
    [],
  );

  const initialEdges = useMemo<Edge[]>(
    () =>
      EDITOR_EDGES.map(([source, target], i) => ({
        id: `e-${source}-${target}-${i}`,
        source,
        target,
        type: "smoothstep",
      })),
    [],
  );

  const [nodes, setNodes, onNodesChange] = useNodesState<WFNode>(initialNodes);
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>(initialEdges);
  const [selectedId, setSelectedId] = useState<string | null>("analyst");
  const [warning, setWarning] = useState<string | null>(null);

  /* ---------- 撤销 / 重做历史栈 ---------- */
  const historyRef = useRef<{ nodes: WFNode[]; edges: Edge[] }[]>([]);
  const historyIndexRef = useRef(-1);
  const skipHistoryRef = useRef(false);

  const pushHistory = useCallback((nds: WFNode[], eds: Edge[]) => {
    if (skipHistoryRef.current) return;
    const history = historyRef.current;
    const idx = historyIndexRef.current;
    // 截断 redo 分支
    history.length = idx + 1;
    history.push({ nodes: nds.map((n) => ({ ...n, position: { ...n.position } })), edges: eds.map((e) => ({ ...e })) });
    historyIndexRef.current = history.length - 1;
    // 最多保留 50 步
    if (history.length > 50) {
      history.shift();
      historyIndexRef.current = history.length - 1;
    }
  }, []);

  // 初始化历史栈
  useEffect(() => {
    pushHistory(initialNodes, initialEdges);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const canUndo = historyIndexRef.current > 0;
  const canRedo = historyIndexRef.current < historyRef.current.length - 1;

  const undo = useCallback(() => {
    const history = historyRef.current;
    const idx = historyIndexRef.current;
    if (idx <= 0) return;
    const prev = history[idx - 1];
    historyIndexRef.current = idx - 1;
    skipHistoryRef.current = true;
    setNodes(prev.nodes.map((n) => ({ ...n, position: { ...n.position } })));
    setEdges(prev.edges.map((e) => ({ ...e })));
    setTimeout(() => { skipHistoryRef.current = false; }, 0);
  }, [setNodes, setEdges]);

  const redo = useCallback(() => {
    const history = historyRef.current;
    const idx = historyIndexRef.current;
    if (idx >= history.length - 1) return;
    const next = history[idx + 1];
    historyIndexRef.current = idx + 1;
    skipHistoryRef.current = true;
    setNodes(next.nodes.map((n) => ({ ...n, position: { ...n.position } })));
    setEdges(next.edges.map((e) => ({ ...e })));
    setTimeout(() => { skipHistoryRef.current = false; }, 0);
  }, [setNodes, setEdges]);

  // 节点/边变化时记录历史（防抖：同一帧内的多次变化只记录一次）
  const historyTimerRef = useRef<number | null>(null);
  const scheduleHistory = useCallback(() => {
    if (skipHistoryRef.current) return;
    if (historyTimerRef.current) window.clearTimeout(historyTimerRef.current);
    historyTimerRef.current = window.setTimeout(() => {
      pushHistory(nodes, edges);
    }, 300);
  }, [nodes, edges, pushHistory]);

  useEffect(() => {
    scheduleHistory();
  }, [nodes, edges, scheduleHistory]);

  /* ---------- 快捷键 ---------- */
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement;
      // 输入框中不触发编辑类快捷键
      if (target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable)) {
        return;
      }

      const isMac = navigator.platform.toUpperCase().indexOf("MAC") >= 0;
      const mod = isMac ? e.metaKey : e.ctrlKey;

      // Backspace / Delete：删除选中节点
      if ((e.key === "Backspace" || e.key === "Delete") && selectedId) {
        e.preventDefault();
        removeSelected();
        return;
      }

      // Ctrl/Cmd + Z：撤销
      if (mod && e.key === "z" && !e.shiftKey) {
        e.preventDefault();
        undo();
        return;
      }

      // Ctrl/Cmd + Shift + Z 或 Ctrl/Cmd + Y：重做
      if (mod && (e.key === "y" || (e.key === "z" && e.shiftKey))) {
        e.preventDefault();
        redo();
        return;
      }

      // Ctrl/Cmd + S：保存
      if (mod && e.key === "s") {
        e.preventDefault();
        handleSave();
        return;
      }

      // Ctrl/Cmd + A：全选（不拦截，让 React Flow 自己处理）
    };

    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedId, undo, redo]);

  // React Flow 的 NodeTypes 要求 ComponentType<NodeProps>（data 为 Record<string, unknown>），
  // 而本组件是 NodeProps<WFNode>（data 更具体）。函数参数逆变导致二者不兼容，
  // 所以这里断言一次；data 的实际形状由 initialNodes / onDrop 的构造处保证。
  const nodeTypes = useMemo(
    () => ({ wf: WorkflowNodeView }) as unknown as NodeTypes,
    [],
  );

  const current = nodes.find((n) => n.id === selectedId);

  // fitView 属性在首帧执行时，右侧属性面板（xl 断点）可能尚未完成布局，
  // 容器宽度偏小会让算出的缩放过于保守、图形两侧留白过多。
  // 等节点测量完成后补一次 fitView，拿到的是稳定的容器尺寸。
  const nodesInitialized = useNodesInitialized();
  useEffect(() => {
    if (nodesInitialized) void fitView({ padding: 0.12, maxZoom: 1 });
  }, [nodesInitialized, fitView]);

  const flashWarning = useCallback((msg: string) => {
    setWarning(msg);
    window.setTimeout(() => setWarning(null), 3400);
  }, []);

  /* ---------- 连线（含环检测） ---------- */
  const onConnect = useCallback(
    (c: Connection) => {
      if (!c.source || !c.target) return;
      if (createsCycle(edges, c.source, c.target)) {
        flashWarning(
          c.source === c.target
            ? `拒绝连线：${c.source} 不能连接到自己。`
            : `拒绝连线：${c.source} → ${c.target} 会形成环，DAG 必须无环。`,
        );
        return;
      }
      setEdges((eds) =>
        addEdge(
          { ...c, id: `e-${c.source}-${c.target}-${Date.now()}`, type: "smoothstep" },
          eds,
        ),
      );
    },
    [edges, setEdges, flashWarning],
  );

  /* ---------- 从左侧节点库拖入创建 ---------- */
  const onDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
  }, []);

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      const kind = e.dataTransfer.getData(DRAG_MIME) as NodeKind;
      if (!kind || !NODE_META[kind]) return;
      const pos = screenToFlowPosition({ x: e.clientX, y: e.clientY });
      setNodes((nds) => {
        const taken = new Set(nds.map((n) => n.id));
        return nds.concat({
          id: nextNodeId(kind, taken),
          type: "wf",
          // 让光标落在节点中心，而不是左上角
          position: { x: pos.x - 84, y: pos.y - 32 },
          data: { kind, config: { ...DEFAULT_CONFIG[kind] } },
        });
      });
    },
    [screenToFlowPosition, setNodes],
  );

  /* ---------- 属性编辑 ---------- */
  const patch = (key: string, part: Partial<EditorNode["config"]>) => {
    setNodes((nds) =>
      nds.map((n) =>
        n.id === key
          ? { ...n, data: { ...n.data, config: { ...n.data.config, ...part } } }
          : n,
      ),
    );
  };

  /* ---------- 上游节点变量 ---------- */
  const upstreamNodes = useMemo(() => {
    if (!selectedId) return [];
    const upstream: string[] = [];
    const visited = new Set<string>();
    const stack = [selectedId];
    while (stack.length) {
      const cur = stack.pop()!;
      for (const e of edges) {
        if (e.target === cur && !visited.has(e.source)) {
          visited.add(e.source);
          upstream.push(e.source);
          stack.push(e.source);
        }
      }
    }
    return upstream
      .map((id) => nodes.find((n) => n.id === id))
      .filter((n): n is WFNode => !!n);
  }, [selectedId, edges, nodes]);

  const insertVar = (nodeId: string, field: "prompt" | "system", variable: string) => {
    const current = nodes.find((n) => n.id === nodeId);
    if (!current) return;
    const val = current.data.config[field] ?? "";
    patch(nodeId, { [field]: `${val}${val ? " " : ""}{{${variable}}}` });
  };

  const removeSelected = () => {
    if (!selectedId) return;
    // deleteElements 会连带清理挂在它上面的边
    void deleteElements({ nodes: [{ id: selectedId }] });
    setSelectedId(null);
  };

  const handleSave = () => {
    setWarning(null);
    // 模拟保存
    window.setTimeout(() => {
      flashWarning("已保存");
    }, 100);
  };

  const toggleFullscreen = () => {
    const el = wrapRef.current;
    if (!el) return;
    if (document.fullscreenElement) void document.exitFullscreen();
    else void el.requestFullscreen();
  };

  return (
    <div
      ref={wrapRef}
      className="-mx-6 -my-6 flex h-[calc(100vh-4rem)] flex-col bg-bg"
    >
      {/* 工具栏 */}
      <div className="flex h-14 shrink-0 items-center gap-3 border-b border-line bg-surface/50 px-5">
        <Link
          to="/workflows"
          className="flex items-center gap-1 text-xs text-fg-muted transition-colors hover:text-fg"
        >
          <ChevronLeft size={15} />
          返回
        </Link>
        <div className="h-5 w-px bg-line" />
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="truncate text-sm font-semibold text-fg">技术分析工作流</span>
            <Badge tone="mint">已发布</Badge>
          </div>
          <div className="tnum text-2xs text-fg-subtle">
            #{id ?? 1} · {nodes.length} 节点 · {edges.length} 连线
          </div>
        </div>

        <div className="ml-auto flex items-center gap-2">
          <div className="mr-2 flex items-center gap-1 border-r border-line pr-2">
            <IconButton
              label="撤销 (Ctrl+Z)"
              size="sm"
              onClick={undo}
              disabled={!canUndo}
            >
              <Undo2 size={14} />
            </IconButton>
            <IconButton
              label="重做 (Ctrl+Shift+Z)"
              size="sm"
              onClick={redo}
              disabled={!canRedo}
            >
              <Redo2 size={14} />
            </IconButton>
          </div>
          <Badge tone="neutral">已自动保存</Badge>
          <Button variant="outline" size="sm" icon={<Save size={13} />} onClick={handleSave}>
            保存
          </Button>
          <Button variant="brand" size="sm" icon={<Play size={13} />}>
            运行
          </Button>
        </div>
      </div>

      <div className="flex min-h-0 flex-1">
        {/* 左：节点库 */}
        <div className="scrollbar-none hidden w-[188px] shrink-0 overflow-y-auto border-r border-line bg-surface/40 p-3 lg:block">
          <div className="mb-3 px-1 text-2xs font-semibold uppercase tracking-widest text-fg-subtle">
            节点库
          </div>
          <div className="space-y-1.5">
            {PALETTE.map(({ type, hint }) => {
              const meta = NODE_META[type];
              const Icon = meta.icon;
              return (
                <button
                  key={type}
                  draggable
                  onDragStart={(e) => {
                    e.dataTransfer.setData(DRAG_MIME, type);
                    e.dataTransfer.effectAllowed = "move";
                  }}
                  className={cn(
                    "group flex w-full cursor-grab items-center gap-2.5 rounded-lg border border-line",
                    "bg-surface px-3 py-2.5 text-left transition-all duration-150 ease-smooth",
                    "hover:-translate-y-px hover:border-line-strong hover:shadow-sm",
                    "active:cursor-grabbing",
                  )}
                >
                  <span
                    className={cn(
                      "flex h-7 w-7 shrink-0 items-center justify-center rounded-md",
                      meta.chip,
                    )}
                  >
                    <Icon size={13.5} />
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="block text-xs font-medium text-fg">{meta.label}</span>
                    <span className="block truncate text-2xs text-fg-subtle">{hint}</span>
                  </span>
                </button>
              );
            })}
          </div>

          <p className="mt-3 px-1 text-2xs leading-relaxed text-fg-subtle">
            拖拽到画布即可新增节点
          </p>

          <div className="divider my-4" />

          <div className="mb-2 px-1 text-2xs font-semibold uppercase tracking-widest text-fg-subtle">
            画布操作
          </div>
          <div className="space-y-0.5">
            <button
              onClick={() => void fitView({ padding: 0.2, duration: 300 })}
              className="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-xs text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg"
            >
              <LayoutGrid size={14} />
              适应画布
            </button>
            <button
              onClick={() => setNodes((nds) => autoLayout(nds, edges))}
              className="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-xs text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg"
            >
              <Settings2 size={14} />
              自动整理
            </button>
            <button
              onClick={toggleFullscreen}
              className="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-xs text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg"
            >
              <Maximize2 size={14} />
              全屏
            </button>
          </div>
        </div>

        {/* 中：画布 */}
        <div className="relative min-w-0 flex-1" onDrop={onDrop} onDragOver={onDragOver}>
          <ReactFlow<WFNode, Edge>
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            nodeTypes={nodeTypes}
            onNodeClick={(_, n) => setSelectedId(n.id)}
            onPaneClick={() => setSelectedId(null)}
            onNodesDelete={() => setSelectedId(null)}
            fitView
            fitViewOptions={{ padding: 0.12, maxZoom: 1 }}
            minZoom={0.3}
            maxZoom={1.8}
            deleteKeyCode={["Backspace", "Delete"]}
            proOptions={{ hideAttribution: true }}
            defaultEdgeOptions={{ type: "smoothstep" }}
            className="bg-surface-2/40"
          >
            {/* color="currentColor" 是关键：SVG attribute 不解析 var()，
                但 currentColor 会取 CSS 的 color，而 color 可以用变量（见 index.css） */}
            <Background
              variant={BackgroundVariant.Dots}
              gap={18}
              size={1.4}
              color="currentColor"
            />
            <Controls showInteractive={false} position="bottom-left" />
            <MiniMap
              position="bottom-right"
              pannable
              zoomable
              nodeStrokeWidth={0}
              nodeColor={(n) => NODE_META[(n.data as WFData).kind]?.map ?? "#8b8b9a"}
            />
          </ReactFlow>

          {/* 环检测提示 */}
          {warning && (
            <div
              className={cn(
                "glass absolute left-1/2 top-4 z-20 flex -translate-x-1/2 items-center gap-2.5",
                "rounded-lg border border-rose/40 px-3.5 py-2.5 shadow-md animate-fade-up",
              )}
              role="alert"
            >
              <AlertTriangle size={15} className="shrink-0 text-rose" />
              <span className="text-xs text-fg">{warning}</span>
            </div>
          )}
        </div>

        {/* 右：属性检查器 */}
        <div className="scrollbar-none hidden w-[288px] shrink-0 overflow-y-auto border-l border-line bg-surface/40 p-4 xl:block">
          {!current ? (
            <div className="mt-10 text-center">
              <div className="mx-auto mb-3 flex h-10 w-10 items-center justify-center rounded-xl bg-surface-2 text-fg-subtle">
                <Settings2 size={17} />
              </div>
              <p className="text-xs text-fg-subtle">选择一个节点查看配置</p>
            </div>
          ) : (
            <div className="animate-fade-in space-y-4">
              <div className="flex items-center gap-2.5">
                <span
                  className={cn(
                    "flex h-8 w-8 items-center justify-center rounded-lg",
                    NODE_META[current.data.kind].chip,
                  )}
                >
                  {(() => {
                    const Icon = NODE_META[current.data.kind].icon;
                    return <Icon size={15} />;
                  })()}
                </span>
                <div className="min-w-0">
                  <div className="truncate font-mono text-xs font-medium text-fg">
                    {current.id}
                  </div>
                  <div className="text-2xs text-fg-subtle">
                    {NODE_META[current.data.kind].label} 节点
                  </div>
                </div>
                <IconButton
                  label="删除节点"
                  variant="ghost"
                  className="ml-auto text-fg-subtle hover:bg-rose/10 hover:text-rose"
                  onClick={removeSelected}
                >
                  <Trash2 size={14} />
                </IconButton>
              </div>

              <div className="divider" />

              {current.data.kind === "llm" && (
                <>
                  <Field label="模型">
                    <Select
                      value={current.data.config.model ?? ""}
                      onChange={(e) => patch(current.id, { model: e.target.value })}
                    >
                      <option value="deepseek-v3">deepseek-v3</option>
                      <option value="deepseek-r1">deepseek-r1</option>
                      <option value="gpt-4o">gpt-4o</option>
                      <option value="gpt-4o-mini">gpt-4o-mini</option>
                      <option value="glm-4-plus">glm-4-plus</option>
                      <option value="claude-3.5-sonnet">claude-3.5-sonnet</option>
                    </Select>
                  </Field>

                  <Field label="Prompt 模板" hint="选择模板快速填充">
                    <select
                      className="select w-full"
                      value=""
                      onChange={(e) => {
                        const tpl = PROMPT_TEMPLATES.find((t) => t.id === e.target.value);
                        if (!tpl) return;
                        patch(current.id, {
                          prompt: tpl.prompt,
                          system: tpl.system ?? "",
                        });
                        // 重置 select 显示
                        e.target.value = "";
                      }}
                    >
                      <option value="" disabled>
                        选择模板…
                      </option>
                      {(["通用", "分析", "写作", "翻译", "代码"] as const).map((cat) => (
                        <optgroup key={cat} label={cat}>
                          {PROMPT_TEMPLATES.filter((t) => t.category === cat).map((t) => (
                            <option key={t.id} value={t.id}>
                              {t.name}
                            </option>
                          ))}
                        </optgroup>
                      ))}
                    </select>
                  </Field>

                  <Field label="System 提示词">
                    <Textarea
                      rows={4}
                      value={current.data.config.system ?? ""}
                      onChange={(e) => patch(current.id, { system: e.target.value })}
                      placeholder="定义模型角色与约束…"
                    />
                  </Field>
                  <Field label="用户提示词">
                    <Textarea
                      rows={3}
                      value={current.data.config.prompt ?? ""}
                      onChange={(e) => patch(current.id, { prompt: e.target.value })}
                      placeholder="支持 {{input}} 引用上游输出"
                    />
                  </Field>

                  <div>
                    <div className="mb-1.5 text-2xs font-medium text-fg-subtle">
                      可用变量
                      <span className="ml-1 text-fg-muted">（点击插入）</span>
                    </div>
                    <div className="flex flex-wrap gap-1.5">
                      {upstreamNodes.length === 0 ? (
                        <span className="text-2xs text-fg-muted">暂无上游节点</span>
                      ) : (
                        upstreamNodes.map((n) => {
                          const vars =
                            n.data.kind === "input"
                              ? ["input"]
                              : n.data.kind === "rag"
                                ? ["output", "context"]
                                : ["output"];
                          return vars.map((v) => (
                            <button
                              key={`${n.id}-${v}`}
                              onClick={() => insertVar(current.id, "prompt", `${n.id}.${v}`)}
                              className="rounded-md border border-line bg-surface px-2 py-1 font-mono text-[11px] text-fg-subtle transition-colors hover:border-brand/40 hover:bg-brand/5 hover:text-brand"
                            >
                              {`{{${n.id}.${v}}}`}
                            </button>
                          ));
                        })
                      )}
                    </div>
                  </div>
                </>
              )}

              {current.data.kind === "rag" && (
                <>
                  <Field label="知识库">
                    <Select
                      value={String(current.data.config.knowledgeBaseId ?? "")}
                      onChange={(e) =>
                        patch(current.id, { knowledgeBaseId: Number(e.target.value) })
                      }
                    >
                      <option value="1">平台技术文档</option>
                      <option value="2">产品需求库</option>
                    </Select>
                  </Field>
                  <Field label="检索提示词" hint="用于改写 query，提升召回质量">
                    <Textarea
                      rows={3}
                      value={current.data.config.prompt ?? ""}
                      onChange={(e) => patch(current.id, { prompt: e.target.value })}
                    />
                  </Field>
                </>
              )}

              {current.data.kind === "condition" && (
                <>
                  <Field label="条件表达式" hint="支持 {{变量}} 引用，返回 true/false">
                    <Textarea
                      rows={3}
                      value={current.data.config.expression ?? ""}
                      onChange={(e) => patch(current.id, { expression: e.target.value })}
                      placeholder="{{input.sentiment}} == 'positive'"
                    />
                  </Field>
                  <div>
                    <div className="mb-1.5 text-2xs font-medium text-fg-subtle">
                      可用变量
                    </div>
                    <div className="flex flex-wrap gap-1.5">
                      {upstreamNodes.length === 0 ? (
                        <span className="text-2xs text-fg-muted">暂无上游节点</span>
                      ) : (
                        upstreamNodes.map((n) => (
                          <button
                            key={n.id}
                            onClick={() =>
                              patch(current.id, {
                                expression: `${current.data.config.expression ?? ""}{{${n.id}.output}}`,
                              })
                            }
                            className="rounded-md border border-line bg-surface px-2 py-1 font-mono text-[11px] text-fg-subtle transition-colors hover:border-brand/40 hover:bg-brand/5 hover:text-brand"
                          >
                            {`{{${n.id}.output}}`}
                          </button>
                        ))
                      )}
                    </div>
                  </div>
                  <div className="rounded-lg bg-amber/5 p-3 text-2xs leading-relaxed text-amber">
                    <strong>分支语义</strong>
                    <div className="mt-1 space-y-0.5 text-fg-subtle">
                      <div>· T 端口：表达式为 true 时走该分支</div>
                      <div>· F 端口：表达式为 false 时走该分支</div>
                    </div>
                  </div>
                </>
              )}

              {current.data.kind === "tool" && (
                <Field label="工具">
                  <Select
                    value={current.data.config.tool ?? ""}
                    onChange={(e) => patch(current.id, { tool: e.target.value })}
                  >
                    <option value="calculator">Calculator</option>
                    <option value="http">HTTP 请求</option>
                    <option value="time">时间</option>
                    <option value="code">CodeRunner</option>
                  </Select>
                </Field>
              )}

              {(current.data.kind === "input" || current.data.kind === "output") && (
                <p className="text-xs leading-relaxed text-fg-subtle">
                  {current.data.kind === "input"
                    ? "输入节点接收任务提交时的 input 字段，无额外配置。"
                    : "输出节点收集全部上游结果作为任务最终输出。"}
                </p>
              )}

              <div className="divider" />

              <div className="grid grid-cols-2 gap-3">
                <Field label="最大重试">
                  <Input
                    type="number"
                    value={current.data.config.maxRetry ?? 2}
                    onChange={(e) => patch(current.id, { maxRetry: Number(e.target.value) })}
                  />
                </Field>
                <Field label="超时（秒）">
                  <Input
                    type="number"
                    value={current.data.config.timeoutSec ?? 60}
                    onChange={(e) =>
                      patch(current.id, { timeoutSec: Number(e.target.value) })
                    }
                  />
                </Field>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

/** useReactFlow 必须在 ReactFlowProvider 内部使用，所以外面包一层。 */
export default function WorkflowEditor() {
  return (
    <ReactFlowProvider>
      <EditorInner />
    </ReactFlowProvider>
  );
}

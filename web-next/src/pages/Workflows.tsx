import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import {
  GitBranch,
  Plus,
  Search,
  Trash2,
} from "lucide-react";
import {
  Badge,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  ErrorState,
  Field,
  IconButton,
  Input,
  PageLoader,
  Segmented,
  StatusBadge,
  ToastContainer,
  useToast,
} from "@/components/ui";
import { api } from "@/api";
import { useDebounce, useFetch } from "@/hooks";
import type { WorkflowSummary } from "@/types";
import { cn, numberOrNull, timeAgo } from "@/lib/utils";

/** 每页条数：与 api 层"为列表补详情"的上限（24）对齐，避免拉不到的条目显示空统计 */
const PAGE_SIZE = 24;

/** 迷你 DAG 缩略图：把节点相对坐标映射到小视口，仅表达拓扑形状。 */
function MiniDag({ wf }: { wf: WorkflowSummary }) {
  // 列表接口本身不下发 nodes/edges；api 层会为当前页补一次详情，
  // 因此这里拿到的是真实拓扑（归一化坐标 + 真实连线），拉不到时给占位提示。
  const mini = Array.isArray(wf.mini) ? wf.mini : [];
  const miniEdges = Array.isArray(wf.miniEdges) ? wf.miniEdges : [];

  const pos = useMemo(() => {
    const m = new Map<string, { x: number; y: number }>();
    mini.forEach((n) => m.set(n.id, { x: 7 + (n.x / 100) * 86, y: 9 + (n.y / 100) * 42 }));
    return m;
  }, [mini]);

  // 没有拓扑数据时给占位，而不是渲染一个永远空白的框
  if (mini.length === 0) {
    return (
      <div className="dot-grid flex h-[104px] items-center justify-center rounded-lg border border-line bg-surface-2/50">
        <span className="text-2xs text-fg-subtle">打开详情查看拓扑</span>
      </div>
    );
  }

  const first = mini[0]?.id;
  const last = mini[mini.length - 1]?.id;

  return (
    <div className="dot-grid rounded-lg border border-line bg-surface-2/50">
      <svg viewBox="0 0 100 60" className="h-[104px] w-full" aria-hidden>
        {/* 连线 */}
        {miniEdges.map((edge, i) => {
          // 边的形状由接口决定：不是二元组时跳过，避免解构抛 TypeError
          if (!Array.isArray(edge)) return null;
          const [a, b] = edge;
          const p = pos.get(a);
          const q = pos.get(b);
          if (!p || !q) return null;
          const mx = (p.x + q.x) / 2;
          return (
            <path
              key={i}
              d={`M ${p.x} ${p.y} C ${mx} ${p.y}, ${mx} ${q.y}, ${q.x} ${q.y}`}
              fill="none"
              stroke="rgb(var(--c-brand) / 0.38)"
              strokeWidth={1.25}
              strokeLinecap="round"
            />
          );
        })}
        {/* 节点 */}
        {mini.map((n) => {
          const p = pos.get(n.id)!;
          const isEnd = n.id === first || n.id === last;
          return (
            <g key={n.id}>
              <circle
                cx={p.x}
                cy={p.y}
                r={isEnd ? 3.4 : 2.8}
                fill={isEnd ? "rgb(var(--c-brand))" : "rgb(var(--c-surface))"}
                stroke="rgb(var(--c-brand))"
                strokeWidth={1.5}
              />
            </g>
          );
        })}
      </svg>
    </div>
  );
}

function WorkflowCard({ wf, onDelete }: { wf: WorkflowSummary; onDelete: (wf: WorkflowSummary) => void }) {
  // 统计字段由 api 层补详情后写入；拉不到详情时保持 undefined →
  // 隐藏对应 UI，而不是渲染 "undefined 节点" / "NaN%"。
  const nodeCount = numberOrNull(wf.nodeCount);
  const edgeCount = numberOrNull(wf.edgeCount);

  return (
    <Link to={`/workflows/${wf.id}`} className="group block">
      <Card hover className="flex h-full flex-col">
        <div className="mb-3.5 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="truncate text-sm font-semibold text-fg">{wf.name}</h3>
            <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-fg-subtle">
              {wf.description || "暂无描述"}
            </p>
          </div>
          <div className="shrink-0 opacity-0 transition-opacity group-hover:opacity-100">
            <IconButton
              label="删除工作流"
              variant="ghost"
              onClick={(e) => {
                e.preventDefault();
                e.stopPropagation();
                onDelete(wf);
              }}
            >
              <Trash2 size={14} className="text-rose" />
            </IconButton>
          </div>
        </div>

        <MiniDag wf={wf} />

        <div className="mt-4 flex items-center justify-between">
          <StatusBadge status={wf.status} />
          <div className="flex items-center gap-3 text-2xs text-fg-subtle">
            {nodeCount !== null && <span className="tnum">{nodeCount} 节点</span>}
            {edgeCount !== null && <span className="tnum">{edgeCount} 连线</span>}
            {nodeCount === null && edgeCount === null && <span>拓扑未随列表下发</span>}
          </div>
        </div>

        <div className="divider my-3.5" />

        <div className="flex items-center justify-between text-2xs">
          {/* 后端没有 runs / success_rate：不再显示"运行数未知"这类伪字段，
              只展示真实存在的更新时间。 */}
          <span className="text-fg-subtle">
            {wf.isTemplate ? "模板" : "自有工作流"}
          </span>
          <span className="text-fg-subtle">更新于 {timeAgo(wf.updatedAt)}</span>
        </div>
      </Card>
    </Link>
  );
}

export default function Workflows() {
  const navigate = useNavigate();
  const [filter, setFilter] = useState<"all" | "published" | "draft">("all");
  const [q, setQ] = useState("");
  const debouncedQ = useDebounce(q, 300);
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [newDesc, setNewDesc] = useState("");
  const [busy, setBusy] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<WorkflowSummary | null>(null);
  const { toasts, toast } = useToast();

  const { data, loading, error, refresh } = useFetch(
    () =>
      api.workflows.list({
        status: filter === "all" ? undefined : filter,
        q: debouncedQ || undefined,
        pageSize: PAGE_SIZE,
      }),
    [filter, debouncedQ],
  );

  // 列表接口不下发 items/total 时才可能是 undefined：统一收成数组/数字
  const list = Array.isArray(data?.items) ? data.items : [];
  const total = numberOrNull(data?.total) ?? 0;

  const counts = useMemo(() => {
    return {
      all: total,
      published: list.filter((w) => w.status === "published").length,
      draft: list.filter((w) => w.status === "draft").length,
    };
  }, [list, total]);

  const handleCreate = async () => {
    const name = newName.trim();
    if (!name) {
      toast("请填写工作流名称", "error");
      return;
    }
    setBusy(true);
    try {
      // 后端会拒绝空工作流（scheduler.ErrEmptyWorkflow），所以创建一个
      // 最小可运行骨架：input → output。节点/连线都是真实可保存的结构。
      const created = await api.workflows.create({
        name,
        description: newDesc.trim(),
        status: "draft",
        nodes: [
          { id: "input", type: "input", x: 80, y: 200, config: { label: "输入" } },
          { id: "output", type: "output", x: 460, y: 200, config: { label: "输出" } },
        ],
        edges: [["input", "output"]],
      });
      toast(`已创建工作流「${created.name}」`, "success");
      setCreating(false);
      setNewName("");
      setNewDesc("");
      if (created.id) navigate(`/workflows/${created.id}`);
      else refresh();
    } catch (e) {
      toast(`创建失败：${(e as Error).message}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const handleDelete = async () => {
    const target = deleteTarget;
    if (!target) return;
    try {
      await api.workflows.remove(target.id);
      toast(`已删除「${target.name}」`, "success");
      setDeleteTarget(null);
      refresh();
    } catch (e) {
      toast(`删除失败：${(e as Error).message}`, "error");
    }
  };

  if (loading) return <PageLoader text="加载工作流列表…" />;

  if (error) return <ErrorState title="加载工作流列表失败" description={error.message} onRetry={refresh} />;

  return (
    <div className="space-y-6">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">工作流</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            共 {total} 个工作流 · 拖拽节点即可编排 DAG
          </p>
        </div>
        <Button
          variant="brand"
          icon={<Plus size={15} />}
          onClick={() => setCreating((v) => !v)}
        >
          新建工作流
        </Button>
      </div>

      {/* 新建表单 */}
      {creating && (
        <Card>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="名称">
              <Input
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="例如：日报生成流水线"
              />
            </Field>
            <Field label="描述（可选）">
              <Input
                value={newDesc}
                onChange={(e) => setNewDesc(e.target.value)}
                placeholder="一句话说明这个工作流做什么"
              />
            </Field>
          </div>
          <div className="mt-4 flex items-center gap-2">
            <Button variant="brand" size="sm" onClick={handleCreate} disabled={busy}>
              {busy ? "创建中…" : "创建并打开编辑器"}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
              取消
            </Button>
            <span className="text-2xs text-fg-subtle">
              会创建一个 input → output 的最小骨架（后端不允许保存空工作流）
            </span>
          </div>
        </Card>
      )}

      {/* 过滤条 */}
      <div className="flex flex-wrap items-center gap-3">
        <Segmented
          value={filter}
          onChange={setFilter}
          options={[
            { value: "all", label: "全部", count: counts.all },
            { value: "published", label: "已发布", count: counts.published },
            { value: "draft", label: "草稿", count: counts.draft },
          ]}
        />
        <div className="relative ml-auto w-full sm:w-64">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
          />
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="搜索工作流…"
            className="pl-8"
          />
        </div>
      </div>

      {/* 列表 */}
      {list.length === 0 ? (
        <EmptyState
          icon={<GitBranch size={19} />}
          title={total === 0 && !debouncedQ && filter === "all" ? "还没有工作流" : "没有匹配的工作流"}
          description={
            total === 0 && !debouncedQ && filter === "all"
              ? "点右上角「新建工作流」创建第一个 DAG。"
              : "换个关键词，或清空筛选条件再看看。"
          }
          action={
            <Button
              variant="outline"
              onClick={() => {
                setQ("");
                setFilter("all");
              }}
            >
              清空筛选
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {list.map((wf) => (
            <WorkflowCard
              key={wf.id}
              wf={wf}
              onDelete={(target) => setDeleteTarget(target)}
            />
          ))}
        </div>
      )}

      {/* 说明条 */}
      <div className="flex items-center gap-2.5 rounded-xl border border-line bg-surface-2/50 px-4 py-3">
        <Badge tone="violet">提示</Badge>
        <p className="text-xs text-fg-muted">
          保存时会做环检测与悬挂边校验，非法 DAG（如 A→B→C→A）会被直接拒绝。
        </p>
      </div>

      <ConfirmDialog
        open={deleteTarget !== null}
        title={`删除「${deleteTarget?.name ?? ""}」？`}
        description="删除后无法恢复，该工作流的历史任务记录仍会保留。"
        confirmText="删除"
        cancelText="取消"
        tone="danger"
        onConfirm={handleDelete}
        onCancel={() => setDeleteTarget(null)}
      />

      <ToastContainer toasts={toasts} />
    </div>
  );
}

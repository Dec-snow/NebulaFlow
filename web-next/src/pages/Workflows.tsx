import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  GitBranch,
  MoreHorizontal,
  Plus,
  Search,
  TrendingUp,
} from "lucide-react";
import {
  Badge,
  Button,
  Card,
  EmptyState,
  ErrorState,
  IconButton,
  Input,
  PageLoader,
  Segmented,
  StatusBadge,
} from "@/components/ui";
import { api } from "@/api";
import { useDebounce, useFetch } from "@/hooks";
import type { WorkflowSummary } from "@/types";
import { cn, numberOrNull, timeAgo } from "@/lib/utils";

/** 迷你 DAG 缩略图：把节点相对坐标映射到小视口，仅表达拓扑形状。 */
function MiniDag({ wf }: { wf: WorkflowSummary }) {
  // 列表接口不下发拓扑：后端的 workflow 列表是 `nodes,omitempty`（未 join
  // workflow_nodes 时为 nil），也没有 mini/miniEdges 这种字段。
  // 所以这里先兜底成空数组——否则 wf.mini.forEach 会直接抛 TypeError 白屏。
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

function WorkflowCard({ wf }: { wf: WorkflowSummary }) {
  // 统计字段是后端列表接口的"期望值"而非承诺：没有就隐藏，
  // 而不是渲染出 "undefined 节点" / "NaN%"。
  const nodeCount = numberOrNull(wf.nodeCount);
  const edgeCount = numberOrNull(wf.edgeCount);
  const successRate = numberOrNull(wf.successRate);
  const runs = numberOrNull(wf.runs);

  return (
    <Link to={`/workflows/${wf.id}`} className="group block">
      <Card hover className="flex h-full flex-col">
        <div className="mb-3.5 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="truncate text-sm font-semibold text-fg">{wf.name}</h3>
            <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-fg-subtle">
              {wf.description}
            </p>
          </div>
          <div
            onClick={(e) => e.preventDefault()}
            className="shrink-0 opacity-0 transition-opacity group-hover:opacity-100"
          >
            <IconButton label="更多操作" variant="ghost">
              <MoreHorizontal size={15} />
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
          <div className="flex items-center gap-1.5 text-fg-subtle">
            <TrendingUp size={12} />
            <span className="tnum">
              {runs === null ? "运行数未知" : `${runs} 次运行`}
            </span>
          </div>
          <div className="flex items-center gap-2">
            {successRate !== null && (
              <span
                className={cn(
                  "tnum font-medium",
                  successRate >= 98 ? "text-mint" : "text-amber",
                )}
              >
                {successRate}%
              </span>
            )}
            <span className="text-fg-subtle">{timeAgo(wf.updatedAt)}</span>
          </div>
        </div>
      </Card>
    </Link>
  );
}

export default function Workflows() {
  const [filter, setFilter] = useState<"all" | "published" | "draft">("all");
  const [q, setQ] = useState("");
  const debouncedQ = useDebounce(q, 300);

  const { data, loading, error, refresh } = useFetch(
    () =>
      api.workflows.list({
        status: filter === "all" ? undefined : filter,
        q: debouncedQ || undefined,
      }),
    [filter, debouncedQ],
  );

  // 列表接口不一定下发 items / total（分页字段缺失时后端可能只回数组本身）
  const list = Array.isArray(data?.items) ? data.items : [];
  const total = numberOrNull(data?.total) ?? 0;

  // 计算各状态数量（用全量数据近似，实际应由后端返回）
  const allList = list;
  const counts = {
    all: total,
    published: allList.filter((w) => w.status === "published").length,
    draft: allList.filter((w) => w.status === "draft").length,
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
        <Button variant="brand" icon={<Plus size={15} />}>
          新建工作流
        </Button>
      </div>

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
          title="没有匹配的工作流"
          description="换个关键词，或清空筛选条件再看看。"
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
            <WorkflowCard key={wf.id} wf={wf} />
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
    </div>
  );
}

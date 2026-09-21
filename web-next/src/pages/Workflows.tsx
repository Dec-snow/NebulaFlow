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
  IconButton,
  Input,
  Segmented,
  StatusBadge,
} from "@/components/ui";
import { WORKFLOWS, type WorkflowSummary } from "@/lib/mock";
import { cn, timeAgo } from "@/lib/utils";

/** 迷你 DAG 缩略图：把节点相对坐标映射到小视口，仅表达拓扑形状。 */
function MiniDag({ wf }: { wf: WorkflowSummary }) {
  const pos = useMemo(() => {
    const m = new Map<string, { x: number; y: number }>();
    wf.mini.forEach((n) => m.set(n.id, { x: 7 + (n.x / 100) * 86, y: 9 + (n.y / 100) * 42 }));
    return m;
  }, [wf]);

  const first = wf.mini[0]?.id;
  const last = wf.mini[wf.mini.length - 1]?.id;

  return (
    <div className="dot-grid rounded-lg border border-line bg-surface-2/50">
      <svg viewBox="0 0 100 60" className="h-[104px] w-full" aria-hidden>
        {/* 连线 */}
        {wf.miniEdges.map(([a, b], i) => {
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
        {wf.mini.map((n) => {
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
            <span className="tnum">{wf.nodeCount} 节点</span>
            <span className="tnum">{wf.edgeCount} 连线</span>
          </div>
        </div>

        <div className="divider my-3.5" />

        <div className="flex items-center justify-between text-2xs">
          <div className="flex items-center gap-1.5 text-fg-subtle">
            <TrendingUp size={12} />
            <span className="tnum">{wf.runs} 次运行</span>
          </div>
          <div className="flex items-center gap-2">
            <span
              className={cn(
                "tnum font-medium",
                wf.successRate >= 98 ? "text-mint" : "text-amber",
              )}
            >
              {wf.successRate}%
            </span>
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

  const list = WORKFLOWS.filter(
    (w) =>
      (filter === "all" || w.status === filter) &&
      (q === "" || w.name.toLowerCase().includes(q.toLowerCase())),
  );

  const counts = {
    all: WORKFLOWS.length,
    published: WORKFLOWS.filter((w) => w.status === "published").length,
    draft: WORKFLOWS.filter((w) => w.status === "draft").length,
  };

  return (
    <div className="space-y-6">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">工作流</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            共 {WORKFLOWS.length} 个工作流 · 拖拽节点即可编排 DAG
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

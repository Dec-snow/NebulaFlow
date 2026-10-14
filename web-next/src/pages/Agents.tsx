import { useMemo, useState } from "react";
import { Bot, Plus, Search, Trash2, Zap } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  ErrorState,
  Input,
  PageLoader,
  Segmented,
  StatusBadge,
  ToastContainer,
  useToast,
} from "@/components/ui";
import { api } from "@/api";
import { useDebounce, useFetch } from "@/hooks";
import { cn, numberOrNull } from "@/lib/utils";
import type { Agent, AgentRuntimeType } from "@/types";

type RuntimeFilter = "all" | AgentRuntimeType;

const RUNTIME_TONE: Record<AgentRuntimeType, string> = {
  native: "bg-brand-soft text-brand",
  langchain: "bg-violet/12 text-violet",
  http: "bg-sky/12 text-sky",
};

const RUNTIME_LABEL: Record<AgentRuntimeType, string> = {
  native: "Native",
  langchain: "LangChain",
  http: "HTTP",
};

function AgentCard({ agent, onDelete, onTest }: { agent: Agent; onDelete: (id: number) => void; onTest: (id: number) => void }) {
  // 后端未配置能力时该字段可能缺失，直接 .slice/.length 会抛 TypeError
  const capabilities = Array.isArray(agent.capabilities) ? agent.capabilities : [];

  return (
    <Card hover className="flex h-full flex-col">
      <div className="mb-3 flex items-start gap-3">
        <span
          className={cn(
            "flex h-10 w-10 shrink-0 items-center justify-center rounded-lg",
            RUNTIME_TONE[agent.runtimeType],
          )}
        >
          <Bot size={18} />
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h3 className="truncate text-sm font-semibold text-fg">{agent.name}</h3>
          </div>
          <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-fg-subtle">
            {agent.description}
          </p>
        </div>
      </div>

      {/* 能力标签 */}
      <div className="mb-3 flex flex-wrap gap-1.5">
        {capabilities.slice(0, 3).map((cap) => (
          <Badge key={cap} tone="neutral">
            {cap}
          </Badge>
        ))}
        {capabilities.length > 3 && (
          <Badge tone="neutral">+{capabilities.length - 3}</Badge>
        )}
      </div>

      <div className="divider my-1" />

      <div className="mt-3 flex items-center justify-between">
        <div className="flex items-center gap-2">
          <span className="font-mono text-2xs text-fg-subtle">v{agent.version}</span>
          <StatusBadge status={agent.status} />
        </div>
        <Badge tone="neutral" className="text-2xs">
          {RUNTIME_LABEL[agent.runtimeType]}
        </Badge>
      </div>

      <div className="mt-4 flex gap-2">
        <Button
          variant="outline"
          size="sm"
          block
          icon={<Zap size={13} />}
          onClick={() => onTest(agent.id)}
        >
          测试连接
        </Button>
        <Button
          variant="ghost"
          size="sm"
          block
          icon={<Trash2 size={13} />}
          onClick={() => onDelete(agent.id)}
          className="text-rose hover:bg-rose/10 hover:text-rose"
        >
          删除
        </Button>
      </div>
    </Card>
  );
}

export default function Agents() {
  const [filter, setFilter] = useState<RuntimeFilter>("all");
  const [q, setQ] = useState("");
  const debouncedQ = useDebounce(q, 300);
  const { toasts, toast } = useToast();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [deleteId, setDeleteId] = useState<number | null>(null);

  const { data, loading, error, refresh } = useFetch(
    () => api.agents.list({ runtimeType: filter === "all" ? undefined : filter, q: debouncedQ }),
    [filter, debouncedQ],
  );

  const counts = useMemo(() => {
    if (!data) return { all: 0, native: 0, langchain: 0, http: 0 };
    // items / total 可能缺失：直接 .filter 会抛 TypeError
    const items = Array.isArray(data.items) ? data.items : [];
    return {
      all: numberOrNull(data.total) ?? 0,
      native: items.filter((a) => a.runtimeType === "native").length,
      langchain: items.filter((a) => a.runtimeType === "langchain").length,
      http: items.filter((a) => a.runtimeType === "http").length,
    };
  }, [data]);

  const handleDeleteClick = (id: number) => {
    setDeleteId(id);
    setConfirmOpen(true);
  };

  const handleConfirmDelete = async () => {
    if (deleteId == null) return;
    try {
      await api.agents.delete(deleteId);
      toast("删除成功", "success");
      setConfirmOpen(false);
      setDeleteId(null);
      refresh();
    } catch (e) {
      toast("删除失败，请重试", "error");
    }
  };

  const handleTest = async (id: number) => {
    // 简单模拟：打印日志并刷新
    console.log("[agents] test connection", id);
  };

  if (loading) {
    return <PageLoader text="加载 Agent 列表…" />;
  }

  if (error) {
    return <ErrorState title="加载 Agent 列表失败" description={error.message} onRetry={refresh} />;
  }

  const list = Array.isArray(data?.items) ? data.items : [];

  return (
    <div className="space-y-6">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">我的 Agent</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            共 {data?.total ?? 0} 个 Agent · 支持 Native / LangChain / HTTP 三种运行时
          </p>
        </div>
        <Button variant="brand" icon={<Plus size={15} />}>
          新建 Agent
        </Button>
      </div>

      {/* 过滤条 */}
      <div className="flex flex-wrap items-center gap-3">
        <Segmented
          value={filter}
          onChange={(v) => setFilter(v as RuntimeFilter)}
          options={[
            { value: "all", label: "全部", count: counts.all },
            { value: "native", label: "Native", count: counts.native },
            { value: "langchain", label: "LangChain", count: counts.langchain },
            { value: "http", label: "HTTP", count: counts.http },
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
            placeholder="搜索 Agent…"
            className="pl-8"
          />
        </div>
      </div>

      {/* 列表 */}
      {list.length === 0 ? (
        <EmptyState
          icon={<Bot size={19} />}
          title="没有匹配的 Agent"
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
          {list.map((agent) => (
            <AgentCard
              key={agent.id}
              agent={agent}
              onDelete={handleDeleteClick}
              onTest={handleTest}
            />
          ))}
        </div>
      )}

      {/* 说明条 */}
      <div className="flex items-center gap-2.5 rounded-xl border border-line bg-surface-2/50 px-4 py-3">
        <Badge tone="sky">提示</Badge>
        <p className="text-xs text-fg-muted">
          Agent 支持三种运行时：Native（内置引擎）、LangChain（Python 服务）、HTTP（外部服务）。
        </p>
      </div>

      <ConfirmDialog
        open={confirmOpen}
        title="确认删除 Agent？"
        description="删除后无法恢复，相关工作流可能会受到影响。"
        confirmText="删除"
        cancelText="取消"
        tone="danger"
        onConfirm={handleConfirmDelete}
        onCancel={() => {
          setConfirmOpen(false);
          setDeleteId(null);
        }}
      />

      <ToastContainer toasts={toasts} />
    </div>
  );
}

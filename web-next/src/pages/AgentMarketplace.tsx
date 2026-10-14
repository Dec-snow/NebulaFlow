import { useMemo, useState } from "react";
import { Bot, Check, Download, Search, Store } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  EmptyState,
  ErrorState,
  Input,
  PageLoader,
  Segmented,
  Select,
  ToastContainer,
  useToast,
} from "@/components/ui";
import { api } from "@/api";
import { useDebounce, useFetch } from "@/hooks";
import { cn } from "@/lib/utils";
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

function MarketplaceAgentCard({
  agent,
  installed,
  onInstall,
}: {
  agent: Agent;
  installed: boolean;
  onInstall: (id: number, name: string) => void;
}) {
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
            {installed && (
              <Badge tone="mint" className="text-2xs">
                <Check size={10} />
                已安装
              </Badge>
            )}
          </div>
          <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-fg-subtle">
            {agent.description}
          </p>
        </div>
      </div>

      {/* 能力标签 */}
      <div className="mb-3 flex flex-wrap gap-1.5">
        {capabilities.slice(0, 4).map((cap) => (
          <Badge key={cap} tone="neutral">
            {cap}
          </Badge>
        ))}
        {capabilities.length > 4 && (
          <Badge tone="neutral">+{capabilities.length - 4}</Badge>
        )}
      </div>

      <div className="divider my-1" />

      <div className="mt-3 flex items-center justify-between">
        <span className="font-mono text-2xs text-fg-subtle">v{agent.version}</span>
        <Badge tone="neutral" className="text-2xs">
          {RUNTIME_LABEL[agent.runtimeType]}
        </Badge>
      </div>

      <div className="mt-4">
        <Button
          variant={installed ? "soft" : "brand"}
          size="sm"
          block
          icon={installed ? <Check size={13} /> : <Download size={13} />}
          disabled={installed}
          onClick={() => !installed && onInstall(agent.id, agent.name)}
        >
          {installed ? "已安装" : "安装"}
        </Button>
      </div>
    </Card>
  );
}

export default function AgentMarketplace() {
  const [filter, setFilter] = useState<RuntimeFilter>("all");
  const [q, setQ] = useState("");
  const debouncedQ = useDebounce(q, 300);
  const [capFilter, setCapFilter] = useState<string>("all");
  const [installedSet, setInstalledSet] = useState<Set<string>>(new Set());
  const { toasts, toast } = useToast();

  const { data, loading, error, refresh } = useFetch(
    () =>
      api.agents.marketplace({
        runtimeType: filter === "all" ? undefined : filter,
        q: debouncedQ,
      }),
    [filter, debouncedQ],
  );

  // 市场接口的 items / installedNames 可能缺失（空结果时后端可能只回 null）
  const items = data && Array.isArray(data.items) ? data.items : [];

  // 收集所有能力标签（capabilities 可能缺失/为空，不能直接 .forEach）
  const allCapabilities = useMemo(() => {
    if (items.length === 0) return [] as string[];
    const set = new Set<string>();
    items.forEach((a) => (Array.isArray(a.capabilities) ? a.capabilities : []).forEach((c) => set.add(c)));
    return Array.from(set);
  }, [items]);

  // 合并初始已安装 + 新安装的
  const installedNames = useMemo(() => {
    const base = new Set(Array.isArray(data?.installedNames) ? data.installedNames : []);
    installedSet.forEach((n) => base.add(n));
    return base;
  }, [data, installedSet]);

  // 按能力筛选
  const filteredItems = useMemo(() => {
    if (items.length === 0) return [] as Agent[];
    if (capFilter === "all") return items;
    return items.filter((a) => (Array.isArray(a.capabilities) ? a.capabilities : []).includes(capFilter));
  }, [items, capFilter]);

  const counts = useMemo(() => {
    return {
      all: items.length,
      native: items.filter((a) => a.runtimeType === "native").length,
      langchain: items.filter((a) => a.runtimeType === "langchain").length,
      http: items.filter((a) => a.runtimeType === "http").length,
    };
  }, [items]);

  const handleInstall = async (id: number, name: string) => {
    await api.agents.install(id);
    setInstalledSet((prev) => new Set(prev).add(name));
    toast(`${name} 安装成功`, "success");
  };

  if (loading) {
    return <PageLoader text="加载 Agent 市场…" />;
  }

  if (error) {
    return <ErrorState title="加载 Agent 市场失败" description={error.message} onRetry={refresh} />;
  }

  const list = filteredItems;

  return (
    <div className="space-y-6">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">Agent 市场</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            共 {items.length} 个 Agent · 一键安装到你的工作区
          </p>
        </div>
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
        <Select
          value={capFilter}
          onChange={(e) => setCapFilter(e.target.value)}
          className="w-36"
        >
          <option value="all">全部能力</option>
          {allCapabilities.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </Select>
        <div className="relative ml-auto w-full sm:w-64">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
          />
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="搜索 Agent 或能力…"
            className="pl-8"
          />
        </div>
      </div>

      {/* 列表 */}
      {list.length === 0 ? (
        <EmptyState
          icon={<Store size={19} />}
          title="没有匹配的 Agent"
          description="换个关键词或筛选条件再看看。"
          action={
            <Button
              variant="outline"
              onClick={() => {
                setQ("");
                setFilter("all");
                setCapFilter("all");
              }}
            >
              清空筛选
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {list.map((agent) => (
            <MarketplaceAgentCard
              key={agent.id}
              agent={agent}
              installed={installedNames.has(agent.name)}
              onInstall={handleInstall}
            />
          ))}
        </div>
      )}

      <ToastContainer toasts={toasts} />
    </div>
  );
}

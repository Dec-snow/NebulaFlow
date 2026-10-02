import { useMemo, useState } from "react";
import { Bot, Check, Download, Search, Store } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageLoader,
  Segmented,
  Select,
  ToastContainer,
  useToast,
} from "@/components/ui";
import { api } from "@/api";
import { useFetch } from "@/hooks";
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
  return (
    <Card hover className="flex h-full flex-col">
      <div className="mb-3 flex items-start gap-3">
        <span
          className={cn(
            "flex h-10 w-10 shrink-0 items-center justify-center rounded-lg",
            RUNTIME_TONE[agent.runtime_type],
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
        {agent.capabilities.slice(0, 4).map((cap) => (
          <Badge key={cap} tone="neutral">
            {cap}
          </Badge>
        ))}
        {agent.capabilities.length > 4 && (
          <Badge tone="neutral">+{agent.capabilities.length - 4}</Badge>
        )}
      </div>

      <div className="divider my-1" />

      <div className="mt-3 flex items-center justify-between">
        <span className="font-mono text-2xs text-fg-subtle">v{agent.version}</span>
        <Badge tone="neutral" className="text-2xs">
          {RUNTIME_LABEL[agent.runtime_type]}
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
  const [capFilter, setCapFilter] = useState<string>("all");
  const [installedSet, setInstalledSet] = useState<Set<string>>(new Set());
  const { toasts, toast } = useToast();

  const { data, loading } = useFetch(
    () =>
      api.agents.marketplace({
        runtime_type: filter === "all" ? undefined : filter,
        q,
      }),
    [filter, q],
  );

  // 收集所有能力标签
  const allCapabilities = useMemo(() => {
    if (!data) return [] as string[];
    const set = new Set<string>();
    data.items.forEach((a) => a.capabilities.forEach((c) => set.add(c)));
    return Array.from(set);
  }, [data]);

  // 合并初始已安装 + 新安装的
  const installedNames = useMemo(() => {
    const base = new Set(data?.installed_names ?? []);
    installedSet.forEach((n) => base.add(n));
    return base;
  }, [data, installedSet]);

  // 按能力筛选
  const filteredItems = useMemo(() => {
    if (!data) return [] as Agent[];
    if (capFilter === "all") return data.items;
    return data.items.filter((a) => a.capabilities.includes(capFilter));
  }, [data, capFilter]);

  const counts = useMemo(() => {
    if (!data) return { all: 0, native: 0, langchain: 0, http: 0 };
    return {
      all: data.items.length,
      native: data.items.filter((a) => a.runtime_type === "native").length,
      langchain: data.items.filter((a) => a.runtime_type === "langchain").length,
      http: data.items.filter((a) => a.runtime_type === "http").length,
    };
  }, [data]);

  const handleInstall = async (id: number, name: string) => {
    await api.agents.install(id);
    setInstalledSet((prev) => new Set(prev).add(name));
    toast(`${name} 安装成功`, "success");
  };

  if (loading) {
    return <PageLoader text="加载 Agent 市场…" />;
  }

  const list = filteredItems;

  return (
    <div className="space-y-6">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">Agent 市场</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            共 {data?.items.length ?? 0} 个 Agent · 一键安装到你的工作区
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

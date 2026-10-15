import { useState } from "react";
import { ArrowRight, Boxes, Check, MoreHorizontal, Plus, Server, Zap } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  ErrorState,
  Field,
  IconButton,
  Input,
  PageLoader,
  StatusBadge,
  ToastContainer,
  useToast,
} from "@/components/ui";
import { api } from "@/api";
import { useFetch } from "@/hooks";
import type { Provider } from "@/types";
import { cn, numberOrNull } from "@/lib/utils";

function ProviderCard({
  p,
  onToggle,
  onAddModel,
}: {
  p: Provider;
  onToggle: (p: Provider) => void;
  onAddModel: (p: Provider, name: string, maxTokens: number) => void;
}) {
  const isDown = p.status === "disabled";
  // 列表接口的 models 是 join 出来的，查询失败时后端会下发 null
  const models = Array.isArray(p.models) ? p.models : [];
  const priority = numberOrNull(p.priority);
  const [adding, setAdding] = useState(false);
  const [modelName, setModelName] = useState("");
  const [maxTokens, setMaxTokens] = useState("4096");

  return (
    <Card
      className={cn(
        "flex h-full flex-col transition-all duration-200 ease-smooth",
        isDown ? "opacity-70" : "hover:-translate-y-0.5 hover:shadow-md",
      )}
    >
      <div className="mb-3.5 flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <span
            className={cn(
              "flex h-9 w-9 shrink-0 items-center justify-center rounded-lg",
              isDown ? "bg-surface-2 text-fg-subtle" : "bg-brand-soft text-brand",
            )}
          >
            <Server size={16} />
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-1.5">
              <span className="truncate text-sm font-semibold text-fg">{p.name}</span>
              {p.isDefault && (
                <span title="默认 Provider">
                  <Check size={12} className="text-mint" />
                </span>
              )}
            </div>
            <div className="truncate font-mono text-2xs text-fg-subtle">
              {p.baseUrl || "—"}
            </div>
          </div>
        </div>
        <IconButton label="Provider 详情" variant="ghost" onClick={() => setAdding((v) => !v)}>
          <MoreHorizontal size={15} />
        </IconButton>
      </div>

      <div className="mb-3 flex flex-wrap items-center gap-2">
        {/* 后端的 enabled 布尔值映射成 enabled/disabled；没有 latency 字段，不显示假延迟 */}
        <StatusBadge status={p.status} />
        <Badge tone="neutral">优先级 {priority ?? "—"}</Badge>
        <Badge tone="sky">{models.length} 个模型</Badge>
      </div>

      <div className="divider my-1" />

      <div className="mt-3 flex-1">
        <div className="mb-2 text-2xs font-medium uppercase tracking-wider text-fg-subtle">
          可用模型
        </div>
        <div className="space-y-1.5">
          {models.map((m) => (
            <div
              key={m.id ?? m.name}
              className="flex items-center justify-between rounded-md border border-line bg-surface-2/40 px-2.5 py-1.5"
            >
              <span className="truncate font-mono text-2xs text-fg">{m.name}</span>
              <span className="tnum shrink-0 text-2xs text-fg-subtle">
                {numberOrNull(m.maxTokens)?.toLocaleString() ?? "—"} tok
              </span>
            </div>
          ))}
          {models.length === 0 && (
            <p className="text-2xs text-fg-subtle">
              {Array.isArray(p.models) ? "未配置模型" : "模型列表未下发"}
            </p>
          )}
        </div>

        {adding && (
          <div className="mt-3 space-y-2 rounded-lg border border-line bg-surface-2/40 p-2.5">
            <Field label="模型名">
              <Input
                value={modelName}
                onChange={(e) => setModelName(e.target.value)}
                placeholder="deepseek-chat"
              />
            </Field>
            <Field label="max_tokens">
              <Input
                value={maxTokens}
                onChange={(e) => setMaxTokens(e.target.value)}
                placeholder="4096"
              />
            </Field>
            <div className="flex items-center gap-2">
              <Button
                variant="brand"
                size="sm"
                onClick={() => {
                  const name = modelName.trim();
                  if (!name) return;
                  onAddModel(p, name, Number(maxTokens) || 0);
                  setModelName("");
                  setAdding(false);
                }}
              >
                添加
              </Button>
              <span className="text-2xs text-fg-subtle">
                POST /api/providers/{p.id}/models（需 admin）
              </span>
            </div>
          </div>
        )}
      </div>

      <div className="mt-4 flex gap-2">
        <Button variant="outline" size="sm" block onClick={() => setAdding((v) => !v)}>
          {adding ? "收起" : "添加模型"}
        </Button>
        <Button variant="ghost" size="sm" block onClick={() => onToggle(p)}>
          {p.status === "enabled" ? "停用" : "启用"}
        </Button>
      </div>
    </Card>
  );
}

export default function Models() {
  const { data: providers, loading, error, refresh } = useFetch(() => api.providers.list(), []);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ name: "", baseUrl: "", apiKey: "" });
  const [busy, setBusy] = useState(false);
  const { toasts, toast } = useToast();

  const handleCreate = async () => {
    if (!form.name.trim() || !form.baseUrl.trim()) {
      toast("名称与 Base URL 必填", "error");
      return;
    }
    setBusy(true);
    try {
      await api.providers.create({
        name: form.name.trim(),
        baseUrl: form.baseUrl.trim(),
        apiKey: form.apiKey,
      });
      toast("Provider 已创建", "success");
      setForm({ name: "", baseUrl: "", apiKey: "" });
      setCreating(false);
      refresh();
    } catch (e) {
      toast(`创建失败：${(e as Error).message}（该接口仅 admin 可调用）`, "error");
    } finally {
      setBusy(false);
    }
  };

  const handleToggle = async (p: Provider) => {
    try {
      await api.providers.update(p.id, { enabled: p.status !== "enabled" });
      toast(p.status === "enabled" ? "已停用" : "已启用", "success");
      refresh();
    } catch (e) {
      toast(`操作失败：${(e as Error).message}`, "error");
    }
  };

  const handleAddModel = async (p: Provider, name: string, maxTokens: number) => {
    try {
      await api.providers.addModel(p.id, { name, maxTokens });
      toast(`已添加模型 ${name}`, "success");
      refresh();
    } catch (e) {
      toast(`添加失败：${(e as Error).message}（该接口仅 admin 可调用）`, "error");
    }
  };

  if (loading) return <PageLoader text="加载模型与供应商…" />;

  if (error) return <ErrorState title="加载模型与供应商失败" description={error.message} onRetry={refresh} />;

  const providerList = Array.isArray(providers) ? providers : [];
  // 故障转移按 priority 升序（后端 fallback 顺序：越小越优先）
  const enabled = providerList
    .filter((p) => p.status !== "disabled")
    .slice()
    .sort((a, b) => (a.priority ?? 100) - (b.priority ?? 100));

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">
            模型与供应商
          </h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            统一 Provider 抽象 · 按优先级自动故障转移
          </p>
        </div>
        <Button variant="brand" icon={<Plus size={15} />} onClick={() => setCreating((v) => !v)}>
          添加 Provider
        </Button>
      </div>

      {creating && (
        <Card>
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="名称">
              <Input
                value={form.name}
                onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                placeholder="deepseek"
              />
            </Field>
            <Field label="Base URL">
              <Input
                value={form.baseUrl}
                onChange={(e) => setForm((f) => ({ ...f, baseUrl: e.target.value }))}
                placeholder="https://api.deepseek.com/v1"
              />
            </Field>
            <Field label="API Key">
              <Input
                value={form.apiKey}
                onChange={(e) => setForm((f) => ({ ...f, apiKey: e.target.value }))}
                placeholder="sk-..."
              />
            </Field>
          </div>
          <div className="mt-4 flex items-center gap-2">
            <Button variant="brand" size="sm" onClick={handleCreate} disabled={busy}>
              {busy ? "提交中…" : "创建"}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
              取消
            </Button>
            <span className="text-2xs text-fg-subtle">
              POST /api/providers（仅 admin 角色可调用）
            </span>
          </div>
        </Card>
      )}

      {/* 故障转移链路 */}
      <Card>
        <CardHeader
          title="故障转移顺序"
          subtitle="调用失败时按优先级依次降级，首 token 之前可安全切换"
          action={
            <Badge tone="mint">
              <Zap size={10} />
              {enabled.length} 个启用
            </Badge>
          }
        />
        <div className="flex flex-wrap items-center gap-2">
          {enabled.map((p, i) => (
            <div key={p.id} className="flex items-center gap-2">
              <div
                className={cn(
                  "flex items-center gap-2 rounded-lg border px-3 py-2",
                  i === 0
                    ? "border-transparent bg-brand-soft ring-1 ring-brand/30"
                    : "border-line bg-surface-2/50",
                )}
              >
                <span className="h-1.5 w-1.5 rounded-full bg-mint" />
                <span className="text-xs font-medium text-fg">{p.name}</span>
                <span className="tnum text-2xs text-fg-subtle">
                  优先级 {numberOrNull(p.priority) ?? "—"}
                </span>
              </div>
              {i < enabled.length - 1 && (
                <ArrowRight size={13} className="shrink-0 text-fg-subtle" />
              )}
            </div>
          ))}
          {enabled.length === 0 && (
            <p className="text-xs text-fg-subtle">暂无启用的 Provider</p>
          )}
        </div>
        <div className="mt-4 flex items-center gap-2.5 rounded-lg border border-line bg-surface-2/50 px-3.5 py-2.5">
          <Boxes size={14} className="shrink-0 text-fg-subtle" />
          <p className="text-2xs leading-relaxed text-fg-muted">
            Provider 的延迟由调用时实时观测（后端 LLMProvider 不存 latency 字段），
            因此这里只展示配置侧的优先级顺序。
          </p>
        </div>
      </Card>

      {/* Provider 卡片 */}
      {providerList.length === 0 ? (
        <Card className="text-xs text-fg-subtle">
          还没有配置 Provider。点右上角「添加 Provider」创建一个（需要 admin 角色）。
        </Card>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
          {providerList.map((p) => (
            <ProviderCard
              key={p.id}
              p={p}
              onToggle={handleToggle}
              onAddModel={handleAddModel}
            />
          ))}
        </div>
      )}

      <ToastContainer toasts={toasts} />
    </div>
  );
}

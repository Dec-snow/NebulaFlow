import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Layers, Search, Sparkles, Users } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageLoader,
  Segmented,
  ToastContainer,
  useToast,
} from "@/components/ui";
import { api } from "@/api";
import { useFetch } from "@/hooks";
import { cn } from "@/lib/utils";
import type { WorkflowTemplate } from "@/types";

const CATEGORY_TONE: Record<string, string> = {
  内容创作: "bg-violet/12 text-violet",
  数据分析: "bg-sky/12 text-sky",
  客户服务: "bg-amber/15 text-amber",
  研发提效: "bg-mint/12 text-mint",
  教育学习: "bg-rose/12 text-rose",
};

function TemplateCard({
  template,
  onUse,
}: {
  template: WorkflowTemplate;
  onUse: (id: number, name: string) => void;
}) {
  const toneClass = CATEGORY_TONE[template.category] ?? "bg-brand-soft text-brand";

  return (
    <Card hover className="flex h-full flex-col">
      <div className="mb-3 flex items-start gap-3">
        <span
          className={cn(
            "flex h-10 w-10 shrink-0 items-center justify-center rounded-lg text-lg",
            toneClass,
          )}
        >
          {template.icon ?? <Sparkles size={18} />}
        </span>
        <div className="min-w-0 flex-1">
          <h3 className="truncate text-sm font-semibold text-fg">{template.name}</h3>
          <p className="mt-1 line-clamp-2 text-xs leading-relaxed text-fg-subtle">
            {template.description}
          </p>
        </div>
      </div>

      {/* 分类标签 + 节点数 + 使用次数 */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Badge tone="neutral">{template.category}</Badge>
        <span className="flex items-center gap-1 text-2xs text-fg-subtle">
          <Layers size={11} />
          <span className="tnum">{template.nodes} 节点</span>
        </span>
        <span className="flex items-center gap-1 text-2xs text-fg-subtle">
          <Users size={11} />
          <span className="tnum">{template.used_count.toLocaleString()} 次使用</span>
        </span>
      </div>

      <div className="divider my-1" />

      <div className="mt-4">
        <Button
          variant="brand"
          size="sm"
          block
          icon={<Sparkles size={13} />}
          onClick={() => onUse(template.id, template.name)}
        >
          使用模板
        </Button>
      </div>
    </Card>
  );
}

export default function Templates() {
  const [category, setCategory] = useState<string>("all");
  const [q, setQ] = useState("");
  const navigate = useNavigate();
  const { toasts, toast } = useToast();

  const { data, loading } = useFetch(
    () =>
      api.templates.list({
        category: category === "all" ? undefined : category,
        q,
      }),
    [category, q],
  );

  const { data: categories } = useFetch(() => api.templates.categories(), []);

  const counts = useMemo(() => {
    if (!categories) return { all: 0 };
    const map: Record<string, number> = {};
    categories.forEach((c) => {
      if (c.name === "全部") {
        map.all = c.count;
      } else {
        map[c.name] = c.count;
      }
    });
    return map;
  }, [categories]);

  const segmentedOptions = useMemo(() => {
    if (!categories) return [];
    return categories.map((c) => ({
      value: c.name === "全部" ? "all" : c.name,
      label: c.name,
      count: c.count,
    }));
  }, [categories]);

  const handleUse = async (id: number, name: string) => {
    const res = await api.templates.use(id);
    toast(`${name} 使用成功，已创建新工作流`, "success");
    // 延迟跳转到编辑器
    setTimeout(() => {
      navigate(`/workflows/${res.workflowId}`);
    }, 800);
  };

  if (loading) {
    return <PageLoader text="加载模板市场…" />;
  }

  const list = data?.items ?? [];

  return (
    <div className="space-y-6">
      {/* 页头 */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">模板市场</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            精选工作流模板，一键创建
          </p>
        </div>
      </div>

      {/* 过滤条 */}
      <div className="flex flex-wrap items-center gap-3">
        <Segmented
          value={category}
          onChange={setCategory}
          options={segmentedOptions}
        />
        <div className="relative ml-auto w-full sm:w-64">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
          />
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="搜索模板…"
            className="pl-8"
          />
        </div>
      </div>

      {/* 列表 */}
      {list.length === 0 ? (
        <EmptyState
          icon={<Sparkles size={19} />}
          title="没有匹配的模板"
          description="换个关键词，或清空筛选条件再看看。"
          action={
            <Button
              variant="outline"
              onClick={() => {
                setQ("");
                setCategory("all");
              }}
            >
              清空筛选
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {list.map((template) => (
            <TemplateCard
              key={template.id}
              template={template}
              onUse={handleUse}
            />
          ))}
        </div>
      )}

      {/* 说明条 */}
      <div className="flex items-center gap-2.5 rounded-xl border border-line bg-surface-2/50 px-4 py-3">
        <Badge tone="violet">提示</Badge>
        <p className="text-xs text-fg-muted">
          点击「使用模板」即可基于模板快速创建新工作流，创建后可在编辑器中自由修改。
        </p>
      </div>

      <ToastContainer toasts={toasts} />
    </div>
  );
}

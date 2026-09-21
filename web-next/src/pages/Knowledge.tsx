import { useState } from "react";
import {
  Database,
  FileText,
  Plus,
  Search,
  Sparkles,
  UploadCloud,
} from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  EmptyState,
  Input,
  Progress,
  StatusBadge,
} from "@/components/ui";
import { KNOWLEDGE_BASES, RETRIEVE_HITS } from "@/lib/mock";
import { cn, timeAgo } from "@/lib/utils";

export default function Knowledge() {
  const [selected, setSelected] = useState(1);
  const [query, setQuery] = useState("Worker Pool 背压机制");
  const [searched, setSearched] = useState(true);

  const kb = KNOWLEDGE_BASES.find((k) => k.id === selected) ?? KNOWLEDGE_BASES[0];
  const totalChunks = kb.docs.reduce((s, d) => s + d.chunks, 0);

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">知识库</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            文档上传 → 分块 → 向量化 → 余弦 Top-K 检索
          </p>
        </div>
        <Button variant="brand" icon={<Plus size={15} />}>
          新建知识库
        </Button>
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        {/* 左：知识库列表 */}
        <div className="space-y-2.5">
          {KNOWLEDGE_BASES.map((k) => {
            const active = k.id === selected;
            const chunks = k.docs.reduce((s, d) => s + d.chunks, 0);
            return (
              <button
                key={k.id}
                onClick={() => setSelected(k.id)}
                className={cn(
                  "w-full rounded-xl border p-4 text-left transition-all duration-200 ease-smooth",
                  active
                    ? "border-transparent bg-brand-soft ring-2 ring-brand/35"
                    : "border-line bg-surface hover:-translate-y-px hover:border-line-strong hover:shadow-sm",
                )}
              >
                <div className="flex items-start gap-3">
                  <span
                    className={cn(
                      "flex h-8 w-8 shrink-0 items-center justify-center rounded-lg",
                      active ? "bg-brand text-brand-fg" : "bg-surface-2 text-fg-muted",
                    )}
                  >
                    <Database size={15} />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium text-fg">
                      {k.name}
                    </div>
                    <div className="mt-0.5 line-clamp-2 text-2xs leading-relaxed text-fg-subtle">
                      {k.description}
                    </div>
                    <div className="mt-2 flex items-center gap-3 text-2xs text-fg-subtle">
                      <span className="tnum">{k.docs.length} 文档</span>
                      <span className="tnum">{chunks} 分块</span>
                    </div>
                  </div>
                </div>
              </button>
            );
          })}

          <Card className="border-dashed bg-surface/40">
            <div className="flex items-center gap-2.5">
              <Sparkles size={14} className="text-brand" />
              <p className="text-2xs leading-relaxed text-fg-subtle">
                Embedding 支持 DeepSeek / OpenAI，失败时自动降级到本地确定性 hash 向量。
              </p>
            </div>
          </Card>
        </div>

        {/* 右：文档 + 检索 */}
        <div className="space-y-4 lg:col-span-2">
          <Card>
            <CardHeader
              title={kb.name}
              subtitle={`${kb.docs.length} 个文档 · ${totalChunks} 个分块`}
              action={<Badge tone="mint">已索引</Badge>}
            />

            <div className="space-y-1.5">
              {kb.docs.map((d) => (
                <div
                  key={d.id}
                  className="flex items-center gap-3 rounded-lg border border-line bg-surface-2/40 px-3 py-2.5 transition-colors hover:border-line-strong"
                >
                  <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md bg-surface text-fg-subtle">
                    <FileText size={13.5} />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="truncate font-mono text-xs text-fg">
                      {d.filename}
                    </div>
                    <div className="mt-0.5 text-2xs text-fg-subtle">
                      {d.sizeKb} KB · {d.chunks} 分块 · {timeAgo(d.createdAt)}
                    </div>
                  </div>
                  <StatusBadge status={d.status} />
                </div>
              ))}
            </div>

            {/* 上传区 */}
            <div className="mt-3.5 flex flex-col items-center justify-center rounded-lg border border-dashed border-line bg-surface-2/40 px-4 py-6 transition-colors hover:border-brand/40 hover:bg-brand-soft/30">
              <UploadCloud size={20} className="mb-2 text-fg-subtle" />
              <p className="text-xs text-fg-muted">
                拖拽文件到此处，或
                <span className="ml-1 cursor-pointer text-brand hover:underline">
                  点击选择
                </span>
              </p>
              <p className="mt-1 text-2xs text-fg-subtle">
                支持 .md / .txt / .pdf / .csv，单文件上限 10 MB
              </p>
            </div>
          </Card>

          {/* 检索测试 */}
          <Card>
            <CardHeader
              title="检索测试"
              subtitle="输入问题，查看向量召回结果与相似度"
              action={<Badge tone="sky">Top-3</Badge>}
            />

            <div className="flex gap-2">
              <div className="relative flex-1">
                <Search
                  size={14}
                  className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                />
                <Input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="例如：Worker Pool 背压机制"
                  className="pl-8"
                  onKeyDown={(e) => e.key === "Enter" && setSearched(true)}
                />
              </div>
              <Button variant="brand" onClick={() => setSearched(true)}>
                检索
              </Button>
            </div>

            <div className="mt-4">
              {!searched ? (
                <EmptyState
                  icon={<Search size={18} />}
                  title="等待检索"
                  description="输入问题后点击检索，结果会按相似度排序展示。"
                />
              ) : (
                <div className="space-y-2.5">
                  {RETRIEVE_HITS.map((h, i) => (
                    <div
                      key={h.id}
                      className="rounded-lg border border-line bg-surface-2/40 p-3.5 transition-colors hover:border-line-strong"
                    >
                      <div className="mb-2 flex items-center gap-2">
                        <span className="tnum flex h-5 w-5 items-center justify-center rounded bg-surface text-2xs font-medium text-fg-muted">
                          {i + 1}
                        </span>
                        <span className="font-mono text-2xs text-fg-subtle">
                          {h.source}
                        </span>
                        <div className="ml-auto flex items-center gap-2">
                          <Progress
                            value={h.score}
                            tone="brand"
                            className="w-16"
                            height={4}
                          />
                          <span className="tnum text-2xs font-medium text-brand">
                            {h.score.toFixed(2)}
                          </span>
                        </div>
                      </div>
                      <p className="text-xs leading-relaxed text-fg-muted">
                        {h.content}
                      </p>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}

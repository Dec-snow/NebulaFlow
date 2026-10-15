import { useRef, useState } from "react";
import {
  Database,
  FileText,
  Plus,
  Search,
  Sparkles,
  Trash2,
  UploadCloud,
} from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorState,
  Field,
  Input,
  PageLoader,
  Progress,
  StatusBadge,
  ToastContainer,
  useToast,
} from "@/components/ui";
import { api } from "@/api";
import { useFetch } from "@/hooks";
import type { RetrievalHit } from "@/types";
import { cn, numberOrNull, timeAgo } from "@/lib/utils";

/** 浏览器端只能直接读文本类文件；PDF 需要后端抽取，前端先明确拒绝而不是传垃圾。 */
const TEXT_EXTENSIONS = [".txt", ".md", ".markdown", ".text", ".csv", ".json", ".log"];

function isTextFile(name: string): boolean {
  const lower = name.toLowerCase();
  return TEXT_EXTENSIONS.some((ext) => lower.endsWith(ext));
}

export default function Knowledge() {
  const [selected, setSelected] = useState<number | null>(null);
  const [query, setQuery] = useState("");
  const [searched, setSearched] = useState(false);
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [newDesc, setNewDesc] = useState("");
  const [busy, setBusy] = useState(false);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const { toasts, toast } = useToast();

  const { data: bases, loading: basesLoading, error: basesError, refresh: refreshBases } = useFetch(
    () => api.knowledge.list(),
    [],
  );

  const baseList = Array.isArray(bases) ? bases : [];
  const kb = baseList.find((k) => k.id === selected) ?? baseList[0];
  const kbId = kb?.id ?? null;

  // 文档明细来自独立接口：GET /api/knowledge-bases/:id/documents
  const {
    data: docsData,
    loading: docsLoading,
    refresh: refreshDocs,
  } = useFetch(
    () => (kbId ? api.knowledge.documents(kbId) : Promise.resolve([])),
    [kbId],
  );

  const { data: hits, loading: searchLoading, refresh: refreshSearch } = useFetch(
    () => {
      if (!searched || !kbId) return Promise.resolve([] as RetrievalHit[]);
      return api.knowledge.search(kbId, query, 5);
    },
    [searched, kbId, query],
  );

  const handleCreate = async () => {
    const name = newName.trim();
    if (!name) {
      toast("请填写知识库名称", "error");
      return;
    }
    setBusy(true);
    try {
      const created = await api.knowledge.create({ name, description: newDesc.trim() });
      toast(`已创建知识库「${created.name}」`, "success");
      setCreating(false);
      setNewName("");
      setNewDesc("");
      setSelected(created.id);
      refreshBases();
    } catch (e) {
      toast(`创建失败：${(e as Error).message}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const handleDelete = async () => {
    if (!kbId) return;
    setBusy(true);
    try {
      await api.knowledge.remove(kbId);
      toast("知识库已删除", "success");
      setSelected(null);
      refreshBases();
    } catch (e) {
      toast(`删除失败：${(e as Error).message}`, "error");
    } finally {
      setBusy(false);
    }
  };

  const handleFiles = async (files: FileList | null) => {
    if (!kbId || !files || files.length === 0) return;
    setBusy(true);
    try {
      for (const file of Array.from(files)) {
        if (!isTextFile(file.name)) {
          toast(`${file.name}：浏览器内暂只支持纯文本类文件`, "error");
          continue;
        }
        const content = await file.text();
        await api.knowledge.uploadDocument(kbId, { filename: file.name, content });
        toast(`${file.name} 已上传，后端正在异步索引`, "success");
      }
      refreshDocs();
    } catch (e) {
      toast(`上传失败：${(e as Error).message}`, "error");
    } finally {
      setBusy(false);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  };

  if (basesLoading) return <PageLoader text="加载知识库…" />;

  if (basesError) return <ErrorState title="加载知识库失败" description={basesError.message} onRetry={refreshBases} />;

  const docs = Array.isArray(docsData) ? docsData : [];
  const totalChunks = docs.reduce((s, d) => s + (numberOrNull(d.chunks) ?? 0), 0);
  const hitsList = Array.isArray(hits) ? hits : [];

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">知识库</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            文档上传 → 分块 → 向量化 → 余弦 Top-K 检索
          </p>
        </div>
        <Button variant="brand" icon={<Plus size={15} />} onClick={() => setCreating((v) => !v)}>
          新建知识库
        </Button>
      </div>

      {creating && (
        <Card>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="名称">
              <Input
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="例如：架构文档库"
              />
            </Field>
            <Field label="描述（可选）">
              <Input
                value={newDesc}
                onChange={(e) => setNewDesc(e.target.value)}
                placeholder="这个知识库放什么内容"
              />
            </Field>
          </div>
          <div className="mt-4 flex items-center gap-2">
            <Button variant="brand" size="sm" onClick={handleCreate} disabled={busy}>
              {busy ? "创建中…" : "创建"}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
              取消
            </Button>
          </div>
        </Card>
      )}

      {baseList.length === 0 ? (
        <EmptyState
          icon={<Database size={19} />}
          title="暂无知识库"
          description="创建一个知识库，然后上传 .md / .txt 文档即可开始检索。"
        />
      ) : (
        <div className="grid gap-4 lg:grid-cols-3">
          {/* 左：知识库列表 */}
          <div className="space-y-2.5">
            {baseList.map((k) => {
              const active = k.id === kb?.id;
              return (
                <button
                  key={k.id}
                  onClick={() => {
                    setSelected(k.id);
                    setSearched(false);
                  }}
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
                        {k.description || "暂无描述"}
                      </div>
                      <div className="mt-2 text-2xs text-fg-subtle">
                        创建于 {timeAgo(k.createdAt)}
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
                title={kb?.name ?? ""}
                subtitle={
                  docsLoading
                    ? "加载文档明细…"
                    : `${docs.length} 个文档 · ${totalChunks} 个分块`
                }
                action={
                  <div className="flex items-center gap-2">
                    <Badge tone="mint">已同步</Badge>
                    <Button
                      variant="ghost"
                      size="sm"
                      icon={<Trash2 size={13} />}
                      onClick={handleDelete}
                      disabled={busy}
                    >
                      删除
                    </Button>
                  </div>
                }
              />

              <div className="space-y-1.5">
                {docs.map((d) => (
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
                        {numberOrNull(d.sizeKb) ?? "—"} KB · {numberOrNull(d.chunks) ?? "—"} 分块 ·{" "}
                        {timeAgo(d.createdAt)}
                      </div>
                    </div>
                    <StatusBadge status={d.status} />
                  </div>
                ))}
                {!docsLoading && docs.length === 0 && (
                  <p className="py-2 text-2xs text-fg-subtle">
                    这个知识库还没有文档，上传一个 .md / .txt 试试。
                  </p>
                )}
              </div>

              {/* 上传区（真实上传：POST /api/knowledge-bases/:id/documents） */}
              <button
                type="button"
                disabled={busy}
                onClick={() => fileInputRef.current?.click()}
                className="mt-3.5 flex w-full flex-col items-center justify-center rounded-lg border border-dashed border-line bg-surface-2/40 px-4 py-6 transition-colors hover:border-brand/40 hover:bg-brand-soft/30"
              >
                <UploadCloud size={20} className="mb-2 text-fg-subtle" />
                <p className="text-xs text-fg-muted">
                  {busy ? "上传中…" : "点击选择文件上传"}
                </p>
                <p className="mt-1 text-2xs text-fg-subtle">
                  支持 .md / .txt / .csv / .json（浏览器内直接读文本）
                </p>
              </button>
              <input
                ref={fileInputRef}
                type="file"
                multiple
                accept=".txt,.md,.markdown,.text,.csv,.json,.log"
                className="hidden"
                onChange={(e) => void handleFiles(e.target.files)}
              />
            </Card>

            {/* 检索测试 */}
            <Card>
              <CardHeader
                title="检索测试"
                subtitle="GET /api/knowledge-bases/:id/retrieve?q=&k="
                action={<Badge tone="sky">Top-5</Badge>}
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
                    onKeyDown={(e) => {
                      if (e.key === "Enter") {
                        setSearched(true);
                        refreshSearch();
                      }
                    }}
                  />
                </div>
                <Button
                  variant="brand"
                  onClick={() => {
                    setSearched(true);
                    refreshSearch();
                  }}
                  disabled={!kbId}
                >
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
                ) : searchLoading ? (
                  <PageLoader text="检索中…" size="sm" />
                ) : hitsList.length === 0 ? (
                  <EmptyState
                    icon={<Search size={18} />}
                    title="没有召回结果"
                    description="换个问法，或先给这个知识库上传文档。"
                  />
                ) : (
                  <div className="space-y-2.5">
                    {hitsList.map((h, i) => {
                      // 检索接口的 score 是 omitempty；来源字段后端叫 filename
                      const score = numberOrNull(h.score);
                      return (
                        <div
                          key={h.id || i}
                          className="rounded-lg border border-line bg-surface-2/40 p-3.5 transition-colors hover:border-line-strong"
                        >
                          <div className="mb-2 flex items-center gap-2">
                            <span className="tnum flex h-5 w-5 items-center justify-center rounded bg-surface text-2xs font-medium text-fg-muted">
                              {i + 1}
                            </span>
                            <span className="truncate font-mono text-2xs text-fg-subtle">
                              {h.filename ?? "—"}
                            </span>
                            <div className="ml-auto flex items-center gap-2">
                              <Progress
                                value={score ?? 0}
                                tone="brand"
                                className="w-16"
                                height={4}
                              />
                              <span className="tnum text-2xs font-medium text-brand">
                                {score?.toFixed(2) ?? "—"}
                              </span>
                            </div>
                          </div>
                          <p className="text-xs leading-relaxed text-fg-muted">
                            {h.content}
                          </p>
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>
            </Card>
          </div>
        </div>
      )}

      <ToastContainer toasts={toasts} />
    </div>
  );
}

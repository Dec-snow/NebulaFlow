import { useEffect, useRef, useState } from "react";
import { FileText, Plus, Search, Trash2, Upload } from "lucide-react";
import { api } from "../api/client";
import type { Document, KnowledgeBase } from "../types";

const docStatus: Record<string, string> = {
  pending: "text-yellow-400 bg-yellow-400/10 border-yellow-400/30",
  indexed: "text-emerald-400 bg-emerald-400/10 border-emerald-400/30",
  failed: "text-red-400 bg-red-400/10 border-red-400/30",
};

export default function Knowledge() {
  const [kbs, setKbs] = useState<KnowledgeBase[]>([]);
  const [selected, setSelected] = useState<KnowledgeBase | null>(null);
  const [docs, setDocs] = useState<Document[]>([]);
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<{ content: string; score: number; chunk_index: number }[] | null>(null);
  const [newKB, setNewKB] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  const load = () => api.listKBs().then((r) => setKbs(r.knowledge_bases)).catch(() => {});
  useEffect(() => {
    load();
  }, []);

  useEffect(() => {
    if (selected) api.listDocs(selected.id).then((r) => setDocs(r.documents)).catch(() => {});
    else setDocs([]);
  }, [selected]);

  const createKB = async () => {
    if (!newKB.trim()) return;
    const kb = await api.createKB(newKB.trim(), "");
    setNewKB("");
    await load();
    setSelected(kb);
  };

  const removeKB = async (id: number) => {
    if (!confirm("删除知识库及其全部文档？")) return;
    await api.deleteKB(id);
    if (selected?.id === id) setSelected(null);
    load();
  };

  const upload = async (file: File) => {
    if (!selected) return;
    const text = await file.text();
    const doc = await api.uploadDoc(selected.id, text, file.name);
    setDocs((prev) => [doc, ...prev]);
  };

  const doRetrieve = async () => {
    if (!selected || !query.trim()) return;
    const r = await api.retrieve(selected.id, query, 5);
    setHits(r.hits);
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold text-slate-100">Knowledge Base</h2>
          <p className="text-xs text-slate-500 mt-1">上传文档 → 自动分块向量化 → 检索增强生成</p>
        </div>
        <div className="flex items-center gap-2">
          <input
            className="input max-w-[220px]"
            placeholder="新知识库名称…"
            value={newKB}
            onChange={(e) => setNewKB(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && createKB()}
          />
          <button className="btn-primary" onClick={createKB}>
            <Plus size={14} /> 创建
          </button>
        </div>
      </div>

      <div className="grid lg:grid-cols-4 gap-6">
        {/* 知识库列表 */}
        <div className="space-y-2">
          {kbs.map((kb) => (
            <div
              key={kb.id}
              className={`group flex items-center justify-between px-3 py-2.5 rounded-lg border cursor-pointer transition-colors ${
                selected?.id === kb.id
                  ? "bg-nebula-800 border-nebula-500"
                  : "bg-nebula-900 border-nebula-800 hover:border-nebula-600"
              }`}
              onClick={() => setSelected(kb)}
            >
              <div className="min-w-0">
                <div className="text-sm text-slate-200 truncate">{kb.name}</div>
                <div className="text-[10px] text-slate-500">#{kb.id}</div>
              </div>
              <button
                className="opacity-0 group-hover:opacity-100 text-slate-500 hover:text-red-400"
                onClick={(e) => {
                  e.stopPropagation();
                  removeKB(kb.id);
                }}
              >
                <Trash2 size={13} />
              </button>
            </div>
          ))}
          {kbs.length === 0 && <p className="text-xs text-slate-600">暂无知识库</p>}
        </div>

        {/* 文档管理 */}
        <div className="lg:col-span-2 card">
          <div className="flex items-center justify-between mb-4">
            <h3 className="text-sm font-medium text-slate-200">
              {selected ? `文档 · ${selected.name}` : "选择知识库"}
            </h3>
            {selected && (
              <button className="btn-ghost text-xs" onClick={() => fileRef.current?.click()}>
                <Upload size={13} /> 上传文档
              </button>
            )}
            <input
              ref={fileRef}
              type="file"
              accept=".txt,.md,.markdown,.pdf,.json"
              className="hidden"
              onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])}
            />
          </div>
          <div className="space-y-2">
            {!selected && <p className="text-xs text-slate-600">从左侧选择知识库</p>}
            {docs.map((d) => (
              <div key={d.id} className="flex items-center gap-3 px-3 py-2 rounded-lg bg-nebula-950 border border-nebula-800">
                <FileText size={14} className="text-nebula-400 shrink-0" />
                <span className="text-xs text-slate-200 flex-1 truncate">{d.filename}</span>
                <span className={`text-[9px] px-2 py-0.5 rounded-full border ${docStatus[d.status] || docStatus.pending}`}>
                  {d.status}
                </span>
              </div>
            ))}
            {selected && docs.length === 0 && <p className="text-xs text-slate-600">暂无文档，上传 txt/md 即可自动索引</p>}
          </div>
        </div>

        {/* 检索测试 */}
        <div className="card">
          <h3 className="text-sm font-medium text-slate-200 mb-3">RAG 检索测试</h3>
          <div className="flex gap-2">
            <input
              className="input"
              placeholder="检索问题…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && doRetrieve()}
            />
            <button className="btn-primary shrink-0" onClick={doRetrieve}>
              <Search size={13} />
            </button>
          </div>
          <div className="mt-3 space-y-2">
            {hits?.map((h, i) => (
              <div key={i} className="text-[11px] bg-nebula-950 rounded-lg p-2.5 border border-nebula-800">
                <div className="text-nebula-400 mb-1">
                  chunk #{h.chunk_index} · score {(h.score * 100).toFixed(1)}%
                </div>
                <div className="text-slate-400 line-clamp-3">{h.content}</div>
              </div>
            ))}
            {hits && hits.length === 0 && <p className="text-xs text-slate-600">无匹配结果</p>}
          </div>
        </div>
      </div>
    </div>
  );
}

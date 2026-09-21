import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { GitBranch, Plus, Trash2 } from "lucide-react";
import { api } from "../api/client";
import type { Workflow } from "../types";

export default function Workflows() {
  const [list, setList] = useState<Workflow[]>([]);
  const [loading, setLoading] = useState(true);
  const navigate = useNavigate();

  const load = () => api.listWorkflows().then((r) => setList(r.workflows)).finally(() => setLoading(false));

  useEffect(() => {
    load();
  }, []);

  const create = async () => {
    const wf = await api.createWorkflow({
      name: "未命名工作流",
      description: "",
      status: "draft",
      nodes: [],
      edges: [],
    });
    navigate(`/workflows/${wf.id}`);
  };

  const remove = async (e: React.MouseEvent, id: number) => {
    e.preventDefault();
    e.stopPropagation();
    if (!confirm("确定删除该工作流？")) return;
    await api.deleteWorkflow(id);
    load();
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold text-slate-100">Workflows</h2>
          <p className="text-xs text-slate-500 mt-1">DAG 工作流编排 · 拖拽节点，连接依赖，一键运行</p>
        </div>
        <button onClick={create} className="btn-primary">
          <Plus size={15} /> 新建工作流
        </button>
      </div>

      {loading ? (
        <p className="text-sm text-slate-500">加载中...</p>
      ) : list.length === 0 ? (
        <div className="card text-center py-16">
          <GitBranch className="mx-auto text-nebula-500 mb-3" size={32} />
          <p className="text-sm text-slate-400">还没有工作流</p>
          <button onClick={create} className="btn-primary mt-4">
            <Plus size={15} /> 创建第一个
          </button>
        </div>
      ) : (
        <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
          {list.map((w) => (
            <Link
              key={w.id}
              to={`/workflows/${w.id}`}
              className="card hover:border-nebula-500 transition-colors group relative"
            >
              <div className="flex items-start justify-between">
                <div className="min-w-0">
                  <h3 className="font-medium text-slate-100 truncate">{w.name}</h3>
                  <p className="text-xs text-slate-500 mt-1 line-clamp-2 min-h-[2rem]">{w.description || "暂无描述"}</p>
                </div>
                <button
                  onClick={(e) => remove(e, w.id)}
                  className="opacity-0 group-hover:opacity-100 text-slate-500 hover:text-red-400 transition-all"
                  title="删除"
                >
                  <Trash2 size={14} />
                </button>
              </div>
              <div className="flex items-center gap-3 mt-4 text-[10px] text-slate-500">
                <span className="px-2 py-0.5 rounded-full bg-nebula-800 border border-nebula-700">
                  {w.nodes?.length ?? 0} 节点
                </span>
                <span className="px-2 py-0.5 rounded-full bg-nebula-800 border border-nebula-700">
                  {w.edges?.length ?? 0} 边
                </span>
                <span className="ml-auto">{new Date(w.updated_at).toLocaleString("zh-CN")}</span>
              </div>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}

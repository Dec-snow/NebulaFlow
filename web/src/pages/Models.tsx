import { useEffect, useState } from "react";
import { Boxes, Cpu, Globe, KeyRound, ListOrdered, Power } from "lucide-react";
import { api } from "../api/client";
import type { Provider } from "../types";

export default function Models() {
  const [providers, setProviders] = useState<Provider[]>([]);

  useEffect(() => {
    api.listProviders().then((r) => setProviders(r.providers)).catch(() => {});
  }, []);

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-xl font-semibold text-slate-100">LLM Providers</h2>
        <p className="text-xs text-slate-500 mt-1">
          LLM Gateway 按优先级依次尝试，失败自动切换下一个（Primary → Fallback）
        </p>
      </div>

      <div className="grid md:grid-cols-2 xl:grid-cols-3 gap-4">
        {providers.map((p) => (
          <div key={p.id} className="card">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2">
                <Boxes size={16} className="text-nebula-400" />
                <span className="font-medium text-slate-100">{p.name}</span>
                {p.is_default && (
                  <span className="text-[9px] px-1.5 py-0.5 rounded bg-nebula-500/20 text-nebula-300 border border-nebula-500/40">
                    default
                  </span>
                )}
              </div>
              <div className="flex items-center gap-2">
                <span
                  className={`flex items-center gap-1 text-[10px] ${
                    p.enabled ? "text-emerald-400" : "text-slate-500"
                  }`}
                >
                  <Power size={11} /> {p.enabled ? "enabled" : "disabled"}
                </span>
                <span className="flex items-center gap-1 text-[10px] text-slate-500">
                  <ListOrdered size={11} /> priority {p.priority}
                </span>
              </div>
            </div>

            <div className="mt-3 space-y-1.5 text-[11px] text-slate-400">
              <div className="flex items-center gap-2">
                <Globe size={11} className="text-slate-600" />
                <span className="truncate">{p.base_url}</span>
              </div>
              <div className="flex items-center gap-2">
                <KeyRound size={11} className="text-slate-600" />
                <span>{p.api_key ? "••••" + p.api_key.slice(-4) : "无密钥"}</span>
              </div>
              <div className="flex items-center gap-2">
                <Cpu size={11} className="text-slate-600" />
                <span>{(p.models || []).map((m) => m.name).join(", ") || "无模型"}</span>
              </div>
            </div>
          </div>
        ))}
        {providers.length === 0 && (
          <p className="text-xs text-slate-600">
            暂无 Provider 配置。运行 <code className="text-nebula-400">go run ./cmd/seed</code> 初始化 mock / deepseek。
          </p>
        )}
      </div>

      <div className="card text-xs text-slate-500 leading-relaxed">
        <p className="font-medium text-slate-300 mb-2">故障转移链路示意</p>
        <pre className="bg-nebula-950 p-3 rounded-lg border border-nebula-800 text-slate-400">
{`DeepSeek ── timeout ──▶ Ollama ── error ──▶ Mock ──▶ Success
     ▲                                   ▲
     └────── primary（priority 最小）      └────── fallback 顺序执行

前端完全无感，仅收到事件: [fallback] 模型调用失败，正在切换备用模型…`}
        </pre>
      </div>
    </div>
  );
}

import { FormEvent, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Rocket } from "lucide-react";
import { api } from "../api/client";
import { useAuth } from "../store/auth";

export default function Login() {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const setAuth = useAuth((s) => s.setAuth);
  const navigate = useNavigate();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);
    try {
      const res =
        mode === "login"
          ? await api.login(username, password)
          : await api.register(username, email, password);
      setAuth(res.token, res.username, res.user_id);
      navigate("/");
    } catch (err: any) {
      setError(err.message || "请求失败");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-nebula-950 relative overflow-hidden">
      {/* 背景星云装饰 */}
      <div className="absolute inset-0 opacity-40">
        <div className="absolute w-[600px] h-[600px] rounded-full bg-nebula-500/20 blur-[120px] -top-40 -left-40" />
        <div className="absolute w-[500px] h-[500px] rounded-full bg-purple-500/10 blur-[120px] -bottom-40 -right-40" />
      </div>

      <div className="relative w-full max-w-sm">
        <div className="flex items-center justify-center gap-3 mb-8">
          <div className="w-3 h-3 rounded-full bg-nebula-400 animate-pulse" />
          <h1 className="text-2xl font-bold text-slate-100 tracking-widest">NebulaFlow</h1>
        </div>

        <form onSubmit={submit} className="card space-y-4">
          <div className="flex gap-1 p-1 bg-nebula-950 rounded-lg">
            {(["login", "register"] as const).map((m) => (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                className={`flex-1 py-2 rounded-md text-sm transition-colors ${
                  mode === m ? "bg-nebula-700 text-white" : "text-slate-400 hover:text-slate-200"
                }`}
              >
                {m === "login" ? "登录" : "注册"}
              </button>
            ))}
          </div>

          <div>
            <label className="label">用户名</label>
            <input className="input" value={username} onChange={(e) => setUsername(e.target.value)} required />
          </div>
          {mode === "register" && (
            <div>
              <label className="label">邮箱</label>
              <input
                className="input"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
              />
            </div>
          )}
          <div>
            <label className="label">密码</label>
            <input
              className="input"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              minLength={6}
            />
          </div>

          {error && <p className="text-xs text-red-400">{error}</p>}

          <button type="submit" disabled={loading} className="btn-primary w-full justify-center">
            <Rocket size={15} />
            {loading ? "处理中..." : mode === "login" ? "进入控制台" : "创建账号"}
          </button>

          <p className="text-[10px] text-slate-500 text-center">
            Build · Execute · Observe
          </p>
        </form>
      </div>
    </div>
  );
}

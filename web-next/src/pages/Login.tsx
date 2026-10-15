import { useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  ArrowRight,
  GitBranch,
  Lock,
  Mail,
  ShieldCheck,
  Sparkles,
  User,
  Zap,
} from "lucide-react";
import { Button, Field, Input, Segmented } from "@/components/ui";
import { BrandMark } from "@/components/shell/Brand";
import { ThemeToggle } from "@/components/shell/ThemeToggle";
import { USE_MOCK } from "@/api";
import { useSession } from "@/lib/session";

const FEATURES = [
  { icon: GitBranch, title: "DAG 编排引擎", desc: "入度驱动并行调度，失败自动向下游传播" },
  { icon: Zap, title: "背压与优雅退出", desc: "固定 Worker Pool，队列满即阻塞，不丢任务" },
  { icon: ShieldCheck, title: "可靠投递", desc: "Consumer Group + 崩溃认领 + 延迟重投 + DLQ" },
];

type Mode = "login" | "register";

/** 后端错误码 → 用户能看懂的文案（未知错误原样透出，方便排查）。 */
function humanizeError(message: string): string {
  const m = message.toLowerCase();
  if (m.includes("invalid credentials") || m.includes("unauthorized")) {
    return "用户名或密码不正确";
  }
  if (m.includes("already exists") || m.includes("duplicate")) {
    return "用户名或邮箱已被注册";
  }
  if (m.includes("rate limit") || m.includes("too many")) {
    return "尝试过于频繁，请稍后再试";
  }
  return message || "请求失败，请重试";
}

export default function Login() {
  const navigate = useNavigate();
  const location = useLocation();
  const login = useSession((s) => s.login);
  const register = useSession((s) => s.register);

  const [mode, setMode] = useState<Mode>("login");
  const [username, setUsername] = useState(USE_MOCK ? "demo" : "");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState(USE_MOCK ? "demo123456" : "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const from = (location.state as { from?: string } | null)?.from ?? "/";

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy) return;
    const name = username.trim();
    if (!name || !password) {
      setError("请填写用户名与密码");
      return;
    }
    if (mode === "register" && !email.trim()) {
      setError("注册需要填写邮箱");
      return;
    }

    setBusy(true);
    setError(null);
    try {
      if (mode === "login") await login(name, password);
      else await register({ username: name, email: email.trim(), password });
      navigate(from, { replace: true });
    } catch (err) {
      setError(humanizeError((err as Error)?.message ?? ""));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="app-ambient flex min-h-screen">
      {/* 左侧：品牌区 */}
      <div className="relative z-10 hidden flex-1 flex-col justify-between p-12 lg:flex">
        <div className="flex items-center gap-2.5">
          <BrandMark size={32} />
          <span className="text-base font-semibold tracking-tight text-fg">
            Nebula<span className="text-gradient">Flow</span>
          </span>
        </div>

        <div className="max-w-lg">
          <div className="badge badge-violet mb-5">
            <Sparkles size={11} />
            v1.0 · 高并发 Agent 平台
          </div>
          <h1 className="text-[2.6rem] font-semibold leading-[1.15] tracking-tight text-fg">
            把 Agent 工作流
            <br />
            当成分布式系统来做
          </h1>
          <p className="mt-5 text-[0.9375rem] leading-relaxed text-fg-muted">
            不是"调用大模型"，而是自己实现一套可讲清楚的 Workflow Engine：
            调度、并发控制、可靠投递、故障转移与可观测性。
          </p>

          <div className="mt-10 space-y-5">
            {FEATURES.map(({ icon: Icon, title, desc }) => (
              <div key={title} className="flex gap-3.5">
                <div className="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-brand-soft text-brand">
                  <Icon size={15} />
                </div>
                <div>
                  <div className="text-sm font-medium text-fg">{title}</div>
                  <div className="mt-0.5 text-xs text-fg-subtle">{desc}</div>
                </div>
              </div>
            ))}
          </div>
        </div>

        <p className="text-2xs text-fg-subtle">
          Go · PostgreSQL · Redis Stream · SSE · Prometheus
        </p>
      </div>

      {/* 右侧：表单 */}
      <div className="relative z-10 flex w-full flex-col items-center justify-center p-6 lg:w-[480px] lg:border-l lg:border-line lg:bg-surface/40">
        <div className="absolute right-6 top-6">
          <ThemeToggle />
        </div>

        <form onSubmit={submit} className="w-full max-w-sm animate-fade-up">
          <div className="mb-8 lg:hidden">
            <BrandMark size={36} />
          </div>

          <h2 className="text-xl font-semibold tracking-tight text-fg">
            {mode === "login" ? "登录控制台" : "创建账号"}
          </h2>
          <p className="mt-1.5 text-xs text-fg-subtle">
            {mode === "login"
              ? "使用你的 NebulaFlow 账号登录"
              : "注册后即可编排工作流并提交任务"}
          </p>

          <div className="mt-6">
            <Segmented
              value={mode}
              onChange={(v) => {
                setMode(v as Mode);
                setError(null);
              }}
              options={[
                { value: "login", label: "登录" },
                { value: "register", label: "注册" },
              ]}
            />
          </div>

          <div className="mt-5 space-y-4">
            <Field label="用户名">
              <div className="relative">
                <User
                  size={15}
                  className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                />
                <Input
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  placeholder="用户名（3–32 字符）"
                  className="pl-9"
                  autoComplete="username"
                />
              </div>
            </Field>

            {mode === "register" && (
              <Field label="邮箱">
                <div className="relative">
                  <Mail
                    size={15}
                    className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                  />
                  <Input
                    type="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder="you@example.com"
                    className="pl-9"
                    autoComplete="email"
                  />
                </div>
              </Field>
            )}

            <Field label="密码">
              <div className="relative">
                <Lock
                  size={15}
                  className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                />
                <Input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder="至少 6 位"
                  className="pl-9"
                  autoComplete={mode === "login" ? "current-password" : "new-password"}
                />
              </div>
            </Field>
          </div>

          {error && (
            <div className="mt-4 rounded-lg border border-rose/30 bg-rose/10 px-3.5 py-2.5 text-xs text-rose">
              {error}
            </div>
          )}

          <Button
            type="submit"
            variant="brand"
            block
            className="mt-6"
            disabled={busy}
            trailing={<ArrowRight size={15} />}
          >
            {busy ? "处理中…" : mode === "login" ? "进入控制台" : "注册并进入"}
          </Button>

          <div className="mt-6 rounded-lg border border-line bg-surface-2/60 px-3.5 py-3">
            <div className="text-2xs font-medium text-fg-muted">
              {USE_MOCK ? "演示模式（VITE_USE_MOCK=true）" : "真实后端"}
            </div>
            <div className="mt-1 font-mono text-2xs text-fg-subtle">
              {USE_MOCK
                ? "demo / demo123456"
                : "调用 POST /api/auth/login · 首个注册用户为 admin"}
            </div>
          </div>
        </form>
      </div>
    </div>
  );
}

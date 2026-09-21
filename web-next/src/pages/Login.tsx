import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { ArrowRight, GitBranch, Lock, ShieldCheck, Sparkles, User, Zap } from "lucide-react";
import { Button, Field, Input } from "@/components/ui";
import { BrandMark } from "@/components/shell/Brand";
import { ThemeToggle } from "@/components/shell/ThemeToggle";
import { useSession } from "@/lib/session";

const FEATURES = [
  { icon: GitBranch, title: "DAG 编排引擎", desc: "入度驱动并行调度，失败自动向下游传播" },
  { icon: Zap, title: "背压与优雅退出", desc: "固定 Worker Pool，队列满即阻塞，不丢任务" },
  { icon: ShieldCheck, title: "可靠投递", desc: "Consumer Group + 崩溃认领 + 延迟重投 + DLQ" },
];

export default function Login() {
  const navigate = useNavigate();
  const login = useSession((s) => s.login);
  const [username, setUsername] = useState("demo");
  const [password, setPassword] = useState("demo123456");

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    login(username.trim() || "demo");
    navigate("/");
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

          <h2 className="text-xl font-semibold tracking-tight text-fg">欢迎回来</h2>
          <p className="mt-1.5 text-xs text-fg-subtle">
            使用演示账号直接进入，或输入任意用户名体验
          </p>

          <div className="mt-7 space-y-4">
            <Field label="用户名">
              <div className="relative">
                <User
                  size={15}
                  className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                />
                <Input
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  placeholder="demo"
                  className="pl-9"
                  autoComplete="username"
                />
              </div>
            </Field>

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
                  placeholder="••••••••"
                  className="pl-9"
                  autoComplete="current-password"
                />
              </div>
            </Field>
          </div>

          <Button
            type="submit"
            variant="brand"
            block
            className="mt-6"
            trailing={<ArrowRight size={15} />}
          >
            进入控制台
          </Button>

          <div className="mt-6 rounded-lg border border-line bg-surface-2/60 px-3.5 py-3">
            <div className="text-2xs font-medium text-fg-muted">演示账号</div>
            <div className="mt-1 font-mono text-2xs text-fg-subtle">
              demo / demo123456
            </div>
          </div>
        </form>
      </div>
    </div>
  );
}

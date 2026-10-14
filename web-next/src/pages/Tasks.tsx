import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight, Filter, ListChecks, Search } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  EmptyState,
  ErrorState,
  Input,
  PageLoader,
  Segmented,
  StatusBadge,
} from "@/components/ui";
import { api } from "@/api";
import { useDebounce, useFetch } from "@/hooks";
import { fmtDuration, numberOrNull, timeAgo } from "@/lib/utils";

type Filter = "all" | "running" | "succeeded" | "failed";

export default function Tasks() {
  const navigate = useNavigate();
  const [filter, setFilter] = useState<Filter>("all");
  const [q, setQ] = useState("");
  const debouncedQ = useDebounce(q, 300);

  const { data, loading, error, refresh } = useFetch(
    () =>
      api.tasks.list({
        status: filter === "all" ? undefined : filter,
        q: debouncedQ || undefined,
      }),
    [filter, debouncedQ],
  );

  // items / total 可能缺失（后端只回数组或分页字段被省略）
  const list = Array.isArray(data?.items) ? data.items : [];
  const total = numberOrNull(data?.total) ?? 0;

  // 各状态数量（基于当前页数据近似）
  const pageList = list;
  const counts = {
    all: total,
    running: pageList.filter((t) => t.status === "running").length,
    succeeded: pageList.filter((t) => t.status === "succeeded").length,
    failed: pageList.filter((t) => t.status === "failed").length,
  };

  if (loading) return <PageLoader text="加载任务列表…" />;

  if (error) return <ErrorState title="加载任务列表失败" description={error.message} onRetry={refresh} />;

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold tracking-tight text-fg">任务</h2>
          <p className="mt-0.5 text-xs text-fg-subtle">
            共 {total} 条执行记录 · 点击查看节点明细
          </p>
        </div>
        <Button variant="outline" icon={<Filter size={14} />}>
          高级筛选
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <Segmented
          value={filter}
          onChange={setFilter}
          options={[
            { value: "all", label: "全部", count: counts.all },
            { value: "running", label: "运行中", count: counts.running },
            { value: "succeeded", label: "成功", count: counts.succeeded },
            { value: "failed", label: "失败", count: counts.failed },
          ]}
        />
        <div className="relative ml-auto w-full sm:w-64">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
          />
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="搜索任务号 / 工作流…"
            className="pl-8"
          />
        </div>
      </div>

      {list.length === 0 ? (
        <EmptyState
          icon={<ListChecks size={19} />}
          title="没有匹配的任务"
          description="换个筛选条件或关键词试试。"
          action={
            <Button
              variant="outline"
              onClick={() => {
                setQ("");
                setFilter("all");
              }}
            >
              清空筛选
            </Button>
          }
        />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th className="w-20">任务</th>
                <th>工作流</th>
                <th className="w-28">状态</th>
                <th className="w-24 text-right">耗时</th>
                <th className="w-24 text-right">Token</th>
                <th className="w-28">开始时间</th>
                <th className="w-10" />
              </tr>
            </thead>
            <tbody>
              {list.map((t) => {
                // tokens 不是后端 task 的字段（只在 task_node 上有），缺失时保持原来的 "—" 语义
                const tokens = numberOrNull(t.tokens);
                return (
                <tr
                  key={t.id}
                  onClick={() => navigate(`/tasks/${t.id}`)}
                  className="group cursor-pointer"
                >
                  <td>
                    <span className="tnum font-mono text-xs text-fg-subtle">
                      #{t.id}
                    </span>
                  </td>
                  <td>
                    <div className="text-xs font-medium text-fg">{t.workflow}</div>
                    {/* 摘要宽度跟着列宽走：max-w-md(448px) 会让 600px 宽的列右侧空出一大块，
                        放宽到 560px 后长输入也能充分利用横向空间。truncate 保留省略号兜底。 */}
                    <div className="mt-0.5 max-w-[560px] truncate text-2xs text-fg-subtle">
                      {t.input}
                    </div>
                  </td>
                  <td>
                    <StatusBadge status={t.status} />
                  </td>
                  <td className="tnum text-right text-xs">
                    {fmtDuration(t.durationMs)}
                  </td>
                  <td className="tnum text-right text-xs">
                    {tokens !== null && tokens > 0 ? tokens.toLocaleString() : "—"}
                  </td>
                  <td className="text-2xs text-fg-subtle">
                    {timeAgo(t.startedAt)}
                  </td>
                  <td>
                    <ChevronRight
                      size={14}
                      className="text-fg-subtle transition-transform group-hover:translate-x-0.5"
                    />
                  </td>
                </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <Card className="flex items-center gap-2.5">
        <Badge tone="sky">SSE</Badge>
        <p className="text-xs text-fg-muted">
          进入任务详情可订阅实时事件流：节点状态、token 逐字输出与最终结果。
        </p>
      </Card>
    </div>
  );
}

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/queue"
)

// 这组用例守的是全链路 trace 的**另一半**：worker 侧有没有把上游上下文接上。
//
// 只测"提交侧把 traceparent 写进了消息"是不够的——写了但没人读，
// 结果和没写一样，而单看提交侧测试会全绿。真正决定链路连不连得上的
// 是 executeTask 有没有从 job.TraceParent 还原上下文。

func withSchedulerTracing(t *testing.T) *tracing.Provider {
	t.Helper()
	p, err := tracing.Setup(context.Background(), tracing.Config{Enabled: true})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		// Setup 改进程级全局状态，用完必须还原，否则会串到同包其他用例
		if _, err := tracing.Setup(context.Background(), tracing.Config{Enabled: false}); err != nil {
			t.Fatalf("还原 tracing 全局状态失败: %v", err)
		}
	})
	return p
}

func traceSpanNames(tr *tracing.Trace) []string {
	out := make([]string, 0, len(tr.Spans))
	for _, s := range tr.Spans {
		out = append(out, s.Name)
	}
	return out
}

func traceHasSpan(tr *tracing.Trace, name string) bool {
	for _, s := range tr.Spans {
		if s.Name == name {
			return true
		}
	}
	return false
}

// waitTaskDone 轮询到任务进入终态。用轮询而不是固定 sleep：
// 固定 sleep 要么白等、要么在慢机器上偶发失败。
func waitTaskDone(t *testing.T, tk *model.Task, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if tk.Status != model.TaskPending && tk.Status != model.TaskRunning {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务在 %v 内未进入终态，当前 %s", timeout, tk.Status)
}

// waitTraceSpan 轮询到 trace 里出现指定 span 后返回快照。
//
// 为什么不能"任务终态了就直接查"：`task.execute` 的 span.End() 在 executeTask 的
// defer 里，而任务状态是在 defer **之前**写库的。两者之间有一个很短的窗口，
// 恰好查在窗口里就会看到"任务已完成，但 task.execute 还不存在"——
// 看起来像链路断了，其实是竞态。这个坑在端到端脚本里也踩过一次。
func waitTraceSpan(t *testing.T, store *tracing.SpanStore, traceID, name string, timeout time.Duration) *tracing.Trace {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		tr, ok := store.Snapshot(traceID)
		if ok && traceHasSpan(tr, name) {
			return tr
		}
		if time.Now().After(deadline) {
			if !ok {
				t.Fatalf("取不到 trace %s 的快照", traceID)
			}
			t.Fatalf("trace 里应出现 %s，实际 %v", name, traceSpanNames(tr))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func linearRig(t *testing.T) *testRig {
	t.Helper()
	nodes := []model.WorkflowNode{
		{NodeKey: "in", NodeType: model.NodeInput},
		{NodeKey: "llm", NodeType: model.NodeLLM, Config: model.NodeConfig{Model: "mock-chat"}},
		{NodeKey: "out", NodeType: model.NodeOutput},
	}
	edges := []model.WorkflowEdge{
		{SourceNode: "in", TargetNode: "llm"},
		{SourceNode: "llm", TargetNode: "out"},
	}
	return newTestRig(t, wfOf(1, 1, nodes, edges))
}

// 上游给了 traceparent → worker 必须续接，而不是自起一条新链。
func TestExecuteTaskContinuesUpstreamTrace(t *testing.T) {
	p := withSchedulerTracing(t)

	// 模拟 API 进程：入队那一刻起一个 span，把它编码成 traceparent。
	upCtx, up := tracing.StartInternal(context.Background(), tracing.SpanQueueEnqueue)
	tp := tracing.Inject(upCtx)
	upSpanID := up.SpanContext().SpanID().String()
	wantTrace := tracing.TraceID(upCtx)
	up.End()

	// 装置自检：上游没产出 traceparent 的话，下面的断言会退化成
	// "空串等于空串"，看起来全绿却什么都没验证。
	if tp == "" || upSpanID == "" || wantTrace == "" {
		t.Fatalf("装置自检失败：上游未产出有效上下文（tp=%q span=%q trace=%q）",
			tp, upSpanID, wantTrace)
	}

	rig := linearRig(t)
	rig.sched.SetTracing(p)

	tk := &model.Task{ID: 100, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "hi"}
	rig.tasks.seed(tk)

	// 必须走 consumeLoop 而不是直接调 executeTask：
	// `Extract(ctx, job.TraceParent)` 这一步在 consumeLoop 里，
	// 直接调 executeTask 会把被测的那一行整个跳过去，
	// 于是"写了 traceparent 但没人读"这个缺陷在测试里完全看不出来。
	consumeCtx, stopConsume := context.WithCancel(context.Background())
	defer stopConsume()
	go rig.sched.consumeLoop(consumeCtx, "c-trace-test")

	if err := rig.queue.Enqueue(context.Background(),
		queue.Job{TaskID: tk.ID, TraceParent: tp}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitTaskDone(t, tk, 5*time.Second)
	stopConsume()

	if tk.Status != model.TaskSucceeded {
		t.Fatalf("任务应成功，实际 %s（err=%s）", tk.Status, tk.Error)
	}

	store := p.Store()
	gotTrace, ok := store.TraceForTask(tk.ID)
	if !ok {
		t.Fatal("worker 侧应建立 task → trace 反查索引")
	}
	if gotTrace != wantTrace {
		t.Fatalf("worker 侧必须续接上游 trace %s，实际 %s（说明它另起了一条链）", wantTrace, gotTrace)
	}

	tr := waitTraceSpan(t, store, gotTrace, tracing.SpanTaskExecute, 2*time.Second)
	var exec *tracing.SpanRecord
	for i := range tr.Spans {
		if tr.Spans[i].Name == tracing.SpanTaskExecute {
			exec = &tr.Spans[i]
		}
	}
	if exec == nil {
		t.Fatalf("trace 里应有 %s span，实际 %v", tracing.SpanTaskExecute, traceSpanNames(tr))
	}
	if exec.ParentID != upSpanID {
		t.Fatalf("task.execute 的父 span 应是上游的 %s，实际 %q —— 跨进程父子关系断了",
			upSpanID, exec.ParentID)
	}
	// 节点 span 也要在同一条链上：只有外壳接上、里面还是断的，等于没接
	if !traceHasSpan(tr, tracing.SpanNodeExecute) {
		t.Fatalf("节点执行 span 应属于同一条 trace，实际 %v", traceSpanNames(tr))
	}
	// 归属必须记到任务所有者，否则查询接口会 fail-closed 把自己人也拒掉
	if owner, known := store.Owner(gotTrace); !known || owner != 1 {
		t.Fatalf("trace 归属应是用户 1，实际 owner=%d known=%v", owner, known)
	}
}

// 没有上游（tracing 之前写的旧消息、或提交侧未开 tracing）时，
// worker 应自起一条链**而不是彻底不可观测**，更不能因此让任务失败。
//
// 这条挡的是一个很容易写错的方向：把"取不到上游"当成错误处理，
// 于是可观测性反过来成了任务执行的前置条件。
func TestExecuteTaskStartsOwnTraceWhenNoUpstream(t *testing.T) {
	p := withSchedulerTracing(t)

	rig := linearRig(t)
	rig.sched.SetTracing(p)

	tk := &model.Task{ID: 101, WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "hi"}
	rig.tasks.seed(tk)

	if err := rig.sched.executeTask(context.Background(),
		queue.Job{TaskID: tk.ID}, "msg-2"); err != nil {
		t.Fatalf("没有 traceparent 不应影响任务执行: %v", err)
	}
	if tk.Status != model.TaskSucceeded {
		t.Fatalf("任务应正常成功，实际 %s（err=%s）", tk.Status, tk.Error)
	}

	gotTrace, ok := p.Store().TraceForTask(tk.ID)
	if !ok {
		t.Fatal("没有上游时 worker 应自起一条链，而不是完全不可观测")
	}
	tr, ok := p.Store().Snapshot(gotTrace)
	if !ok || tr.SpanCount == 0 {
		t.Fatalf("自起的链里应有 span，实际 ok=%v", ok)
	}
	if !traceHasSpan(tr, tracing.SpanTaskExecute) {
		t.Fatalf("自起的链里应有 %s，实际 %v", tracing.SpanTaskExecute, traceSpanNames(tr))
	}
}

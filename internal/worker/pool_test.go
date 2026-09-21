package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// echoExecutor 是测试用执行器：立即返回输出。
type echoExecutor struct {
	mu    atomic.Int64 // 并发峰值
	cur   atomic.Int64
	peak  atomic.Int64
	total atomic.Int64
}

func (e *echoExecutor) ExecuteNode(_ context.Context, j NodeJob) (NodeResult, error) {
	n := e.cur.Add(1)
	for {
		p := e.peak.Load()
		if n <= p || e.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer e.cur.Add(-1)
	time.Sleep(5 * time.Millisecond) // 模拟节点耗时
	e.total.Add(1)
	return NodeResult{Output: j.Input, Provider: "test"}, nil
}

// 100 个任务、10 个 worker：全部完成且并发峰值不超过 10。
func TestWorkerPool_100Tasks10Workers(t *testing.T) {
	ctx := context.Background()
	exec := &echoExecutor{}
	pool := NewWorkerPool(ctx, 10, 40, exec, slog.Default(), nil)
	pool.Start()

	const n = 100
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		i := i
		if !pool.Submit(NodeJob{
			TaskID: int64(i), NodeKey: fmt.Sprintf("n%d", i), Input: fmt.Sprintf("input-%d", i),
			OnDone: func(NodeResult, error) { done <- struct{}{} },
		}) {
			t.Fatalf("submit %d rejected", i)
		}
	}
	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for job %d", i)
		}
	}
	if exec.total.Load() != n {
		t.Fatalf("expected %d executions, got %d", n, exec.total.Load())
	}
	if peak := exec.peak.Load(); peak > 10 {
		t.Fatalf("concurrency exceeded worker count: peak=%d > 10", peak)
	}
	pool.Shutdown(5 * time.Second)
}

// 优雅关闭：提交期间 shutdown，之后 Submit 被拒绝。
func TestWorkerPool_GracefulShutdown(t *testing.T) {
	ctx := context.Background()
	exec := &echoExecutor{}
	pool := NewWorkerPool(ctx, 4, 16, exec, slog.Default(), nil)
	pool.Start()

	for i := 0; i < 20; i++ {
		if !pool.Submit(NodeJob{TaskID: int64(i), NodeKey: "x"}) {
			t.Fatalf("unexpected reject before shutdown")
		}
	}
	pool.Shutdown(5 * time.Second)
	if pool.Submit(NodeJob{TaskID: 999, NodeKey: "y"}) {
		t.Fatal("submit must be rejected after shutdown")
	}
	if got := exec.total.Load(); got == 0 {
		t.Fatal("expected some jobs to drain before shutdown")
	}
}

// 执行器出错不应导致 worker 崩溃。
func TestWorkerPool_ExecutorError(t *testing.T) {
	ctx := context.Background()
	exec := &failExecutor{}
	pool := NewWorkerPool(ctx, 2, 8, exec, slog.Default(), nil)
	pool.Start()
	if !pool.Submit(NodeJob{TaskID: 1, NodeKey: "bad"}) {
		t.Fatal("submit rejected")
	}
	time.Sleep(50 * time.Millisecond)
	if pool.Submit(NodeJob{TaskID: 2, NodeKey: "good"}) == false {
		t.Fatal("pool should still accept jobs after an executor error")
	}
	pool.Shutdown(3 * time.Second)
}

type failExecutor struct{}

func (failExecutor) ExecuteNode(_ context.Context, _ NodeJob) (NodeResult, error) {
	return NodeResult{}, fmt.Errorf("boom")
}

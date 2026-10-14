package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ctxAwareExecutor 记录节点是否观察到"被取消"，用来验证 Shutdown 真的能
// 唤醒在途节点（而不只是取消一个对显式 Ctx 无效的 poolCtx）。
type ctxAwareExecutor struct {
	started chan struct{}
	once    sync.Once
}

func (e *ctxAwareExecutor) ExecuteNode(ctx context.Context, _ NodeJob) (NodeResult, error) {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done() // 一个"只在 ctx 被取消时才返回"的节点
	return NodeResult{}, ctx.Err()
}

// 回归：Shutdown(timeout) 必须在 timeout 内返回，而不是被一个
// （只认自己那个显式 Ctx 的）在途节点无限拖住。
//
// 原实现超时后只 cancelFn()（= 取消 poolCtx），而 scheduler 永远显式传
// 任务级 Ctx，所以那句取消对真实节点无效；紧接着的 `<-done` 没有上限。
// 实测：Shutdown(500ms) 被拖到 3s 以上。
func TestShutdownCancelsRunningNodeContexts(t *testing.T) {
	poolCtx, cancelPool := context.WithCancel(context.Background())
	defer cancelPool()

	exec := &ctxAwareExecutor{started: make(chan struct{})}
	pool := NewWorkerPool(poolCtx, 2, 8, exec,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	pool.Start()

	// 关键：节点带自己的 ctx（模拟 scheduler 的行为），它与 poolCtx 无关
	nodeCtx, cancelNode := context.WithCancel(context.Background())
	defer cancelNode()
	done := make(chan struct{})
	pool.Submit(NodeJob{
		TaskID: 1, NodeKey: "slow", NodeType: model.NodeLLM, Ctx: nodeCtx,
		OnDone: func(NodeResult, error) { close(done) },
	})

	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("node never started")
	}

	start := time.Now()
	pool.Shutdown(300 * time.Millisecond)
	elapsed := time.Since(start)

	// 节点必须在强杀阶段被唤醒（它的 ctx 被取消），并且 Shutdown 有界返回
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("running node was not woken up by shutdown (its ctx was never cancelled)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown must honour its timeout, took %v", elapsed)
	}
}

// stubbornExecutor 完全不理会 ctx，也不返回——最坏情况。
// Shutdown 仍然必须有界返回（否则进程的优雅退出会被无限挂住）。
type stubbornExecutor struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *stubbornExecutor) ExecuteNode(context.Context, NodeJob) (NodeResult, error) {
	e.once.Do(func() { close(e.started) })
	<-e.release
	return NodeResult{}, nil
}

func TestShutdownReturnsEvenIfNodeIgnoresContext(t *testing.T) {
	poolCtx, cancelPool := context.WithCancel(context.Background())
	defer cancelPool()

	exec := &stubbornExecutor{started: make(chan struct{}), release: make(chan struct{})}
	pool := NewWorkerPool(poolCtx, 1, 4, exec,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	pool.Start()

	pool.Submit(NodeJob{TaskID: 2, NodeKey: "stubborn", NodeType: model.NodeLLM,
		Ctx: context.Background()})
	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("node never started")
	}

	start := time.Now()
	pool.Shutdown(200 * time.Millisecond)
	elapsed := time.Since(start)
	close(exec.release) // 放行被卡住的节点，避免测试结束时 goroutine 泄漏

	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown must return even when a node ignores ctx, took %v", elapsed)
	}
}

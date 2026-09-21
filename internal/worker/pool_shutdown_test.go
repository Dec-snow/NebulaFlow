package worker

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// 背压场景下 Shutdown 不能死锁：
// 原实现用 RWMutex，Submit 持读锁阻塞在满队列上，Shutdown 等写锁 → 互相等待。
// 这里把 worker 卡住（模拟慢节点），验证 Shutdown 能在超时后强制结束。
func TestWorkerPool_ShutdownUnderBackpressure(t *testing.T) {
	ctx := context.Background()
	exec := &blockingExecutor{}
	// 容量刻意设小：1 个 worker + 1 条队列，提交立即打满
	pool := NewWorkerPool(ctx, 1, 1, exec, slog.Default(), nil)
	pool.Start()

	// 先占住唯一的 worker（它会一直阻塞到 ctx 取消）。
	// 注意这里刻意不传 NodeJob.Ctx：节点继承 poolCtx，
	// 这样 Shutdown 超时后 cancelFn 才能真正打断它。
	// （若传一个永不取消的 context，节点将永远无法被中断——Go 里没有
	//  强制杀死 goroutine 的手段，这正是"节点必须尊重 ctx"的原因。）
	pool.Submit(NodeJob{TaskID: 0, NodeKey: "n"})

	// 等第一个节点真正进入执行
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && exec.started.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if exec.started.Load() == 0 {
		t.Fatal("executor never started")
	}

	// 再起一个 goroutine 持续提交，制造"Submit 阻塞在满队列上"的局面
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				pool.Submit(NodeJob{TaskID: 99, NodeKey: "blocked"})
			}
		}
	}()
	// 给后台提交者一点时间把队列塞满并卡住
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		pool.Shutdown(2 * time.Second)
		close(done)
	}()

	select {
	case <-done:
		// 期望：即使有卡住的节点，Shutdown 也能在超时后返回
	case <-time.After(20 * time.Second):
		close(stop)
		t.Fatal("Shutdown deadlocked while submitters were blocked")
	}
	close(stop)

	// 关闭后新提交必须被拒绝，且不会 panic
	if pool.Submit(NodeJob{TaskID: 1, NodeKey: "after"}) {
		t.Fatal("submit after shutdown must be rejected")
	}
}

// 节点 panic 不能让 worker 消失：池子必须继续消费后续任务。
func TestWorkerPool_PanicDoesNotKillWorker(t *testing.T) {
	ctx := context.Background()
	exec := &panicExecutor{}
	pool := NewWorkerPool(ctx, 2, 16, exec, slog.Default(), nil)
	pool.Start()

	done := make(chan struct{}, 3)
	for i := 0; i < 3; i++ {
		pool.Submit(NodeJob{
			TaskID: int64(i), NodeKey: "p", Ctx: ctx,
			OnDone: func(NodeResult, error) { done <- struct{}{} },
		})
	}
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("job %d never completed; worker likely died from panic", i)
		}
	}
	pool.Shutdown(3 * time.Second)
}

// Stats 的排队数必须反映真实队列长度，而不是"当前有多少个 Submit 在跑"。
func TestWorkerPool_StatsQueueLength(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	exec := &gatedExecutor{gate: release}
	pool := NewWorkerPool(ctx, 1, 16, exec, slog.Default(), nil)
	pool.Start()

	for i := 0; i < 5; i++ {
		pool.Submit(NodeJob{TaskID: int64(i), NodeKey: "n", Ctx: ctx})
	}
	// 等 worker 取走一个（active=1），其余应留在队列里排队。
	// 注意不能只等 queued：Submit 的入队是瞬时的，worker 可能还没取到任务。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a, q := pool.Stats()
		if a == 1 && q >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	active, queued := pool.Stats()
	if active != 1 {
		t.Fatalf("expected 1 active worker, got %d", active)
	}
	if queued < 4 {
		t.Fatalf("expected >=4 queued jobs, got %d", queued)
	}
	close(release)
	pool.Shutdown(3 * time.Second)
}

// ---------- 测试用执行器 ----------

type blockingExecutor struct{ started atomic.Int64 }

func (e *blockingExecutor) ExecuteNode(ctx context.Context, _ NodeJob) (NodeResult, error) {
	e.started.Add(1)
	<-ctx.Done()
	return NodeResult{}, ctx.Err()
}

type panicExecutor struct{}

func (panicExecutor) ExecuteNode(context.Context, NodeJob) (NodeResult, error) {
	panic("boom")
}

type gatedExecutor struct{ gate chan struct{} }

func (e *gatedExecutor) ExecuteNode(ctx context.Context, j NodeJob) (NodeResult, error) {
	select {
	case <-e.gate:
	case <-ctx.Done():
		return NodeResult{}, ctx.Err()
	}
	return NodeResult{Output: j.Input}, nil
}

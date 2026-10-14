package queue

import (
	"context"
	"testing"
	"time"
)

// 回归：内存队列的延迟重投不能绑定调用方的 ctx。
//
// 原实现把重投的发送挂在调用方 ctx 上，而调度器收尾路径传的是
// detached(ctx, 10s)（函数返回时立刻 cancel）。重投延迟至少 100ms，
// 远大于收尾耗时，于是 select 稳定走 ctx.Done 分支：
// **承诺了重试却一条都不重试，而且没有任何日志**。
// 内存模式（无 Redis 的演示 / 压测 / 单测模式）下，任务级重试与
// 锁竞争重投因此全部静默失效。
func TestInMemoryQueueNackAfterSurvivesCallerContextCancellation(t *testing.T) {
	q := NewInMemoryQueue(4)
	defer q.Close()

	if err := q.Enqueue(context.Background(), Job{TaskID: 7}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, msgID, err := q.Dequeue(context.Background(), "w1", time.Second)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	// 模拟调度器收尾时"已经被取消"的上下文
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := q.NackAfter(ctx, msgID, "lock busy", 1, 30*time.Millisecond); err != nil {
		t.Fatalf("NackAfter: %v", err)
	}

	select {
	case got := <-q.ch:
		if got.TaskID != 7 {
			t.Fatalf("requeued wrong job: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delayed requeue was lost when the caller's context was already cancelled")
	}
}

// 显式延迟的重投（等锁）不推进 Attempt：它不是一次"失败的执行尝试"，
// 推进 Attempt 会让队列的指数退避越滚越大（Redis 后端封顶 5 分钟），
// 表现为"提交后几分钟才出结果"。
func TestInMemoryQueueNackAfterDoesNotInflateAttempt(t *testing.T) {
	q := NewInMemoryQueue(4)
	defer q.Close()

	if err := q.Enqueue(context.Background(), Job{TaskID: 9}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, msgID, err := q.Dequeue(context.Background(), "w1", time.Second)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if err := q.NackAfter(context.Background(), msgID, "lock busy", 1, 10*time.Millisecond); err != nil {
		t.Fatalf("NackAfter: %v", err)
	}
	select {
	case got := <-q.ch:
		if got.Attempt != 0 {
			t.Fatalf("lock-contention requeue must not inflate Attempt, got %d", got.Attempt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job was not requeued")
	}

	// 对照：普通 Nack（真实失败重试）仍然要推进 Attempt
	if err := q.Enqueue(context.Background(), Job{TaskID: 10}); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	_, msgID2, err := q.Dequeue(context.Background(), "w1", time.Second)
	if err != nil {
		t.Fatalf("dequeue 2: %v", err)
	}
	if err := q.Nack(context.Background(), msgID2, "boom", 3); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	select {
	case got := <-q.ch:
		if got.Attempt != 1 {
			t.Fatalf("failure retry must increment Attempt, got %d", got.Attempt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed job was not requeued")
	}
}

// Nack 对未知消息必须报错（而不是静默成功）。调度器依赖这个错误判断
// "消息根本不在队列里"，静默成功会让"任务丢了"看起来像"重投成功"。
func TestInMemoryQueueNackUnknownMessageErrors(t *testing.T) {
	q := NewInMemoryQueue(4)
	defer q.Close()
	if err := q.Nack(context.Background(), "mem-does-not-exist", "boom", 3); err == nil {
		t.Fatal("Nack on an unknown message must return an error")
	}
	// 通过 NackAfter 走同一条路径
	if err := q.NackAfter(context.Background(), "mem-does-not-exist", "boom", 3, time.Second); err == nil {
		t.Fatal("NackAfter on an unknown message must return an error")
	}
}

// 实现 DelayNacker 是可选能力，但两个内置实现都必须提供：
// 调度器用类型断言发现它，缺失时会退回指数退避（等锁会被推得越来越远）。
func TestBuiltinQueuesImplementDelayNacker(t *testing.T) {
	var _ DelayNacker = NewInMemoryQueue(1)
	var _ DelayNacker = (*RedisStreamQueue)(nil)
}

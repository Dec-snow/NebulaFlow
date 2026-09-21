package queue

import (
	"context"
	"testing"
	"time"
)

// Nack(retryLeft>0) 必须把消息延迟重新投递，且 attempt 递增。
func TestInMemoryQueue_NackRequeuesWithBackoff(t *testing.T) {
	ctx := context.Background()
	q := NewInMemoryQueue(8)
	if err := q.Enqueue(ctx, Job{TaskID: 42}); err != nil {
		t.Fatal(err)
	}
	job, id, err := q.Dequeue(ctx, "w1", time.Second)
	if err != nil || job.TaskID != 42 {
		t.Fatalf("dequeue: %v %v", job, err)
	}
	if id == "" {
		t.Fatal("expected unique message id")
	}
	if err := q.Nack(ctx, id, "boom", 2); err != nil {
		t.Fatalf("nack: %v", err)
	}
	// 重投后 attempt 应递增
	job2, id2, err := q.Dequeue(ctx, "w1", 3*time.Second)
	if err != nil {
		t.Fatalf("expected requeued message, got %v", err)
	}
	if job2.TaskID != 42 || job2.Attempt != 1 {
		t.Fatalf("expected same task with attempt=1, got %+v", job2)
	}
	if id2 == id {
		t.Fatal("requeued message should get a new id")
	}
}

// Nack(retryLeft<=0) 表示重试用尽：消息不再回到队列（等价于进入 DLQ）。
func TestInMemoryQueue_NackExhaustedDrops(t *testing.T) {
	ctx := context.Background()
	q := NewInMemoryQueue(8)
	_ = q.Enqueue(ctx, Job{TaskID: 1})
	_, id, _ := q.Dequeue(ctx, "w1", time.Second)
	if err := q.Nack(ctx, id, "dead", 0); err == nil {
		t.Fatal("expected error when retries exhausted")
	}
	if _, _, err := q.Dequeue(ctx, "w1", 200*time.Millisecond); err != ErrNoMessage {
		t.Fatalf("dropped message must not be redelivered, got %v", err)
	}
}

// Ack 之后消息不应再出现。
func TestInMemoryQueue_AckRemovesMessage(t *testing.T) {
	ctx := context.Background()
	q := NewInMemoryQueue(8)
	_ = q.Enqueue(ctx, Job{TaskID: 5})
	_, id, _ := q.Dequeue(ctx, "w1", time.Second)
	if err := q.Ack(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.Nack(ctx, id, "late", 1); err == nil {
		t.Fatal("nack on acked message should fail")
	}
}

// Dequeue 无消息时应返回 ErrNoMessage 而不是阻塞。
func TestInMemoryQueue_DequeueTimeout(t *testing.T) {
	q := NewInMemoryQueue(1)
	start := time.Now()
	if _, _, err := q.Dequeue(context.Background(), "w1", 100*time.Millisecond); err != ErrNoMessage {
		t.Fatalf("expected ErrNoMessage, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dequeue blocked too long: %v", elapsed)
	}
}

// 退避时长必须随 attempt 指数增长并封顶，避免无限放大。
func TestBackoffFor(t *testing.T) {
	q := &RedisStreamQueue{baseRetry: 2 * time.Second}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{99, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := q.backoffFor(c.attempt); got != c.want {
			t.Fatalf("attempt %d: got %v want %v", c.attempt, got, c.want)
		}
	}
}

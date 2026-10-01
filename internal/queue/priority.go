package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// PriorityQueue 是支持三级优先级的队列。
//
// 内部维护三个子队列（high / normal / low），
// 出队时按权重轮询（默认 3:2:1），保证：
//   - 高优先级任务先处理
//   - 低优先级任务不会"饿死"（永远轮不到）
//
// 为什么不直接用"先出 high，high 空了出 normal"：
//   如果 high 队列一直有任务，low 队列的任务永远得不到执行，
//   这叫"饥饿问题"。权重轮询是业界标准解法。
type PriorityQueue struct {
	high   Queue
	normal Queue
	low    Queue
	// 轮询状态：当前在哪个优先级、已经取了几个
	pos    int // 0=high, 1=normal, 2=low
	count  int // 当前优先级已取的数量
}

// NewPriorityQueue 创建一个优先级队列。
// 三个子队列可以是 Redis Stream（生产）或 InMemory（测试）。
func NewPriorityQueue(high, normal, low Queue) *PriorityQueue {
	return &PriorityQueue{
		high:   high,
		normal: normal,
		low:    low,
	}
}

// Enqueue 按优先级入队。
// priority 非法时自动归一化为 normal。
func (pq *PriorityQueue) Enqueue(ctx context.Context, priority string, j Job) error {
	switch model.NormalizePriority(priority) {
	case model.PriorityHigh:
		return pq.high.Enqueue(ctx, j)
	case model.PriorityLow:
		return pq.low.Enqueue(ctx, j)
	default:
		return pq.normal.Enqueue(ctx, j)
	}
}

// Dequeue 从优先级队列中取出一条消息。
//
// 调度策略：加权轮询（Weighted Round Robin）
//   high 权重 3 → 连续取 3 条
//   normal 权重 2 → 连续取 2 条
//   low 权重 1 → 取 1 条
//   然后循环
//
// 如果当前优先级队列为空，跳到下一个优先级。
// 如果所有队列都为空，阻塞等待 block 时间。
func (pq *PriorityQueue) Dequeue(ctx context.Context, workerID string, block time.Duration) (Job, string, string, error) {
	deadline := time.Now().Add(block)
	queues := []struct {
		name   string
		q      Queue
		weight int
	}{
		{model.PriorityHigh, pq.high, model.PriorityWeight(model.PriorityHigh)},
		{model.PriorityNormal, pq.normal, model.PriorityWeight(model.PriorityNormal)},
		{model.PriorityLow, pq.low, model.PriorityWeight(model.PriorityLow)},
	}

	// 从当前位置开始，轮询所有优先级
	// 最多扫三轮（避免无限循环）
	for round := 0; round < 3; round++ {
		for i := 0; i < len(queues); i++ {
			idx := (pq.pos + i) % len(queues)
			q := queues[idx]

			// 当前优先级还有配额吗
			if pq.pos == idx && pq.count >= q.weight {
				// 配额用完，跳到下一个
				pq.pos = (idx + 1) % len(queues)
				pq.count = 0
				continue
			}

			// 尝试从这个队列取一条（非阻塞）
			remaining := time.Until(deadline)
			if remaining <= 0 {
				remaining = 0
			}
			job, msgID, err := q.q.Dequeue(ctx, workerID, 0)
			if err == nil {
				// 取到了
				if pq.pos == idx {
					pq.count++
				} else {
					pq.pos = idx
					pq.count = 1
				}
				return job, msgID, q.name, nil
			}
			if err != ErrNoMessage {
				return Job{}, "", "", err
			}

			// 这个队列空了，跳到下一个
			if pq.pos == idx {
				pq.pos = (idx + 1) % len(queues)
				pq.count = 0
			}
		}

		// 一轮扫完都没有消息，如果还有时间就等一会儿再试
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		// 等一小段时间再试（避免空转 CPU）
		wait := 50 * time.Millisecond
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return Job{}, "", "", ctx.Err()
		case <-time.After(wait):
		}
	}

	return Job{}, "", "", ErrNoMessage
}

// Ack 确认消息。priority 决定去哪个队列 Ack。
func (pq *PriorityQueue) Ack(ctx context.Context, priority string, msgID string) error {
	switch model.NormalizePriority(priority) {
	case model.PriorityHigh:
		return pq.high.Ack(ctx, msgID)
	case model.PriorityLow:
		return pq.low.Ack(ctx, msgID)
	default:
		return pq.normal.Ack(ctx, msgID)
	}
}

// Nack 拒绝消息。retryLeft > 0 时重试，否则入 DLQ。
func (pq *PriorityQueue) Nack(ctx context.Context, priority string, msgID string, errMsg string, retryLeft int) error {
	switch model.NormalizePriority(priority) {
	case model.PriorityHigh:
		return pq.high.Nack(ctx, msgID, errMsg, retryLeft)
	case model.PriorityLow:
		return pq.low.Nack(ctx, msgID, errMsg, retryLeft)
	default:
		return pq.normal.Nack(ctx, msgID, errMsg, retryLeft)
	}
}

// Recover 认领遗留消息（崩溃恢复）。
func (pq *PriorityQueue) Recover(ctx context.Context, workerID string, count int64) (int, error) {
	total := 0
	for _, q := range []Queue{pq.high, pq.normal, pq.low} {
		n, err := q.Recover(ctx, workerID, count)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Len 返回各优先级的队列长度。
func (pq *PriorityQueue) Len(ctx context.Context) (high, normal, low int64, err error) {
	high, err = pq.high.Len(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("high: %w", err)
	}
	normal, err = pq.normal.Len(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("normal: %w", err)
	}
	low, err = pq.low.Len(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("low: %w", err)
	}
	return high, normal, low, nil
}

// SubQueue 暴露子队列（用于 StreamStats 等扩展接口）。
func (pq *PriorityQueue) SubQueue(priority string) Queue {
	switch model.NormalizePriority(priority) {
	case model.PriorityHigh:
		return pq.high
	case model.PriorityLow:
		return pq.low
	default:
		return pq.normal
	}
}

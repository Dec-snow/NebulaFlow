package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 这一组用例覆盖 RedisStreamQueue——此前它零测试（queue_test.go 只测了 InMemoryQueue），
// 而 P0-2 的缺陷正好就在它身上：4 处 XAdd 都没有容量水位，stream 只进不出。
//
// 需要真实 Redis（或 RESP 兼容实现），默认跳过：
//
//	NEBULA_TEST_REDIS_ADDR=127.0.0.1:6379 go test ./internal/queue -run RedisStream -v
//
// 每个用例用独立的 stream 名并在结束时清理，不会污染正在跑的服务。
func newTestQueue(t *testing.T, opt Options) (*RedisStreamQueue, context.Context, func()) {
	t.Helper()
	addr := os.Getenv("NEBULA_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("未设置 NEBULA_TEST_REDIS_ADDR，跳过需要真实 Redis 的集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		cancel()
		t.Fatalf("连接 Redis %s: %v", addr, err)
	}

	// 唯一 stream 名：避免与正在运行的服务、以及并发用例互相干扰
	stream := fmt.Sprintf("nebula_it_%d", time.Now().UnixNano())
	q, err := NewRedisStreamQueue(rdb, stream, opt)
	if err != nil {
		cancel()
		_ = rdb.Close()
		t.Fatalf("创建队列: %v", err)
	}

	cleanup := func() {
		// 用独立的 ctx：用例超时后 ctx 已取消，否则清理会失败
		c := context.Background()
		_ = rdb.Del(c, stream, stream+"_dlq", stream+"_retry").Err()
		_ = rdb.Close()
		cancel()
	}
	return q, ctx, cleanup
}

// P0-2 的核心断言：持续入队后 stream 的**物理长度**必须收敛到 MaxLen 量级，
// 而不是随入队次数线性增长。
//
// 没有 MaxLen 时这个用例会直接失败（XLEN = probe）。
//
// AdmitRatio 必须显式关掉：这条用例要验的是**最后一道护栏**（MAXLEN 在 XADD 时刻裁剪），
// 而默认准入线只有 MaxLen 的 80%，不关的话第 400 条就会被 ErrQueueFull 拦下，
// 根本走不到 MAXLEN 那条路径——测的就不是同一个机制了。
func TestRedisStreamQueue_MaxLenBoundsStream(t *testing.T) {
	const maxLen = 500
	const probe = 20000
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: maxLen, DLQMaxLen: 100, AdmitRatio: -1})
	defer cleanup()

	for i := 0; i < probe; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("第 %d 条入队失败: %v", i, err)
		}
	}

	n, err := q.StreamLen(ctx)
	if err != nil {
		t.Fatalf("XLEN: %v", err)
	}
	t.Logf("入队 %d 条后 XLEN=%d（MaxLen=%d）", probe, n, maxLen)

	// 宽松上界：近似裁剪（MAXLEN ~）只裁完整宏节点，真 Redis 下实际长度会略高于 MaxLen。
	// 取 4 倍是为了对两种实现都成立，同时仍然能抓住"完全没裁剪"这个缺陷。
	if n > maxLen*4 {
		t.Fatalf("XLEN=%d 远超 MaxLen=%d 的合理上界（%d），说明容量水位没生效",
			n, maxLen, maxLen*4)
	}

	// 裁剪必须裁掉**最老的**条目、保住最新的：否则新任务会被立刻丢掉，队列就废了。
	last, err := q.rdb.XRevRangeN(ctx, q.stream, "+", "-", 1).Result()
	if err != nil {
		t.Fatalf("XREVRANGE: %v", err)
	}
	if len(last) != 1 {
		t.Fatalf("期望至少 1 条最新消息，拿到 %d 条", len(last))
	}
	payload, _ := last[0].Values["payload"].(string)
	want := fmt.Sprintf(`{"task_id":%d}`, probe-1)
	if payload != want {
		t.Fatalf("最新一条消息应为 %s，实际 %s", want, payload)
	}
}

// DLQ 同样必须有水位：它不会被消费，不给上限就是永久增长。
//
// 探测量必须远大于上界（这里 2000 vs 上界 400），否则去掉水位也照样能通过——
// 这一点是变异测试抓出来的：最初用 300 条探测、上界 400，去掉 MaxLen 后用例仍然 PASS。
//
// 同样要关掉准入：主 stream 的 MaxLen=1000 → 准入线 800，而这条用例要往主 stream
// 灌 2000 条，不关就在第 800 条被拦下，测不到 DLQ 的水位。
func TestRedisStreamQueue_DLQBounded(t *testing.T) {
	const dlqMaxLen = 100
	const n = 2000
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: 1000, DLQMaxLen: dlqMaxLen, AdmitRatio: -1})
	defer cleanup()

	for i := 0; i < n; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("入队: %v", err)
		}
		_, msgID, err := q.Dequeue(ctx, "w1", time.Second)
		if err != nil {
			t.Fatalf("第 %d 次出队: %v", i, err)
		}
		// retryLeft=0 → 重试用尽，进 DLQ
		if err := q.Nack(ctx, msgID, "boom", 0); err != nil {
			t.Fatalf("第 %d 次 Nack: %v", i, err)
		}
	}

	got, err := q.DLQLen(ctx)
	if err != nil {
		t.Fatalf("DLQ XLEN: %v", err)
	}
	t.Logf("送入 DLQ %d 条后 XLEN=%d（DLQMaxLen=%d）", n, got, dlqMaxLen)
	if got > dlqMaxLen*4 {
		t.Fatalf("DLQ 长度 %d 超过上界（%d），说明死信 stream 没限长", got, dlqMaxLen*4)
	}
	if got == 0 {
		t.Fatal("DLQ 为空，Nack(retryLeft=0) 没有把消息送进死信队列")
	}
}

// 容量水位不能破坏正常消费：每条消息仍应恰好被投递一次、可被 ACK。
func TestRedisStreamQueue_MaxLenDoesNotAffectConsumption(t *testing.T) {
	const n = 200
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: 1000, DLQMaxLen: 100})
	defer cleanup()

	for i := 0; i < n; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("入队: %v", err)
		}
	}

	seen := make(map[int64]int, n)
	for len(seen) < n {
		job, msgID, err := q.Dequeue(ctx, "w1", 2*time.Second)
		if err == ErrNoMessage {
			t.Fatalf("只收到 %d/%d 条消息就空了", len(seen), n)
		}
		if err != nil {
			t.Fatalf("出队: %v", err)
		}
		seen[job.TaskID]++
		if err := q.Ack(ctx, msgID); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("task %d 被投递了 %d 次，应为 1 次", id, c)
		}
	}

	// 全部 ACK 之后，消费组的 pending 必须归零——这是"ACK 真的生效"的直接证据。
	//
	// 这里刻意**不**断言 queue.Len()==0：Len() 把组的 lag 也算进积压量，
	// 而 miniredis 对 XINFO GROUPS 的 lag 报的是 stream 全长（本例实测 lag=200、
	// pending=0），真 Redis 此时 lag 应为 0。这是 miniredis 的已知差异，
	// 与本用例要验证的容量水位无关，所以只对 pending 做硬断言。
	groups, err := q.rdb.XInfoGroups(ctx, q.stream).Result()
	if err != nil {
		t.Fatalf("XINFO GROUPS: %v", err)
	}
	if len(groups) == 0 {
		t.Fatal("消费组不存在")
	}
	for _, g := range groups {
		t.Logf("group=%s consumers=%d pending=%d lag=%d", g.Name, g.Consumers, g.Pending, g.Lag)
		if g.Pending != 0 {
			t.Fatalf("全部 ACK 后 pending 应为 0，实际 %d（ACK 未生效）", g.Pending)
		}
	}
}

// 安全默认值：传零值 Options 也必须是有上界的，绝不能退化成"无限增长"。
func TestRedisStreamQueue_ZeroOptionsStaysBounded(t *testing.T) {
	q, _, cleanup := newTestQueue(t, Options{})
	defer cleanup()

	if q.maxLen != defaultMaxLen {
		t.Fatalf("零值 Options 下 maxLen 应为默认 %d，实际 %d", defaultMaxLen, q.maxLen)
	}
	if q.dlqMaxLen != defaultDLQMaxLen {
		t.Fatalf("零值 Options 下 dlqMaxLen 应为默认 %d，实际 %d", defaultDLQMaxLen, q.dlqMaxLen)
	}
	if q.maxLen <= 0 {
		t.Fatal("maxLen 不能为 0——那等于没有水位")
	}
}

// 延迟重投回流的消息也必须走带水位的 XAdd，否则重投路径会绕过容量控制。
func TestRedisStreamQueue_RequeuePathsAreBounded(t *testing.T) {
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: 1000, DLQMaxLen: 100})
	defer cleanup()

	// 把退避时间压到 0（必须在 Nack 之前设置：score 是在 Nack 时按 baseRetry 算出来的）
	q.baseRetry = time.Nanosecond

	if err := q.Enqueue(ctx, Job{TaskID: 7}); err != nil {
		t.Fatalf("入队: %v", err)
	}
	_, msgID, err := q.Dequeue(ctx, "w1", time.Second)
	if err != nil {
		t.Fatalf("出队: %v", err)
	}
	// retryLeft>0 → 进延迟队列（ZSET），不写 stream
	if err := q.Nack(ctx, msgID, "retry me", 2); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	if n, err := q.PendingRetries(ctx); err != nil || n != 1 {
		t.Fatalf("延迟重投队列应有 1 条，实际 %d (err=%v)", n, err)
	}

	if err := q.promoteDueRetries(ctx, 32); err != nil {
		t.Fatalf("promoteDueRetries: %v", err)
	}
	if n, _ := q.PendingRetries(ctx); n != 0 {
		t.Fatalf("重投后 ZSET 应清空，实际 %d", n)
	}

	// 搬回来的是新消息，attempt 已递增
	job, _, err := q.Dequeue(ctx, "w1", 2*time.Second)
	if err != nil {
		t.Fatalf("重投后应能再次出队: %v", err)
	}
	if job.TaskID != 7 || job.Attempt != 1 {
		t.Fatalf("期望 task=7 attempt=1，实际 %+v", job)
	}
}

// ---------- TrimConsumed：安全回收「已确认前缀」 ----------
//
// 这一组用例对应 P0-2 压测暴露的第二个问题：
// MAXLEN 裁掉的是最老的条目，而那可能正是"还没被任何消费者读走"的消息。
// 实测（2 万条突发）：MaxLen=10000 落地率 52.28%、MaxLen=1000 落地率 7.63%，
// 差额全部是被静默裁掉的未消费消息。
//
// TrimConsumed 只删「已投递且已确认」的前缀，因此下面三个用例分别在验证：
//  1. 已 ACK 的前缀会被回收（否则等于没修）；
//  2. 未读消息一条都不能少（这是与 MAXLEN 的本质区别）；
//  3. 已投递但未 ACK 的消息也不能删（它们还在 PEL 里，删了就真丢了）。

// consume 从队列取 n 条并返回取到的 taskID；ack 决定取到后是否 ACK。
func consume(t *testing.T, q *RedisStreamQueue, ctx context.Context, n int, ack bool) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		job, msgID, err := q.Dequeue(ctx, "trim-test", 500*time.Millisecond)
		if err != nil {
			t.Fatalf("第 %d 次出队失败（已取到 %d 条）: %v", i, len(ids), err)
		}
		ids = append(ids, job.TaskID)
		if ack {
			if err := q.Ack(ctx, msgID); err != nil {
				t.Fatalf("ACK %s: %v", msgID, err)
			}
		}
	}
	return ids
}

func xlen(t *testing.T, q *RedisStreamQueue, ctx context.Context) int64 {
	t.Helper()
	n, err := q.StreamLen(ctx)
	if err != nil {
		t.Fatalf("XLEN: %v", err)
	}
	return n
}

// 已投递且已 ACK 的前缀必须被回收，否则 stream 依然是只进不出。
func TestRedisStreamQueue_TrimConsumedReclaimsAckedPrefix(t *testing.T) {
	const total = 5000
	// MaxLen 故意设得远大于 total：本用例只验证回收路径，
	// 不能让 MAXLEN 的裁剪混进来（否则分不清 XLEN 变小是谁干的）。
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: 10_000_000, DLQMaxLen: 100})
	defer cleanup()

	for i := 0; i < total; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("入队: %v", err)
		}
	}
	consume(t, q, ctx, total, true)

	before := xlen(t, q, ctx)
	if before != total {
		t.Fatalf("全部 ACK 后 XLEN 应为 %d（XACK 不删消息本体），实际 %d", total, before)
	}

	removed, err := q.TrimConsumed(ctx)
	if err != nil {
		t.Fatalf("TrimConsumed: %v", err)
	}
	after := xlen(t, q, ctx)
	t.Logf("全部 ACK 后：XLEN %d -> %d（回收 %d 条）", before, after, removed)

	if after != 0 {
		t.Fatalf("已确认前缀应被完全回收，XLEN 仍为 %d", after)
	}
	if removed != total {
		t.Fatalf("应回收 %d 条，实际 %d", total, removed)
	}
}

// 关键用例：未被读走的消息，回收后必须一条不少。
//
// 这正是 MAXLEN 做不到的——把上面这行改成依赖 MAXLEN（MaxLen=1000）时，
// 未读消息会被裁掉，后续 consume 拿不到足量消息直接失败。
func TestRedisStreamQueue_TrimConsumedKeepsUnreadMessages(t *testing.T) {
	const total = 5000
	const readFirst = 1000
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: 10_000_000, DLQMaxLen: 100})
	defer cleanup()

	for i := 0; i < total; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("入队: %v", err)
		}
	}
	first := consume(t, q, ctx, readFirst, true)
	for i, id := range first {
		if id != int64(i) {
			t.Fatalf("第 %d 条应是 task %d，实际 %d（投递乱序）", i, i, id)
		}
	}

	removed, err := q.TrimConsumed(ctx)
	if err != nil {
		t.Fatalf("TrimConsumed: %v", err)
	}
	after := xlen(t, q, ctx)
	t.Logf("读走并 ACK 前 %d 条后：回收 %d 条，XLEN=%d", readFirst, removed, after)

	if after != total-readFirst {
		t.Fatalf("未读的 %d 条必须原样保留，XLEN 实际 %d", total-readFirst, after)
	}

	// 把剩下的全部读出来，逐条核对：一条都不能少、不能重。
	rest := consume(t, q, ctx, total-readFirst, true)
	if len(rest) != total-readFirst {
		t.Fatalf("剩余应有 %d 条，实际取到 %d", total-readFirst, len(rest))
	}
	for i, id := range rest {
		want := int64(readFirst + i)
		if id != want {
			t.Fatalf("第 %d 条剩余消息应是 task %d，实际 %d —— 回收过程丢了消息", i, want, id)
		}
	}
}

// 已投递但**未 ACK** 的消息仍在 PEL 里，回收位点不能越过它们，
// 否则消费者崩溃后 XAUTOCLAIM 就没有可认领的条目了（任务真丢）。
func TestRedisStreamQueue_TrimConsumedStopsAtPending(t *testing.T) {
	const total = 1000
	const inflight = 400
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: 10_000_000, DLQMaxLen: 100})
	defer cleanup()

	for i := 0; i < total; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("入队: %v", err)
		}
	}
	// 取 400 条但不 ACK：它们进入 PEL，成为回收位点的下限
	consume(t, q, ctx, inflight, false)

	removed, err := q.TrimConsumed(ctx)
	if err != nil {
		t.Fatalf("TrimConsumed: %v", err)
	}
	if n := xlen(t, q, ctx); n != total {
		t.Fatalf("有未 ACK 的在途消息时不应回收任何条目，XLEN 应仍为 %d，实际 %d（回收了 %d）",
			total, n, removed)
	}
	if removed != 0 {
		t.Fatalf("回收条数应为 0，实际 %d", removed)
	}

	// 把在途的 ACK 掉，位点应当推进到第 400 条之后
	pend, err := q.rdb.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		t.Fatalf("XPENDING: %v", err)
	}
	if pend.Count != inflight {
		t.Fatalf("PEL 里应有 %d 条，实际 %d", inflight, pend.Count)
	}
	for _, ext := range q.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.stream, Group: q.group, Start: "-", End: "+", Count: inflight,
	}).Val() {
		if err := q.Ack(ctx, ext.ID); err != nil {
			t.Fatalf("ACK %s: %v", ext.ID, err)
		}
	}

	removed, err = q.TrimConsumed(ctx)
	if err != nil {
		t.Fatalf("第二次 TrimConsumed: %v", err)
	}
	t.Logf("ACK 掉在途消息后回收 %d 条，XLEN=%d", removed, xlen(t, q, ctx))
	if n := xlen(t, q, ctx); n != total-inflight {
		t.Fatalf("ACK 之后应能回收前 %d 条，XLEN 应为 %d，实际 %d", inflight, total-inflight, n)
	}
}

// ---------- P0-5 提交侧背压（准入控制） ----------

// 达到准入线之后必须**明确拒绝**，而不是收下让 MAXLEN 裁掉。
//
// 这条用例同时钉住一个容易搞错的因果关系：水位由物理长度决定，
// XACK 只把消息移出 PEL、不删条目，所以「消费掉了」不等于「水位降了」。
// 准入放行必须等 TrimConsumed 真正回收掉已确认前缀。
func TestRedisStreamQueue_AdmitRejectsAtThreshold(t *testing.T) {
	const maxLen = 100
	// 默认 AdmitRatio=0.8 → 准入线 80
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: maxLen, DLQMaxLen: 10})
	defer cleanup()

	if got := q.AdmitLimit(); got != 80 {
		t.Fatalf("默认准入线应为 MaxLen 的 80%%（80），实际 %d", got)
	}

	// 填到准入线：这 80 次必须全部成功
	for i := 0; i < 80; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("第 %d 条不应被拒: %v", i+1, err)
		}
	}

	// 第 81 条：Enqueue 与 Admit 都必须返回 ErrQueueFull
	if err := q.Enqueue(ctx, Job{TaskID: 80}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("达到准入线后 Enqueue 应返回 ErrQueueFull，实际 %v", err)
	}
	if err := q.Admit(ctx); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("达到准入线后 Admit 应返回 ErrQueueFull，实际 %v", err)
	}
	// 被拒绝的请求不得留下任何痕迹
	if n := xlen(t, q, ctx); n != 80 {
		t.Fatalf("拒绝不得写入条目，XLEN 应仍为 80，实际 %d", n)
	}

	// 消费并 ACK 40 条。XLEN 不会因此下降——XACK 只移出 PEL，不删消息本体。
	consume(t, q, ctx, 40, true)
	if n := xlen(t, q, ctx); n != 80 {
		t.Fatalf("XACK 不改变物理长度，XLEN 应仍为 80，实际 %d", n)
	}
	if err := q.Admit(ctx); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("仅 ACK 不足以让水位回落，Admit 应仍拒绝，实际 %v", err)
	}

	// 回收已确认前缀之后水位才真正下降，准入随之放行
	removed, err := q.TrimConsumed(ctx)
	if err != nil {
		t.Fatalf("TrimConsumed: %v", err)
	}
	if removed != 40 {
		t.Fatalf("应回收 40 条已确认前缀，实际 %d", removed)
	}
	if n := xlen(t, q, ctx); n != 40 {
		t.Fatalf("回收后 XLEN 应为 40，实际 %d", n)
	}
	if err := q.Admit(ctx); err != nil {
		t.Fatalf("水位回落到准入线以下后应放行，实际 %v", err)
	}
	if err := q.Enqueue(ctx, Job{TaskID: 999}); err != nil {
		t.Fatalf("水位回落后 Enqueue 应成功，实际 %v", err)
	}
}

// 对照用例（变异测试的「对照组」）：把准入关掉之后，
// 上面那条用例里的 ErrQueueFull 必须彻底消失，容量只由 MAXLEN 兜底。
// 如果哪天 AdmitRatio<0 被误实现成「用默认值」，这条会立刻红。
func TestRedisStreamQueue_AdmitDisabledByNegativeRatio(t *testing.T) {
	const maxLen = 50
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: maxLen, DLQMaxLen: 10, AdmitRatio: -1})
	defer cleanup()

	if got := q.AdmitLimit(); got != 0 {
		t.Fatalf("负比例应关闭准入，AdmitLimit 应为 0，实际 %d", got)
	}
	if err := q.Admit(ctx); err != nil {
		t.Fatalf("准入关闭时 Admit 必须放行，实际 %v", err)
	}

	// 写入 4 倍容量：一次都不该被拒，长度由 MAXLEN 兜底
	for i := 0; i < maxLen*4; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("准入关闭时第 %d 条不应被拒: %v", i+1, err)
		}
	}
	if n := xlen(t, q, ctx); n > maxLen {
		t.Fatalf("MAXLEN 兜底后 XLEN 不应超过 %d，实际 %d", maxLen, n)
	}
}

// 重投（Nack 后的延迟回流）与崩溃认领必须**绕过准入**。
//
// 理由：这些是已经收下的工作。队列满时把它们再拒一次，等于平台先答应了调用方
// 「任务已受理」，事后又悄悄丢掉——正是背压要消灭的那种失败模式。
func TestRedisStreamQueue_RequeueBypassesAdmission(t *testing.T) {
	const maxLen = 10 // 准入线 8
	q, ctx, cleanup := newTestQueue(t, Options{MaxLen: maxLen, DLQMaxLen: 10})
	defer cleanup()
	q.baseRetry = 10 * time.Millisecond // 让延迟重投立刻到期，不必等真实退避

	for i := 0; i < 8; i++ {
		if err := q.Enqueue(ctx, Job{TaskID: int64(i)}); err != nil {
			t.Fatalf("入队 %d: %v", i, err)
		}
	}
	if err := q.Admit(ctx); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("队列应已到准入线，实际 %v", err)
	}

	// 取一条出来标记为需要重投（retryLeft>0 → 进延迟队列而非 DLQ）
	job, msgID, err := q.Dequeue(ctx, "w1", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if job.TaskID != 0 {
		t.Fatalf("应先取到 task 0，实际 %d", job.TaskID)
	}
	if err := q.Nack(ctx, msgID, "boom", 3); err != nil {
		t.Fatalf("Nack: %v", err)
	}

	// 到期后由 Dequeue 内部收割回主 stream。此时准入线仍是 8，
	// 但这条回流必须成功——放回后长度会变成 9（>准入线），这就是绕过的证据。
	time.Sleep(30 * time.Millisecond)
	got := consume(t, q, ctx, 8, true)

	want := []int64{1, 2, 3, 4, 5, 6, 7, 0} // 原未读 7 条，最后是重投回来的 task 0
	if len(got) != len(want) {
		t.Fatalf("应消费到 %d 条，实际 %d：%v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条应是 task %d，实际 %d（完整序列 %v）", i, want[i], got[i], got)
		}
	}
	if n := xlen(t, q, ctx); n != 9 {
		t.Fatalf("重投应绕过准入写入主 stream（准入线 8），XLEN 应为 9，实际 %d", n)
	}
}

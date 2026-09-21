// Package queue 抽象任务队列。
//
// Redis Stream 实现承担跨进程持久队列：XADD 入队，Consumer Group 消费，
// XACK 确认，PEL（Pending Entries List）配合 XAUTOCLAIM 实现
// "Worker 崩溃后任务恢复"，处理失败且重试用尽的消息进入 DLQ。
//
// InMemoryQueue 用于测试与本地无 Redis 演示。
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrNoMessage = errors.New("no message in queue")

// ErrQueueFull 表示队列已接近容量上界，本次入队被拒绝（背压）。
//
// 为什么是「明确拒绝」而不是「先收下、超了让 MAXLEN 裁掉」：
// MAXLEN 裁的是最老条目，而那正是最可能还没被消费的那批——收下就等于承诺了一件
// 做不到的事：调用方拿到 201、任务行落库，却永远等不到执行结果。
// 宁可让调用方拿到一个明确的失败，也不要让它拿到一个假的成功。
var ErrQueueFull = errors.New("task queue is at capacity")

// Job 是队列消息负载。
type Job struct {
	TaskID int64 `json:"task_id"`
	// Attempt 记录该消息被重新投递的次数，用于指数退避。
	Attempt int `json:"attempt,omitempty"`
	// TraceParent 是 W3C Trace Context 的 traceparent 值，用于跨进程续接调用链。
	//
	// 为什么必须放进消息体：调用链的父子关系靠 span context 传递，而
	// "提交任务的进程"与"执行任务的进程"之间**唯一的**数据通路就是这条消息。
	// 不带上它，worker 只能自己起一条新链，于是同一次请求会变成两条互不相连的 trace
	// （API 侧一条、worker 侧一条），恰好把最该看清楚的"跨进程那一段"切没了。
	//
	// 空值合法且是旧消息的默认值：tracing 未启用、或消息由旧版本写入时，
	// 消费者按"新链"处理。因此这个字段是**向后兼容**的，不需要迁移历史消息。
	TraceParent string `json:"traceparent,omitempty"`
}

func (j Job) Bytes() []byte {
	b, _ := json.Marshal(j)
	return b
}

// Queue 统一接口：生产 / 消费 / 确认 / 拒绝。
type Queue interface {
	Name() string
	Enqueue(ctx context.Context, j Job) error
	// Dequeue 阻塞最多 block 时长，无消息返回 ErrNoMessage。
	Dequeue(ctx context.Context, workerID string, block time.Duration) (Job, string, error)
	// Ack 确认消息已处理完成。
	Ack(ctx context.Context, msgID string) error
	// Nack 处理失败（可带错误信息），retryLeft>0 时延迟重投，否则入 DLQ。
	Nack(ctx context.Context, msgID string, errMsg string, retryLeft int) error
	Len(ctx context.Context) (int64, error)
	// Recover 认领其他 worker 遗留的 pending 消息（崩溃恢复）。
	Recover(ctx context.Context, workerID string, count int64) (int, error)
}

// StreamStats 是队列实现「可选」暴露的容量水位能力。
//
// 调度器用类型断言发现它：实现得了就周期性回收 + 上报水位指标，实现不了就静默跳过。
// 刻意不放进 Queue 接口——内存队列出队即删除，压根不存在「已确认但还占着空间的前缀」，
// 硬塞进主接口只会逼所有实现写一堆无意义的空方法。
type StreamStats interface {
	// StreamLen 是主 stream 的物理长度（含已 ACK 但尚未回收的历史消息）。
	StreamLen(ctx context.Context) (int64, error)
	// DLQLen 是死信 stream 的物理长度。
	DLQLen(ctx context.Context) (int64, error)
	// MaxLen 是主 stream 配置的容量上界，用于计算水位占比。
	MaxLen() int64
	// AdmitLimit 是准入阈值（绝对条数），<=0 表示准入控制已关闭。
	AdmitLimit() int64
	// TrimConsumed 回收「已投递且已确认」的前缀，返回实际删除的条目数。
	TrimConsumed(ctx context.Context) (int64, error)
}

// Admitter 是队列实现「可选」暴露的准入预检能力。
//
// 存在的理由：背压不只是「返回 503」，拒绝路径本身也必须是廉价的。
// 如果准入检查只发生在 Enqueue，那么每个被拒绝的请求在此之前已经付过
// 一次 INSERT（建任务行）和一次 UPDATE（标 failed）——过载时拒绝得越多，
// 往数据库压的写就越多，而数据库恰恰是最先扛不住的那一环。
// 让调用方能在落库之前先问一句「现在还能收吗」，拒绝路径就退化成一次 XLEN。
//
// 这与 Enqueue 内部的那次检查不重复：预检和实际入队之间存在 TOCTOU 窗口
// （预检通过后队列可能被别的实例填满），Enqueue 的那次才是正确性保证，
// 预检只是把绝大多数拒绝挡在落库之前。
type Admitter interface {
	// Admit 返回 nil 表示当前还能接受新任务，ErrQueueFull 表示应当拒绝。
	Admit(ctx context.Context) error
}

// ---------- Redis Stream 实现 ----------

// 容量水位默认值。
//
// 取值依据：压测单档位是 1300 个任务，消费者最多 20 个，同时刻在途的消息数远小于 10000；
// 取 10000 意味着「即使积压到 1 万条也一条都不会被裁掉」，同时又把无限增长封住了。
const (
	defaultMaxLen    = 10000
	defaultDLQMaxLen = 1000
	// defaultAdmitRatio 是准入阈值占 MaxLen 的比例。
	//
	// 为什么准入线要低于告警线（0.9）：准入是第一道防线，告警的语义是"第一道防线失效了"。
	// 两者取同一个值的话，队列会稳定停在阈值上反复穿越，告警变成噪声。
	defaultAdmitRatio = 0.8
)

// Options 控制 stream 的容量水位（见 RedisStreamQueue 的 XAdd 说明）。
//
// 零值可用：MaxLen / DLQMaxLen <= 0 时回落到默认值，**绝不会退化成"无上限"**。
// 这一点是刻意的——容量上限属于安全默认值，不该因为调用方少填一个字段就消失。
type Options struct {
	// MaxLen 是主 stream 与延迟重投回流的近似上限。
	MaxLen int64
	// DLQMaxLen 是死信 stream 的近似上限。
	// DLQ 只用于事后排查、不会被消费，不给上限就会永久增长。
	DLQMaxLen int64
	// AdmitRatio 是准入阈值占 MaxLen 的比例，取值 (0,1)。
	//   0  → 用默认值 0.8
	//   <0 → **关闭准入控制**（只用于压测对照，生产不要关）
	AdmitRatio float64
}

type RedisStreamQueue struct {
	rdb    *redis.Client
	stream string
	group  string
	dlq    string
	retry  string // ZSET：延迟重投（score = 到期的 unix 毫秒）
	// baseRetry 是首次重投的延迟，之后按 2 的幂次退避。
	baseRetry time.Duration
	// maxLen / dlqMaxLen 是所有 XAdd 都必须带的容量水位。见 xadd 的说明。
	maxLen    int64
	dlqMaxLen int64
	// admitLimit 是准入阈值（绝对条数）。达到它就拒绝新提交，而不是等 MAXLEN 去裁。
	// <= 0 表示关闭准入控制。
	admitLimit int64
}

const defaultGroup = "nebula_workers"

func NewRedisStreamQueue(rdb *redis.Client, stream string, opt Options) (*RedisStreamQueue, error) {
	if opt.MaxLen <= 0 {
		opt.MaxLen = defaultMaxLen
	}
	if opt.DLQMaxLen <= 0 {
		opt.DLQMaxLen = defaultDLQMaxLen
	}
	admitLimit := int64(0) // <=0 表示关闭准入
	switch {
	case opt.AdmitRatio < 0:
		// 显式关闭：只用于压测对照
	case opt.AdmitRatio == 0:
		admitLimit = int64(float64(opt.MaxLen) * defaultAdmitRatio)
	default:
		ratio := opt.AdmitRatio
		if ratio > 1 {
			ratio = 1
		}
		admitLimit = int64(float64(opt.MaxLen) * ratio)
	}
	q := &RedisStreamQueue{
		rdb:        rdb,
		stream:     stream,
		group:      defaultGroup,
		dlq:        stream + "_dlq",
		retry:      stream + "_retry",
		baseRetry:  2 * time.Second,
		maxLen:     opt.MaxLen,
		dlqMaxLen:  opt.DLQMaxLen,
		admitLimit: admitLimit,
	}
	if err := q.rdb.XGroupCreateMkStream(context.Background(), q.stream, q.group, "0").Err(); err != nil && !isBusyGroup(err) {
		return nil, fmt.Errorf("create consumer group: %w", err)
	}
	return q, nil
}

// xadd 是所有 XAdd 的唯一出口，统一带上容量水位。
//
// 为什么必须有水位：**XACK 只是把消息从 PEL 移除，不删除消息本体**。
// 原实现全项目没有任何 MaxLen/XDel/XTrim，消息只进不出，长期运行必然把 Redis 撑爆
// （队列和限流都依赖同一个 Redis）。重投与崩溃认领还会不断往同一个 stream 追加新消息，
// 增长是单调的。
//
// 为什么用 Approx（`MAXLEN ~`）：精确裁剪每次 XADD 都要扫描并摘除节点，
// 代价随 stream 长度上升；近似裁剪只裁掉完整的宏节点，代价恒定。
// 代价是实际长度会略高于 MaxLen（真 Redis 下取决于节点大小，可能高出几十到几百条），
// 所以水位是"上界附近的软约束"，需要按 80% 设告警而不是按 100% 设断言。
//
// ⚠ 这里有一个必须知道的取舍：**MAXLEN 裁掉的是最老的条目，而那正是最可能还没被 ACK 的条目。**
// 一条消息被投递给某个 consumer（进入 PEL）之后：
//   - consumer 还活着 → 它手里已有 payload，照常跑完，裁剪不影响；
//   - consumer 崩了 → 消息本来就靠 XAUTOCLAIM 认领重投，但条目已被裁掉，
//     XAUTOCLAIM 会把它当成"已删除"静默移出 PEL —— **任务永久丢失，且不留痕迹**。
//
// 所以 MaxLen 是"防止无限增长的护栏"，**不是"把队列调小"的旋钮**：
// 它必须大于你愿意丢掉的积压量。正确的用法是水位留足（默认 1 万条），
// 配合"XLEN 超过 80% 就告警"，而不是把它压到跟并发数一个量级。
//
// 真正承担日常回收职责的是 TrimConsumed：它只删「已投递且已确认」的前缀，
// 不丢任务。MAXLEN 退居为「消费端整体停摆时兜住内存」的最后一道防线。
func (q *RedisStreamQueue) xadd(ctx context.Context, stream string, maxLen int64, values map[string]any) error {
	return q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		MaxLen: maxLen,
		Approx: true,
		Values: values,
	}).Err()
}

// StreamLen 返回主 stream 的物理长度（含已 ACK 但未被裁剪的历史消息）。
//
// 注意与 Len 的区别：Len 返回的是"积压量"（lag + pending + 延迟重投），
// 这是运维关心的两个不同指标——前者反映容量水位，后者反映消费压力。
func (q *RedisStreamQueue) StreamLen(ctx context.Context) (int64, error) {
	return q.rdb.XLen(ctx, q.stream).Result()
}

// DLQLen 返回死信 stream 的物理长度。
func (q *RedisStreamQueue) DLQLen(ctx context.Context) (int64, error) {
	return q.rdb.XLen(ctx, q.dlq).Result()
}

// MaxLen 返回主 stream 配置的容量上界（用于计算水位占比）。
func (q *RedisStreamQueue) MaxLen() int64 { return q.maxLen }

// AdmitLimit 返回准入阈值（绝对条数），<=0 表示准入控制已关闭。
func (q *RedisStreamQueue) AdmitLimit() int64 { return q.admitLimit }

// TrimConsumed 把「消费组已经确认过的前缀」从 stream 里物理删除，返回删除条数。
//
// 为什么必须有它：XACK 只把消息移出 PEL，**消息本体仍然留在 stream 里**。
// 所以只靠 MAXLEN 限长，等于拿「裁掉最老的、可能还没被任何消费者读走的条目」去换内存。
// 这一点在 P0-2 的对照压测里被量化过（2 万条突发、消费者约 380 tasks/s）：
//
//	QUEUE_MAXLEN=100000000（≈无上限）  XLEN 稳态 20200   落地率 100%
//	QUEUE_MAXLEN=10000（默认）         XLEN 稳态 10000   落地率 52.28%
//	QUEUE_MAXLEN=1000                  XLEN 稳态  1000   落地率  7.63%
//
// 落地数 ≈ MaxLen + 突发期间已消费数，差额全部是「还没来得及被读走就被裁掉」的消息——
// 它们对应的任务永久停在 pending，而且 Redis 侧不留任何痕迹（XAUTOCLAIM 会把
// 已被删除的 PEL 条目当成"已删除"静默移出，连告警都没有）。
//
// 安全裁剪位点怎么算：
//   - XINFO GROUPS 的 last-delivered-id：ID ≤ 它的条目都已经投递给某个消费者；
//   - XPENDING 摘要的 Lower：PEL 里最小的 ID，小于它的条目不在 PEL 里，即已被 XACK。
//
// 两者取小，就得到「已投递且已确认」的分界点，删掉严格小于它的条目不会丢任何任务。
//
// 与 MAXLEN 的关系是「正常路径回收 + 极端情况护栏」，不是二选一：
// 消费端整体停摆时（消费者全挂、消费组被删）没有任何条目会被 ACK，
// 本方法会一直原地不动，此时只有 MAXLEN 能兜住内存。
func (q *RedisStreamQueue) TrimConsumed(ctx context.Context) (int64, error) {
	groups, err := q.rdb.XInfoGroups(ctx, q.stream).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil // stream 还不存在，没什么可回收
		}
		return 0, err
	}
	lastDelivered := ""
	for _, g := range groups {
		if g.Name == q.group {
			lastDelivered = g.LastDeliveredID
			break
		}
	}
	if lastDelivered == "" || lastDelivered == "0-0" {
		return 0, nil // 一条都没投递过，没有任何已确认前缀
	}

	safeID := ""
	if pend, perr := q.rdb.XPending(ctx, q.stream, q.group).Result(); perr == nil &&
		pend != nil && pend.Count > 0 && pend.Lower != "" {
		// PEL 非空：还有已投递但未 ACK 的在途消息，裁剪位点不能越过最老的那条。
		// XTRIM MINID 删的是"严格小于"给定 ID 的条目，所以传 pend.Lower 正好把它留下。
		safeID = pend.Lower
	} else {
		// PEL 为空：所有已投递的条目都已 ACK，连 last-delivered-id 本身也可以删。
		// 这里必须传它的下一个 ID——传它自己的话会永远留下一条删不掉的尾巴。
		safeID = nextStreamID(lastDelivered)
	}
	if safeID == "" || safeID == "0-0" {
		return 0, nil
	}
	return q.rdb.XTrimMinID(ctx, q.stream, safeID).Result()
}

// nextStreamID 返回严格大于给定 stream ID 的最小 ID（形如 "<ms>-<seq>"）。
//
// stream ID 是「毫秒时间戳-序号」，序号在同一个毫秒内单调递增，
// 所以 seq+1 就是紧随其后的合法 ID：它严格大于原 ID，又一定不大于下一条真实条目的 ID。
//
// 解析失败返回空串，调用方据此跳过裁剪——宁可这一轮少删几条，
// 也不能因为 ID 格式异常而把不该删的消息删掉。
func nextStreamID(id string) string {
	dash := strings.LastIndex(id, "-")
	if dash <= 0 || dash == len(id)-1 {
		return ""
	}
	ms, err1 := strconv.ParseInt(id[:dash], 10, 64)
	seq, err2 := strconv.ParseInt(id[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || ms < 0 || seq < 0 {
		return ""
	}
	return fmt.Sprintf("%d-%d", ms, seq+1)
}

func isBusyGroup(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "BUSYGROUP") || strings.Contains(msg, "Consumer Group name already exists")
}

func (q *RedisStreamQueue) Name() string { return "redis-stream:" + q.stream }

// Admit 做一次准入预检：队列接近容量上界时返回 ErrQueueFull（背压）。
//
// 检查**必须在 XADD 之前**，不能反过来"先写再看长度"：
// MAXLEN 是在 XADD 时刻裁剪的，等写进去才发现超了，最老的那条（很可能是还没被读走的）
// 已经被裁掉了——损失已经发生，再 XDEL 自己也只是亡羊补牢。
//
// 代价是热路径多一次 Redis 往返（XLEN）。这里没有用 Lua 脚本合并成一次往返，
// 因为 EVAL 在测试用的 miniredis 上支持不完整，会让这条路径失去集成测试覆盖——
// 相比之下一次本地 Redis 往返（约 0.05ms）相对于同一个请求里的 PG 插入可以忽略。
func (q *RedisStreamQueue) Admit(ctx context.Context) error {
	if q.admitLimit <= 0 {
		return nil
	}
	n, err := q.rdb.XLen(ctx, q.stream).Result()
	if err != nil {
		// 读长度失败不拦：宁可放过去（最多让 MAXLEN 兜底），
		// 也不要因为一次 Redis 抖动就把正常提交全部拒掉。
		return nil
	}
	if n >= q.admitLimit {
		return ErrQueueFull
	}
	return nil
}

// Enqueue 提交一个新任务；队列接近容量上界时返回 ErrQueueFull（背压）。
//
// 注意：重投（Nack）与崩溃认领（Recover）走的是内部 xadd，**不经过准入**——
// 那些是已经收下的工作，再拒一次就等于把已承诺的任务丢掉。
func (q *RedisStreamQueue) Enqueue(ctx context.Context, j Job) error {
	if err := q.Admit(ctx); err != nil {
		return err
	}
	return q.xadd(ctx, q.stream, q.maxLen, map[string]any{"payload": string(j.Bytes())})
}

// Dequeue 取一条消息。每次调用前先把到期的延迟重投消息放回主 stream。
func (q *RedisStreamQueue) Dequeue(ctx context.Context, workerID string, block time.Duration) (Job, string, error) {
	// 先做一次延迟队列收割；失败不影响正常消费（下一轮会重试）
	_ = q.promoteDueRetries(ctx, 32)
	res, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.group,
		Consumer: workerID,
		Streams:  []string{q.stream, ">"},
		Count:    1,
		Block:    block,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return Job{}, "", ErrNoMessage
		}
		return Job{}, "", err
	}
	for _, stream := range res {
		for _, msg := range stream.Messages {
			var j Job
			payload, _ := msg.Values["payload"].(string)
			if err := json.Unmarshal([]byte(payload), &j); err != nil {
				// 坏消息直接 ACK 掉，避免阻塞队列
				_ = q.Ack(ctx, msg.ID)
				continue
			}
			return j, msg.ID, nil
		}
	}
	return Job{}, "", ErrNoMessage
}

func (q *RedisStreamQueue) Ack(ctx context.Context, msgID string) error {
	if msgID == "" {
		return nil
	}
	return q.rdb.XAck(ctx, q.stream, q.group, msgID).Err()
}

// Nack：延迟重投或直接送 DLQ。
//
// 原实现用 XADD 的 MinID 参数塞一个"未来时间戳"来伪造延迟队列，这是错的：
// MinID 只是约束新条目 ID 的下限，XREADGROUP ">" 会立刻把它投递出去，
// 延迟根本没生效；更糟的是它会把 stream 的 top ID 顶到未来，
// 后续 XADD(*) 只能被迫跟在这个未来 ID 后面，破坏 stream 的时间序语义。
//
// 正确做法：用一个 ZSET 当作延迟队列，score 是到期时间戳；
// 到期的条目在 Dequeue 前被重新 XADD 回主 stream 真正投递。
func (q *RedisStreamQueue) Nack(ctx context.Context, msgID string, errMsg string, retryLeft int) error {
	var payload string
	if msgID != "" {
		msgs, err := q.rdb.XRange(ctx, q.stream, msgID, msgID).Result()
		if err == nil && len(msgs) > 0 {
			payload, _ = msgs[0].Values["payload"].(string)
		}
	}
	if payload == "" {
		payload = "{}"
	}

	if retryLeft > 0 {
		var j Job
		_ = json.Unmarshal([]byte(payload), &j)
		j.Attempt++
		if b, err := json.Marshal(j); err == nil {
			payload = string(b)
		}
		delay := q.backoffFor(j.Attempt)
		score := float64(time.Now().Add(delay).UnixMilli())
		// 用 payload+到期时间做 member，保证同一条消息的重投记录可去重
		member := fmt.Sprintf("%d:%s", time.Now().Add(delay).UnixMilli(), payload)
		if err := q.rdb.ZAdd(ctx, q.retry, redis.Z{Score: score, Member: member}).Err(); err != nil {
			return err
		}
		return q.Ack(ctx, msgID)
	}
	// 重试用尽：进 DLQ。同样要限长——DLQ 不会被消费，不给上限就是永久增长。
	meta := map[string]any{"payload": payload, "error": errMsg, "failed_at": time.Now().UnixMilli()}
	if err := q.xadd(ctx, q.dlq, q.dlqMaxLen, meta); err != nil {
		return err
	}
	return q.Ack(ctx, msgID)
}

// backoffFor 指数退避：1x, 2x, 4x ... baseRetry，上限 5 分钟。
func (q *RedisStreamQueue) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := q.baseRetry
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= 5*time.Minute {
			d = 5 * time.Minute
			break
		}
	}
	return d
}

// promoteDueRetries 把到期的延迟重投消息搬回主 stream。
// ZREM 先于 XADD：谁先移除谁负责投递，多实例并发时不会重复投递。
func (q *RedisStreamQueue) promoteDueRetries(ctx context.Context, limit int64) error {
	if limit <= 0 {
		limit = 32
	}
	now := float64(time.Now().UnixMilli())
	members, err := q.rdb.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
		Key:     q.retry,
		Start:   "-inf",
		Stop:    now,
		ByScore: true,
		Count:   limit,
	}).Result()
	if err != nil || len(members) == 0 {
		return err
	}
	for _, m := range members {
		member, _ := m.Member.(string)
		if member == "" {
			continue
		}
		if removed, err := q.rdb.ZRem(ctx, q.retry, member).Result(); err != nil || removed == 0 {
			continue // 已被其他实例接管
		}
		payload := member
		if i := strings.Index(member, ":"); i >= 0 {
			payload = member[i+1:]
		}
		if err := q.xadd(ctx, q.stream, q.maxLen, map[string]any{"payload": payload}); err != nil {
			// 投递失败就放回去，避免丢消息
			_ = q.rdb.ZAdd(ctx, q.retry, redis.Z{Score: m.Score, Member: member}).Err()
			return err
		}
	}
	return nil
}

// PendingRetries 返回当前等待重投的消息数（监控用）。
func (q *RedisStreamQueue) PendingRetries(ctx context.Context) (int64, error) {
	return q.rdb.ZCard(ctx, q.retry).Result()
}

func (q *RedisStreamQueue) Len(ctx context.Context) (int64, error) {
	// 待消费 = 组内未投递（lag）+ 处理中/遗留（pending）+ 延迟重投
	total := int64(0)
	if info, err := q.rdb.XInfoGroups(ctx, q.stream).Result(); err == nil {
		for _, g := range info {
			total += g.Lag + g.Pending
		}
	} else if l, err2 := q.rdb.XLen(ctx, q.stream).Result(); err2 == nil {
		total += l
	}
	if n, err := q.rdb.ZCard(ctx, q.retry).Result(); err == nil {
		total += n
	}
	return total, nil
}

// Recover：用 XAUTOCLAIM 认领其他 worker 遗留超过 minIdle 的 pending 消息，
// 并把认领到的消息重新 XADD 回队列（新 ID）+ XACK 原消息，
// 使主循环的 XREADGROUP ">" 能再次投递（崩溃恢复真正生效）。
func (q *RedisStreamQueue) Recover(ctx context.Context, workerID string, count int64) (int, error) {
	_ = q.promoteDueRetries(ctx, count)
	// go-redis v9 的 Result() 签名：(messages []XMessage, start string, err error)
	msgs, _, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   q.stream,
		Group:    q.group,
		Consumer: workerID,
		MinIdle:  30 * time.Second,
		Start:    "0",
		Count:    count,
	}).Result()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range msgs {
		payload, _ := m.Values["payload"].(string)
		if payload == "" {
			continue
		}
		// 重新入队（新 ID），让 ">" 投递
		if err := q.xadd(ctx, q.stream, q.maxLen, map[string]any{"payload": payload}); err != nil {
			return n, err
		}
		if err := q.rdb.XAck(ctx, q.stream, q.group, m.ID).Err(); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (q *RedisStreamQueue) slogWarn(msg string) { _ = msg }

// ---------- 内存实现（测试 / 演示） ----------

// InMemoryQueue 复刻 Redis Stream 的核心语义（唯一 msgID + 在途消息表），
// 因此 Nack 重投、Ack 确认在内存模式下同样可用，
// 单元测试不需要 Redis 就能覆盖"失败重试"链路。
type InMemoryQueue struct {
	ch       chan Job
	mu       sync.Mutex
	seq      uint64
	inflight map[string]Job
}

func NewInMemoryQueue(size int) *InMemoryQueue {
	if size <= 0 {
		size = 1024
	}
	return &InMemoryQueue{ch: make(chan Job, size), inflight: make(map[string]Job)}
}

func (q *InMemoryQueue) Name() string { return "in-memory" }

func (q *InMemoryQueue) Enqueue(ctx context.Context, j Job) error {
	select {
	case q.ch <- j:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *InMemoryQueue) Dequeue(ctx context.Context, _ string, block time.Duration) (Job, string, error) {
	timer := time.NewTimer(block)
	defer timer.Stop()
	select {
	case j := <-q.ch:
		q.mu.Lock()
		q.seq++
		id := fmt.Sprintf("mem-%d", q.seq)
		if q.inflight == nil {
			q.inflight = make(map[string]Job)
		}
		q.inflight[id] = j
		q.mu.Unlock()
		return j, id, nil
	case <-ctx.Done():
		return Job{}, "", ctx.Err()
	case <-timer.C:
		return Job{}, "", ErrNoMessage
	}
}

func (q *InMemoryQueue) Ack(_ context.Context, msgID string) error {
	q.mu.Lock()
	delete(q.inflight, msgID)
	q.mu.Unlock()
	return nil
}

// Nack 支持重投：retryLeft>0 时按指数退避重新入队，否则丢弃（等价于 DLQ）。
func (q *InMemoryQueue) Nack(ctx context.Context, msgID string, errMsg string, retryLeft int) error {
	q.mu.Lock()
	j, ok := q.inflight[msgID]
	delete(q.inflight, msgID)
	q.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown message %q", msgID)
	}
	if retryLeft <= 0 {
		return fmt.Errorf("message %s dropped to dlq: %s", msgID, errMsg)
	}
	j.Attempt++
	delay := time.Duration(1<<(j.Attempt-1)) * 100 * time.Millisecond
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	time.AfterFunc(delay, func() {
		select {
		case q.ch <- j:
		case <-ctx.Done():
		}
	})
	return nil
}

func (q *InMemoryQueue) Len(context.Context) (int64, error) {
	q.mu.Lock()
	n := len(q.ch) + len(q.inflight)
	q.mu.Unlock()
	return int64(n), nil
}

func (q *InMemoryQueue) Recover(context.Context, string, int64) (int, error) { return 0, nil }

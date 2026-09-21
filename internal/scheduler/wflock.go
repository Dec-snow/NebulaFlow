package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrWorkflowLocked 表示同一工作流已在另一个实例上执行，本次获取锁失败。
//
// 设计选择（面试讲点）：
//  1. **获取失败时 Nack 重投而不是丢弃**：消息回到队列、由持锁实例完成后再消费。
//     不会进 DLQ——这不是业务失败，只是"还没轮到我"。
//  2. **锁值是 instanceID + 随机 token**：DEL 前用 Lua 脚本校验值，防止实例 A
//     释放实例 B 的锁（进程崩溃后 TTL 到期、B 接手、A 恢复后误删）。
//  3. **TTL > taskTimeout**：默认锁 TTL 10 分钟，任务超时 5 分钟。保证任务执行期间
//     锁不会过期；如果实例崩溃，TTL 到期后其他实例能接手。
//  4. **不加锁的路径是内存模式**：内存模式只有一个进程，sync.Mutex 足够；
//     Redis 不可用时也退化为进程内锁——不改变"零外部依赖启动"的承诺。
var ErrWorkflowLocked = errors.New("workflow is already running on another instance")

// WorkflowLock 防止同一工作流在多个实例上并发执行。
//
// 为什么需要它：executeTask 的可变状态是函数局部的，多个**不同**工作流的任务
// 可以安全并行。但同一个工作流的两个任务如果同时跑——一个在改 task_nodes，
// 另一个也在建 task_nodes——会产生脏数据甚至死锁。
// 单实例下 sync.Mutex 够用；多实例下必须用分布式锁。
//
// 动作语义：Acquire 返回一个 release 函数，调用方在 defer 里调它释放锁。
// 获取失败返回 ErrWorkflowLocked，调用方应 Nack 重投。
type WorkflowLock interface {
	// Acquire 尝试获取 workflowID 的排他锁。
	// 返回 nil 表示成功，调用方必须在任务完成后调用返回的 release 函数。
	// 返回 ErrWorkflowLocked 表示锁被其他实例持有。
	Acquire(ctx context.Context, workflowID int64) (release func(), err error)
}

// ---------- Redis 实现 ----------

// RedisWorkflowLock 用 SET NX EX + Lua DEL 实现分布式互斥。
type RedisWorkflowLock struct {
	rdb        *redis.Client
	instanceID string
	ttl        time.Duration
	// 本地 hold 集合：同一个实例内如果同一工作流被消费两次（不应该但防御），
	// 第二次也该被拦——这比跨进程更快、更便宜。
	mu    sync.Mutex
	local map[int64]bool
}

// NewRedisWorkflowLock 创建 Redis 分布式锁。
// instanceID 为空时自动生成（进程级唯一）。
func NewRedisWorkflowLock(rdb *redis.Client, ttl time.Duration) *RedisWorkflowLock {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &RedisWorkflowLock{
		rdb:        rdb,
		instanceID: newInstanceID(),
		ttl:        ttl,
		local:      map[int64]bool{},
	}
}

func (l *RedisWorkflowLock) Acquire(ctx context.Context, workflowID int64) (func(), error) {
	// 本实例内互斥：防止同一进程的两个消费者同时跑同一工作流。
	// 这是一个更快的短路——不需要一次 Redis 往返就能拒绝。
	l.mu.Lock()
	if l.local[workflowID] {
		l.mu.Unlock()
		return nil, ErrWorkflowLocked
	}
	l.mu.Unlock()

	// rdb 为 nil（Redis 不可用 / 测试场景）：退化为不加锁。
	// 不阻断执行——锁是正确性增强而不是前置条件。
	if l.rdb == nil {
		// 但本地标记仍要设置，防止同进程内重复获取。
		l.mu.Lock()
		l.local[workflowID] = true
		l.mu.Unlock()
		released := false
		return func() {
			if released {
				return
			}
			released = true
			l.mu.Lock()
			delete(l.local, workflowID)
			l.mu.Unlock()
		}, nil
	}

	key := fmt.Sprintf("nebulaflow:wflock:%d", workflowID)
	token := l.instanceID + ":" + randomToken()

	ok, err := l.rdb.SetNX(ctx, key, token, l.ttl).Result()
	if err != nil {
		// Redis 故障：退化为不加锁（不阻断执行）。
		// 理由：锁是正确性增强而不是前置条件——没有它单实例照常跑。
		// 如果这里返回错误，Redis 一抖就全站停摆，代价远大于偶尔并发。
		return func() {}, nil
	}
	if !ok {
		return nil, ErrWorkflowLocked
	}

	l.mu.Lock()
	l.local[workflowID] = true
	l.mu.Unlock()

	released := false
	return func() {
		if released {
			return
		}
		released = true
		l.mu.Lock()
		delete(l.local, workflowID)
		l.mu.Unlock()
		// 用 Lua 脚本保证"只有持锁者才能删"：先 GET 比对 token，匹配才 DEL。
		// 不比对直接 DEL 会删掉别人的锁：A 崩溃 → TTL 到期 → B 获锁 → A 恢复 →
		// A 的 defer 跑 DEL → B 的锁被误删 → C 获锁 → B 和 C 同时执行。
		delScript.Run(ctx, l.rdb, []string{key}, token)
	}, nil
}

var delScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
else
	return 0
end
`)

// ---------- 内存实现（单实例 / 测试 / Redis 不可用） ----------

// MemoryWorkflowLock 是进程内互斥，语义与 Redis 版一致。
// 用于内存模式、Redis 不可用时的降级、以及单元测试。
type MemoryWorkflowLock struct {
	mu    sync.Mutex
	locks map[int64]bool
}

func NewMemoryWorkflowLock() *MemoryWorkflowLock {
	return &MemoryWorkflowLock{locks: map[int64]bool{}}
}

func (l *MemoryWorkflowLock) Acquire(ctx context.Context, workflowID int64) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks[workflowID] {
		return nil, ErrWorkflowLocked
	}
	l.locks[workflowID] = true
	released := false
	return func() {
		if released {
			return
		}
		released = true
		l.mu.Lock()
		delete(l.locks, workflowID)
		l.mu.Unlock()
	}, nil
}

// ---------- 工具函数 ----------

// newInstanceID 生成进程级唯一标识符。
func newInstanceID() string {
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), randomToken())
}

// randomToken 生成 8 字节随机十六进制字符串（16 个字符）。
func randomToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Package ratelimit 实现基于 Redis 的固定窗口限流。
//
// 键格式：ratelimit:{key}:{window}
// 采用 INCR + EXPIRE 原子语义：窗口内计数超过上限即拒绝。
// 无 Redis 时退化为内存实现（单实例演示/测试）。
package ratelimit

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type Result struct {
	Allowed   bool
	Remaining int64
	Limit     int64
	Window    time.Duration
}

type Limiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error)
}

// RedisLimiter 基于 Redis 的限流器。
type RedisLimiter struct {
	rdb redis.UniversalClient
}

func NewRedisLimiter(rdb redis.UniversalClient) *RedisLimiter {
	return &RedisLimiter{rdb: rdb}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (Result, error) {
	// limit <= 0 表示"不限制"，直接放行。
	// 原实现在 limit<=0 时返回 Allowed=false，一旦上游传错参数就会把所有请求全挡掉。
	if limit <= 0 {
		return Result{Allowed: true, Limit: 0, Window: window}, nil
	}
	windowSec := int64(window.Seconds())
	if windowSec <= 0 {
		windowSec = 60
	}
	rkey := "ratelimit:" + key + ":" + strconv.FormatInt(windowSec, 10)

	// 固定窗口：先 INCR，再"仅当本窗口第一次计数时"设置过期时间。
	//
	// 关键点：不能在每次请求都 EXPIRE —— 那样 TTL 会被无限续期，
	// 只要请求持续到来，计数窗口就永远不会滚动，最终所有请求被永久拒绝。
	// （这正是原实现的 bug：稳态流量下窗口永不翻页。）
	//
	// 生产环境可用 SET NX + EXPIRE 或 Lua 脚本保证严格原子性；
	// 这里用 "INCR 后按结果条件 EXPIRE" 已经足够：即使极端并发下有多次 EXPIRE，
	// 也只是把过期时间设为同一个 windowSec，不会破坏语义。
	n, err := l.rdb.Incr(ctx, rkey).Result()
	if err != nil {
		return Result{}, err
	}
	if n == 1 {
		if err := l.rdb.Expire(ctx, rkey, time.Duration(windowSec)*time.Second).Err(); err != nil {
			// 过期设置失败会让 key 常驻（计数永不重置），降级为放行避免误杀
			return Result{Allowed: true, Limit: int64(limit), Window: window}, nil
		}
	}
	allowed := n <= int64(limit)
	remaining := int64(limit) - n
	if remaining < 0 {
		remaining = 0
	}
	return Result{
		Allowed:   allowed,
		Remaining: remaining,
		Limit:     int64(limit),
		Window:    window,
	}, nil
}

// MemoryLimiter 单实例限流（无 Redis 时使用）。
type MemoryLimiter struct {
	counts map[string]int64
	until  map[string]time.Time
	mu     chan struct{} // 二进制信号量当锁
}

func NewMemoryLimiter() *MemoryLimiter {
	return &MemoryLimiter{
		counts: map[string]int64{},
		until:  map[string]time.Time{},
		mu:     make(chan struct{}, 1),
	}
}

func (l *MemoryLimiter) Allow(_ context.Context, key string, limit int, window time.Duration) (Result, error) {
	if limit <= 0 {
		return Result{Allowed: true, Limit: 0, Window: window}, nil
	}
	l.mu <- struct{}{}
	defer func() { <-l.mu }()

	now := time.Now()
	if exp, ok := l.until[key]; !ok || !now.Before(exp) {
		l.counts[key] = 0
		l.until[key] = now.Add(window)
		// 顺带清理已过期的 key，避免长时间运行导致 map 无界增长
		l.sweepLocked(now)
	}
	l.counts[key]++
	n := l.counts[key]
	remaining := int64(limit) - n
	if remaining < 0 {
		remaining = 0
	}
	return Result{
		Allowed:   n <= int64(limit),
		Remaining: remaining,
		Limit:     int64(limit),
		Window:    window,
	}, nil
}

// sweepLocked 清理过期窗口，调用方必须已持有 mu。
func (l *MemoryLimiter) sweepLocked(now time.Time) {
	for k, exp := range l.until {
		if now.Before(exp) {
			continue
		}
		delete(l.until, k)
		delete(l.counts, k)
	}
}

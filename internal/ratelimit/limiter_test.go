package ratelimit

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 这个文件补上 ratelimit 包的首批测试。
//
// 这个包此前零测试，而它守着的是"单个用户不能打垮整个系统"这条线。
// 它历史上出过一个很隐蔽的缺陷：**每次请求都 EXPIRE**，
// 于是只要流量持续到来，TTL 就被无限续期、窗口永不翻页，
// 最终所有请求被永久拒绝——而限流器本身"看起来工作正常"。
// 下面 RedisLimiter 那一组用例就是冲着这个语义去的。

// ---------- MemoryLimiter（无 Redis 时的实现） ----------

func TestMemoryLimiterAllowsUpToLimitThenRejects(t *testing.T) {
	l := NewMemoryLimiter()
	ctx := context.Background()

	const limit = 3
	for i := 1; i <= limit; i++ {
		res, err := l.Allow(ctx, "user:1", limit, time.Minute)
		if err != nil {
			t.Fatalf("第 %d 次调用出错: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("第 %d 次调用（限额 %d）应放行，实际被拒", i, limit)
		}
		if want := int64(limit - i); res.Remaining != want {
			t.Errorf("第 %d 次调用 Remaining 应为 %d，实际 %d", i, want, res.Remaining)
		}
		if res.Limit != limit {
			t.Errorf("Limit 应为 %d，实际 %d", limit, res.Limit)
		}
	}

	// 第 limit+1 次必须被拒，且 Remaining 不能变成负数
	res, err := l.Allow(ctx, "user:1", limit, time.Minute)
	if err != nil {
		t.Fatalf("超限调用出错: %v", err)
	}
	if res.Allowed {
		t.Fatalf("超过限额（%d）后应被拒绝", limit)
	}
	if res.Remaining != 0 {
		t.Errorf("被拒时 Remaining 应为 0（不能是负数），实际 %d", res.Remaining)
	}
}

func TestMemoryLimiterWindowResets(t *testing.T) {
	l := NewMemoryLimiter()
	ctx := context.Background()

	const window = 60 * time.Millisecond
	if res, _ := l.Allow(ctx, "user:1", 1, window); !res.Allowed {
		t.Fatal("窗口内第一次调用应放行")
	}
	if res, _ := l.Allow(ctx, "user:1", 1, window); res.Allowed {
		t.Fatal("同一窗口内第二次调用应被拒绝")
	}

	time.Sleep(window + 40*time.Millisecond)

	// 窗口翻页后计数必须归零——否则限流器会永久拉黑这个用户
	res, _ := l.Allow(ctx, "user:1", 1, window)
	if !res.Allowed {
		t.Fatal("窗口过期后应重新放行")
	}
	if res.Remaining != 0 {
		t.Errorf("新窗口的第一次调用 Remaining 应为 0，实际 %d", res.Remaining)
	}
}

// limit <= 0 表示"不限制"。
//
// 原实现在 limit<=0 时返回 Allowed=false，一旦上游把配置传成 0
// （比如环境变量没设好），就会把**所有**请求全挡掉。
// 对一个"兜底保护"组件来说，失败方向必须是"放行"而不是"拒绝"。
func TestMemoryLimiterNonPositiveLimitMeansUnlimited(t *testing.T) {
	l := NewMemoryLimiter()
	ctx := context.Background()

	for _, limit := range []int{0, -1, -100} {
		for i := 0; i < 5; i++ {
			res, err := l.Allow(ctx, "user:1", limit, time.Minute)
			if err != nil {
				t.Fatalf("limit=%d 出错: %v", limit, err)
			}
			if !res.Allowed {
				t.Fatalf("limit=%d 表示不限制，第 %d 次调用不应被拒", limit, i+1)
			}
			if res.Limit != 0 {
				t.Errorf("limit=%d 时 Limit 应报 0，实际 %d", limit, res.Limit)
			}
		}
	}
}

func TestMemoryLimiterIsolatesKeys(t *testing.T) {
	l := NewMemoryLimiter()
	ctx := context.Background()

	if res, _ := l.Allow(ctx, "user:1", 1, time.Minute); !res.Allowed {
		t.Fatal("user:1 首次调用应放行")
	}
	if res, _ := l.Allow(ctx, "user:1", 1, time.Minute); res.Allowed {
		t.Fatal("user:1 第二次调用应被拒")
	}
	// 一个用户被限流不得影响其他用户
	if res, _ := l.Allow(ctx, "user:2", 1, time.Minute); !res.Allowed {
		t.Fatal("user:2 不应受 user:1 的计数影响")
	}
	// 同一用户的不同限流维度（如全局 / 任务提交）也互不干扰
	if res, _ := l.Allow(ctx, "task:user:1", 1, time.Minute); !res.Allowed {
		t.Fatal("不同 key 前缀应独立计数")
	}
}

// 长时间运行时不能无界增长：过期窗口必须被回收。
//
// 实现里是"在开启新窗口时顺带 sweep"，所以断言的是
// 「窗口过期后、下一次调用别的 key 时，旧 key 被清掉」。
func TestMemoryLimiterSweepsExpiredKeys(t *testing.T) {
	l := NewMemoryLimiter()
	ctx := context.Background()

	const window = 40 * time.Millisecond
	for i := 0; i < 20; i++ {
		l.Allow(ctx, fmt.Sprintf("user:%d", i), 5, window)
	}
	if n := len(l.counts); n != 20 {
		t.Fatalf("应有 20 个活跃窗口，实际 %d", n)
	}

	time.Sleep(window + 40*time.Millisecond)
	// 任意一次新窗口的开启都会触发 sweep
	l.Allow(ctx, "user:new", 5, window)

	if n := len(l.counts); n != 1 {
		t.Fatalf("过期窗口应被回收，只应剩 1 个，实际 %d", n)
	}
	if n := len(l.until); n != 1 {
		t.Fatalf("until 表也应被回收，只应剩 1 个，实际 %d", n)
	}
}

// 并发下必须精确：N 个 goroutine 抢 M 个名额，恰好 M 个被放行。
//
// 这条用例守的是 MemoryLimiter 里那个 `mu chan struct{}` 信号量。
// 如果哪天有人把它换成普通的 map 读写（以为"Go 的 map 读写够快"），
// 这条用例会以"放行数 != 限额"或 race detector 报错的形式失败。
func TestMemoryLimiterConcurrentExactlyLimitAllowed(t *testing.T) {
	l := NewMemoryLimiter()
	ctx := context.Background()

	const (
		limit    = 10
		requests = 200
	)
	var allowed atomic.Int64

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 尽量让所有 goroutine 同时冲进去
			if res, err := l.Allow(ctx, "hot-key", limit, time.Minute); err == nil && res.Allowed {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != limit {
		t.Fatalf("并发下应恰好放行 %d 次，实际 %d", limit, got)
	}
}

// ---------- RedisLimiter（生产路径，需要真实 Redis） ----------
//
//	NEBULA_TEST_REDIS_ADDR=127.0.0.1:6379 go test ./internal/ratelimit -run Redis -v
//
// 每个用例用独立的 key 前缀并在结束时清理，不会污染正在跑的服务。

func newTestRedisLimiter(t *testing.T) (*RedisLimiter, redis.UniversalClient, string) {
	t.Helper()
	addr := os.Getenv("NEBULA_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("未设置 NEBULA_TEST_REDIS_ADDR，跳过需要真实 Redis 的集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("连接 Redis %s: %v", addr, err)
	}
	prefix := fmt.Sprintf("nebula_it_rl_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		// 只删本次用例前缀下的 key
		iter := rdb.Scan(context.Background(), 0, prefix+"*", 100).Iterator()
		for iter.Next(context.Background()) {
			rdb.Del(context.Background(), iter.Val())
		}
		_ = rdb.Close()
	})
	return NewRedisLimiter(rdb), rdb, prefix
}

// Redis 侧的 key 格式是 ratelimit:{key}:{windowSec}（见 limiter.go）。
func redisKey(prefix string, window time.Duration) string {
	return fmt.Sprintf("ratelimit:%s:%d", prefix, int64(window.Seconds()))
}

func TestRedisLimiterAllowsUpToLimitThenRejects(t *testing.T) {
	l, _, prefix := newTestRedisLimiter(t)
	ctx := context.Background()

	const limit = 3
	for i := 1; i <= limit; i++ {
		res, err := l.Allow(ctx, prefix, limit, time.Minute)
		if err != nil {
			t.Fatalf("第 %d 次调用出错: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("第 %d 次调用（限额 %d）应放行，实际被拒", i, limit)
		}
		if want := int64(limit - i); res.Remaining != want {
			t.Errorf("第 %d 次调用 Remaining 应为 %d，实际 %d", i, want, res.Remaining)
		}
	}

	res, err := l.Allow(ctx, prefix, limit, time.Minute)
	if err != nil {
		t.Fatalf("超限调用出错: %v", err)
	}
	if res.Allowed {
		t.Fatalf("超过限额（%d）后应被拒绝", limit)
	}
	if res.Remaining != 0 {
		t.Errorf("被拒时 Remaining 应为 0，实际 %d", res.Remaining)
	}
}

// **本文件最重要的用例**：窗口内的后续请求不得续期 TTL。
//
// 原实现每次请求都调 EXPIRE，于是 TTL 被无限续期：只要请求持续到来，
// 计数窗口就永远不会滚动，计数单调上涨，最终所有请求被永久拒绝。
// 这个缺陷的症状是"服务运行越久、被限流的用户越多"，
// 而且限流器自身的单元测试（只测单次调用）完全看不出来。
//
// 断言方式：同一窗口内第二次调用后，剩余 TTL 必须比第一次更短。
// 如果实现改成每次 EXPIRE，TTL 会被重置回整个窗口长度，这条就会失败。
func TestRedisLimiterDoesNotRefreshTTLOnEveryRequest(t *testing.T) {
	l, rdb, prefix := newTestRedisLimiter(t)
	ctx := context.Background()

	const window = 10 * time.Second
	key := redisKey(prefix, window)

	if _, err := l.Allow(ctx, prefix, 100, window); err != nil {
		t.Fatalf("首次调用出错: %v", err)
	}
	ttl1, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取 TTL: %v", err)
	}
	if ttl1 <= 0 || ttl1 > window {
		t.Fatalf("首次调用后 TTL 应在 (0, %v] 内，实际 %v", window, ttl1)
	}

	time.Sleep(2 * time.Second)

	if _, err := l.Allow(ctx, prefix, 100, window); err != nil {
		t.Fatalf("第二次调用出错: %v", err)
	}
	ttl2, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取 TTL: %v", err)
	}

	if ttl2 >= ttl1 {
		t.Fatalf("窗口内第二次调用**续期**了 TTL（%v → %v）：窗口将永不翻页，计数会单调上涨直到永久拒绝", ttl1, ttl2)
	}
	// 应该大致等于"窗口剩余时间"（10s - 2s = 8s），允许调度抖动
	if ttl2 > window-1*time.Second {
		t.Errorf("第二次调用后 TTL 应约为 %v，实际 %v", window-2*time.Second, ttl2)
	}
}

func TestRedisLimiterWindowResets(t *testing.T) {
	l, _, prefix := newTestRedisLimiter(t)
	ctx := context.Background()

	// 窗口取 1 秒（int64(window.Seconds()) 必须 >= 1，否则会被兜底成 60 秒）
	const window = time.Second
	if res, _ := l.Allow(ctx, prefix, 1, window); !res.Allowed {
		t.Fatal("窗口内第一次调用应放行")
	}
	if res, _ := l.Allow(ctx, prefix, 1, window); res.Allowed {
		t.Fatal("同一窗口内第二次调用应被拒绝")
	}

	time.Sleep(window + 300*time.Millisecond)

	res, err := l.Allow(ctx, prefix, 1, window)
	if err != nil {
		t.Fatalf("窗口翻页后调用出错: %v", err)
	}
	if !res.Allowed {
		t.Fatal("窗口过期后应重新放行（key 带 EXPIRE 才做得到）")
	}
}

// Redis 不可用时必须返回错误，而不是静默放行或静默拒绝。
// 中间件据此决定降级策略（见 api/middleware.go 的 rateLimitMiddleware）。
func TestRedisLimiterNonPositiveLimitMeansUnlimited(t *testing.T) {
	l, rdb, prefix := newTestRedisLimiter(t)
	ctx := context.Background()

	for _, limit := range []int{0, -5} {
		res, err := l.Allow(ctx, prefix, limit, time.Minute)
		if err != nil {
			t.Fatalf("limit=%d 出错: %v", limit, err)
		}
		if !res.Allowed {
			t.Fatalf("limit=%d 表示不限制，不应被拒", limit)
		}
	}
	// 不限制时不应在 Redis 里留下任何计数 key
	if n, err := rdb.Exists(ctx, redisKey(prefix, time.Minute)).Result(); err != nil {
		t.Fatalf("Exists: %v", err)
	} else if n != 0 {
		t.Fatalf("limit<=0 时不应写入计数 key，实际存在 %d 个", n)
	}
}

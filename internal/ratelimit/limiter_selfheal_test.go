package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 这个文件守的是「Redis 限流器不会把用户永久锁死」这条可用性边界。
//
// 缺陷回顾：INCR 之后只在 n == 1 时设置 EXPIRE，而 Expire 失败时原实现只是
// "降级为放行"（返回 Allowed=true）就返回了 —— 这一次请求放行了，
// 但那个 key 已经留在 Redis 里且**没有 TTL**。后续请求走到 n > 1 的分支，
// 永远不会再尝试设置 TTL，于是：
//
//	计数只增不减 → 一个窗口之后用户被**永久** 429，重启服务才能恢复。
//
// 这是最糟糕的一类故障组合：出事的那一瞬间看起来没事（请求被放行了），
// 真正的问题在一个窗口之后才爆发，而且症状（个别用户永远被限流）
// 和原因（一次 Redis Expire 抖动）之间隔着十万八千里。
//
// 修复：Expire 失败时自愈 —— 删掉这个没 TTL 的 key（下次从 n=1 重新开始，
// 能重新设置 TTL），并把错误上报给中间件由它降级放行。

// flakyRedis 是一个可控的 redis 替身：只实现本包用到的 INCR / EXPIRE / DEL，
// 并允许模拟 EXPIRE 失败。
//
// 为什么不用真 Redis：要让"第一次 EXPIRE 失败"稳定复现，必须能精确控制失败时机；
// 真 Redis 上只能靠 kill 连接来碰运气，那种测试迟早会变成 flaky 的假绿。
// 嵌入 redis.UniversalClient 是为了满足接口（其余方法永远不会被本包调用）。
type flakyRedis struct {
	redis.UniversalClient

	mu         sync.Mutex
	counts     map[string]int64
	expires    map[string]time.Time
	expireErrs int // 还需要失败几次 Expire
	expireCall int
	delCall    int
}

func newFlakyRedis() *flakyRedis {
	return &flakyRedis{
		counts:  map[string]int64{},
		expires: map[string]time.Time{},
	}
}

// dropExpiredLocked 模拟 Redis 的 TTL 语义：到期的 key 视为不存在。
func (f *flakyRedis) dropExpiredLocked(now time.Time) {
	for k, exp := range f.expires {
		if !now.Before(exp) {
			delete(f.expires, k)
			delete(f.counts, k)
		}
	}
}

func (f *flakyRedis) Incr(_ context.Context, key string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropExpiredLocked(time.Now())
	f.counts[key]++
	return redis.NewIntResult(f.counts[key], nil)
}

func (f *flakyRedis) Expire(_ context.Context, key string, ttl time.Duration) *redis.BoolCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireCall++
	if f.expireErrs > 0 {
		f.expireErrs--
		return redis.NewBoolResult(false, errors.New("模拟的网络抖动：EXPIRE 失败"))
	}
	if _, ok := f.counts[key]; !ok {
		return redis.NewBoolResult(false, nil)
	}
	f.expires[key] = time.Now().Add(ttl)
	return redis.NewBoolResult(true, nil)
}

func (f *flakyRedis) Del(_ context.Context, keys ...string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCall++
	var n int64
	for _, k := range keys {
		if _, ok := f.counts[k]; ok {
			delete(f.counts, k)
			delete(f.expires, k)
			n++
		}
	}
	return redis.NewIntResult(n, nil)
}

// liveKeys 返回当前在"库里"的计数 key 数（用于断言没有残留的毒 key）。
func (f *flakyRedis) liveKeys() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropExpiredLocked(time.Now())
	return len(f.counts)
}

// **本条是缺陷 6 的核心回归测试**：第一次 EXPIRE 失败之后，
// 窗口过去这个用户必须能重新通过（而不是被永久拒绝）。
func TestRedisLimiterSelfHealsWhenExpireFails(t *testing.T) {
	f := newFlakyRedis()
	f.expireErrs = 1 // 只让第一次 EXPIRE 失败
	l := NewRedisLimiter(f)
	ctx := context.Background()

	// 注意：limiter 内部用 int64(window.Seconds()) 计算 TTL，亚秒窗口会被兜底成 60 秒，
	// 所以这里必须用整秒窗口（与生产配置一致）。
	const window = time.Second
	const limit = 1

	// --- 第一次：INCR 成功、EXPIRE 失败 ---
	res, err := l.Allow(ctx, "user:1", limit, window)
	if err == nil {
		t.Fatal("EXPIRE 失败必须上报错误，让中间件有机会按降级策略处理（而不是静默吞掉）")
	}
	if !res.Allowed {
		t.Fatal("基础设施故障时不能把用户挡在门外（fail-open）")
	}
	if n := f.liveKeys(); n != 0 {
		t.Fatalf("EXPIRE 失败后必须清掉这个没有 TTL 的计数 key，实际残留 %d 个："+
			"它会永久累积，用户将被永久 429", n)
	}
	if f.delCall == 0 {
		t.Fatal("自愈必须真的调用 DEL")
	}

	// --- 之后的请求：EXPIRE 恢复正常 ---
	res, err = l.Allow(ctx, "user:1", limit, window)
	if err != nil {
		t.Fatalf("EXPIRE 恢复后不应报错: %v", err)
	}
	if !res.Allowed {
		t.Fatal("毒 key 已被清掉，这是新窗口的第一次计数，应放行")
	}
	if res, _ = l.Allow(ctx, "user:1", limit, window); res.Allowed {
		t.Fatal("同一窗口内超过限额应被拒绝（自愈不能把限流功能整个废掉）")
	}

	// --- 关键断言：窗口过去后必须重新放行 ---
	time.Sleep(window + 300*time.Millisecond)
	res, err = l.Allow(ctx, "user:1", limit, window)
	if err != nil {
		t.Fatalf("窗口翻页后不应报错: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("窗口过去后应重新放行；被拒说明一次 EXPIRE 抖动把用户**永久**锁死了"+
			"（liveKeys=%d）", f.liveKeys())
	}
}

// 对照组：EXPIRE 一直正常时，限流语义不能被"自愈逻辑"改变。
func TestRedisLimiterNormalPathUnaffected(t *testing.T) {
	f := newFlakyRedis()
	l := NewRedisLimiter(f)
	ctx := context.Background()

	const window = time.Second
	if res, err := l.Allow(ctx, "user:1", 2, window); err != nil || !res.Allowed {
		t.Fatalf("第一次应放行且无错误，实际 %+v err=%v", res, err)
	}
	if res, err := l.Allow(ctx, "user:1", 2, window); err != nil || !res.Allowed {
		t.Fatalf("第二次应放行且无错误，实际 %+v err=%v", res, err)
	}
	if res, _ := l.Allow(ctx, "user:1", 2, window); res.Allowed {
		t.Fatal("第三次超过限额应被拒绝")
	}
	if f.delCall != 0 {
		t.Fatalf("EXPIRE 正常时不应触发自愈（DEL），实际调用了 %d 次", f.delCall)
	}
	if f.expireCall != 1 {
		t.Fatalf("只应在窗口的第一次计数时 EXPIRE 一次（每次续期会让窗口永不翻页），实际 %d 次",
			f.expireCall)
	}

	time.Sleep(window + 300*time.Millisecond)
	if res, _ := l.Allow(ctx, "user:1", 2, window); !res.Allowed {
		t.Fatal("窗口过期后应重新放行")
	}
}

// EXPIRE 连续失败时，每一次失败都必须把 key 清掉（不能只是第一次自愈）。
func TestRedisLimiterSelfHealsRepeatedExpireFailures(t *testing.T) {
	f := newFlakyRedis()
	f.expireErrs = 5
	l := NewRedisLimiter(f)
	ctx := context.Background()

	const window = time.Second
	for i := 0; i < 5; i++ {
		res, err := l.Allow(ctx, "user:9", 1, window)
		if err == nil {
			t.Fatalf("第 %d 次 EXPIRE 失败应上报错误", i+1)
		}
		if !res.Allowed {
			t.Fatalf("第 %d 次：故障期间必须放行（fail-open）", i+1)
		}
		if n := f.liveKeys(); n != 0 {
			t.Fatalf("第 %d 次之后仍有 %d 个没有 TTL 的 key 残留", i+1, n)
		}
	}

	// 故障恢复后，窗口语义必须完全正常
	if res, err := l.Allow(ctx, "user:9", 1, window); err != nil || !res.Allowed {
		t.Fatalf("恢复后第一次应放行，实际 %+v err=%v", res, err)
	}
	if res, _ := l.Allow(ctx, "user:9", 1, window); res.Allowed {
		t.Fatal("恢复后同一窗口内超限应被拒绝")
	}
	time.Sleep(window + 300*time.Millisecond)
	if res, _ := l.Allow(ctx, "user:9", 1, window); !res.Allowed {
		t.Fatal("恢复后窗口过期应重新放行")
	}
}

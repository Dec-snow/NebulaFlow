package llm

import (
	"sync"
	"testing"
	"time"
)

// ---------- 单个 CircuitBreaker 状态流转测试 ----------

// 初始状态为 Closed，允许请求通过。
func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := NewCircuitBreaker()
	if cb.State() != StateClosed {
		t.Fatalf("expected closed, got %v", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("should allow requests in closed state")
	}
}

// 连续失败达到阈值后进入 Open 状态，拒绝请求。
func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreakerWithConfig(3, time.Minute)
	for i := 0; i < 2; i++ {
		cb.RecordFailure()
		if cb.State() != StateClosed {
			t.Fatalf("should still be closed after %d failures", i+1)
		}
		if !cb.Allow() {
			t.Fatal("should allow before threshold")
		}
	}
	// 第 3 次失败 → 熔断
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open, got %v", cb.State())
	}
	if cb.Allow() {
		t.Fatal("should not allow in open state")
	}
}

// 成功后重置失败计数（Closed 状态下）。
func TestCircuitBreaker_SuccessResetsCount(t *testing.T) {
	cb := NewCircuitBreakerWithConfig(5, time.Minute)
	for i := 0; i < 3; i++ {
		cb.RecordFailure()
	}
	if cb.FailureCount() != 3 {
		t.Fatalf("expected 3 failures, got %d", cb.FailureCount())
	}
	cb.RecordSuccess()
	if cb.FailureCount() != 0 {
		t.Fatalf("failure count should reset to 0, got %d", cb.FailureCount())
	}
	if cb.State() != StateClosed {
		t.Fatalf("state should remain closed after success")
	}
}

// 冷却期过后进入 HalfOpen，放一个请求试探。
func TestCircuitBreaker_HalfOpenAfterCoolDown(t *testing.T) {
	cb := NewCircuitBreakerWithConfig(2, 50*time.Millisecond)
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open, got %v", cb.State())
	}

	// 冷却期内仍然是 Open
	if cb.Allow() {
		t.Fatal("should not allow during cool down")
	}

	// 等待冷却期结束
	time.Sleep(60 * time.Millisecond)

	// 冷却后应为 HalfOpen，允许一次试探
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half-open after cool down, got %v", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("should allow one probe in half-open state")
	}
}

// HalfOpen 状态下成功 → 回到 Closed。
func TestCircuitBreaker_HalfOpenSuccessCloses(t *testing.T) {
	cb := NewCircuitBreakerWithConfig(2, 20*time.Millisecond)
	cb.RecordFailure()
	cb.RecordFailure() // → Open

	time.Sleep(30 * time.Millisecond)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half-open, got %v", cb.State())
	}

	cb.RecordSuccess()
	if cb.State() != StateClosed {
		t.Fatalf("expected closed after half-open success, got %v", cb.State())
	}
	if cb.FailureCount() != 0 {
		t.Fatalf("failure count should be 0, got %d", cb.FailureCount())
	}
}

// HalfOpen 状态下失败 → 回到 Open 并重新计时冷却。
func TestCircuitBreaker_HalfOpenFailureReopens(t *testing.T) {
	cb := NewCircuitBreakerWithConfig(2, 30*time.Millisecond)
	cb.RecordFailure()
	cb.RecordFailure() // → Open

	time.Sleep(40 * time.Millisecond)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half-open, got %v", cb.State())
	}

	cb.RecordFailure() // 试探失败 → 回到 Open
	if cb.State() != StateOpen {
		t.Fatalf("expected open after half-open failure, got %v", cb.State())
	}

	// 重新进入 Open，冷却期应该重新计时
	// 立刻查询应该还是 Open
	if cb.Allow() {
		t.Fatal("should not allow right after re-open")
	}

	// 再等冷却期过后才能再次 HalfOpen
	time.Sleep(40 * time.Millisecond)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half-open after second cool down, got %v", cb.State())
	}
}

// ---------- Registry 测试 ----------

// Registry 为不同 provider 维护独立的熔断器。
func TestCircuitBreakerRegistry_IndependentBreakers(t *testing.T) {
	r := NewCircuitBreakerRegistryWithConfig(2, time.Minute)

	// p1 熔断
	r.RecordFailure("p1")
	r.RecordFailure("p1")
	if r.State("p1") != StateOpen {
		t.Fatalf("p1 should be open")
	}

	// p2 不受影响
	if r.State("p2") != StateClosed {
		t.Fatalf("p2 should be closed, got %v", r.State("p2"))
	}
	if !r.Allow("p2") {
		t.Fatal("p2 should allow requests")
	}
}

// Registry 的 Allow / RecordSuccess / RecordFailure 正常工作。
func TestCircuitBreakerRegistry_BasicFlow(t *testing.T) {
	r := NewCircuitBreakerRegistryWithConfig(3, time.Minute)
	name := "test-provider"

	if !r.Allow(name) {
		t.Fatal("should allow initially")
	}

	r.RecordFailure(name)
	r.RecordFailure(name)
	if !r.Allow(name) {
		t.Fatal("should allow before threshold")
	}

	r.RecordFailure(name) // 第 3 次 → 熔断
	if r.Allow(name) {
		t.Fatal("should not allow after threshold")
	}
	if r.State(name) != StateOpen {
		t.Fatalf("expected open, got %v", r.State(name))
	}

	r.RecordSuccess(name) // 外部强制重置（如手动恢复）
	if r.State(name) != StateClosed {
		t.Fatalf("expected closed after success, got %v", r.State(name))
	}
	if !r.Allow(name) {
		t.Fatal("should allow after reset")
	}
}

// ---------- 并发安全测试 ----------

// 并发调用 RecordFailure / Allow 不应出现数据竞争。
// 必须配合 -race 运行。
func TestCircuitBreaker_ConcurrentAccess(t *testing.T) {
	cb := NewCircuitBreakerWithConfig(100, time.Minute)
	var wg sync.WaitGroup
	const goroutines = 20
	const iterations = 100

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if id%2 == 0 {
					cb.RecordFailure()
				} else {
					cb.RecordSuccess()
				}
				_ = cb.Allow()
				_ = cb.State()
				_ = cb.FailureCount()
			}
		}(g)
	}
	wg.Wait()
	// 不做精确断言——只要不 panic / 不 race 就算通过
}

// Registry 并发安全测试。
func TestCircuitBreakerRegistry_ConcurrentAccess(t *testing.T) {
	r := NewCircuitBreakerRegistryWithConfig(10, time.Minute)
	var wg sync.WaitGroup
	const goroutines = 20
	const iterations = 100

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				name := "provider-" + string(rune('a'+id%5))
				if id%3 == 0 {
					r.RecordFailure(name)
				} else if id%3 == 1 {
					r.RecordSuccess(name)
				} else {
					_ = r.Allow(name)
				}
			}
		}(g)
	}
	wg.Wait()
}

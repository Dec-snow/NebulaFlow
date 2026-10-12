package llm

import (
	"sync"
	"time"
)

// CircuitState 表示熔断器的三种状态。
type CircuitState int

const (
	// StateClosed：正常状态，请求正常通过。
	StateClosed CircuitState = iota
	// StateOpen：熔断状态，连续失败达到阈值，直接拒绝请求。
	StateOpen
	// StateHalfOpen：半开状态，冷却期过后放一个请求试探。
	StateHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// 默认熔断参数。
const (
	// defaultFailureThreshold 连续失败次数阈值，达到后熔断。
	defaultFailureThreshold = 5
	// defaultCoolDownDuration 熔断后冷却时间，进入半开状态。
	defaultCoolDownDuration = 30 * time.Second
)

// CircuitBreaker 是单个 provider 的轻量熔断器。
//
// 状态流转：
//
//	Closed ──连续失败 N 次──▶ Open ──冷却期后──▶ HalfOpen ──试探成功──▶ Closed
//	                                              │
//	                                              └──试探失败──▶ Open（重置冷却）
//
// 设计要点：
//   - 每个 provider 独立维护自己的熔断器，互不影响；
//   - 用 sync.Mutex 保护状态与计数，并发安全；
//   - 失败只在「真正请求过 provider」后才计数，context 取消类错误不计入
//     （否则上层并发取消时会把所有 provider 一起熔断）。
type CircuitBreaker struct {
	mu               sync.Mutex
	state            CircuitState
	failureCount     int
	failureThreshold int
	coolDownDuration time.Duration
	openAt           time.Time // 进入 Open 状态的时间，用于计算冷却是否结束
}

// NewCircuitBreaker 创建一个熔断器，使用默认参数。
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: defaultFailureThreshold,
		coolDownDuration: defaultCoolDownDuration,
	}
}

// NewCircuitBreakerWithConfig 创建一个可配置阈值的熔断器（测试用）。
func NewCircuitBreakerWithConfig(threshold int, coolDown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = defaultFailureThreshold
	}
	if coolDown <= 0 {
		coolDown = defaultCoolDownDuration
	}
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: threshold,
		coolDownDuration: coolDown,
	}
}

// State 返回当前熔断器状态（线程安全）。
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.maybeTransition()
	return cb.state
}

// Allow 判断当前是否允许请求通过。
// 返回值：
//   - true：允许请求（Closed 或 HalfOpen）
//   - false：拒绝请求（Open 且尚未冷却）
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.maybeTransition()
	return cb.state != StateOpen
}

// maybeTransition 在持有锁的前提下检查是否需要状态迁移。
// 仅当当前为 Open 且冷却期已过时，迁移到 HalfOpen。
func (cb *CircuitBreaker) maybeTransition() {
	if cb.state == StateOpen && time.Since(cb.openAt) >= cb.coolDownDuration {
		cb.state = StateHalfOpen
	}
}

// RecordSuccess 记录一次成功请求，重置失败计数并回到 Closed。
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount = 0
	cb.state = StateClosed
}

// RecordFailure 记录一次失败请求。
//
// 注意：只有「provider 真实返回错误」才应调用此方法；
// context 取消 / deadline exceeded 这类调用方主动中止的情况不应计入，
// 否则一次批量取消会把所有 provider 全部熔断。
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount++
	switch cb.state {
	case StateClosed:
		if cb.failureCount >= cb.failureThreshold {
			cb.state = StateOpen
			cb.openAt = time.Now()
		}
	case StateHalfOpen:
		// 半开状态下一次失败就立即回到 Open，重新计时冷却
		cb.state = StateOpen
		cb.openAt = time.Now()
	case StateOpen:
		// Open 状态下理论上不会有新请求失败（Allow 会拒绝），
		// 但防御性地重置 openAt，避免极端竞态下冷却被提前结束。
		cb.openAt = time.Now()
	}
}

// FailureCount 返回当前连续失败次数（用于观测/调试）。
func (cb *CircuitBreaker) FailureCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failureCount
}

// ---------- Provider 级熔断管理器 ----------

// CircuitBreakerRegistry 维护每个 provider 名称对应的熔断器实例。
// Gateway 通过它在选择 provider 前查询熔断状态，跳过 Open 状态的 provider。
type CircuitBreakerRegistry struct {
	mu       sync.Mutex
	breakers map[string]*CircuitBreaker

	failureThreshold int
	coolDownDuration time.Duration
}

// NewCircuitBreakerRegistry 创建熔断器注册表，使用默认参数。
func NewCircuitBreakerRegistry() *CircuitBreakerRegistry {
	return &CircuitBreakerRegistry{
		breakers:         make(map[string]*CircuitBreaker),
		failureThreshold: defaultFailureThreshold,
		coolDownDuration: defaultCoolDownDuration,
	}
}

// NewCircuitBreakerRegistryWithConfig 创建可配置的熔断器注册表（测试用）。
func NewCircuitBreakerRegistryWithConfig(threshold int, coolDown time.Duration) *CircuitBreakerRegistry {
	if threshold <= 0 {
		threshold = defaultFailureThreshold
	}
	if coolDown <= 0 {
		coolDown = defaultCoolDownDuration
	}
	return &CircuitBreakerRegistry{
		breakers:         make(map[string]*CircuitBreaker),
		failureThreshold: threshold,
		coolDownDuration: coolDown,
	}
}

// getOrCreate 获取或创建指定 provider 的熔断器。
// 调用方必须持有 mu 锁。
func (r *CircuitBreakerRegistry) getOrCreate(name string) *CircuitBreaker {
	if cb, ok := r.breakers[name]; ok {
		return cb
	}
	cb := NewCircuitBreakerWithConfig(r.failureThreshold, r.coolDownDuration)
	r.breakers[name] = cb
	return cb
}

// Allow 查询指定 provider 是否允许请求通过。
func (r *CircuitBreakerRegistry) Allow(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getOrCreate(name).Allow()
}

// RecordSuccess 记录指定 provider 的成功请求。
func (r *CircuitBreakerRegistry) RecordSuccess(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getOrCreate(name).RecordSuccess()
}

// RecordFailure 记录指定 provider 的失败请求。
func (r *CircuitBreakerRegistry) RecordFailure(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getOrCreate(name).RecordFailure()
}

// State 返回指定 provider 的当前熔断状态（用于观测）。
func (r *CircuitBreakerRegistry) State(name string) CircuitState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getOrCreate(name).State()
}

// FailureCount 返回指定 provider 的连续失败次数。
func (r *CircuitBreakerRegistry) FailureCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getOrCreate(name).FailureCount()
}

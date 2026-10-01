package model

import (
	"math"
	"time"
)

// ---------- 任务优先级 ----------

const (
	PriorityHigh   = "high"
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// ValidPriority 校验优先级是否合法。
func ValidPriority(p string) bool {
	switch p {
	case PriorityHigh, PriorityNormal, PriorityLow:
		return true
	}
	return false
}

// NormalizePriority 归一化优先级，空值或非法值回退为 normal。
func NormalizePriority(p string) string {
	if ValidPriority(p) {
		return p
	}
	return PriorityNormal
}

// PriorityWeight 返回优先级的权重（用于调度时的比例分配）。
// high: 3, normal: 2, low: 1
// 意思是：每次调度，high 队列拿 3 个，normal 拿 2 个，low 拿 1 个
// 这样既保证了高优先级先处理，又不会让低优先级"饿死"。
func PriorityWeight(p string) int {
	switch p {
	case PriorityHigh:
		return 3
	case PriorityNormal:
		return 2
	case PriorityLow:
		return 1
	}
	return 2
}

// ---------- 重试策略 ----------

const (
	BackoffExponential = "exponential" // 指数退避
	BackoffFixed       = "fixed"       // 固定间隔
	BackoffImmediate   = "immediate"   // 立即重试
)

// RetryPolicy 是节点的重试配置。
type RetryPolicy struct {
	MaxRetry int           `json:"max_retry"`
	Backoff  string        `json:"backoff"`
	Base     time.Duration `json:"base"` // 基础间隔，指数退避的起始值或固定间隔的值
	MaxDelay time.Duration `json:"max_delay"` // 最大延迟（指数退避的上限）
}

// DefaultRetryPolicy 是默认重试策略（不重试）。
var DefaultRetryPolicy = RetryPolicy{
	MaxRetry: 0,
	Backoff:  BackoffExponential,
	Base:     1 * time.Second,
	MaxDelay: 30 * time.Second,
}

// ShouldRetry 判断是否应该重试（已重试次数 < 最大重试次数）。
func (r RetryPolicy) ShouldRetry(retryCount int) bool {
	return r.MaxRetry > 0 && retryCount < r.MaxRetry
}

// Delay 返回第 retryCount 次重试的等待时间。
//
// retryCount 是"已重试次数"，第一次重试时 retryCount = 0。
//
// 策略说明：
//   - immediate: 立即重试，延迟为 0
//   - fixed: 固定间隔，每次都是 base
//   - exponential: 指数退避，base * 2^retryCount，不超过 max_delay
func (r RetryPolicy) Delay(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}

	switch r.Backoff {
	case BackoffImmediate:
		return 0

	case BackoffFixed:
		if r.Base <= 0 {
			return 1 * time.Second
		}
		return r.Base

	default: // exponential
		base := r.Base
		if base <= 0 {
			base = 1 * time.Second
		}
		maxDelay := r.MaxDelay
		if maxDelay <= 0 {
			maxDelay = 30 * time.Second
		}

		// 指数计算：base * 2^retryCount
		factor := math.Pow(2, float64(retryCount))
		delay := time.Duration(float64(base) * factor)

		// 加上一点随机抖动（±20%），避免重试风暴
		// 所有客户端同时失败 → 同时重试 → 把下游打挂
		jitter := time.Duration(float64(delay) * 0.2 * (float64(hashInt(retryCount)) / float64(math.MaxInt32) - 0.5))
		delay += jitter

		if delay > maxDelay {
			delay = maxDelay
		}
		if delay < 0 {
			delay = 0
		}
		return delay
	}
}

// hashInt 是一个简单的整数哈希，用于生成重试抖动。
// 用确定性哈希而不是 rand，保证同一次重试的延迟是可预测的
// （便于测试和排障）。
func hashInt(n int) int {
	// xorshift 32-bit
	x := uint32(n*2654435761) ^ 0x9e3779b9
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	return int(x) & 0x7fffffff
}

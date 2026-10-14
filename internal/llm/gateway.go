package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// UsageFunc 在每个成功的模型调用后回调（用于写 usage_records 与指标）。
type UsageFunc func(u *Usage)

// ErrAllProvidersCircuitOpen 表示本次调用里所有 provider 都因熔断被跳过，
// 一个上游模型都没有真正被调用过。
//
// 为什么需要这个哨兵：原实现只有"真正调用过 provider 并且失败"才会给 lastErr
// 赋值，于是"全部被熔断跳过"这条分支上 lastErr 仍是 nil，
// `fmt.Errorf("all llm providers failed: %w", nil)` 会产出
// `all llm providers failed: %!w(<nil>)`：
//   - 日志里只剩一句看不懂的畸形字符串，运维拿不到任何线索；
//   - errors.Is / errors.Unwrap 全部失效，上层无法区分"熔断"与"真失败"。
//
// 这恰恰发生在"所有模型都挂了"这种最需要信息的时刻，所以必须显式给一个错误值。
var ErrAllProvidersCircuitOpen = errors.New("all providers skipped by circuit breaker")

// allProvidersFailedError 组装"所有 provider 都失败"的聚合错误。
//
//	action:  场景前缀文案（Chat / Stream 各自的措辞）
//	skipped: 因熔断被跳过的 provider 名
//	lastErr: 最后一次真实失败的原因；全部被跳过时为 nil
//
// 返回值保证一定是个"真"错误：lastErr 为 nil 时退化为 ErrAllProvidersCircuitOpen，
// 绝不把 nil 塞进 %w。被跳过的 provider 名会附在错误串里，便于直接定位。
func allProvidersFailedError(action string, skipped []string, lastErr error) error {
	if lastErr == nil {
		lastErr = ErrAllProvidersCircuitOpen
	}
	if len(skipped) > 0 {
		return fmt.Errorf("%s: %w (circuit breaker open: %s)",
			action, lastErr, strings.Join(skipped, ", "))
	}
	return fmt.Errorf("%s: %w", action, lastErr)
}

type Usage struct {
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	LatencyMS    int64
	Error        string
}

// Gateway 是上层（scheduler）唯一依赖的入口：
//   - 按 providers 顺序依次尝试（primary → fallback）
//   - Chat：每次尝试带独立超时，失败即换下一个 provider
//   - Stream：等待首个 token 超时 / 流未产出任何内容即换下一个 provider；
//     一旦已经开始产出内容就"已提交"，中途失败不再回退
//     （否则用户会看到两段拼接的重复输出）
//   - 熔断器：连续失败达到阈值的 provider 会被临时跳过，避免每次都先
//     试一个已知挂掉的 provider 浪费超时时间
type Gateway struct {
	// mu 保护 providers：网关支持运行期热更新（用户在 UI 里配好 provider
	// 后应当立刻生效，而不是"必须重启服务"）。
	//
	// 原实现只在启动时读一次数据库：配置好 provider 之后节点仍然走 Mock 兜底，
	// 输出里带着 [mock-...] 前缀，用户完全看不出"我配的 key 根本没生效"。
	mu                sync.RWMutex
	providers         []Provider // 按优先级排序（已启用）
	logger            *slog.Logger
	attemptTimeout    time.Duration
	firstTokenTimeout time.Duration // 等待首个 token 的上限，超时即切换 provider
	onUsage           UsageFunc
	breakers          *CircuitBreakerRegistry // 每个 provider 独立熔断器
}

// defaultFirstTokenTimeout 是等待模型吐出第一个 token 的默认上限。
// 只卡"首 token"而不卡整条流：长回答可能持续几十秒，
// 整体时长应由上层的节点/任务超时来兜底，网关不应擅自截断。
const defaultFirstTokenTimeout = 30 * time.Second

func NewGateway(providers []Provider, logger *slog.Logger, attemptTimeout time.Duration, onUsage UsageFunc) *Gateway {
	if attemptTimeout <= 0 {
		attemptTimeout = 60 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gateway{
		providers:         providers,
		logger:            logger,
		attemptTimeout:    attemptTimeout,
		firstTokenTimeout: defaultFirstTokenTimeout,
		onUsage:           onUsage,
		breakers:          NewCircuitBreakerRegistry(),
	}
}

// SetCircuitBreaker 允许外部注入自定义熔断器配置（测试 / 特殊配置）。
func (g *Gateway) SetCircuitBreaker(breakers *CircuitBreakerRegistry) {
	if breakers != nil {
		g.breakers = breakers
	}
}

// Providers 返回当前可用 Provider 列表（调试/前端展示）。
func (g *Gateway) Providers() []Provider {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Provider, len(g.providers))
	copy(out, g.providers)
	return out
}

// SetProviders 在运行期替换 provider 列表（热更新）。
//
// 为什么需要它：provider 是用户在 UI 里配置的，而原实现只在进程启动时读一次
// 数据库 —— 用户配好 key、建好工作流、点运行，拿到的仍然是 Mock 兜底的输出
// （带 [mock-...] 前缀），除非重启服务。这类"改了配置没反应、还没有任何报错"
// 的问题最难排查，所以这里把列表变成可替换的，并由 provider 的增删改触发刷新。
func (g *Gateway) SetProviders(providers []Provider) {
	g.mu.Lock()
	g.providers = providers
	g.mu.Unlock()
}

// snapshot 返回当前 provider 列表的一份拷贝，供一次调用内的故障转移循环使用。
// 循环内必须用同一份快照：中途被 SetProviders 换掉会让"试到第几个"变得不可预期。
func (g *Gateway) snapshot() []Provider {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.providers
}

// Name 实现 Provider 接口，让 Gateway 可以直接作为 Provider 使用。
func (g *Gateway) Name() string { return "gateway" }

// Chat 带故障转移：依次尝试每个 provider，全部失败返回聚合错误。
// 熔断器：处于 Open 状态的 provider 会被跳过，避免浪费超时时间。
func (g *Gateway) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	providers := g.snapshot() // 一次调用用同一份快照，避免中途被热更新换掉
	if len(providers) == 0 {
		return nil, errors.New("no llm provider configured")
	}
	var lastErr error
	skipped := 0
	skippedNames := make([]string, 0, len(providers))
	for i, p := range providers {
		name := p.Name()
		// 熔断跳过：Open 状态的 provider 直接跳过
		if !g.breakers.Allow(name) {
			skipped++
			skippedNames = append(skippedNames, name)
			g.logger.Debug("llm provider skipped (circuit breaker open)",
				"provider", name, "model", req.Model)
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, g.attemptTimeout)
		resp, err := p.Chat(attemptCtx, req)
		cancel()
		if err == nil {
			g.breakers.RecordSuccess(name)
			if g.onUsage != nil {
				g.onUsage(&Usage{
					Provider: resp.Provider, Model: resp.Model,
					InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens,
					LatencyMS: resp.LatencyMS,
				})
			}
			if i > 0 || skipped > 0 {
				g.logger.Info("llm fallback succeeded",
					"provider", resp.Provider, "model", req.Model,
					"after_attempts", i+1-skipped, "skipped", skipped)
			}
			return resp, nil
		}
		lastErr = err
		// context 取消/超时类错误不计入熔断（可能是调用方主动中止，不是 provider 故障）
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			g.breakers.RecordFailure(name)
		}
		if g.onUsage != nil {
			g.onUsage(&Usage{Provider: name, Model: req.Model, Error: err.Error()})
		}
		g.logger.Warn("llm attempt failed, trying fallback",
			"provider", name, "model", req.Model, "attempt", i+1, "error", err)
	}
	return nil, allProvidersFailedError("all llm providers failed", skippedNames, lastErr)
}

// Stream 带故障转移的流式输出。
//
// 策略：
//   - 每个 provider 先等待"首 token"，超时或流直接结束且毫无内容 → 换下一个；
//   - 一旦拿到内容就视为已提交，之后原样转发，中途错误通过 Chunk.Err 上抛，
//     由节点层按重试策略处理（不在中途切换，避免输出拼接出错）；
//   - 流式期间不设总时长上限，只受调用方 ctx 约束，长回答不会被网关截断。
//   - 熔断器：Open 状态的 provider 会被直接跳过。
func (g *Gateway) Stream(ctx context.Context, req ChatRequest) (<-chan Chunk, error) {
	providers := g.snapshot() // 同上：一次调用用同一份快照
	if len(providers) == 0 {
		return nil, errors.New("no llm provider configured")
	}
	var lastErr error
	skipped := 0
	skippedNames := make([]string, 0, len(providers))
	for i, p := range providers {
		name := p.Name()
		// 熔断跳过：Open 状态的 provider 直接跳过
		if !g.breakers.Allow(name) {
			skipped++
			skippedNames = append(skippedNames, name)
			g.logger.Debug("llm stream provider skipped (circuit breaker open)",
				"provider", name, "model", req.Model)
			continue
		}
		// 用 WithCancel 而非 WithTimeout：流的生命周期交给调用方控制，
		// 网关只保证"首 token 不无限等待"。
		attemptCtx, cancel := context.WithCancel(ctx)
		ch, err := p.Stream(attemptCtx, req)
		if err != nil {
			cancel()
			lastErr = err
			// context 取消/超时不计入熔断
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				g.breakers.RecordFailure(name)
			}
			g.logger.Warn("llm stream attempt failed, trying fallback",
				"provider", name, "model", req.Model, "attempt", i+1, "error", err)
			continue
		}

		buffered, ok := g.awaitFirstToken(attemptCtx, ch)
		if !ok {
			cancel()
			lastErr = fmt.Errorf("provider %s produced no content before deadline", name)
			// 首 token 超时 / 无内容也算失败
			if ctx.Err() == nil {
				g.breakers.RecordFailure(name)
			}
			g.logger.Warn("llm stream produced nothing, trying fallback",
				"provider", name, "model", req.Model, "attempt", i+1)
			continue
		}
		// 拿到了内容 → 记录成功
		g.breakers.RecordSuccess(name)
		if i > 0 || skipped > 0 {
			g.logger.Info("llm stream fallback succeeded", "provider", name, "model", req.Model,
				"skipped", skipped)
		}

		out := make(chan Chunk, 64)
		go func(buf []Chunk, src <-chan Chunk, done context.CancelFunc) {
			defer close(out)
			defer done()
			for _, c := range buf {
				if !forward(ctx, out, c) {
					return
				}
			}
			for c := range src {
				if !forward(ctx, out, c) {
					return
				}
			}
		}(buffered, ch, cancel)
		return out, nil
	}
	return nil, allProvidersFailedError("all llm providers failed to stream", skippedNames, lastErr)
}

// awaitFirstToken 缓冲直到拿到第一个非空内容块，或判定该 provider 无产出。
// 返回 false 表示应当切换到下一个 provider。
// 之所以要缓冲：OpenAI 兼容协议会先发一个 content 为空的 role 块，
// 只判断"第一个块"会误判成失败而白白触发 fallback。
func (g *Gateway) awaitFirstToken(ctx context.Context, ch <-chan Chunk) ([]Chunk, bool) {
	timeout := g.firstTokenTimeout
	if timeout <= 0 {
		timeout = defaultFirstTokenTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var buf []Chunk
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				// 流直接结束：只要收到过内容就算成功
				return buf, len(buf) > 0 && hasContent(buf)
			}
			buf = append(buf, c)
			if c.Content != "" {
				return buf, true
			}
			if c.Done {
				// 结束块：没有内容就是失败
				return buf, hasContent(buf)
			}
		case <-timer.C:
			return buf, hasContent(buf)
		case <-ctx.Done():
			return buf, hasContent(buf)
		}
	}
}

func hasContent(chunks []Chunk) bool {
	for _, c := range chunks {
		if c.Content != "" {
			return true
		}
	}
	return false
}

// forward 非阻塞转发一个 chunk，调用方 ctx 结束时停止。
func forward(ctx context.Context, out chan<- Chunk, c Chunk) bool {
	select {
	case out <- c:
		return !c.Done
	case <-ctx.Done():
		return false
	}
}

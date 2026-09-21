package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// UsageFunc 在每个成功的模型调用后回调（用于写 usage_records 与指标）。
type UsageFunc func(u *Usage)

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
type Gateway struct {
	providers         []Provider // 按优先级排序（已启用）
	logger            *slog.Logger
	attemptTimeout    time.Duration
	firstTokenTimeout time.Duration // 等待首个 token 的上限，超时即切换 provider
	onUsage           UsageFunc
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
	}
}

// Providers 返回当前可用 Provider 列表（调试/前端展示）。
func (g *Gateway) Providers() []Provider { return g.providers }

// Chat 带故障转移：依次尝试每个 provider，全部失败返回聚合错误。
func (g *Gateway) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if len(g.providers) == 0 {
		return nil, errors.New("no llm provider configured")
	}
	var lastErr error
	for i, p := range g.providers {
		attemptCtx, cancel := context.WithTimeout(ctx, g.attemptTimeout)
		resp, err := p.Chat(attemptCtx, req)
		cancel()
		if err == nil {
			if g.onUsage != nil {
				g.onUsage(&Usage{
					Provider: resp.Provider, Model: resp.Model,
					InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens,
					LatencyMS: resp.LatencyMS,
				})
			}
			if i > 0 {
				g.logger.Info("llm fallback succeeded",
					"provider", resp.Provider, "model", req.Model, "after_attempts", i+1)
			}
			return resp, nil
		}
		lastErr = err
		if g.onUsage != nil {
			g.onUsage(&Usage{Provider: p.Name(), Model: req.Model, Error: err.Error()})
		}
		g.logger.Warn("llm attempt failed, trying fallback",
			"provider", p.Name(), "model", req.Model, "attempt", i+1, "error", err)
	}
	return nil, fmt.Errorf("all llm providers failed: %w", lastErr)
}

// Stream 带故障转移的流式输出。
//
// 策略：
//   - 每个 provider 先等待"首 token"，超时或流直接结束且毫无内容 → 换下一个；
//   - 一旦拿到内容就视为已提交，之后原样转发，中途错误通过 Chunk.Err 上抛，
//     由节点层按重试策略处理（不在中途切换，避免输出拼接出错）；
//   - 流式期间不设总时长上限，只受调用方 ctx 约束，长回答不会被网关截断。
func (g *Gateway) Stream(ctx context.Context, req ChatRequest) (<-chan Chunk, error) {
	if len(g.providers) == 0 {
		return nil, errors.New("no llm provider configured")
	}
	var lastErr error
	for i, p := range g.providers {
		// 用 WithCancel 而非 WithTimeout：流的生命周期交给调用方控制，
		// 网关只保证"首 token 不无限等待"。
		attemptCtx, cancel := context.WithCancel(ctx)
		ch, err := p.Stream(attemptCtx, req)
		if err != nil {
			cancel()
			lastErr = err
			g.logger.Warn("llm stream attempt failed, trying fallback",
				"provider", p.Name(), "model", req.Model, "attempt", i+1, "error", err)
			continue
		}

		buffered, ok := g.awaitFirstToken(attemptCtx, ch)
		if !ok {
			cancel()
			lastErr = fmt.Errorf("provider %s produced no content before deadline", p.Name())
			g.logger.Warn("llm stream produced nothing, trying fallback",
				"provider", p.Name(), "model", req.Model, "attempt", i+1)
			continue
		}
		if i > 0 {
			g.logger.Info("llm stream fallback succeeded", "provider", p.Name(), "model", req.Model)
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
	return nil, fmt.Errorf("all llm providers failed to stream: %w", lastErr)
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

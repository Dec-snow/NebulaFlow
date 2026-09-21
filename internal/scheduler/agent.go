package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/tool"
	"github.com/hoarfrost/nebulaflow/internal/worker"
)

// 本文件实现 P2-11：让 LLM 自主决定调用哪个工具。
//
// 与"工具节点"的区别（这是整件事的意义所在）：
//   - 工具节点是**用户手动编排**的：画布上写死"这一步调 calculator"，
//     输入是什么、调用几次、要不要调，全部由人在设计期决定；
//   - Agent 节点是**模型自主决定**的：人只给出可用工具清单与目标，
//     调哪个、传什么参数、一轮不够再来一轮、某个工具报错了换一个，
//     全部由模型在运行期决定。
//
// 实现上的关键取舍：
//
//  1. **非流式**。OpenAI 的 tool_calls 在 SSE 里是一串增量分片，要还原必须
//     维护按 index 聚合的状态机；而"这一轮到底有没有工具调用"必须等本轮结束
//     才知道，所以流式在这个场景下并不能让用户更早看到最终答案。
//     最终回答仍然会以 token 事件推给前端，只是它一次到齐而非逐字。
//  2. **工具失败不致命**。工具报错会作为"错误：…"的文本回灌给模型，
//     由模型决定换个工具或换个参数。把一次失败的调用升级成节点失败，
//     会让 Agent 在第一次试错时就死掉 —— 而试错正是 Agent 的工作方式。
//     唯一例外是任务被取消：那必须立刻中断，否则"取消"只是改了个数据库状态。
//  3. **轮数硬上限**。模型有可能反复调用同一个工具，没有上限就是无限循环 +
//     无限 token 消耗。撞上上限算节点失败（而不是返回半成品答案），
//     因为一个没给出结论的 Agent 输出被当成"成功"会让下游拿到空结果继续跑。
const (
	defaultAgentMaxRounds   = 4
	hardAgentMaxRounds      = 16
	defaultAgentToolTimeout = 20 * time.Second
	// maxToolResultInContext 限制回灌给模型的单次工具结果长度。
	// http_request 一次能拿回 4000 字节，多轮累积会迅速顶满上下文窗口，
	// 而超出部分模型也基本用不上。
	maxToolResultInContext = 4000
)

// agentOptions 是 Agent 模式的生效配置（由 NodeConfig.Extra 解析而来）。
type agentOptions struct {
	Enabled     bool
	Tools       []string // 允许模型调用的工具名；空 = 全部已注册工具
	MaxRounds   int
	ToolTimeout time.Duration
}

// parseAgentOptions 从节点配置解析 Agent 模式开关。
//
// 两种开启方式，任一满足即开启：
//   - extra.agentic = true
//   - extra.tools = ["calculator", ...]（声明了工具清单，显然是想让模型用它们）
//
// 第二个返回值是需要打给运维看的告警（如轮数被夹到上限、配置类型写错）。
// 不做静默修正：配置写了 max_tool_rounds: 100 却只跑了 16 轮，
// 如果没有任何提示，排查时只能靠猜。
func parseAgentOptions(cfg model.NodeConfig) (agentOptions, []string) {
	opt := agentOptions{MaxRounds: defaultAgentMaxRounds, ToolTimeout: defaultAgentToolTimeout}
	if len(cfg.Extra) == 0 {
		return opt, nil
	}
	var warns []string

	if v, ok := cfg.Extra["agentic"]; ok {
		switch t := v.(type) {
		case bool:
			opt.Enabled = t
		case string:
			// JSON 配置里写成字符串 "true" 很常见，宽容处理
			if b, err := strconv.ParseBool(t); err == nil {
				opt.Enabled = b
			} else {
				warns = append(warns, fmt.Sprintf("agentic 取值 %q 无法解析为布尔，已忽略", t))
			}
		default:
			warns = append(warns, fmt.Sprintf("agentic 类型为 %T，期望 bool，已忽略", v))
		}
	}

	if v, ok := cfg.Extra["tools"]; ok {
		names, err := stringList(v)
		if err != nil {
			warns = append(warns, "tools: "+err.Error())
		} else if len(names) > 0 {
			opt.Tools = names
			opt.Enabled = true // 声明了工具清单即视为开启
		}
	}

	if n, ok, err := extraInt(cfg.Extra, "max_tool_rounds"); err != nil {
		warns = append(warns, "max_tool_rounds: "+err.Error())
	} else if ok {
		switch {
		case n < 1:
			warns = append(warns, fmt.Sprintf("max_tool_rounds=%d 无意义，已按 1 处理", n))
			n = 1
		case n > hardAgentMaxRounds:
			warns = append(warns, fmt.Sprintf("max_tool_rounds=%d 超过硬上限 %d，已夹到 %d", n, hardAgentMaxRounds, hardAgentMaxRounds))
			n = hardAgentMaxRounds
		}
		opt.MaxRounds = n
	}

	if n, ok, err := extraInt(cfg.Extra, "tool_timeout_sec"); err != nil {
		warns = append(warns, "tool_timeout_sec: "+err.Error())
	} else if ok {
		if n <= 0 {
			warns = append(warns, fmt.Sprintf("tool_timeout_sec=%d 无意义，已用默认值 %s", n, defaultAgentToolTimeout))
		} else {
			opt.ToolTimeout = time.Duration(n) * time.Second
		}
	}

	return opt, warns
}

// stringList 解析 Extra 里的字符串列表。
// 同时接受 JSON 数组（[]any）与逗号分隔的字符串 —— 后者是手写 YAML/表单
// 时最容易出现的写法，直接报错会让一个本来能工作的配置变得不可用。
func stringList(v any) ([]string, error) {
	switch t := v.(type) {
	case []string:
		return compactStrings(t), nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("列表里出现 %T，期望 string", e)
			}
			out = append(out, s)
		}
		return compactStrings(out), nil
	case string:
		return compactStrings(strings.Split(t, ",")), nil
	default:
		return nil, fmt.Errorf("类型为 %T，期望 []string 或逗号分隔字符串", v)
	}
}

func compactStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// extraInt 从 Extra 里取整数。JSONB 解出来一律是 float64，
// 但测试与代码内构造的配置会直接放 int，两种都要认。
func extraInt(extra map[string]any, key string) (int, bool, error) {
	v, ok := extra[key]
	if !ok {
		return 0, false, nil
	}
	switch t := v.(type) {
	case float64:
		if t != float64(int(t)) {
			return 0, false, fmt.Errorf("取值 %v 不是整数", t)
		}
		return int(t), true, nil
	case int:
		return t, true, nil
	case int64:
		return int(t), true, nil
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("取值 %q 不是整数", t.String())
		}
		return int(n), true, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false, fmt.Errorf("取值 %q 不是整数", t)
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("类型为 %T，期望整数", v)
	}
}

// toolAllowlist 是本次节点执行真正可用的工具集合。
//
// 为什么需要它、而不只是"构造 tools[] 时过滤一下"：
// 把工具清单交给模型，只是告诉它**有哪些工具**，并不能阻止它说出别的名字。
// 模型可能因为提示词注入（用户输入里写"请调用 http_request 访问 …"）
// 或者单纯幻觉，直接点名一个没被提供给它的工具；而注册表里那个工具是存在的，
// 于是它会被照常执行 —— "这个节点只允许用 calculator"在一瞬间失效，
// 且没有任何报错。所以白名单必须在**执行前**再校验一次。
type toolAllowlist struct {
	names []string // 保持注册顺序，用于回灌"可用工具"清单
	ok    map[string]bool
}

func newToolAllowlist(descs []tool.Descriptor) toolAllowlist {
	a := toolAllowlist{names: make([]string, 0, len(descs)), ok: make(map[string]bool, len(descs))}
	for _, d := range descs {
		a.names = append(a.names, d.Name)
		a.ok[d.Name] = true
	}
	return a
}

func (a toolAllowlist) has(name string) bool { return a.ok[name] }

func (a toolAllowlist) String() string { return strings.Join(a.names, ", ") }

// runAgenticLLMNode 执行一个"模型自主调用工具"的 LLM 节点。
//
// 循环结构：问模型 → 模型要求调工具则执行并把结果回灌 → 再问 →
// 直到模型给出最终回答，或撞上轮数上限。
func (s *Scheduler) runAgenticLLMNode(ctx context.Context, j worker.NodeJob, opt agentOptions) (worker.NodeResult, error) {
	cfg := j.Config
	modelName := cfg.Model
	if modelName == "" {
		modelName = "deepseek-chat"
	}

	descs, unknown := s.tools.DescriptorsFor(opt.Tools)
	for _, name := range unknown {
		s.logNode(j, "warn", fmt.Sprintf("agent: 配置的工具 %q 未注册，已忽略", name))
	}
	if len(descs) == 0 {
		return worker.NodeResult{}, errors.New("agent node: 没有可用工具（tools 里的名字都没注册？）")
	}
	specs := make([]llm.ToolSpec, 0, len(descs))
	for _, d := range descs {
		specs = append(specs, llm.ToolSpec{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	allow := newToolAllowlist(descs)
	s.logNode(j, "info", fmt.Sprintf("agent: 开启工具调用，可用工具 [%s]，轮数上限 %d", allow, opt.MaxRounds))

	messages := buildLLMMessages(cfg.System, cfg.Prompt, j.Input)

	maxRetry := cfg.MaxRetry
	if maxRetry <= 0 {
		maxRetry = s.defaultMaxRetry
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}

	var (
		inTokens  int
		outTokens int
	)

	for round := 1; round <= opt.MaxRounds; round++ {
		// 每轮一个 agent.round span —— 这就是"卡在第几轮"的答案本身。
		//
		// P2-11 之后 Agent 节点内部可能有 N 轮对话 + M 次工具调用，而当时只有聚合指标
		// （"平均轮数 2.3"），说不出**某一个**慢任务停在哪一轮。
		// 把轮次拆成独立 span 之后，"慢在第几轮、那一轮里是模型慢还是工具慢"一眼可见。
		roundCtx, roundSpan := tracing.StartInternal(ctx, tracing.SpanAgentRound,
			tracing.KV("nebulaflow.agent.round", round),
			tracing.KV("nebulaflow.agent.max_rounds", opt.MaxRounds),
			tracing.KV("nebulaflow.task.id", j.TaskID),
			tracing.KV("nebulaflow.node.key", j.NodeKey),
		)
		resp, err := s.chatWithRetry(roundCtx, j, messages, specs, modelName, maxRetry, timeout)
		if err != nil {
			tracing.Finish(roundSpan, err)
			s.observeAgentRounds("error", round)
			return worker.NodeResult{}, fmt.Errorf("agent node round %d: %w", round, err)
		}
		inTokens += resp.InputTokens
		outTokens += resp.OutputTokens
		// 每轮单独落一条 usage：Agent 的成本是各轮之和，只记一条总数
		// 会让"哪一轮烧的钱最多"无从查起，而轮数恰恰是 Agent 成本失控的主因。
		s.recordUsage(j, resp.Provider, resp.Model, resp.InputTokens, resp.OutputTokens)

		if len(resp.ToolCalls) == 0 {
			// 这里不必再判空内容：chatWithRetry 已经把"既无内容又无工具调用"
			// 的轮次判为失败并重试过了，走到这里 Content 一定非空。
			// 留一个不可达的分支只会让后来人以为它真的会触发。
			roundSpan.SetAttributes(tracing.KV("nebulaflow.agent.tool_calls", 0))
			tracing.Finish(roundSpan, nil)
			s.observeAgentRounds("completed", round)
			// 非流式拿到整段回答，仍以 token 事件推给前端，
			// 否则前端只能等节点完成才显示内容，看起来像卡住了。
			s.pushTokenEvent(j, resp.Content)
			return worker.NodeResult{
				Output: resp.Content, Provider: resp.Provider, Model: resp.Model,
				InputTokens: inTokens, OutputTokens: outTokens,
			}, nil
		}

		// 模型本轮的文字（若有）是它给用户看的"过程说明"，不是最终答案。
		// 用日志而不是 token 事件推出去：token 事件会被前端累积成最终输出，
		// 把中间过程混进去会污染答案。
		if strings.TrimSpace(resp.Content) != "" {
			s.logNode(j, "info", "agent: "+truncateForSSE(resp.Content, 500))
		}

		// 回灌 assistant 的 tool_calls。顺序与内容必须原样保留：
		// 下一轮模型要靠 tool_call_id 把结果对应回自己发起的调用。
		messages = append(messages, llm.Message{
			Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
		})

		roundSpan.SetAttributes(tracing.KV("nebulaflow.agent.tool_calls", len(resp.ToolCalls)))
		for _, call := range resp.ToolCalls {
			out, err := s.runAgentTool(roundCtx, j, opt, call, allow)
			if err != nil {
				// 只有"必须中断循环"的错误会走到这里（任务被取消）
				tracing.Finish(roundSpan, err)
				s.observeAgentRounds("cancelled", round)
				return worker.NodeResult{}, err
			}
			messages = append(messages, llm.Message{
				Role: llm.RoleTool, ToolCallID: call.ID, Content: out,
			})
		}
		tracing.Finish(roundSpan, nil)
	}

	s.observeAgentRounds("max_rounds", opt.MaxRounds)
	return worker.NodeResult{}, fmt.Errorf(
		"agent node: 达到工具调用轮数上限 %d 仍未给出最终回答（可用 extra.max_tool_rounds 调整，上限 %d）",
		opt.MaxRounds, hardAgentMaxRounds)
}

// runAgentTool 执行一次工具调用，返回要回灌给模型的文本。
//
// 工具自身的失败**不返回 error**：它会以"错误：…"的文本回灌，
// 由模型决定换工具还是换参数 —— 这正是 Agent 该有的行为。
// 返回的 error 只表示必须中断整个循环（任务被取消）。
func (s *Scheduler) runAgentTool(ctx context.Context, j worker.NodeJob, opt agentOptions,
	call llm.ToolCall, allow toolAllowlist) (out string, retErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// 一次工具调用一个 span。
	//
	// 记的是参数**长度**而不是参数原文：span 属性会随 trace 一起被查询接口返回，
	// 而工具参数里可能带用户数据。要排查内容看日志，不要把用户数据复制到可观测性系统里
	// （那是数据泄露最常见的入口之一）。
	ctx, span := tracing.StartInternal(ctx, tracing.SpanAgentTool,
		tracing.KV("nebulaflow.tool.name", call.Name),
		tracing.KV("nebulaflow.tool.caller", "agent"),
		tracing.KV("nebulaflow.tool.args_length", len(call.Arguments)),
		tracing.KV("nebulaflow.task.id", j.TaskID),
		tracing.KV("nebulaflow.node.key", j.NodeKey),
	)
	var (
		callErr error
		skip    bool
		result  = observability.ToolResultSuccess
	)
	defer func() {
		span.SetAttributes(tracing.KV("nebulaflow.tool.result", result))
		switch {
		case callErr != nil:
			tracing.Finish(span, callErr)
		case skip:
			// 被拒绝的调用也必须有 span：它是一次"发生了但没成功"的调用，
			// 直接从 trace 里消失会让面板上的调用数偏少，比完全看不到更难查。
			tracing.Finish(span, fmt.Errorf("tool %q not allowed", call.Name))
		default:
			tracing.Finish(span, retErr)
		}
	}()

	s.publishToolCall(j, call)

	start := time.Now()
	if !allow.has(call.Name) {
		// 未授权：**根本不查注册表**，直接按"工具不存在"回灌。
		// 注册表里可能确实有这个工具（比如 http_request），但这个节点没被允许用它；
		// 查一次再判断"存在但不许用"，只会多一处可能写错的地方。
		skip = true
		_, inRegistry := s.tools.Get(call.Name)
		if inRegistry {
			// 这条日志是安全信号：模型点名了一个存在但未授权的工具。
			// 通常意味着提示词注入或模型幻觉，值得单独标出来。
			s.logNode(j, "warn", fmt.Sprintf(
				"agent: 模型请求了未授权工具 %q（该工具已注册但不在本节点的 tools 清单里），已拒绝执行",
				call.Name))
		}
	}
	if !skip {
		toolCtx, cancel := context.WithTimeout(ctx, opt.ToolTimeout)
		out, callErr = s.tools.CallWithArguments(toolCtx, call.Name, call.Arguments)
		cancel()
	}
	dur := time.Since(start)

	switch {
	case skip:
		// 这里覆盖两种情况：模型点名了未授权的工具，或点名了一个根本没注册的工具。
		// 两者都归到 unknown_tool，不必区分 —— 从模型的角度看结果一样
		// （"这个工具我调不了"），而对运维来说真正需要区分的那件事
		// （工具存在但未授权 ⇒ 疑似提示词注入）已经在上面的日志里单独标出来了。
		result = observability.ToolResultUnknownTool
		out = fmt.Sprintf("错误：工具 %q 不可用。可用工具：%s", call.Name, allow)
	case callErr == nil:
		// 成功：原样回灌
	default:
		result = observability.ToolResultToolError
		out = "错误：" + callErr.Error()
	}
	// 注意这里**没有** tool.ErrUnknownTool 的分支：allow 是从注册表推导出来的子集，
	// allow.has(name) 为真就意味着该工具一定注册过，CallWithArguments 不可能
	// 再返回"未注册"。留一个不可达的分支只会让人误以为它真的会触发。
	out = truncateRunes(out, maxToolResultInContext)

	// 指标与事件对每一次调用都要记，包括被取消的那次 ——
	// 否则"调了工具但一直没结果"在面板上表现为调用数偏少，
	// 反而比完全看不到更难查。
	s.observeToolCall(call.Name, result, dur)
	s.publishToolResult(j, call, out, result, dur, callErr)

	// 任务级取消优先于工具错误：工具超时（toolCtx 到期）与任务取消（ctx 到期）
	// 都会让 CallWithArguments 返回 context 错误，但前者该回灌给模型、后者必须中断。
	// 判据是**父 ctx** 的状态，而不是返回的 error 类型。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	return out, nil
}

// chatWithRetry 是 Agent 路径上的单轮模型调用：带指数退避重试。
//
// 与流式路径共用同一套重试语义（退避上限 30s、失败推 fallback 提示），
// 但判断"这轮算不算成功"多了一条：**有工具调用也算成功**。
// 只判断 Content 非空的话，一个纯工具调用轮次会被当成空响应反复重试，
// 同一个工具调用被白白执行三次。
func (s *Scheduler) chatWithRetry(ctx context.Context, j worker.NodeJob, messages []llm.Message,
	specs []llm.ToolSpec, modelName string, maxRetry int, timeout time.Duration) (*llm.ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if attempt > 0 {
			backoff := backoffFor(attempt)
			s.logNode(j, "warn", fmt.Sprintf("agent: retrying in %s (attempt %d/%d)", backoff, attempt, maxRetry))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		nodeCtx, cancel := context.WithTimeout(ctx, timeout)
		// **每次尝试**一个 llm.chat span。Agent 一轮对话可能有多次重试，
		// 而"某一轮为什么花了 20 秒"的答案通常就藏在这里。
		llmCtx, llmSpan := tracing.StartInternal(nodeCtx, tracing.SpanLLMChat,
			tracing.KV("gen_ai.request.model", modelName),
			tracing.KV("nebulaflow.llm.attempt", attempt),
			tracing.KV("nebulaflow.llm.tools", len(specs)),
			tracing.KV("nebulaflow.task.id", j.TaskID),
			tracing.KV("nebulaflow.node.key", j.NodeKey),
		)
		resp, err := s.llm.Chat(llmCtx, llm.ChatRequest{
			Model: modelName, Messages: messages, Tools: specs, MaxTokens: 2048, Temperature: 0.4,
		})
		cancel()
		if err != nil {
			tracing.Finish(llmSpan, err)
			lastErr = err
			s.noteFallback(j, err)
			continue
		}
		if strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0 {
			lastErr = errors.New("empty llm response")
			tracing.Finish(llmSpan, lastErr)
			s.noteFallback(j, lastErr)
			continue
		}
		llmSpan.SetAttributes(
			tracing.KV("gen_ai.system", resp.Provider),
			tracing.KV("gen_ai.response.model", resp.Model),
			tracing.KV("gen_ai.usage.input_tokens", resp.InputTokens),
			tracing.KV("gen_ai.usage.output_tokens", resp.OutputTokens),
			// 这一轮模型决定要调几个工具 —— 与 agent.round 上的同名属性互为印证。
			tracing.KV("nebulaflow.llm.tool_calls", len(resp.ToolCalls)),
		)
		tracing.Finish(llmSpan, nil)
		return resp, nil
	}
	if s.metrics != nil {
		s.metrics.ObserveLLM("llm", modelName, false, 0, 0, 0)
	}
	return nil, fmt.Errorf("llm call failed after %d attempts: %w", maxRetry+1, lastErr)
}

// ---------- 事件与指标 ----------

func (s *Scheduler) publishToolCall(j worker.NodeJob, call llm.ToolCall) {
	s.logNode(j, "info", fmt.Sprintf("agent: 调用工具 %s 参数 %s", call.Name, truncateForSSE(call.Arguments, 300)))
	ev := task.NewEvent(task.EventToolCall, j.TaskID)
	ev.NodeKey = j.NodeKey
	ev.NodeType = string(j.NodeType)
	ev.ToolName = call.Name
	ev.Content = truncateForSSE(call.Arguments, 500)
	ev.Message = "模型请求调用工具 " + call.Name
	s.hub.Publish(ev)
}

func (s *Scheduler) publishToolResult(j worker.NodeJob, call llm.ToolCall, out, result string,
	dur time.Duration, callErr error) {
	ev := task.NewEvent(task.EventToolResult, j.TaskID)
	ev.NodeKey = j.NodeKey
	ev.NodeType = string(j.NodeType)
	ev.ToolName = call.Name
	ev.Content = truncateForSSE(out, 500)
	ev.DurationMS = dur.Milliseconds()
	switch {
	case callErr == nil:
		ev.Message = fmt.Sprintf("工具 %s 执行成功（%dms）", call.Name, dur.Milliseconds())
		s.logNode(j, "info", ev.Message)
	case errors.Is(callErr, context.Canceled):
		// 区分"工具自己失败"与"任务被取消"：都写"执行失败"会让人以为
		// 是被调用的外部服务出了问题，从而去查错方向。
		ev.Message = fmt.Sprintf("工具 %s 已取消", call.Name)
		s.logNode(j, "warn", ev.Message)
	default:
		ev.Message = fmt.Sprintf("工具 %s 执行失败：%s", call.Name, callErr.Error())
		s.logNode(j, "warn", ev.Message)
	}
	s.hub.Publish(ev)
}

func (s *Scheduler) observeToolCall(name, result string, dur time.Duration) {
	if s.metrics != nil {
		s.metrics.ObserveToolCall(name, result, dur.Seconds())
	}
}

func (s *Scheduler) observeAgentRounds(result string, rounds int) {
	if s.metrics != nil {
		s.metrics.ObserveAgentRounds(result, rounds)
	}
}

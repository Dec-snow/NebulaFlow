package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/worker"
)

// maxPersistedInput 是落库的节点输入上限（避免 RAG 上下文撑爆 task_nodes）。
const maxPersistedInput = 16000

// ExecuteNode 实现 worker.Executor：按节点类型分发执行。
// 每个节点执行都带：状态落库、SSE 事件、日志、重试与超时。
func (s *Scheduler) ExecuteNode(ctx context.Context, j worker.NodeJob) (worker.NodeResult, error) {
	start := time.Now()
	// 节点 span 的父 span 是 worker 侧的 task.execute（j.Ctx 由 executeTask 派生），
	// 因此「HTTP 请求 → 任务 → 节点」是一条连续的链，而不是三段各自为政的记录。
	ctx, span := tracing.StartInternal(ctx, tracing.SpanNodeExecute,
		tracing.KV("nebulaflow.task.id", j.TaskID),
		tracing.KV("nebulaflow.node.key", j.NodeKey),
		tracing.KV("nebulaflow.node.type", string(j.NodeType)),
	)
	var res worker.NodeResult
	var err error
	// 闭包捕获的是变量本身，因此这里读到的是函数退出时的最终 err（不是 defer 注册时的值）。
	defer func() { tracing.Finish(span, err) }()

	s.publishNode(ctx, j, model.NodeRunning, "", "", start, 0, nil)
	s.logNode(j, "info", "node started")

	switch j.NodeType {
	case model.NodeInput:
		res = worker.NodeResult{Output: j.Input}
	case model.NodeLLM:
		res, err = s.runLLMNode(ctx, j)
	case model.NodeRAG:
		res, err = s.runRAGNode(ctx, j)
	case model.NodeTool:
		res, err = s.runToolNode(ctx, j)
	case model.NodeOutput:
		res = worker.NodeResult{Output: j.Input}
	default:
		err = fmt.Errorf("unknown node type: %s", j.NodeType)
	}

	res.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		// 区分"被取消"与"真失败"：context.Canceled 来自用户取消任务，
		// DeadlineExceeded 才是超时。两者混为一谈，取消后的节点会显示成失败，
		// 用户会以为自己的流程有 bug。
		status := model.NodeFailed
		if errors.Is(err, context.Canceled) {
			status = model.NodeCancelled
			err = fmt.Errorf("节点已取消")
		}
		span.SetAttributes(tracing.KV("nebulaflow.node.status", string(status)))
		s.publishNode(ctx, j, status, res.Output, err.Error(), start, res.DurationMS, &res)
		s.logNode(j, "error", err.Error())
		return res, err
	}
	span.SetAttributes(
		tracing.KV("nebulaflow.node.status", string(model.NodeSucceeded)),
		tracing.KV("nebulaflow.node.duration_ms", res.DurationMS),
	)
	s.publishNode(ctx, j, model.NodeSucceeded, res.Output, "", start, res.DurationMS, &res)
	s.logNode(j, "info", fmt.Sprintf("node completed in %dms", res.DurationMS))
	return res, nil
}

// ---------- LLM 节点 ----------

// runLLMNode 组装 prompt → LLM 调用（带重试与故障转移）→ 流式 token 推送。
//
// 若节点开启了 Agent 模式（extra.agentic / extra.tools），则改走
// runAgenticLLMNode：模型自主决定调用工具，多轮对话直到给出最终回答。
// 分发放在最前面、且默认关闭，保证未开启时这条路径的行为与 P0-0 压测时
// 完全一致 —— 否则"加了个新功能"会静默改变历史基线数字的含义。
func (s *Scheduler) runLLMNode(ctx context.Context, j worker.NodeJob) (worker.NodeResult, error) {
	cfg := j.Config
	if opt, warns := parseAgentOptions(cfg); opt.Enabled {
		for _, w := range warns {
			s.logNode(j, "warn", "agent 配置: "+w)
		}
		return s.runAgenticLLMNode(ctx, j, opt)
	}

	modelName := cfg.Model
	if modelName == "" {
		modelName = "deepseek-chat"
	}
	messages := buildLLMMessages(cfg.System, cfg.Prompt, j.Input)

	// 流式标志：默认开启，可用配置关闭（离线/非交互场景）
	stream := true
	if v, ok := cfg.Extra["stream"].(bool); ok {
		stream = v
	}

	maxRetry := cfg.MaxRetry
	if maxRetry <= 0 {
		maxRetry = s.defaultMaxRetry
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if attempt > 0 {
			// 指数退避：1s / 2s / 4s …… 上限 30s
			backoff := backoffFor(attempt)
			s.logNode(j, "warn", fmt.Sprintf("retrying in %s (attempt %d/%d)", backoff, attempt, maxRetry))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return worker.NodeResult{}, ctx.Err()
			}
		}
		nodeCtx, cancel := context.WithTimeout(ctx, timeout)
		// **每次尝试**一个 llm.chat span，而不是整个重试循环一个。
		// 重试恰恰是 LLM 节点最常见的"慢"的来源：把三次尝试压成一个 span，
		// 就完全看不出"哪一次慢、哪一次失败、退避等了多久"。
		llmCtx, llmSpan := tracing.StartInternal(nodeCtx, tracing.SpanLLMChat,
			tracing.KV("gen_ai.request.model", modelName),
			tracing.KV("nebulaflow.llm.stream", stream),
			tracing.KV("nebulaflow.llm.attempt", attempt),
			tracing.KV("nebulaflow.task.id", j.TaskID),
			tracing.KV("nebulaflow.node.key", j.NodeKey),
		)

		if stream {
			ch, err := s.llm.Stream(llmCtx, llm.ChatRequest{
				Model: modelName, Messages: messages, MaxTokens: 2048, Temperature: 0.4,
			})
			if err != nil {
				tracing.Finish(llmSpan, err)
				cancel()
				lastErr = err
				s.noteFallback(j, err)
				continue
			}
			var sb strings.Builder
			tokens := 0
			var streamErr error
			for chunk := range ch {
				if chunk.Err != nil {
					// 流中途失败：已产出的内容丢弃，整块重试（换 provider 由 Gateway 负责）
					streamErr = chunk.Err
					break
				}
				if chunk.Done {
					break
				}
				if chunk.Content != "" {
					sb.WriteString(chunk.Content)
					tokens++
					s.pushTokenEvent(j, chunk.Content)
				}
			}
			cancel()
			if streamErr != nil {
				tracing.Finish(llmSpan, streamErr)
				lastErr = streamErr
				s.noteFallback(j, streamErr)
				continue
			}
			content := sb.String()
			// 空响应一律视为失败并重试。原实现写成
			// `strings.TrimSpace(content)=="" && lastErr==nil`，
			// 只要上一次尝试留下过 lastErr，本次空响应就会被当成成功返回，
			// 于是下游拿到空输入继续跑，最终输出一片空白。
			if strings.TrimSpace(content) == "" {
				lastErr = errors.New("empty llm stream response")
				tracing.Finish(llmSpan, lastErr)
				s.noteFallback(j, lastErr)
				continue
			}
			inTokens := estimateTokens(messages)
			llmSpan.SetAttributes(
				tracing.KV("gen_ai.usage.input_tokens", inTokens),
				tracing.KV("gen_ai.usage.output_tokens", tokens),
			)
			tracing.Finish(llmSpan, nil)
			s.recordUsage(j, "llm", modelName, inTokens, tokens)
			return worker.NodeResult{
				Output: content, Provider: "llm", Model: modelName,
				InputTokens: inTokens, OutputTokens: tokens,
			}, nil
		}

		resp, err := s.llm.Chat(llmCtx, llm.ChatRequest{
			Model: modelName, Messages: messages, MaxTokens: 2048, Temperature: 0.4,
		})
		cancel()
		if err != nil {
			tracing.Finish(llmSpan, err)
			lastErr = err
			s.noteFallback(j, err)
			continue
		}
		if strings.TrimSpace(resp.Content) == "" {
			lastErr = errors.New("empty llm response")
			tracing.Finish(llmSpan, lastErr)
			s.noteFallback(j, lastErr)
			continue
		}
		// provider 是 Gateway 故障转移之后才知道的，只能在这里补属性。
		// 它决定了"这条链到底打到了哪个模型"，是排查供应商侧问题的第一手信息。
		llmSpan.SetAttributes(
			tracing.KV("gen_ai.system", resp.Provider),
			tracing.KV("gen_ai.response.model", resp.Model),
			tracing.KV("gen_ai.usage.input_tokens", resp.InputTokens),
			tracing.KV("gen_ai.usage.output_tokens", resp.OutputTokens),
		)
		tracing.Finish(llmSpan, nil)
		s.recordUsage(j, resp.Provider, resp.Model, resp.InputTokens, resp.OutputTokens)
		return worker.NodeResult{
			Output: resp.Content, Provider: resp.Provider, Model: resp.Model,
			InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens,
		}, nil
	}

	// 全部尝试失败：记一条失败指标，方便 Grafana 看到 provider 维度的错误率
	if s.metrics != nil {
		s.metrics.ObserveLLM("llm", modelName, false, 0, 0, 0)
	}
	return worker.NodeResult{}, fmt.Errorf("llm node failed after %d attempts: %w", maxRetry+1, lastErr)
}

// backoffFor 返回第 attempt 次重试前的退避时长（1s, 2s, 4s ... 上限 30s）。
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6 // 1<<5 = 32s，再做封顶
	}
	d := time.Duration(1<<(attempt-1)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func estimateTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(strings.Fields(m.Content))
	}
	return total
}

func buildLLMMessages(system, prompt, input string) []llm.Message {
	var msgs []llm.Message
	if system != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: system})
	}
	user := input
	if prompt != "" {
		user = prompt
		if input != "" {
			user = prompt + "\n\n用户输入：\n" + input
		}
	}
	if user == "" {
		user = "请根据你的能力作答。"
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: user})
	return msgs
}

// noteFallback 在 LLM 失败时向前端推送"切换备用模型"提示。
func (s *Scheduler) noteFallback(j worker.NodeJob, err error) {
	ev := task.NewEvent(task.EventFallback, j.TaskID)
	ev.NodeKey = j.NodeKey
	ev.Message = "模型调用失败（" + err.Error() + "），正在切换备用模型……"
	s.hub.Publish(ev)
}

// pushTokenEvent 推送 LLM 流式 token（浏览器逐字显示）。
func (s *Scheduler) pushTokenEvent(j worker.NodeJob, token string) {
	ev := task.NewEvent(task.EventToken, j.TaskID)
	ev.NodeKey = j.NodeKey
	ev.Content = token
	s.hub.Publish(ev)
}

// ---------- RAG 节点 ----------

func (s *Scheduler) runRAGNode(ctx context.Context, j worker.NodeJob) (worker.NodeResult, error) {
	kbID := j.Config.KnowledgeBaseID
	if kbID == 0 {
		return worker.NodeResult{}, errors.New("rag node: knowledge_base_id not configured")
	}
	ctx, span := tracing.StartInternal(ctx, tracing.SpanRAGRetrieve,
		tracing.KV("nebulaflow.task.id", j.TaskID),
		tracing.KV("nebulaflow.node.key", j.NodeKey),
		tracing.KV("nebulaflow.rag.kb_id", kbID),
		tracing.KV("nebulaflow.rag.top_k", 5),
	)
	// 检索完全下推：本函数不再加载知识库分块，只把 query 交给 rag.Service
	// 向量化后由存储层（pgvector HNSW）裁剪出 Top-K。见 P0-1。
	hits, err := s.rag.Retrieve(ctx, s.knowledge, kbID, j.Input, 5)
	if err != nil {
		tracing.Finish(span, err)
		return worker.NodeResult{}, fmt.Errorf("rag node: %w", err)
	}
	// 命中数是这条 span 最值得看的一个数：P0-1 修掉的正是
	// "多租户下静默返回不足 k 条"——只看耗时看不出召回率掉了。
	span.SetAttributes(tracing.KV("nebulaflow.rag.hits", len(hits)))
	tracing.Finish(span, nil)
	if len(hits) == 0 {
		return worker.NodeResult{
			Output: "未检索到相关内容。", Provider: "rag", Model: "kb-" + fmt.Sprint(kbID),
		}, nil
	}
	var sb strings.Builder
	sb.WriteString(ragBuildContext(hits))
	sb.WriteString("\n[检索命中] ")
	for i, h := range hits {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(fmt.Sprintf("doc#%d chunk#%d (%.3f)", h.DocumentID, h.ChunkIndex, h.Score))
	}
	return worker.NodeResult{
		Output: sb.String(), Provider: "rag", Model: "kb-" + fmt.Sprint(kbID),
	}, nil
}

// ragBuildContext 构造注入 LLM 的参考上下文。
func ragBuildContext(hits []rag.Hit) string {
	var sb strings.Builder
	sb.WriteString("【参考资料】\n")
	for i, h := range hits {
		sb.WriteString(fmt.Sprintf("[%d] %s\n", i+1, h.Content))
	}
	return sb.String()
}

// ---------- Tool 节点 ----------

func (s *Scheduler) runToolNode(ctx context.Context, j worker.NodeJob) (worker.NodeResult, error) {
	toolName := j.Config.Tool
	if toolName == "" {
		return worker.NodeResult{}, errors.New("tool node: tool not configured")
	}
	// 与 Agent 自主调用的工具共用一个 span 名，用 caller 区分来源。
	// 这样"哪个工具最慢"能把两种调用方式合并统计，而需要时又能拆开看。
	ctx, span := tracing.StartInternal(ctx, tracing.SpanToolCall,
		tracing.KV("nebulaflow.tool.name", toolName),
		tracing.KV("nebulaflow.tool.caller", "node"),
		tracing.KV("nebulaflow.task.id", j.TaskID),
		tracing.KV("nebulaflow.node.key", j.NodeKey),
	)
	t, ok := s.tools.Get(toolName)
	if !ok {
		err := fmt.Errorf("tool node: unknown tool %q", toolName)
		tracing.Finish(span, err)
		return worker.NodeResult{}, err
	}
	out, err := t.Call(ctx, j.Input)
	tracing.Finish(span, err)
	if err != nil {
		return worker.NodeResult{}, fmt.Errorf("tool %s: %w", toolName, err)
	}
	return worker.NodeResult{Output: out, Provider: "tool", Model: toolName}, nil
}

// ---------- 落库与事件辅助 ----------

// publishNode 落库节点状态并推 SSE 事件。
// durationMS 与 res 只在终态传入，保证 task_nodes 里能查到真实耗时与 token 消耗。
func (s *Scheduler) publishNode(ctx context.Context, j worker.NodeJob, status model.TaskNodeStatus,
	output, errMsg string, start time.Time, durationMS int64, res *worker.NodeResult) {
	n := &model.TaskNode{
		TaskID: j.TaskID, NodeKey: j.NodeKey, NodeType: j.NodeType,
		Status: status, Output: output, Error: errMsg,
		DurationMS: durationMS,
		Input:      truncateRunes(j.Input, maxPersistedInput),
	}
	if res != nil {
		n.TokensIn, n.TokensOut = res.InputTokens, res.OutputTokens
	}
	// 任务被取消时 ctx 已失效，但"节点已终止"这件事仍必须落库，
	// 否则前端永远看到节点卡在 running。
	writeCtx, cancel := writable(ctx, 5*time.Second)
	defer cancel()
	if err := s.tasks.UpdateTaskNode(writeCtx, n); err != nil {
		s.logger.Error("update task node failed",
			"task_id", j.TaskID, "node", j.NodeKey, "status", status, "error", err)
	}

	ev := task.NewEvent(eventForStatus(status), j.TaskID)
	ev.NodeKey = j.NodeKey
	ev.NodeType = string(j.NodeType)
	ev.NodeStatus = string(status)
	ev.Message = errMsg
	ev.DurationMS = durationMS
	if status == model.NodeSucceeded {
		ev.Content = truncateForSSE(output, 200)
	}
	s.hub.Publish(ev)
}

// writable 返回一个可用于写库的上下文：ctx 已失效时退化为短超时后台上下文。
func writable(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx != nil && ctx.Err() == nil {
		return context.WithTimeout(ctx, timeout)
	}
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, timeout)
}

func eventForStatus(status model.TaskNodeStatus) task.EventType {
	switch status {
	case model.NodeRunning:
		return task.EventNodeStarted
	case model.NodeSucceeded:
		return task.EventNodeCompleted
	case model.NodeFailed:
		return task.EventNodeFailed
	case model.NodeSkipped:
		return task.EventNodeSkipped
	case model.NodeCancelled:
		return task.EventNodeCancelled
	default:
		return task.EventLog
	}
}

func (s *Scheduler) logNode(j worker.NodeJob, level, msg string) {
	ctx, cancel := writable(nil, 5*time.Second)
	defer cancel()
	_ = s.tasks.AppendLog(ctx, &model.TaskLog{
		TaskID: j.TaskID, NodeKey: j.NodeKey, Level: level, Message: msg,
	})
}

func (s *Scheduler) recordUsage(j worker.NodeJob, provider, modelName string, inTokens, outTokens int) {
	ctx, cancel := writable(nil, 5*time.Second)
	defer cancel()
	_ = s.tasks.RecordUsage(ctx, &model.UsageRecord{
		UserID: j.UserID, TaskID: j.TaskID, NodeKey: j.NodeKey,
		Provider: provider, Model: modelName,
		InputTokens: inTokens, OutputTokens: outTokens,
	})
	if s.metrics != nil {
		s.metrics.ObserveLLM(provider, modelName, true, 0, inTokens, outTokens)
	}
}

// truncateForSSE 按字节上限截断，但保留完整 UTF-8 字符。
func truncateForSSE(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}

// truncateRunes 按字符数截断并保证 UTF-8 完整（用于限制落库文本长度）。
func truncateRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…（已截断）"
}

package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/tool"
)

// NativeRuntime 是 NebulaFlow 自研的 ReAct 风格 Agent。
//
// 核心循环（ReAct 范式）：
//   Thought → Action (Tool Call) → Observation → Thought → ... → Final Answer
//
// 与 LangChain 的 ReAct Agent 原理一致，但完全自研实现，不依赖任何外部框架。
//
// 为什么要独立成 Runtime，而不是嵌在 scheduler 里：
//  1. 单一职责：调度器负责"何时执行"，Runtime 负责"怎么执行"
//  2. 可测试性：Agent 逻辑可以脱离调度器独立单测
//  3. 可替换性：换成 LangChain Runtime 不需要改调度器
//  4. 可注册性：可以被 Agent Registry 发现和管理
type NativeRuntime struct {
	name        string
	version     string
	description string
	provider    llm.Provider
	tools       *tool.Registry
	defaultModel string
}

// NativeConfig 是 NativeRuntime 的配置。
type NativeConfig struct {
	Name         string
	Version      string
	Description  string
	Provider     llm.Provider
	Tools        *tool.Registry
	DefaultModel string
}

// NewNativeRuntime 创建一个自研 Agent Runtime。
func NewNativeRuntime(cfg NativeConfig) *NativeRuntime {
	name := cfg.Name
	if name == "" {
		name = "native-react-agent"
	}
	version := cfg.Version
	if version == "" {
		version = "1.0.0"
	}
	desc := cfg.Description
	if desc == "" {
		desc = "NebulaFlow 自研 ReAct Agent，支持工具调用与多轮推理"
	}
	return &NativeRuntime{
		name:         name,
		version:      version,
		description:  desc,
		provider:     cfg.Provider,
		tools:        cfg.Tools,
		defaultModel: cfg.DefaultModel,
	}
}

func (r *NativeRuntime) Name() string        { return r.name }
func (r *NativeRuntime) Version() string     { return r.version }
func (r *NativeRuntime) Description() string { return r.description }
func (r *NativeRuntime) Capabilities() []Capability {
	return []Capability{CapToolCall, CapStreaming}
}

// Execute 执行一次 Agent 推理，支持多轮工具调用。
//
// 循环：LLM 判断 → 需要工具则执行并回灌结果 → 再问 LLM → 直到给出最终回答。
func (r *NativeRuntime) Execute(ctx context.Context, input Input) (Result, error) {
	start := time.Now()

	model := input.Model
	if model == "" {
		model = r.defaultModel
	}
	if model == "" {
		model = "deepseek-chat"
	}

	maxRounds := input.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 5
	}

	// 构造消息
	messages := make([]llm.Message, 0, 8)
	if input.System != "" {
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: input.System})
	}
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: input.Prompt})

	// 准备工具描述
	var specs []llm.ToolSpec
	if r.tools != nil {
		descs, _ := r.tools.DescriptorsFor(input.Tools)
		specs = make([]llm.ToolSpec, 0, len(descs))
		for _, d := range descs {
			specs = append(specs, llm.ToolSpec{
				Name: d.Name, Description: d.Description, Parameters: d.Parameters,
			})
		}
	}

	var (
		inTokens  int
		outTokens int
		toolCalls []ToolCallRecord
	)

	for round := 1; round <= maxRounds; round++ {
		resp, err := r.provider.Chat(ctx, llm.ChatRequest{
			Model:    model,
			Messages: messages,
			Tools:    specs,
		})
		if err != nil {
			return Result{}, fmt.Errorf("agent round %d: %w", round, err)
		}
		inTokens += resp.InputTokens
		outTokens += resp.OutputTokens

		// 没有工具调用 → 结束，返回最终回答
		if len(resp.ToolCalls) == 0 {
			return Result{
				Output:       resp.Content,
				Rounds:       round,
				ToolCalls:    toolCalls,
				InputTokens:  inTokens,
				OutputTokens: outTokens,
				Provider:     resp.Provider,
				Model:        resp.Model,
				Duration:     time.Since(start),
			}, nil
		}

		// 回灌 assistant 消息（带 tool_calls）
		messages = append(messages, llm.Message{
			Role: llm.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
		})

		// 执行工具调用（并发更高效，但为了保持简单和可观测，串行执行）
		for _, call := range resp.ToolCalls {
			toolStart := time.Now()
			result, err := r.callTool(ctx, call.Name, call.Arguments)
			duration := time.Since(toolStart)

			toolCalls = append(toolCalls, ToolCallRecord{
				Name: call.Name, Args: call.Arguments, Result: result,
				Duration: duration,
			})
			if err != nil {
				toolCalls[len(toolCalls)-1].Error = err.Error()
				// 工具出错也要把结果回灌，让模型自己决定下一步
				result = fmt.Sprintf("Error: %v", err)
			}

			// 回灌 tool 消息
			messages = append(messages, llm.Message{
				Role: llm.RoleTool, Content: result, ToolCallID: call.ID,
			})
		}
	}

	// 轮数用尽仍未得出最终回答 → 返回最后一条 assistant 消息
	lastContent := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == llm.RoleAssistant {
			lastContent = messages[i].Content
			break
		}
	}
	return Result{
		Output:       lastContent,
		Rounds:       maxRounds,
		ToolCalls:    toolCalls,
		InputTokens:  inTokens,
		OutputTokens: outTokens,
		Duration:     time.Since(start),
	}, fmt.Errorf("agent: reached max rounds (%d) without final answer", maxRounds)
}

// callTool 调用一个已注册的工具。
// 优先使用 ArgumentCaller 接口（模型 JSON 格式参数），
// 不支持则回退到普通 Call（字符串参数）。
func (r *NativeRuntime) callTool(ctx context.Context, name, args string) (string, error) {
	if r.tools == nil {
		return "", fmt.Errorf("tool registry not configured")
	}
	t, ok := r.tools.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %q not found", name)
	}
	if ac, ok := t.(tool.ArgumentCaller); ok {
		return ac.CallWithArguments(ctx, args)
	}
	// 回退：把 JSON 字符串直接传进去（简单工具能处理）
	return t.Call(ctx, args)
}

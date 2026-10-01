// Package agent 定义 Agent Runtime 抽象层。
//
// NebulaFlow 的 Agent 节点不绑定具体实现，而是通过 Runtime 接口接入：
//   - NativeRuntime：NebulaFlow 自研的 Agent（工具调用 + 多轮推理）
//   - LangChainRuntime：通过 HTTP 调用 LangChain / LangServe 部署的 Agent
//
// 设计目标：让工作流引擎可以编排任意 Agent Runtime，
// 而不是"只能跑自己的 Agent"。这也是企业级平台与 Demo 的核心区别。
package agent

import (
	"context"
	"time"
)

// Input 是 Agent 执行的统一输入。
type Input struct {
	// Prompt 是用户输入 / 本轮提示词。
	Prompt string
	// System 是系统提示词（可选）。
	System string
	// Tools 是允许 Agent 调用的工具名列表；空表示全部可用工具。
	Tools []string
	// MaxRounds 是最大推理轮数（防止无限循环）。
	MaxRounds int
	// Model 是模型名；空表示使用 runtime 默认模型。
	Model string
	// Metadata 是透传字段，用于 trace / 日志 / 计费。
	Metadata map[string]string
}

// Result 是 Agent 执行的统一输出。
type Result struct {
	// Output 是 Agent 的最终回答。
	Output string
	// Rounds 是实际用了多少轮推理。
	Rounds int
	// ToolCalls 是本轮调用的工具清单（用于审计 / 展示）。
	ToolCalls []ToolCallRecord
	// InputTokens / OutputTokens 是 token 消耗。
	InputTokens  int
	OutputTokens int
	// Provider 是实际调用的模型提供方（如 deepseek / openai / langchain）。
	Provider string
	// Model 是实际使用的模型名。
	Model string
	// Duration 是总耗时。
	Duration time.Duration
}

// ToolCallRecord 记录一次工具调用（用于审计和前端展示）。
type ToolCallRecord struct {
	Name     string        `json:"name"`
	Args     string        `json:"args"`
	Result   string        `json:"result"`
	Duration time.Duration `json:"duration_ms"`
	Error    string        `json:"error,omitempty"`
}

// Runtime 是 Agent 运行时的统一抽象。
//
// 任何实现了这个接口的 Agent 都可以被 NebulaFlow 工作流编排：
//   - NativeRuntime：NebulaFlow 自研的 function calling Agent
//   - LangChainRuntime：调用外部 LangChain / LangServe 服务
//   - 未来可以扩展 OpenAI Assistant、AutoGPT 等
type Runtime interface {
	// Name 返回 runtime 名称，用于日志和审计。
	Name() string
	// Execute 同步执行一次 Agent 推理，返回最终结果。
	Execute(ctx context.Context, input Input) (Result, error)
}

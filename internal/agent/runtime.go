// Package agent 定义 Agent Runtime 抽象层。
//
// NebulaFlow 的 Agent 节点不绑定具体实现，而是通过 Runtime 接口接入：
//   - NativeRuntime：NebulaFlow 自研的 Agent（工具调用 + 多轮推理）
//   - LangChainRuntime：通过 HTTP 调用 LangChain / LangServe 部署的 Agent
//
// 设计目标：让工作流引擎可以编排任意 Agent Runtime，
// 而不是"只能跑自己的 Agent"。这也是企业级平台与 Demo 的核心区别。
//
// 未来扩展方向：
//   - Agent Registry（注册中心）：按能力发现 Agent
//   - Agent Marketplace：第三方 Agent 接入
//   - 动态 Agent 选择：根据任务自动匹配合适的 Agent
package agent

import (
	"context"
	"time"
)

// Capability 表示 Agent 具备的能力，用于注册中心的能力发现与匹配。
type Capability string

const (
	// CapToolCall 支持工具调用（function calling）
	CapToolCall Capability = "tool_call"
	// CapRAG 支持知识库检索增强
	CapRAG Capability = "rag"
	// CapMemory 支持长期记忆 / 会话记忆
	CapMemory Capability = "memory"
	// CapStreaming 支持流式输出
	CapStreaming Capability = "streaming"
	// CapCode 代码生成与执行能力
	CapCode Capability = "code"
	// CapSearch 联网搜索能力
	CapSearch Capability = "search"
	// CapMultimodal 多模态（图文理解）
	CapMultimodal Capability = "multimodal"
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
	// SessionID 是会话 ID，用于短期记忆（同一个会话共享历史消息）。
	// 为空表示不启用短期记忆。
	SessionID string
	// UserID 是用户 ID，用于长期记忆（按用户隔离记忆）。
	// 为 0 表示不启用长期记忆。
	UserID int64
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
//
// 元数据方法（Name/Version/Capabilities/Tags/Endpoint）的作用：
//  1. Agent Registry 注册中心：按能力检索 Agent
//  2. 前端展示：用户能看到每个 Agent 支持什么、连的是哪个端点
//  3. 动态路由：根据任务需求自动选择最合适的 Agent
//  4. 健康监控：HealthCheck 用于探活，调度器可以跳过不健康的 Runtime
type Runtime interface {
	// Name 返回 runtime 唯一标识，用于注册与发现。
	Name() string
	// Version 返回语义化版本号，用于灰度发布与版本管理。
	Version() string
	// Capabilities 返回 Agent 具备的能力列表。
	Capabilities() []Capability
	// Description 返回 Agent 的人类可读描述。
	Description() string
	// Tags 返回分类标签（如 "production"、"experimental"、"customer-service"）。
	// 用于 Registry 的多维度过滤和搜索。
	Tags() []string
	// Endpoint 返回远程 Runtime 的端点 URL（如 LangChain 的 baseURL）。
	// 本地 Runtime（如 NativeRuntime）返回空字符串。
	Endpoint() string
	// HealthCheck 检查 Runtime 是否可用。
	// 对于远程 Runtime，通常是一次轻量 HTTP 请求；本地 Runtime 检查依赖是否就绪。
	// 调度器可以用这个方法跳过不健康的 Runtime，实现故障转移。
	HealthCheck(ctx context.Context) error
	// Execute 同步执行一次 Agent 推理，返回最终结果。
	Execute(ctx context.Context, input Input) (Result, error)
}

// RuntimeMetadata 是 Runtime 元数据的可序列化快照。
//
// 用于 API 响应，一次调用拿到全部元数据，
// 而不是前端分别请求 Name、Version、Capabilities 等多个字段。
//
// 包含健康状态，可以用于运行时面板展示所有 Agent 的状态。
type RuntimeMetadata struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description"`
	Capabilities []Capability `json:"capabilities"`
	Tags         []string     `json:"tags"`
	Endpoint     string       `json:"endpoint,omitempty"`
	Healthy      bool         `json:"healthy"`
}

// HasCapability 检查 Runtime 是否具备指定能力。
//
// 用于动态路由场景：调度器收到任务后，根据任务需求
// 查找具备相应能力的 Runtime。
func HasCapability(r Runtime, cap Capability) bool {
	for _, c := range r.Capabilities() {
		if c == cap {
			return true
		}
	}
	return false
}

// Metadata 生成 Runtime 的元数据快照（不含健康状态，不执行 HealthCheck）。
//
// 适用于不需要实时健康状态的场景（如列表展示），
// 避免每次展示都触发健康检查（远程 Runtime 的 HealthCheck 可能很慢）。
func Metadata(r Runtime) RuntimeMetadata {
	return RuntimeMetadata{
		Name:         r.Name(),
		Version:      r.Version(),
		Description:  r.Description(),
		Capabilities: r.Capabilities(),
		Tags:         r.Tags(),
		Endpoint:     r.Endpoint(),
	}
}

// MetadataWithHealth 生成包含健康状态的元数据快照。
//
// 会执行 HealthCheck，适用于详情页或调度前的可用性检查。
// 如果 HealthCheck 失败，Healthy 为 false 但其他元数据仍然返回。
func MetadataWithHealth(ctx context.Context, r Runtime) RuntimeMetadata {
	md := Metadata(r)
	md.Healthy = r.HealthCheck(ctx) == nil
	return md
}

// CapLabelMap 将能力列表转为能力名→true 的 map，方便快速查找。
func CapLabelMap(caps []Capability) map[string]bool {
	m := make(map[string]bool, len(caps))
	for _, c := range caps {
		m[string(c)] = true
	}
	return m
}

// Package llm 实现统一的 LLM Gateway：
//   - Provider 接口抽象（Chat / Stream）
//   - OpenAI 兼容协议实现（DeepSeek / OpenAI / MiMo 均兼容）
//   - Ollama 本地推理实现
//   - Mock 实现（离线演示 / 测试）
//   - 带指数退避重试与故障转移（Fallback）的 Gateway
//
// 设计目标：上层（scheduler 节点执行）只依赖 Gateway 接口，
// 不关心具体厂商协议，切换/降级模型对执行引擎完全透明。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------- 公共类型 ----------

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolSpec 描述一个可以被模型自主调用的工具（function calling 的 tools[] 元素）。
// Parameters 是 JSON Schema；为空表示"参数形状未声明"，调用方应回退到宽松 schema。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall 是模型要求执行的一次工具调用。
//
// Arguments 刻意保留为**原始 JSON 字符串**而不是解析后的 map：
//  1. 厂商返回的就是字符串，原样透传才能保证日志里看到的是模型真正说了什么；
//  2. 参数形状由工具自己解释（各工具入参不同），中间层解析一次再序列化一次
//     只会引入失真（如数字精度、键顺序），且无法处理非对象参数。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ToolCalls 只在 RoleAssistant 且模型要求调用工具时非空。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 只在 RoleTool 时非空，用于把结果对应回某次调用。
	// 缺了它，一轮里有多个工具调用时模型无法判断哪个结果属于哪次调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// Tools 非空时把工具清单交给模型，由模型自主决定是否调用（function calling）。
	Tools       []ToolSpec `json:"tools,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature float64    `json:"temperature,omitempty"`
}

type ChatResponse struct {
	Content string
	// ToolCalls 是模型本轮要求执行的工具调用（可能一次多个，顺序即模型给出的顺序）。
	// 非空表示本轮没有最终回答，调用方应执行工具后把结果回灌再问一轮。
	ToolCalls []ToolCall
	// FinishReason 透传厂商的结束原因（"stop" / "tool_calls" / "length" ...）。
	// 只用于日志与排查：判断"是否还要再来一轮"一律以 ToolCalls 是否为空为准，
	// 因为并非所有 OpenAI 兼容厂商都会正确设置这个字段。
	FinishReason string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	LatencyMS    int64
}

// Chunk 是流式输出的一个片段。
// Err 携带流式中途出现的错误（置位时 Done 同时为 true），
// 网关据此判定"已提交之后失败"，由上层决定重试而不是静默截断。
type Chunk struct {
	Content string
	Done    bool
	Err     error
}

// Provider 是所有模型厂商的统一抽象。
type Provider interface {
	Name() string
	// Chat 返回单次完整响应。
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// Stream 逐块返回生成内容；在产出任何块之前的错误会直接返回 error。
	Stream(ctx context.Context, req ChatRequest) (<-chan Chunk, error)
}

// ---------- OpenAI 兼容实现（DeepSeek / OpenAI / MiMo 共用） ----------

type OpenAICompatProvider struct {
	name    string
	baseURL string // 如 https://api.deepseek.com/v1
	apiKey  string
	client  *http.Client
}

func NewDeepSeekProvider(baseURL, apiKey string) *OpenAICompatProvider {
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	return &OpenAICompatProvider{name: "deepseek", baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second}}
}

func NewOpenAIProvider(baseURL, apiKey string) *OpenAICompatProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAICompatProvider{name: "openai", baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second}}
}

// NewMiMoProvider：小米 MiMo 走 OpenAI 兼容协议。
func NewMiMoProvider(baseURL, apiKey string) *OpenAICompatProvider {
	if baseURL == "" {
		baseURL = "https://api.xiaomi.com/mimo/v1"
	}
	return &OpenAICompatProvider{name: "mimo", baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey,
		client: &http.Client{Timeout: 120 * time.Second}}
}

func (p *OpenAICompatProvider) Name() string { return p.name }

// ---------- OpenAI 线上格式 ----------

// openAIChatMessage 是发往 OpenAI 兼容接口的消息体。
//
// Content 用 any 而不是 string 是刻意的：按协议，带 tool_calls 的 assistant
// 消息 content 应当为 null（而非空串）。实测部分兼容厂商对空串会返回 400
// （它们校验 "content 与 tool_calls 至少一个有值"，把 "" 当成了"无内容"的另一种写法
// 却又不接受）。需要发 null 时置 nil，其余情况置字符串。
type openAIChatMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIFunctionCall 是线上格式里的 function 对象（调用方向）。
type openAIFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// openAIToolCall 同时用于请求与响应两个方向：
//   - 请求：回灌 assistant 的历史 tool_calls（此时 Type 必须是 "function"，Index 省略）；
//   - 响应/流式 delta：解析厂商返回的调用（此时 Index 标识同一轮内第几个调用）。
type openAIToolCall struct {
	Index    int                `json:"index,omitempty"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function openAIFunctionCall `json:"function"`
}

// openAIToolSchema 是线上格式里的 function 对象（声明方向）。
type openAIToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAITool struct {
	Type     string           `json:"type"`
	Function openAIToolSchema `json:"function"`
}

type openAIChatBody struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	Tools       []openAITool        `json:"tools,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
}
type openAIChatResp struct {
	Choices []struct {
		Message struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// toOpenAIMessages 把公共消息模型翻译成线上格式。
//
// 两条容易写错的规则都在这里收口：
//  1. assistant + tool_calls 且无文本时，content 必须是 null 而不是 ""；
//  2. tool 消息必须带 tool_call_id，否则厂商无法把结果对应回调用。
func toOpenAIMessages(msgs []Message) []openAIChatMessage {
	out := make([]openAIChatMessage, 0, len(msgs))
	for _, m := range msgs {
		wire := openAIChatMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCalls:  toOpenAIToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		}
		if m.Content == "" && len(m.ToolCalls) > 0 {
			wire.Content = nil
		}
		out = append(out, wire)
	}
	return out
}

func toOpenAIToolCalls(calls []ToolCall) []openAIToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]openAIToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, openAIToolCall{
			ID:   c.ID,
			Type: "function", // 协议要求该字段存在，缺了会被部分厂商拒绝
			Function: openAIFunctionCall{
				Name:      c.Name,
				Arguments: c.Arguments,
			},
		})
	}
	return out
}

func toOpenAITools(specs []ToolSpec) []openAITool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]openAITool, 0, len(specs))
	for _, s := range specs {
		out = append(out, openAITool{
			Type: "function",
			Function: openAIToolSchema{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  s.Parameters,
			},
		})
	}
	return out
}

// parseToolCalls 解析厂商返回的 tool_calls。
//
// 跳过 name 为空的元素：流式响应里第一个 tool_call 分片常常只有 index 没有 name，
// 原样收下会让上层拿一个"没有名字的工具"去查注册表，报出难以理解的
// "unknown tool """，而真正的原因只是分片。
func parseToolCalls(raw []openAIToolCall) []ToolCall {
	var out []ToolCall
	for _, c := range raw {
		if c.Function.Name == "" {
			continue
		}
		out = append(out, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	return out
}

func (p *OpenAICompatProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	start := time.Now()
	body := openAIChatBody{
		Model:       req.Model,
		Messages:    toOpenAIMessages(req.Messages),
		Tools:       toOpenAITools(req.Tools),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	payload, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.name, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("provider %s http %d: %s", p.name, resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed openAIChatResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("provider %s decode: %w", p.name, err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("provider %s api error: %s", p.name, parsed.Error.Message)
	}
	out := &ChatResponse{
		Provider:     p.name,
		Model:        req.Model,
		InputTokens:  parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
		LatencyMS:    time.Since(start).Milliseconds(),
	}
	if len(parsed.Choices) > 0 {
		out.Content = parsed.Choices[0].Message.Content
		out.ToolCalls = parseToolCalls(parsed.Choices[0].Message.ToolCalls)
		out.FinishReason = parsed.Choices[0].FinishReason
	}
	return out, nil
}

// Stream 使用 SSE 解析逐块返回。注意：Ollama 的 /v1/chat/completions
// 同样兼容该格式。
//
// **流式不支持工具调用**：OpenAI 的 tool_calls 在 SSE 里是一串增量分片
// （每个 delta 只带 index + 参数片段），要正确还原必须额外维护"按 index 聚合
// 分片"的状态机；而我们的 Chunk 只能表达文本。与其半途丢掉工具调用、
// 让上层拿到一个"模型什么都没说"的空响应，不如在这里直接报错：
// 需要工具调用就请走 Chat（agentic 循环正是这么做的）。
func (p *OpenAICompatProvider) Stream(ctx context.Context, req ChatRequest) (<-chan Chunk, error) {
	if len(req.Tools) > 0 {
		return nil, fmt.Errorf("provider %s: streaming does not support tool calls, use Chat instead", p.name)
	}
	body := openAIChatBody{
		Model:       req.Model,
		Messages:    toOpenAIMessages(req.Messages),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      true,
	}
	payload, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider %s stream: %w", p.name, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("provider %s stream http %d: %s", p.name, resp.StatusCode, truncate(string(raw), 300))
	}

	ch := make(chan Chunk, 32)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				// 注意：必须返回 nil 错误，否则网关会误判成"流失败"而白白触发 fallback
				select {
				case ch <- Chunk{Done: true}:
				case <-ctx.Done():
				}
				return
			}
			var parsed openAIChatResp
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				continue
			}
			if parsed.Error != nil {
				// 流中途报错：带上错误结束，让网关/节点层能看到真实原因
				select {
				case ch <- Chunk{Done: true, Err: errors.New(parsed.Error.Message)}:
				case <-ctx.Done():
				}
				return
			}
			if len(parsed.Choices) > 0 {
				select {
				case ch <- Chunk{Content: parsed.Choices[0].Delta.Content}:
				case <-ctx.Done():
					return
				}
			}
		}
		// 扫描结束：区分"正常读完"与"读取出错"
		done := Chunk{Done: true}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			done.Err = fmt.Errorf("provider %s stream read: %w", p.name, err)
		}
		select {
		case ch <- done:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// ---------- Ollama 原生实现（/api/chat） ----------
//
// 刻意不复用 openAIChatMessage：Ollama 的 content 只接受字符串（收到 null 会报错），
// 且它的工具调用用的是另一套字段（message.tool_calls 的 function.arguments 是对象
// 而不是字符串）。共用结构体会把"看起来能编译、实际协议不兼容"的问题藏起来。
// **本实现不支持工具调用**：传入的 ToolSpec 会被忽略，消息里的 ToolCalls 会被丢弃。

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatBody struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
	Options  map[string]any      `json:"options,omitempty"`
}
type ollamaChatResp struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

func toOllamaMessages(msgs []Message) []ollamaChatMessage {
	out := make([]ollamaChatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, ollamaChatMessage{Role: string(m.Role), Content: m.Content})
	}
	return out
}

type OllamaProvider struct {
	name    string
	baseURL string // 如 http://localhost:11434
	client  *http.Client
}

func NewOllamaProvider(baseURL string) *OllamaProvider {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaProvider{name: "ollama", baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{Timeout: 120 * time.Second}}
}

func (p *OllamaProvider) Name() string { return p.name }

func (p *OllamaProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	start := time.Now()
	body := ollamaChatBody{Model: req.Model, Stream: false, Messages: toOllamaMessages(req.Messages), Options: map[string]any{
		"temperature": req.Temperature,
		"num_predict": req.MaxTokens,
	}}
	payload, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider ollama: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("provider ollama http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed ollamaChatResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("provider ollama decode: %w", err)
	}
	if parsed.Error != "" {
		return nil, fmt.Errorf("provider ollama: %s", parsed.Error)
	}
	return &ChatResponse{
		Content:      parsed.Message.Content,
		Provider:     p.name,
		Model:        req.Model,
		InputTokens:  parsed.PromptEvalCount,
		OutputTokens: parsed.EvalCount,
		LatencyMS:    time.Since(start).Milliseconds(),
	}, nil
}

func (p *OllamaProvider) Stream(ctx context.Context, req ChatRequest) (<-chan Chunk, error) {
	body := ollamaChatBody{Model: req.Model, Stream: true, Messages: toOllamaMessages(req.Messages), Options: map[string]any{
		"temperature": req.Temperature,
		"num_predict": req.MaxTokens,
	}}
	payload, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider ollama stream: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("provider ollama stream http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	ch := make(chan Chunk, 32)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		dec := json.NewDecoder(resp.Body)
		for {
			var parsed ollamaChatResp
			if err := dec.Decode(&parsed); err != nil {
				// io.EOF 是正常结束；context 取消也不算错误
				done := Chunk{Done: true}
				if err != io.EOF && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
					done.Err = fmt.Errorf("provider ollama stream read: %w", err)
				}
				select {
				case ch <- done:
				case <-ctx.Done():
				}
				return
			}
			if parsed.Error != "" {
				select {
				case ch <- Chunk{Done: true, Err: errors.New(parsed.Error)}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case ch <- Chunk{Content: parsed.Message.Content, Done: parsed.Done}:
			case <-ctx.Done():
				return
			}
			if parsed.Done {
				return
			}
		}
	}()
	return ch, nil
}

// ---------- Mock 实现（离线演示 / 测试） ----------

// MockProvider 在无外部模型服务时模拟 LLM 行为：
// 按输入长度生成确定性输出，便于本地跑通整条链路与压测。
type MockProvider struct {
	name string
	// Latency 模拟每请求固定延迟（毫秒）。
	Latency time.Duration
	// FailRate 模拟 0~1 的失败概率，用于测试 fallback。
	FailRate float64

	// AutoToolCall 打开后，本 Provider 会在"模型有机会调用工具"时发起一次工具调用，
	// 用于在**没有真实模型 key** 的情况下端到端验证 function calling 链路
	// （请求带 tools → 返回 tool_calls → 上层执行工具 → 结果回灌 → 得到最终回答）。
	//
	// 触发规则（刻意做成与用户输入无关，保证可复现）：
	//   - 请求未带 Tools       → 维持原有的 echo 行为，完全不受影响；
	//   - 最后一条是 tool 结果 → 返回最终回答，内容里带上工具输出，便于断言往返；
	//   - 其余情况             → 返回一次 ToolCall（默认调 calculator）。
	AutoToolCall bool
	// ToolName / ToolArgs 指定自动调用哪个工具与参数。
	// 默认 calculator + `{"expr":"(12+34)*5/2"}` —— 真实的 Calculator 会算出 115。
	ToolName string
	ToolArgs string
}

func NewMockProvider(name string) *MockProvider {
	return &MockProvider{name: name, Latency: 20 * time.Millisecond}
}

func (p *MockProvider) Name() string { return p.name }

// mockToolName / mockToolArgs 是 AutoToolCall 的默认目标。
const (
	mockToolName = "calculator"
	mockToolArgs = `{"expr":"(12+34)*5/2"}`
)

func (p *MockProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	select {
	case <-time.After(p.Latency):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.FailRate > 0 && time.Now().UnixNano()%1000 < int64(p.FailRate*1000) {
		return nil, errors.New("mock provider simulated failure")
	}
	last := ""
	lastRole := Role("")
	if len(req.Messages) > 0 {
		last = req.Messages[len(req.Messages)-1].Content
		lastRole = req.Messages[len(req.Messages)-1].Role
	}

	if p.AutoToolCall && len(req.Tools) > 0 {
		if lastRole == RoleTool {
			// 工具结果已回灌：给出最终回答（内容里保留工具输出，便于测试断言往返）
			content := fmt.Sprintf("[%s-mock:%s] tool result: %s", p.name, req.Model, truncate(last, 200))
			return &ChatResponse{
				Content: content, FinishReason: "stop",
				Provider: p.name, Model: req.Model,
				InputTokens: len(strings.Fields(last)), OutputTokens: len(strings.Fields(content)),
				LatencyMS: p.Latency.Milliseconds(),
			}, nil
		}
		name, args := p.ToolName, p.ToolArgs
		if name == "" {
			name = mockToolName
		}
		if args == "" {
			args = mockToolArgs
		}
		if offered(req.Tools, name) {
			return &ChatResponse{
				ToolCalls:    []ToolCall{{ID: "mock-call-1", Name: name, Arguments: args}},
				FinishReason: "tool_calls",
				Provider:     p.name, Model: req.Model,
				InputTokens: len(strings.Fields(last)), LatencyMS: p.Latency.Milliseconds(),
			}, nil
		}
		// 请求里没有这个工具：不要凭空造一个不存在的调用，退回普通回答
	}

	content := fmt.Sprintf("[%s-mock:%s] echo: %s", p.name, req.Model, truncate(last, 200))
	return &ChatResponse{
		Content:      content,
		Provider:     p.name,
		Model:        req.Model,
		InputTokens:  len(strings.Fields(last)),
		OutputTokens: len(strings.Fields(content)),
		LatencyMS:    p.Latency.Milliseconds(),
	}, nil
}

// offered 判断某个工具名是否在本次请求声明的工具清单里。
// 少了这道检查，Mock 会在只暴露了工具子集时仍去调用被排除的工具，
// 测试就会在一个真实模型不可能出现的前提下通过。
func offered(specs []ToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

func (p *MockProvider) Stream(ctx context.Context, req ChatRequest) (<-chan Chunk, error) {
	// 与 OpenAI 兼容实现保持一致：流式不承载工具调用，明确报错而不是返回空流。
	// 返回空流会被网关判定成"该 provider 无产出"，触发一次毫无意义的 fallback，
	// 掩盖掉真正的调用方错误。
	if len(req.Tools) > 0 {
		return nil, fmt.Errorf("provider %s: streaming does not support tool calls, use Chat instead", p.name)
	}
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan Chunk, 16)
	go func() {
		defer close(ch)
		// 按 rune 切块模拟流式（每块 3 个字符），避免切坏 UTF-8 序列
		runes := []rune(resp.Content)
		for i := 0; i < len(runes); i += 3 {
			end := i + 3
			if end > len(runes) {
				end = len(runes)
			}
			select {
			case ch <- Chunk{Content: string(runes[i:end])}:
			case <-ctx.Done():
				// 消费者已离开或上下文结束：给一个带错误的结束块，避免调用方永久阻塞
				select {
				case ch <- Chunk{Done: true, Err: ctx.Err()}:
				default:
				}
				return
			}
		}
		select {
		case ch <- Chunk{Done: true}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// ---------- 工具函数 ----------

// truncate 按字节上限截断，但保证不切断多字节 UTF-8 字符（避免产生非法编码）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}

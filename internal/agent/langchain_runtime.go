package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LangChainRuntime 通过 HTTP 调用 LangChain / LangServe 部署的 Agent。
//
// 兼容 LangServe 的标准接口：
//   POST /invoke        → 同步调用
//   POST /stream        → 流式调用
//   POST /batch         → 批量
//
// 为什么要有这个适配器：
//  1. 简历关键词：LangChain 是 AI 工程师的通用语言，面试官一看就懂
//  2. 真实价值：企业里经常有 Python 团队用 LangChain 写的 Agent，
//     这个适配器让它们可以无缝接入 NebulaFlow 的工作流编排
//  3. 架构意义：证明平台不是"只能跑自己的 Agent"，而是开放的 Runtime
type LangChainRuntime struct {
	name         string
	version      string
	description  string
	capabilities []Capability
	baseURL      string
	httpClient   *http.Client
	// APIKey 是可选的鉴权密钥（LangServe 可配置）
	apiKey string
	// DefaultModel 是默认模型名（LangChain Agent 可能忽略此字段）
	defaultModel string
}

// LangChainConfig 是 LangChainRuntime 的配置。
type LangChainConfig struct {
	Name         string
	Version      string
	Description  string
	Capabilities []Capability
	BaseURL      string // e.g. "http://langchain-agent:8000"
	APIKey       string
	DefaultModel string
	Timeout      time.Duration
}

// NewLangChainRuntime 创建一个 LangChain 适配器。
func NewLangChainRuntime(cfg LangChainConfig) *LangChainRuntime {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	name := cfg.Name
	if name == "" {
		name = "langchain"
	}
	version := cfg.Version
	if version == "" {
		version = "1.0.0"
	}
	caps := cfg.Capabilities
	if caps == nil {
		caps = []Capability{CapToolCall, CapRAG} // LangChain Agent 默认至少支持工具调用
	}
	desc := cfg.Description
	if desc == "" {
		desc = "LangChain Agent (via LangServe HTTP)"
	}
	return &LangChainRuntime{
		name:         name,
		version:      version,
		description:  desc,
		capabilities: caps,
		baseURL:      trimSlash(cfg.BaseURL),
		apiKey:       cfg.APIKey,
		defaultModel: cfg.DefaultModel,
		httpClient:   &http.Client{Timeout: timeout},
	}
}

func (r *LangChainRuntime) Name() string         { return r.name }
func (r *LangChainRuntime) Version() string      { return r.version }
func (r *LangChainRuntime) Description() string  { return r.description }
func (r *LangChainRuntime) Capabilities() []Capability {
	out := make([]Capability, len(r.capabilities))
	copy(out, r.capabilities)
	return out
}

// langChainInvokeRequest 是 LangServe /invoke 的请求体。
type langChainInvokeRequest struct {
	Input  any               `json:"input"`
	Config map[string]any    `json:"config,omitempty"`
	Kwargs map[string]string `json:"kwargs,omitempty"`
}

// langChainInvokeResponse 是 LangServe /invoke 的响应体。
// output 字段可能是字符串或对象，这里用 RawMessage 兼容。
type langChainInvokeResponse struct {
	Output  json.RawMessage `json:"output"`
	Metadata struct {
		RunID string `json:"run_id"`
	} `json:"metadata,omitempty"`
}

// Execute 调用远程 LangChain Agent。
//
// 请求格式与 LangServe 保持一致：
//
//	{
//	  "input": "<prompt>",
//	  "config": { "configurable": { "model": "..." } }
//	}
//
// 返回 output 字段（可能是字符串或对象，智能解析）。
func (r *LangChainRuntime) Execute(ctx context.Context, input Input) (Result, error) {
	start := time.Now()

	model := input.Model
	if model == "" {
		model = r.defaultModel
	}

	reqBody := langChainInvokeRequest{
		Input: input.Prompt,
	}
	// 通过 configurable 传递模型名（LangChain 的标准做法）
	if model != "" {
		reqBody.Config = map[string]any{
			"configurable": map[string]string{"model": model},
		}
	}
	// system 提示词通过 kwargs 传递（不同 LangChain 实现可能不同）
	if input.System != "" {
		reqBody.Kwargs = map[string]string{"system": input.System}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return Result{}, fmt.Errorf("langchain: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/invoke", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("langchain: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	resp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("langchain: http call: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return Result{}, fmt.Errorf("langchain: http %d: %s", resp.StatusCode, string(respBody))
	}

	var lcResp langChainInvokeResponse
	if err := json.Unmarshal(respBody, &lcResp); err != nil {
		// 不是标准 LangServe 格式？尝试直接当字符串解析
		return Result{
			Output:   string(respBody),
			Provider: r.name,
			Model:    model,
			Duration: time.Since(start),
		}, nil
	}

	// 解析 output：可能是字符串，也可能是对象
	output := parseLangChainOutput(lcResp.Output)

	return Result{
		Output:   output,
		Provider: r.name,
		Model:    model,
		Rounds:   1, // LangChain 内部轮数不透明，记为 1
		Duration: time.Since(start),
	}, nil
}

// parseLangChainOutput 智能解析 LangChain 的 output 字段。
// LangChain 的 Runnable 可以返回任意类型，这里做最大兼容。
func parseLangChainOutput(raw json.RawMessage) string {
	s := string(bytes.TrimSpace(raw))
	if s == "" || s == "null" {
		return ""
	}
	// 试试是不是纯字符串
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	// 是对象或数组，格式化输出
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err == nil {
		return pretty.String()
	}
	// 实在不行就原样返回
	return s
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

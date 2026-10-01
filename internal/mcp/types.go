// Package mcp 实现 Model Context Protocol（MCP）客户端。
//
// MCP 是 Anthropic 提出的标准化协议，让 LLM 可以统一方式连接外部工具、
// 数据源、文件系统等。类似于 OpenAPI 之于 REST API，但专为 LLM 设计。
//
// 参考：https://modelcontextprotocol.io/
//
// NebulaFlow 通过 MCP 客户端可以接入任何 MCP Server，
// 把 MCP 工具注册到 Tool Registry，让 Agent 像调用内部工具一样调用外部 MCP 工具。
//
// 架构：
//
//	Agent → Tool Registry → MCP Tool Adapter → MCP Client → MCP Server
//	                                                      (外部服务)
//
// 支持的传输方式：
//   - SSE：HTTP 长连接，最常用，适合远程 MCP Server
//   - （未来可扩展 stdio：本地进程通信）
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ---------- JSON-RPC 基础类型 ----------
//
// MCP 基于 JSON-RPC 2.0 协议。
// 请求：{ jsonrpc: "2.0", id: number|string, method: string, params?: object }
// 响应：{ jsonrpc: "2.0", id: number|string, result: object } 或 { error: { code, message } }
// 通知：{ jsonrpc: "2.0", method: string, params?: object } （无 id）

// Request 是 JSON-RPC 请求。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response 是 JSON-RPC 响应。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError 是 JSON-RPC 错误。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

// Notification 是 JSON-RPC 通知（无 id）。
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ---------- MCP 核心方法 ----------

// InitializeParams 是 initialize 请求的参数。
type InitializeParams struct {
	ProtocolVersion string        `json:"protocolVersion"`
	ClientInfo      Implementation `json:"clientInfo"`
	Capabilities    ClientCaps    `json:"capabilities"`
}

// InitializeResult 是 initialize 响应。
type InitializeResult struct {
	ProtocolVersion string        `json:"protocolVersion"`
	ServerInfo      Implementation `json:"serverInfo"`
	Capabilities    ServerCaps    `json:"capabilities"`
}

// Implementation 描述了 Client 或 Server 的名称与版本。
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCaps 是客户端支持的能力。
type ClientCaps struct {
	Roots  *RootsCapability  `json:"roots,omitempty"`
	Sampling *SamplingCapability `json:"sampling,omitempty"`
}

// ServerCaps 是服务端支持的能力。
type ServerCaps struct {
	Tools     *ToolCapability     `json:"tools,omitempty"`
	Resources *ResourceCapability `json:"resources,omitempty"`
	Prompts   *PromptCapability   `json:"prompts,omitempty"`
}

// ToolCapability 表示服务端支持工具调用。
type ToolCapability struct {
	// 工具列表变化时是否发通知
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourceCapability 表示服务端支持资源访问。
type ResourceCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptCapability 表示服务端支持提示词模板。
type PromptCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// RootsCapability 表示客户端支持提供根目录。
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// SamplingCapability 表示客户端支持采样（LLM 调用）。
type SamplingCapability struct{}

// ---------- 工具相关类型 ----------

// Tool 是 MCP 工具描述。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"` // JSON Schema
	// Annotations 是可选的元数据（如标题、图标、标签）。
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations 是工具的可选元数据。
type ToolAnnotations struct {
	Title             string   `json:"title,omitempty"`
	Icon              string   `json:"icon,omitempty"`
	ReadOnlyHint      bool     `json:"readOnlyHint,omitempty"`
	DestructiveHint   bool     `json:"destructiveHint,omitempty"`
	OpenWorldHint     bool     `json:"openWorldHint,omitempty"`
	IdempotentHint    bool     `json:"idempotentHint,omitempty"`
}

// ListToolsResult 是 tools/list 响应。
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams 是 tools/call 参数。
type CallToolParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

// CallToolResult 是 tools/call 响应。
type CallToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent 是工具返回的内容块。
type ToolContent struct {
	Type string `json:"type"` // "text" / "image" / "resource"
	// Text 内容
	Text string `json:"text,omitempty"`
	// Image 内容
	Data     string `json:"data,omitempty"`     // base64
	MIMEType string `json:"mimeType,omitempty"`
	// Resource 内容
	Resource *ResourceContent `json:"resource,omitempty"`
}

// ResourceContent 是资源类型的工具返回。
type ResourceContent struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// ---------- Client 接口 ----------

// Client 是 MCP 客户端的抽象接口。
//
// 不同的传输方式（SSE、stdio）有不同的实现，
// 但上层只需要这个接口就能调用 MCP 工具。
type Client interface {
	// Connect 连接到 MCP Server，并执行 initialize 握手。
	Connect(ctx context.Context) error
	// Close 关闭连接。
	Close() error
	// ListTools 列出服务端的所有工具。
	ListTools(ctx context.Context) ([]Tool, error)
	// CallTool 调用一个工具。
	CallTool(ctx context.Context, name string, arguments map[string]interface{}) (*CallToolResult, error)
	// ServerInfo 返回服务端信息（名称、版本、能力）。
	ServerInfo() *Implementation
	// Capabilities 返回服务端能力。
	Capabilities() ServerCaps
}

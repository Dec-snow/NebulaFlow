package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hoarfrost/nebulaflow/internal/tool"
)

// ToolAdapter 把 MCP 工具包装成 NebulaFlow 的 tool.Tool 接口。
//
// 这样 MCP 工具就能无缝接入 Tool Registry，
// Agent 调用 MCP 工具和调用内部工具完全一样。
//
// 实现了三个接口：
//   - tool.Tool:            基础工具接口
//   - tool.SchemaProvider:  提供 JSON Schema 参数描述
//   - tool.ArgumentCaller:  支持 function calling 的 JSON 参数
type ToolAdapter struct {
	client   Client
	mcpTool  Tool
	serverName string // 所属 MCP Server 名称，用于工具名前缀避免冲突
}

// ToolAdapterOption 是 ToolAdapter 的配置选项。
type ToolAdapterOption func(*ToolAdapter)

// WithServerName 设置工具所属的 MCP Server 名称，
// 工具名会变成 "servername_toolname"，避免不同 server 的工具重名。
func WithServerName(name string) ToolAdapterOption {
	return func(a *ToolAdapter) {
		a.serverName = name
	}
}

// NewToolAdapter 创建一个 MCP 工具适配器。
func NewToolAdapter(client Client, mcpTool Tool, opts ...ToolAdapterOption) *ToolAdapter {
	a := &ToolAdapter{
		client:  client,
		mcpTool: mcpTool,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Name 返回工具名。
// 如果设置了 serverName，格式为 "servername_toolname"。
func (a *ToolAdapter) Name() string {
	if a.serverName != "" {
		return a.serverName + "_" + a.mcpTool.Name
	}
	return a.mcpTool.Name
}

// Description 返回工具描述（加上 MCP 标识）。
func (a *ToolAdapter) Description() string {
	desc := a.mcpTool.Description
	if desc == "" {
		desc = "MCP tool: " + a.mcpTool.Name
	}
	if a.serverName != "" {
		desc = fmt.Sprintf("[MCP/%s] %s", a.serverName, desc)
	} else {
		desc = "[MCP] " + desc
	}
	return desc
}

// Parameters 返回工具的 JSON Schema（来自 MCP 的 inputSchema）。
func (a *ToolAdapter) Parameters() json.RawMessage {
	if len(a.mcpTool.InputSchema) > 0 {
		return a.mcpTool.InputSchema
	}
	// 兜底：接受任意对象
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// CallWithArguments 以 function calling 的 JSON 参数调用 MCP 工具。
// 这是 Agent 调用 MCP 工具的主要方式。
func (a *ToolAdapter) CallWithArguments(ctx context.Context, args string) (string, error) {
	// 解析 JSON 参数
	var params map[string]interface{}
	if args != "" && strings.TrimSpace(args) != "" {
		s := strings.TrimSpace(args)
		// 如果是合法 JSON 对象，直接解析
		if strings.HasPrefix(s, "{") {
			if err := json.Unmarshal([]byte(s), &params); err != nil {
				// 解析失败，把整个字符串作为单个参数传入
				params = map[string]interface{}{"input": s}
			}
		} else {
			// 不是 JSON，作为 input 字段传入
			params = map[string]interface{}{"input": s}
		}
	}

	result, err := a.client.CallTool(ctx, a.mcpTool.Name, params)
	if err != nil {
		return "", fmt.Errorf("mcp tool %s: %w", a.Name(), err)
	}

	// 把 MCP 的内容块格式化成可读字符串
	return formatToolResult(result), nil
}

// Call 是字符串形式的调用（兼容旧接口）。
// 把参数作为 "input" 字段传给 MCP 工具。
func (a *ToolAdapter) Call(ctx context.Context, arg string) (string, error) {
	return a.CallWithArguments(ctx, arg)
}

// formatToolResult 把 MCP CallToolResult 格式化成可读字符串。
func formatToolResult(result *CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		if result != nil && result.IsError {
			return "Error: tool returned no content"
		}
		return ""
	}

	var sb strings.Builder
	for i, block := range result.Content {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		switch block.Type {
		case "text":
			sb.WriteString(block.Text)
		case "image":
			fmt.Fprintf(&sb, "[Image: %s, %d bytes]", block.MIMEType, len(block.Data))
		case "resource":
			if block.Resource != nil {
				fmt.Fprintf(&sb, "[Resource: %s (%s)]", block.Resource.Name, block.Resource.URI)
			} else {
				sb.WriteString("[Resource]")
			}
		default:
			fmt.Fprintf(&sb, "[%s content]", block.Type)
		}
	}

	if result.IsError {
		return "Error: " + sb.String()
	}
	return sb.String()
}

// 确保 ToolAdapter 实现了所有必要的接口
var (
	_ tool.Tool            = (*ToolAdapter)(nil)
	_ tool.SchemaProvider  = (*ToolAdapter)(nil)
	_ tool.ArgumentCaller  = (*ToolAdapter)(nil)
)

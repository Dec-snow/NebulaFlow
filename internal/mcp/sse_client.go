package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SSEClient 是基于 SSE（Server-Sent Events）的 MCP 客户端。
//
// MCP SSE 传输协议：
//   1. 客户端 GET 请求到 /sse 端点
//   2. 服务端返回 SSE 流，其中有一条 "endpoint" 事件
//      告诉客户端往哪个 URL 发 JSON-RPC 请求
//   3. 客户端 POST JSON-RPC 请求到 endpoint URL
//   4. 服务端通过 SSE 流返回 JSON-RPC 响应
//
// 为什么用 SSE 而不是 WebSocket：
//   - SSE 是单向服务端推送，更简单，兼容 HTTP 基础设施
//   - 请求走普通 POST，容易调试、容易过代理/CDN
//   - 是 MCP 协议官方推荐的远程传输方式
type SSEClient struct {
	// baseURL 是 MCP Server 的基础 URL（不含 /sse）
	baseURL string
	// endpointURL 是发送 JSON-RPC 请求的 URL（从 SSE endpoint 事件获取）
	endpointURL string
	httpClient  *http.Client

	// 服务器信息（initialize 后填充）
	serverInfo   *Implementation
	capabilities ServerCaps
	initialized  bool

	// 请求 ID 生成器
	requestID atomic.Int64

	// 响应通道：request_id → chan Response
	pendingMu sync.Mutex
	pending   map[interface{}]chan Response

	// SSE 连接管理
	sseCancel context.CancelFunc
	sseDone   chan struct{}

	// 通知处理
	notifMu      sync.Mutex
	notifHandler func(Notification)
}

// SSEConfig 是 SSE 客户端配置。
type SSEConfig struct {
	BaseURL string        // MCP Server 基础 URL，如 "http://localhost:3000"
	Timeout time.Duration // 请求超时，默认 30s
}

// NewSSEClient 创建一个 SSE 传输的 MCP 客户端。
func NewSSEClient(cfg SSEConfig) *SSEClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &SSEClient{
		baseURL:    trimSlash(cfg.BaseURL),
		httpClient: &http.Client{Timeout: timeout},
		pending:    make(map[interface{}]chan Response),
		sseDone:    make(chan struct{}),
	}
}

// OnNotification 设置通知处理器。
func (c *SSEClient) OnNotification(h func(Notification)) {
	c.notifMu.Lock()
	c.notifHandler = h
	c.notifMu.Unlock()
}

// Connect 连接到 MCP Server，建立 SSE 流并执行 initialize 握手。
func (c *SSEClient) Connect(ctx context.Context) error {
	// 1. 建立 SSE 连接
	sseCtx, cancel := context.WithCancel(ctx)
	c.sseCancel = cancel

	req, err := http.NewRequestWithContext(sseCtx, "GET", c.baseURL+"/sse", nil)
	if err != nil {
		cancel()
		return fmt.Errorf("mcp sse: create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("mcp sse: connect: %w", err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		cancel()
		return fmt.Errorf("mcp sse: unexpected status %d", resp.StatusCode)
	}

	// 2. 启动 SSE 读取协程
	go c.readSSE(sseCtx, resp.Body)

	// 3. 等待 endpoint 事件（告诉我们 POST 到哪里）
	//    给个超时，避免服务端异常时卡死
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()

	// 循环等 endpointURL 被设置
	for {
		select {
		case <-waitCtx.Done():
			cancel()
			return fmt.Errorf("mcp sse: timeout waiting for endpoint event")
		case <-c.sseDone:
			cancel()
			return fmt.Errorf("mcp sse: connection closed before endpoint event")
		default:
			if c.endpointURL != "" {
				goto gotEndpoint
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
gotEndpoint:

	// 4. 发送 initialize 请求
	initResult, err := c.initialize(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("mcp initialize: %w", err)
	}
	c.serverInfo = &initResult.ServerInfo
	c.capabilities = initResult.Capabilities
	c.initialized = true

	// 5. 发送 initialized 通知
	_ = c.sendNotification(ctx, "notifications/initialized", nil)

	return nil
}

// initialize 发送 initialize 请求。
func (c *SSEClient) initialize(ctx context.Context) (*InitializeResult, error) {
	params := InitializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo: Implementation{
			Name:    "nebulaflow",
			Version: "1.0.0",
		},
		Capabilities: ClientCaps{},
	}
	paramsJSON, _ := json.Marshal(params)

	resp, err := c.sendRequest(ctx, "initialize", paramsJSON)
	if err != nil {
		return nil, err
	}
	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse initialize result: %w", err)
	}
	return &result, nil
}

// ListTools 列出服务端的所有工具。
func (c *SSEClient) ListTools(ctx context.Context) ([]Tool, error) {
	if !c.initialized {
		return nil, fmt.Errorf("mcp: not connected")
	}
	resp, err := c.sendRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var result ListToolsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse tools/list result: %w", err)
	}
	return result.Tools, nil
}

// CallTool 调用一个工具。
func (c *SSEClient) CallTool(ctx context.Context, name string, arguments map[string]interface{}) (*CallToolResult, error) {
	if !c.initialized {
		return nil, fmt.Errorf("mcp: not connected")
	}
	params := CallToolParams{Name: name, Arguments: arguments}
	paramsJSON, _ := json.Marshal(params)

	resp, err := c.sendRequest(ctx, "tools/call", paramsJSON)
	if err != nil {
		return nil, err
	}
	var result CallToolResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse tools/call result: %w", err)
	}
	return &result, nil
}

// ServerInfo 返回服务端信息。
func (c *SSEClient) ServerInfo() *Implementation {
	return c.serverInfo
}

// Capabilities 返回服务端能力。
func (c *SSEClient) Capabilities() ServerCaps {
	return c.capabilities
}

// Close 关闭连接。
func (c *SSEClient) Close() error {
	if c.sseCancel != nil {
		c.sseCancel()
	}
	// 关闭所有 pending 的响应通道
	c.pendingMu.Lock()
	for _, ch := range c.pending {
		close(ch)
	}
	c.pending = make(map[interface{}]chan Response)
	c.pendingMu.Unlock()
	return nil
}

// ---------- 内部方法 ----------

// sendRequest 发送一个 JSON-RPC 请求，等待响应。
func (c *SSEClient) sendRequest(ctx context.Context, method string, params json.RawMessage) (*Response, error) {
	id := c.requestID.Add(1)

	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	reqBody, _ := json.Marshal(req)

	// 创建响应通道
	respCh := make(chan Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	// POST 到 endpoint
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("mcp request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp request: HTTP %d", httpResp.StatusCode)
	}

	// 同步模式：如果服务端直接在 HTTP 响应里返回结果（某些实现支持），直接用
	var directResp Response
	if err := json.NewDecoder(httpResp.Body).Decode(&directResp); err == nil && directResp.JSONRPC == "2.0" {
		if directResp.Error != nil {
			return nil, directResp.Error
		}
		return &directResp, nil
	}

	// 异步模式：等待 SSE 流里的响应
	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, fmt.Errorf("mcp: response channel closed")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return &resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// sendNotification 发送一个 JSON-RPC 通知（不需要响应）。
func (c *SSEClient) sendNotification(ctx context.Context, method string, params json.RawMessage) error {
	notif := Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	body, _ := json.Marshal(notif)

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpointURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// readSSE 读取 SSE 流，处理每个事件。
func (c *SSEClient) readSSE(ctx context.Context, body io.ReadCloser) {
	defer body.Close()
	defer close(c.sseDone)

	scanner := bufio.NewScanner(body)
	// SSE 事件可能很长，把缓冲调大
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType string
	var dataBuf strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// 空行表示事件结束
			if eventType != "" && dataBuf.Len() > 0 {
				c.handleSSEEvent(ctx, eventType, dataBuf.String())
			}
			eventType = ""
			dataBuf.Reset()
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(data)
		}
	}
}

// handleSSEEvent 处理一个 SSE 事件。
func (c *SSEClient) handleSSEEvent(_ context.Context, eventType, data string) {
	switch eventType {
	case "endpoint":
		// endpoint 事件：告诉我们 POST 请求应该发到哪里
		c.endpointURL = data

	case "message":
		// message 事件：JSON-RPC 响应或通知
		var resp Response
		if err := json.Unmarshal([]byte(data), &resp); err == nil && resp.ID != nil {
			// 是响应，转给对应的 pending channel
			c.pendingMu.Lock()
			ch, ok := c.pending[resp.ID]
			c.pendingMu.Unlock()
			if ok {
				select {
				case ch <- resp:
				default:
				}
			}
			return
		}

		// 可能是通知
		var notif Notification
		if err := json.Unmarshal([]byte(data), &notif); err == nil && notif.Method != "" {
			c.dispatchNotification(notif)
		}
	}
}

// dispatchNotification 分发通知。
func (c *SSEClient) dispatchNotification(notif Notification) {
	c.notifMu.Lock()
	h := c.notifHandler
	c.notifMu.Unlock()
	if h != nil {
		h(notif)
	}
}

// trimSlash 去掉 URL 末尾的斜杠。
func trimSlash(url string) string {
	return strings.TrimRight(url, "/")
}

package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hoarfrost/nebulaflow/internal/tool"
)

// ServerConfig 是一个 MCP Server 的配置。
type ServerConfig struct {
	Name    string `json:"name"`     // 标识名，用作工具前缀
	BaseURL string `json:"base_url"` // SSE 模式的基础 URL
	// Transport 传输方式："sse"（目前只支持 SSE，未来可加 "stdio"）
	Transport string `json:"transport,omitempty"`
	// 是否在启动时自动连接
	AutoConnect bool `json:"auto_connect,omitempty"`
}

// ServerManager 管理多个 MCP Server 连接。
//
// 职责：
//   - 维护 MCP Server 配置
//   - 管理连接生命周期（连接/断开/重连）
//   - 把所有 MCP 工具注册到 Tool Registry
//   - 提供工具查找
//
// 设计类似 Nginx 的 upstream 管理：
// 配置好多个上游 MCP Server，Manager 负责连接和维护，
// 上层只需要"有哪些工具可用"，不需要关心有几个 MCP Server。
type ServerManager struct {
	registry *tool.Registry
	servers  map[string]*managedServer
	mu       sync.RWMutex
	logger   *slog.Logger
}

type managedServer struct {
	config ServerConfig
	client Client
	tools  []*ToolAdapter
	connected bool
}

// NewServerManager 创建一个 MCP Server 管理器。
func NewServerManager(registry *tool.Registry, logger *slog.Logger) *ServerManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &ServerManager{
		registry: registry,
		servers:  make(map[string]*managedServer),
		logger:   logger,
	}
}

// AddServer 添加一个 MCP Server 配置（不自动连接）。
func (m *ServerManager) AddServer(cfg ServerConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("mcp server name is required")
	}
	if cfg.BaseURL == "" {
		return fmt.Errorf("mcp server base_url is required")
	}
	if cfg.Transport == "" {
		cfg.Transport = "sse"
	}
	if cfg.Transport != "sse" {
		return fmt.Errorf("unsupported mcp transport: %s (only 'sse' supported)", cfg.Transport)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.servers[cfg.Name]; exists {
		return fmt.Errorf("mcp server %q already exists", cfg.Name)
	}

	ms := &managedServer{config: cfg}

	// 创建客户端（不连接）
	switch cfg.Transport {
	case "sse":
		ms.client = NewSSEClient(SSEConfig{BaseURL: cfg.BaseURL})
	}

	m.servers[cfg.Name] = ms
	return nil
}

// ConnectAll 连接所有配置了 auto_connect 的 MCP Server。
// 某个 server 连接失败不影响其他的，记录错误继续。
func (m *ServerManager) ConnectAll(ctx context.Context) int {
	m.mu.RLock()
	servers := make([]*managedServer, 0, len(m.servers))
	for _, s := range m.servers {
		if s.config.AutoConnect {
			servers = append(servers, s)
		}
	}
	m.mu.RUnlock()

	connected := 0
	for _, s := range servers {
		if err := m.connectServer(ctx, s); err != nil {
			m.logger.Error("mcp: failed to connect server",
				"server", s.config.Name, "error", err)
		} else {
			connected++
		}
	}
	return connected
}

// Connect 连接指定的 MCP Server。
func (m *ServerManager) Connect(ctx context.Context, name string) error {
	m.mu.RLock()
	s, ok := m.servers[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("mcp server %q not found", name)
	}
	return m.connectServer(ctx, s)
}

// connectServer 连接一个 MCP Server 并注册它的所有工具。
func (m *ServerManager) connectServer(ctx context.Context, s *managedServer) error {
	if err := s.client.Connect(ctx); err != nil {
		return err
	}

	tools, err := s.client.ListTools(ctx)
	if err != nil {
		s.client.Close()
		return fmt.Errorf("list tools: %w", err)
	}

	// 为每个 MCP 工具创建适配器并注册到 Tool Registry
	s.tools = make([]*ToolAdapter, 0, len(tools))
	for _, t := range tools {
		adapter := NewToolAdapter(s.client, t, WithServerName(s.config.Name))
		s.tools = append(s.tools, adapter)
		m.registry.Register(adapter)
	}

	s.connected = true
	m.logger.Info("mcp: server connected",
		"server", s.config.Name,
		"tools", len(tools),
		"server_info", s.client.ServerInfo().Name,
	)
	return nil
}

// Disconnect 断开指定的 MCP Server（注销其工具）。
func (m *ServerManager) Disconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.servers[name]
	if !ok {
		return fmt.Errorf("mcp server %q not found", name)
	}
	if !s.connected {
		return nil
	}

	s.client.Close()
	s.connected = false

	// 注意：Tool Registry 没有注销功能（设计上是只增不减的），
	// 断开连接后工具仍然在 Registry 里，但调用会失败。
	// 这是故意的——工作流可能引用了这些工具，不能因为
	// MCP Server 挂了就让工作流定义失效。

	m.logger.Info("mcp: server disconnected", "server", name)
	return nil
}

// ListServers 列出所有 MCP Server 及其状态。
func (m *ServerManager) ListServers() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]ServerStatus, 0, len(m.servers))
	for _, s := range m.servers {
		status := ServerStatus{
			Name:      s.config.Name,
			Transport: s.config.Transport,
			Connected: s.connected,
			ToolCount: len(s.tools),
		}
		if s.connected && s.client.ServerInfo() != nil {
			status.ServerInfo = s.client.ServerInfo().Name
			status.ServerVersion = s.client.ServerInfo().Version
		}
		out = append(out, status)
	}
	return out
}

// GetServer 获取指定 Server 的客户端（用于高级操作）。
func (m *ServerManager) GetServer(name string) (Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.servers[name]
	if !ok || !s.connected {
		return nil, false
	}
	return s.client, true
}

// ServerStatus 是 MCP Server 的状态信息。
type ServerStatus struct {
	Name          string `json:"name"`
	Transport     string `json:"transport"`
	Connected     bool   `json:"connected"`
	ToolCount     int    `json:"tool_count"`
	ServerInfo    string `json:"server_info,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
}

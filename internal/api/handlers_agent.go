package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/agent"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// agentHandler 处理 Agent Registry 的 CRUD + 健康检查 + 能力发现 API。
//
// 端点设计参考 Kubernetes API 风格：
//   GET    /api/agents           列表（支持 ?runtime_type= &capability= 过滤）
//   POST   /api/agents           注册新 Agent
//   GET    /api/agents/:id       详情
//   PUT    /api/agents/:id       更新
//   DELETE /api/agents/:id       删除
//   GET    /api/agents/:id/health 健康检查
//   GET    /api/agents/capabilities 能力列表（用于前端筛选器）
type agentHandler struct {
	registry *agent.Registry
}

// agentPayload 是注册/更新 Agent 的请求体。
type agentPayload struct {
	Name         string   `json:"name" binding:"required"`
	Description  string   `json:"description"`
	RuntimeType  string   `json:"runtime_type" binding:"required"`
	Endpoint     string   `json:"endpoint"`
	Model        string   `json:"model"`
	Capabilities []string `json:"capabilities"`
	Version      string   `json:"version"`
	TimeoutSec   int      `json:"timeout_sec"`
	Status       string   `json:"status"`
}

// GET /api/agents?runtime_type=&capability=
func (h *agentHandler) list(c *gin.Context) {
	u := getUser(c)
	rt := model.AgentRuntimeType(c.Query("runtime_type"))
	cap := c.Query("capability")

	agents, err := h.registry.List(c.Request.Context(), u.ID, rt, cap)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"agents": agents, "total": len(agents)})
}

// POST /api/agents
func (h *agentHandler) create(c *gin.Context) {
	u := getUser(c)
	var p agentPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	a := &model.AgentRegistry{
		UserID:       u.ID,
		Name:         p.Name,
		Description:  p.Description,
		RuntimeType:  model.AgentRuntimeType(p.RuntimeType),
		Endpoint:     p.Endpoint,
		Model:        p.Model,
		Capabilities: p.Capabilities,
		Version:      p.Version,
		TimeoutSec:   p.TimeoutSec,
		Status:       model.AgentStatus(p.Status),
	}

	if err := h.registry.Register(c.Request.Context(), a); err != nil {
		// 不支持的 runtime_type 返回 400
		if strings.Contains(err.Error(), "unsupported runtime type") {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusCreated, a)
}

// GET /api/agents/:id
func (h *agentHandler) get(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)

	a, err := h.registry.Get(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, err)
		return
	}
	// 权限校验：只能看自己的 Agent（系统内置的 user_id=0 所有人可见）
	if a.UserID != 0 && a.UserID != u.ID {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		return
	}
	c.JSON(http.StatusOK, a)
}

// PUT /api/agents/:id
func (h *agentHandler) update(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)

	// 先查现有，做归属校验
	existing, err := h.registry.Get(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, err)
		return
	}
	if existing.UserID != u.ID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not your agent"})
		return
	}

	var p agentPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 更新字段（部分更新语义）
	existing.Name = p.Name
	existing.Description = p.Description
	if p.RuntimeType != "" {
		existing.RuntimeType = model.AgentRuntimeType(p.RuntimeType)
	}
	existing.Endpoint = p.Endpoint
	existing.Model = p.Model
	existing.Capabilities = p.Capabilities
	if p.Version != "" {
		existing.Version = p.Version
	}
	existing.TimeoutSec = p.TimeoutSec
	if p.Status != "" {
		existing.Status = model.AgentStatus(p.Status)
	}

	if err := h.registry.Update(c.Request.Context(), existing); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, existing)
}

// DELETE /api/agents/:id
func (h *agentHandler) delete(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)

	// 归属校验
	existing, err := h.registry.Get(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, err)
		return
	}
	if existing.UserID != u.ID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not your agent"})
		return
	}

	if err := h.registry.Delete(c.Request.Context(), id); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// GET /api/agents/:id/health —— 健康检查
//
// 调用 Runtime 的 HealthCheck 方法，返回健康状态 + 元数据快照。
// 前端可以用这个接口做 Agent 状态面板。
func (h *agentHandler) health(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)

	a, err := h.registry.Get(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, err)
		return
	}
	if a.UserID != 0 && a.UserID != u.ID {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "agent not found"})
		return
	}

	rt, err := h.registry.GetRuntime(c.Request.Context(), id)
	if err != nil {
		// Runtime 创建失败也是不健康
		c.JSON(http.StatusOK, gin.H{
			"agent_id": id,
			"healthy":  false,
			"error":    err.Error(),
		})
		return
	}

	// 执行健康检查
	err = rt.HealthCheck(c.Request.Context())
	md := agent.MetadataWithHealth(c.Request.Context(), rt)

	c.JSON(http.StatusOK, gin.H{
		"agent_id":    id,
		"healthy":     md.Healthy,
		"error":       "",
		"metadata":    md,
	})
}

// GET /api/agents/capabilities —— 能力列表
//
// 返回所有标准能力标签，前端用于渲染筛选器和 Agent 配置表单的下拉选项。
func (h *agentHandler) capabilities(c *gin.Context) {
	caps := []gin.H{
		{"value": string(agent.CapToolCall), "label": "工具调用 (Tool Call)"},
		{"value": string(agent.CapRAG), "label": "知识检索 (RAG)"},
		{"value": string(agent.CapMemory), "label": "记忆 (Memory)"},
		{"value": string(agent.CapStreaming), "label": "流式输出 (Streaming)"},
		{"value": string(agent.CapCode), "label": "代码生成 (Code)"},
		{"value": string(agent.CapSearch), "label": "联网搜索 (Search)"},
		{"value": string(agent.CapMultimodal), "label": "多模态 (Multimodal)"},
	}
	c.JSON(http.StatusOK, gin.H{"capabilities": caps})
}

// GET /api/agents/runtime-types —— Runtime 类型列表
//
// 返回支持的 Runtime 类型，前端用于注册 Agent 时的类型选择。
func (h *agentHandler) runtimeTypes(c *gin.Context) {
	types := []gin.H{
		{"value": string(model.RuntimeNative), "label": "Native（自研 Agent）"},
		{"value": string(model.RuntimeLangChain), "label": "LangChain（LangServe HTTP）"},
		{"value": string(model.RuntimeHTTP), "label": "HTTP（通用 HTTP Agent）"},
	}
	c.JSON(http.StatusOK, gin.H{"runtime_types": types})
}

// GET /api/agents/discover?capability=xxx —— 按能力发现 Agent
//
// 能力发现接口：传入一个能力标签，返回所有具备该能力的活跃 Agent。
// 用于 Multi-Agent 协作场景：Supervisor 通过这个接口找到合适的子 Agent。
func (h *agentHandler) discover(c *gin.Context) {
	u := getUser(c)
	cap := c.Query("capability")
	if cap == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "capability parameter is required"})
		return
	}

	agents, err := h.registry.FindByCapability(c.Request.Context(), u.ID, cap)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"agents": agents, "total": len(agents), "capability": cap})
}

// ---------- Agent 市场 ----------

// GET /api/agents/marketplace —— Agent 市场列表（系统内置 Agent）
func (h *agentHandler) marketplaceList(c *gin.Context) {
	u := getUser(c)
	rt := model.AgentRuntimeType(c.Query("runtime_type"))
	cap := c.Query("capability")

	agents, err := h.registry.ListMarketplace(c.Request.Context(), rt, cap)
	if err != nil {
		abortWithError(c, err)
		return
	}

	// 检查用户是否已安装（用于前端展示"已安装"标记）
	// 简单实现：查用户自己的 agent 名字列表
	userAgents, err := h.registry.List(c.Request.Context(), u.ID, "", "")
	if err != nil {
		abortWithError(c, err)
		return
	}
	installedNames := make(map[string]bool)
	for _, a := range userAgents {
		if a.UserID == u.ID {
			installedNames[a.Name] = true
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"agents":          agents,
		"total":           len(agents),
		"installed_names": installedNames,
	})
}

// GET /api/agents/marketplace/:id —— 市场 Agent 详情
func (h *agentHandler) marketplaceDetail(c *gin.Context) {
	id := mustID(c)

	a, err := h.registry.Get(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, err)
		return
	}
	// 市场详情只返回系统内置 Agent
	if a.UserID != 0 {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "agent not found in marketplace"})
		return
	}
	c.JSON(http.StatusOK, a)
}

// POST /api/agents/marketplace/:id/install —— 一键安装 Agent 到用户注册中心
func (h *agentHandler) marketplaceInstall(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)

	installed, err := h.registry.InstallAgent(c.Request.Context(), id, u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"agent_id": installed.ID,
		"name":     installed.Name,
		"message":  "Agent 已安装到你的注册中心",
	})
}


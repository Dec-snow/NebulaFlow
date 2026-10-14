package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/scheduler"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

type workflowHandler struct {
	store *store.Store
}

// workflowPayload 是前端保存工作流的请求体（React Flow 直接产出）。
type workflowPayload struct {
	Name        string        `json:"name" binding:"required"`
	Description string        `json:"description"`
	Status      string        `json:"status"`
	Nodes       []nodePayload `json:"nodes"`
	Edges       []edgePayload `json:"edges"`
}

type nodePayload struct {
	Key    string           `json:"key" binding:"required"`
	Type   string           `json:"type" binding:"required"`
	Config model.NodeConfig `json:"config"`
	X      float64          `json:"x"`
	Y      float64          `json:"y"`
}

type edgePayload struct {
	Source string `json:"source" binding:"required"`
	Target string `json:"target" binding:"required"`
}

// POST /api/workflows
func (h *workflowHandler) create(c *gin.Context) {
	u := getUser(c)
	var p workflowPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	wf := model.Workflow{
		UserID:      u.ID,
		Name:        p.Name,
		Description: p.Description,
		Status:      model.WorkflowStatus(orDefault(p.Status, "draft")),
	}
	for _, n := range p.Nodes {
		wf.Nodes = append(wf.Nodes, model.WorkflowNode{
			NodeKey: n.Key, NodeType: model.NodeType(n.Type),
			Config: n.Config, PositionX: n.X, PositionY: n.Y,
		})
	}
	for _, e := range p.Edges {
		wf.Edges = append(wf.Edges, model.WorkflowEdge{
			SourceNode: e.Source, TargetNode: e.Target,
		})
	}
	// 保存前校验 DAG 合法性（环/悬挂边）
	if _, err := scheduler.BuildDAG(wf.Nodes, wf.Edges); err != nil {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": "invalid workflow: " + err.Error()})
		return
	}
	if err := h.store.Workflows.CreateWorkflow(c.Request.Context(), &wf); err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusCreated, wf)
}

// GET /api/workflows
func (h *workflowHandler) list(c *gin.Context) {
	u := getUser(c)
	list, err := h.store.Workflows.ListWorkflows(c.Request.Context(), u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"workflows": list})
}

// GET /api/workflows/:id
func (h *workflowHandler) get(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	wf, err := h.store.Workflows.GetWorkflow(c.Request.Context(), id, u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, wf)
}

// PUT /api/workflows/:id
func (h *workflowHandler) update(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	var p workflowPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	wf := model.Workflow{
		ID: id, UserID: u.ID,
		Name: p.Name, Description: p.Description,
		Status: model.WorkflowStatus(orDefault(p.Status, "draft")),
	}
	for _, n := range p.Nodes {
		wf.Nodes = append(wf.Nodes, model.WorkflowNode{
			NodeKey: n.Key, NodeType: model.NodeType(n.Type),
			Config: n.Config, PositionX: n.X, PositionY: n.Y,
		})
	}
	for _, e := range p.Edges {
		wf.Edges = append(wf.Edges, model.WorkflowEdge{SourceNode: e.Source, TargetNode: e.Target})
	}
	// 保存前校验 DAG 合法性
	if _, err := scheduler.BuildDAG(wf.Nodes, wf.Edges); err != nil {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": "invalid workflow: " + err.Error()})
		return
	}
	if err := h.store.Workflows.UpdateWorkflow(c.Request.Context(), &wf); err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, wf)
}

// DELETE /api/workflows/:id
func (h *workflowHandler) delete(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	if err := h.store.Workflows.DeleteWorkflow(c.Request.Context(), id, u.ID); err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"deleted": id})
}

// ---------- 模板市场 ----------

// GET /api/templates —— 模板列表（支持按分类过滤）
func (h *workflowHandler) listTemplates(c *gin.Context) {
	u := getUser(c)
	category := c.Query("category")
	list, err := h.store.Workflows.ListTemplates(c.Request.Context(), u.ID, category)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"templates": list, "total": len(list)})
}

// GET /api/templates/:id —— 模板详情（含节点/边，用于预览）
func (h *workflowHandler) getTemplate(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	if c.IsAborted() {
		return
	}
	wf, err := h.store.Workflows.GetTemplate(c.Request.Context(), id, u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, wf)
}

// POST /api/templates/:id/use —— 从模板创建工作流（一键使用）
// 返回新建的 workflow ID，前端可以跳转到编辑器。
func (h *workflowHandler) useTemplate(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)

	var body struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&body) // name 可选，空则用默认名

	newWF, err := h.store.Workflows.CreateFromTemplate(c.Request.Context(), id, u.ID, body.Name)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusCreated, gin.H{
		"workflow_id": newWF.ID,
		"name":        newWF.Name,
		"message":     "已从模板创建工作流",
	})
}

// GET /api/templates/categories —— 模板分类列表
// 返回用户可见模板的所有分类（去重），用于前端分类筛选器。
func (h *workflowHandler) listCategories(c *gin.Context) {
	u := getUser(c)
	categories, err := h.store.Workflows.ListTemplateCategories(c.Request.Context(), u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, gin.H{"categories": categories})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// mustID 解析 `:id` 路径参数。
//
// 解析失败时写 400 并返回 0 —— 注意**调用方仍然会继续往下执行**
// （Go 里没法让被调函数提前结束调用方），所以真正的防线是响应写出的守卫
// （见 middleware.go 的 writeJSON / abortJSON）：第一个响应写出之后，
// 后续任何写入都会被丢弃，body 里永远只有一个 JSON。
func mustID(c *gin.Context) int64 {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": "invalid id parameter"})
		return 0
	}
	return id
}

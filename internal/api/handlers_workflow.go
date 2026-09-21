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
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid workflow: " + err.Error()})
		return
	}
	if err := h.store.Workflows.CreateWorkflow(c.Request.Context(), &wf); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusCreated, wf)
}

// GET /api/workflows
func (h *workflowHandler) list(c *gin.Context) {
	u := getUser(c)
	list, err := h.store.Workflows.ListWorkflows(c.Request.Context(), u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"workflows": list})
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
	c.JSON(http.StatusOK, wf)
}

// PUT /api/workflows/:id
func (h *workflowHandler) update(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	var p workflowPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid workflow: " + err.Error()})
		return
	}
	if err := h.store.Workflows.UpdateWorkflow(c.Request.Context(), &wf); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, wf)
}

// DELETE /api/workflows/:id
func (h *workflowHandler) delete(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	if err := h.store.Workflows.DeleteWorkflow(c.Request.Context(), id, u.ID); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": id})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func mustID(c *gin.Context) int64 {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	return id
}

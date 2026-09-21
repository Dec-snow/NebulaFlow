package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

type providerHandler struct {
	store *store.Store
}

// GET /api/providers —— 列出所有模型 Provider（含模型列表）
func (h *providerHandler) list(c *gin.Context) {
	providers, err := h.store.Providers.ListProviders(c.Request.Context())
	if err != nil {
		abortWithError(c, err)
		return
	}
	type providerResp struct {
		model.LLMProvider
		Models []model.LLMModel `json:"models"`
	}
	out := make([]providerResp, 0, len(providers))
	for i := range providers {
		p := providers[i]
		p.APIKey = maskKey(p.APIKey) // 不回传明文密钥
		models, err := h.store.Providers.ListModels(c.Request.Context(), p.ID)
		if err != nil {
			models = nil
		}
		out = append(out, providerResp{LLMProvider: p, Models: models})
	}
	c.JSON(http.StatusOK, gin.H{"providers": out})
}

// PUT /api/providers/:id —— 更新 Provider 配置
func (h *providerHandler) update(c *gin.Context) {
	id := mustID(c)
	var req struct {
		BaseURL   string `json:"base_url"`
		APIKey    string `json:"api_key"`
		IsDefault bool   `json:"is_default"`
		Enabled   *bool  `json:"enabled"`
		Priority  int    `json:"priority"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p, err := h.store.Providers.GetProvider(c.Request.Context(), id)
	if err != nil {
		abortWithError(c, err)
		return
	}
	if req.BaseURL != "" {
		p.BaseURL = req.BaseURL
	}
	if req.APIKey != "" {
		p.APIKey = req.APIKey
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}
	if req.IsDefault {
		p.IsDefault = true
	}
	if req.Priority > 0 {
		p.Priority = req.Priority
	}
	if err := h.store.Providers.UpsertProvider(c.Request.Context(), p); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// POST /api/providers —— 新增 Provider
func (h *providerHandler) create(c *gin.Context) {
	var req struct {
		Name     string `json:"name" binding:"required"`
		BaseURL  string `json:"base_url" binding:"required"`
		APIKey   string `json:"api_key"`
		Priority int    `json:"priority"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p := &model.LLMProvider{
		Name: req.Name, BaseURL: req.BaseURL, APIKey: req.APIKey,
		Enabled: true, Priority: req.Priority,
	}
	if p.Priority == 0 {
		p.Priority = 100
	}
	if err := h.store.Providers.UpsertProvider(c.Request.Context(), p); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

// DELETE /api/providers/:id
func (h *providerHandler) delete(c *gin.Context) {
	id := mustID(c)
	if err := h.store.Providers.DeleteProvider(c.Request.Context(), id); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": id})
}

// POST /api/providers/:id/models —— 给 Provider 添加模型
func (h *providerHandler) addModel(c *gin.Context) {
	providerID := mustID(c)
	var req struct {
		Name      string `json:"name" binding:"required"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, err := h.store.Providers.GetProvider(c.Request.Context(), providerID); err != nil {
		abortWithError(c, err)
		return
	}
	m := &model.LLMModel{ProviderID: providerID, Name: req.Name, MaxTokens: req.MaxTokens}
	if m.MaxTokens == 0 {
		m.MaxTokens = 4096
	}
	if err := h.store.Providers.UpsertModel(c.Request.Context(), m); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusCreated, m)
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

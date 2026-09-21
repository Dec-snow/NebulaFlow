package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/auth"
)

type authHandler struct {
	svc *auth.Service
}

type registerReq struct {
	Username string `json:"username" binding:"required,min=3,max=32"`
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=6"`
}

type loginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type tokenResp struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	UserID   int64  `json:"user_id"`
}

// POST /api/auth/register
func (h *authHandler) register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, err := h.svc.Register(c.Request.Context(), req.Username, req.Email, req.Password)
	if err != nil {
		abortWithError(c, err)
		return
	}
	token, err := h.svc.Issue(u)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusCreated, tokenResp{Token: token, Username: u.Username, UserID: u.ID})
}

// POST /api/auth/login
func (h *authHandler) login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token, u, err := h.svc.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, tokenResp{Token: token, Username: u.Username, UserID: u.ID})
}

// GET /api/auth/me
func (h *authHandler) me(c *gin.Context) {
	u := getUser(c)
	c.JSON(http.StatusOK, gin.H{"id": u.ID, "username": u.Username})
}

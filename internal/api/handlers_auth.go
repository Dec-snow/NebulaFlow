package api

import (
	"net/http"
	"strconv"

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
		abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if msg := passwordLengthError(req.Password); msg != "" {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": msg})
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
	writeJSON(c, http.StatusCreated, tokenResp{Token: token, Username: u.Username, UserID: u.ID})
}

// POST /api/auth/login
func (h *authHandler) login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if msg := passwordLengthError(req.Password); msg != "" {
		abortJSON(c, http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	token, u, err := h.svc.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		abortWithError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, tokenResp{Token: token, Username: u.Username, UserID: u.ID})
}

// passwordLengthError 做请求层的密码长度上限校验，合法时返回空串。
//
// **必须按字节数而不是字符数**：binding 标签里的 `min=6` 是字符语义
// （validator 对 string 用 utf8.RuneCountInString），而 bcrypt 的上限
// 是 72 **字节**。30 个中文字符只有 30 个字符却有 90 字节——
// 用字符数判上限会放行，然后挂在 bcrypt 上返回 500，
// 客户端拿到的是库内文案 "bcrypt: password length exceeds 72 bytes"。
//
// 校验放在请求层（而不是只依赖服务层）是为了让 400 的文案是**我们自己的**：
// 用户需要知道"密码太长了"，而不是知道"我们用 bcrypt、它的缓冲区是 72 字节"。
func passwordLengthError(password string) string {
	if len(password) > auth.MaxPasswordBytes {
		return "password must be at most " + strconv.Itoa(auth.MaxPasswordBytes) +
			" bytes (got " + strconv.Itoa(len(password)) + ")"
	}
	return ""
}

// GET /api/auth/me
func (h *authHandler) me(c *gin.Context) {
	u := getUser(c)
	writeJSON(c, http.StatusOK, gin.H{"id": u.ID, "username": u.Username})
}

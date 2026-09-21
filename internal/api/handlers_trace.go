package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/task"
)

// traceHandler 提供调用链查询。
//
// 查询后端是进程内的 span 环形缓冲（见 tracing.SpanStore），
// 因此**多实例部署时每个实例只能看到自己那部分 span**——这不是缺陷而是取舍：
// 本机没有 Jaeger/Tempo，把"能不能查"绑定在外部服务上就等于把
// "真做出来了"降级成"看起来做出来了"。生产环境应配上 OTLP endpoint，
// 由 collector 负责汇聚（本包两条路都支持，见 tracing.Config.OTLPEndpoint）。
type traceHandler struct {
	svc     *task.Service
	tracing *tracing.Provider
}

func (h *traceHandler) store() *tracing.SpanStore {
	if h.tracing == nil || !h.tracing.Enabled() {
		return nil
	}
	return h.tracing.Store()
}

// requireUser 取当前登录用户；未认证时写 401 并返回 false。
//
// 这些路由本来就挂在 authMiddleware 之后，理论上取不到用户是不可能的。
// 但"不可能"这个前提是靠路由配置维持的（以后有人把某条 trace 路由挪到免认证分组
// 就破了），而这里一旦拿到 nil 就是解引用 panic——那是 500 而不是 401，
// 排查方向会从一开始就错。用一个显式检查换掉这个隐患很划算。
func requireUser(c *gin.Context) (*ctxUser, bool) {
	u := getUser(c)
	if u == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return nil, false
	}
	return u, true
}

// GET /api/tasks/:id/trace —— 按任务查调用链（最常用的入口）
//
// 先校验任务归属再看 trace，复用任务查询而不是自己判权限：
// 权限判断只写一处，就不会出现"某个新接口忘了判"的经典问题。
func (h *traceHandler) byTask(c *gin.Context) {
	store := h.store()
	if store == nil {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "tracing is not enabled"})
		return
	}
	u, ok := requireUser(c)
	if !ok {
		return
	}
	id := mustID(c)
	if _, err := h.svc.GetTask(c.Request.Context(), id, u.ID); err != nil {
		abortWithError(c, err)
		return
	}
	traceID, ok := store.TraceForTask(id)
	if !ok {
		// 任务还没被 worker 取走时确实没有 trace，这是正常状态而不是错误。
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "no trace recorded for this task yet (it may still be queued)",
		})
		return
	}
	h.write(c, store, traceID, u)
}

// GET /api/traces/:trace_id —— 按 trace id 查（日志/响应头里拿到 id 时用）
func (h *traceHandler) byID(c *gin.Context) {
	store := h.store()
	if store == nil {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "tracing is not enabled"})
		return
	}
	u, ok := requireUser(c)
	if !ok {
		return
	}
	h.write(c, store, c.Param("trace_id"), u)
}

// GET /api/traces?limit= —— 最近的调用链（不含 span 明细）
func (h *traceHandler) list(c *gin.Context) {
	store := h.store()
	if store == nil {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "tracing is not enabled"})
		return
	}
	u, ok := requireUser(c)
	if !ok {
		return
	}
	limit := parseQueryInt(c, "limit", 20)
	c.JSON(http.StatusOK, gin.H{
		"traces": store.Recent(limit, u.ID),
		"store":  store.Stats(),
	})
}

// write 做归属校验并输出 span 树。
func (h *traceHandler) write(c *gin.Context, store *tracing.SpanStore, traceID string, u *ctxUser) {
	owner, known := store.Owner(traceID)
	// 严格按 owner 匹配（fail-closed）：owner 为 0 表示这条 trace 从未被关联到用户，
	// 此时**拒绝**而不是放行——放行等于给所有已登录用户开了一个读任意 trace 的口子，
	// 而 span 里带着原始错误文本（可能含 SQL、表名、内网地址）。
	if !known || owner != u.ID {
		// 用 404 而不是 403：403 等于确认"这条 trace 存在，只是不属于你"，
		// 那就把 trace id 变成了一个可枚举的存在性预言机。
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "trace not found"})
		return
	}
	t, ok := store.Snapshot(traceID)
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "trace not found"})
		return
	}
	c.Header("X-Trace-Span-Count", strconv.Itoa(t.SpanCount))
	c.JSON(http.StatusOK, t)
}

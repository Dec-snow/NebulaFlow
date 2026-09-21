// Package api 实现 REST API 层：路由、中间件与各领域 handler。
package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/ratelimit"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
)

// userKey 用于在 gin.Context 中传递认证用户。
const userKey = "nebulaflow.user"

// ctxUser 是认证中间件解析出的用户信息。
type ctxUser struct {
	ID       int64
	Username string
}

// ---------- CORS ----------

func corsMiddleware(origins string) gin.HandlerFunc {
	allow := "*"
	if origins != "" && origins != "*" {
		allow = origins
	}
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", allow)
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Expose-Headers", "X-Task-Id, X-Trace-Id")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// ---------- 可观测性中间件 ----------

func metricsMiddleware(m *observability.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if m != nil {
			m.ObserveHTTP(c.Request.Method, c.FullPath(), c.Writer.Status(),
				time.Since(start).Seconds())
		}
	}
}

// ---------- 全链路 Trace ----------

// traceSkipPaths 是不产生 span 的路径。
//
// /metrics 必须排除，理由不只是"省一点开销"：Prometheus 每 15 秒抓一次，
// 一天就是 5760 条 trace，会把真正有价值的业务 trace 全部挤出环形缓冲。
// 这是一个典型的自噬——**监控接口把监控数据淹掉了**，而且症状很隐蔽
// （业务 trace 莫名其妙"查不到"，看起来像 tracing 没生效）。
var traceSkipPaths = map[string]bool{
	"/metrics": true,
	"/healthz": true,
}

// tracingMiddleware 为每个请求起一个根 span，并把 trace id 回写到响应头。
//
// 采样决策在这里做一次（链路入口），后续所有进程靠 ParentBased 继承这个决定。
func tracingMiddleware(p *tracing.Provider) gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || !p.Enabled() || traceSkipPaths[c.Request.URL.Path] {
			c.Next()
			return
		}
		// 上游若已经传了 traceparent（网关/前端起了链），就续接而不是另起一条。
		parent := tracing.Extract(c.Request.Context(), c.GetHeader("traceparent"))
		// span 名用**路由模板**而不是实际路径：用实际路径的话，
		// /api/tasks/12 和 /api/tasks/13 会变成两个不同的 span 名，
		// 聚合维度直接爆炸（这与 Prometheus 里必须用 c.FullPath() 是同一个道理）。
		name := "HTTP " + c.Request.Method
		if fp := c.FullPath(); fp != "" {
			name += " " + fp
		}
		ctx, span := tracing.StartServer(parent, name,
			tracing.KV("http.request.method", c.Request.Method),
			tracing.KV("url.path", c.Request.URL.Path),
			tracing.KV("client.address", c.ClientIP()),
		)
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		// 把 trace id 回给调用方，出问题时用户可以直接把这个值贴给运维。
		c.Header("X-Trace-Id", tracing.TraceID(ctx))

		c.Next()

		// 状态码只有 handler 跑完才知道。
		tracing.MarkHTTPStatus(span, c.Writer.Status())
		// 记录归属：认证中间件在 tracing **之后**执行，所以只有在这里才拿得到用户。
		// 不记录的话，这条 trace 就变成"任何已登录用户都能读"，
		// 而 span 里带着原始错误文本（可能含 SQL、表名、内网地址）。
		if u := getUser(c); u != nil {
			p.Store().Link(tracing.TraceID(ctx), 0, u.ID)
		}
	}
}

// ---------- JWT 认证 ----------

func authMiddleware(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		// auth-scheme 大小写不敏感（RFC 7235 §2.1）："bearer"/"BEARER" 都合法。
		// 原实现用 strings.HasPrefix(header, "Bearer ")，严格区分大小写，
		// 客户端发 `bearer <token>` 会被判成"缺少 token"而 401——
		// 而响应文案说的是 missing bearer token，排查起来非常费劲。
		const scheme = "bearer "
		if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := authSvc.Parse(strings.TrimSpace(header[len(scheme):]))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		c.Set(userKey, &ctxUser{ID: claims.UserID, Username: claims.Username})
		c.Next()
	}
}

func getUser(c *gin.Context) *ctxUser {
	v, ok := c.Get(userKey)
	if !ok {
		return nil
	}
	u, _ := v.(*ctxUser)
	return u
}

// ---------- 限流 ----------

func rateLimitMiddleware(l ratelimit.Limiter, perMin int) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := getUser(c)
		// 未装配限流器（单测 / 极简部署）时直接放行，不因为缺少依赖而 500
		if l == nil || u == nil || perMin <= 0 {
			c.Next()
			return
		}
		// 任务提交类接口单独限流（在 handler 层做），这里做全局兜底
		res, err := l.Allow(c.Request.Context(),
			"user:"+stringInt(u.ID), perMin, time.Minute)
		if err != nil {
			c.Next()
			return
		}
		c.Header("X-RateLimit-Limit", stringInt(res.Limit))
		c.Header("X-RateLimit-Remaining", stringInt(res.Remaining))
		if !res.Allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded, retry later",
			})
			return
		}
		c.Next()
	}
}

func stringInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------- 错误映射 ----------

// abortWithError 把领域错误映射成 HTTP 状态码。
// err 为 nil 时按 500 处理并给出明确文案——调用方存在传 nil 的路径，
// 直接 err.Error() 会 panic（这是 Go 里很典型的一类线上崩溃）。
func abortWithError(c *gin.Context, err error) {
	if err == nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
	case errors.Is(err, store.ErrUserExists):
		// 必须匹配 store.ErrUserExists，而不是 auth 里某个同名错误：
		// store.UserRepo 在唯一约束冲突（PG 23505）时返回的就是这一个。
		// 原先这里写的是 auth.ErrUserExists——消息完全相同但**值不同**，
		// 于是 errors.Is 永远为假，重复注册返回 500 而不是 409。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
	case isNotFound(err):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, task.ErrTaskNotCancellable):
		// 任务已终态，不是错误也不是服务端故障：409 才是正确语义
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, queue.ErrQueueFull):
		// 队列接近容量上界（背压）。这是**过载**而不是**故障**：
		// 503 而不是 500，并带上 Retry-After 告诉调用方退避重试。
		// 语义上等价于限流，因此复用 429 的响应头习惯；用 503 是因为
		// 这不是"这个用户提交太快"，而是"整个服务当前接不下"。
		c.Header("Retry-After", "1")
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "task queue is at capacity, retry later",
		})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrWorkflowNotFound) ||
		errors.Is(err, store.ErrTaskNotFound) ||
		errors.Is(err, store.ErrKBNotFound) ||
		errors.Is(err, store.ErrDocNotFound) ||
		errors.Is(err, store.ErrProviderNotFound)
}

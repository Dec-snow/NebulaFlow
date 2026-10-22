// Package api 实现 REST API 层：路由、中间件与各领域 handler。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"fmt"
	"log"
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

// ---------- 访问日志 ----------

// accessLogSink 是访问日志的输出目标（可注入，测试用）。
var accessLogSink = func(msg string) { log.Print(msg) }

// accessLogMiddleware 输出结构化访问日志：method / path / status / latency / user_id。
//
// 为什么不用 gin.Logger()：那是非结构化的文本行，且不记录"是谁"——
// 而排查"哪个用户在哪个接口上慢"恰恰需要 user 维度。这里的顺序约定：
// 访问日志放在 authMiddleware **之后**注册（Router 中），这样能拿到用户；
// 放在 metricsMiddleware 之后共享同一个计时起点没有意义，各自计时即可。
//
// 路径用 c.FullPath()（路由模板）：与 metrics 同一口径，避免 /api/tasks/12
// 这类路径把日志与指标的聚合维度炸掉；404（无路由）时 FullPath 为空，退回原始路径。
func accessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		p := c.FullPath()
		if p == "" {
			p = c.Request.URL.Path
		}
		user := "-"
		if u := getUser(c); u != nil {
			user = strconvInt(u.ID)
		}
		accessLogSink(fmt.Sprintf("[api] %s %s -> %d %s user=%s ip=%s",
			c.Request.Method, p, c.Writer.Status(),
			time.Since(start).Round(time.Microsecond), user, c.ClientIP()))
	}
}

func strconvInt(n int64) string {
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
//
// 关于 `traceparent` 请求头：**只对已认证请求生效，且只允许续接自己的 trace**。
//
// 这是本中间件最重要的一条安全约束。请求头是客户端完全可控的输入，而 trace 的
// 归属（SpanStore.ownerID）决定谁能读到整条链路；原实现无条件采信客户端给的
// traceparent，于是攻击者只要拿到别人的 traceparent 发一次请求，就能把自己的
// user id 写进别人的 trace，随后从 /api/traces/<id> 读到对方的内部错误细节
// （span 里带着 SQL、表名、内网地址）。
//
// 现在的规则：
//   - 未认证（没有合法 bearer token）的请求**一律另起新链**，不续接任何外部 trace；
//   - 已认证请求也只能续接"尚无人认领"或"认领者就是自己"的 trace，
//     别人的 trace 一律忽略该头（另起新链）。
func tracingMiddleware(p *tracing.Provider, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if p == nil || !p.Enabled() || traceSkipPaths[c.Request.URL.Path] {
			c.Next()
			return
		}
		// 认证中间件在本中间件**之后**执行（trace 必须包住认证，否则"401 是哪个
		// 环节拒的"就没有链路信息），所以这里自己解析一次 token 来判断能不能
		// 采信 traceparent。JWT 校验是纯 CPU 的 HMAC 运算（微秒级），
		// 用一次多余的解析换"归属不可伪造"是划算的。
		uid := authenticatedUserID(c, authSvc)

		parent := c.Request.Context()
		if candidate, ok := trustedParent(c, p, uid); ok {
			parent = candidate
		}
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

		// 归属在 span **一开始**就认领，而不是等请求结束。
		// 否则"真正的发起者"和"拿着同一个 traceparent 的后来者"之间会有一个竞态窗口，
		// 谁先跑完谁成为 owner——而攻击者可以刻意让自己的请求更快返回。
		// 先认领（首次写入者胜，见 SpanStore.Link），后来者只能另起新链。
		if uid != 0 {
			p.Store().Link(tracing.TraceID(ctx), 0, uid)
		}

		c.Next()

		// 状态码只有 handler 跑完才知道。
		tracing.MarkHTTPStatus(span, c.Writer.Status())
		// 记录归属：认证中间件在 tracing **之后**执行，所以只有在这里才拿得到用户。
		// 不记录的话，这条 trace 就变成"任何已登录用户都能读"，
		// 而 span 里带着原始错误文本（可能含 SQL、表名、内网地址）。
		// 这里是幂等写入：已有归属（含上面刚认领的）不会被覆盖。
		if u := getUser(c); u != nil {
			p.Store().Link(tracing.TraceID(ctx), 0, u.ID)
		}
	}
}

// trustedParent 判断客户端给的 traceparent 能不能作为父上下文续接。
//
// 两条同时满足才续接：
//  1. 请求携带了**合法**的 bearer token —— 否则它是一个匿名请求，
//     没有资格把自己的 span 写进任何已有 trace；
//  2. 目标 trace 尚无人认领，或者认领者就是当前用户 —— 否则就是"劫持他人 trace"，
//     本请求另起新链（不是返回错误：traceparent 只是链路传播信息，
//     不是访问控制凭据，为它返回 4xx 反而会破坏正常的跨服务调用）。
func trustedParent(c *gin.Context, p *tracing.Provider, uid int64) (context.Context, bool) {
	raw := strings.TrimSpace(c.GetHeader("traceparent"))
	if raw == "" || uid == 0 || p.Store() == nil {
		return nil, false
	}
	candidate := tracing.Extract(c.Request.Context(), raw)
	traceID := tracing.TraceID(candidate)
	if traceID == "" {
		return nil, false
	}
	owner, known := p.Store().Owner(traceID)
	if known && owner != 0 && owner != uid {
		return nil, false
	}
	return candidate, true
}

// authenticatedUserID 解析 Authorization 头并返回用户 id；未认证（或 token 非法）返回 0。
func authenticatedUserID(c *gin.Context, authSvc *auth.Service) int64 {
	if authSvc == nil {
		return 0
	}
	token := bearerToken(c)
	if token == "" {
		return 0
	}
	claims, err := authSvc.Parse(token)
	if err != nil {
		return 0
	}
	return claims.UserID
}

// bearerToken 取出 Authorization 头里的 token。
// auth-scheme 大小写不敏感（RFC 7235 §2.1）："bearer"/"BEARER" 都合法。
//
// 原实现用 strings.HasPrefix(header, "Bearer ")，严格区分大小写，
// 客户端发 `bearer <token>` 会被判成"缺少 token"而 401——
// 而响应文案说的是 missing bearer token，排查起来非常费劲。
// 抽成函数是为了让 tracing 中间件与认证中间件用**同一套**解析规则，
// 两边对"什么算已认证"的判断必须完全一致，否则就会出现
// "认证中间件放行了、tracing 中间件却认为它未认证"这种诡异的不一致。
func bearerToken(c *gin.Context) string {
	const scheme = "bearer "
	header := c.GetHeader("Authorization")
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(header[len(scheme):])
}

// ---------- JWT 认证 ----------

func authMiddleware(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c)
		if token == "" {
			abortJSON(c, http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		claims, err := authSvc.Parse(token)
		if err != nil {
			abortJSON(c, http.StatusUnauthorized, gin.H{"error": "invalid token"})
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

// ---------- admin 权限校验 ----------

// adminMiddleware 校验当前用户是否为 admin 角色。
// 必须在 authMiddleware 之后执行（依赖 userKey）。
func adminMiddleware(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := getUser(c)
		if u == nil {
			// 理论上 authMiddleware 已经保证了用户存在，
			// 这里做防御性检查，避免中间件顺序配错时出现空指针。
			abortJSON(c, http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		if err := authSvc.RequireAdmin(c.Request.Context(), u.ID); err != nil {
			abortWithError(c, err)
			return
		}
		c.Next()
	}
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
			abortJSON(c, http.StatusTooManyRequests, gin.H{
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

// authRateLimitMiddleware 是给**未认证**的认证接口（注册/登录）用的限流。
//
// 为什么不能直接复用 rateLimitMiddleware：那个按 `user:{id}` 计数，而注册/登录
// 恰恰发生在拿到 user 之前——`u == nil` 时它会直接放行。于是认证接口成了全站
// 唯一没有任何限流的入口：攻击者可以无限次猜密码（每次都是 401，监控上不产生
// 5xx，看起来"系统很健康"），也可以无限次批量注册把库塞满。
//
// 这里按 **客户端 IP + 用户名** 两个维度计数：
//   - 带 IP 是必须的（挡"换用户名继续猜"的暴力扫描）；
//   - 带用户名是为了避免同一出口 IP（公司 NAT、运营商 CGNAT）下的正常用户
//     被邻居的错误密码连累——这是"按 IP 限流"最常见的误伤。
func authRateLimitMiddleware(l ratelimit.Limiter, perMin int) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 未装配限流器（单测 / 极简部署）时直接放行，不因为缺少依赖而 500。
		if l == nil || perMin <= 0 {
			c.Next()
			return
		}
		key := "auth:" + c.ClientIP()
		if username := peekUsername(c); username != "" {
			key += ":" + strings.ToLower(username)
		}
		res, err := l.Allow(c.Request.Context(), key, perMin, time.Minute)
		if err != nil {
			// 限流器故障（例如 Redis 挂了）时放行：认证接口宁可被刷，
			// 也不能因为一个"保护措施"把自己的门关上。
			c.Next()
			return
		}
		c.Header("X-RateLimit-Limit", stringInt(res.Limit))
		c.Header("X-RateLimit-Remaining", stringInt(res.Remaining))
		if !res.Allowed {
			abortJSON(c, http.StatusTooManyRequests, gin.H{
				"error": "too many attempts, retry later",
			})
			return
		}
		c.Next()
	}
}

// authPeekLimit 是我们从请求体里最多读取多少字节去解析 username。
const authPeekLimit = 8192

// peekUsername 从 JSON 请求体里"偷看"username 字段，并把请求体**完整还原**。
//
// 还原这一步是必须的：中间件只是看一眼用户名，真正的绑定仍然发生在 handler 里
// （c.ShouldBindJSON）。读完不还回去，handler 只会看到一个空 body，
// 于是所有登录请求都会变成 400「EOF」——一个典型的"安全补丁把功能打坏了"。
func peekUsername(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	orig := c.Request.Body
	raw, err := io.ReadAll(io.LimitReader(orig, authPeekLimit))
	// 用 MultiReader 拼回"已读部分 + 剩余部分"，因此即使 body 超过上限被截断，
	// handler 仍然能读到完整的原始数据。
	c.Request.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), orig), orig}
	if err != nil || len(raw) == 0 {
		return ""
	}
	var probe struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		// 不是 JSON（或超长被截断）：退化成只按 IP 计数，不能因此拒绝请求——
		// 参数错误的响应应该由 handler 给出，而不是被限流中间件拦成一个 429。
		return ""
	}
	return probe.Username
}

// ---------- 响应写出 ----------

// writeJSON 是**成功**响应的统一出口。
//
// 它和 abortJSON 一起保证"一个请求只写一个 JSON 响应体"。
//
// 原实现没有这层保护，而 mustID 之类的辅助函数写完 400 之后**调用方仍会继续执行**
// （Go 里没有任何机制能让被调用的函数提前结束调用方；`return` 只能写在调用点）。
// 于是第二个错误/成功响应会被追加进同一个 body：
//
//	{"error":"invalid id parameter"}{"error":"task not found"}
//
// 两个合法 JSON 拼在一起就不再是合法 JSON，客户端 json.Unmarshal 直接失败——
// 于是一个"参数写错了"的 400，在调用方看来变成了"响应损坏"，
// 而这恰恰是最难定位的一类缺陷（服务端日志一切正常，只有客户端解析报错）。
func writeJSON(c *gin.Context, status int, body any) {
	if c.Writer.Written() {
		// 响应体已经写出：丢弃这次写入并终止后续 handler。
		c.Abort()
		return
	}
	c.JSON(status, body)
}

// abortJSON 是**错误**响应的统一出口，语义与 writeJSON 相同：
// 响应已经写出就不再写第二个（并终止后续 handler）。
func abortJSON(c *gin.Context, status int, body any) {
	if c.Writer.Written() {
		c.Abort()
		return
	}
	c.AbortWithStatusJSON(status, body)
}

// ---------- 错误映射 ----------

// abortWithError 把领域错误映射成 HTTP 状态码。
// err 为 nil 时按 500 处理并给出明确文案——调用方存在传 nil 的路径，
// 直接 err.Error() 会 panic（这是 Go 里很典型的一类线上崩溃）。
//
// 两条贯穿始终的规则：
//
//  1. **未知错误不回显 err.Error()**。基础设施错误的文本里经常带着 SQL 语句、
//     表名、内网地址（实测 PG 连接失败会把 `dial tcp 10.x.x.x:5432` 整段回给调用方），
//     那等于把内部拓扑免费送给任何一个能触发 500 的人。细节进日志，客户端只拿固定文案。
//     只有**显式定义为领域错误**的文本才回显（见下面各个 case）。
//  2. 响应已经写出就不再写第二个（abortJSON 的守卫）：mustID 之类的辅助函数
//     写完 400 后调用方仍会继续执行，第二个 JSON 会把响应体拼坏。
func abortWithError(c *gin.Context, err error) {
	if err == nil {
		abortJSON(c, http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		abortJSON(c, http.StatusUnauthorized, gin.H{"error": err.Error()})
	case errors.Is(err, auth.ErrInvalidToken):
		abortJSON(c, http.StatusUnauthorized, gin.H{"error": err.Error()})
	case errors.Is(err, auth.ErrInsufficientPermission):
		abortJSON(c, http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, auth.ErrWeakPassword),
		errors.Is(err, auth.ErrPasswordTooShort),
		errors.Is(err, auth.ErrPasswordTooLong):
		// 密码策略错误：文本是**我们自己的**文案（不含任何库内部信息），
		// 属于可展示的校验错误 → 400。bcrypt 的实现细节一律不出现在响应里。
		abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, store.ErrUserExists):
		// 必须匹配 store.ErrUserExists，而不是 auth 里某个同名错误：
		// store.UserRepo 在唯一约束冲突（PG 23505）时返回的就是这一个。
		// 原先这里写的是 auth.ErrUserExists——消息完全相同但**值不同**，
		// 于是 errors.Is 永远为假，重复注册返回 500 而不是 409。
		abortJSON(c, http.StatusConflict, gin.H{"error": err.Error()})
	case isNotFound(err):
		abortJSON(c, http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, task.ErrTaskNotCancellable):
		// 任务已终态，不是错误也不是服务端故障：409 才是正确语义
		abortJSON(c, http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, store.ErrValidation):
		// 参数校验失败：400 Bad Request
		abortJSON(c, http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, task.ErrUserConnectionsExceeded):
		// 单用户 SSE 连接数超限：429（用户级别的资源限制）
		abortJSON(c, http.StatusTooManyRequests, gin.H{"error": err.Error()})
	case errors.Is(err, queue.ErrQueueFull):
		// 队列接近容量上界（背压）。这是**过载**而不是**故障**：
		// 503 而不是 500，并带上 Retry-After 告诉调用方退避重试。
		// 语义上等价于限流，因此复用 429 的响应头习惯；用 503 是因为
		// 这不是"这个用户提交太快"，而是"整个服务当前接不下"。
		c.Header("Retry-After", "1")
		abortJSON(c, http.StatusServiceUnavailable, gin.H{
			"error": "task queue is at capacity, retry later",
		})
	case errors.Is(err, task.ErrTooManyConnections):
		// 全局 SSE 连接数超限：503（服务级容量上限）
		c.Header("Retry-After", "5")
		abortJSON(c, http.StatusServiceUnavailable, gin.H{
			"error": "sse connection limit reached, retry later",
		})
	default:
		// 未知错误 = 服务端故障，文本里可能含 SQL / 表名 / 内网地址：
		// 细节只写日志，客户端拿固定文案。
		logInternalError(c, err)
		abortJSON(c, http.StatusInternalServerError, gin.H{"error": "internal server error"})
	}
}

// logInternalError 把未知错误的细节写进服务端日志。
//
// 这是"500 不回显"的另一半：信息不能既不回给客户端、又不落日志，
// 否则线上出现 5xx 时运维手里什么都没有，只能靠复现碰运气。
func logInternalError(c *gin.Context, err error) {
	method, path := "", ""
	if c != nil && c.Request != nil {
		method, path = c.Request.Method, c.Request.URL.Path
	}
	log.Printf("[api] 500 %s %s: %v", method, path, err)
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrWorkflowNotFound) ||
		errors.Is(err, store.ErrTaskNotFound) ||
		errors.Is(err, store.ErrKBNotFound) ||
		errors.Is(err, store.ErrDocNotFound) ||
		errors.Is(err, store.ErrProviderNotFound)
}

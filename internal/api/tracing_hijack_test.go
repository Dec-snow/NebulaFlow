package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// 这个文件守的是**trace 归属不可被请求头伪造**这条安全边界。
//
// 缺陷回顾（越权）：`traceparent` 是客户端完全可控的请求头，而 tracing 中间件
// 在**鉴权之前**就采信它，并且请求结束时无条件把当前用户写进这条 trace 的归属。
// 于是攻击者只要拿到（或从日志/响应头里收集到）别人的 traceparent，
// 发一个普通请求就能：
//  1. 把自己的 user id 写成那条 trace 的 owner；
//  2. 随后 GET /api/traces/<id> 读到对方整条链路 —— 而 span 里带着
//     原始错误文本（SQL 片段、表名、内网地址）。
//
// 修复后：未认证请求不续接任何外部 trace；已认证请求只能续接"无人认领"
// 或"就是自己"的 trace；归属首次写入者胜。

// newHijackRig 装配一个"tracing + 认证 + trace 查询"的最小路由。
func newHijackRig(t *testing.T) (*gin.Engine, *tracing.Provider, *auth.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	p, err := tracing.Setup(context.Background(), tracing.Config{Enabled: true})
	if err != nil {
		t.Fatalf("tracing.Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ms := store.NewMemory()
	svc := auth.NewService(ms.Store.Users, testJWTSecret, time.Hour)

	r := gin.New()
	// 与生产一致：tracing 在最外层（包住认证），认证在路由组上。
	r.Use(tracingMiddleware(p, svc))

	apiG := r.Group("/api")
	apiG.Use(authMiddleware(svc))
	apiG.GET("/whoami", func(c *gin.Context) {
		u := getUser(c)
		writeJSON(c, http.StatusOK, gin.H{"id": u.ID})
	})
	apiG.GET("/traces/:trace_id", (&traceHandler{tracing: p}).byID)
	return r, p, svc
}

func registerUser(t *testing.T, svc *auth.Service, name string) (int64, string) {
	t.Helper()
	u, err := svc.Register(context.Background(), name, name+"@example.com", "password1")
	if err != nil {
		t.Fatalf("注册 %s 失败: %v", name, err)
	}
	token, err := svc.Issue(u)
	if err != nil {
		t.Fatalf("签发 %s 的 token 失败: %v", name, err)
	}
	return u.ID, token
}

func tracedGet(r *gin.Engine, path, token, traceparent string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
	r.ServeHTTP(w, req)
	return w
}

// traceparentFor 用某条 trace 的 id 拼一个合法的 W3C traceparent（父 span id 任意）。
func traceparentFor(traceID string) string {
	return "00-" + traceID + "-00f067aa0ba902b7-01"
}

func traceSpanCount(t *testing.T, p *tracing.Provider, traceID string) int {
	t.Helper()
	tr, ok := p.Store().Snapshot(traceID)
	if !ok {
		return 0
	}
	return tr.SpanCount
}

// **本条是缺陷 1 的核心回归测试**：bob 拿着 alice 的 traceparent
// 既不能读到、也不能抢走 alice 的 trace。
func TestTraceCannotBeHijackedViaTraceparentHeader(t *testing.T) {
	r, p, svc := newHijackRig(t)
	aliceID, aliceToken := registerUser(t, svc, "alice")
	_, bobToken := registerUser(t, svc, "bob")

	// --- alice 正常发一次请求，拿到自己的 trace id ---
	w := tracedGet(r, "/api/whoami", aliceToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("alice 的请求应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	aliceTrace := w.Header().Get("X-Trace-Id")
	if aliceTrace == "" {
		t.Fatal("tracing 启用时响应头必须带 X-Trace-Id")
	}
	if owner, known := p.Store().Owner(aliceTrace); !known || owner != aliceID {
		t.Fatalf("alice 的 trace 归属应为 %d，实际 %d（known=%v）", aliceID, owner, known)
	}
	spansBefore := traceSpanCount(t, p, aliceTrace)

	// --- bob 拿 alice 的 traceparent 发请求（伪装成"续接上游链路"） ---
	w2 := tracedGet(r, "/api/whoami", bobToken, traceparentFor(aliceTrace))
	if w2.Code != http.StatusOK {
		t.Fatalf("bob 的请求应 200（traceparent 不是访问控制凭据，不该因此报错），实际 %d", w2.Code)
	}
	if bobTrace := w2.Header().Get("X-Trace-Id"); bobTrace == aliceTrace {
		t.Fatalf("bob 用别人的 traceparent 续接成功了：他的请求被并进了 alice 的 trace（%s）", bobTrace)
	}

	// 断言一：归属没有被抢走
	if owner, _ := p.Store().Owner(aliceTrace); owner != aliceID {
		t.Fatalf("alice 的 trace 归属被抢走了：应为 %d，实际 %d", aliceID, owner)
	}
	// 断言二：bob 的 span 没有落进 alice 的 trace（否则等于往别人链路里注入数据）
	if got := traceSpanCount(t, p, aliceTrace); got != spansBefore {
		t.Fatalf("bob 的 span 被写进了 alice 的 trace：%d → %d 条", spansBefore, got)
	}
	// 断言三：bob 读不到 alice 的 trace，且必须是 404（403 会变成存在性预言机）
	wb := tracedGet(r, "/api/traces/"+aliceTrace, bobToken, "")
	if wb.Code != http.StatusNotFound {
		t.Fatalf("bob 读 alice 的 trace 应 404，实际 %d（%s）", wb.Code, wb.Body.String())
	}
	if strings.Contains(wb.Body.String(), aliceTrace) {
		t.Fatalf("拒绝响应里不应泄露 trace id，实际 %s", wb.Body.String())
	}

	// 对照组（必须有）：alice 自己仍然读得到自己的 trace。
	// 少了这一条，上面的 404 可能只是因为"查询接口整个坏掉了"而通过。
	wa := tracedGet(r, "/api/traces/"+aliceTrace, aliceToken, "")
	if wa.Code != http.StatusOK {
		t.Fatalf("alice 读自己的 trace 应 200，实际 %d（%s）", wa.Code, wa.Body.String())
	}
}

// 未认证请求不得用外部 traceparent 续接已有 trace（原来它在鉴权之前就采信了）。
func TestUnauthenticatedRequestDoesNotContinueForeignTrace(t *testing.T) {
	r, p, svc := newHijackRig(t)
	aliceID, aliceToken := registerUser(t, svc, "alice")

	w := tracedGet(r, "/api/whoami", aliceToken, "")
	aliceTrace := w.Header().Get("X-Trace-Id")
	spansBefore := traceSpanCount(t, p, aliceTrace)

	// 无 token，但带着 alice 的 traceparent
	anon := tracedGet(r, "/api/whoami", "", traceparentFor(aliceTrace))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，实际 %d", anon.Code)
	}
	if got := anon.Header().Get("X-Trace-Id"); got == aliceTrace {
		t.Fatalf("未认证请求续接了 alice 的 trace（%s）——它连身份都没有，没有资格往别人的链路里写 span", got)
	}
	if owner, _ := p.Store().Owner(aliceTrace); owner != aliceID {
		t.Fatalf("未认证请求改变了 alice 的 trace 归属：应为 %d，实际 %d", aliceID, owner)
	}
	if got := traceSpanCount(t, p, aliceTrace); got != spansBefore {
		t.Fatalf("未认证请求的 span 落进了 alice 的 trace：%d → %d 条", spansBefore, got)
	}
}

// 对照组：修复不能变成"一律忽略 traceparent"。
//
// 已认证用户续接**自己**的 trace 是正常且必要的（浏览器重试、前端把上一次的
// traceparent 带回来继续排查），必须还能工作 —— 否则这次"修安全"就把功能删掉了。
func TestAuthenticatedUserCanStillContinueOwnTrace(t *testing.T) {
	r, _, svc := newHijackRig(t)
	_, aliceToken := registerUser(t, svc, "alice")

	first := tracedGet(r, "/api/whoami", aliceToken, "")
	aliceTrace := first.Header().Get("X-Trace-Id")

	second := tracedGet(r, "/api/whoami", aliceToken, traceparentFor(aliceTrace))
	if got := second.Header().Get("X-Trace-Id"); got != aliceTrace {
		t.Fatalf("alice 应能续接自己的 trace：期望 %s，实际 %s", aliceTrace, got)
	}
}

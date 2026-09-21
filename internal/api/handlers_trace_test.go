package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
)

// 这组用例守的是 trace 查询接口的**权限**，而不是它的功能。
//
// 为什么权限在这里格外重要：span 里会带上原始错误文本，而错误文本经常包含
// SQL 片段、表名、内网地址（P1-6 已经发现过 abortWithError 把 err.Error()
// 原样返回客户端的问题，是同一类风险）。如果 trace 接口不做归属校验，
// 那就等于给所有已登录用户开了一个读内部细节的口子——而且它比那个更隐蔽，
// 因为它不在业务接口上，很容易在评审时被当成"运维接口"放过。

func newTraceRig(t *testing.T) (*traceHandler, *tracing.Provider) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	p, err := tracing.Setup(context.Background(), tracing.Config{Enabled: true})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return &traceHandler{tracing: p}, p
}

// emitTrace 起一条 trace 并把它关联给 userID，返回 trace id。
func emitTrace(t *testing.T, p *tracing.Provider, userID, taskID int64) string {
	t.Helper()
	ctx, span := tracing.StartServer(context.Background(), "HTTP POST /api/tasks")
	span.End()
	id := tracing.TraceID(ctx)
	if id == "" {
		t.Fatal("应产生有效 trace id")
	}
	p.Store().Link(id, taskID, userID)
	return id
}

func traceCtx(t *testing.T, userID int64, traceID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/traces/"+traceID, nil)
	c.Params = gin.Params{{Key: "trace_id", Value: traceID}}
	if userID != 0 {
		c.Set(userKey, &ctxUser{ID: userID, Username: "u"})
	}
	return c, w
}

func TestTraceByIDAllowsOwner(t *testing.T) {
	h, p := newTraceRig(t)
	id := emitTrace(t, p, 7, 55)

	c, w := traceCtx(t, 7, id)
	h.byID(c)

	if w.Code != http.StatusOK {
		t.Fatalf("本人查询应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Trace-Span-Count"); got != "1" {
		t.Fatalf("应回写 span 数，实际 %q", got)
	}
}

// 别人的 trace 必须读不到，而且必须是 **404 而不是 403**：
// 403 等于确认"这条 trace 存在，只是不属于你"，把 trace id 变成了
// 一个可枚举的存在性预言机（trace id 是 128 位随机数，本来枚举不出来，
// 但一个"存在/不存在"的区分接口会把这个性质直接废掉）。
func TestTraceByIDHidesOtherUsersTrace(t *testing.T) {
	h, p := newTraceRig(t)
	id := emitTrace(t, p, 7, 55)

	c, w := traceCtx(t, 8, id)
	h.byID(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("他人的 trace 应返回 404（而不是 403），实际 %d", w.Code)
	}
	// 响应体里不能泄露任何 span 信息
	if body := w.Body.String(); len(body) == 0 {
		t.Fatal("应返回错误体")
	}
}

// owner 为 0 表示这条 trace 从未被关联到用户。此时必须**拒绝**（fail-closed）。
// 放行的话，任何"没走到关联那一步"的 trace（例如中间件之前的路径、
// 或将来新增的入口）都会变成全员可读。
func TestTraceByIDRejectsUnownedTrace(t *testing.T) {
	h, _ := newTraceRig(t)
	// 只产生 span，不做 Link
	ctx, span := tracing.StartServer(context.Background(), "HTTP GET /orphan")
	span.End()
	id := tracing.TraceID(ctx)

	c, w := traceCtx(t, 7, id)
	h.byID(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("无归属的 trace 应被拒绝（fail-closed），实际 %d", w.Code)
	}
}

func TestTraceByIDUnknownTraceIs404(t *testing.T) {
	h, _ := newTraceRig(t)
	c, w := traceCtx(t, 7, "00000000000000000000000000000000")
	h.byID(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的 trace 应 404，实际 %d", w.Code)
	}
}

// 未认证时必须是 401，而不是解引用 nil 用户导致 500。
// 路由当前挂在 authMiddleware 之后，但那个前提靠配置维持——
// 一旦有人把某条 trace 路由挪到免认证分组，这里就是 500 而不是 401。
func TestTraceByIDWithoutUserIs401(t *testing.T) {
	h, p := newTraceRig(t)
	id := emitTrace(t, p, 7, 55)

	c, w := traceCtx(t, 0, id)
	h.byID(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", w.Code)
	}
}

// tracing 未启用时接口应明确返回 501，而不是 500 或空结果。
// 空结果会让调用方以为"这个任务没有 trace"，而真相是"根本没开 tracing"——
// 两者的排查方向完全不同。
func TestTraceByIDWhenTracingDisabledIs501(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p, err := tracing.Setup(context.Background(), tracing.Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	h := &traceHandler{tracing: p}

	c, w := traceCtx(t, 7, "00000000000000000000000000000000")
	h.byID(c)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("未启用 tracing 应 501，实际 %d", w.Code)
	}
}

func TestTraceListOnlyReturnsOwnTraces(t *testing.T) {
	h, p := newTraceRig(t)
	emitTrace(t, p, 7, 101)
	emitTrace(t, p, 7, 102)
	emitTrace(t, p, 8, 201)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/traces", nil)
	c.Set(userKey, &ctxUser{ID: 7, Username: "u"})
	h.list(c)

	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"task_id":101`) || !strings.Contains(body, `"task_id":102`) {
		t.Fatalf("应包含自己的两条 trace，实际 %s", body)
	}
	if strings.Contains(body, `"task_id":201`) {
		t.Fatalf("不应包含其他用户的 trace，实际 %s", body)
	}
}

// ---------- /api/tasks/:id/trace（最常用的入口） ----------

// newTraceTaskRig 装配一个带真实 task.Service 的装置。
// byTask 的权限判断**复用任务查询**（GetTask 本身就按 user_id 过滤），
// 所以这里必须用真的 store，不能拿假对象糊过去——那样测的就不是真实语义了。
func newTraceTaskRig(t *testing.T) (*traceHandler, *tracing.Provider, *store.MemoryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	p, err := tracing.Setup(context.Background(), tracing.Config{Enabled: true})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ms := store.NewMemory()
	svc := task.NewService(ms.Store, &fakeQueue{}, task.NewHub())
	return &traceHandler{svc: svc, tracing: p}, p, ms
}

func taskTraceCtx(t *testing.T, userID, taskID int64) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/tasks/%d/trace", taskID), nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(taskID, 10)}}
	if userID != 0 {
		c.Set(userKey, &ctxUser{ID: userID, Username: "u"})
	}
	return c, w
}

func seedTask(t *testing.T, ms *store.MemoryStore, userID int64) int64 {
	t.Helper()
	tk := &model.Task{WorkflowID: 1, UserID: userID, Status: model.TaskPending, Input: "hi"}
	if err := ms.Store.Tasks.CreateTask(context.Background(), tk); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return tk.ID
}

// 这条是安全边界：trace 里带着原始错误文本（可能含 SQL、表名、内网地址），
// 而按任务 id 查是**最容易被猜到的入口**——任务 id 是自增整数，不是 128 位随机数。
// 所以 byTask 必须走任务归属校验，不能只凭一个 task id 就把链路交出去。
func TestTraceByTaskChecksTaskOwnership(t *testing.T) {
	h, p, ms := newTraceTaskRig(t)
	taskID := seedTask(t, ms, 7)
	traceID := emitTrace(t, p, 7, taskID)

	// 本人：正常读到
	c, w := taskTraceCtx(t, 7, taskID)
	h.byTask(c)
	if w.Code != http.StatusOK {
		t.Fatalf("本人查自己任务的链路应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), traceID) {
		t.Fatalf("响应体应含该 trace，实际 %s", w.Body.String())
	}

	// 他人：读不到，而且响应体里不能出现任何 trace 内容
	c2, w2 := taskTraceCtx(t, 8, taskID)
	h.byTask(c2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("他人的任务链路应 404，实际 %d（%s）", w2.Code, w2.Body.String())
	}
	if strings.Contains(w2.Body.String(), traceID) {
		t.Fatalf("拒绝时不应泄露 trace id，实际 %s", w2.Body.String())
	}
}

// 任务还没被 worker 取走时确实没有 trace，这是**正常状态而不是错误**。
// 错误消息必须说清楚"可能还在排队"，否则调用方会以为链路丢了。
func TestTraceByTaskWithoutTraceYetIs404WithHint(t *testing.T) {
	h, _, ms := newTraceTaskRig(t)
	taskID := seedTask(t, ms, 7)

	c, w := taskTraceCtx(t, 7, taskID)
	h.byTask(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("尚无 trace 时应 404，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "queued") {
		t.Fatalf("应提示任务可能仍在排队，实际 %s", w.Body.String())
	}
}

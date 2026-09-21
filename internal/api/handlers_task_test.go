package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
)

// 这个文件补上 api 包的第一批测试。
// 此前该包零测试，而它承载的是**对外 HTTP 契约**——领域错误到状态码的映射一旦写错，
// 客户端就会把「过载」当成「服务故障」来重试，或者把「已存在」当成「参数错误」。
//
// ---------- 错误映射 ----------

func TestAbortWithErrorMapsDomainErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name           string
		err            error
		wantStatus     int
		wantRetryAfter string
	}{
		// 调用方存在传 nil 的路径，err.Error() 会 panic——这是 Go 里很典型的一类线上崩溃。
		{"nil 错误按 500 处理且不 panic", nil, http.StatusInternalServerError, ""},
		{"凭据错误 → 401", auth.ErrInvalidCredentials, http.StatusUnauthorized, ""},
		{"用户已存在 → 409", store.ErrUserExists, http.StatusConflict, ""},
		{"工作流不存在 → 404", store.ErrWorkflowNotFound, http.StatusNotFound, ""},
		{"任务不存在 → 404", store.ErrTaskNotFound, http.StatusNotFound, ""},
		{"知识库不存在 → 404", store.ErrKBNotFound, http.StatusNotFound, ""},
		{"Provider 不存在 → 404", store.ErrProviderNotFound, http.StatusNotFound, ""},
		{"任务已终态 → 409", task.ErrTaskNotCancellable, http.StatusConflict, ""},
		// 背压的关键契约：503（过载）而不是 500（故障），并告诉调用方可以重试。
		{"队列饱和 → 503 + Retry-After", queue.ErrQueueFull, http.StatusServiceUnavailable, "1"},
		{"未知错误 → 500", errors.New("boom"), http.StatusInternalServerError, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/tasks", nil)

			abortWithError(c, tc.err)

			if w.Code != tc.wantStatus {
				t.Fatalf("状态码应为 %d，实际 %d（body=%s）", tc.wantStatus, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tc.wantRetryAfter {
				t.Fatalf("Retry-After 应为 %q，实际 %q", tc.wantRetryAfter, got)
			}
			if !strings.Contains(w.Body.String(), "error") {
				t.Fatalf("响应体应含 error 字段，实际 %s", w.Body.String())
			}
		})
	}
}

// 错误映射必须走 errors.Is，而不是 ==：上层几乎总是用 %w 包一层上下文再往上抛。
// 用 == 比较的话，一个包装过的 ErrQueueFull 就会掉进 default 分支变成 500，
// 客户端于是把「过载、可以重试」误判成「服务故障」。
func TestAbortWithErrorUnwrapsWrappedErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	wrapped := fmt.Errorf("create task: %w", queue.ErrQueueFull)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/tasks", nil)

	abortWithError(c, wrapped)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("包装过的 ErrQueueFull 应仍映射为 503，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("包装过的 ErrQueueFull 也应带 Retry-After，实际 %q", got)
	}
}

// ---------- 背压的端到端契约 ----------

// fakeQueue 只实现 Queue + Admitter，用来在 handler 层验证背压行为。
// 用假的而不是真 RedisStreamQueue：这里要验的是 HTTP 契约，
// 队列自身的准入语义已由 internal/queue 的集成测试覆盖。
type fakeQueue struct {
	admitErr error
	enqueued int
}

func (f *fakeQueue) Name() string { return "fake" }

// Admit 返回预设结果，模拟「队列已满」或「还能收」。
func (f *fakeQueue) Admit(ctx context.Context) error { return f.admitErr }

func (f *fakeQueue) Enqueue(ctx context.Context, j queue.Job) error {
	f.enqueued++
	return nil
}

func (f *fakeQueue) Dequeue(ctx context.Context, workerID string, block time.Duration) (queue.Job, string, error) {
	return queue.Job{}, "", queue.ErrNoMessage
}
func (f *fakeQueue) Ack(ctx context.Context, msgID string) error { return nil }
func (f *fakeQueue) Nack(ctx context.Context, msgID, errMsg string, retryLeft int) error {
	return nil
}
func (f *fakeQueue) Len(ctx context.Context) (int64, error) { return 0, nil }
func (f *fakeQueue) Recover(ctx context.Context, workerID string, count int64) (int, error) {
	return 0, nil
}

// newTaskTestRouter 装配一个只挂 POST /api/tasks 的最小路由。
func newTaskTestRouter(t *testing.T, q queue.Queue) (*gin.Engine, *observability.Metrics, *store.MemoryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ms := store.NewMemory()
	metrics := observability.NewMetrics(prometheus.NewRegistry())
	svc := task.NewService(ms.Store, q, task.NewHub())
	h := &taskHandler{svc: svc, metrics: metrics}

	r := gin.New()
	// 直接注入认证用户，跳过 JWT——这里测的不是认证
	r.Use(func(c *gin.Context) {
		c.Set(userKey, &ctxUser{ID: 1, Username: "tester"})
		c.Next()
	})
	r.POST("/api/tasks", h.create)
	return r, metrics, ms
}

// P0-5 的核心 HTTP 契约：队列饱和时返回 503 + Retry-After，
// 并且**在存储里不留任何痕迹**。
//
// 最后一条断言才是重点：最初的实现把准入检查放在 Enqueue 里，任务行已经落库了才被拒，
// 于是每个被拒绝的请求都会留下一条 failed 记录——过载时拒绝得越多，
// 往数据库压的写就越多，而数据库恰恰是背压要保护的那一环。
func TestCreateTaskQueueFullReturns503AndPersistsNothing(t *testing.T) {
	q := &enqueueFailQueue{fakeQueue: &fakeQueue{admitErr: queue.ErrQueueFull}}
	r, metrics, ms := newTaskTestRouter(t, q)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"workflow_id":1,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("队列饱和时应返回 503，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("应带 Retry-After: 1，实际 %q", got)
	}
	if !strings.Contains(w.Body.String(), "capacity") {
		t.Fatalf("响应体应说明队列已满，实际 %s", w.Body.String())
	}

	// 拒绝必须发生在落库之前：不建任务行、不调 Enqueue
	if q.enqueued != 0 {
		t.Fatalf("被拒绝的请求不应调用 Enqueue，实际调用 %d 次", q.enqueued)
	}
	list, err := ms.Store.Tasks.ListTasks(context.Background(), 1, 100, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("被拒绝的请求不得留下任务行（这正是「幽灵 pending」的成因），实际留下 %d 条", len(list))
	}

	// 拒绝要能被观测到，否则运维不知道系统正在过载
	if got := testutil.ToFloat64(metrics.QueueRejectedTotal); got != 1 {
		t.Fatalf("rejected_total 应为 1，实际 %v", got)
	}
}

// 对照组：准入通过时必须正常创建任务并入队——否则上面那条 503 用例可能
// 只是因为「handler 整个坏掉了」而通过。
func TestCreateTaskAdmittedCreatesAndEnqueues(t *testing.T) {
	q := &fakeQueue{} // admitErr 为 nil → 准入通过
	r, metrics, ms := newTaskTestRouter(t, q)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"workflow_id":1,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("准入通过时应返回 201，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if q.enqueued != 1 {
		t.Fatalf("应入队 1 次，实际 %d", q.enqueued)
	}
	if w.Header().Get("X-Task-Id") == "" {
		t.Fatal("201 响应应带 X-Task-Id")
	}

	list, err := ms.Store.Tasks.ListTasks(context.Background(), 1, 100, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应创建 1 条任务，实际 %d 条", len(list))
	}
	if list[0].Status != "pending" {
		t.Fatalf("新任务状态应为 pending，实际 %s", list[0].Status)
	}

	// 准入通过时不应该计入 rejected
	if got := testutil.ToFloat64(metrics.QueueRejectedTotal); got != 0 {
		t.Fatalf("未被拒绝时 rejected_total 应为 0，实际 %v", got)
	}
}

// 入队失败（准入预检通过、但 Enqueue 时被拒）的兜底路径：
// 任务行必须被标成 failed，绝不能留在 pending —— 那正是 P0-5 要消灭的「幽灵任务」。
func TestCreateTaskEnqueueFailureMarksTaskFailed(t *testing.T) {
	q := &fakeQueue{}
	q.admitErr = nil
	r, _, ms := newTaskTestRouter(t, &enqueueFailQueue{fakeQueue: q})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"workflow_id":1,"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("入队失败应返回 503，实际 %d（body=%s）", w.Code, w.Body.String())
	}

	list, err := ms.Store.Tasks.ListTasks(context.Background(), 1, 100, 0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("这条路径上任务行已落库，应有 1 条，实际 %d 条", len(list))
	}
	if list[0].Status != "failed" {
		t.Fatalf("入队失败的任务必须标为 failed（不能留 pending），实际 %s", list[0].Status)
	}
}

// enqueueFailQueue 模拟「预检通过、但真正入队时队列已满」的 TOCTOU 窗口。
type enqueueFailQueue struct{ *fakeQueue }

func (f *enqueueFailQueue) Enqueue(ctx context.Context, j queue.Job) error {
	return queue.ErrQueueFull
}

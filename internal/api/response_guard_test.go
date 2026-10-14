package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
)

// 这个文件守的是「一个请求只写一个 JSON 响应体」这条最基本的 HTTP 契约。
//
// 缺陷回顾：`mustID` 在 `:id` 非法时先写道 400，但它无法阻止调用方继续执行
// （Go 里 `return` 只能写在调用点，被调函数没有任何机制能提前结束调用方）。
// 于是 handler 继续往下跑，最终又写了一次响应（404 或 200），
// 两个 JSON 被拼进同一个 body：
//
//	{"error":"invalid id parameter"}{"error":"task not found"}
//
// 单独看每一个 JSON 都是合法的，拼在一起就不是 JSON 了：
// 客户端 json.Unmarshal 直接失败。症状是"参数写错了"变成"响应损坏"——
// 服务端日志一切正常，只有客户端解析报错，属于最难定位的一类缺陷。
//
// 修复是**一处覆盖全部调用点**的：所有响应写出都汇总到 writeJSON / abortJSON，
// 响应已写出就丢弃后续写入（并 Abort 掉后续 handler）。

func newGuardRig(t *testing.T) (*gin.Engine, *store.MemoryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ms := store.NewMemory()
	metrics := observability.NewMetrics(prometheus.NewRegistry())
	taskSvc := task.NewService(ms.Store, &fakeQueue{}, task.NewHub())

	r := gin.New()
	// 直接注入认证用户，跳过 JWT——这里测的是响应写出，不是认证。
	r.Use(func(c *gin.Context) {
		c.Set(userKey, &ctxUser{ID: 1, Username: "tester"})
		c.Next()
	})

	r.GET("/api/tasks/:id", (&taskHandler{svc: taskSvc, metrics: metrics}).get)
	r.GET("/api/workflows/:id", (&workflowHandler{store: ms.Store}).get)
	r.GET("/api/knowledge-bases/:id/documents", (&knowledgeHandler{store: ms.Store}).listDocs)
	r.PUT("/api/providers/:id", (&providerHandler{store: ms.Store}).update)

	// trace 接口在 tracing 未启用时会直接回 501（连 mustID 都走不到），
	// 所以这里要真的开一个 provider，才能覆盖它的 mustID 路径。
	tp, err := tracing.Setup(context.Background(), tracing.Config{Enabled: true})
	if err != nil {
		t.Fatalf("tracing.Setup: %v", err)
	}
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	r.GET("/api/tasks/:id/trace", (&traceHandler{svc: taskSvc, tracing: tp}).byTask)
	return r, ms
}

func getWithBody(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

// 非法 `:id` 时，响应体必须是**单个**可解析的 JSON（而不是两个拼在一起）。
func TestInvalidIDWritesExactlyOneJSONBody(t *testing.T) {
	r, _ := newGuardRig(t)

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/tasks/abc", ""},
		{http.MethodGet, "/api/workflows/abc", ""},
		{http.MethodGet, "/api/knowledge-bases/abc/documents", ""},
		// 这条覆盖"mustID 之后还会写成功响应"的路径：
		// body 合法 → 绑定通过 → 继续查库（id=0）→ 又一个错误响应。
		{http.MethodPut, "/api/providers/abc", `{"base_url":"http://x"}`},
		// trace 接口也走 mustID（byTask 里 mustID 在 GetTask 之前）
		{http.MethodGet, "/api/tasks/abc/trace", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := getWithBody(r, tc.method, tc.path, tc.body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("非法 id 应返回 400，实际 %d（body=%s）", w.Code, w.Body.String())
			}
			var parsed map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
				t.Fatalf("响应体不是单个合法 JSON（两个 JSON 被拼在一个 body 里了）：%v\nbody=%q",
					err, w.Body.String())
			}
			// 第一个写出的响应必须胜出：错误文案是 mustID 给的，不是后面那个 404
			if got, _ := parsed["error"].(string); got != "invalid id parameter" {
				t.Fatalf("应保留第一个（400）响应的文案，实际 %q", got)
			}
		})
	}
}

// 对照组：合法 id 的正常路径不受影响——修好"多写"不能变成"少写"。
//
// 少了这条，上面的用例可能只是因为"整个响应链路被写坏了"而通过。
func TestValidIDStillWritesSingleJSONBody(t *testing.T) {
	r, ms := newGuardRig(t)

	tk := &model.Task{WorkflowID: 1, UserID: 1, Status: model.TaskPending, Input: "hi"}
	if err := ms.Store.Tasks.CreateTask(context.Background(), tk); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	w := getWithBody(r, http.MethodGet, "/api/tasks/"+strconv.FormatInt(tk.ID, 10), "")
	if w.Code != http.StatusOK {
		t.Fatalf("自己的任务应 200，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("正常响应的 body 必须是合法 JSON：%v（body=%q）", err, w.Body.String())
	}
	if parsed["id"] == nil {
		t.Fatalf("响应体应含任务字段，实际 %s", w.Body.String())
	}

	// 合法但不存在的 id → 单个 404 JSON（而不是 400，也不是两个 JSON）
	w2 := getWithBody(r, http.MethodGet, "/api/tasks/99999", "")
	if w2.Code != http.StatusNotFound {
		t.Fatalf("不存在的任务应 404，实际 %d（body=%s）", w2.Code, w2.Body.String())
	}
	var parsed2 map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &parsed2); err != nil {
		t.Fatalf("404 的 body 也必须是合法 JSON：%v（body=%q）", err, w2.Body.String())
	}
}

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// ---------- 缺陷 3：真实故障被伪装成 404 ----------

// ownedKBStore 是可控的 KnowledgeStore：只覆写 BelongsToUser，其余方法委托给
// 真实的内存实现。
//
// 为什么用"嵌入接口 + 覆写一个方法"，而不是手写 16 个方法：
// 手写的假实现很容易在别处与真实仓储的语义漂移，而那种漂移恰恰会让测试
// 在"和真实适配器不同的世界里"通过。嵌入真实实现，只替换要模拟的那一个方法。
type ownedKBStore struct {
	store.KnowledgeStore
	ok  bool
	err error
}

func (f ownedKBStore) BelongsToUser(context.Context, int64, int64) (bool, error) {
	return f.ok, f.err
}

func newKBRetrieveRig(t *testing.T, kb store.KnowledgeStore) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &knowledgeHandler{store: &store.Store{Knowledge: kb}}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(userKey, &ctxUser{ID: 1, Username: "tester"})
		c.Next()
	})
	r.GET("/api/knowledge-bases/:id/retrieve", h.retrieve)
	return r
}

// **本条是缺陷 3 的回归测试**：归属查询失败必须是 500，而不是 404。
//
// 缺陷回顾：`if ok, _ := h.store.Knowledge.BelongsToUser(...); !ok` 把 err 丢掉了。
// 后果是数据库/连接池故障时所有用户都收到 404「知识库不存在」：
// 监控上没有任何 5xx，用户却被告知"你的知识库没了"，
// 排查方向从一开始就完全错了（这与 auth.Login 把基础设施故障伪装成
// 凭据错误是同一类缺陷）。
func TestKnowledgeRetrieveStoreErrorIsInternalErrorNot404(t *testing.T) {
	ms := store.NewMemory()
	dbErr := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	r := newKBRetrieveRig(t, ownedKBStore{KnowledgeStore: ms.Store.Knowledge, err: dbErr})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/knowledge-bases/7/retrieve?q=hello", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("存储层故障应返回 500，实际 %d（body=%s）—— 404 说明 err 被吞掉了",
			w.Code, w.Body.String())
	}
	// 顺带守住缺陷 4：500 的响应体里不能出现内部地址/端口/驱动细节
	for _, leak := range []string{"10.0.0.5", "5432", "dial tcp"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("500 响应泄露了内部信息 %q：%s", leak, w.Body.String())
		}
	}
}

// 对照组：确实"不是你的知识库"（err == nil, ok == false）必须还是 404。
// 少了这一条，上面的修复可能被写成"一律 500"，那同样是把故障性质搞错。
func TestKnowledgeRetrieveNotOwnerIs404(t *testing.T) {
	ms := store.NewMemory()
	r := newKBRetrieveRig(t, ownedKBStore{KnowledgeStore: ms.Store.Knowledge, ok: false})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/knowledge-bases/7/retrieve?q=hello", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("无权访问应 404（不能暴露「存在但无权」这个信息），实际 %d（body=%s）",
			w.Code, w.Body.String())
	}
}

// ---------- 缺陷 4：500 泄露内部信息 ----------

// 未知/基础设施错误的文本不得回给客户端。
//
// 缺陷回顾：abortWithError 的 default 分支是
// `gin.H{"error": err.Error()}`，把驱动/SQL/内网地址原样回给调用方。
// 实测能拿到这样的响应体：
//
//	{"error":"pq: relation \"users\" does not exist (dial tcp 10.1.2.3:5432)"}
//
// 那等于把内部拓扑免费送给任何一个能触发一次 500 的人。
func TestUnknownErrorResponseDoesNotLeakInternals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	internal := errors.New(`pq: relation "users" does not exist (dial tcp 10.1.2.3:5432)`)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/workflows", nil)

	abortWithError(c, internal)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("未知错误应 500，实际 %d", w.Code)
	}
	body := w.Body.String()
	for _, leak := range []string{"pq:", "users", "10.1.2.3", "5432", "relation"} {
		if strings.Contains(body, leak) {
			t.Fatalf("500 响应泄露了内部信息 %q：%s", leak, body)
		}
	}
	// 但必须是**可解析且有意义**的固定文案，而不是空 body。
	// 空 body 会让前端只能显示"未知错误"，同样无法排查。
	if !strings.Contains(body, "internal server error") {
		t.Fatalf("500 应给出固定的可展示文案，实际 %s", body)
	}
}

// 反向守卫：**显式标记为可展示**的领域错误必须保留原文。
//
// 这条很重要：修复"500 泄露"最容易的做法是无脑把所有错误都换成固定文案，
// 那样用户看到"密码错误"时也不知道到底错在哪（是太短？太长？格式不对？）。
// 领域错误的文案是我们自己写的、专门给用户看的，必须原样透出。
func TestClientVisibleDomainErrorsKeepTheirText(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantInBody string
	}{
		{"密码过短", auth.ErrPasswordTooShort, http.StatusBadRequest, "at least 6 characters"},
		{"密码过长", auth.ErrPasswordTooLong, http.StatusBadRequest, "at most 72 bytes"},
		{"无法哈希的密码", auth.ErrWeakPassword, http.StatusBadRequest, "invalid password"},
		{"参数校验", store.ErrValidation, http.StatusBadRequest, "validation error"},
		{"用户已存在", store.ErrUserExists, http.StatusConflict, "already exists"},
		{"资源不存在", store.ErrWorkflowNotFound, http.StatusNotFound, "workflow not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)

			abortWithError(c, tc.err)

			if w.Code != tc.wantStatus {
				t.Fatalf("状态码应为 %d，实际 %d（body=%s）", tc.wantStatus, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantInBody) {
				t.Fatalf("可展示的领域错误必须保留原文（含 %q），实际 %s", tc.wantInBody, w.Body.String())
			}
		})
	}
}

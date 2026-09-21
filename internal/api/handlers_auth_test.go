package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// 这个文件补上认证接口的**端到端 HTTP 契约**测试。
//
// 为什么一定要走完整路由而不是直接调 abortWithError：
// 这里要挡的缺陷恰恰是"两层各自都对、接起来就错"的那一类。
// 典型例子就是重复注册返回 500 而不是 409——
// store 层返回 store.ErrUserExists 是对的，api 层映射 ErrUserExists → 409 也是对的，
// 但两者比的是**不同的错误值**，只有端到端跑一遍才会暴露。

const testJWTSecret = "0123456789abcdef0123456789abcdef"

// newAuthTestRouter 装配一个只挂认证路由的最小路由。
func newAuthTestRouter(t *testing.T) (*gin.Engine, *store.MemoryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ms := store.NewMemory()
	svc := auth.NewService(ms.Store.Users, testJWTSecret, time.Hour)
	h := &authHandler{svc: svc}

	r := gin.New()
	g := r.Group("/api/auth")
	g.POST("/register", h.register)
	g.POST("/login", h.login)
	g.GET("/me", authMiddleware(svc), h.me)
	return r, ms
}

func postJSON(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// P1-6 的核心用例，也是本次发现的缺陷的回归测试。
//
// 缺陷回顾：auth 包里曾有一个与 store 包里**同名、同消息、不同值**的
// ErrUserExists。api 层写的是 `errors.Is(err, auth.ErrUserExists)`，
// 而 store.UserRepo 返回的是 `store.ErrUserExists`——errors.Is 永远为假，
// 于是重复注册掉进 default 分支变成 500。
//
// 这类缺陷的特殊之处：单看任何一层都"是对的"，两个包的测试也都过，
// 只有把注册请求真的打两遍才会露出来。
func TestRegisterDuplicateUsernameReturns409NotInternalError(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	body := `{"username":"alice","email":"alice@example.com","password":"password1"}`
	if w := postJSON(r, "/api/auth/register", body); w.Code != http.StatusCreated {
		t.Fatalf("首次注册应返回 201，实际 %d（body=%s）", w.Code, w.Body.String())
	}

	// 同用户名、不同邮箱
	w := postJSON(r, "/api/auth/register",
		`{"username":"alice","email":"other@example.com","password":"password2"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("重复用户名应返回 409，实际 %d（body=%s）—— 500 说明错误映射没匹配上",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "already exists") {
		t.Fatalf("409 响应应说明冲突原因，实际 %s", w.Body.String())
	}

	// 同邮箱、不同用户名（users 表对 email 也有唯一约束）
	w = postJSON(r, "/api/auth/register",
		`{"username":"bob","email":"alice@example.com","password":"password3"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("重复邮箱应返回 409，实际 %d（body=%s）", w.Code, w.Body.String())
	}
}

func TestRegisterValidationReturns400(t *testing.T) {
	cases := []struct{ name, body string }{
		{"密码不足 6 位", `{"username":"alice","email":"a@example.com","password":"12345"}`},
		{"用户名不足 3 位", `{"username":"ab","email":"a@example.com","password":"password1"}`},
		{"邮箱格式非法", `{"username":"alice","email":"not-an-email","password":"password1"}`},
		{"缺字段", `{"username":"alice"}`},
		{"不是 JSON", `not json at all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newAuthTestRouter(t)
			w := postJSON(r, "/api/auth/register", tc.body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("应返回 400，实际 %d（body=%s）", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "error") {
				t.Fatalf("400 响应应含 error 字段，实际 %s", w.Body.String())
			}
		})
	}
}

// 参数校验必须发生在落库之前。
//
// 否则用户修正密码后重试会撞上唯一约束，看到的是「用户已存在」
// 而不是「密码太短」——一条完全误导的错误，而且用户永远绕不出去。
func TestRegisterShortPasswordCreatesNoUser(t *testing.T) {
	r, ms := newAuthTestRouter(t)

	w := postJSON(r, "/api/auth/register",
		`{"username":"alice","email":"a@example.com","password":"12345"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if _, err := ms.Store.Users.GetUserByUsername(context.Background(), "alice"); !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("密码过短不应创建用户，实际 err=%v", err)
	}

	// 对照组：同一个用户名 + 合法密码必须能注册成功。
	// 少了这一条，上面的断言可能只是因为"注册整个坏掉了"而通过。
	w = postJSON(r, "/api/auth/register",
		`{"username":"alice","email":"a@example.com","password":"password1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("修正密码后重试应成功，实际 %d（body=%s）", w.Code, w.Body.String())
	}
}

// 注册成功要返回可用 token，并且该 token 能直接访问受保护接口。
// 少了这条，上面那些失败用例可能只是因为"注册整个坏掉了"而通过。
func TestRegisterThenTokenWorksOnMe(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	w := postJSON(r, "/api/auth/register",
		`{"username":"alice","email":"a@example.com","password":"password1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("注册应返回 201，实际 %d（body=%s）", w.Code, w.Body.String())
	}

	token := extractToken(t, w.Body.String())
	me := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(me, req)

	if me.Code != http.StatusOK {
		t.Fatalf("带 token 访问 /me 应返回 200，实际 %d（body=%s）", me.Code, me.Body.String())
	}
	if !strings.Contains(me.Body.String(), "alice") {
		t.Fatalf("/me 应返回用户名，实际 %s", me.Body.String())
	}
}

// 用户名枚举防护的 HTTP 层契约：
// 「用户不存在」与「密码错误」必须返回**逐字节相同**的响应体。
//
// 只要两者可区分，攻击者就能先枚举出有效用户名，再集中爆破密码；
// 而用户名往往是邮箱，本身就泄露了组织信息。
func TestLoginWrongPasswordAndUnknownUserAreIndistinguishable(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	if w := postJSON(r, "/api/auth/register",
		`{"username":"alice","email":"a@example.com","password":"password1"}`); w.Code != http.StatusCreated {
		t.Fatalf("注册应返回 201，实际 %d（body=%s）", w.Code, w.Body.String())
	}

	wrongPwd := postJSON(r, "/api/auth/login", `{"username":"alice","password":"wrong-password"}`)
	unknown := postJSON(r, "/api/auth/login", `{"username":"nobody","password":"whatever"}`)

	if wrongPwd.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("两种失败都应返回 401，实际 密码错误=%d 用户不存在=%d",
			wrongPwd.Code, unknown.Code)
	}
	if wrongPwd.Body.String() != unknown.Body.String() {
		t.Fatalf("两种失败的响应体必须一致（否则可枚举用户名）：\n密码错误  = %s\n用户不存在= %s",
			wrongPwd.Body.String(), unknown.Body.String())
	}
}

func TestLoginSuccessReturnsToken(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	if w := postJSON(r, "/api/auth/register",
		`{"username":"alice","email":"a@example.com","password":"password1"}`); w.Code != http.StatusCreated {
		t.Fatalf("注册应返回 201，实际 %d（body=%s）", w.Code, w.Body.String())
	}

	w := postJSON(r, "/api/auth/login", `{"username":"alice","password":"password1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("登录应返回 200，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	extractToken(t, w.Body.String()) // 解析失败会直接 Fail
}

// /me 的鉴权契约：缺失、格式错误、伪造的 token 都必须 401，
// 且**不能**因为 token 解析失败而 500（那会把攻击流量变成 5xx 告警噪声）。
func TestMeRejectsBadAuthorizationHeaders(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	cases := []struct{ name, header string }{
		{"完全没有 Authorization 头", ""},
		{"只有 Bearer 前缀", "Bearer "},
		{"缺少 Bearer 前缀", "abcdef"},
		{"前缀对但 token 是伪造的", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJ1aWQiOjF9.forged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("应返回 401，实际 %d（body=%s）", w.Code, w.Body.String())
			}
		})
	}
}

// auth-scheme 必须大小写不敏感（RFC 7235 §2.1）。
//
// 原实现是 strings.HasPrefix(header, "Bearer ")，严格区分大小写：
// 客户端发 `bearer <token>` 会被判成"缺少 token"而 401，
// 而响应文案说的是 missing bearer token —— 它明明发了，排查起来非常费劲。
// 这类缺陷不会在自家前端暴露（自家前端写的是 "Bearer "），
// 只在第三方客户端或 SDK 集成时冒出来。
func TestAuthMiddlewareAcceptsCaseInsensitiveScheme(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	w := postJSON(r, "/api/auth/register",
		`{"username":"alice","email":"a@example.com","password":"password1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("注册应返回 201，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	token := extractToken(t, w.Body.String())

	for _, scheme := range []string{"Bearer ", "bearer ", "BEARER ", "BeArEr "} {
		t.Run(scheme, func(t *testing.T) {
			me := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			req.Header.Set("Authorization", scheme+token)
			r.ServeHTTP(me, req)

			if me.Code != http.StatusOK {
				t.Fatalf("scheme %q 应被接受，实际 %d（body=%s）", scheme, me.Code, me.Body.String())
			}
		})
	}
}

// extractToken 从 {"token":"..."} 里取出 token。
// 刻意用最小解析而不是引入 encoding/json 的完整解码，
// 是为了让失败信息里能看到原始响应体（否则只报"解析失败"，很难定位）。
func extractToken(t *testing.T, body string) string {
	t.Helper()
	const key = `"token":"`
	i := strings.Index(body, key)
	if i < 0 {
		t.Fatalf("响应里没有 token 字段：%s", body)
	}
	rest := body[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("token 字段格式异常：%s", body)
	}
	token := rest[:j]
	if token == "" {
		t.Fatalf("token 为空：%s", body)
	}
	return token
}

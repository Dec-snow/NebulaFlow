package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/ratelimit"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// ---------- 缺陷 5：认证接口无限流 ----------

func newAuthLimitRouter(t *testing.T, perMin int) (*gin.Engine, *auth.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ms := store.NewMemory()
	svc := auth.NewService(ms.Store.Users, testJWTSecret, time.Hour)
	h := &authHandler{svc: svc}
	limit := authRateLimitMiddleware(ratelimit.NewMemoryLimiter(), perMin)

	r := gin.New()
	g := r.Group("/api/auth")
	// 与 server.go 的装配一致：注册与登录共用同一维度（IP + 用户名）
	g.POST("/register", limit, h.register)
	g.POST("/login", limit, h.login)
	return r, svc
}

// **本条是缺陷 5 的回归测试**：猜密码超过阈值后必须是 429，而不是无限 401。
//
// 缺陷回顾：server.go 里 api 组挂了 rateLimitMiddleware，而 authGroup 什么限流都没挂。
// 而 rateLimitMiddleware 按 `user:{id}` 计数——注册/登录发生在拿到 user **之前**，
// `u == nil` 时它会直接放行。于是认证接口成了全站唯一没有限流的写入口：
// 攻击者可以无限次猜密码，每次都是 401，监控上不产生任何 5xx，
// 看起来"系统非常健康"，而密码可能已经被撞开。
func TestAuthLoginIsRateLimitedInsteadOfUnlimited401(t *testing.T) {
	const perMin = 3
	r, svc := newAuthLimitRouter(t, perMin)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册 alice: %v", err)
	}

	const body = `{"username":"alice","password":"wrong-password"}`
	for i := 1; i <= perMin; i++ {
		w := postJSON(r, "/api/auth/login", body)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误密码应 401（还在阈值内），实际 %d（body=%s）", i, w.Code, w.Body.String())
		}
	}

	w := postJSON(r, "/api/auth/login", body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超过阈值（%d 次/分钟）后必须 429，实际 %d（body=%s），"+
			"401 说明这个接口可以被无限爆破", perMin, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("429 应带 error 字段，实际 %s", w.Body.String())
	}
	if got := w.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("429 响应应带 X-RateLimit-Remaining: 0，实际 %q", got)
	}

	// 即使密码**正确**也应该被挡：限流发生在认证之前是有意为之，
	// 否则攻击者可以靠"撞对了就放行"从响应差异里判断密码是否正确。
	if w := postJSON(r, "/api/auth/login", `{"username":"alice","password":"password1"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("被限流后即使密码正确也应 429，实际 %d", w.Code)
	}

	// 另一个用户名不该被连累：限流维度是 IP + 用户名，
	// 而不是"整个出口 IP 一起罚"（否则公司 NAT 后面的所有人都会被邻居拖下水）。
	if w := postJSON(r, "/api/auth/login", `{"username":"bob","password":"whatever"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("其他用户名不应受 alice 的失败次数影响，实际 %d（body=%s）", w.Code, w.Body.String())
	}
}

// 注册接口同样有限流（批量注册是另一种滥用）。
//
// 注意这里刻意用**同一个用户名**重复提交：限流键是 IP + 用户名，
// 换用户名就等于换了一个计数桶。第 2 次会撞上唯一约束（409），
// 但"被业务拒绝"和"被限流拒绝"是两件事——它照样要占一个配额。
func TestAuthRegisterIsRateLimited(t *testing.T) {
	const perMin = 2
	r, _ := newAuthLimitRouter(t, perMin)

	const body = `{"username":"dup","email":"dup@example.com","password":"password1"}`
	for i := 1; i <= perMin; i++ {
		w := postJSON(r, "/api/auth/register", body)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("阈值内（第 %d 次）不应 429，实际 %d（body=%s）", i, w.Code, w.Body.String())
		}
		if w.Code != http.StatusCreated && w.Code != http.StatusConflict {
			t.Fatalf("第 %d 次注册应 201/409，实际 %d（body=%s）", i, w.Code, w.Body.String())
		}
	}

	w := postJSON(r, "/api/auth/register", body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超过阈值后注册应 429，实际 %d（body=%s）—— 无限流时批量注册可以无限刷",
			w.Code, w.Body.String())
	}
}

// 限流中间件"偷看"用户名之后必须把请求体还给 handler。
//
// 这条挡的是"安全补丁把功能打坏"这一类事故：读完 body 不还原，
// handler 只会看到 EOF，于是所有登录都变成 400（而不是 401）——
// 限流上线当天所有用户都登不进来。
func TestAuthRateLimitPreservesRequestBody(t *testing.T) {
	r, svc := newAuthLimitRouter(t, 10)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册 alice: %v", err)
	}

	w := postJSON(r, "/api/auth/login", `{"username":"alice","password":"password1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("限流中间件读完 body 后必须还原，否则绑定会失败；实际 %d（body=%s）",
			w.Code, w.Body.String())
	}
	extractToken(t, w.Body.String())
}

// ---------- 缺陷 7：超长密码 500 + 泄露库内文案 ----------

// **本条是缺陷 7 的回归测试**：73 / 200 字节的密码必须 400，且响应体不含 "bcrypt"。
//
// 缺陷回顾：registerReq 的密码校验只有 `min=6`，没有上限，而 bcrypt 只处理
// 前 72 **字节**，超了直接返回 ErrPasswordTooLong。原实现把它当成内部故障：
//
//	500 {"error":"hash password: bcrypt: password length exceeds 72 bytes"}
//
// 两句都不对：把一个"用户输入不合法"报成"服务端故障"，还把哈希库的名字、
// 它的缓冲区大小一并告诉了客户端。
func TestRegisterOverlongPasswordIs400AndDoesNotLeakBcrypt(t *testing.T) {
	r, ms := newAuthTestRouter(t)

	for _, n := range []int{73, 200} {
		t.Run("len="+strconv.Itoa(n), func(t *testing.T) {
			pw := strings.Repeat("a", n)
			body := `{"username":"alice","email":"a@example.com","password":"` + pw + `"}`

			w := postJSON(r, "/api/auth/register", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%d 字节的密码应返回 400，实际 %d（body=%s）—— 500 说明 bcrypt 错误被当成了服务端故障",
					n, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "bcrypt") {
				t.Fatalf("响应体泄露了哈希库实现细节：%s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "72") {
				t.Fatalf("应告诉用户长度上限是多少，实际 %s", w.Body.String())
			}
			// 被拒绝的密码不得留下"半成品"用户：否则用户改短密码重试时
			// 会撞上唯一约束，看到"用户已存在"这条完全误导的错误。
			if _, err := ms.Store.Users.GetUserByUsername(context.Background(), "alice"); err == nil {
				t.Fatal("超长密码不应创建用户")
			}
		})
	}
}

// 上限必须按**字节**而不是字符：bcrypt 的 72 是字节数。
// 25 个中文字符只有 25 个字符却有 75 字节——按字符数判上限会放行，然后挂在 bcrypt 上。
func TestPasswordLimitIsBytesNotRunes(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	// 25 个中文字符 = 75 字节 → 必须被拒
	over := strings.Repeat("密", 25)
	if len(over) != 75 {
		t.Fatalf("用例自身有误：应为 75 字节，实际 %d", len(over))
	}
	w := postJSON(r, "/api/auth/register", `{"username":"alice","email":"a@example.com","password":"`+over+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("75 字节（25 字符）的密码应 400，实际 %d（body=%s）——按字符数判上限就会漏掉它",
			w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "bcrypt") {
		t.Fatalf("响应体泄露了哈希库实现细节：%s", w.Body.String())
	}

	// 24 个中文字符 = 72 字节 → 正好在上限，必须被接受（否则就是过度拒绝）
	ok := strings.Repeat("密", 24)
	if len(ok) != 72 {
		t.Fatalf("用例自身有误：应为 72 字节，实际 %d", len(ok))
	}
	w2 := postJSON(r, "/api/auth/register", `{"username":"bob","email":"b@example.com","password":"`+ok+`"}`)
	if w2.Code != http.StatusCreated {
		t.Fatalf("正好 72 字节的密码应被接受（边界不能过度拒绝），实际 %d（body=%s）",
			w2.Code, w2.Body.String())
	}
	// 而且这个密码必须能登录回来说明它真的被哈希了
	w3 := postJSON(r, "/api/auth/login", `{"username":"bob","password":"`+ok+`"}`)
	if w3.Code != http.StatusOK {
		t.Fatalf("用 72 字节密码登录应 200，实际 %d（body=%s）", w3.Code, w3.Body.String())
	}
}

// 登录接口对超长密码同样回 400（而不是 500，也不去跑注定失败的 bcrypt）。
func TestLoginOverlongPasswordIs400(t *testing.T) {
	r, _ := newAuthTestRouter(t)

	pw := strings.Repeat("a", 200)
	w := postJSON(r, "/api/auth/login", `{"username":"alice","password":"`+pw+`"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超长密码登录应 400，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "bcrypt") {
		t.Fatalf("响应体泄露了哈希库实现细节：%s", w.Body.String())
	}
}

// 用探针请求确认：认证接口不再返回任何 5xx（把"400 还是 500"这件事钉死）。
func TestAuthEndpointsNeverReturn5xxForBadInput(t *testing.T) {
	r, _ := newAuthLimitRouter(t, 100)

	cases := []string{
		`{"username":"alice","password":"` + strings.Repeat("a", 5000) + `"}`,
		`{"username":"","password":""}`,
		`not json`,
		`{"username":"alice"}`,
	}
	for _, body := range cases {
		w := postJSON(r, "/api/auth/login", body)
		if w.Code >= http.StatusInternalServerError {
			t.Fatalf("非法输入不应产生 5xx，实际 %d（body=%s）", w.Code, w.Body.String())
		}
	}
}

// httptest 里 RemoteAddr 默认是 192.0.2.1:1234；这里显式改成别的网段，
// 确认"换 IP 就换 key"（IP 维度确实参与了计数）。
func TestAuthRateLimitKeyIncludesClientIP(t *testing.T) {
	r, svc := newAuthLimitRouter(t, 1)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册 alice: %v", err)
	}

	post := func(remote string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
			strings.NewReader(`{"username":"alice","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		r.ServeHTTP(w, req)
		return w.Code
	}

	if got := post("192.0.2.10:1234"); got != http.StatusUnauthorized {
		t.Fatalf("第一个 IP 的首次尝试应 401，实际 %d", got)
	}
	if got := post("192.0.2.10:1234"); got != http.StatusTooManyRequests {
		t.Fatalf("同一 IP 超过阈值应 429，实际 %d", got)
	}
	// 说明：key 是 IP+用户名 的组合。换 IP 会换 key —— 这正是"不因为一个 IP 的
	// 坏邻居把所有人挡住"的代价，真实的暴力破解防护还需要在网关层按 IP 兜底。
	if got := post("198.51.100.7:1234"); got != http.StatusUnauthorized {
		t.Fatalf("不同 IP 属于不同计数维度，应 401，实际 %d", got)
	}
}

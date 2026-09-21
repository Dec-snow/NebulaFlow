package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// 这个文件补上 auth 包的首批测试。
//
// auth 是"有实际逻辑且风险最高"的零测试包之一：它决定谁能进系统。
// 这里覆盖三类事情：
//   - 注册/登录的业务规则（密码长度、密码哈希、重复用户）
//   - **基础设施故障不能被伪装成凭据错误**（数据库挂了不该报"密码错误"）
//   - 令牌校验的负面路径（过期、篡改、换密钥、算法混淆）

// fakeStore 刻意**复刻真实仓储的错误语义**，而不是返回自定义错误。
//
// 这一点是这个文件里最关键的测试设计：真实的 store.UserRepo 在唯一约束冲突时
// 返回 `store.ErrUserExists`、查不到用户时返回 `store.ErrUserNotFound`。
// 如果 fake 返回别的错误，测试就会在"和真实适配器不同的世界里"通过——
// 而 API 层的状态码映射恰恰依赖这两个哨兵值。
type fakeStore struct {
	users    map[string]*model.User // username → user
	byEmail  map[string]bool
	nextID   int64
	forceErr error // 非 nil 时 GetUserByUsername 直接返回它，模拟数据库故障
}

func newFakeStore() *fakeStore {
	return &fakeStore{users: map[string]*model.User{}, byEmail: map[string]bool{}}
}

func (f *fakeStore) CreateUser(_ context.Context, u *model.User) error {
	if _, ok := f.users[u.Username]; ok {
		return store.ErrUserExists
	}
	if f.byEmail[u.Email] {
		return store.ErrUserExists
	}
	f.nextID++
	u.ID = f.nextID
	f.users[u.Username] = u
	f.byEmail[u.Email] = true
	return nil
}

func (f *fakeStore) GetUserByUsername(_ context.Context, username string) (*model.User, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	u, ok := f.users[username]
	if !ok {
		return nil, store.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeStore) GetUserByID(_ context.Context, id int64) (*model.User, error) {
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, store.ErrUserNotFound
}

const testSecret = "0123456789abcdef0123456789abcdef"

func newTestService(f *fakeStore) *Service {
	return NewService(f, testSecret, time.Hour)
}

// ---------- 注册 ----------

func TestRegisterRejectsShortPassword(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)

	_, err := svc.Register(context.Background(), "alice", "alice@example.com", "12345")
	if err == nil {
		t.Fatal("5 位密码应被拒绝")
	}
	// 拒绝必须发生在落库之前：否则会留下一个"半成品"用户，
	// 后续用同一个用户名重试会撞上唯一约束，用户看到的是"已存在"而不是"密码太短"。
	if len(f.users) != 0 {
		t.Fatalf("密码过短时不应创建用户，实际创建了 %d 个", len(f.users))
	}
}

// 密码必须以哈希形式落库。这条断言看着很废话，但它挡的是
// "把明文塞进 PasswordHash 字段"这类事故——那类改动在功能上完全正常，
// 只有安全审计或数据泄露时才会暴露。
func TestRegisterHashesPassword(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)

	const plain = "s3cret-password"
	u, err := svc.Register(context.Background(), "alice", "alice@example.com", plain)
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if u.PasswordHash == plain {
		t.Fatal("密码不能以明文形式落库")
	}
	if !strings.HasPrefix(u.PasswordHash, "$2") {
		t.Fatalf("密码应是 bcrypt 哈希（$2a$/$2b$ 前缀），实际 %q", u.PasswordHash)
	}
	// 哈希必须可验证，否则登录永远失败
	if _, _, err := svc.Login(context.Background(), "alice", plain); err != nil {
		t.Fatalf("用原密码登录应成功，实际 %v", err)
	}
}

// 重复注册必须把 store 的唯一约束错误原样透出。
//
// 这条断言连接的是 API 层的 409 契约：api 层用 errors.Is 匹配
// `store.ErrUserExists` 来决定返回 409 还是 500。
// 如果这里换成一个自定义错误，409 就永远不会触发——
// 而这个缺陷单看 api 层或单看 store 层都看不出来，只有端到端才暴露。
func TestRegisterDuplicateUsernamePropagatesStoreSentinel(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)

	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}

	// 同用户名、不同邮箱
	_, err := svc.Register(context.Background(), "alice", "other@example.com", "password2")
	if !errors.Is(err, store.ErrUserExists) {
		t.Fatalf("重复用户名应返回 store.ErrUserExists，实际 %v", err)
	}

	// 同邮箱、不同用户名（users 表对 email 也有唯一约束）
	_, err = svc.Register(context.Background(), "bob", "a@example.com", "password3")
	if !errors.Is(err, store.ErrUserExists) {
		t.Fatalf("重复邮箱应返回 store.ErrUserExists，实际 %v", err)
	}
}

// 密码长度必须按**字符数**计，而不是字节数（P3-15）。
//
// 原实现是 `len(password) < 6`，而 Go 的 len 对 string 返回的是**字节数**。
// 对 ASCII 两者相同，所以这个缺陷对英文用户完全不可见；
// 而中文密码 `"密码密"` 只有 3 个字符却有 9 字节，会被判成"足够长"而放行——
// 错误消息里写的却是 "at least 6 characters"，说的是字符，量的是字节。
//
// **这条规则只有这一类输入能拦住**：所有 ASCII 短密码（如 "12345"）
// 用字节数也能拦住，因此 TestRegisterRejectsShortPassword 覆盖不到它。
// 这正是"每条规则都必须有一个只有它能拦住的输入"的又一个实例。
func TestRegisterCountsPasswordInCharactersNotBytes(t *testing.T) {
	cases := []struct {
		name     string
		password string
		runes    int
		bytes    int
	}{
		{"中文 3 字符", "密码密", 3, 9},
		{"中文 4 字符", "密码密码", 4, 12},
		{"希腊字母 5 字符", "ααααα", 5, 10},
		{"emoji 5 字符", "😀😀😀😀😀", 5, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 装置自检一：用例自身的字符数/字节数必须与预期一致。
			if got := utf8.RuneCountInString(tc.password); got != tc.runes {
				t.Fatalf("用例自身有误：%q 应为 %d 个字符，实际 %d", tc.password, tc.runes, got)
			}
			if got := len(tc.password); got != tc.bytes {
				t.Fatalf("用例自身有误：%q 应为 %d 字节，实际 %d", tc.password, tc.bytes, got)
			}
			// 装置自检二（关键）：字节数必须达标。
			// 否则按字节数的校验同样能拦住它，这条用例就无法区分修复前后——
			// 测试会在"修复前"也通过，那它就什么也没证明。
			if tc.bytes < 6 {
				t.Fatalf("用例无效：%q 只有 %d 字节，按字节数的校验也能拦住它", tc.password, tc.bytes)
			}

			f := newFakeStore()
			svc := newTestService(f)
			if _, err := svc.Register(context.Background(), "alice", "a@example.com", tc.password); err == nil {
				t.Fatalf("%d 个字符的密码应被拒绝（尽管它有 %d 字节）", tc.runes, tc.bytes)
			}
			if len(f.users) != 0 {
				t.Fatalf("密码过短时不应创建用户，实际创建了 %d 个", len(f.users))
			}
		})
	}
}

// 反向守卫：修复不能变成"越严越好"。6 个字符（无论多少字节）必须被接受。
//
// 注意按字符数计是**严格更宽松**的（字符数 ≤ 字节数），所以修复只会让
// "多字节但字符不足"的密码被拒，不会误伤任何原本合法的密码。
func TestRegisterAcceptsSixCharacterPassword(t *testing.T) {
	for _, pw := range []string{"123456", "密码密码密码", "αααααα"} {
		if got := utf8.RuneCountInString(pw); got != 6 {
			t.Fatalf("用例自身有误：%q 应为 6 个字符，实际 %d", pw, got)
		}
		f := newFakeStore()
		svc := newTestService(f)
		if _, err := svc.Register(context.Background(), "alice", "a@example.com", pw); err != nil {
			t.Errorf("6 个字符的密码 %q 应被接受，实际 %v", pw, err)
		}
	}
}

// ---------- 登录 ----------

func TestLoginSuccess(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	token, u, err := svc.Login(context.Background(), "alice", "password1")
	if err != nil {
		t.Fatalf("登录应成功: %v", err)
	}
	if token == "" {
		t.Fatal("登录应返回 token")
	}
	if u == nil || u.Username != "alice" {
		t.Fatalf("登录应返回用户，实际 %+v", u)
	}

	// 登录签发的 token 必须能被自己的 Parse 校验通过
	claims, err := svc.Parse(token)
	if err != nil {
		t.Fatalf("登录签发的 token 应可解析: %v", err)
	}
	if claims.UserID != u.ID || claims.Username != "alice" {
		t.Fatalf("claims 应携带 uid/username，实际 %+v", claims)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	_, _, err := svc.Login(context.Background(), "alice", "wrong-password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("密码错误应返回 ErrInvalidCredentials，实际 %v", err)
	}
}

// 用户不存在与密码错误必须返回**同一个**错误。
// 区分开会让攻击者能枚举有效用户名（先确认用户名存在，再集中爆破密码）。
func TestLoginUnknownUserLooksLikeWrongPassword(t *testing.T) {
	svc := newTestService(newFakeStore())

	_, _, err := svc.Login(context.Background(), "nobody", "whatever")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("用户不存在应返回 ErrInvalidCredentials（与密码错误不可区分），实际 %v", err)
	}
}

// 数据库故障不能被伪装成凭据错误。
//
// 这是本文件里最重要的一条断言。原实现是
//
//	if err != nil { return "", nil, ErrInvalidCredentials }
//
// 把**所有**查询错误都当成"用户不存在"。后果是 PG 挂掉或连接池耗尽时：
//   - 所有用户收到 401「用户名或密码错误」，而监控上不出现任何 5xx；
//   - 客服收到一堆"我的密码怎么突然错了"，排查方向从一开始就是错的；
//   - 真实故障被一条看起来最无害的错误路径吞掉。
//
// 正确行为：只有"用户不存在"是凭据错误，其他一律原样上报，由 API 层映射成 5xx。
func TestLoginDoesNotMaskInfrastructureErrors(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 模拟数据库故障：注意不能是 ErrUserNotFound
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	f.forceErr = dbErr

	_, _, err := svc.Login(context.Background(), "alice", "password1")
	if err == nil {
		t.Fatal("数据库故障时登录不应成功")
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("数据库故障被伪装成了凭据错误（这正是要修的缺陷）：%v", err)
	}
	if !errors.Is(err, dbErr) {
		t.Fatalf("原始错误应被保留（%v 包一层），实际 %v", dbErr, err)
	}
}

// 时间侧信道：用户不存在时也必须跑 bcrypt，使两条失败路径耗时一致。
//
// 原缺陷：Login 在"用户不存在"时直接返回（不跑 bcrypt），
// 而"密码错误"跑一次 bcrypt.CompareHashAndPassword（约 60ms）。
// 攻击者可以通过测量响应耗时判断用户名是否有效——
// 先确认有效用户名，再集中爆破密码（用户名枚举攻击）。
//
// 修复：在"用户不存在"分支也跑一次 dummy bcrypt 比对。
//
// 这条测试用**时间比值**而非绝对耗时来判别——
// bcrypt 是故意设计成慢的（~60ms@cost10），所以：
//   - 修复后：两条路径都跑 bcrypt，耗时在同一数量级（比值 < 5x）
//   - 修复前：用户不存在不跑 bcrypt，比值 > 100x
//
// 用比值而非绝对值是因为跨机器的绝对时间不稳定。
func TestLoginUserNotFoundRunsBcrypt(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 装置自检：dummyHash 必须是合法 bcrypt 哈希，否则比对会立即返回错误
	// 而不消耗 CPU 时间——那侧信道缓解就形同虚设。
	if !strings.HasPrefix(string(dummyHash), "$2") {
		t.Fatalf("dummyHash 应是 bcrypt 哈希（$2 前缀），实际 %q", string(dummyHash))
	}
	// 装置自检二：dummyHash 比对结果必须"不匹配"——
	// 如果它碰巧匹配了测试密码，那这个 dummy 就不是 dummy 了。
	if bcrypt.CompareHashAndPassword(dummyHash, []byte("password1")) == nil {
		t.Fatal("dummyHash 不应匹配任何真实密码（测试密码匹配了 = dummy 失效）")
	}

	ctx := context.Background()

	// 测量"用户不存在"耗时
	unknownStart := time.Now()
	_, _, _ = svc.Login(ctx, "nobody", "whatever")
	unknownElapsed := time.Since(unknownStart)

	// 测量"密码错误"耗时（这条路径一直跑 bcrypt，是基准）
	wrongStart := time.Now()
	_, _, _ = svc.Login(ctx, "alice", "wrong-password")
	wrongElapsed := time.Since(wrongStart)

	// 判别性断言：用户不存在时也必须跑了 bcrypt。
	// bcrypt@DefaultCost(10) 在大多数机器上 > 20ms。
	// 如果缺陷存在（不跑 bcrypt），耗时 < 1ms。
	// 这里用 5ms 作为阈值——远低于 bcrypt 的实际耗时，
	// 但远高于"不跑 bcrypt"的 ~0ms。
	if unknownElapsed < 5*time.Millisecond {
		t.Fatalf("用户不存在路径耗时仅 %v（< 5ms），bcrypt 未被执行——时间侧信道未修复", unknownElapsed)
	}

	// 比值断言：两条路径耗时应在同一数量级。
	// 修复前：unknownElapsed ~0ms，wrongElapsed ~60ms → 比值 > 1000
	// 修复后：两条都跑 bcrypt → 比值应 < 5x
	// 取 10x 留足安全余量（跨机器 bcrypt 耗时可能有波动）。
	ratio := float64(unknownElapsed) / float64(wrongElapsed)
	if ratio < 0.1 || ratio > 10 {
		t.Fatalf("用户不存在 vs 密码错误耗时比值 %.1f（应接近 1.0），"+
			"unknown=%v wrong=%v——两条路径耗时不在同一数量级",
			ratio, unknownElapsed, wrongElapsed)
	}

	t.Logf("时间侧信道修复有效：unknown=%v wrong=%v ratio=%.2f", unknownElapsed, wrongElapsed, ratio)
}

func TestParseRejectsExpiredToken(t *testing.T) {
	f := newFakeStore()
	// 负的过期时间 → Issue 直接签出一个已过期的 token
	svc := NewService(f, testSecret, -time.Hour)

	token, err := svc.Issue(&model.User{ID: 1, Username: "alice"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := svc.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("过期 token 应被拒绝，实际 %v", err)
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	f := newFakeStore()
	issuer := NewService(f, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Hour)
	verifier := NewService(f, testSecret, time.Hour)

	token, err := issuer.Issue(&model.User{ID: 1, Username: "alice"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := verifier.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("用别的密钥签的 token 应被拒绝，实际 %v", err)
	}
}

// 篡改 payload 必须导致签名校验失败。
// 这里改的是 payload 中间的一个字符——base64url 解码后仍是合法 JSON 结构，
// 所以只有 HMAC 校验能挡住它。
func TestParseRejectsTamperedPayload(t *testing.T) {
	svc := newTestService(newFakeStore())

	token, err := svc.Issue(&model.User{ID: 1, Username: "alice"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 应有三段，实际 %d 段", len(parts))
	}
	// 把 payload 的第一个字符换成另一个合法 base64url 字符
	payload := []byte(parts[1])
	if payload[0] == 'A' {
		payload[0] = 'B'
	} else {
		payload[0] = 'A'
	}
	tampered := parts[0] + "." + string(payload) + "." + parts[2]

	if _, err := svc.Parse(tampered); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("被篡改的 token 应被拒绝，实际 %v", err)
	}
}

// 算法混淆：`alg: none` 的无签名 token 必须被拒绝。
//
// 这是 JWT 最经典的漏洞——某些库在校验时按 token 头里的 alg 选算法，
// 于是攻击者把 alg 改成 none、把签名段留空，就能伪造任意身份。
// 这里的防线是 Parse 里的 `t.Method.(*jwt.SigningMethodHMAC)` 类型断言：
// 只接受 HMAC，其余（none / RS256）一律拒绝。
func TestParseRejectsNoneAlgorithm(t *testing.T) {
	svc := newTestService(newFakeStore())

	claims := &Claims{
		UserID:   999,
		Username: "attacker",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "nebulaflow",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("构造 alg=none 的 token 失败: %v", err)
	}

	if _, err := svc.Parse(signed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("alg=none 的 token 必须被拒绝，实际 %v", err)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	svc := newTestService(newFakeStore())

	for _, raw := range []string{
		"",
		"not-a-token",
		"a.b.c",
		"eyJhbGciOiJIUzI1NiJ9.", // 只有两段
		"....",
	} {
		if _, err := svc.Parse(raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Parse(%q) 应返回 ErrInvalidToken，实际 %v", raw, err)
		}
	}
}

// Issue 必须把 uid/username 与 exp 都写进 claims——缺任何一项，
// 中间件就拿不到用户身份（userKey 里会是零值），或者令牌永不过期。
func TestIssueSetsClaims(t *testing.T) {
	svc := newTestService(newFakeStore())

	before := time.Now()
	token, err := svc.Issue(&model.User{ID: 42, Username: "alice"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims, err := svc.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if claims.UserID != 42 {
		t.Errorf("claims.UserID 应为 42，实际 %d", claims.UserID)
	}
	if claims.Username != "alice" {
		t.Errorf("claims.Username 应为 alice，实际 %q", claims.Username)
	}
	if claims.Subject != "42" {
		t.Errorf("Subject 应为 \"42\"，实际 %q", claims.Subject)
	}
	if claims.Issuer != "nebulaflow" {
		t.Errorf("Issuer 应为 nebulaflow，实际 %q", claims.Issuer)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("ExpiresAt 不能为空（否则令牌永不过期）")
	}
	want := before.Add(time.Hour)
	if d := claims.ExpiresAt.Time.Sub(want); d > time.Minute || d < -time.Minute {
		t.Errorf("ExpiresAt 应约等于 %v，实际 %v", want, claims.ExpiresAt.Time)
	}
}

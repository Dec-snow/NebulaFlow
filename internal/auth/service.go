// Package auth 实现基于 JWT 的用户认证：注册、登录、令牌签发与校验。
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials   = errors.New("invalid username or password")
	ErrInvalidToken         = errors.New("invalid or expired token")
	ErrInsufficientPermission = errors.New("insufficient permission")

	// 下面三个是**可展示**的密码策略错误（API 层映射为 400 并把文本回给客户端）。
	//
	// 为什么必须自成一类：
	//   - 掉进 API 层的 default 分支会变成 500 —— 把"用户密码太长"报成"服务端故障"，
	//     用户以为服务挂了，运维也收不到有用的信息；
	//   - 被当成凭据错误会变成 401 —— 用户以为密码错了，实际是长度超限，
	//     怎么改都"错"，永远绕不出去。
	//
	// 文本里**绝不能**出现底层库的名字。原实现直接透传 bcrypt 的错误，
	// 客户端拿到的是 "bcrypt: password length exceeds 72 bytes"：
	// 既泄露了实现细节（攻击者据此知道我们用什么哈希库、参数上限在哪），
	// 也对用户毫无指导意义。
	ErrWeakPassword = errors.New("invalid password")
	// ErrPasswordTooShort 是"不足 6 个字符"。注意单位是**字符**（rune），
	// 见 Register 里的说明。
	ErrPasswordTooShort = errors.New("password must be at least 6 characters")
	// ErrPasswordTooLong 是"超过 bcrypt 的 72 **字节**上限"。
	// 单位是字节而不是字符：这个上限来自哈希算法本身，与显示宽度无关。
	ErrPasswordTooLong = errors.New("password must be at most 72 bytes")
)

// MaxPasswordBytes 是密码的字节上限，等于 bcrypt 能处理的最大长度。
//
// 导出是为了让 API 层的请求校验能与服务层用**同一个**常量：
// 两处各写一个字面量 72，改一处漏一处时就会出现"校验放行、哈希 500"的缝隙。
const MaxPasswordBytes = 72

// dummyHash 是一个**固定**的 bcrypt 哈希，用于消除 Login 的时间侧信道。
//
// 原实现：用户不存在时直接返回 ErrInvalidCredentials（不跑 bcrypt），
// 密码错误时跑一次 bcrypt.CompareHashAndPassword（约 60ms）。
// 攻击者可以通过响应耗时差异判断用户名是否存在——先确认有效用户名，
// 再集中爆破密码（用户名枚举）。
//
// 修复：在"用户不存在"分支也跑一次 bcrypt 比对（对这个 dummy hash），
// 使两条路径的耗时一致。dummyHash 对应的明文是一个永不会成为真实密码的
// 固定随机串，所以比对结果永远是"不匹配"——但它消耗的 CPU 时间与
// 比对真实用户哈希时相同。
//
// 这是 OWASP 推荐的标准缓解措施（"equal response times for existing and
// non-existing users"），不是我们发明的。
//
// 用 GenerateFromPassword(const, DefaultCost) 生成，cost=10 与 Register 一致。
var dummyHash = []byte("$2a$10$gCTjba3MPTGDwGHixT8Fq.z/RFWtU9jZNdzJZ4dGpueJWS3uSMnSe")

// 注意这里**没有** ErrUserExists。
//
// store/errors.go 声明了「领域错误统一在此定义，API 层据此映射 HTTP 状态码」，
// 而 auth 曾经另立了一个 ErrUserExists（消息相同、值不同）。
// 后果是 api 层用 errors.Is(err, auth.ErrUserExists) 去匹配
// store.UserRepo 返回的 store.ErrUserExists——**永远不匹配**，
// 于是重复注册返回 500 而不是 409。两个同名的哨兵错误是编译器抓不到的缺陷。
//
// 现在 Register 原样透出 store 的错误，映射由 API 层统一负责。

type Store interface {
	CreateUser(ctx context.Context, u *model.User) error
	GetUserByUsername(ctx context.Context, username string) (*model.User, error)
	GetUserByID(ctx context.Context, id int64) (*model.User, error)
	GetUserRole(ctx context.Context, userID int64) (string, error)
	SetUserRole(ctx context.Context, userID int64, role string) error
	CountUsers(ctx context.Context) (int64, error)
}

type Service struct {
	store  Store
	secret []byte
	expire time.Duration
}

func NewService(store Store, secret string, expire time.Duration) *Service {
	return &Service{store: store, secret: []byte(secret), expire: expire}
}

type Claims struct {
	UserID   int64  `json:"uid"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func (s *Service) Register(ctx context.Context, username, email, password string) (*model.User, error) {
	// 用**字符数**而不是字节数。
	//
	// 原实现是 `len(password) < 6`，而 Go 的 len 对 string 返回的是**字节数**。
	// 对 ASCII 两者相同，所以这个缺陷对英文用户完全不可见；
	// 但中文密码 `"密码密码"` 只有 4 个字符却有 12 字节，会被判成"足够长"而放行，
	// 而错误消息写的是 "at least 6 characters" —— 说的是字符，量的是字节。
	// 同理 `"密码密"`（3 字符 / 9 字节）也会被放行。
	//
	// 这类缺陷单看代码很难发现（`len(x) < 6` 看上去完全正常），
	// 只有拿一个"字符数不足但字节数达标"的输入才能把它逼出来。
	if utf8.RuneCountInString(password) < 6 {
		return nil, ErrPasswordTooShort
	}
	// 上限则必须按**字节数**：bcrypt 只处理前 72 字节，超了直接报错。
	//
	// 这两个单位不同不是笔误：下限是产品策略（"6 个字符"对用户友好），
	// 上限是算法约束（72 字节，与字符宽度无关）。用错单位就会漏判：
	// 30 个中文字符只有 30 个字符却有 90 字节，按字符数校验会放行，
	// 然后挂在 bcrypt 上——一个"看起来完全合法"的密码换来一个 500。
	if len(password) > MaxPasswordBytes {
		return nil, fmt.Errorf("%w (got %d bytes)", ErrPasswordTooLong, len(password))
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		// 走到这里说明密码本身不合格（长度超限是唯一现实原因）。
		// 它是**校验错误**而不是服务端故障：绝不能升级成 500，
		// 否则任何用户输入一个超长密码就能让服务端报"内部错误"。
		// 同时不透传库内文案——只给出我们自己的说明。
		return nil, fmt.Errorf("%w: at most %d bytes allowed", ErrWeakPassword, MaxPasswordBytes)
	}

	// 第一个注册的用户自动获得 admin 角色（引导期便利，后续可由 admin 手动降级）。
	// 用 COUNT 而不是"ID == 1"判断：ID 可能因迁移/重置而不连续。
	role := model.RoleUser
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("count users: %w", err)
	}
	if count == 0 {
		role = model.RoleAdmin
	}

	u := &model.User{
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		Role:         role,
	}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, err // 唯一约束冲突由 store 层映射为 store.ErrUserExists（API 层 → 409）
	}
	return u, nil
}

func (s *Service) Login(ctx context.Context, username, password string) (string, *model.User, error) {
	// 超过 bcrypt 上限的密码**不可能**匹配任何合法哈希（注册侧已经拒绝了它们），
	// 直接按校验错误返回：既不用跑一次注定失败的 bcrypt，也不会把库内错误文案漏出去。
	// 这条检查只依赖输入、不依赖用户是否存在，因此不会引入用户名枚举的侧信道。
	if len(password) > MaxPasswordBytes {
		return "", nil, fmt.Errorf("%w (got %d bytes)", ErrPasswordTooLong, len(password))
	}
	u, err := s.store.GetUserByUsername(ctx, username)
	if err != nil {
		// 只有「用户不存在」是凭据错误。
		//
		// 原实现是 `if err != nil { return "", nil, ErrInvalidCredentials }`，
		// 把**所有**查询错误都当成"用户不存在"。后果是 PG 挂掉或连接池耗尽时：
		//   - 所有用户收到 401「用户名或密码错误」，而监控上不出现任何 5xx；
		//   - 客服收到一堆"我的密码怎么突然错了"，排查方向从一开始就是错的；
		//   - 真实故障被一条看起来最无害的错误路径吞掉。
		//
		// 这里把翻译放在 auth 边界而不是 API 层，是因为"不区分用户不存在与密码错误"
		// 是一条**安全属性**（防用户名枚举），它应该由服务层默认保证，
		// 而不是指望每个调用方都记得遵守。
		if errors.Is(err, store.ErrUserNotFound) {
			// 时间侧信道缓解：跑一次 dummy bcrypt 比对，使"用户不存在"
			// 与"密码错误"两条路径的耗时一致（见 dummyHash 注释）。
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, fmt.Errorf("get user %q: %w", username, err)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", nil, ErrInvalidCredentials
	}
	token, err := s.Issue(u)
	if err != nil {
		return "", nil, err
	}
	return token, u, nil
}

func (s *Service) Issue(u *model.User) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   u.ID,
		Username: u.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", u.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.expire)),
			Issuer:    "nebulaflow",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

// Parse 校验令牌并返回 Claims。
func (s *Service) Parse(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// RequireAdmin 校验指定用户是否拥有 admin 角色。
// 非 admin 用户返回 ErrInsufficientPermission。
func (s *Service) RequireAdmin(ctx context.Context, userID int64) error {
	role, err := s.store.GetUserRole(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user role: %w", err)
	}
	if role != model.RoleAdmin {
		return ErrInsufficientPermission
	}
	return nil
}

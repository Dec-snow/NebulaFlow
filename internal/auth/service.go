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
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidToken       = errors.New("invalid or expired token")
)

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
		return nil, errors.New("password must be at least 6 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	u := &model.User{
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
	}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, err // 唯一约束冲突由 store 层映射为 store.ErrUserExists（API 层 → 409）
	}
	return u, nil
}

func (s *Service) Login(ctx context.Context, username, password string) (string, *model.User, error) {
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

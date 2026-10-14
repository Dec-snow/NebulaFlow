package auth

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// 这个文件守的是密码长度的**上限**（缺陷 7 的服务层一半）。
//
// 缺陷回顾：Register 只校验了下限（`utf8.RuneCountInString(password) < 6`），
// 而 bcrypt 只处理前 72 **字节**，超了直接返回 ErrPasswordTooLong。
// 原实现是 `fmt.Errorf("hash password: %w", err)` —— 把"用户输入不合法"
// 包成了一条普通错误，到了 API 层只能掉进 default 分支变成 500，
// 并把库内文案 "bcrypt: password length exceeds 72 bytes" 原样回给客户端。
//
// 正确行为：
//   - 上限用**字节**（算法的约束）；
//   - 归类为**校验错误**（ErrPasswordTooLong / ErrWeakPassword），
//     由 API 层映射成 400；
//   - 错误文本里不出现 bcrypt（不泄露实现细节）。

func TestRegisterRejectsPasswordOverBcryptLimit(t *testing.T) {
	for _, n := range []int{73, 200, 4096} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			f := newFakeStore()
			svc := newTestService(f)

			_, err := svc.Register(context.Background(), "alice", "a@example.com", strings.Repeat("a", n))
			if err == nil {
				t.Fatalf("%d 字节的密码应被拒绝", n)
			}
			if !errors.Is(err, ErrPasswordTooLong) {
				t.Fatalf("%d 字节的密码应返回 ErrPasswordTooLong（API 层据此映射 400），实际 %v", n, err)
			}
			if errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("超长密码不能被伪装成凭据错误（401 会让用户以为密码错了）：%v", err)
			}
			if strings.Contains(err.Error(), "bcrypt") {
				t.Fatalf("错误文本泄露了哈希库实现细节：%v", err)
			}
			if len(f.users) != 0 {
				t.Fatalf("被拒绝的密码不应创建用户，实际创建了 %d 个", len(f.users))
			}
		})
	}
}

// 边界：正好 72 字节必须被接受（72 是 bcrypt 的**上限**而不是"小于"）。
// 少了这条，修复很容易写成过度拒绝，把合法密码挡在门外。
func TestRegisterAcceptsExactly72Bytes(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)

	pw := strings.Repeat("a", MaxPasswordBytes)
	if len(pw) != 72 {
		t.Fatalf("用例自身有误：应为 72 字节，实际 %d", len(pw))
	}
	u, err := svc.Register(context.Background(), "alice", "a@example.com", pw)
	if err != nil {
		t.Fatalf("正好 72 字节的密码应被接受，实际 %v", err)
	}
	// 而且必须真的能登录（说明它被正确哈希了，不是被静默截断）
	if _, _, err := svc.Login(context.Background(), "alice", pw); err != nil {
		t.Fatalf("用 72 字节密码登录应成功，实际 %v", err)
	}
	if !strings.HasPrefix(u.PasswordHash, "$2") {
		t.Fatalf("密码应以 bcrypt 哈希落库，实际 %q", u.PasswordHash)
	}
}

// 上限的单位是**字节**：25 个中文字符只有 25 个字符却有 75 字节。
// 按字符数判上限的实现会放行，然后挂在 bcrypt 上（也就是原来的 500）。
func TestRegisterLimitIsBytesNotRunes(t *testing.T) {
	over := strings.Repeat("密", 25) // 25 字符 / 75 字节
	if len([]rune(over)) != 25 || len(over) != 75 {
		t.Fatalf("用例自身有误：%d 字符 / %d 字节", len([]rune(over)), len(over))
	}
	f := newFakeStore()
	svc := newTestService(f)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", over); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("75 字节（25 字符）的密码应因**字节**超限被拒，实际 %v", err)
	}

	// 24 个中文字符 = 72 字节：正好在上限，必须接受
	ok := strings.Repeat("密", 24)
	if len(ok) != 72 {
		t.Fatalf("用例自身有误：应为 72 字节，实际 %d", len(ok))
	}
	f2 := newFakeStore()
	svc2 := newTestService(f2)
	if _, err := svc2.Register(context.Background(), "bob", "b@example.com", ok); err != nil {
		t.Fatalf("正好 72 字节的多字节密码应被接受，实际 %v", err)
	}
}

// 登录侧：超长密码同样按校验错误处理，且不去跑一次注定失败的 bcrypt。
func TestLoginRejectsPasswordOverBcryptLimit(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "password1"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	_, _, err := svc.Login(context.Background(), "alice", strings.Repeat("a", 200))
	if !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("超长密码登录应返回 ErrPasswordTooLong（API → 400），实际 %v", err)
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("不能被伪装成凭据错误：%v", err)
	}
	if strings.Contains(err.Error(), "bcrypt") {
		t.Fatalf("错误文本泄露了哈希库实现细节：%v", err)
	}
}

// 下限仍然按**字符数**计（P3-15 的老规矩不能被这次修改带崩）。
func TestRegisterShortPasswordStillUsesRunes(t *testing.T) {
	f := newFakeStore()
	svc := newTestService(f)
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "密码密"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("3 个字符（9 字节）的密码应返回 ErrPasswordTooShort，实际 %v", err)
	}
	if _, err := svc.Register(context.Background(), "alice", "a@example.com", "密码密码密码"); err != nil {
		t.Fatalf("6 个字符的密码应被接受，实际 %v", err)
	}
}

package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testKey 是一个 32 字节的合法 AES-256 密钥。
var testKey = []byte("0123456789abcdef0123456789abcdef")

// TestEncryptDecryptRoundTrip 加解密往返：加密再解密应还原原文。
func TestEncryptDecryptRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		plaintext string
	}{
		{"普通字符串", "sk-abcdefghijklmnopqrstuvwxyz1234567890"},
		{"空字符串", ""},
		{"中文密钥", "测试密钥-你好世界-1234567890"},
		{"特殊字符", "sk-!@#$%^&*()_+-=[]{}|;':\",./<>?"},
		{"长密钥", strings.Repeat("a", 1024)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, err := Encrypt([]byte(tc.plaintext), testKey)
			if err != nil {
				t.Fatalf("加密失败: %v", err)
			}

			// 密文应该是合法的 base64
			if _, err := base64.StdEncoding.DecodeString(ct); err != nil {
				t.Fatalf("密文不是合法 base64: %v", err)
			}

			// 密文不应该包含明文
			if strings.Contains(ct, tc.plaintext) && tc.plaintext != "" {
				t.Fatal("密文中包含明文，加密失效")
			}

			pt, err := Decrypt(ct, testKey)
			if err != nil {
				t.Fatalf("解密失败: %v", err)
			}
			if pt != tc.plaintext {
				t.Fatalf("解密结果不匹配：期望 %q，实际 %q", tc.plaintext, pt)
			}
		})
	}
}

// TestEncryptProducesUniqueOutput 每次加密即使明文相同也应产生不同密文（nonce 随机）。
func TestEncryptProducesUniqueOutput(t *testing.T) {
	plaintext := "same-secret-key"
	seen := make(map[string]bool)
	for i := 0; i < 10; i++ {
		ct, err := Encrypt([]byte(plaintext), testKey)
		if err != nil {
			t.Fatalf("第 %d 次加密失败: %v", i, err)
		}
		if seen[ct] {
			t.Fatalf("第 %d 次加密产生了重复密文，nonce 可能不是随机的", i)
		}
		seen[ct] = true

		// 每次都能正确解密回原文
		pt, err := Decrypt(ct, testKey)
		if err != nil {
			t.Fatalf("第 %d 次解密失败: %v", i, err)
		}
		if pt != plaintext {
			t.Fatalf("第 %d 次解密结果不匹配", i)
		}
	}
}

// TestDecryptWithWrongKey 错误密钥解密必须失败（GCM 认证不通过）。
func TestDecryptWithWrongKey(t *testing.T) {
	plaintext := "my-secret-api-key"
	ct, err := Encrypt([]byte(plaintext), testKey)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	wrongKey := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, err = Decrypt(ct, wrongKey)
	if err == nil {
		t.Fatal("用错误密钥解密居然成功了，GCM 认证未生效")
	}
}

// TestTamperedCiphertextDetected 篡改密文必须被检测到（GCM 认证标签校验）。
func TestTamperedCiphertextDetected(t *testing.T) {
	plaintext := "tamper-me-if-you-can"
	ct, err := Encrypt([]byte(plaintext), testKey)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	raw, _ := base64.StdEncoding.DecodeString(ct)

	// 篡改密文中间的一个字节（跳过 nonce，改 ciphertext 部分）
	// nonce 通常 12 字节，所以从第 15 字节改起
	tampered := make([]byte, len(raw))
	copy(tampered, raw)
	if len(tampered) > 15 {
		tampered[15] ^= 0x01 // 翻转一位
	}
	tamperedCT := base64.StdEncoding.EncodeToString(tampered)

	_, err = Decrypt(tamperedCT, testKey)
	if err == nil {
		t.Fatal("篡改后的密文居然解密成功了，GCM 完整性校验未生效")
	}

	// 同时测试：截断密文
	truncatedCT := base64.StdEncoding.EncodeToString(raw[:len(raw)/2])
	_, err = Decrypt(truncatedCT, testKey)
	if err == nil {
		t.Fatal("截断后的密文居然解密成功了")
	}
}

// TestValidateKey 校验密钥长度。
func TestValidateKey(t *testing.T) {
	cases := []struct {
		name    string
		key     []byte
		wantErr bool
	}{
		{"正好 32 字节", []byte(strings.Repeat("a", 32)), false},
		{"31 字节（短一个）", []byte(strings.Repeat("a", 31)), true},
		{"33 字节（长一个）", []byte(strings.Repeat("a", 33)), true},
		{"空密钥", []byte{}, true},
		{"16 字节（AES-128，不够）", []byte(strings.Repeat("a", 16)), true},
		{"24 字节（AES-192，不够）", []byte(strings.Repeat("a", 24)), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKey(tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateKey(%d 字节)：期望错误=%v，实际=%v",
					len(tc.key), tc.wantErr, err)
			}
		})
	}
}

// TestEncryptShortKey 短密钥加密必须立即报错。
func TestEncryptShortKey(t *testing.T) {
	shortKey := []byte("too-short") // 9 字节
	_, err := Encrypt([]byte("hello"), shortKey)
	if err == nil {
		t.Fatal("短密钥加密居然成功了，ValidateKey 未生效")
	}
}

// TestDecryptShortKey 短密钥解密必须立即报错。
func TestDecryptShortKey(t *testing.T) {
	shortKey := []byte("too-short")
	_, err := Decrypt("some-base64-string", shortKey)
	if err == nil {
		t.Fatal("短密钥解密居然成功了，ValidateKey 未生效")
	}
}

// TestDecryptInvalidBase64 非法 base64 密文必须报错。
func TestDecryptInvalidBase64(t *testing.T) {
	_, err := Decrypt("!!!not-base64!!!", testKey)
	if err == nil {
		t.Fatal("非法 base64 密文居然解密成功了")
	}
}

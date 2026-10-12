// Package crypto 提供 AES-256-GCM 对称加密工具，用于加密存储敏感字段（如 LLM Provider API Key）。
//
// 密文格式：base64(nonce + ciphertext)
//   - nonce: 12 字节，由 crypto/rand 生成，拼在密文前面
//   - ciphertext: AES-256-GCM 加密结果（含认证标签）
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// KeySize 是 AES-256 要求的密钥长度（32 字节）。
const KeySize = 32

// ErrInvalidKey 表示密钥长度不是 32 字节。
var ErrInvalidKey = errors.New("crypto: key must be exactly 32 bytes for AES-256")

// ValidateKey 校验密钥长度必须为 32 字节（AES-256）。
func ValidateKey(key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("%w: got %d bytes", ErrInvalidKey, len(key))
	}
	return nil
}

// Encrypt 使用 AES-256-GCM 加密明文，返回 base64 编码的密文字符串。
// 密文格式：base64(nonce + ciphertext)，nonce 长度为 gcm.NonceSize()（12 字节）。
func Encrypt(plaintext, key []byte) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: create aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: create gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: generate nonce: %w", err)
	}

	// Seal 把结果追加到 nonce 后面，得到 nonce + ciphertext
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt 使用 AES-256-GCM 解密密文（base64 编码字符串），返回明文字符串。
// 密文必须是 Encrypt 生成的格式：base64(nonce + ciphertext)。
// 若密文被篡改或密钥错误，会返回错误（GCM 认证失败）。
func Decrypt(ciphertext string, key []byte) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}

	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("crypto: decode base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: create aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: create gcm: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("crypto: ciphertext too short")
	}

	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: gcm decrypt: %w", err)
	}
	return string(plaintext), nil
}

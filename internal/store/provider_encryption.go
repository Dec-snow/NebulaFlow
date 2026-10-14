package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	nebulaflowcrypto "github.com/hoarfrost/nebulaflow/internal/crypto"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// encryptedKeyPrefix 是新版密文在库里的版本前缀：`v1:` + base64(nonce + ciphertext)。
//
// 为什么必须有这个前缀：旧实现靠"Decrypt 成功 = 已加密，失败 = 明文"来判定，
// 而 AES-GCM 在密钥不对时只会静默失败 —— 于是"密文"和"明文"在错误密钥下
// 完全无法区分。由此产生两个致命后果：
//
//  1. 换了 ENCRYPTION_KEY（或配错）后启动迁移，会把已有密文当成明文再加密一层。
//     旧 key 若没备份，用户存在库里的 API Key 就永久不可恢复。
//  2. 读取路径把 base64 密文当明文返回，Gateway 拼出
//     `Authorization: Bearer <base64密文>` 发给上游，表现为 401 而不是
//     "加密配置错误"，排查方向被彻底带偏。
//
// 有了前缀，密文可以自证身份：带前缀 = 一定是密文 → 解不开就是 key 不对 → fail-closed。
const encryptedKeyPrefix = "v1:"

// ErrEncryptionKeyMismatch 表示"已识别为密文，但用当前密钥解不开"。
//
// 这几乎总是 ENCRYPTION_KEY 与写入时不一致（密钥轮换 / 配错），必须 fail-closed：
// 既不能重新加密一层，也绝不能把密文当明文返回。
var ErrEncryptionKeyMismatch = errors.New(
	"provider api key decryption failed: encryption key mismatch or corrupted ciphertext")

// 历史密文（无前缀）的格式常量：base64(nonce12 + GCM 密文)，
// 其中 GCM 认证标签固定 16 字节。仅用于判断"这段无前缀的值有没有可能是历史密文"。
const (
	legacyNonceSize = 12
	legacyTagSize   = 16
)

// storedKeyKind 是底层存储里 API Key 的形态。
type storedKeyKind int

const (
	// storedKeyEmpty：空值，不需要任何处理。
	storedKeyEmpty storedKeyKind = iota
	// storedKeyPlaintext：不是密文（明文，或迁移过渡期的存量明文）。
	storedKeyPlaintext
	// storedKeyCiphertextV1：带版本前缀的密文（新格式）。
	storedKeyCiphertextV1
	// storedKeyCiphertextLegacy：历史无前缀密文（base64(nonce+ct)）。
	storedKeyCiphertextLegacy
)

// classifyStoredKey 判定底层存储里的值属于哪种形态。
//
// 判定顺序：
//  1. 空 → empty；
//  2. 带 `v1:` 前缀 → 一定是密文（新格式自证，无需猜测）；
//  3. 无前缀，但能按标准 base64 解出 >= nonce+tag 字节 → "疑似历史密文"，
//     是否真的能解开交给 Decrypt 判断；
//  4. 其余 → 明文。
//
// 第 3 条是有意的 fail-closed 启发式。真实 API Key（`sk-...`，含 '-' / '_'）
// 不在标准 base64 字母表里，会直接落到明文分支；只有"看起来就是旧密文"的值
// 才会被送进 Decrypt，解不开就明确报错，而不是当明文再加密一遍。
//
// 已知代价（残留风险，见文件末尾说明）：无前缀的**明文**如果恰好是 >=28 字节
// 的合法 base64，会与"错误密钥下的历史密文"无法区分，此时选择拒绝而不是猜测。
func classifyStoredKey(v string) storedKeyKind {
	if v == "" {
		return storedKeyEmpty
	}
	if strings.HasPrefix(v, encryptedKeyPrefix) {
		return storedKeyCiphertextV1
	}
	if raw, err := base64.StdEncoding.DecodeString(v); err == nil &&
		len(raw) >= legacyNonceSize+legacyTagSize {
		return storedKeyCiphertextLegacy
	}
	return storedKeyPlaintext
}

// encryptStored 加密并打上版本前缀，返回可直接落库的字符串。
func encryptStored(plaintext string, key []byte) (string, error) {
	ct, err := nebulaflowcrypto.Encrypt([]byte(plaintext), key)
	if err != nil {
		return "", err
	}
	return encryptedKeyPrefix + ct, nil
}

// ProviderStoreWithEncryption 是 ProviderStore 的加密装饰器。
//
// 它包装底层 ProviderStore，对 API Key 字段做 AES-256-GCM 加解密：
//   - 写入（UpsertProvider）：加密 api_key 后再落库（格式：`v1:` + base64 密文）
//   - 读取（ListProviders / GetProvider）：解密 api_key 后返回（供 Gateway 使用）
//   - 其余方法（DeleteProvider / ListModels / UpsertModel）：直接透传
//
// 读取路径是 **fail-closed** 的：识别为密文但解不开 → 返回
// ErrEncryptionKeyMismatch，绝不把 base64 密文当明文返回。
// 只有"明确不是密文"（空 / 非密文形态的明文）才原样返回，
// 这样既兼容尚未迁移的存量明文，又不会把密文误当 key 用。
type ProviderStoreWithEncryption struct {
	inner ProviderStore
	key   []byte
}

// NewProviderStoreWithEncryption 构造加密装饰器。key 必须是 32 字节（AES-256）。
func NewProviderStoreWithEncryption(inner ProviderStore, key []byte) (*ProviderStoreWithEncryption, error) {
	if err := nebulaflowcrypto.ValidateKey(key); err != nil {
		return nil, err
	}
	return &ProviderStoreWithEncryption{inner: inner, key: key}, nil
}

// UpsertProvider 加密 API Key 后写入底层存储。
//
// 注意：我们传入底层的是一个加密后的拷贝，底层返回的 ID / CreatedAt
// 需要回填到原始 p 上，否则调用方拿不到新分配的 ID。
func (s *ProviderStoreWithEncryption) UpsertProvider(ctx context.Context, p *model.LLMProvider) error {
	// 空 key 不需要加密，直接透传
	if p.APIKey == "" {
		return s.inner.UpsertProvider(ctx, p)
	}

	// 防御性幂等：调用方如果传回来的是"本格式密文"（例如把底层读出来的值
	// 原样写回），必须原样透传，绝不能二次加密 —— 二次加密会把 key 变成
	// 不可恢复的数据。解不开说明 key 不对，同样 fail-closed 拒绝写入。
	//
	// 这里**只认带前缀的密文**，不对无前缀的值做"疑似历史密文"猜测：
	// 写入路径的入参是用户在 UI 里输入的明文，其中可能碰巧是合法 base64
	// （例如纯 hex token），猜测会把正常 key 拒之门外。历史密文由读取/迁移
	// 路径负责识别并升级，不会从这里进来（读取路径返回的一定是明文）。
	if classifyStoredKey(p.APIKey) == storedKeyCiphertextV1 {
		if _, err := s.decryptStored(p.APIKey); err != nil {
			return fmt.Errorf("refusing to overwrite provider api key: %w", err)
		}
		return s.inner.UpsertProvider(ctx, p)
	}

	ct, err := encryptStored(p.APIKey, s.key)
	if err != nil {
		return err
	}
	// 做一个拷贝，把明文换成密文后写入底层
	encrypted := *p
	encrypted.APIKey = ct
	if err := s.inner.UpsertProvider(ctx, &encrypted); err != nil {
		return err
	}
	// 回填 ID 和 CreatedAt，让调用方的指针也能拿到这些值
	p.ID = encrypted.ID
	p.CreatedAt = encrypted.CreatedAt
	return nil
}

// ListProviders 从底层存储读取后解密所有 API Key。
//
// 任何一条解不开都直接返回错误，不返回"半解密"的列表：
// 调用方（API / Gateway）拿到半成品只会把密文当 key 用，错得更隐蔽。
func (s *ProviderStoreWithEncryption) ListProviders(ctx context.Context) ([]model.LLMProvider, error) {
	list, err := s.inner.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if err := s.decryptInPlace(&list[i]); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// GetProvider 从底层存储读取后解密 API Key。
// 识别为密文但解不开时返回 ErrEncryptionKeyMismatch（fail-closed）。
func (s *ProviderStoreWithEncryption) GetProvider(ctx context.Context, id int64) (*model.LLMProvider, error) {
	p, err := s.inner.GetProvider(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.decryptInPlace(p); err != nil {
		return nil, err
	}
	return p, nil
}

// DeleteProvider 直接透传。
func (s *ProviderStoreWithEncryption) DeleteProvider(ctx context.Context, id int64) error {
	return s.inner.DeleteProvider(ctx, id)
}

// ListModels 直接透传（模型数据不涉及加密）。
func (s *ProviderStoreWithEncryption) ListModels(ctx context.Context, providerID int64) ([]model.LLMModel, error) {
	return s.inner.ListModels(ctx, providerID)
}

// UpsertModel 直接透传（模型数据不涉及加密）。
func (s *ProviderStoreWithEncryption) UpsertModel(ctx context.Context, m *model.LLMModel) error {
	return s.inner.UpsertModel(ctx, m)
}

// decryptStored 还原底层存储里的值：
//   - 空 / 明文 → 原样返回（兼容尚未迁移的存量明文）；
//   - 密文（新格式或历史格式）→ 解密；解不开返回 ErrEncryptionKeyMismatch。
//
// 无论哪条路径都**不会**把密文当明文返回。
func (s *ProviderStoreWithEncryption) decryptStored(value string) (string, error) {
	switch classifyStoredKey(value) {
	case storedKeyEmpty, storedKeyPlaintext:
		return value, nil
	case storedKeyCiphertextV1:
		pt, err := nebulaflowcrypto.Decrypt(strings.TrimPrefix(value, encryptedKeyPrefix), s.key)
		if err != nil {
			return "", fmt.Errorf("%w: v1 ciphertext: %w", ErrEncryptionKeyMismatch, err)
		}
		return pt, nil
	case storedKeyCiphertextLegacy:
		pt, err := nebulaflowcrypto.Decrypt(value, s.key)
		if err != nil {
			return "", fmt.Errorf("%w: legacy (unprefixed) ciphertext: %w", ErrEncryptionKeyMismatch, err)
		}
		return pt, nil
	default:
		return value, nil
	}
}

// decryptInPlace 就地解密 p 的 APIKey 字段。
//
// 解密失败时除了返回错误，还会把 APIKey 清空（防御纵深）：
// 即使调用方忽略了 error，也拿不到 base64 密文去当 Bearer token 用。
func (s *ProviderStoreWithEncryption) decryptInPlace(p *model.LLMProvider) error {
	if p.APIKey == "" {
		return nil
	}
	pt, err := s.decryptStored(p.APIKey)
	if err != nil {
		p.APIKey = ""
		return fmt.Errorf("provider id=%d name=%q: %w", p.ID, p.Name, err)
	}
	p.APIKey = pt
	return nil
}

// MigratePlaintextKeys 扫描全部 Provider，把明文 API Key 加密写回。
//
// 检测逻辑（不再依赖"解密成功与否"）：
//   - 空 → 跳过；
//   - 带 `v1:` 前缀 / 疑似历史密文 → 尝试解密，**解不开就整体 fail-closed**；
//   - 明文 → 加密后写回（带上 `v1:` 前缀）。
//
// 关键约束：判定分两阶段，先把全部条目判定完，确认没有"解不开的密文"才开始写库。
// 一旦发现解不开的密文（典型场景：换了 ENCRYPTION_KEY 或配错），
// 立即 logger.Error 并返回错误，且**一条都不写** —— 保证"迁移前后库内密文字节
// 完全一致"，从而保证旧 key 备份还在时数据可恢复。
//
// 幂等：已经是密文（新格式或能解开的历史格式）的条目不会被重复处理。
//
// 返回成功迁移的条目数。
//
// 启动语义：调用方（cmd/server/main.go）目前只记录该错误、不阻断启动。
// 但装饰器自身是 fail-closed 的：无法解密的条目在 GetProvider/ListProviders
// 上会稳定返回 ErrEncryptionKeyMismatch，因此不会出现"把密文当 key 发给上游"
// 的静默错误路径。（要做到"进程直接退出"，需要调用方把该错误升级为致命错误。）
func (s *ProviderStoreWithEncryption) MigratePlaintextKeys(ctx context.Context, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	list, err := s.inner.ListProviders(ctx)
	if err != nil {
		return 0, err
	}

	// ---------- 第一阶段：只判定，不写库 ----------
	var pending []model.LLMProvider
	for i := range list {
		p := list[i]
		switch classifyStoredKey(p.APIKey) {
		case storedKeyEmpty:
			continue
		case storedKeyPlaintext:
			pending = append(pending, p)
		default:
			// 已（或疑似）是密文：必须能用当前密钥解开，否则整体放弃迁移
			if _, derr := s.decryptStored(p.APIKey); derr != nil {
				logger.Error("provider key migration aborted: existing ciphertext cannot be decrypted with the current ENCRYPTION_KEY; refusing to re-encrypt (would make the key unrecoverable)",
					"provider_id", p.ID, "provider_name", p.Name, "error", derr)
				return 0, fmt.Errorf(
					"provider key migration aborted, nothing was written (provider_id=%d name=%q): %w",
					p.ID, p.Name, derr)
			}
		}
	}

	// ---------- 第二阶段：加密写回 ----------
	migrated := 0
	for _, item := range pending {
		ct, err := encryptStored(item.APIKey, s.key)
		if err != nil {
			logger.Error("migrate provider key: encrypt failed",
				"provider_id", item.ID, "provider_name", item.Name, "error", err)
			continue
		}
		encrypted := item
		encrypted.APIKey = ct
		if err := s.inner.UpsertProvider(ctx, &encrypted); err != nil {
			logger.Error("migrate provider key: upsert failed",
				"provider_id", item.ID, "provider_name", item.Name, "error", err)
			continue
		}
		migrated++
		logger.Info("migrated provider api key to encrypted storage",
			"provider_id", item.ID, "provider_name", item.Name)
	}

	if migrated > 0 {
		logger.Info("provider key migration completed", "migrated_count", migrated)
	} else {
		logger.Info("provider key migration: all keys already encrypted")
	}
	return migrated, nil
}

// 编译期断言：装饰器满足 ProviderStore 接口。
var _ ProviderStore = (*ProviderStoreWithEncryption)(nil)

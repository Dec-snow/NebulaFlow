package store

import (
	"context"
	"log/slog"

	nebulaflowcrypto "github.com/hoarfrost/nebulaflow/internal/crypto"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ProviderStoreWithEncryption 是 ProviderStore 的加密装饰器。
//
// 它包装底层 ProviderStore，对 API Key 字段做 AES-256-GCM 加解密：
//   - 写入（UpsertProvider）：加密 api_key 后再落库
//   - 读取（ListProviders / GetProvider）：解密 api_key 后返回（供 Gateway 使用）
//   - 其余方法（DeleteProvider / ListModels / UpsertModel）：直接透传
//
// 读取路径是"宽容"的：如果解密失败，按明文返回。这是为了支持存量数据的
// 平滑迁移——数据库里可能还有明文的 API Key，系统不能一启动就全部读失败。
// 写入路径则始终加密，保证新数据一定是密文。配合 MigratePlaintextKeys
// 可以在启动时把存量明文全部加密写回，完成迁移。
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
	ct, err := nebulaflowcrypto.Encrypt([]byte(p.APIKey), s.key)
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
// 解密失败的条目按明文返回（兼容迁移过渡期）。
func (s *ProviderStoreWithEncryption) ListProviders(ctx context.Context) ([]model.LLMProvider, error) {
	list, err := s.inner.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		s.decryptInPlace(&list[i])
	}
	return list, nil
}

// GetProvider 从底层存储读取后解密 API Key。
// 解密失败按明文返回（兼容迁移过渡期）。
func (s *ProviderStoreWithEncryption) GetProvider(ctx context.Context, id int64) (*model.LLMProvider, error) {
	p, err := s.inner.GetProvider(ctx, id)
	if err != nil {
		return nil, err
	}
	s.decryptInPlace(p)
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

// decryptInPlace 尝试解密 p 的 APIKey 字段。
// 如果解密失败（说明是明文或数据损坏），保持原值不变。
// 空 key 直接跳过。
func (s *ProviderStoreWithEncryption) decryptInPlace(p *model.LLMProvider) {
	if p.APIKey == "" {
		return
	}
	pt, err := nebulaflowcrypto.Decrypt(p.APIKey, s.key)
	if err != nil {
		// 解密失败 → 按明文处理（可能是迁移过渡期的存量数据）
		return
	}
	p.APIKey = pt
}

// MigratePlaintextKeys 扫描全部 Provider，把明文 API Key 加密写回。
//
// 检测逻辑：尝试用当前密钥解密，失败则视为明文，重新 Upsert 触发加密。
// 返回成功迁移的条目数。
//
// 设计意图：在服务启动时调用一次，完成存量明文数据到加密存储的迁移。
// 幂等：已经加密的数据不会被重复处理。
func (s *ProviderStoreWithEncryption) MigratePlaintextKeys(ctx context.Context, logger *slog.Logger) (int, error) {
	list, err := s.inner.ListProviders(ctx)
	if err != nil {
		return 0, err
	}

	migrated := 0
	for i := range list {
		p := list[i]
		if p.APIKey == "" {
			continue
		}
		// 尝试解密：成功 → 已经是加密的，跳过；失败 → 明文，需要迁移
		if _, err := nebulaflowcrypto.Decrypt(p.APIKey, s.key); err == nil {
			continue // 已经是密文，跳过
		}
		// 明文 → 加密写回
		ct, err := nebulaflowcrypto.Encrypt([]byte(p.APIKey), s.key)
		if err != nil {
			logger.Error("migrate provider key: encrypt failed",
				"provider_id", p.ID, "provider_name", p.Name, "error", err)
			continue
		}
		encrypted := p
		encrypted.APIKey = ct
		if err := s.inner.UpsertProvider(ctx, &encrypted); err != nil {
			logger.Error("migrate provider key: upsert failed",
				"provider_id", p.ID, "provider_name", p.Name, "error", err)
			continue
		}
		migrated++
		logger.Info("migrated provider api key to encrypted storage",
			"provider_id", p.ID, "provider_name", p.Name)
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

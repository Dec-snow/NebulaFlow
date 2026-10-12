package store

import (
	"context"
	"log/slog"
	"os"
	"testing"

	nebulaflowcrypto "github.com/hoarfrost/nebulaflow/internal/crypto"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// testEncKey 是一个 32 字节的测试加密密钥。
var testEncKey = []byte("0123456789abcdef0123456789abcdef")

// newTestEncStore 构造一个用内存存储打底的加密装饰器，方便测试。
func newTestEncStore(t *testing.T) (*ProviderStoreWithEncryption, *MemoryStore) {
	t.Helper()
	mem := NewMemory()
	enc, err := NewProviderStoreWithEncryption(mem.Store.Providers, testEncKey)
	if err != nil {
		t.Fatalf("创建加密装饰器失败: %v", err)
	}
	return enc, mem
}

// TestEncryptionUpsertAndGet 写入一个 Provider，验证数据库里存的是密文，
// 而通过装饰器读出来的是明文。
func TestEncryptionUpsertAndGet(t *testing.T) {
	enc, mem := newTestEncStore(t)
	ctx := context.Background()

	plainKey := "sk-test-abcdefghijklmnopqrstuvwxyz"
	p := &model.LLMProvider{
		Name:     "openai",
		BaseURL:  "https://api.openai.com/v1",
		APIKey:   plainKey,
		Enabled:  true,
		Priority: 1,
	}

	if err := enc.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider 失败: %v", err)
	}

	// 1. 通过装饰器读取 → 应该是明文
	got, err := enc.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProvider 失败: %v", err)
	}
	if got.APIKey != plainKey {
		t.Fatalf("通过装饰器读取应为明文，期望 %q，实际 %q", plainKey, got.APIKey)
	}

	// 2. 直接读底层内存存储 → 应该是密文（不等于明文、且能解密回明文）
	raw, err := mem.Store.Providers.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("直接读底层存储失败: %v", err)
	}
	if raw.APIKey == plainKey {
		t.Fatal("底层存储里居然还是明文，加密未生效")
	}
	// 验证密文能正确解密
	decrypted, err := nebulaflowcrypto.Decrypt(raw.APIKey, testEncKey)
	if err != nil {
		t.Fatalf("底层存储的密文无法解密: %v", err)
	}
	if decrypted != plainKey {
		t.Fatalf("解密结果不匹配：期望 %q，实际 %q", plainKey, decrypted)
	}
}

// TestEncryptionListProviders ListProviders 也应返回解密后的明文。
func TestEncryptionListProviders(t *testing.T) {
	enc, _ := newTestEncStore(t)
	ctx := context.Background()

	keys := []string{"sk-key-one", "sk-key-two", "sk-key-three"}
	for i, k := range keys {
		p := &model.LLMProvider{
			Name:     "provider-" + string(rune('a'+i)),
			APIKey:   k,
			Enabled:  true,
			Priority: i + 1,
		}
		if err := enc.UpsertProvider(ctx, p); err != nil {
			t.Fatalf("UpsertProvider 失败: %v", err)
		}
	}

	list, err := enc.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders 失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("期望 3 个 provider，实际 %d", len(list))
	}

	// 每个的 APIKey 都应该是解密后的明文
	for i, p := range list {
		if p.APIKey != keys[i] {
			t.Errorf("第 %d 个 provider 的 APIKey 应为 %q，实际 %q", i, keys[i], p.APIKey)
		}
	}
}

// TestEncryptionEmptyAPIKey 空 API Key 不加密，也不报错。
func TestEncryptionEmptyAPIKey(t *testing.T) {
	enc, mem := newTestEncStore(t)
	ctx := context.Background()

	p := &model.LLMProvider{
		Name:    "ollama",
		BaseURL: "http://localhost:11434",
		// 没有 APIKey（ollama 不需要）
		Enabled:  true,
		Priority: 1,
	}

	if err := enc.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider 失败: %v", err)
	}

	got, err := enc.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProvider 失败: %v", err)
	}
	if got.APIKey != "" {
		t.Fatalf("空 API Key 应保持为空，实际 %q", got.APIKey)
	}

	// 底层存储也应该是空的
	raw, _ := mem.Store.Providers.GetProvider(ctx, p.ID)
	if raw.APIKey != "" {
		t.Fatalf("底层存储的空 API Key 不应被改动，实际 %q", raw.APIKey)
	}
}

// TestEncryptionPlaintextMigration 测试明文数据的自动迁移：
// 底层存储里是明文，通过装饰器读取能正常工作（宽容读取），
// 调用 MigratePlaintextKeys 后全部变成密文。
func TestEncryptionPlaintextMigration(t *testing.T) {
	enc, mem := newTestEncStore(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// 直接往底层写明文数据（模拟迁移前的状态）
	plainKeys := []string{"sk-plain-111", "sk-plain-222", ""}
	for i, k := range plainKeys {
		p := &model.LLMProvider{
			Name:     "mig-provider-" + string(rune('a'+i)),
			APIKey:   k,
			Enabled:  true,
			Priority: i + 1,
		}
		if err := mem.Store.Providers.UpsertProvider(ctx, p); err != nil {
			t.Fatalf("直接写入明文失败: %v", err)
		}
	}

	// 1. 迁移前通过装饰器读取 → 宽容读取，明文原样返回（因为解密失败）
	list, err := enc.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders 失败: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("期望 3 个 provider，实际 %d", len(list))
	}
	// 有明文 key 的应该原样返回（解密失败 → 按明文处理）
	if list[0].APIKey != "sk-plain-111" {
		t.Errorf("迁移前读取明文 key 应原样返回，实际 %q", list[0].APIKey)
	}

	// 2. 执行迁移
	migrated, err := enc.MigratePlaintextKeys(ctx, logger)
	if err != nil {
		t.Fatalf("MigratePlaintextKeys 失败: %v", err)
	}
	// 2 个非空 key 需要迁移，1 个空的跳过
	if migrated != 2 {
		t.Fatalf("期望迁移 2 条，实际 %d", migrated)
	}

	// 3. 迁移后底层存储应该是密文
	rawList, _ := mem.Store.Providers.ListProviders(ctx)
	for i, p := range rawList {
		if i == 2 {
			// 第三个是空 key，还是空的
			if p.APIKey != "" {
				t.Errorf("空 key 迁移后仍应为空，实际 %q", p.APIKey)
			}
			continue
		}
		if p.APIKey == plainKeys[i] {
			t.Errorf("第 %d 个 provider 迁移后底层存储仍是明文", i)
		}
		// 验证能解密回明文
		decrypted, err := nebulaflowcrypto.Decrypt(p.APIKey, testEncKey)
		if err != nil {
			t.Errorf("第 %d 个 provider 迁移后密文无法解密: %v", i, err)
		}
		if decrypted != plainKeys[i] {
			t.Errorf("第 %d 个 provider 解密结果不匹配：期望 %q，实际 %q",
				i, plainKeys[i], decrypted)
		}
	}

	// 4. 迁移后通过装饰器读取 → 正常解密返回明文
	listAfter, err := enc.ListProviders(ctx)
	if err != nil {
		t.Fatalf("迁移后 ListProviders 失败: %v", err)
	}
	for i, p := range listAfter {
		if p.APIKey != plainKeys[i] {
			t.Errorf("迁移后第 %d 个 provider 的 APIKey 应为 %q，实际 %q",
				i, plainKeys[i], p.APIKey)
		}
	}

	// 5. 再跑一次迁移 → 幂等，0 条迁移
	migrated2, err := enc.MigratePlaintextKeys(ctx, logger)
	if err != nil {
		t.Fatalf("第二次 MigratePlaintextKeys 失败: %v", err)
	}
	if migrated2 != 0 {
		t.Fatalf("第二次迁移应为 0 条（幂等），实际 %d", migrated2)
	}
}

// TestEncryptionInvalidKey 构造装饰器时传入非法密钥应报错。
func TestEncryptionInvalidKey(t *testing.T) {
	mem := NewMemory()
	_, err := NewProviderStoreWithEncryption(mem.Store.Providers, []byte("short"))
	if err == nil {
		t.Fatal("短密钥构造装饰器居然成功了")
	}
}

// TestEncryptionPassthroughMethods 验证 Delete / Models 相关方法透传。
func TestEncryptionPassthroughMethods(t *testing.T) {
	enc, _ := newTestEncStore(t)
	ctx := context.Background()

	// 先创建一个 provider
	p := &model.LLMProvider{
		Name:     "test-provider",
		APIKey:   "sk-test",
		Enabled:  true,
		Priority: 1,
	}
	if err := enc.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider 失败: %v", err)
	}

	// ListModels / UpsertModel 应该正常工作
	m := &model.LLMModel{
		ProviderID: p.ID,
		Name:       "gpt-4o-mini",
		MaxTokens:  128000,
	}
	if err := enc.UpsertModel(ctx, m); err != nil {
		t.Fatalf("UpsertModel 失败: %v", err)
	}
	models, err := enc.ListModels(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListModels 失败: %v", err)
	}
	if len(models) != 1 || models[0].Name != "gpt-4o-mini" {
		t.Fatalf("ListModels 结果异常: %v", models)
	}

	// DeleteProvider 应该正常工作
	if err := enc.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProvider 失败: %v", err)
	}
	_, err = enc.GetProvider(ctx, p.ID)
	if err == nil {
		t.Fatal("删除后 GetProvider 应该报错")
	}
}

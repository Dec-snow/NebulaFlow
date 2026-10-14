package store

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	nebulaflowcrypto "github.com/hoarfrost/nebulaflow/internal/crypto"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// testEncKey 是一个 32 字节的测试加密密钥。
var testEncKey = []byte("0123456789abcdef0123456789abcdef")

// testWrongKey 是另一个合法的 32 字节密钥，用来模拟"换了 ENCRYPTION_KEY"。
var testWrongKey = []byte("ffffffffffffffffffffffffffffffff")

// encryptedKeyPrefixForTest 是密文格式的版本前缀。
// 刻意写成字面量而不是引用生产常量：测试要独立守住"磁盘上的格式"这件事。
const encryptedKeyPrefixForTest = "v1:"

// decryptStoredValue 把底层存储里的原始值还原成明文。
// 同时兼容新格式（"v1:" + base64）与历史格式（无前缀 base64）。
func decryptStoredValue(t *testing.T, stored string, key []byte) string {
	t.Helper()
	raw := strings.TrimPrefix(stored, encryptedKeyPrefixForTest)
	pt, err := nebulaflowcrypto.Decrypt(raw, key)
	if err != nil {
		t.Fatalf("底层存储的密文无法解密（stored=%q）: %v", stored, err)
	}
	return pt
}

// assertKeyMismatch 断言"无法解密的密文"被明确报错，
// 而不是被静默当成明文回吐（那会让 Gateway 把 base64 密文当成 key 发给上游，
// 表现为 401 而不是"配置错误"）。
func assertKeyMismatch(t *testing.T, err error, ciphertext string) {
	t.Helper()
	if err == nil {
		t.Fatal("解密失败时必须返回错误，而不是静默按明文处理")
	}
	if !errors.Is(err, ErrEncryptionKeyMismatch) {
		t.Fatalf("错误应可用 errors.Is 判定为 ErrEncryptionKeyMismatch，实际: %v", err)
	}
	if ciphertext != "" && strings.Contains(err.Error(), ciphertext) {
		t.Fatalf("错误信息里不应回吐密文原文: %v", err)
	}
}

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
	// 验证密文能正确解密（新格式带 "v1:" 版本前缀）
	if decrypted := decryptStoredValue(t, raw.APIKey, testEncKey); decrypted != plainKey {
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
		// 验证能解密回明文（新格式带 "v1:" 版本前缀）
		if decrypted := decryptStoredValue(t, p.APIKey, testEncKey); decrypted != plainKeys[i] {
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

// TestEncryptionMigrationWithWrongKeyDoesNotDoubleEncrypt
// 换错 ENCRYPTION_KEY 时执行启动迁移，绝不能把已有密文再加密一层。
//
// 原实现用"Decrypt 成功即视为已加密，失败即视为明文"来判定，而
// decryptInPlace/Decrypt 在密钥不对时只会静默失败 —— 于是"密文"与"明文"
// 在错误密钥下无法区分，一次配错 key 就把密文二次加密；旧 key 若没备份，
// 用户存在库里的 API Key 就永久不可恢复了。
func TestEncryptionMigrationWithWrongKeyDoesNotDoubleEncrypt(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mem := NewMemory()
	enc, err := NewProviderStoreWithEncryption(mem.Store.Providers, testEncKey)
	if err != nil {
		t.Fatalf("创建加密装饰器失败: %v", err)
	}
	const plainKey = "sk-rotation-secret-0123456789abcdef"
	p := &model.LLMProvider{Name: "openai", APIKey: plainKey, Enabled: true, Priority: 1}
	if err := enc.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider 失败: %v", err)
	}

	before, err := mem.Store.Providers.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("读取底层存储失败: %v", err)
	}
	if before.APIKey == plainKey {
		t.Fatal("底层存储里居然还是明文，加密未生效")
	}

	// 换成错误密钥（模拟换了 ENCRYPTION_KEY / 配错了 key）再跑一次启动迁移
	encWrong, err := NewProviderStoreWithEncryption(mem.Store.Providers, testWrongKey)
	if err != nil {
		t.Fatalf("创建错误密钥装饰器失败: %v", err)
	}
	migrated, migErr := encWrong.MigratePlaintextKeys(ctx, logger)
	if migErr == nil {
		t.Fatal("密钥不匹配时迁移必须 fail-closed 返回错误，而不是静默继续")
	}
	if migrated != 0 {
		t.Fatalf("密钥不匹配时不允许迁移任何条目，实际 migrated=%d", migrated)
	}

	after, err := mem.Store.Providers.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("读取底层存储失败: %v", err)
	}
	if after.APIKey != before.APIKey {
		t.Fatalf("密文被二次加密了！\n迁移前: %q\n迁移后: %q", before.APIKey, after.APIKey)
	}
	// 数据必须仍然能用原 key 解出原文（证明没有被破坏）
	if got := decryptStoredValue(t, after.APIKey, testEncKey); got != plainKey {
		t.Fatalf("迁移后原 key 解出的明文不对：期望 %q，实际 %q", plainKey, got)
	}
}

// TestEncryptionLegacyCiphertextWithoutPrefix 验证向后兼容：
// 库里已经存在的"无前缀"历史密文必须仍能正常解密，且迁移不会动它。
func TestEncryptionLegacyCiphertextWithoutPrefix(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mem := NewMemory()
	const plainKey = "sk-legacy-ciphertext-key"
	legacyCT, err := nebulaflowcrypto.Encrypt([]byte(plainKey), testEncKey)
	if err != nil {
		t.Fatalf("构造历史密文失败: %v", err)
	}
	if strings.HasPrefix(legacyCT, encryptedKeyPrefixForTest) {
		t.Fatal("历史密文本不该带版本前缀（测试前提被破坏）")
	}
	p := &model.LLMProvider{Name: "legacy", APIKey: legacyCT, Enabled: true, Priority: 1}
	if err := mem.Store.Providers.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("写入历史密文失败: %v", err)
	}

	enc, err := NewProviderStoreWithEncryption(mem.Store.Providers, testEncKey)
	if err != nil {
		t.Fatalf("创建加密装饰器失败: %v", err)
	}

	// 1. 读取：历史密文必须被解开成明文
	got, err := enc.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("历史密文读取失败: %v", err)
	}
	if got.APIKey != plainKey {
		t.Fatalf("历史密文应解出明文 %q，实际 %q", plainKey, got.APIKey)
	}
	list, err := enc.ListProviders(ctx)
	if err != nil {
		t.Fatalf("ListProviders 失败: %v", err)
	}
	if len(list) != 1 || list[0].APIKey != plainKey {
		t.Fatalf("历史密文经 ListProviders 应解出明文，实际 %+v", list)
	}

	// 2. 迁移：已经是密文 → 0 条迁移，且底层字节不变
	migrated, err := enc.MigratePlaintextKeys(ctx, logger)
	if err != nil {
		t.Fatalf("MigratePlaintextKeys 失败: %v", err)
	}
	if migrated != 0 {
		t.Fatalf("历史密文不应被迁移，实际 migrated=%d", migrated)
	}
	raw, err := mem.Store.Providers.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("读取底层存储失败: %v", err)
	}
	if raw.APIKey != legacyCT {
		t.Fatalf("历史密文被改写了：\n原值: %q\n新值: %q", legacyCT, raw.APIKey)
	}
}

// TestEncryptionReadFailsClosedOnWrongKey 验证读取路径 fail-closed：
// 密钥不对时 GetProvider / ListProviders 必须报错，
// 绝不能把 base64 密文当明文返回（否则 Gateway 会拼出
// "Authorization: Bearer <base64密文>" 发给上游，表现为 401 而不是配置错误）。
func TestEncryptionReadFailsClosedOnWrongKey(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	enc, err := NewProviderStoreWithEncryption(mem.Store.Providers, testEncKey)
	if err != nil {
		t.Fatalf("创建加密装饰器失败: %v", err)
	}
	const plainKey = "sk-fail-closed-secret"
	p := &model.LLMProvider{Name: "openai", APIKey: plainKey, Enabled: true, Priority: 1}
	if err := enc.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider 失败: %v", err)
	}
	stored, err := mem.Store.Providers.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("读取底层存储失败: %v", err)
	}

	// 1. 新格式密文必须带可识别的版本前缀（见 TestEncryptionCiphertextHasVersionPrefix）
	encWrong, err := NewProviderStoreWithEncryption(mem.Store.Providers, testWrongKey)
	if err != nil {
		t.Fatalf("创建错误密钥装饰器失败: %v", err)
	}

	// 2. GetProvider：必须报错，不能回吐密文
	got, gerr := encWrong.GetProvider(ctx, p.ID)
	if gerr == nil {
		apiKey := ""
		if got != nil {
			apiKey = got.APIKey
		}
		if apiKey == stored.APIKey {
			t.Fatalf("GetProvider 把密文当明文返回了（Gateway 会拿它当 Bearer token）: %q", apiKey)
		}
		t.Fatalf("密钥不匹配时 GetProvider 应报错，实际返回 APIKey=%q", apiKey)
	}
	assertKeyMismatch(t, gerr, stored.APIKey)

	// 3. ListProviders：同样必须报错
	list, lerr := encWrong.ListProviders(ctx)
	if lerr == nil {
		t.Fatalf("密钥不匹配时 ListProviders 应报错，实际返回 %d 条", len(list))
	}
	assertKeyMismatch(t, lerr, "")
}

// TestEncryptionCiphertextHasVersionPrefix
// 新写入的密文必须带可识别的版本前缀：只有"能自证的密文"才能在密钥不对时
// 与明文区分开（这是修复"密文被二次加密 / 被当明文回吐"的基础）。
func TestEncryptionCiphertextHasVersionPrefix(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	enc, err := NewProviderStoreWithEncryption(mem.Store.Providers, testEncKey)
	if err != nil {
		t.Fatalf("创建加密装饰器失败: %v", err)
	}
	p := &model.LLMProvider{Name: "openai", APIKey: "sk-prefix-check", Enabled: true, Priority: 1}
	if err := enc.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider 失败: %v", err)
	}
	stored, err := mem.Store.Providers.GetProvider(ctx, p.ID)
	if err != nil {
		t.Fatalf("读取底层存储失败: %v", err)
	}
	if !strings.HasPrefix(stored.APIKey, encryptedKeyPrefixForTest) {
		t.Fatalf("密文应带版本前缀 %q，实际 %q", encryptedKeyPrefixForTest, stored.APIKey)
	}
	if got := decryptStoredValue(t, stored.APIKey, testEncKey); got != "sk-prefix-check" {
		t.Fatalf("密文解密结果不对: %q", got)
	}
}

// TestEncryptionUpsertIsIdempotentForCiphertext
// 防御性用例：如果调用方把"已经是密文"的值再写回来，不能被二次加密。
func TestEncryptionUpsertIsIdempotentForCiphertext(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	enc, err := NewProviderStoreWithEncryption(mem.Store.Providers, testEncKey)
	if err != nil {
		t.Fatalf("创建加密装饰器失败: %v", err)
	}
	const plainKey = "sk-idempotent-write"
	p := &model.LLMProvider{Name: "openai", APIKey: plainKey, Enabled: true, Priority: 1}
	if err := enc.UpsertProvider(ctx, p); err != nil {
		t.Fatalf("UpsertProvider 失败: %v", err)
	}
	stored, _ := mem.Store.Providers.GetProvider(ctx, p.ID)

	// 把底层读出来的密文原样再写回去
	again := &model.LLMProvider{Name: "openai", APIKey: stored.APIKey, Enabled: true, Priority: 1}
	if err := enc.UpsertProvider(ctx, again); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	after, _ := mem.Store.Providers.GetProvider(ctx, p.ID)
	if after.APIKey != stored.APIKey {
		t.Fatalf("密文被二次加密了：\n写入前: %q\n写入后: %q", stored.APIKey, after.APIKey)
	}
	if got := decryptStoredValue(t, after.APIKey, testEncKey); got != plainKey {
		t.Fatalf("二次写入后解出的明文不对：期望 %q，实际 %q", plainKey, got)
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

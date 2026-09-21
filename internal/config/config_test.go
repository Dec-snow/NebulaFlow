package config

import (
	"strings"
	"testing"
	"time"
)

// 这个文件补上 config 包的首批测试。
// 配置解析是「错了也不会报错、只会在运行时表现异常」的典型区域：
// 环境变量拼错一个字母，值就静默退回默认值，而默认值往往看起来也是合理的。

func TestFromEnvDefaults(t *testing.T) {
	// 清掉可能从外部环境带进来的变量，保证断言的是"纯默认值"
	for _, k := range []string{
		"APP_ENV", "PORT", "WORKER_COUNT", "QUEUE_MAXLEN", "QUEUE_ADMIT_RATIO",
		"DB_MAX_CONNS", "DB_MIN_CONNS", "DB_MAX_CONN_LIFETIME", "DB_CONNECT_TIMEOUT",
		"WORKFLOW_CACHE_TTL", "STORAGE_MODE",
	} {
		t.Setenv(k, "")
	}

	cfg := FromEnv()

	if cfg.Port != "8080" {
		t.Errorf("默认端口应为 8080，实际 %s", cfg.Port)
	}
	if cfg.WorkerCount != 20 {
		t.Errorf("默认 WorkerCount 应为 20，实际 %d", cfg.WorkerCount)
	}
	if cfg.QueueMaxLen != 10000 {
		t.Errorf("默认 QueueMaxLen 应为 10000，实际 %d", cfg.QueueMaxLen)
	}
	if cfg.QueueAdmitRatio != 0.8 {
		t.Errorf("默认 QueueAdmitRatio 应为 0.8，实际 %v", cfg.QueueAdmitRatio)
	}
	if cfg.StorageMode != "postgres" {
		t.Errorf("默认存储后端应为 postgres，实际 %s", cfg.StorageMode)
	}

	// 连接池：默认值必须是"能跑起来"的显式配置，而不是留给 pgxpool 的
	// max(4, NumCPU)。这三个断言是 P0-4 的核心——它们保证默认配置
	// 在任何核数的机器上行为一致。
	if cfg.DBMaxConns != 25 {
		t.Errorf("默认 DBMaxConns 应为 25，实际 %d", cfg.DBMaxConns)
	}
	if cfg.DBMinConns != 5 {
		t.Errorf("默认 DBMinConns 应为 5（保持热连接），实际 %d", cfg.DBMinConns)
	}
	if cfg.DBConnectTimeout != 5*time.Second {
		t.Errorf("默认 DBConnectTimeout 应为 5s（0 会退回操作系统 TCP 超时），实际 %v", cfg.DBConnectTimeout)
	}
	if cfg.DBMaxConnLifetime != time.Hour {
		t.Errorf("默认 DBMaxConnLifetime 应为 1h，实际 %v", cfg.DBMaxConnLifetime)
	}
	if cfg.WorkflowCacheTTL != 30*time.Second {
		t.Errorf("默认 WorkflowCacheTTL 应为 30s，实际 %v", cfg.WorkflowCacheTTL)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("WORKER_COUNT", "50")
	t.Setenv("QUEUE_ADMIT_RATIO", "0.5")
	t.Setenv("DB_MAX_CONNS", "40")
	t.Setenv("DB_MAX_CONN_LIFETIME", "2h")
	t.Setenv("WORKFLOW_CACHE_TTL", "0")

	cfg := FromEnv()

	if cfg.Port != "9090" {
		t.Errorf("Port 应为 9090，实际 %s", cfg.Port)
	}
	if cfg.WorkerCount != 50 {
		t.Errorf("WorkerCount 应为 50，实际 %d", cfg.WorkerCount)
	}
	if cfg.QueueAdmitRatio != 0.5 {
		t.Errorf("QueueAdmitRatio 应为 0.5，实际 %v", cfg.QueueAdmitRatio)
	}
	if cfg.DBMaxConns != 40 {
		t.Errorf("DBMaxConns 应为 40，实际 %d", cfg.DBMaxConns)
	}
	if cfg.DBMaxConnLifetime != 2*time.Hour {
		t.Errorf("DBMaxConnLifetime 应为 2h，实际 %v", cfg.DBMaxConnLifetime)
	}
	// 0 是合法值（关闭缓存），必须能被显式设置——不能因为"零值等于未设置"而被默认值覆盖
	if cfg.WorkflowCacheTTL != 0 {
		t.Errorf("WORKFLOW_CACHE_TTL=0 应生效（关闭缓存），实际 %v", cfg.WorkflowCacheTTL)
	}
}

// 负值是准入控制的"关闭"开关（仅压测对照用）。
// 如果解析器把负值当成非法而退回默认 0.8，压测对照组就静默失效了——
// 而那正是 P0-5 用来复现缺陷的那一组。
func TestQueueAdmitRatioAcceptsNegative(t *testing.T) {
	t.Setenv("QUEUE_ADMIT_RATIO", "-1")
	if got := FromEnv().QueueAdmitRatio; got != -1 {
		t.Fatalf("QUEUE_ADMIT_RATIO=-1 应原样保留，实际 %v", got)
	}
}

// getEnvDuration 只接受 Go 风格时长字符串。
//
// 刻意不接受裸数字：`DB_MAX_CONN_LIFETIME=3600` 的意图是秒还是纳秒无法判断，
// 与其猜一个（猜错就是 1 小时变成 3600 纳秒），不如退回默认值。
func TestGetEnvDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 42 * time.Second},      // 未设置 → 默认值
		{"30s", 30 * time.Second},   // 标准写法
		{"2h", 2 * time.Hour},       //
		{"1h30m", 90 * time.Minute}, // 复合时长
		{"500ms", 500 * time.Millisecond},
		{"3600", 42 * time.Second}, // 裸数字 → 拒绝，退回默认（不猜单位）
		{"abc", 42 * time.Second},  // 非法 → 退回默认
		{"-5s", -5 * time.Second},  // 负值合法（语义由调用方决定）
	}
	for _, tc := range cases {
		t.Setenv("TEST_DURATION", tc.raw)
		if got := getEnvDuration("TEST_DURATION", 42*time.Second); got != tc.want {
			t.Errorf("getEnvDuration(%q) = %v，期望 %v", tc.raw, got, tc.want)
		}
	}
}

func TestGetEnvIntAndFloatFallBackOnGarbage(t *testing.T) {
	t.Setenv("TEST_INT", "not-a-number")
	if got := getEnvInt("TEST_INT", 7); got != 7 {
		t.Errorf("非法整数应退回默认 7，实际 %d", got)
	}
	t.Setenv("TEST_FLOAT", "abc")
	if got := getEnvFloat("TEST_FLOAT", 0.8); got != 0.8 {
		t.Errorf("非法浮点应退回默认 0.8，实际 %v", got)
	}
	t.Setenv("TEST_INT", "0")
	if got := getEnvInt("TEST_INT", 7); got != 0 {
		t.Errorf("显式设置 0 应生效（不能被当成未设置），实际 %d", got)
	}
}

func TestGetEnvBool(t *testing.T) {
	for raw, want := range map[string]bool{
		"true": true, "1": true, "false": false, "0": false, "": true, "garbage": true,
	} {
		t.Setenv("TEST_BOOL", raw)
		// 默认值给 true，这样 "" 与 "garbage" 都能和 false 区分开
		if got := getEnvBool("TEST_BOOL", true); got != want {
			t.Errorf("getEnvBool(%q) = %v，期望 %v", raw, got, want)
		}
	}
}

// ---------- Validate（P0-3 启动校验） ----------

// strongSecret 是一个"看起来像真随机"的 32 字节密钥。
//
// ⚠ 测试里**不要**用 strings.Repeat("a", 32) 当合法值：它正好会命中
// 「单字符重复」这条规则，用例会以一个看起来莫名其妙的方式失败。
const strongSecret = "0123456789abcdef0123456789abcdef"

// validProdConfig 是一份**完全正确**的生产配置。
//
// 它的作用不只是给别的用例当基线，更重要的是反向断言：
// 一份没有任何问题的配置，Validate 必须返回零条问题（见下一个用例）。
// 少了这条断言，校验函数会随着时间累积误报，而误报会让运维习惯性忽略告警——
// 那时校验就只剩心理安慰作用了。
func validProdConfig() *Config {
	return &Config{
		Env:                 "prod",
		JWTSecret:           strongSecret,
		StorageMode:         "postgres",
		UseRedisQueue:       true,
		QueueAdmitRatio:     0.8,
		RateLimitPerMin:     120,
		TaskRateLimitPerMin: 60,
		CORSOrigins:         "https://nebulaflow.example.com",
	}
}

func problemFor(ps []Problem, field string) (Problem, bool) {
	for _, p := range ps {
		if p.Field == field {
			return p, true
		}
	}
	return Problem{}, false
}

func TestValidateAcceptsCleanProdConfig(t *testing.T) {
	if ps := validProdConfig().Validate(); len(ps) != 0 {
		t.Fatalf("一份正确的生产配置不应产生任何问题，实际 %d 条：%v", len(ps), ps)
	}
}

// P0-3 的核心用例。
//
// 这里的用例刻意分成两组，因为它们由**不同**的规则拦住：
//
//	长度组：""、31 字节、64 个 'a'
//	占位值组：change-me-in-production 系列
//
// 第二组必须包含**长度 ≥ 32 的占位值**（下面第三条与第四条）。这是变异测试
// 发现的问题：原先只用 `change-me-in-production`（24 字节）当用例，
// 而它恰好被长度规则兜住了——于是把整张占位名单删掉，测试依然全绿。
// 用例必须让每条规则都成为"唯一能拦住某个输入"的那一条。
func TestValidateJWTSecret(t *testing.T) {
	cases := []struct {
		name      string
		secret    string
		wantIssue bool
		wantFatal bool
	}{
		// —— 空值 ——
		{"空", "", true, true},

		// —— 长度规则 ——
		{"31 字节（差一个字节）", strings.Repeat("x", 31), true, true},
		{"64 字节但只有 1 个字节的熵", strings.Repeat("a", 64), true, true},

		// —— 占位值规则：注意这几条**长度都够**，只有占位名单能拦住 ——
		{"代码库里的默认值（19 字节，长度规则也能拦）", "dev-secret-change-me", true, true},
		{"compose 原先的默认值（24 字节，长度规则也能拦）", "change-me-in-production", true, true},
		{"占位值 + 后缀（33 字节，只有占位名单能拦）", "change-me-in-production-abcdefgh", true, true},
		{"占位值 + 大写（33 字节，验证大小写不敏感）", "CHANGE-ME-IN-PRODUCTION-ABCDEFGH", true, true},
		{"产品名拼出来的密钥（36 字节，只有占位名单能拦）", "nebulaflow-jwt-secret-key-0123456789", true, true},

		// —— 合法值 ——
		{"32 字节随机", strongSecret, false, false},
		{"64 字节随机", strings.Repeat("0123456789abcdef", 4), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validProdConfig()
			cfg.JWTSecret = tc.secret

			p, found := problemFor(cfg.Validate(), "JWT_SECRET")
			if found != tc.wantIssue {
				t.Fatalf("JWT_SECRET=%q：期望有问题=%v，实际=%v（%v）",
					tc.secret, tc.wantIssue, found, p)
			}
			if found && p.Fatal != tc.wantFatal {
				t.Errorf("JWT_SECRET=%q：期望致命=%v，实际=%v", tc.secret, tc.wantFatal, p.Fatal)
			}
		})
	}
}

// dev 下只提醒、不拦。
//
// 这是"能用"与"安全"之间的分界：本地开发与面试演示必须能零配置起得来，
// 否则开发者会去改代码（把默认值改成一个"看起来更长"的字符串）而不是去设环境变量，
// 问题只是被挪到了更难发现的地方。
func TestValidateDevOnlyWarnsOnDefaultSecret(t *testing.T) {
	cfg := validProdConfig()
	cfg.Env = "dev"
	cfg.JWTSecret = "dev-secret-change-me"

	ps := cfg.Validate()
	if len(ps) == 0 {
		t.Fatal("dev 下使用公开的默认密钥也应有提醒")
	}
	for _, p := range ps {
		if p.Fatal {
			t.Errorf("dev 下不应有致命问题，实际：%v", p)
		}
	}
}

// APP_ENV 取值无法识别时必须走**严格**分支（fail-safe）。
//
// 宽松分支判错的代价是"带着一个公开密钥上线"，严格分支判错的代价只是
// "启动被拒 + 一条说明该怎么改的日志"。所以这里刻意用 `Env != "dev"`
// 而不是 `Env == "prod"` 来判定严格模式。
//
// 注意 "prod "（尾部空格）与 "PROD"（大小写）：它们都是很容易出现的拼写，
// 而它们都会落入严格分支。
func TestValidateUnknownEnvIsStrict(t *testing.T) {
	for _, env := range []string{"Development", "PROD", "production", "prod ", "staging"} {
		cfg := validProdConfig()
		cfg.Env = env
		cfg.JWTSecret = "dev-secret-change-me"

		p, found := problemFor(cfg.Validate(), "JWT_SECRET")
		if !found || !p.Fatal {
			t.Errorf("APP_ENV=%q 应走严格分支并拒绝默认密钥，实际 found=%v fatal=%v",
				env, found, p.Fatal)
		}
	}
}

func TestValidateRejectsUnsafeProdSwitches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		field  string
		fatal  bool
	}{
		{"非 dev 环境用内存存储", func(c *Config) { c.StorageMode = "memory" }, "STORAGE_MODE", true},
		{"关闭队列准入（背压）", func(c *Config) { c.QueueAdmitRatio = -1 }, "QUEUE_ADMIT_RATIO", true},
		{"退回进程内队列", func(c *Config) { c.UseRedisQueue = false }, "USE_REDIS_QUEUE", false},
		{"关闭全局限流", func(c *Config) { c.RateLimitPerMin = 0 }, "RATE_LIMIT_PER_MIN / TASK_RATE_LIMIT_PER_MIN", false},
		{"关闭任务提交限流", func(c *Config) { c.TaskRateLimitPerMin = -1 }, "RATE_LIMIT_PER_MIN / TASK_RATE_LIMIT_PER_MIN", false},
		{"通配 CORS", func(c *Config) { c.CORSOrigins = "*" }, "CORS_ORIGINS", false},
		{"空 CORS", func(c *Config) { c.CORSOrigins = "" }, "CORS_ORIGINS", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validProdConfig()
			tc.mutate(cfg)

			p, found := problemFor(cfg.Validate(), tc.field)
			if !found {
				t.Fatalf("生产环境下应报出 %s，实际无问题", tc.field)
			}
			if p.Fatal != tc.fatal {
				t.Errorf("%s：期望致命=%v，实际=%v", tc.field, tc.fatal, p.Fatal)
			}
		})
	}
}

// 上面那些开关在 dev 下必须**全部**放行：
// 内存存储是演示模式，关背压是压测对照组的必要手段（P0-5 就是靠它复现的）。
// 如果 dev 下也拦，"零外部依赖启动"的承诺与前几轮的压测方法都会失效。
func TestValidateDevAllowsUnsafeSwitches(t *testing.T) {
	cfg := validProdConfig()
	cfg.Env = "dev"
	cfg.StorageMode = "memory"
	cfg.QueueAdmitRatio = -1
	cfg.UseRedisQueue = false
	cfg.RateLimitPerMin = 0
	cfg.TaskRateLimitPerMin = 0
	cfg.CORSOrigins = "*"

	for _, p := range cfg.Validate() {
		if p.Fatal {
			t.Errorf("dev 下不应有致命问题，实际：%v", p)
		}
	}
}

func TestValidateFlagsUnrecognizedEnvName(t *testing.T) {
	cfg := validProdConfig()
	cfg.Env = "Production" // 大小写不符：走严格分支，但要提醒环境名无法归类

	p, found := problemFor(cfg.Validate(), "APP_ENV")
	if !found {
		t.Fatal("无法识别的 APP_ENV 取值应产生一条提醒")
	}
	if p.Fatal {
		t.Error("环境名无法归类只是提醒，不应致命（严格分支已经兜住了真正的风险）")
	}
}

// FromEnv 与 Validate 的联动：默认配置必须能零配置启动。
// 这是本地开发与面试演示的底线——如果这条断了，P0-3 的改动就是在给自己挖坑。
func TestFromEnvDefaultPassesValidation(t *testing.T) {
	for _, k := range []string{
		"APP_ENV", "JWT_SECRET", "STORAGE_MODE", "QUEUE_ADMIT_RATIO",
		"USE_REDIS_QUEUE", "RATE_LIMIT_PER_MIN", "TASK_RATE_LIMIT_PER_MIN", "CORS_ORIGINS",
	} {
		t.Setenv(k, "")
	}

	for _, p := range FromEnv().Validate() {
		if p.Fatal {
			t.Fatalf("默认配置（APP_ENV 未设置 → dev）不应被拒绝启动，实际：%v", p)
		}
	}
}

// 长度检查挡不住 "aaaa…"：32 个 'a' 有 32 字节，却只有 1 个字节的熵。
func TestAllSameByte(t *testing.T) {
	for raw, want := range map[string]bool{
		"":                                 false,
		"a":                                false, // 单字符不算"重复"，交给长度规则处理
		"aa":                               true,
		strings.Repeat("a", 64):            true,
		"ab":                               false,
		"aab":                              false,
		"0123456789abcdef0123456789abcdef": false,
	} {
		if got := allSameByte(raw); got != want {
			t.Errorf("allSameByte(%q) = %v，期望 %v", raw, got, want)
		}
	}
}

// looksLikePlaceholder 单独测一遍：它在 Validate 里是"唯一能拦住某个输入"
// 的那条规则，所以值得有独立用例（否则它坏掉时只有间接证据）。
func TestLooksLikePlaceholder(t *testing.T) {
	for raw, want := range map[string]bool{
		"dev-secret-change-me":                true,
		"change-me-in-production":             true,
		"change-me-in-production-abcdefgh":    true, // 长度够了，只能靠这条规则拦
		"CHANGE-ME-IN-PRODUCTION-ABCDEFGH":    true, // 大小写不敏感
		"nebulaflow-jwt-secret-key-01234":     true,
		"MyS3cretPasswordForJwt2024":          true, // 含 "password"
		"":                                    false,
		strongSecret:                          false,
		strings.Repeat("0123456789abcdef", 4): false,
	} {
		if got := looksLikePlaceholder(raw); got != want {
			t.Errorf("looksLikePlaceholder(%q) = %v，期望 %v", raw, got, want)
		}
	}
}

func TestProblemString(t *testing.T) {
	fatal := Problem{Field: "JWT_SECRET", Msg: "太短", Fatal: true}
	warn := Problem{Field: "CORS_ORIGINS", Msg: "通配符", Fatal: false}

	// 日志里必须能一眼看出该改哪个环境变量，以及它是否致命
	for _, want := range []string{"JWT_SECRET", "太短", "致命"} {
		if !strings.Contains(fatal.String(), want) {
			t.Errorf("致命问题的文案应包含 %q，实际 %q", want, fatal.String())
		}
	}
	if !strings.Contains(warn.String(), "CORS_ORIGINS") {
		t.Errorf("提醒文案应包含变量名，实际 %q", warn.String())
	}
	if strings.Contains(warn.String(), "致命") {
		t.Errorf("提醒不应被标为致命，实际 %q", warn.String())
	}
}

// Package config 从环境变量加载服务配置。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env           string // dev / prod
	Port          string
	DatabaseURL   string
	RedisAddr     string
	RedisPassword string
	JWTSecret     string
	JWTExpire     time.Duration
	WorkerCount   int
	// ConsumerCount 是调度器"消费循环"的并发度（同时执行多少个任务）。
	// 它与 WorkerCount 是两个独立旋钮：
	//   WorkerCount   = 节点执行并发度（一个任务内部有多少个节点可以同时跑）
	//   ConsumerCount = 任务消费并发度（队列侧同时推进多少个 DAG）
	// 压测实测过：只调 WorkerCount 时任务吞吐恒定不变（因为消费是串行的）。
	// <=0 表示跟随 WorkerCount（默认，绝大多数场景够用）。
	ConsumerCount int
	QueueName     string // Redis Stream / 内存队列名
	// QueueMaxLen / QueueDLQMaxLen 是 Redis Stream 的容量水位（XADD MAXLEN ~）。
	// 必须显式给上限：XACK 只把消息移出 PEL、不删除消息本体，
	// 没有 MaxLen 的 stream 会单调增长直到把 Redis 撑爆（队列和限流共用一个 Redis）。
	//
	// ⚠ 水位裁掉的是**最老的**条目，也就是最可能还没被 ACK 的条目。
	// 它必须大于「你愿意丢掉的积压量」，否则消费不过来时未投递的消息会被直接裁掉。
	// 所以这是防止无限增长的护栏，不是把队列调小的旋钮；建议按水位的 90% 设告警。
	// 见 internal/queue/queue.go 的 xadd 说明。
	QueueMaxLen    int64
	QueueDLQMaxLen int64
	// QueueAdmitRatio 是准入阈值占 QueueMaxLen 的比例（背压），默认 0.8。
	// 达到该水位就拒绝新提交（POST /api/tasks 返回 503），而不是等 MAXLEN 把
	// 尚未投递的最老消息静默裁掉。
	//   0  → 用默认值 0.8
	//   <0 → 关闭准入控制（只用于压测对照，生产不要关）
	QueueAdmitRatio float64
	// EmbedDim 是 embedding 向量的维度，必须与实际使用的 Embedder 一致。
	// 它决定 document_chunks.embedding_v 的列类型 vector(N) 与 HNSW 索引——
	// pgvector 的索引要求列有确定维度，所以维度必须显式配置而不是从数据推断。
	//   LocalHashEmbedder（默认）      → 384
	//   OpenAI text-embedding-3-small  → 1536
	//   OpenAI text-embedding-3-large  → 3072
	// 改这个值会触发一次 ALTER COLUMN TYPE，历史数据需重新索引。
	EmbedDim            int
	UseRedisQueue       bool
	TaskTimeoutSec      int
	DefaultMaxRetry     int
	RateLimitPerMin     int // 每用户每分钟全局 API 调用上限（<=0 不限制）
	TaskRateLimitPerMin int // 每用户每分钟任务提交上限（<=0 不限制）
	CORSOrigins         string
	// StorageMode 选择存储后端：
	//   - postgres（默认）：PostgreSQL + Redis，生产形态
	//   - memory：纯内存实现，零外部依赖启动（演示 / 本地开发 / 面试现场）
	// 见 internal/store/memory*.go 与 cmd/server/main.go 的装配分支。
	StorageMode string
	// OllamaURL / OllamaEmbedModel 用于 Embedder，
	// 若数据库 llm_providers 表中存在 ollama provider 则优先使用其 BaseURL。
	OllamaURL        string
	OllamaEmbedModel string

	// MockToolCall 控制"没有任何真实模型 provider 时"的兜底 Mock 是否具备
	// 发起工具调用的能力（见 llm.MockProvider.AutoToolCall）。
	//
	// 默认 true：Mock 只在没配任何 provider 时启用，也就是演示/离线场景；
	// 在那种场景下 Agent 节点如果永远调不动工具，"LLM 自主调用工具"这个能力
	// 就只存在于单元测试里，没法在本地端到端看见。
	// 它不会影响未开启 Agent 的节点 —— 请求里没有 tools 时 Mock 行为完全不变。
	MockToolCall bool

	// ---------- PostgreSQL 连接池 ----------
	//
	// 为什么必须显式配置而不是用 pgxpool 的默认值（实测 v5.11.0 的默认值见括号）：
	//   - 默认 MaxConns = max(4, NumCPU)（本机 32 核 → 32，4 核机器 → 4）。
	//     池大小随部署机器核数变化，且与 WORKER_COUNT 毫无关系——
	//     一个 WORKER_COUNT=100 的服务跑在 4 核机器上只有 4 条连接，
	//     所有查询在池上排队，表现为"worker 都在跑但吞吐上不去"。
	//   - 默认 MinConns = 0：池会缩到 0，空闲之后的第一波请求要现付
	//     TCP + 认证握手，冷启动的 p99 会很难看。
	//   - 默认 ConnectTimeout = 0：不设 dialer 超时，PG 不可达时
	//     实际超时取决于操作系统 TCP 重传（Linux 约 130s，Windows 约 2 分钟）。
	//
	// 定值规则（见 internal/database/database.go 的 PoolOptions）：
	//   MaxConns ≥ 节点执行并发度 + HTTP 并发度，
	//   同时保证「单实例 MaxConns × 实例数 < PG 的 max_connections」。
	DBMaxConns int
	// DBMinConns > 0 让池常驻一批热连接，避免冷启动与空闲后的握手开销。
	DBMinConns int
	// DBMaxConnLifetime 是单条连接的最大存活时长。
	// PG 侧参数变更、负载均衡后连接漂移，都要靠它把老连接逐步换掉。
	DBMaxConnLifetime time.Duration
	// DBMaxConnIdleTime 是空闲连接被回收前的等待时长。
	DBMaxConnIdleTime time.Duration
	// DBConnectTimeout 是建立单条连接的超时。
	DBConnectTimeout time.Duration

	// WorkflowCacheTTL 是工作流读取缓存的存活时间，<=0 表示关闭缓存。
	//
	// 为什么必须有 TTL、而不能只靠「更新时主动失效」：缓存是**进程内**的。
	// 多实例部署时实例 B 改了工作流，实例 A 收不到通知，只能等 TTL 过期。
	// 单实例部署下主动失效是即时的，TTL 只作为兜底（挡住漏失效与坏值）。
	WorkflowCacheTTL time.Duration

	// ---------- 全链路 Trace（P2-12） ----------

	// TraceEnabled 控制是否采集跨 API → Task → Node → LLM/Tool 的调用链。
	//
	// **默认关闭**。开启后每个请求、每个节点都会多出若干 span，
	// 会改变 P0-0 / P0-4 压测基线的含义。与 extra.agentic 同理：
	// 新功能不能静默改变历史数字的含义。要复现那些数字，保持它关闭即可。
	TraceEnabled bool
	// TraceSampleRatio 是链路入口的采样比例（0,1]。<=0 视为 1。
	// 只在入口生效：后续进程靠 ParentBased 继承，避免同一条链被截成几段。
	TraceSampleRatio float64
	// TraceOTLPEndpoint 形如 "localhost:4318" 或 "http://localhost:4318"。
	// 为空则 span 只留在进程内（供 /api/traces 查询），不外发。
	TraceOTLPEndpoint string
	// TraceMaxTraces / TraceMaxSpansPerTrace 是进程内存储的容量上界。
	// 超出后按 FIFO 淘汰最老的 trace —— 这是有意的丢弃，且有指标可观测。
	TraceMaxTraces        int
	TraceMaxSpansPerTrace int
}

func FromEnv() *Config {
	return &Config{
		Env:                   getEnv("APP_ENV", "dev"),
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           getEnv("DATABASE_URL", "postgres://nebula:nebula@localhost:5432/nebulaflow?sslmode=disable"),
		RedisAddr:             getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:         getEnv("REDIS_PASSWORD", ""),
		JWTSecret:             getEnv("JWT_SECRET", "dev-secret-change-me"),
		JWTExpire:             time.Duration(getEnvInt("JWT_EXPIRE_HOURS", 72)) * time.Hour,
		WorkerCount:           getEnvInt("WORKER_COUNT", 20),
		ConsumerCount:         getEnvInt("CONSUMER_COUNT", 0),
		QueueName:             getEnv("QUEUE_NAME", "workflow_tasks"),
		QueueMaxLen:           int64(getEnvInt("QUEUE_MAXLEN", 10000)),
		QueueDLQMaxLen:        int64(getEnvInt("QUEUE_DLQ_MAXLEN", 1000)),
		QueueAdmitRatio:       getEnvFloat("QUEUE_ADMIT_RATIO", 0.8),
		EmbedDim:              getEnvInt("EMBED_DIM", 384),
		UseRedisQueue:         getEnvBool("USE_REDIS_QUEUE", true),
		TaskTimeoutSec:        getEnvInt("TASK_TIMEOUT_SEC", 300),
		DefaultMaxRetry:       getEnvInt("DEFAULT_MAX_RETRY", 2),
		RateLimitPerMin:       getEnvInt("RATE_LIMIT_PER_MIN", 120),
		TaskRateLimitPerMin:   getEnvInt("TASK_RATE_LIMIT_PER_MIN", 60),
		CORSOrigins:           getEnv("CORS_ORIGINS", "*"),
		StorageMode:           getEnv("STORAGE_MODE", "postgres"),
		OllamaURL:             getEnv("OLLAMA_URL", "http://localhost:11434"),
		OllamaEmbedModel:      getEnv("OLLAMA_EMBED_MODEL", ""),
		MockToolCall:          getEnvBool("LLM_MOCK_TOOL_CALL", true),
		DBMaxConns:            getEnvInt("DB_MAX_CONNS", 25),
		DBMinConns:            getEnvInt("DB_MIN_CONNS", 5),
		DBMaxConnLifetime:     getEnvDuration("DB_MAX_CONN_LIFETIME", time.Hour),
		DBMaxConnIdleTime:     getEnvDuration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute),
		DBConnectTimeout:      getEnvDuration("DB_CONNECT_TIMEOUT", 5*time.Second),
		WorkflowCacheTTL:      getEnvDuration("WORKFLOW_CACHE_TTL", 30*time.Second),
		TraceEnabled:          getEnvBool("OTEL_ENABLED", false),
		TraceSampleRatio:      getEnvFloat("OTEL_SAMPLE_RATIO", 1),
		TraceOTLPEndpoint:     getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		TraceMaxTraces:        getEnvInt("OTEL_MAX_TRACES", 256),
		TraceMaxSpansPerTrace: getEnvInt("OTEL_MAX_SPANS_PER_TRACE", 200),
	}
}

// ---------- 启动校验 ----------

// Problem 是一条配置问题。
//
// 做成「返回值」而不是在 FromEnv 里直接打日志，原因有三条：
//   - 能写测试。这类校验写错的后果是服务直接起不来（或该拦的没拦），
//     必须有测试兜住，而"打日志"是无法断言的；
//   - 能区分「致命」与「只是提醒」。生产配置里绝大多数问题是后者；
//     如果一律拒绝启动，运维会习惯性地绕过校验，校验就失效了；
//   - 调用方可以决定表现方式（日志 / 拒绝启动 / 只打印不启动）。
type Problem struct {
	// Field 是环境变量名，让日志能直接告诉运维该改哪个变量，
	// 而不是只丢一句"配置非法"。
	Field string
	// Msg 说明「问题是什么」以及「后果是什么」。
	Msg string
	// Fatal 为 true 表示该问题会让进程拒绝启动。
	Fatal bool
}

func (p Problem) String() string {
	if p.Fatal {
		return p.Field + "（致命）: " + p.Msg
	}
	return p.Field + ": " + p.Msg
}

// weakSecretTokens 是公开在代码库/文档里的占位密钥**片段**。
//
// 判定用「包含」而不是「相等」，因为"占位值 + 年份/环境名"是极常见的写法：
// `change-me-in-production-2024` 同样是人人都能猜到的密钥。
//
// 这条规则与长度规则是互补的，不是重复的：
// `change-me-in-production` 恰好只有 24 字节，长度规则能兜住它；
// 但 `change-me-in-production-abcdefgh` 有 33 字节，**只有这条规则能拦住**。
// 所以两者的测试用例必须分开，否则删掉这条规则测试也不会失败
// （这一点是变异测试发现的）。
//
// 误报风险：随机密钥里出现 "secret"/"example" 这类 6 字符子串的概率
// 约为 27 × 64^-6 ≈ 4e-10，可以忽略；而误报的代价（拒绝启动）
// 远高于漏报的代价（带着公开密钥上线），所以宁可偏严。
var weakSecretTokens = []string{
	"dev-secret",
	"change-me",
	"changeme",
	"changeit",
	"jwt-secret",
	"secret",
	"password",
	"placeholder",
	"example",
	"nebulaflow",
}

// minSecretLen 是 HS256 的密钥长度下限。
// RFC 7518 §3.2 要求 HMAC 密钥不短于哈希输出长度（HS256 → 256 bit = 32 字节）。
const minSecretLen = 32

// IsDev 报告当前是否处于开发环境。
//
// 只有精确等于 "dev" 才算开发。这是刻意的 fail-safe：APP_ENV 拼错
// （"Development"、"prod "、"production"）时落入**严格**分支而不是宽松分支。
// 宽松分支判错的代价是"带着一个公开密钥上线"，严格分支判错的代价是
// "启动被拒 + 一条说明该怎么改的日志"——后者便宜得多。
func (c *Config) IsDev() bool { return c.Env == "dev" }

// Validate 检查配置在**当前 Env 下**是否安全、可用。
//
// 它只做"静态可判定"的检查：不连数据库、不连 Redis、不发网络请求。
// 所以它能在 main 的最开头调用——在建立任何连接之前就把不该上线的配置拦下来。
func (c *Config) Validate() []Problem {
	var ps []Problem
	strict := !c.IsDev()

	// ---------- JWT 密钥 ----------
	// 这一条是 P0-3 的核心：原先 JWT_SECRET 的默认值是一个公开在代码库里的
	// 固定字符串，且启动时没有任何校验，生产部署只要漏配这个变量，
	// 服务照常启动，任何人都能用它签发合法 token。
	switch {
	case c.JWTSecret == "":
		ps = append(ps, Problem{"JWT_SECRET",
			"未设置：空密钥下任意 token 的签名都能通过校验", strict})
	case looksLikePlaceholder(c.JWTSecret):
		ps = append(ps, Problem{"JWT_SECRET",
			"使用了公开在代码库/文档里的占位密钥：任何人都能用它签发合法 token、伪造任意用户身份。" +
				"生成一个真正的随机密钥：openssl rand -hex 32", strict})
	case allSameByte(c.JWTSecret):
		ps = append(ps, Problem{"JWT_SECRET",
			"密钥由同一个字符重复组成，实际熵远低于其长度。" +
				"生成一个真正的随机密钥：openssl rand -hex 32", strict})
	case len(c.JWTSecret) < minSecretLen:
		ps = append(ps, Problem{"JWT_SECRET",
			fmt.Sprintf("长度 %d 字节 < %d 字节（HS256 要求密钥不短于哈希输出长度，RFC 7518 §3.2）",
				len(c.JWTSecret), minSecretLen), strict})
	}

	// ---------- 全链路 Trace ----------
	// 采样比例越界会被**静默**当成 1（全采样）。在生产里的后果是
	// 进程内环形缓冲被极快地冲掉：看起来"配置生效了"，实际上一小时前的 trace
	// 早就被挤出去了——而排查问题的时候，往往正是要看一小时前的那条。
	if c.TraceEnabled && (c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1) {
		ps = append(ps, Problem{"OTEL_SAMPLE_RATIO",
			fmt.Sprintf("取值 %v 不在 (0,1] 区间内，会被当成 1（全采样）：写错不会报错，"+
				"只会让 trace 存储比预期快得多地被冲掉", c.TraceSampleRatio), false})
	}

	// ---------- 以下只在非 dev 环境检查 ----------
	// dev 下这些都是合理选择（内存存储便于演示、关背压便于压测对照），
	// 所以不在这里报，避免把开发环境的日志变成噪声。
	if !strict {
		return ps
	}

	if c.TraceEnabled && c.TraceOTLPEndpoint == "" {
		ps = append(ps, Problem{"OTEL_EXPORTER_OTLP_ENDPOINT",
			"开启了 tracing 却没有配 OTLP endpoint：span 只留在**本进程**内存里。" +
				"多实例部署时每个实例只能查到经过自己的那一段链路，且进程重启即丢", false})
	}

	if c.StorageMode == "memory" {
		ps = append(ps, Problem{"STORAGE_MODE",
			"非 dev 环境使用内存存储：进程重启即丢失全部工作流、任务与知识库数据", true})
	}

	if c.QueueAdmitRatio < 0 {
		ps = append(ps, Problem{"QUEUE_ADMIT_RATIO",
			"负值 = 关闭队列准入（背压）。该开关只用于压测对照：关掉之后队列会一直涨到 " +
				"QUEUE_MAXLEN，而 MAXLEN 裁掉的是**尚未投递的最老条目**，会静默丢任务", true})
	}

	if !c.UseRedisQueue {
		ps = append(ps, Problem{"USE_REDIS_QUEUE",
			"关闭 Redis 队列后退回进程内队列：进程重启会丢失全部在途任务", false})
	}

	if c.RateLimitPerMin <= 0 || c.TaskRateLimitPerMin <= 0 {
		ps = append(ps, Problem{"RATE_LIMIT_PER_MIN / TASK_RATE_LIMIT_PER_MIN",
			"<=0 表示不限制。没有限流时，单个用户的突发流量会直接打到数据库与 LLM 配额上", false})
	}

	if c.CORSOrigins == "" || c.CORSOrigins == "*" {
		ps = append(ps, Problem{"CORS_ORIGINS",
			"为通配符：任何站点都能调用这个 API。这里**只是提醒而非致命**——" +
				"认证走 Authorization 头而不是 Cookie，浏览器不会自动携带凭据，" +
				"所以它不构成 CSRF 式的凭据暴露；实际代价是攻击面变大，生产仍应收紧为前端实际域名", false})
	}

	// ---------- Env 本身的取值 ----------
	if c.Env != "prod" && c.Env != "staging" {
		ps = append(ps, Problem{"APP_ENV",
			fmt.Sprintf("取值 %q 不在 {dev, prod, staging} 中：校验已按严格模式执行（fail-safe），"+
				"但日志与告警里会出现无法归类的环境名", c.Env), false})
	}

	return ps
}

// looksLikePlaceholder 报告 s 是否包含公开的占位密钥片段（大小写不敏感）。
func looksLikePlaceholder(s string) bool {
	low := strings.ToLower(s)
	for _, t := range weakSecretTokens {
		if strings.Contains(low, t) {
			return true
		}
	}
	return false
}

// allSameByte 报告 s 是否非空且所有字节相同（"aaaa…" 这类）。
// 长度检查挡不住它：32 个 'a' 有 32 字节，但只有 1 个字节的熵。
func allSameByte(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// getEnvDuration 解析 Go 风格的时长字符串（如 "30m"、"1h"、"5s"）。
// 不接受裸数字：`DB_MAX_CONN_LIFETIME=3600` 的意图是秒还是纳秒无法判断，
// 与其猜一个，不如退回默认值并在启动日志里体现出来。
func getEnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// NebulaFlow —— 高并发 AI Agent 工作流平台
//
// 入口负责装配全部模块：
//
//	PostgreSQL（pgx pool + migrations）→ Redis（队列/限流/缓存）
//	→ LLM Gateway → RAG → Worker Pool → Scheduler → API Server
//
// 启动：go run ./cmd/server
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/agent"
	"github.com/hoarfrost/nebulaflow/internal/api"
	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/cache"
	"github.com/hoarfrost/nebulaflow/internal/config"
	nebulaflowcrypto "github.com/hoarfrost/nebulaflow/internal/crypto"
	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/ratelimit"
	"github.com/hoarfrost/nebulaflow/internal/scheduler"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/tool"
	"github.com/hoarfrost/nebulaflow/internal/worker"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// -check-config 只做静态校验、不连接任何依赖，用于部署前的配置检查
	// （以及本地验证一套生产环境变量是否齐全），不会启动服务。
	checkOnly := flag.Bool("check-config", false, "只校验配置并退出（不连接数据库/Redis）")
	flag.Parse()

	cfg := config.FromEnv()

	// 配置校验必须发生在建立任何连接之前。
	// 带着一个公开的 JWT 密钥去连数据库，等于把伪造身份的窗口开在启动日志里。
	if problems := cfg.Validate(); len(problems) > 0 {
		var fatal int
		for _, p := range problems {
			if p.Fatal {
				fatal++
				logger.Error("configuration rejected", "field", p.Field, "problem", p.Msg)
				continue
			}
			logger.Warn("configuration", "field", p.Field, "problem", p.Msg)
		}
		if fatal > 0 {
			logger.Error("refusing to start", "env", cfg.Env, "fatal_problems", fatal,
				"hint", "这是为了避免在非 dev 环境带着不安全的默认值上线；本地开发可设 APP_ENV=dev")
			os.Exit(1)
		}
	}
	if *checkOnly {
		logger.Info("configuration ok", "env", cfg.Env)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	memMode := cfg.StorageMode == "memory"

	// ---------- 0. 全链路 Trace ----------
	// 必须**最先**装配：它设置的是 otel 的全局 tracer provider 与 propagator，
	// 后面所有 span（HTTP 中间件、任务、节点、LLM、工具）都从这里取。
	// 放在基础设施之后的话，第一段（连数据库/Redis）的调用就不会被覆盖到。
	tp, err := tracing.Setup(runCtx, tracing.Config{
		Enabled:          cfg.TraceEnabled,
		ServiceName:      "nebulaflow",
		SampleRatio:      cfg.TraceSampleRatio,
		OTLPEndpoint:     cfg.TraceOTLPEndpoint,
		MaxTraces:        cfg.TraceMaxTraces,
		MaxSpansPerTrace: cfg.TraceMaxSpansPerTrace,
	})
	if err != nil {
		return err
	}
	if tp.Enabled() {
		logger.Info("tracing enabled",
			"sample_ratio", cfg.TraceSampleRatio,
			"otlp_endpoint", cfg.TraceOTLPEndpoint,
			"max_traces", cfg.TraceMaxTraces)
	} else {
		// 默认关闭，且这件事必须显式说出来：否则"查不到 trace"
		// 会被误当成"tracing 坏了"，而真相只是没开。
		logger.Info("tracing disabled (OTEL_ENABLED=false)")
	}

	// ---------- 1. 基础设施 ----------
	// 存储与队列是两条独立的轴：
	//   STORAGE_MODE    选择存储后端（postgres / memory）
	//   USE_REDIS_QUEUE 选择队列后端（redis-stream / in-process）
	// postgres 模式下 Redis 是硬依赖；memory 模式下是可选增强——
	// 连不上就退回进程内实现，"零外部依赖启动"的承诺依然成立。
	// 解耦的意义：可以只用内存存储 + Redis 队列，单独压测消费组 / PEL / XAUTOCLAIM 这条链路。
	var (
		db  *database.DB
		rdb *redis.Client
	)
	if memMode {
		logger.Warn("STORAGE_MODE=memory：使用内存存储，数据不持久化，仅供演示/本地开发")
	} else {
		var err error
		poolOpt := database.PoolOptions{
			MaxConns:          int32(cfg.DBMaxConns),
			MinConns:          int32(cfg.DBMinConns),
			MaxConnLifetime:   cfg.DBMaxConnLifetime,
			MaxConnIdleTime:   cfg.DBMaxConnIdleTime,
			ConnectTimeout:    cfg.DBConnectTimeout,
			HealthCheckPeriod: time.Minute,
		}
		// 连接池配错是典型的"线上才发现"。这里把可判定的问题在启动阶段就说出来，
		// 尤其是 MaxConns < WORKER_COUNT 这条——它正是"worker 都在跑但吞吐上不去"
		// 的成因，而且现象会随部署机器核数变化，事后很难归因。
		for _, w := range poolOpt.Warnings(cfg.WorkerCount) {
			logger.Warn("database pool config", "issue", w)
		}
		db, err = database.Connect(runCtx, cfg.DatabaseURL, poolOpt)
		if err != nil {
			return err
		}
		defer db.Close()
		logger.Info("postgres pool configured",
			"max_conns", poolOpt.MaxConns, "min_conns", poolOpt.MinConns,
			"max_conn_lifetime", poolOpt.MaxConnLifetime,
			"max_conn_idle_time", poolOpt.MaxConnIdleTime,
			"connect_timeout", poolOpt.ConnectTimeout,
			"worker_count", cfg.WorkerCount)
		if cfg.MigrateOnStartup {
			if err := db.Migrate(runCtx); err != nil {
				return err
			}
			// 向量列维度与 HNSW 索引无法写在 SQL 迁移里（维度来自配置，SQL 不可参数化），
			// 所以迁移之后单独收敛一次。失败直接拒绝启动：检索是 RAG 的核心路径，
			// 带着一个没有索引（= 全表顺序扫描）的知识库上线，不如启动就报错。
			if err := db.EnsureVectorSchema(runCtx, cfg.EmbedDim); err != nil {
				return fmt.Errorf("初始化向量存储（EMBED_DIM=%d）: %w", cfg.EmbedDim, err)
			}
			logger.Info("postgres connected & migrated", "embed_dim", cfg.EmbedDim)
		} else {
			logger.Info("postgres connected (migrate on startup disabled)")
		}
	}

	if cfg.RedisAddr != "" {
		c := redis.NewClient(&redis.Options{
			Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: 0,
		})
		if err := c.Ping(runCtx).Err(); err != nil {
			if !memMode {
				return err
			}
			logger.Warn("redis unavailable, falling back to in-process implementations",
				"addr", cfg.RedisAddr, "error", err)
			_ = c.Close()
		} else {
			rdb = c
			defer c.Close()
			logger.Info("redis connected", "addr", cfg.RedisAddr)
		}
	}

	// ---------- 2. 存储与领域服务 ----------
	var st *store.Store
	if memMode {
		st = store.NewMemory().Store
	} else {
		st = store.New(db)
	}

	// ---------- Provider API Key 加密 ----------
	//
	// 配置了 ENCRYPTION_KEY 就用 AES-256-GCM 加密存储 Provider 的 API Key，
	// 否则保持明文（dev 环境零配置启动的兜底）。
	// 装饰器替换 st.Providers：上层所有调用方（API / Gateway / 迁移）
	// 都走同一个加密层，不会有旁路。
	if cfg.EncryptionKey != "" {
		encKey := []byte(cfg.EncryptionKey)
		if err := nebulaflowcrypto.ValidateKey(encKey); err != nil {
			return fmt.Errorf("ENCRYPTION_KEY 非法: %w", err)
		}
		encProvider, err := store.NewProviderStoreWithEncryption(st.Providers, encKey)
		if err != nil {
			return fmt.Errorf("初始化 provider 加密层失败: %w", err)
		}
		// 启动时执行存量明文迁移：把数据库里还没加密的 API Key 加密写回。
		// 幂等操作，已加密的数据不会被重复处理。
		// 只在 postgres 模式下跑迁移：memory 模式每次启动都是空的，没必要。
		if !memMode {
			if _, err := encProvider.MigratePlaintextKeys(runCtx, logger); err != nil {
				// 迁移失败必须阻断启动（fail-closed）。
				//
				// 原实现"只记日志、照样启动"，而当时的迁移判定是"解密成功=已加密、
				// 否则=明文"——用错 ENCRYPTION_KEY 启动一次就会把已有密文**再加密一层**，
				// 旧密钥没备份时 API Key 永久不可恢复，日志里却只有一行"迁移成功"。
				// 现在迁移本身已改成两阶段：只要有一条密文解不开就一条都不写并返回错误，
				// 这里再把错误升级为拒绝启动，避免"带着错密钥继续跑"这种最坏组合
				// （表现为把 base64 密文当 Bearer token 发给上游）。
				return fmt.Errorf("provider api key migration failed (检查 ENCRYPTION_KEY 是否与库中密文匹配): %w", err)
			}
		}
		st.Providers = encProvider
		logger.Info("provider api key encryption enabled",
			"algorithm", "AES-256-GCM")
	} else {
		logger.Warn("provider api key encryption disabled (ENCRYPTION_KEY not set): api keys stored in plaintext")
	}

	authSvc := auth.NewService(st.Users, cfg.JWTSecret, cfg.JWTExpire)

	// 任务队列：有 Redis 且开关打开就用 Redis Stream，否则退回内存队列
	var q queue.Queue
	switch {
	case cfg.UseRedisQueue && rdb != nil:
		if rq, err := queue.NewRedisStreamQueue(rdb, cfg.QueueName, queue.Options{
			MaxLen:     cfg.QueueMaxLen,
			DLQMaxLen:  cfg.QueueDLQMaxLen,
			AdmitRatio: cfg.QueueAdmitRatio,
			// 崩溃恢复的空闲阈值必须大于任务最长执行时间，否则滚动发布时
			// XAUTOCLAIM 会把别的实例正在跑的长任务当成遗留重新投递 → 同一任务
			// 被两个消费者并发执行（重复计费、重复副作用）。库默认 30s 只适合短任务。
			RecoverMinIdle: time.Duration(cfg.TaskTimeoutSec)*time.Second + time.Minute,
		}); err == nil {
			q = rq
			logger.Info("task queue: redis stream",
				"stream", cfg.QueueName, "max_len", cfg.QueueMaxLen, "dlq_max_len", cfg.QueueDLQMaxLen,
				"admit_limit", rq.AdmitLimit())
		} else {
			logger.Warn("redis stream queue unavailable, falling back to in-memory", "error", err)
			q = queue.NewInMemoryQueue(4096)
		}
	default:
		q = queue.NewInMemoryQueue(4096)
		logger.Info("task queue: in-memory")
	}

	hub := task.NewHub()
	taskSvc := task.NewService(st, q, hub)

	// 限流 / 缓存：memory 模式下用进程内实现
	var limiter ratelimit.Limiter
	if memMode {
		limiter = ratelimit.NewMemoryLimiter()
	} else {
		limiter = ratelimit.NewRedisLimiter(rdb)
	}
	taskSvc.SetLimiter(limiter)

	// 缓存后端：有 Redis 就用它（进程外，多实例共享），否则退回进程内实现。
	// 缓存是可选优化而不是依赖——memory 模式下 Redis 连不上时，
	// 「零外部依赖启动」的承诺依然成立，只是缓存变成单进程的。
	var cacheSvc cache.Cache
	cacheBackend := "in-process"
	if rdb != nil {
		cacheSvc = cache.NewRedisCache(rdb)
		cacheBackend = "redis"
	} else {
		cacheSvc = cache.NewMemoryCache()
	}

	// 工作流读取缓存。
	//
	// 为什么盯住这条路径：调度器每执行一个任务就读一次工作流
	// （scheduler.executeTask → workflows.GetWorkflow），而它是 3 次查询
	// （工作流行 + 节点 + 边）。P0-0 把任务吞吐提到 464.9 tasks/s 之后，
	// 这条路径等于每秒约 1400 次查询，读的却是同一份几乎不变的数据。
	//
	// 为什么用装饰器替换 st.Workflows 而不是只包 GetWorkflow：
	// 读写一起收口，失效逻辑就不可能被绕过——API 和调度器共用同一个 *Store，
	// 替换一次即全覆盖；将来新增调用方拿到的也是这个装饰器。
	cachedWF := store.NewCachedWorkflows(st.Workflows, cacheSvc, cfg.WorkflowCacheTTL, logger)
	st.Workflows = cachedWF
	if cfg.WorkflowCacheTTL > 0 {
		logger.Info("workflow read cache enabled",
			"backend", cacheBackend, "ttl", cfg.WorkflowCacheTTL)
	} else {
		logger.Warn("workflow read cache disabled (WORKFLOW_CACHE_TTL<=0)")
	}

	// ---------- 5. LLM Gateway ----------
	gateway, embedder := buildLLM(runCtx, st, cfg, logger)

	// ---------- 4. Worker Pool + Scheduler ----------
	metrics := observability.NewMetrics(prometheus.DefaultRegisterer)

	ragSvc := rag.NewService(embedder)

	// 异步文档索引器：后台并发执行向量化，避免大文件上传导致 HTTP 超时。
	// 并发度默认 2，队列默认 128，可通过环境变量调整（后续可接入配置）。
	docIndexer := rag.NewIndexer(ragSvc, st.Knowledge, cfg.IndexConcurrency, 0, logger)
	docIndexer.Start()

	// Embedder 的实际输出维度必须与 EMBED_DIM 一致。
	//
	// 为什么必须显式探测而不是"相信配置"：embedder 是运行时从数据库的 provider
	// 配置里选的（Ollama / OpenAI 兼容 / 本地 hash 兜底），各自维度不同，
	// 而向量列已经按 EMBED_DIM 建成了 vector(N)。两者不一致时：
	//   - 写入直接报错（维度不符），还是好事；
	//   - 若维度恰好相同但换了模型（例如两个都是 384 维的不同模型），
	//     写入不会报错，检索却会静默返回毫无意义的结果——这才是真正危险的。
	// 所以这里用一次真实 embedding 调用把配置和实现对账。
	if !memMode {
		if err := verifyEmbedDim(runCtx, embedder, cfg.EmbedDim); err != nil {
			return err
		}
	}
	toolReg := tool.NewRegistry(tool.Calculator{}, tool.NewHTTPTool(), tool.TimeTool{}, tool.CodeRunner{})

	pool := worker.NewWorkerPool(runCtx, cfg.WorkerCount, cfg.WorkerCount*8, nil, logger, metrics)
	sched := scheduler.New(st, q, pool, gateway, ragSvc, toolReg, hub, logger, metrics,
		time.Duration(cfg.TaskTimeoutSec)*time.Second, cfg.DefaultMaxRetry)
	pool.SetExecutor(sched)
	pool.Start()

	// 取消链路：API → task.Service → Scheduler（真正中断运行中的任务）
	taskSvc.SetCanceller(sched)
	// trace 的 task → trace 反查索引与归属记录（供 /api/tasks/:id/trace）
	sched.SetTracing(tp)

	// 分布式锁：有 Redis 就用 Redis 分布式锁（SET NX EX + Lua DEL），
	// 防止同一任务（默认）或同一工作流（显式配置）在多个实例上并发执行；
	// 否则退回进程内锁。内存模式下也用进程内锁——单实例足够，不需要 Redis。
	//
	// 粒度默认是 task，理由见 internal/scheduler.LockScope：
	// 按工作流加锁会把"同一任务不被两个实例重复执行"退化成"整条工作流串行"，
	// 实测（loadgen 1200 任务 / 20 worker / 单工作流）吞吐 493.5 → 8.9 tasks/s、
	// p50 端到端 1.31s → 97.9s。需要工作流级串行化的场景用
	// WORKFLOW_LOCK_SCOPE=workflow 显式打开。
	lockScope, scopeOK := scheduler.ParseLockScope(cfg.WorkflowLockScope)
	if !scopeOK {
		logger.Warn("unknown WORKFLOW_LOCK_SCOPE, falling back to task scope",
			"configured", cfg.WorkflowLockScope, "valid", "task|workflow|off")
	}

	// 锁 TTL 必须大于任务超时，否则超长任务会在执行中途丢锁，
	// 另一个实例随即拿到同一把锁 → 并发执行（正是这把锁要防的问题）。
	// 原实现把 TTL 硬编码成 10 分钟，与 TASK_TIMEOUT_SEC 无关：
	// 只要把任务超时配到 600s 以上，互斥就静默失效。
	lockTTL := time.Duration(cfg.TaskTimeoutSec)*time.Second + 5*time.Minute

	var wfLock scheduler.WorkflowLock
	if rdb != nil {
		wfLock = scheduler.NewRedisWorkflowLock(rdb, lockTTL)
		logger.Info("execution lock enabled", "backend", "redis",
			"scope", lockScope.String(), "ttl", lockTTL.String())
	} else {
		wfLock = scheduler.NewMemoryWorkflowLock()
		logger.Info("execution lock enabled", "backend", "in-process", "scope", lockScope.String())
	}
	sched.SetWorkflowLock(wfLock)
	sched.SetLockScope(lockScope)

	// ---------- 5. HTTP API ----------
	// Agent Registry：注册中心 + Runtime 工厂
	agentReg := agent.NewRegistry(st.Agents)
	// 注册 Native Runtime 工厂
	agentReg.RegisterFactory(model.RuntimeNative, func(ctx context.Context, a *model.AgentRegistry) (agent.Runtime, error) {
		return agent.NewNativeRuntime(agent.NativeConfig{
			Name:         a.Name,
			Version:      a.Version,
			Description:  a.Description,
			Provider:     gateway,
			Tools:        toolReg,
			DefaultModel: a.Model,
		}), nil
	})
	// 注册 LangChain Runtime 工厂
	agentReg.RegisterFactory(model.RuntimeLangChain, func(ctx context.Context, a *model.AgentRegistry) (agent.Runtime, error) {
		return agent.NewLangChainRuntime(agent.LangChainConfig{
			Name:         a.Name,
			Version:      a.Version,
			Description:  a.Description,
			BaseURL:      a.Endpoint,
			DefaultModel: a.Model,
			Capabilities: strToCapabilities(a.Capabilities),
		}), nil
	})

	server := api.NewServer(st, authSvc, taskSvc, ragSvc, docIndexer, gateway, embedder, q, pool,
		metrics, limiter, prometheus.DefaultGatherer, tp, &api.Config{
			CORSOrigins:         cfg.CORSOrigins,
			RateLimitPerMin:     cfg.RateLimitPerMin,
			TaskRateLimitPerMin: cfg.TaskRateLimitPerMin,
			AuthRateLimitPerMin: cfg.AuthRateLimitPerMin,
			WorkerCount:         cfg.WorkerCount,
		}).WithAgentRegistry(agentReg)
	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           server.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("nebulaflow server listening", "port", cfg.Port)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
			cancel()
		}
	}()

	// 调度器消费循环：并发度默认跟随 WORKER_COUNT，可用 CONSUMER_COUNT 单独覆盖。
	// 两个旋钮解耦的原因见 scheduler.SetConsumers 的注释。
	sched.SetConsumers(cfg.ConsumerCount)
	go sched.Run(runCtx)

	// 存储层运行状态上报：连接池水位 + 工作流缓存命中率。
	go reportRuntimeStats(runCtx, db, cachedWF, metrics, logger, 15*time.Second)

	// ---------- 6. 优雅关闭 ----------
	<-runCtx.Done()
	logger.Info("shutting down ...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 先停 HTTP：停止接收新请求，让在途请求跑完。
	// 必须先于 worker pool：否则 pool 关了但 HTTP 还在收任务，
	// 新任务入队后没人消费，用户看到的就是"提交了但一直 pending"。
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "error", err)
	}
	pool.Shutdown(10 * time.Second)
	// 等待文档索引器完成在途任务，避免关闭时丢失正在索引的文档
	if err := docIndexer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("doc indexer shutdown", "error", err)
	}
	// 刷出还在批量缓冲里的 span。不做这一步，最后几秒的链路会凭空消失——
	// 而"关服前后的那批请求"恰恰是排查重启类问题时最想看的一段。
	if err := tp.Shutdown(shutdownCtx); err != nil {
		logger.Warn("tracing shutdown", "error", err)
	}
	logger.Info("bye")
	return nil
}

// reportRuntimeStats 周期性上报存储层的运行状态：连接池水位 + 工作流缓存命中率。
//
// 为什么这两组指标放在同一个循环里：它们回答的是同一个问题——
// 「调度器每秒几百次读工作流，这些读到底打到哪去了」。
// 只看得见连接池，会把「池很空闲」误读成「数据库压力小」，
// 而真相可能是缓存挡住了绝大部分请求；只看得见缓存命中率，
// 又不知道那部分未命中的请求有没有把池压垮。两者必须一起看。
//
// 连接池部分：MaxConns 的定值规则（≥ 执行并发度）只是估算，真正的答案要看
// EmptyAcquire（池里没空闲连接、调用方被迫等待）与 CanceledAcquire（等待被取消）。
// 这两个数持续增长才说明池小了；反过来池一直满但没人等，说明瓶颈不在池上。
//
// 缓存部分：命中率长期为 0 说明缓存没真的接上（键不匹配 / TTL 太短 / 写入一直失败），
// 这三种情况都不会报错，只会让每次读多付一次 Redis 往返——比不加缓存还慢。
//
// 每 15 秒采一次：两者都是慢变量，比队列维护周期（10s）略慢即可，
// 采得太密只会让 Prometheus 多存一堆重复样本。
func reportRuntimeStats(ctx context.Context, db *database.DB, wf *store.CachedWorkflows,
	m *observability.Metrics, logger *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()

	// 只在「进入饱和」的那一刻告警一次，恢复后再重新武装。
	// 否则池持续吃紧时会每 15 秒刷一条，运维会把它静音掉——
	// 那正好把真正需要看的那条也一起静音了。
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if db != nil {
				s := db.Stats()
				m.SetDBPoolStats(s)
				saturated := s.MaxConns > 0 && s.AcquiredConns >= s.MaxConns
				switch {
				case saturated && !warned:
					logger.Warn("database pool saturated",
						"acquired", s.AcquiredConns, "max", s.MaxConns,
						"empty_acquire_total", s.EmptyAcquire,
						"canceled_acquire_total", s.CanceledAcquire,
						"hint", "调大 DB_MAX_CONNS（并确认 PG 的 max_connections 留有余量）")
					warned = true
				case !saturated && warned:
					logger.Info("database pool recovered",
						"acquired", s.AcquiredConns, "max", s.MaxConns)
					warned = false
				}
			}
			if wf != nil {
				cs := wf.CacheStats()
				m.SetWorkflowCacheStats(cs.Hits, cs.Misses, cs.Errors)
			}
		}
	}
}

// buildLLM 从数据库加载 Provider 配置，装配 Gateway 与 Embedder。
// 无配置时回退到 Mock Provider，保证离线可演示。
func buildLLM(ctx context.Context, st *store.Store, cfg *config.Config, logger *slog.Logger) (*llm.Gateway, *llm.EmbeddingGateway) {
	providers, list, mockOnly := loadProviders(ctx, st, cfg, logger)

	// 只有 Mock 兜底时用 30s：原来的行为就是这样，别顺手改掉历史基线。
	attemptTimeout := 60 * time.Second
	if mockOnly {
		attemptTimeout = 30 * time.Second
	}
	gateway := llm.NewGateway(list, logger, attemptTimeout, nil)
	// 运行期热更新：用户在 UI 里配好 provider 后应当立刻生效。
	startProviderRefresh(ctx, st, cfg, gateway, logger)

	// Embedder：优先从 DB 中的 ollama provider 取 BaseURL，否则用配置
	embedURL := cfg.OllamaURL
	for _, p := range providers {
		if p.Enabled && p.Name == "ollama" && p.BaseURL != "" {
			embedURL = p.BaseURL
			break
		}
	}
	embedder := llm.NewEmbeddingGateway(llm.NewOllamaEmbedder(embedURL, cfg.OllamaEmbedModel), llm.NewLocalHashEmbedder(0))
	return gateway, embedder
}

// loadProviders 从库里读 Provider 配置并翻译成 llm.Provider 列表。
// 抽成独立函数是为了让"启动时装配"和"运行期刷新"走同一段逻辑——
// 两份实现早晚会漂移。
func loadProviders(ctx context.Context, st *store.Store, cfg *config.Config, logger *slog.Logger) ([]model.LLMProvider, []llm.Provider, bool) {
	newMock := func() *llm.MockProvider {
		m := llm.NewMockProvider("mock")
		m.AutoToolCall = cfg.MockToolCall
		return m
	}

	providers, err := st.Providers.ListProviders(ctx)
	if err != nil || len(providers) == 0 {
		// 区分"读失败"与"确实没配"：原实现把两者并成同一句
		// "no llm provider configured"，于是数据库故障/密钥不匹配会被静默
		// 降级成 Mock 输出（用户看到的是 [mock-...] 前缀，却查不到任何错误）。
		if err != nil {
			logger.Error("list llm providers failed, falling back to mock provider",
				"error", err, "hint", "检查数据库连通性与 ENCRYPTION_KEY 是否匹配库中密文")
		} else {
			logger.Warn("no llm provider configured, using mock provider for demo",
				"mock_tool_call", cfg.MockToolCall)
		}
		return providers, []llm.Provider{newMock()}, true
	}

	var list []llm.Provider
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		switch p.Name {
		case "deepseek":
			list = append(list, llm.NewDeepSeekProvider(p.BaseURL, p.APIKey))
		case "openai":
			list = append(list, llm.NewOpenAIProvider(p.BaseURL, p.APIKey))
		case "mimo":
			list = append(list, llm.NewMiMoProvider(p.BaseURL, p.APIKey))
		case "ollama":
			list = append(list, llm.NewOllamaProvider(p.BaseURL))
		case "mock":
			list = append(list, newMock())
		default:
			list = append(list, llm.NewOpenAIProvider(p.BaseURL, p.APIKey))
		}
	}
	if len(list) == 0 {
		list = append(list, newMock())
		return providers, list, true
	}
	return providers, list, false
}

// providerRefreshInterval 是运行期刷新 Provider 配置的周期。
//
// 为什么用轮询而不是在 provider 的增删改接口里回调：
//  1. 不需要让 API 层反向依赖装配层（也不需要在 handler 里塞一个 hook）；
//  2. 多实例部署时，任何实例上的配置变更都会被所有实例看到；
//  3. 代价是一次 ListProviders 查询（15s 一次，内存模式几乎为零）。
//
// 原实现只在进程启动时读一次：用户在 UI 里配好 API Key、建好工作流、点运行，
// 拿到的却是 Mock 兜底的输出（形如 `[mock-mock:gpt-4o] echo: ...`），
// 而且没有任何报错——"改了配置没反应"是最难排查的一类问题。
// 取 5s 而不是更长：这是一次廉价的 ListProviders（内存模式几乎为零开销），
// 而用户的预期是"配好就能用"。
var providerRefreshInterval = 5 * time.Second

func startProviderRefresh(ctx context.Context, st *store.Store, cfg *config.Config,
	gateway *llm.Gateway, logger *slog.Logger) {
	go func() {
		tick := time.NewTicker(providerRefreshInterval)
		defer tick.Stop()
		sig := providerSignature(gateway.Providers())
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			_, list, _ := loadProviders(ctx, st, cfg, logger)
			newSig := providerSignature(list)
			if newSig == sig {
				continue
			}
			gateway.SetProviders(list)
			sig = newSig
			logger.Info("llm providers refreshed", "providers", len(list))
		}
	}()
}

// providerSignature 是 provider 列表的便宜指纹，用来判断"变没变"。
// 只取会影响调用的字段（名字 + base_url），不含 API Key——密钥不该进日志。
func providerSignature(list []llm.Provider) string {
	parts := make([]string, 0, len(list))
	for _, p := range list {
		parts = append(parts, p.Name())
	}
	return strings.Join(parts, ",")
}

// verifyEmbedDim 用一次真实 embedding 调用核对配置维度与 Embedder 实际输出维度。
//
// 不这样做的话，维度不一致只会在「第一次上传文档」时以一条晦涩的
// pgvector 错误暴露出来，而如果是同维度不同模型，则永远不报错。
func verifyEmbedDim(ctx context.Context, g *llm.EmbeddingGateway, want int) error {
	if g == nil {
		return errors.New("Embedder 未初始化")
	}
	vecs, err := g.Embed(ctx, []string{"dimension probe"})
	if err != nil {
		return fmt.Errorf("embedding 维度探测失败: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return errors.New("embedding 维度探测返回空向量：Embedder 未正确配置")
	}
	if got := len(vecs[0]); got != want {
		return fmt.Errorf(
			"EMBED_DIM=%d 与 Embedder 实际维度 %d 不一致：向量列已按 EMBED_DIM 建为 vector(%d)，两者必须相同",
			want, got, want)
	}
	return nil
}

// strToCapabilities 把字符串列表转为 Capability 列表。
// 用于从 DB 记录（AgentRegistry.Capabilities []string）构造 Runtime 配置。
func strToCapabilities(caps []string) []agent.Capability {
	out := make([]agent.Capability, len(caps))
	for i, c := range caps {
		out[i] = agent.Capability(c)
	}
	return out
}

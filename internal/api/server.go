package api

import (
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/auth"
	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/observability/tracing"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/ratelimit"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/worker"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server 持有 API 层依赖并装配路由。
type Server struct {
	store   *store.Store
	auth    *auth.Service
	tasks   *task.Service
	rag     *rag.Service
	llm     *llm.Gateway
	embed   *llm.EmbeddingGateway
	queue   queue.Queue
	pool    *worker.WorkerPool
	metrics *observability.Metrics
	limiter ratelimit.Limiter
	reg     prometheus.Gatherer
	// tracing 为 nil 或未启用时，trace 中间件与查询接口都退化为"不收集、返回 501"。
	tracing *tracing.Provider
	cfg     Config
}

func NewServer(
	st *store.Store, authSvc *auth.Service, taskSvc *task.Service,
	ragSvc *rag.Service, gateway *llm.Gateway, embedder *llm.EmbeddingGateway,
	q queue.Queue, pool *worker.WorkerPool, metrics *observability.Metrics,
	limiter ratelimit.Limiter, reg prometheus.Gatherer, tp *tracing.Provider, cfg *Config,
) *Server {
	resolved := cfg.withDefaults()
	return &Server{
		store: st, auth: authSvc, tasks: taskSvc, rag: ragSvc, llm: gateway,
		embed: embedder, queue: q, pool: pool, metrics: metrics,
		limiter: limiter, reg: reg, tracing: tp, cfg: resolved,
	}
}

// Config 是 API 层需要的运行参数（由 config.Config 映射而来）。
// 单独抽出来是为了让 api 包不直接依赖 config 包，方便测试时构造。
type Config struct {
	// CORSOrigins 允许的来源；空或 "*" 表示允许全部。
	CORSOrigins string
	// RateLimitPerMin 每用户每分钟的全局 API 调用上限，<=0 表示不限制。
	RateLimitPerMin int
	// TaskRateLimitPerMin 每用户每分钟的任务提交上限，<=0 表示不限制。
	TaskRateLimitPerMin int
	// WorkerCount 用于 Dashboard 展示 workers 20/20。
	WorkerCount int
}

func (c *Config) withDefaults() Config {
	out := Config{}
	if c != nil {
		out = *c
	}
	if out.CORSOrigins == "" {
		out.CORSOrigins = "*"
	}
	if out.RateLimitPerMin <= 0 {
		out.RateLimitPerMin = 120
	}
	if out.TaskRateLimitPerMin <= 0 {
		out.TaskRateLimitPerMin = 60
	}
	if out.WorkerCount <= 0 {
		out.WorkerCount = 1
	}
	return out
}

func (s *Server) Router() *gin.Engine {
	cfg := s.cfg

	r := gin.New()
	r.Use(gin.Recovery())
	// tracing 放在最前面：它要包住后续所有中间件（含认证与限流），
	// 否则"401 是哪个环节拒的"就没有任何链路信息。
	r.Use(tracingMiddleware(s.tracing))
	r.Use(corsMiddleware(cfg.CORSOrigins))
	r.Use(metricsMiddleware(s.metrics))

	// 健康检查与指标
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "nebulaflow"})
	})
	if s.reg != nil {
		h := promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{})
		r.GET("/metrics", gin.WrapH(h))
	}

	// 前端静态资源：web/dist 存在时由后端直接托管（单端口一站式访问）
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.AbortWithStatusJSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
			return
		}
		if c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		dist := "web/dist"
		file := path.Join(dist, c.Request.URL.Path)
		if !strings.HasPrefix(file, dist) {
			// 路径穿越防护
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		if st, err := os.Stat(file); err == nil && !st.IsDir() {
			c.File(file)
			return
		}
		// SPA 回退：非 API 请求统一返回 index.html
		if _, err := os.Stat(path.Join(dist, "index.html")); err == nil {
			c.File(path.Join(dist, "index.html"))
			return
		}
		c.AbortWithStatus(http.StatusNotFound)
	})

	authH := &authHandler{svc: s.auth}
	workflowH := &workflowHandler{store: s.store}
	taskH := &taskHandler{svc: s.tasks, limit: cfg.TaskRateLimitPerMin, metrics: s.metrics}
	kbH := &knowledgeHandler{store: s.store, rag: s.rag}
	providerH := &providerHandler{store: s.store}
	traceH := &traceHandler{svc: s.tasks, tracing: s.tracing}
	dashH := &dashboardHandler{metrics: s.metrics, queue: s.queue, pool: s.pool,
		hub: s.hub(), workerCount: cfg.WorkerCount, store: s.store}

	authGroup := r.Group("/api/auth")
	{
		authGroup.POST("/register", authH.register)
		authGroup.POST("/login", authH.login)
		authGroup.GET("/me", authMiddleware(s.auth), authH.me)
	}

	// 认证 + 限流保护的业务 API
	api := r.Group("/api")
	api.Use(authMiddleware(s.auth))
	api.Use(rateLimitMiddleware(s.limiter, cfg.RateLimitPerMin))
	{
		// Workflow
		api.GET("/workflows", workflowH.list)
		api.POST("/workflows", workflowH.create)
		api.GET("/workflows/:id", workflowH.get)
		api.PUT("/workflows/:id", workflowH.update)
		api.DELETE("/workflows/:id", workflowH.delete)

		// Task
		api.POST("/tasks", taskH.create)
		api.GET("/tasks", taskH.list)
		api.GET("/tasks/:id", taskH.get)
		api.POST("/tasks/:id/cancel", taskH.cancel)
		api.GET("/tasks/:id/logs", taskH.logs)
		api.GET("/tasks/:id/stream", taskH.stream) // SSE
		// 全链路 trace：按任务查最常用（前端/运维从"哪个任务慢"出发），
		// 按 trace id 查用于从日志或响应头 X-Trace-Id 反查。
		api.GET("/tasks/:id/trace", traceH.byTask)
		api.GET("/traces", traceH.list)
		api.GET("/traces/:trace_id", traceH.byID)

		// Knowledge Base / RAG
		api.GET("/knowledge-bases", kbH.listKB)
		api.POST("/knowledge-bases", kbH.createKB)
		api.DELETE("/knowledge-bases/:id", kbH.deleteKB)
		api.GET("/knowledge-bases/:id/documents", kbH.listDocs)
		api.POST("/knowledge-bases/:id/documents", kbH.uploadDoc)
		api.GET("/knowledge-bases/:id/retrieve", kbH.retrieve)

		// LLM Providers
		api.GET("/providers", providerH.list)
		api.POST("/providers", providerH.create)
		api.PUT("/providers/:id", providerH.update)
		api.DELETE("/providers/:id", providerH.delete)
		api.POST("/providers/:id/models", providerH.addModel)

		// Dashboard
		api.GET("/dashboard/stats", dashH.stats)
		api.GET("/dashboard/usage", dashH.usage)
	}

	return r
}

// hub 返回 SSE 事件中枢（Dashboard 用它暴露订阅者数与丢弃事件数）。
func (s *Server) hub() *task.Hub {
	if s.tasks == nil {
		return nil
	}
	return s.tasks.Hub()
}

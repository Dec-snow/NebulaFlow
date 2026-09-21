package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/worker"
)

// dashboardHandler 聚合系统实时指标（前端监控页直接消费）。
type dashboardHandler struct {
	metrics     *observability.Metrics
	queue       queue.Queue
	pool        *worker.WorkerPool
	hub         *task.Hub
	workerCount int
	store       *store.Store
}

// GET /api/dashboard/stats
// 覆盖文档要求的控制台指标：队列长度、活跃/总 worker、运行中任务、SSE 订阅与丢弃。
func (h *dashboardHandler) stats(c *gin.Context) {
	queueLen := int64(0)
	if h.queue != nil {
		if n, err := h.queue.Len(c.Request.Context()); err == nil {
			queueLen = n
		}
	}
	active, queued := int64(0), int64(0)
	total := h.workerCount
	if h.pool != nil {
		active, queued = h.pool.Stats()
		total = h.pool.Workers()
	}

	running := int64(0)
	if h.metrics != nil {
		running = h.metrics.RunningTasks.Load()
	}

	subscribers, dropped := int64(0), int64(0)
	if h.hub != nil {
		subscribers, dropped = h.hub.Stats()
	}

	c.JSON(http.StatusOK, gin.H{
		"queue_length":    queueLen, // 待消费任务（含延迟重投）
		"worker_queued":   queued,   // Worker Pool 内排队待执行的节点
		"active_workers":  active,   // 正在执行节点的 worker
		"total_workers":   total,    // worker 总数（活跃/总数）
		"running_tasks":   running,  // 正在执行的工作流任务
		"sse_subscribers": subscribers,
		"sse_dropped":     dropped, // 因客户端消费过慢被丢弃的事件数
		"timestamp":       time.Now().Format(time.RFC3339),
	})
}

// GET /api/dashboard/usage —— token 用量汇总（从 usage_records 表聚合）
func (h *dashboardHandler) usage(c *gin.Context) {
	if h.store == nil || h.store.Tasks == nil {
		c.JSON(http.StatusOK, gin.H{
			"total_input_tokens":  0,
			"total_output_tokens": 0,
			"total_requests":      0,
			"per_provider":        []any{},
			"message":             "store not available",
		})
		return
	}
	totalInput, totalOutput, totalReqs, perProvider, err := h.store.Tasks.UsageSummary(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"total_input_tokens":  0,
			"total_output_tokens": 0,
			"total_requests":      0,
			"per_provider":        []any{},
			"error":               err.Error(),
		})
		return
	}
	if perProvider == nil {
		perProvider = []store.ProviderUsage{}
	}
	c.JSON(http.StatusOK, gin.H{
		"total_input_tokens":  totalInput,
		"total_output_tokens": totalOutput,
		"total_requests":      totalReqs,
		"per_provider":        perProvider,
		"timestamp":           time.Now().Format(time.RFC3339),
	})
}

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/task"
)

type taskHandler struct {
	svc     *task.Service
	limit   int // 每用户每分钟任务提交上限
	metrics *observability.Metrics
}

type createTaskReq struct {
	WorkflowID int64  `json:"workflow_id" binding:"required"`
	Input      string `json:"input"`
}

// POST /api/tasks
func (h *taskHandler) create(c *gin.Context) {
	u := getUser(c)
	var req createTaskReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 任务提交限流：user:{id}:tasks 每分钟上限
	if h.limit > 0 {
		if !h.svc.RateAllowed(c.Request.Context(), u.ID, h.limit) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "task rate limit exceeded, max " + strconv.Itoa(h.limit) + " tasks/min",
			})
			return
		}
	}
	t, err := h.svc.CreateTask(c.Request.Context(), u.ID, req.WorkflowID, req.Input)
	if err != nil {
		// 背压拒绝单独计数：这个指标持续 >0 说明系统在过载，
		// 但它是"健康"的过载——请求被明确拒绝，而不是被静默丢掉。
		if errors.Is(err, queue.ErrQueueFull) {
			h.metrics.AddQueueRejected()
		}
		abortWithError(c, err)
		return
	}
	c.Header("X-Task-Id", strconv.FormatInt(t.ID, 10))
	c.JSON(http.StatusCreated, t)
}

// GET /api/tasks?limit=&offset=
func (h *taskHandler) list(c *gin.Context) {
	u := getUser(c)
	limit := parseQueryInt(c, "limit", 20)
	offset := parseQueryInt(c, "offset", 0)
	list, err := h.svc.ListTasks(c.Request.Context(), u.ID, limit, offset)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": list})
}

// GET /api/tasks/:id
func (h *taskHandler) get(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	t, err := h.svc.GetTask(c.Request.Context(), id, u.ID)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// POST /api/tasks/:id/cancel
func (h *taskHandler) cancel(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	if err := h.svc.CancelTask(c.Request.Context(), id, u.ID); err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"cancelled": id})
}

// GET /api/tasks/:id/logs?limit=
func (h *taskHandler) logs(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	limit := parseQueryInt(c, "limit", 100)
	logs, err := h.svc.GetLogs(c.Request.Context(), id, u.ID, limit)
	if err != nil {
		abortWithError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

// GET /api/tasks/:id/stream —— SSE 实时执行流
func (h *taskHandler) stream(c *gin.Context) {
	u := getUser(c)
	id := mustID(c)
	// 校验任务归属，未找到直接 404
	if _, err := h.svc.GetTask(c.Request.Context(), id, u.ID); err != nil {
		abortWithError(c, err)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Access-Control-Allow-Origin", "*")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}

	ch, unsub := h.svc.Hub().Subscribe(id)
	defer unsub()

	// 发送当前任务快照（重连恢复）
	if t, err := h.svc.GetTask(c.Request.Context(), id, u.ID); err == nil {
		writeSSE(c, flusher, "snapshot", t)
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	reqCtx := c.Request.Context()
	for {
		select {
		case ev := <-ch:
			payload, _ := json.Marshal(ev)
			writeSSERaw(c, flusher, string(ev.Type), payload)
		case <-heartbeat.C:
			// SSE 心跳，保持连接不被代理断开
			if _, err := c.Writer.WriteString(": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-reqCtx.Done():
			return
		}
	}
}

func writeSSE(c *gin.Context, f http.Flusher, event string, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	writeSSERaw(c, f, event, payload)
}

func writeSSERaw(c *gin.Context, f http.Flusher, event string, payload []byte) {
	if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return
	}
	f.Flush()
}

func parseQueryInt(c *gin.Context, key string, def int) int {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

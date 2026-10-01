package task

import (
	"context"
	"sort"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ---------- Execution Timeline ----------

// Timeline 是任务执行时间线，专为前端 Timeline 组件设计。
//
// 与 OTel Trace 的区别：
//   - OTel Trace：系统级，关注性能瓶颈、错误定位（span、延迟、错误）
//   - Execution Timeline：业务级，关注"任务干了什么"（节点、token、工具调用、子任务）
//
// 前端可以直接用这份数据渲染成类似 LangSmith 的执行时间线：
//   ━━━━━━━━━━━━ Task (total: 2.3s)
//   ━━━━ Input (10ms)
//   ━━━━━━━━━━ LLM (800ms, 3000 tokens)
//     ━━━ Tool: search (500ms)
//     ━━━ Tool: calculator (200ms)
//   ━━━━━━━ RAG (600ms)
//   ━━━ Output (10ms)
type Timeline struct {
	TaskID     int64         `json:"task_id"`
	Status     string        `json:"status"`
	TotalMS    int64         `json:"total_ms"`    // 总耗时（毫秒）
	StartedAt  *time.Time    `json:"started_at"`
	FinishedAt *time.Time    `json:"finished_at"`
	Nodes      []NodeTimeline `json:"nodes"`       // 节点时间线（按开始时间排序）
	TokenUsage TokenUsage    `json:"token_usage"`
	ToolCalls  int           `json:"tool_calls"`  // 总工具调用次数
}

// NodeTimeline 是单个节点的执行时间线。
type NodeTimeline struct {
	NodeKey    string          `json:"node_key"`
	NodeType   string          `json:"node_type"`
	Status     string          `json:"status"`
	StartMS    int64           `json:"start_ms"`     // 相对任务开始的偏移（毫秒）
	DurationMS int64           `json:"duration_ms"`  // 耗时（毫秒）
	TokensIn   int             `json:"tokens_in,omitempty"`
	TokensOut  int             `json:"tokens_out,omitempty"`
	Error      string          `json:"error,omitempty"`
	Events     []TimelineEvent `json:"events,omitempty"` // 节点内部事件（工具调用等）
}

// TimelineEvent 是节点内的子事件（工具调用、token 流开始等）。
type TimelineEvent struct {
	Type       string `json:"type"`        // tool_call / tool_result / token_start / waiting
	Name       string `json:"name,omitempty"` // 工具名等
	OffsetMS   int64  `json:"offset_ms"`   // 相对节点开始的偏移
	DurationMS int64  `json:"duration_ms,omitempty"`
	Detail     string `json:"detail,omitempty"` // 简短描述
}

// TokenUsage 是任务级别的 token 统计。
type TokenUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Total      int `json:"total"`
	NodeCount  int `json:"node_count"` // 有 token 消耗的节点数
}

// TimelineStore 是 Timeline 需要的最小数据接口。
type TimelineStore interface {
	GetTaskByID(ctx context.Context, id int64) (*model.Task, error)
	GetTaskNodes(ctx context.Context, taskID int64) ([]model.TaskNode, error)
	ListLogs(ctx context.Context, taskID int64, limit int) ([]model.TaskLog, error)
}

// BuildTimeline 从任务数据构建执行时间线。
//
// 数据源：
//   - Task：任务级信息（状态、总耗时）
//   - TaskNode：节点级信息（开始/结束/耗时/token/状态）
//   - TaskLog：节点内的详细事件（工具调用、错误等）
//
// 设计原则：
//   1. 全部从数据库读，不依赖内存状态（任务执行完很久还能查）
//   2. 结构化输出，前端直接渲染，不需要再做计算
//   3. 相对时间（偏移量），前端画时间线不用自己算
func BuildTimeline(ctx context.Context, store TimelineStore, taskID int64) (*Timeline, error) {
	task, err := store.GetTaskByID(ctx, taskID)
	if err != nil {
		return nil, err
	}

	nodes, err := store.GetTaskNodes(ctx, taskID)
	if err != nil {
		return nil, err
	}

	logs, err := store.ListLogs(ctx, taskID, 500)
	if err != nil {
		return nil, err
	}

	tl := &Timeline{
		TaskID:     task.ID,
		Status:     string(task.Status),
		StartedAt:  task.StartedAt,
		FinishedAt: task.FinishedAt,
	}

	// 计算总耗时
	if task.StartedAt != nil && task.FinishedAt != nil {
		tl.TotalMS = task.FinishedAt.Sub(*task.StartedAt).Milliseconds()
	} else if task.StartedAt != nil {
		tl.TotalMS = time.Since(*task.StartedAt).Milliseconds()
	}

	// 任务开始时间作为时间线基准
	var baseTime time.Time
	if task.StartedAt != nil {
		baseTime = *task.StartedAt
	} else {
		baseTime = task.CreatedAt
	}

	// 按节点分组日志
	logsByNode := make(map[string][]model.TaskLog)
	for _, l := range logs {
		if l.NodeKey != "" {
			logsByNode[l.NodeKey] = append(logsByNode[l.NodeKey], l)
		}
	}

	// 构建节点时间线
	for _, n := range nodes {
		nt := NodeTimeline{
			NodeKey:    n.NodeKey,
			NodeType:   string(n.NodeType),
			Status:     string(n.Status),
			DurationMS: n.DurationMS,
			TokensIn:   n.TokensIn,
			TokensOut:  n.TokensOut,
			Error:      n.Error,
		}

		// 计算相对开始时间
		if n.StartedAt != nil {
			nt.StartMS = n.StartedAt.Sub(baseTime).Milliseconds()
			if nt.StartMS < 0 {
				nt.StartMS = 0
			}
		}

		// 从日志里提取事件
		nt.Events = extractNodeEvents(n, logsByNode[n.NodeKey])

		tl.Nodes = append(tl.Nodes, nt)

		// 累加 token
		tl.TokenUsage.Input += n.TokensIn
		tl.TokenUsage.Output += n.TokensOut
		if n.TokensIn > 0 || n.TokensOut > 0 {
			tl.TokenUsage.NodeCount++
		}
		tl.ToolCalls += countToolCalls(nt.Events)
	}

	tl.TokenUsage.Total = tl.TokenUsage.Input + tl.TokenUsage.Output

	// 按开始时间排序
	sort.Slice(tl.Nodes, func(i, j int) bool {
		return tl.Nodes[i].StartMS < tl.Nodes[j].StartMS
	})

	return tl, nil
}

// extractNodeEvents 从节点日志中提取时间线事件。
func extractNodeEvents(node model.TaskNode, logs []model.TaskLog) []TimelineEvent {
	if len(logs) == 0 {
		return nil
	}

	var events []TimelineEvent
	var nodeStart time.Time
	if node.StartedAt != nil {
		nodeStart = *node.StartedAt
	} else {
		nodeStart = time.Now()
	}

	// 跟踪进行中的工具调用
	pendingTools := make(map[string]time.Time)

	for _, l := range logs {
		offset := l.CreatedAt.Sub(nodeStart).Milliseconds()
		if offset < 0 {
			offset = 0
		}

		// 工具调用开始
		if l.Level == "info" && containsToolCall(l.Message) {
			name := extractToolName(l.Message)
			pendingTools[name] = l.CreatedAt
			events = append(events, TimelineEvent{
				Type:     "tool_call",
				Name:     name,
				OffsetMS: offset,
				Detail:   name,
			})
		}

		// 工具调用结果
		if l.Level == "info" && containsToolResult(l.Message) {
			name := extractToolName(l.Message)
			if start, ok := pendingTools[name]; ok {
				dur := l.CreatedAt.Sub(start).Milliseconds()
				// 找到对应的 tool_call 事件，补上 duration
				for i := len(events) - 1; i >= 0; i-- {
					if events[i].Type == "tool_call" && events[i].Name == name && events[i].DurationMS == 0 {
						events[i].DurationMS = dur
						events[i].Type = "tool"
						break
					}
				}
				delete(pendingTools, name)
			}
		}

		// 错误
		if l.Level == "error" {
			events = append(events, TimelineEvent{
				Type:     "error",
				OffsetMS: offset,
				Detail:   l.Message,
			})
		}
	}

	// 审批等待事件
	if node.Status == model.NodeWaiting {
		events = append(events, TimelineEvent{
			Type:     "waiting",
			OffsetMS: node.DurationMS, // 从开始到现在一直在等
			Detail:   "等待人工审批",
		})
	}

	return events
}

// containsToolCall 判断日志是否是工具调用开始。
func containsToolCall(msg string) bool {
	return (len(msg) > 10 && msg[:10] == "tool_call:") ||
		(len(msg) > 13 && msg[:13] == "agent: tool ")
}

// containsToolResult 判断日志是否是工具调用结果。
func containsToolResult(msg string) bool {
	return (len(msg) > 12 && msg[:12] == "tool_result:") ||
		(len(msg) > 16 && msg[:16] == "agent: tool done")
}

// extractToolName 从日志消息中提取工具名。
func extractToolName(msg string) string {
	// 简单提取：找第一个空格和第二个空格之间的词
	for i := 0; i < len(msg); i++ {
		if msg[i] == ' ' || msg[i] == ':' {
			rest := msg[i+1:]
			// 跳过空格
			j := 0
			for j < len(rest) && rest[j] == ' ' {
				j++
			}
			rest = rest[j:]
			// 取到下一个空格或换行
			end := 0
			for end < len(rest) && rest[end] != ' ' && rest[end] != '\n' && rest[end] != ':' {
				end++
			}
			if end > 0 && end <= 64 {
				return rest[:end]
			}
			return "unknown"
		}
	}
	return "unknown"
}

// countToolCalls 统计工具调用次数。
func countToolCalls(events []TimelineEvent) int {
	count := 0
	for _, e := range events {
		if e.Type == "tool" || e.Type == "tool_call" {
			count++
		}
	}
	return count
}

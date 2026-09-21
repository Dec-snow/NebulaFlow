// Package task 提供任务的服务层与 SSE 实时推送中枢。
//
// Hub 采用订阅-发布模型：scheduler 在节点状态变化时 Publish，
// SSE handler 从 Subscribe 拿到事件流，通过 HTTP SSE 推给浏览器。
package task

import (
	"sync"
	"sync/atomic"
	"time"
)

// EventType 定义 SSE 事件类型。
type EventType string

const (
	EventTaskCreated   EventType = "task_created"
	EventTaskRunning   EventType = "task_running"
	EventTaskCompleted EventType = "task_completed"
	EventTaskFailed    EventType = "task_failed"
	EventTaskCancelled EventType = "task_cancelled"
	EventNodeStarted   EventType = "node_started"
	EventNodeCompleted EventType = "node_completed"
	EventNodeFailed    EventType = "node_failed"
	EventNodeSkipped   EventType = "node_skipped"
	EventNodeCancelled EventType = "node_cancelled"
	EventToken         EventType = "token" // LLM 流式 token
	EventLog           EventType = "log"
	EventFallback      EventType = "fallback" // LLM 故障转移通知

	// EventToolCall / EventToolResult：Agent 自主调用工具的过程事件。
	// 与 node_started/node_completed 分开是有意的 —— 工具调用发生在**一个 LLM 节点内部**，
	// 一轮里可能有多次。塞进节点事件里，前端就只能看到"节点跑了很久"，
	// 看不到"它其实调了三次工具、第二次还失败了"，而这正是 Agent 最需要被观察的部分。
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
)

// Event 是推送给前端的事件。
type Event struct {
	Type       EventType `json:"type"`
	TaskID     int64     `json:"task_id"`
	TaskStatus string    `json:"task_status,omitempty"`
	NodeKey    string    `json:"node_key,omitempty"`
	NodeType   string    `json:"node_type,omitempty"`
	NodeStatus string    `json:"node_status,omitempty"`
	// ToolName 只在 tool_call / tool_result 事件上出现。
	ToolName   string    `json:"tool_name,omitempty"`
	Content    string    `json:"content,omitempty"`
	Message    string    `json:"message,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

func NewEvent(t EventType, taskID int64) Event {
	return Event{Type: t, TaskID: taskID, Timestamp: time.Now()}
}

// replayCap 是每个任务保留的最近事件数，供新订阅者（或断线重连）回放。
const replayCap = 64

// Hub 维护 task → 订阅者通道的映射。
//
// 并发模型：全局一把互斥锁，Publish 在持锁期间完成「写回放缓冲 + 非阻塞发送」。
// 因为发送一定是 select+default 的非阻塞形式，持锁期间不会睡眠，
// 所以序列化 Publish 的代价极低（远小于一次节点执行）。
// unsub 同样需要这把锁，因此 close(ch) 与 ch <- ev 天然互斥，
// 彻底排除 "send on closed channel" panic —— 原实现把发送放在锁外，
// 正好留出了 close 与 send 重叠的窗口，SSE 客户端频繁断连时必然踩到。
type Hub struct {
	mu       sync.Mutex
	subs     map[int64]map[chan Event]struct{}
	replay   map[int64][]Event
	dropped  atomic.Int64
	subCount atomic.Int64
}

func NewHub() *Hub {
	return &Hub{
		subs:   make(map[int64]map[chan Event]struct{}),
		replay: make(map[int64][]Event),
	}
}

// Subscribe 订阅某个任务的实时事件，返回事件通道与退订函数。
// 订阅时会先回放该任务最近的事件，避免浏览器刚连上时错过已完成节点的状态。
func (h *Hub) Subscribe(taskID int64) (<-chan Event, func()) {
	ch := make(chan Event, 128)
	h.mu.Lock()
	if h.subs[taskID] == nil {
		h.subs[taskID] = make(map[chan Event]struct{})
	}
	h.subs[taskID][ch] = struct{}{}
	h.subCount.Add(1)
	backlog := append([]Event(nil), h.replay[taskID]...)
	h.mu.Unlock()

	// 在锁外投递回放，避免慢订阅者阻塞 Publish
	for _, ev := range backlog {
		select {
		case ch <- ev:
		default:
			// 回放塞满说明订阅者已经落后很多，放弃回放即可
		}
	}

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			if set := h.subs[taskID]; set != nil {
				if _, ok := set[ch]; ok {
					delete(set, ch)
					close(ch)
					h.subCount.Add(-1)
				}
				if len(set) == 0 {
					delete(h.subs, taskID)
					delete(h.replay, taskID)
				}
			}
			h.mu.Unlock()
		})
	}
	return ch, unsub
}

// Publish 向订阅者广播事件（非阻塞，满了丢弃并计数）。
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.appendReplay(ev)
	for c := range h.subs[ev.TaskID] {
		select {
		case c <- ev:
		default:
			h.dropped.Add(1)
		}
	}
}

// appendReplay 维护最近事件环形缓冲，调用方必须持有 mu。
func (h *Hub) appendReplay(ev Event) {
	buf := h.replay[ev.TaskID]
	// token 事件量大且可从节点输出还原，不入回放缓冲
	if ev.Type == EventToken {
		return
	}
	buf = append(buf, ev)
	if len(buf) > replayCap {
		buf = buf[len(buf)-replayCap:]
	}
	h.replay[ev.TaskID] = buf
}

// SubscriberCount 返回某任务的实时订阅数（用于监控）。
func (h *Hub) SubscriberCount(taskID int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[taskID])
}

// Stats 返回 Hub 的运行时指标：订阅者总数与被丢弃的事件总数。
// 丢弃数上升说明某个 SSE 客户端消费过慢，是排查"前端卡住"的第一手线索。
func (h *Hub) Stats() (subscribers, dropped int64) {
	return h.subCount.Load(), h.dropped.Load()
}

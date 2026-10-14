// Package task 提供任务的服务层与 SSE 实时推送中枢。
//
// Hub 采用订阅-发布模型：scheduler 在节点状态变化时 Publish，
// SSE handler 从 Subscribe 拿到事件流，通过 HTTP SSE 推给浏览器。
package task

import (
	"container/list"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// 连接数上限常量。
const (
	// MaxSubscriptionsPerUser 每用户最多并发 SSE 连接数。
	MaxSubscriptionsPerUser = 10
	// MaxTotalSubscriptions 全局总连接数上限。
	MaxTotalSubscriptions = 100
)

// SSE 连接超限错误。
var (
	// ErrTooManyConnections 全局连接数达到上限。
	ErrTooManyConnections = errors.New("too many sse connections")
	// ErrUserConnectionsExceeded 单用户连接数达到上限。
	ErrUserConnectionsExceeded = errors.New("user sse connection limit exceeded")
)

// EventType 定义 SSE 事件类型。
type EventType string

const (
	EventTaskCreated   EventType = "task_created"
	EventTaskRunning   EventType = "task_running"
	EventTaskWaiting   EventType = "task_waiting"
	EventTaskCompleted EventType = "task_completed"
	EventTaskFailed    EventType = "task_failed"
	EventTaskCancelled EventType = "task_cancelled"
	EventNodeStarted   EventType = "node_started"
	EventNodeWaiting   EventType = "node_waiting"
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

// MaxReplayTasks 是回放缓冲覆盖的**任务数**上界（超过则淘汰最久未发布的那个任务）。
//
// 为什么必须要有这个上界：replay 是按 taskID 累积的，每个任务留下约 244B 的事件切片。
// 原实现只在"最后一个订阅者退订"时删除条目，而**绝大多数任务从来没有订阅者**
// （不点开详情页就不会连 SSE），于是这些条目永远不会被删掉。
// 实测 20 万个任务累积 46MB，且随历史任务数**线性增长**——一个 7×24 跑着的服务
// 迟早会被它吃光内存，而症状（OOM）看起来和任务执行毫无关系。
//
// 1024 个任务 ≈ 250KB，足够覆盖"用户刚断开、马上重连"这个真实场景；
// 超出后淘汰的是最久没有新事件的任务，正在进行中的任务会因为持续 Publish 而保持新鲜。
const MaxReplayTasks = 1024

// subInfo 记录单个订阅的元信息，用于退订时正确扣减用户计数。
type subInfo struct {
	ch     chan Event
	userID int64
}

// Hub 维护 task → 订阅者通道的映射。
//
// 并发模型：全局一把互斥锁，Publish 在持锁期间完成「写回放缓冲 + 非阻塞发送」。
// 因为发送一定是 select+default 的非阻塞形式，持锁期间不会睡眠，
// 所以序列化 Publish 的代价极低（远小于一次节点执行）。
// unsub 同样需要这把锁，因此 close(ch) 与 ch <- ev 天然互斥，
// 彻底排除 "send on closed channel" panic —— 原实现把发送放在锁外，
// 正好留出了 close 与 send 重叠的窗口，SSE 客户端频繁断连时必然踩到。
//
// 连接数限制：
//   - 每用户最多 MaxSubscriptionsPerUser 个并发连接
//   - 全局最多 MaxTotalSubscriptions 个连接
//   - 超出时 Subscribe 返回错误，避免服务器资源被耗尽
type Hub struct {
	mu       sync.Mutex
	subs     map[int64]map[chan Event]subInfo // taskID -> channel -> subInfo
	replay   map[int64][]Event
	dropped  atomic.Int64
	subCount atomic.Int64

	// replayLRU 按"最近发布"排序回放条目：Front 是最久没更新的任务。
	// replayIdx 是 taskID → 链表节点的反向索引，用于 O(1) 精确删除。
	//
	// 为什么不用一个 []int64 记录顺序：那需要额外的精确删除逻辑，
	// 而"只删切片头部、跳过已失效 id"的做法本身又会让那个切片无界增长——
	// 也就是把内存泄漏从一个 map 挪到另一个 map 里，问题并没有解决。
	replayLRU *list.List
	replayIdx map[int64]*list.Element
	maxReplay int

	// userSubs 记录每用户的连接数，用于单用户限流。
	userSubs map[int64]int
	// maxPerUser / maxTotal 是连接数上限，可由测试调整。
	maxPerUser int
	maxTotal   int
}

func NewHub() *Hub {
	return &Hub{
		subs:       make(map[int64]map[chan Event]subInfo),
		replay:     make(map[int64][]Event),
		replayLRU:  list.New(),
		replayIdx:  make(map[int64]*list.Element),
		maxReplay:  MaxReplayTasks,
		userSubs:   make(map[int64]int),
		maxPerUser: MaxSubscriptionsPerUser,
		maxTotal:   MaxTotalSubscriptions,
	}
}

// SetReplayLimit 调整回放缓冲覆盖的任务数上界（测试用；必须在使用前调用）。
func (h *Hub) SetReplayLimit(n int) {
	if n <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maxReplay = n
}

// ReplayTaskCount 返回当前保留了回放的任务数（监控"长期运行内存是否线性增长"用）。
func (h *Hub) ReplayTaskCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.replay)
}

// SetLimits 允许测试或配置调整连接数上限（必须在使用前调用）。
func (h *Hub) SetLimits(perUser, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if perUser > 0 {
		h.maxPerUser = perUser
	}
	if total > 0 {
		h.maxTotal = total
	}
}

// Subscribe 订阅某个任务的实时事件，返回事件通道、退订函数和错误。
// 订阅时会先回放该任务最近的事件，避免浏览器刚连上时错过已完成节点的状态。
//
// userID 用于连接数限制：每用户最多 MaxSubscriptionsPerUser 个并发 SSE 连接。
// 超出单用户限制返回 ErrUserConnectionsExceeded，超出全局限制返回 ErrTooManyConnections。
func (h *Hub) Subscribe(taskID int64, userID int64) (<-chan Event, func(), error) {
	h.mu.Lock()

	// ---- 连接数校验 ----
	total := int(h.subCount.Load())
	if total >= h.maxTotal {
		h.mu.Unlock()
		return nil, nil, ErrTooManyConnections
	}
	if h.userSubs[userID] >= h.maxPerUser {
		h.mu.Unlock()
		return nil, nil, ErrUserConnectionsExceeded
	}

	ch := make(chan Event, 128)
	if h.subs[taskID] == nil {
		h.subs[taskID] = make(map[chan Event]subInfo)
	}
	h.subs[taskID][ch] = subInfo{ch: ch, userID: userID}
	h.subCount.Add(1)
	h.userSubs[userID]++
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
				if info, ok := set[ch]; ok {
					delete(set, ch)
					close(ch)
					h.subCount.Add(-1)
					// 扣减用户计数
					h.userSubs[info.userID]--
					if h.userSubs[info.userID] <= 0 {
						delete(h.userSubs, info.userID)
					}
				}
				if len(set) == 0 {
					delete(h.subs, taskID)
					h.dropReplayLocked(taskID)
				}
			}
			h.mu.Unlock()
		})
	}
	return ch, unsub, nil
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
	// token 事件量大且可从节点输出还原，不入回放缓冲
	if ev.Type == EventToken {
		return
	}
	if _, seen := h.replayIdx[ev.TaskID]; !seen {
		// 新任务：先腾出位置（淘汰最久没更新的那个任务），再登记。
		h.evictReplayLocked()
		h.replayIdx[ev.TaskID] = h.replayLRU.PushBack(ev.TaskID)
	} else {
		// 已有条目：把它移到链表尾部，保持"最久未发布在最前"的顺序。
		h.replayLRU.MoveToBack(h.replayIdx[ev.TaskID])
	}
	buf := h.replay[ev.TaskID]
	buf = append(buf, ev)
	if len(buf) > replayCap {
		// 只保留最近 replayCap 条：切片尾部就是最新的。
		//
		// 这里刻意用"同一底层数组内往前挪"（overlap 的 memmove 由 copy 保证正确），
		// 而不是 `append(buf[:0], ...)` 之外的两种写法：
		//   - `buf = buf[len(buf)-replayCap:]` 会让底层数组的起始偏移一直往前爬，
		//     被丢掉的事件占的内存要等下一次扩容才还回去；
		//   - `make + copy` 每次发布都新分配一个 64 元素的切片
		//     （≈16KB/事件），在 SSE 这种"每次节点状态变化都发一条"的路径上
		//     是实打实的吞吐回归。
		buf = append(buf[:0], buf[len(buf)-replayCap:]...)
	}
	h.replay[ev.TaskID] = buf
}

// evictReplayLocked 在回放条目数达到上界时淘汰最久未发布的任务。
// 调用方必须持有 mu。
func (h *Hub) evictReplayLocked() {
	if h.maxReplay <= 0 {
		return
	}
	for len(h.replay) >= h.maxReplay && h.replayLRU.Len() > 0 {
		oldest := h.replayLRU.Front()
		taskID, _ := oldest.Value.(int64)
		h.replayLRU.Remove(oldest)
		delete(h.replayIdx, taskID)
		delete(h.replay, taskID)
	}
}

// dropReplayLocked 删除某个任务的回放条目并同步链表索引。调用方必须持有 mu。
func (h *Hub) dropReplayLocked(taskID int64) {
	delete(h.replay, taskID)
	if el, ok := h.replayIdx[taskID]; ok {
		h.replayLRU.Remove(el)
		delete(h.replayIdx, taskID)
	}
}

// SubscriberCount 返回某任务的实时订阅数（用于监控）。
func (h *Hub) SubscriberCount(taskID int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[taskID])
}

// UserSubscriberCount 返回某用户的实时订阅数（用于监控）。
func (h *Hub) UserSubscriberCount(userID int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.userSubs[userID]
}

// HubStats 是 Hub 的运行时指标快照。
type HubStats struct {
	Subscribers int64 // 总订阅数
	Dropped     int64 // 被丢弃的事件总数
	Users       int   // 有活跃连接的用户数
	// ReplayTasks 是当前保留回放的任务数（上界 MaxReplayTasks）。
	// 这个数字应当**稳定在上界附近**而不是随历史任务数一直涨：
	// 它一直涨就说明回放条目没有被回收，内存会线性增长。
	ReplayTasks int
}

// Stats 返回 Hub 的运行时指标：订阅者总数、被丢弃的事件总数、活跃用户数、回放任务数。
// 丢弃数上升说明某个 SSE 客户端消费过慢，是排查"前端卡住"的第一手线索。
func (h *Hub) Stats() HubStats {
	h.mu.Lock()
	users := len(h.userSubs)
	replay := len(h.replay)
	h.mu.Unlock()
	return HubStats{
		Subscribers: h.subCount.Load(),
		Dropped:     h.dropped.Load(),
		Users:       users,
		ReplayTasks: replay,
	}
}

package task

import (
	"strconv"
	"testing"
	"time"
)

// 这个文件守的是 Hub 回放缓冲的**内存上界**。
//
// 缺陷回顾：replay 是按 taskID 累积的 map，每个任务留下约 244B 的事件切片。
// 原实现只在"最后一个订阅者退订"时删除条目，而**绝大多数任务从来没有订阅者**
// （不点开任务详情页就不会连 SSE），于是这些条目永远不会被回收。
// 实测 20 万个任务累积 46MB，且随历史任务数**线性增长**——
// 一个 7×24 运行的服务迟早会被它吃光内存，而 OOM 的症状看起来
// 和任务执行、SSE 推送都毫无关系。
//
// 修复：给回放缓冲加"任务数"上界，超出时淘汰最久没有新事件的任务（LRU）。

// 发布远超上界数量的任务后，回放条目数必须被限制在上界内。
func TestHub_ReplayBoundedByTaskCount(t *testing.T) {
	h := NewHub()
	const limit = 4
	h.SetReplayLimit(limit)

	const tasks = 50
	for i := 1; i <= tasks; i++ {
		ev := NewEvent(EventNodeCompleted, int64(i))
		ev.NodeKey = "node"
		h.Publish(ev)
	}

	if got := h.ReplayTaskCount(); got > limit {
		t.Fatalf("回放条目数必须 <= 上界 %d，实际 %d（内存会随历史任务数线性增长）", limit, got)
	}
	if got := h.Stats().ReplayTasks; got > limit {
		t.Fatalf("Stats().ReplayTasks 也必须反映上界，实际 %d", got)
	}
	// 淘汰不能只是把 map 里的数据删掉，LRU 链表与反向索引也必须同步收缩——
	// 否则泄漏只是从一个 map 挪到了另一个结构里，内存照样线性增长。
	if got := h.replayLRU.Len(); got != h.ReplayTaskCount() {
		t.Fatalf("LRU 链表长度（%d）必须与回放条目数（%d）一致，否则索引本身在泄漏",
			got, h.ReplayTaskCount())
	}
	if got := len(h.replayIdx); got != h.ReplayTaskCount() {
		t.Fatalf("反向索引大小（%d）必须与回放条目数（%d）一致", got, h.ReplayTaskCount())
	}

	// 淘汰方向：留下来的必须是**最新**的任务（"刚断开、马上重连"才是回放的目标场景）
	ch, unsub, err := h.Subscribe(tasks, 1)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()
	select {
	case ev := <-ch:
		if ev.TaskID != tasks {
			t.Fatalf("最新任务应能回放到自己的事件，实际 taskID=%d", ev.TaskID)
		}
	case <-time.After(time.Second):
		t.Fatal("最新的任务被淘汰了：回放缓冲淘汰的应该是最旧的任务")
	}

	// 最旧的任务已经被淘汰：不应再有回放（但订阅本身仍然可用——
	// 淘汰的是历史回放，不是实时推送能力）
	chOld, unsubOld, err := h.Subscribe(1, 2)
	if err != nil {
		t.Fatalf("subscribe old: %v", err)
	}
	defer unsubOld()
	select {
	case ev := <-chOld:
		t.Fatalf("最旧的任务（task 1）应已被淘汰，却回放出了 %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// 上界不能把"正在活跃的任务"误伤掉：持续有事件的任务应当保持新鲜（LRU 而非 FIFO）。
func TestHub_ReplayKeepsActiveTaskFresh(t *testing.T) {
	h := NewHub()
	h.SetReplayLimit(3)

	// task 1 先出现，随后一直被"使用"
	h.Publish(NewEvent(EventNodeStarted, 1))
	for i := 10; i <= 40; i++ {
		h.Publish(NewEvent(EventNodeCompleted, 1)) // 持续刷新 task 1
		h.Publish(NewEvent(EventNodeCompleted, int64(i)))
	}

	ch, unsub, err := h.Subscribe(1, 1)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()
	select {
	case <-ch:
		// 有回放 = 还在缓冲里，正确
	case <-time.After(time.Second):
		t.Fatal("持续有新事件的任务不应被淘汰（LRU 应刷新它的新鲜度）")
	}
	if got := h.ReplayTaskCount(); got > 3 {
		t.Fatalf("回放条目数必须 <= 上界 3，实际 %d", got)
	}
}

// 单个任务的事件条数上界（replayCap）依然生效，且保留的必须是**最新**的那些事件。
func TestHub_ReplayPerTaskStillCapped(t *testing.T) {
	h := NewHub()
	const total = replayCap + 30
	for i := 0; i < total; i++ {
		ev := NewEvent(EventNodeCompleted, 5)
		ev.NodeKey = strconv.Itoa(i)
		h.Publish(ev)
	}
	buf := h.replay[5]
	if len(buf) != replayCap {
		t.Fatalf("单任务回放条数应为 %d，实际 %d", replayCap, len(buf))
	}
	// 窗口滑动方向：丢最旧的、留最新的。顺序错乱的话，前端重连后
	// 拿到的事件时间线会是反的（比没有回放更难排查）。
	if buf[0].NodeKey != strconv.Itoa(total-replayCap) {
		t.Fatalf("窗口里的第一条应为倒数第 %d 条（NodeKey=%d），实际 %s",
			replayCap, total-replayCap, buf[0].NodeKey)
	}
	if buf[len(buf)-1].NodeKey != strconv.Itoa(total-1) {
		t.Fatalf("窗口里的最后一条应是最新事件（NodeKey=%d），实际 %s",
			total-1, buf[len(buf)-1].NodeKey)
	}
}

// 退订不会把回放索引弄乱：最后一个订阅者退订后条目被删，且总条目数不会因此膨胀。
func TestHub_UnsubscribeDropsReplayWithoutLeakingIndex(t *testing.T) {
	h := NewHub()
	h.Publish(NewEvent(EventNodeCompleted, 42))
	if h.ReplayTaskCount() != 1 {
		t.Fatalf("发布后应有 1 条回放，实际 %d", h.ReplayTaskCount())
	}

	_, unsub, err := h.Subscribe(42, 7)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	unsub()

	if got := h.ReplayTaskCount(); got != 0 {
		t.Fatalf("最后一个订阅者退订后应删除回放条目，实际 %d", got)
	}
	// 索引也必须一起清掉，否则 replayLRU 会无界增长（把泄漏从一个 map 挪到链表）
	if got := h.replayLRU.Len(); got != 0 {
		t.Fatalf("回放 LRU 索引应被清空，实际残留 %d 个元素", got)
	}
	if got := len(h.replayIdx); got != 0 {
		t.Fatalf("回放反向索引应被清空，实际残留 %d 个元素", got)
	}
}

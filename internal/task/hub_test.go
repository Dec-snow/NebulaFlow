package task

import (
	"sync"
	"testing"
	"time"
)

// 并发发布 + 反复订阅/退订：原实现在 Publish 释放读锁之后才发送，
// unsub 可能在同一窗口关闭通道，触发 "send on closed channel" panic。
// 这个用例必须配合 -race 跑，且在修复前稳定复现。
func TestHub_PublishSubscribeRace(t *testing.T) {
	h := NewHub()
	const (
		publishers  = 8
		subscribers = 8
		iterations  = 300
	)

	var wg sync.WaitGroup
	// 发布者
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ev := NewEvent(EventNodeCompleted, int64(1+i%3))
				ev.NodeKey = "node"
				ev.Content = "x"
				h.Publish(ev)
			}
		}(p)
	}
	// 订阅者：不断订阅、读取、退订
	for s := 0; s < subscribers; s++ {
		wg.Add(1)
		go func(userID int64) {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				taskID := int64(1 + i%3)
				ch, unsub, err := h.Subscribe(taskID, userID)
				if err != nil {
					t.Errorf("subscribe failed: %v", err)
					return
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					for range ch {
					}
				}()
				time.Sleep(time.Millisecond)
				unsub()
				<-done
			}
		}(int64(s + 1))
	}
	wg.Wait()

	if got := h.SubscriberCount(1); got != 0 {
		t.Fatalf("all subscribers should be gone, got %d", got)
	}
	if stats := h.Stats(); stats.Subscribers != 0 {
		t.Fatalf("subscriber counter leaked: %d", stats.Subscribers)
	}
}

// 新订阅者应能收到订阅前发生的事件回放（断线重连场景下前端不会丢进度）。
func TestHub_ReplayForLateSubscriber(t *testing.T) {
	h := NewHub()
	h.Publish(NewEvent(EventTaskRunning, 7))
	h.Publish(NewEvent(EventNodeCompleted, 7))

	ch, unsub, err := h.Subscribe(7, 1)
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}
	defer unsub()

	got := make([]EventType, 0, 2)
	timeout := time.After(time.Second)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev.Type)
		case <-timeout:
			t.Fatalf("expected replayed events, got %v", got)
		}
	}
	if got[0] != EventTaskRunning || got[1] != EventNodeCompleted {
		t.Fatalf("replay order wrong: %v", got)
	}
}

// 慢消费者不应拖垮发布者：事件被丢弃而不是阻塞。
func TestHub_SlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	h := NewHub()
	ch, unsub, err := h.Subscribe(9, 1)
	if err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			h.Publish(NewEvent(EventNodeCompleted, 9))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked by slow subscriber")
	}
	// 通道应当被填满（说明确实在丢弃而不是阻塞）
	if len(ch) == 0 {
		t.Fatal("expected buffered events")
	}
	if stats := h.Stats(); stats.Dropped == 0 {
		t.Fatal("expected dropped counter to increase")
	}
}

// ---------- 连接数限制测试 ----------

// 单用户连接数达到上限后拒绝新连接。
func TestHub_PerUserLimit(t *testing.T) {
	h := NewHub()
	h.SetLimits(3, 100) // 每用户 3 个

	userID := int64(42)
	var unsubs []func()
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	// 建立 3 个连接，应该都成功
	for i := 0; i < 3; i++ {
		_, unsub, err := h.Subscribe(int64(i+1), userID)
		if err != nil {
			t.Fatalf("subscribe %d should succeed: %v", i, err)
		}
		unsubs = append(unsubs, unsub)
	}

	// 第 4 个连接应该被拒绝
	_, _, err := h.Subscribe(99, userID)
	if err == nil {
		t.Fatal("expected error when exceeding per-user limit")
	}
	if err != ErrUserConnectionsExceeded {
		t.Fatalf("expected ErrUserConnectionsExceeded, got %v", err)
	}

	// 另一个用户应该不受影响
	_, unsub2, err := h.Subscribe(99, 999)
	if err != nil {
		t.Fatalf("other user should not be affected: %v", err)
	}
	unsub2()

	// 退订一个后可以再订阅
	unsubs[0]()
	unsubs = unsubs[1:]
	_, unsub, err := h.Subscribe(99, userID)
	if err != nil {
		t.Fatalf("should allow after unsub: %v", err)
	}
	unsub()
}

// 全局连接数达到上限后拒绝新连接。
func TestHub_TotalLimit(t *testing.T) {
	h := NewHub()
	h.SetLimits(10, 5) // 全局 5 个

	var unsubs []func()
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	// 建立 5 个连接（不同用户）
	for i := 0; i < 5; i++ {
		_, unsub, err := h.Subscribe(int64(i+1), int64(i+1))
		if err != nil {
			t.Fatalf("subscribe %d should succeed: %v", i, err)
		}
		unsubs = append(unsubs, unsub)
	}

	// 第 6 个连接应该被拒绝
	_, _, err := h.Subscribe(99, 999)
	if err == nil {
		t.Fatal("expected error when exceeding total limit")
	}
	if err != ErrTooManyConnections {
		t.Fatalf("expected ErrTooManyConnections, got %v", err)
	}

	// 退订一个后可以再订阅
	unsubs[0]()
	unsubs = unsubs[1:]
	_, unsub, err := h.Subscribe(99, 999)
	if err != nil {
		t.Fatalf("should allow after unsub: %v", err)
	}
	unsub()
}

// 用户计数在退订时正确扣减，退订后用户计数归零则移除条目。
func TestHub_UserCountCleanup(t *testing.T) {
	h := NewHub()
	userID := int64(7)

	_, unsub1, err := h.Subscribe(1, userID)
	if err != nil {
		t.Fatal(err)
	}
	_, unsub2, err := h.Subscribe(2, userID)
	if err != nil {
		t.Fatal(err)
	}

	if h.UserSubscriberCount(userID) != 2 {
		t.Fatalf("expected 2 subs for user, got %d", h.UserSubscriberCount(userID))
	}
	if stats := h.Stats(); stats.Users != 1 {
		t.Fatalf("expected 1 active user, got %d", stats.Users)
	}

	unsub1()
	if h.UserSubscriberCount(userID) != 1 {
		t.Fatalf("expected 1 sub after first unsub, got %d", h.UserSubscriberCount(userID))
	}

	unsub2()
	if h.UserSubscriberCount(userID) != 0 {
		t.Fatalf("expected 0 after all unsub, got %d", h.UserSubscriberCount(userID))
	}
	if stats := h.Stats(); stats.Users != 0 {
		t.Fatalf("expected 0 active users, got %d", stats.Users)
	}
}

// Stats 返回正确的指标。
func TestHub_Stats(t *testing.T) {
	h := NewHub()
	stats := h.Stats()
	if stats.Subscribers != 0 || stats.Dropped != 0 || stats.Users != 0 {
		t.Fatalf("unexpected initial stats: %+v", stats)
	}

	_, unsub1, _ := h.Subscribe(1, 10)
	_, unsub2, _ := h.Subscribe(2, 20)
	_, unsub3, _ := h.Subscribe(3, 10) // 同一用户第二个连接

	stats = h.Stats()
	if stats.Subscribers != 3 {
		t.Fatalf("expected 3 subscribers, got %d", stats.Subscribers)
	}
	if stats.Users != 2 {
		t.Fatalf("expected 2 users, got %d", stats.Users)
	}

	unsub1()
	unsub2()
	unsub3()

	stats = h.Stats()
	if stats.Subscribers != 0 {
		t.Fatalf("expected 0 subscribers after cleanup, got %d", stats.Subscribers)
	}
	if stats.Users != 0 {
		t.Fatalf("expected 0 users after cleanup, got %d", stats.Users)
	}
}

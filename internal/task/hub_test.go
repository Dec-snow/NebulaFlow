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
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				taskID := int64(1 + i%3)
				ch, unsub := h.Subscribe(taskID)
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
		}()
	}
	wg.Wait()

	if got := h.SubscriberCount(1); got != 0 {
		t.Fatalf("all subscribers should be gone, got %d", got)
	}
	if subs, _ := h.Stats(); subs != 0 {
		t.Fatalf("subscriber counter leaked: %d", subs)
	}
}

// 新订阅者应能收到订阅前发生的事件回放（断线重连场景下前端不会丢进度）。
func TestHub_ReplayForLateSubscriber(t *testing.T) {
	h := NewHub()
	h.Publish(NewEvent(EventTaskRunning, 7))
	h.Publish(NewEvent(EventNodeCompleted, 7))

	ch, unsub := h.Subscribe(7)
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
	ch, unsub := h.Subscribe(9)
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
	if _, dropped := h.Stats(); dropped == 0 {
		t.Fatal("expected dropped counter to increase")
	}
}

package queue

import (
	"encoding/json"
	"testing"
)

// TraceParent 必须是**向后兼容**的：旧版本写进 stream 的消息里没有这个字段，
// 新版本读到时不能报错，更不能因此丢掉任务。
//
// 这一点很实际：上线新版本时队列里通常还压着一批旧消息
// （Redis Stream 的消息会一直留着，直到被 ACK 且被 TrimConsumed 回收）。
// 如果新字段是必填的，那批消息会全部解码失败——而 Dequeue 对坏消息的处理是
// "直接 ACK 掉，避免阻塞队列"，等于**静默丢任务**，连 DLQ 都不会进。
func TestJobTraceParentIsBackwardCompatible(t *testing.T) {
	old := []byte(`{"task_id":123,"attempt":2}`)
	var j Job
	if err := json.Unmarshal(old, &j); err != nil {
		t.Fatalf("旧版本写入的消息必须能解码，实际 %v", err)
	}
	if j.TaskID != 123 || j.Attempt != 2 {
		t.Fatalf("旧消息的字段应被正确还原，实际 %+v", j)
	}
	if j.TraceParent != "" {
		t.Fatalf("旧消息没有 traceparent，应得到空串，实际 %q", j.TraceParent)
	}
}

func TestJobTraceParentRoundTrip(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	b := Job{TaskID: 7, TraceParent: tp}.Bytes()

	var out Job
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if out.TraceParent != tp {
		t.Fatalf("traceparent 应原样往返，实际 %q", out.TraceParent)
	}
	if out.TaskID != 7 {
		t.Fatalf("task id 应保留，实际 %d", out.TaskID)
	}
}

// 没有 traceparent 时不应把空字段写进消息体：队列里每条消息都要 XADD 存一份，
// 一个恒为空的字段是纯粹的存储浪费（重投与崩溃认领还会不断复制它）。
func TestJobOmitsEmptyTraceParent(t *testing.T) {
	b := Job{TaskID: 1}.Bytes()
	if string(b) != `{"task_id":1}` {
		t.Fatalf("空 traceparent 不应出现在 payload 里，实际 %s", b)
	}
}

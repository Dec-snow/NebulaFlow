package tracing

import "testing"

// trace 归属被劫持的回归测试（store 层）。
//
// 缺陷回顾：SpanStore.Link 里是
//
//	if ownerID != 0 { rec.ownerID = ownerID }
//
// 无条件覆盖。而 trace id 是**可以从外部带进来的**（W3C traceparent 请求头），
// 于是任何人只要拿到别人的 traceparent 发一次请求，就能把这条 trace 的归属
// 改成自己，随后 /api/traces/<id> 就能读到对方整条链路 —— 而 span 里带着
// 原始错误文本（可能含 SQL、表名、内网地址）。
//
// 期望：**首次写入者胜**。已有非 0 owner 且与新 owner 不同时，
// 只补 taskID，不覆盖 owner。
func TestSpanStoreLinkIsFirstWriterWins(t *testing.T) {
	s := NewSpanStore(8, 8)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	// alice 先认领这条 trace（她的请求开始时就写入了归属）
	s.Link(traceID, 100, 7)
	if owner, known := s.Owner(traceID); !known || owner != 7 {
		t.Fatalf("首次 Link 应把 owner 写成 7，实际 %d（known=%v）", owner, known)
	}

	// bob 拿着同一个 traceparent 又链了一次：不能把归属抢走
	s.Link(traceID, 0, 8)
	if owner, _ := s.Owner(traceID); owner != 7 {
		t.Fatalf("已有归属的 trace 不能被后来者抢走：owner 应为 7，实际 %d", owner)
	}

	// 但任务索引仍然要能补写（任务关联与归属是两件事：
	// 按任务查 trace 的接口自己还要过 GetTask 的归属校验）
	s.Link(traceID, 200, 8)
	if got, ok := s.TraceForTask(200); !ok || got != traceID {
		t.Fatalf("taskID 仍应被补写：应反查到 %s，实际 %s（ok=%v）", traceID, got, ok)
	}
	if owner, _ := s.Owner(traceID); owner != 7 {
		t.Fatalf("补写 taskID 时不能顺手改掉 owner：应为 7，实际 %d", owner)
	}

	// 同一 owner 重复写入必须幂等（每个请求都会写一次）
	s.Link(traceID, 0, 7)
	if owner, _ := s.Owner(traceID); owner != 7 {
		t.Fatalf("同一 owner 重复写入应幂等，实际 %d", owner)
	}

	// owner 为 0 表示"尚未认领"：此时任何人都可以成为第一个认领者
	const fresh = "00000000000000000000000000000001"
	s.Link(fresh, 0, 9)
	if owner, _ := s.Owner(fresh); owner != 9 {
		t.Fatalf("无人认领的 trace 应可被认领，实际 owner=%d", owner)
	}
}

package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是 function calling 的**线上格式一致性测试**。
//
// 为什么必须用假服务端而不是只测自己的结构体：
// tool calling 的成败几乎全在"发出去的 JSON 长什么样"上，而结构体的字段名、
// 嵌套层级、null 与空串的区别，只有把真实的 HTTP 请求体解出来看才能确认。
// 只对 ChatResponse 做单元测试的话，把 tools 写成 {"type":"function"} 少一层
// 或者 parameters 序列化成字符串（json.RawMessage 写成 string 的经典错误）
// 都能全绿，而线上会被厂商直接拒绝。

// capturedRequest 是一次被记录的请求。
type capturedRequest struct {
	body map[string]any
	raw  string
}

type fakeOpenAI struct {
	mu    sync.Mutex
	reqs  []capturedRequest
	reply func(n int) string
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, capturedRequest{body: body, raw: string(raw)})
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(f.reply(n)))
}

func (f *fakeOpenAI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeOpenAI) request(t *testing.T, i int) capturedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.reqs) {
		t.Fatalf("请求 #%d 不存在，共收到 %d 个请求", i, len(f.reqs))
	}
	return f.reqs[i]
}

func startFakeOpenAI(t *testing.T, reply func(n int) string) (*OpenAICompatProvider, *fakeOpenAI) {
	t.Helper()
	fake := &fakeOpenAI{reply: reply}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return NewOpenAIProvider(srv.URL, "test-key"), fake
}

// messagesOf 取出请求体里的 messages 数组。
func messagesOf(t *testing.T, req capturedRequest) []map[string]any {
	t.Helper()
	raw, ok := req.body["messages"].([]any)
	if !ok {
		t.Fatalf("请求体里没有 messages 数组，实际内容：%s", req.raw)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, m := range raw {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("messages[%d] 不是对象：%v", i, m)
		}
		out = append(out, mm)
	}
	return out
}

// ---------- 请求方向：tools 的形状 ----------

const toolCallReply = `{
  "choices": [{
    "message": {
      "role": "assistant",
      "content": null,
      "tool_calls": [
        {"id": "call_abc", "type": "function",
         "function": {"name": "calculator", "arguments": "{\"expr\":\"(12+34)*5/2\"}"}}
      ]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens": 11, "completion_tokens": 7}
}`

// tools 必须以 {"type":"function","function":{...}} 的嵌套形状发出，
// 且 parameters 必须是 JSON **对象**而不是被序列化过的字符串。
func TestOpenAICompatSendsToolsInWireFormat(t *testing.T) {
	prov, fake := startFakeOpenAI(t, func(int) string { return toolCallReply })

	schema := json.RawMessage(`{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}`)
	_, err := prov.Chat(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "算一下"}},
		Tools: []ToolSpec{
			{Name: "calculator", Description: "计算数学表达式", Parameters: schema},
			{Name: "time", Description: "返回当前时间"},
		},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	tools, ok := fake.request(t, 0).body["tools"].([]any)
	if !ok {
		t.Fatalf("请求体里没有 tools 数组：%s", fake.request(t, 0).raw)
	}
	if len(tools) != 2 {
		t.Fatalf("tools 长度 = %d，期望 2", len(tools))
	}

	first, _ := tools[0].(map[string]any)
	if first["type"] != "function" {
		t.Errorf(`tools[0].type = %v，期望 "function"`, first["type"])
	}
	fn, ok := first["function"].(map[string]any)
	if !ok {
		t.Fatalf("tools[0].function 不是对象（是不是把 function 拍平了？）：%v", first)
	}
	if fn["name"] != "calculator" {
		t.Errorf("tools[0].function.name = %v", fn["name"])
	}
	if fn["description"] != "计算数学表达式" {
		t.Errorf("tools[0].function.description = %v", fn["description"])
	}
	// 关键断言：parameters 必须是对象。若 Parameters 字段被声明成 string，
	// 这里拿到的会是一个字符串，而线上厂商会直接拒绝这个 tools 数组。
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("tools[0].function.parameters 不是 JSON 对象而是 %T（json.RawMessage 被当字符串序列化了？）",
			fn["parameters"])
	}
	if params["type"] != "object" {
		t.Errorf("parameters.type = %v", params["type"])
	}
	if req, ok := params["required"].([]any); !ok || len(req) != 1 || req[0] != "expr" {
		t.Errorf("parameters.required = %v，期望 [expr]", params["required"])
	}

	// 第二个工具没有 schema：必须完全省略 parameters 键，
	// 而不是发 "parameters": null —— 部分厂商会因此拒绝整个请求。
	second, _ := tools[1].(map[string]any)
	fn2, _ := second["function"].(map[string]any)
	if _, present := fn2["parameters"]; present {
		t.Errorf("未声明 schema 的工具不应出现 parameters 键，实际：%v", fn2["parameters"])
	}
}

// 没有工具时不发 tools 键（不是发空数组）。
func TestOpenAICompatOmitsToolsWhenEmpty(t *testing.T) {
	prov, fake := startFakeOpenAI(t, func(int) string {
		return `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`
	})
	if _, err := prov.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if _, present := fake.request(t, 0).body["tools"]; present {
		t.Errorf("未提供工具时不应出现 tools 键：%s", fake.request(t, 0).raw)
	}
}

// ---------- 响应方向：tool_calls 的解析 ----------

func TestOpenAICompatParsesToolCalls(t *testing.T) {
	prov, _ := startFakeOpenAI(t, func(int) string { return toolCallReply })

	resp, err := prov.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "算一下"}},
		Tools:    []ToolSpec{{Name: "calculator"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls 长度 = %d，期望 1", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != "call_abc" {
		t.Errorf("ID = %q", call.ID)
	}
	if call.Name != "calculator" {
		t.Errorf("Name = %q", call.Name)
	}
	// 参数必须原样保留为字符串：中间层解析再序列化会引入失真
	if call.Arguments != `{"expr":"(12+34)*5/2"}` {
		t.Errorf("Arguments = %q，期望原样透传", call.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q，期望 tool_calls", resp.FinishReason)
	}
	if resp.Content != "" {
		t.Errorf("content 为 null 时 Content 应为空串，实际 %q", resp.Content)
	}
}

// 一轮里多个工具调用要按顺序全部解析出来。
func TestOpenAICompatParsesMultipleToolCallsInOrder(t *testing.T) {
	reply := `{"choices":[{"message":{"content":null,"tool_calls":[
	  {"id":"c1","type":"function","function":{"name":"calculator","arguments":"{\"expr\":\"1+1\"}"}},
	  {"id":"c2","type":"function","function":{"name":"time","arguments":"{}"}}
	]},"finish_reason":"tool_calls"}]}`
	prov, _ := startFakeOpenAI(t, func(int) string { return reply })

	resp, err := prov.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
		Tools:    []ToolSpec{{Name: "calculator"}, {Name: "time"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls 长度 = %d，期望 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "c1" || resp.ToolCalls[1].ID != "c2" {
		t.Errorf("顺序被破坏: %v", resp.ToolCalls)
	}
}

// name 为空的 tool_call 分片必须跳过。
//
// 流式响应里第一个分片常常只有 index、没有 name。原样收下会让上层拿一个
// 没有名字的工具去查注册表，报出难以理解的 `unknown tool ""`。
func TestOpenAICompatSkipsNamelessToolCallFragments(t *testing.T) {
	reply := `{"choices":[{"message":{"content":null,"tool_calls":[
	  {"index":0,"function":{"arguments":"{\"expr\":"}},
	  {"index":0,"id":"c9","type":"function","function":{"name":"calculator","arguments":"{\"expr\":\"2+2\"}"}}
	]},"finish_reason":"tool_calls"}]}`
	prov, _ := startFakeOpenAI(t, func(int) string { return reply })

	resp, err := prov.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}},
		Tools: []ToolSpec{{Name: "calculator"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls 长度 = %d，期望 1（空名分片应被跳过）: %v", len(resp.ToolCalls), resp.ToolCalls)
	}
	if resp.ToolCalls[0].Name != "calculator" || resp.ToolCalls[0].ID != "c9" {
		t.Errorf("解析结果不对: %+v", resp.ToolCalls[0])
	}
}

// 全是空名分片时返回 nil，而不是一个空壳调用。
func TestOpenAICompatAllNamelessToolCallsYieldsNil(t *testing.T) {
	reply := `{"choices":[{"message":{"content":"ok","tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"stop"}]}`
	prov, _ := startFakeOpenAI(t, func(int) string { return reply })
	resp, err := prov.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}},
		Tools: []ToolSpec{{Name: "calculator"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if resp.ToolCalls != nil {
		t.Errorf("ToolCalls 应为 nil，实际 %v", resp.ToolCalls)
	}
}

// ---------- 回灌方向：assistant / tool 消息的编码 ----------

// 这是整个 function calling 里最容易写错的一环：
//   - 带 tool_calls 的 assistant 消息，content 必须是 null（而不是 ""）；
//   - tool 消息必须带 tool_call_id，否则模型无法把结果对应回调用。
func TestOpenAICompatMarshalsToolRoundTripMessages(t *testing.T) {
	prov, fake := startFakeOpenAI(t, func(int) string {
		return `{"choices":[{"message":{"content":"算好了：115"},"finish_reason":"stop"}]}`
	})

	_, err := prov.Chat(context.Background(), ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleSystem, Content: "你是助手"},
			{Role: RoleUser, Content: "算一下"},
			// 上一轮模型的工具调用（Content 为空，仅有 ToolCalls）
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "call_1", Name: "calculator", Arguments: `{"expr":"(12+34)*5/2"}`},
			}},
			// 工具执行结果回灌
			{Role: RoleTool, ToolCallID: "call_1", Content: "115"},
		},
		Tools: []ToolSpec{{Name: "calculator"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}

	msgs := messagesOf(t, fake.request(t, 0))
	if len(msgs) != 4 {
		t.Fatalf("messages 长度 = %d，期望 4", len(msgs))
	}

	assistant := msgs[2]
	if assistant["role"] != "assistant" {
		t.Fatalf("msgs[2].role = %v", assistant["role"])
	}
	// content 键必须存在且为 JSON null。
	// 断言"键存在且值为 nil"而不是"值为空"：写成 "" 同样会被判为 falsy，
	// 但厂商的校验逻辑区分这两者，发 "" 会被拒。
	content, present := assistant["content"]
	if !present {
		t.Errorf("assistant 消息缺少 content 键，应为 null")
	} else if content != nil {
		t.Errorf("带 tool_calls 且无文本时 content 应为 null，实际 %#v", content)
	}

	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant.tool_calls 形状不对：%v", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Errorf("tool_calls[0].id = %v", call["id"])
	}
	if call["type"] != "function" {
		t.Errorf(`tool_calls[0].type = %v，期望 "function"`, call["type"])
	}
	cfn, ok := call["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0].function 不是对象：%v", call)
	}
	if cfn["name"] != "calculator" {
		t.Errorf("tool_calls[0].function.name = %v", cfn["name"])
	}
	if cfn["arguments"] != `{"expr":"(12+34)*5/2"}` {
		t.Errorf("tool_calls[0].function.arguments = %v", cfn["arguments"])
	}

	toolMsg := msgs[3]
	if toolMsg["role"] != "tool" {
		t.Errorf("msgs[3].role = %v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != "call_1" {
		t.Errorf("tool 消息必须带 tool_call_id，实际 %v", toolMsg["tool_call_id"])
	}
	if toolMsg["content"] != "115" {
		t.Errorf("tool 消息 content = %v，期望 115", toolMsg["content"])
	}
	// tool 消息不应带 tool_calls
	if _, present := toolMsg["tool_calls"]; present {
		t.Errorf("tool 消息不应出现 tool_calls：%v", toolMsg["tool_calls"])
	}
}

// 普通消息的 content 仍然必须是字符串（不能被 any 类型搞成别的东西）。
func TestOpenAICompatPlainMessageContentStaysString(t *testing.T) {
	prov, fake := startFakeOpenAI(t, func(int) string {
		return `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	})
	if _, err := prov.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "你好"}},
	}); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	msgs := messagesOf(t, fake.request(t, 0))
	if msgs[0]["content"] != "你好" {
		t.Errorf("普通消息 content = %#v，期望字符串 \"你好\"", msgs[0]["content"])
	}
}

// ---------- 流式与工具调用 ----------

// 流式不承载工具调用：必须明确报错，而不是返回一个空流。
//
// 返回空流会被网关判成"该 provider 无产出"从而触发一次毫无意义的 fallback，
// 把真正的调用方错误掩盖成"模型不听话"。
func TestStreamRejectsToolsInsteadOfSilentlyDroppingThem(t *testing.T) {
	prov, fake := startFakeOpenAI(t, func(int) string { return toolCallReply })

	_, err := prov.Stream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
		Tools:    []ToolSpec{{Name: "calculator"}},
	})
	if err == nil {
		t.Fatal("带工具调用时 Stream 应报错")
	}
	if !strings.Contains(err.Error(), "tool call") {
		t.Errorf("错误信息应说明原因，实际：%v", err)
	}
	if fake.count() != 0 {
		t.Errorf("报错后不应发出任何 HTTP 请求，实际发了 %d 个", fake.count())
	}
}

func TestMockProviderStreamRejectsTools(t *testing.T) {
	p := NewMockProvider("mock")
	if _, err := p.Stream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}},
		Tools: []ToolSpec{{Name: "calculator"}},
	}); err == nil {
		t.Fatal("MockProvider.Stream 带工具时应报错")
	}
}

// ---------- Ollama：明确不支持工具调用 ----------

func TestOllamaIgnoresToolsWithoutError(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"ok"},"done":true}`))
	}))
	defer srv.Close()

	prov := NewOllamaProvider(srv.URL)
	resp, err := prov.Chat(context.Background(), ChatRequest{
		Model: "qwen",
		Messages: []Message{
			{Role: RoleUser, Content: "x"},
			// 即便传入了 tool 消息，也不能让 content 变成 null —— Ollama 只接受字符串
			{Role: RoleTool, ToolCallID: "c1", Content: "42"},
		},
		Tools: []ToolSpec{{Name: "calculator"}},
	})
	if err != nil {
		t.Fatalf("Ollama Chat 不应因 tools 报错: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q", resp.Content)
	}
	if _, present := captured["tools"]; present {
		t.Errorf("Ollama 请求体不应出现 tools：%v", captured["tools"])
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 长度 = %d", len(msgs))
	}
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		if _, ok := mm["content"].(string); !ok {
			t.Errorf("messages[%d].content 必须是字符串，实际 %#v", i, mm["content"])
		}
	}
}

// ---------- MockProvider 的工具调用能力（离线验证的基础） ----------

func TestMockProviderAutoToolCallRoundTrip(t *testing.T) {
	p := NewMockProvider("mock")
	p.Latency = 0
	p.AutoToolCall = true

	specs := []ToolSpec{{Name: "calculator"}, {Name: "time"}}

	// 第一轮：模型看到工具，要求调用 calculator
	first, err := p.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "算一下"}}, Tools: specs,
	})
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if len(first.ToolCalls) != 1 {
		t.Fatalf("第一轮应返回 1 个工具调用，实际 %d", len(first.ToolCalls))
	}
	if first.ToolCalls[0].Name != "calculator" {
		t.Errorf("工具名 = %q", first.ToolCalls[0].Name)
	}
	if first.ToolCalls[0].ID == "" {
		t.Error("工具调用必须带 ID，否则结果无法对应回调用")
	}
	if first.Content != "" {
		t.Errorf("要求调用工具时不应同时给最终回答，实际 %q", first.Content)
	}
	if first.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", first.FinishReason)
	}

	// 第二轮：工具结果回灌 → 最终回答，且内容里带上工具输出
	second, err := p.Chat(context.Background(), ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleUser, Content: "算一下"},
			{Role: RoleAssistant, ToolCalls: first.ToolCalls},
			{Role: RoleTool, ToolCallID: first.ToolCalls[0].ID, Content: "115"},
		},
		Tools: specs,
	})
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if len(second.ToolCalls) != 0 {
		t.Errorf("第二轮不应再要求调用工具，实际 %v", second.ToolCalls)
	}
	if !strings.Contains(second.Content, "115") {
		t.Errorf("最终回答应包含工具结果 115，实际 %q", second.Content)
	}
}

// AutoToolCall 打开但请求里没有该工具时，不能凭空造一个调用。
//
// 少了这道检查，测试会通过一个真实模型不可能出现的前提
// （只暴露了工具子集却调用被排除的工具），把"白名单失效"掩盖过去。
func TestMockProviderDoesNotCallToolThatWasNotOffered(t *testing.T) {
	p := NewMockProvider("mock")
	p.Latency = 0
	p.AutoToolCall = true

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "算一下"}},
		Tools: []ToolSpec{{Name: "time"}}, // 没有 calculator
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("未提供的工具不应被调用，实际 %v", resp.ToolCalls)
	}
	if !strings.Contains(resp.Content, "echo") {
		t.Errorf("应退回普通 echo 回答，实际 %q", resp.Content)
	}
}

// 不打开 AutoToolCall 时行为完全不变（回归保护）。
func TestMockProviderWithoutAutoToolCallIsUnchanged(t *testing.T) {
	p := NewMockProvider("mock")
	p.Latency = 0

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hello world"}},
		Tools: []ToolSpec{{Name: "calculator"}}, // 有工具也不调用
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("未开启 AutoToolCall 时不应产生工具调用，实际 %v", resp.ToolCalls)
	}
	if !strings.Contains(resp.Content, "echo: hello world") {
		t.Errorf("输出应与改动前一致，实际 %q", resp.Content)
	}
}

// 自定义工具目标（测试里常用）。
func TestMockProviderAutoToolCallHonoursCustomTarget(t *testing.T) {
	p := NewMockProvider("mock")
	p.Latency = 0
	p.AutoToolCall = true
	p.ToolName = "time"
	p.ToolArgs = `{}`

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "几点"}},
		Tools: []ToolSpec{{Name: "calculator"}, {Name: "time"}},
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "time" {
		t.Fatalf("应调用 time，实际 %v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Arguments != "{}" {
		t.Errorf("Arguments = %q", resp.ToolCalls[0].Arguments)
	}
}

// AutoToolCall 下也不该无限循环：第二轮必定收敛到最终回答。
func TestMockProviderAutoToolCallConverges(t *testing.T) {
	p := NewMockProvider("mock")
	p.Latency = 0
	p.AutoToolCall = true
	specs := []ToolSpec{{Name: "calculator"}}

	msgs := []Message{{Role: RoleUser, Content: "go"}}
	for round := 0; round < 5; round++ {
		resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: msgs, Tools: specs})
		if err != nil {
			t.Fatalf("第 %d 轮失败: %v", round, err)
		}
		if len(resp.ToolCalls) == 0 {
			if round == 0 {
				t.Fatalf("第一轮就应该要求调用工具")
			}
			return // 收敛
		}
		msgs = append(msgs, Message{Role: RoleAssistant, ToolCalls: resp.ToolCalls})
		msgs = append(msgs, Message{Role: RoleTool, ToolCallID: resp.ToolCalls[0].ID, Content: "115"})
	}
	t.Fatal("5 轮内未收敛，MockProvider 的工具调用会与轮数上限一起造成死循环")
}

// 请求超时/取消必须能被 Chat 感知（agentic 路径依赖它来中断）。
func TestMockProviderChatHonoursContext(t *testing.T) {
	p := NewMockProvider("mock")
	p.Latency = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Chat(ctx, ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}}); err == nil {
		t.Fatal("上下文超时后 Chat 应返回错误")
	}
}

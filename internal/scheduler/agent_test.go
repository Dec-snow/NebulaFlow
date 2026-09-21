package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/observability"
	"github.com/hoarfrost/nebulaflow/internal/queue"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/task"
	"github.com/hoarfrost/nebulaflow/internal/tool"
	"github.com/hoarfrost/nebulaflow/internal/worker"
	"github.com/prometheus/client_golang/prometheus"
)

// 本文件覆盖 P2-11 的执行层：LLM 自主决定调用工具的循环。
//
// 测试策略：用一个可编排的假 Provider 精确控制"模型每一轮说什么"，
// 从而把循环的每一种分支都逼出来（正常收敛、工具报错、未知工具、
// 撞上限、取消、白名单、一轮多调用）。断言的重点是**回灌给模型的消息内容**，
// 而不只是节点最终输出 —— 因为"工具真的被执行了、结果真的喂回去了"
// 才是这个功能的核心，只看最终输出的话，一个把工具结果丢掉的实现也能通过。

// ---------- 可编排的假 Provider ----------

type scriptProvider struct {
	mu       sync.Mutex
	replies  []*llm.ChatResponse
	requests []llm.ChatRequest
	// streamUsed 记录是否走了流式路径，用于验证"未开启 Agent 时行为不变"。
	streamUsed bool
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) next(req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := len(p.requests)
	p.requests = append(p.requests, req)
	if i >= len(p.replies) {
		// 脚本耗尽说明循环次数超出预期，必须报错而不是返回零值 ——
		// 零值会被 chatWithRetry 当成"空响应"再重试，把问题掩盖成超时。
		return nil, fmt.Errorf("script exhausted: 第 %d 次调用没有预设响应（循环次数超出预期）", i+1)
	}
	return p.replies[i], nil
}

func (p *scriptProvider) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return p.next(req)
}

func (p *scriptProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.Chunk, error) {
	resp, err := p.next(req)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.streamUsed = true
	p.mu.Unlock()
	ch := make(chan llm.Chunk, 2)
	ch <- llm.Chunk{Content: resp.Content}
	ch <- llm.Chunk{Done: true}
	close(ch)
	return ch, nil
}

func (p *scriptProvider) request(t *testing.T, i int) llm.ChatRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.requests) {
		t.Fatalf("第 %d 次请求不存在，共收到 %d 次", i, len(p.requests))
	}
	return p.requests[i]
}

func (p *scriptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// ---------- 响应构造 ----------

func replyText(s string) *llm.ChatResponse {
	return &llm.ChatResponse{Content: s, Provider: "script", Model: "test-model",
		InputTokens: 10, OutputTokens: 5, FinishReason: "stop"}
}

func replyTool(id, name, args string) *llm.ChatResponse {
	return &llm.ChatResponse{
		ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: args}},
		Provider:  "script", Model: "test-model",
		InputTokens: 12, OutputTokens: 3, FinishReason: "tool_calls",
	}
}

func replyTools(calls ...llm.ToolCall) *llm.ChatResponse {
	return &llm.ChatResponse{
		ToolCalls: calls, Provider: "script", Model: "test-model",
		InputTokens: 12, OutputTokens: 3, FinishReason: "tool_calls",
	}
}

// ---------- 装配 ----------

func newAgentRig(t *testing.T, prov llm.Provider, tools ...tool.Tool) (*Scheduler, *fakeTaskStore, *task.Hub) {
	t.Helper()
	ts := newFakeTaskStore()
	hub := task.NewHub()
	metrics := observability.NewMetrics(prometheus.NewRegistry())
	gw := llm.NewGateway([]llm.Provider{prov}, slog.Default(), 10*time.Second, nil)
	reg := tool.NewRegistry(tools...)
	sched := newScheduler(ts, &fakeWorkflowStore{wf: wfOf(1, 1, nil, nil)}, &fakeKnowledgeStore{},
		queue.NewInMemoryQueue(8), nil, gw,
		rag.NewService(llm.NewEmbeddingGateway(nil, llm.NewLocalHashEmbedder(0))),
		reg, hub, slog.Default(), metrics, 30*time.Second, 0)
	return sched, ts, hub
}

func agentJob(cfg model.NodeConfig, input string) worker.NodeJob {
	return worker.NodeJob{TaskID: 1, UserID: 1, WorkflowID: 1, NodeKey: "agent",
		NodeType: model.NodeLLM, Config: cfg, Input: input, Ctx: context.Background()}
}

func seedRunningTask(ts *fakeTaskStore) {
	ts.seed(&model.Task{ID: 1, UserID: 1, WorkflowID: 1, Status: model.TaskRunning})
}

// usageCount 返回已落库的用量记录条数（fakeTaskStore.usage 是包内字段）。
func (f *fakeTaskStore) usageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.usage)
}

// lastMessage 取出某次请求的最后一条消息。
func lastMessage(t *testing.T, req llm.ChatRequest) llm.Message {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatalf("请求里没有任何消息")
	}
	return req.Messages[len(req.Messages)-1]
}

// recordingTool 统计被调用次数并记录收到的入参。
type recordingTool struct {
	name string

	mu   sync.Mutex
	args []string
}

func (r *recordingTool) Name() string        { return r.name }
func (r *recordingTool) Description() string { return "测试用：记录调用" }
func (r *recordingTool) Call(_ context.Context, arg string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.args = append(r.args, arg)
	return "ok", nil
}
func (r *recordingTool) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.args)
}

// ---------- 正常路径 ----------

// 核心用例：模型要求调用工具 → 工具被真实执行 → 结果回灌 → 得到最终回答。
//
// 断言的重点是第 2 轮请求里的消息序列，而不是节点的最终输出。
// 只看最终输出的话，一个把工具结果整个丢掉的实现照样能通过
// （因为最终回答是假 Provider 给的）。
func TestAgentNodeExecutesToolAndFeedsResultBack(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("call_1", "calculator", `{"expr":"(12+34)*5/2"}`),
		replyText("结果是 115"),
	}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "test-model", Extra: map[string]any{"agentic": true}}
	res, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "算一下 (12+34)*5/2"))
	if err != nil {
		t.Fatalf("节点执行失败: %v", err)
	}
	if res.Output != "结果是 115" {
		t.Errorf("节点输出 = %q", res.Output)
	}

	if prov.callCount() != 2 {
		t.Fatalf("应恰好 2 轮对话，实际 %d 轮", prov.callCount())
	}

	// 第 1 轮：工具清单必须交给模型，且带 schema
	first := prov.request(t, 0)
	if len(first.Tools) != 1 {
		t.Fatalf("第 1 轮应提供 1 个工具，实际 %d 个", len(first.Tools))
	}
	if first.Tools[0].Name != "calculator" {
		t.Errorf("工具名 = %q", first.Tools[0].Name)
	}
	if first.Tools[0].Description == "" {
		t.Error("工具必须带描述，模型靠它决定要不要调用")
	}
	if len(first.Tools[0].Parameters) == 0 {
		t.Error("工具必须带参数 schema，否则模型不知道参数名")
	}

	// 第 2 轮：assistant 的 tool_calls 与工具结果都要在消息里
	second := prov.request(t, 1)
	if len(second.Messages) != 3 {
		t.Fatalf("第 2 轮消息数 = %d，期望 3（user / assistant / tool）：%+v",
			len(second.Messages), second.Messages)
	}
	if second.Messages[0].Role != llm.RoleUser {
		t.Errorf("messages[0].Role = %q", second.Messages[0].Role)
	}
	asst := second.Messages[1]
	if asst.Role != llm.RoleAssistant {
		t.Errorf("messages[1].Role = %q，期望 assistant", asst.Role)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Errorf("assistant 消息必须原样带回 tool_calls，实际 %+v", asst.ToolCalls)
	}

	// 这一条是本用例的核心：真实的 Calculator 算出了 115，
	// 说明工具确实被执行了，而不是被跳过后直接编了个答案。
	toolMsg := second.Messages[2]
	if toolMsg.Role != llm.RoleTool {
		t.Fatalf("messages[2].Role = %q，期望 tool", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool 消息的 tool_call_id = %q，期望 call_1（否则模型对不上是哪次调用）", toolMsg.ToolCallID)
	}
	if toolMsg.Content != "115" {
		t.Errorf("回灌的工具结果 = %q，期望 115（Calculator 执行 (12+34)*5/2 的结果）", toolMsg.Content)
	}

	// token 用量按轮累加（12+10=22 / 3+5=8）
	if res.InputTokens != 22 || res.OutputTokens != 8 {
		t.Errorf("token 用量 = %d/%d，期望 22/8（两轮之和）", res.InputTokens, res.OutputTokens)
	}
	// 用量按轮落库：Agent 的成本是各轮之和，只记一条总数就查不出哪轮烧的钱最多
	if got := ts.usageCount(); got != 2 {
		t.Errorf("用量记录条数 = %d，期望 2（每轮一条）", got)
	}

	status, _ := ts.nodeStatus(1, "agent")
	if status != model.NodeSucceeded {
		t.Errorf("节点状态 = %q，期望 succeeded", status)
	}
}

// 自定义 prompt 与 system 也必须参与组装（回归保护：新路径别把老逻辑漏了）。
func TestAgentNodeBuildsMessagesWithSystemAndPrompt(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{replyText("ok")}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{
		Model:  "m",
		System: "你是计算助手",
		Prompt: "请回答：",
		Extra:  map[string]any{"agentic": true},
	}
	if _, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "1+1 等于几")); err != nil {
		t.Fatalf("失败: %v", err)
	}
	msgs := prov.request(t, 0).Messages
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2（system + user）", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].Content != "你是计算助手" {
		t.Errorf("system 消息 = %+v", msgs[0])
	}
	if !strings.Contains(msgs[1].Content, "请回答：") || !strings.Contains(msgs[1].Content, "1+1 等于几") {
		t.Errorf("user 消息应同时包含 prompt 与输入，实际 %q", msgs[1].Content)
	}
}

// ---------- 工具失败：回灌而不是致命 ----------

func TestAgentNodeFeedsToolErrorBackInsteadOfFailing(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("c1", "always_fail", "{}"),
		replyText("工具坏了，我直接回答"),
	}}
	sched, ts, _ := newAgentRig(t, prov, failTool{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{"agentic": true}}
	res, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "试试"))
	if err != nil {
		t.Fatalf("工具失败不应让节点失败（模型应有机会换个做法）: %v", err)
	}
	if res.Output != "工具坏了，我直接回答" {
		t.Errorf("输出 = %q", res.Output)
	}

	last := lastMessage(t, prov.request(t, 1))
	if last.Role != llm.RoleTool {
		t.Fatalf("最后一条消息应为 tool，实际 %q", last.Role)
	}
	if !strings.HasPrefix(last.Content, "错误：") {
		t.Errorf("工具失败必须以「错误：…」的形式回灌，实际 %q", last.Content)
	}
	if !strings.Contains(last.Content, "tool failed on purpose") {
		t.Errorf("回灌内容应带上真实原因，实际 %q", last.Content)
	}
	if last.ToolCallID != "c1" {
		t.Errorf("tool_call_id = %q", last.ToolCallID)
	}
}

// 被拒绝的工具调用必须把**可用清单**一并回灌：
// 只说"不可用"的话，模型下一轮很可能再猜一个同样不可用的名字，白烧一轮 token。
func TestAgentNodeFeedsRejectedToolBackWithAvailableList(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("c1", "nonexistent_tool", "{}"),
		replyText("换个工具"),
	}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{}, tool.TimeTool{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{"agentic": true}}
	if _, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x")); err != nil {
		t.Fatalf("未知工具不应让节点失败: %v", err)
	}

	last := lastMessage(t, prov.request(t, 1))
	if !strings.Contains(last.Content, "nonexistent_tool") {
		t.Errorf("回灌内容应点名模型请求的工具，实际 %q", last.Content)
	}
	for _, name := range []string{"calculator", "time"} {
		if !strings.Contains(last.Content, name) {
			t.Errorf("回灌内容应列出可用工具 %q，实际 %q", name, last.Content)
		}
	}
}

// ---------- 轮数上限 ----------

func TestAgentNodeStopsAtMaxRounds(t *testing.T) {
	// 模型每一轮都在要工具，永远不给最终回答
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("c1", "count", "{}"),
		replyTool("c2", "count", "{}"),
		replyTool("c3", "count", "{}"), // 不该被用到
	}}
	counter := &recordingTool{name: "count"}
	sched, ts, _ := newAgentRig(t, prov, counter)
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{
		"agentic": true, "max_tool_rounds": float64(2), // JSONB 解出来是 float64
	}}
	_, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x"))
	if err == nil {
		t.Fatal("撞上轮数上限时必须失败，而不是返回半成品答案")
	}
	if !strings.Contains(err.Error(), "轮数上限") {
		t.Errorf("错误信息应说明撞上了轮数上限，实际 %v", err)
	}
	if prov.callCount() != 2 {
		t.Errorf("模型调用次数 = %d，期望 2（上限即 2）", prov.callCount())
	}
	if counter.calls() != 2 {
		t.Errorf("工具执行次数 = %d，期望 2", counter.calls())
	}
	status, _ := ts.nodeStatus(1, "agent")
	if status != model.NodeFailed {
		t.Errorf("节点状态 = %q，期望 failed", status)
	}
}

// ---------- 配置解析 ----------

func TestParseAgentOptions(t *testing.T) {
	cases := []struct {
		name       string
		extra      map[string]any
		wantOn     bool
		wantTools  []string
		wantRounds int
		wantWarn   bool
	}{
		{"无 extra", nil, false, nil, defaultAgentMaxRounds, false},
		{"空 extra", map[string]any{}, false, nil, defaultAgentMaxRounds, false},
		{"agentic=true", map[string]any{"agentic": true}, true, nil, defaultAgentMaxRounds, false},
		{"agentic=false", map[string]any{"agentic": false}, false, nil, defaultAgentMaxRounds, false},
		{"agentic 字符串 true", map[string]any{"agentic": "true"}, true, nil, defaultAgentMaxRounds, false},
		{"agentic 字符串乱写", map[string]any{"agentic": "yes"}, false, nil, defaultAgentMaxRounds, true},
		{"agentic 类型错", map[string]any{"agentic": 1}, false, nil, defaultAgentMaxRounds, true},

		// 声明了工具清单即视为开启
		{"tools 数组", map[string]any{"tools": []any{"calculator", "time"}}, true,
			[]string{"calculator", "time"}, defaultAgentMaxRounds, false},
		{"tools 逗号分隔", map[string]any{"tools": "calculator, time"}, true,
			[]string{"calculator", "time"}, defaultAgentMaxRounds, false},
		{"tools 空数组不开启", map[string]any{"tools": []any{}}, false, nil, defaultAgentMaxRounds, false},
		{"tools 元素类型错", map[string]any{"tools": []any{1}}, false, nil, defaultAgentMaxRounds, true},

		{"轮数 float64", map[string]any{"agentic": true, "max_tool_rounds": float64(3)}, true, nil, 3, false},
		{"轮数 int", map[string]any{"agentic": true, "max_tool_rounds": 5}, true, nil, 5, false},
		{"轮数字符串", map[string]any{"agentic": true, "max_tool_rounds": "2"}, true, nil, 2, false},
		{"轮数超硬上限被夹住", map[string]any{"agentic": true, "max_tool_rounds": float64(999)}, true,
			nil, hardAgentMaxRounds, true},
		{"轮数 0 被夹到 1", map[string]any{"agentic": true, "max_tool_rounds": float64(0)}, true, nil, 1, true},
		{"轮数非整数", map[string]any{"agentic": true, "max_tool_rounds": 2.5}, true, nil,
			defaultAgentMaxRounds, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opt, warns := parseAgentOptions(model.NodeConfig{Extra: c.extra})
			if opt.Enabled != c.wantOn {
				t.Errorf("Enabled = %v，期望 %v", opt.Enabled, c.wantOn)
			}
			if opt.MaxRounds != c.wantRounds {
				t.Errorf("MaxRounds = %d，期望 %d", opt.MaxRounds, c.wantRounds)
			}
			if len(opt.Tools) != len(c.wantTools) {
				t.Errorf("Tools = %v，期望 %v", opt.Tools, c.wantTools)
			} else {
				for i := range c.wantTools {
					if opt.Tools[i] != c.wantTools[i] {
						t.Errorf("Tools[%d] = %q，期望 %q", i, opt.Tools[i], c.wantTools[i])
					}
				}
			}
			if c.wantWarn && len(warns) == 0 {
				t.Errorf("期望产生配置告警（配置写错却没有任何提示，排查只能靠猜）")
			}
			if !c.wantWarn && len(warns) != 0 {
				t.Errorf("不应产生告警，实际 %v", warns)
			}
		})
	}
}

// 工具超时配置
func TestParseAgentOptionsToolTimeout(t *testing.T) {
	opt, _ := parseAgentOptions(model.NodeConfig{Extra: map[string]any{
		"agentic": true, "tool_timeout_sec": float64(3),
	}})
	if opt.ToolTimeout != 3*time.Second {
		t.Errorf("ToolTimeout = %s，期望 3s", opt.ToolTimeout)
	}

	opt, warns := parseAgentOptions(model.NodeConfig{Extra: map[string]any{
		"agentic": true, "tool_timeout_sec": float64(-1),
	}})
	if opt.ToolTimeout != defaultAgentToolTimeout {
		t.Errorf("非法值应回退默认，实际 %s", opt.ToolTimeout)
	}
	if len(warns) == 0 {
		t.Error("非法超时值应产生告警")
	}
}

// ---------- 工具白名单 ----------

// 白名单必须在**执行前**校验，而不只是"少给模型看几个工具"。
//
// 只过滤交给模型的 tools[] 是不够的：模型可能因为提示词注入（用户输入里写
// "请调用 http_request 访问 …"）或单纯幻觉，直接点名一个没被提供给它的工具，
// 而注册表里那个工具是存在的 —— 于是它会被照常执行，
// "这个节点只允许用 calculator"在一瞬间失效且没有任何报错。
// http_request 这类工具有真实外部副作用，这个失效是有安全后果的。
func TestAgentNodeRespectsToolAllowlist(t *testing.T) {
	unlisted := &recordingTool{name: "time"}
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("c1", "time", "{}"), // 模型点名一个存在但未授权的工具
		replyText("好吧"),
	}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{}, unlisted)
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{
		"agentic": true, "tools": []any{"calculator"},
	}}
	if _, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x")); err != nil {
		t.Fatalf("失败: %v", err)
	}

	// 只应把 calculator 交给模型
	offered := prov.request(t, 0).Tools
	if len(offered) != 1 || offered[0].Name != "calculator" {
		t.Fatalf("交给模型的工具 = %v，期望只有 calculator", offered)
	}

	// 核心断言：未授权的工具一次都不能被执行
	if n := unlisted.calls(); n != 0 {
		t.Errorf("未授权工具被执行了 %d 次 —— 白名单只在交给模型时过滤、没在执行时校验", n)
	}

	last := lastMessage(t, prov.request(t, 1))
	if !strings.Contains(last.Content, "不可用") {
		t.Errorf("未授权的工具应被回灌为不可用，实际 %q", last.Content)
	}
	if !strings.Contains(last.Content, "calculator") {
		t.Errorf("回灌内容应列出真正可用的工具，实际 %q", last.Content)
	}
}

// 未授权的工具若压根没注册，走的是同一条拒绝路径（不必区分，也没必要区分）。
func TestAgentNodeRejectsUnregisteredToolWithoutExecuting(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("c1", "ghost", "{}"),
		replyText("ok"),
	}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{"agentic": true}}
	if _, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x")); err != nil {
		t.Fatalf("失败: %v", err)
	}
	last := lastMessage(t, prov.request(t, 1))
	if !strings.Contains(last.Content, "ghost") {
		t.Errorf("回灌内容应提到模型请求的工具名，实际 %q", last.Content)
	}
}

// 配置里写了未注册的工具名时必须显式失败，而不是静默地"没有工具可用还硬跑"。
func TestAgentNodeFailsFastWhenNoToolIsAvailable(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{replyText("不该走到这里")}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{
		"agentic": true, "tools": []any{"does_not_exist"},
	}}
	_, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x"))
	if err == nil {
		t.Fatal("没有任何可用工具时应失败")
	}
	if !strings.Contains(err.Error(), "没有可用工具") {
		t.Errorf("错误信息应说明原因，实际 %v", err)
	}
	if prov.callCount() != 0 {
		t.Errorf("不应该白调用一次模型，实际调用 %d 次", prov.callCount())
	}
}

// ---------- 一轮多个工具调用 ----------

func TestAgentNodeHandlesMultipleToolCallsInOneRound(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTools(
			llm.ToolCall{ID: "c1", Name: "calculator", Arguments: `{"expr":"1+1"}`},
			llm.ToolCall{ID: "c2", Name: "code_runner", Arguments: `{"expr":"sum([1,2,3])"}`},
		),
		replyText("都算好了"),
	}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{}, tool.CodeRunner{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{"agentic": true}}
	if _, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x")); err != nil {
		t.Fatalf("失败: %v", err)
	}

	msgs := prov.request(t, 1).Messages
	// user + assistant + 两个 tool
	if len(msgs) != 4 {
		t.Fatalf("消息数 = %d，期望 4：%+v", len(msgs), msgs)
	}
	if len(msgs[1].ToolCalls) != 2 {
		t.Errorf("assistant 应带回 2 个 tool_calls，实际 %d", len(msgs[1].ToolCalls))
	}
	want := map[string]string{"c1": "2", "c2": "6"}
	for _, m := range msgs[2:] {
		if m.Role != llm.RoleTool {
			t.Errorf("角色 = %q，期望 tool", m.Role)
			continue
		}
		if m.Content != want[m.ToolCallID] {
			t.Errorf("tool_call_id=%q 的结果 = %q，期望 %q", m.ToolCallID, m.Content, want[m.ToolCallID])
		}
	}
}

// ---------- 默认关闭：行为与改动前完全一致 ----------

func TestAgentNodeDisabledByDefaultOffersNoTools(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{replyText("普通回答")}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	// 完全不带 Extra：应走原来的非 Agent 路径
	res, err := sched.ExecuteNode(context.Background(), agentJob(model.NodeConfig{Model: "m"}, "你好"))
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if res.Output != "普通回答" {
		t.Errorf("输出 = %q", res.Output)
	}
	if prov.callCount() != 1 {
		t.Errorf("调用次数 = %d，期望 1（不该有多轮）", prov.callCount())
	}
	if got := prov.request(t, 0).Tools; got != nil {
		t.Errorf("未开启 Agent 时不应把工具清单交给模型，实际 %v", got)
	}
	// 走的应是流式路径（与原实现一致）
	prov.mu.Lock()
	used := prov.streamUsed
	prov.mu.Unlock()
	if !used {
		t.Error("未开启 Agent 时应走原来的流式路径")
	}
}

// Extra 里有别的键但没有 agentic/tools 时，同样不能开启。
func TestAgentNodeNotEnabledByUnrelatedExtraKeys(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{replyText("ok")}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{"stream": false, "max_retry": float64(1)}}
	if _, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x")); err != nil {
		t.Fatalf("失败: %v", err)
	}
	if got := prov.request(t, 0).Tools; got != nil {
		t.Errorf("不应交出工具清单，实际 %v", got)
	}
}

// ---------- 取消 ----------

func TestAgentNodeCancellationInterruptsToolCall(t *testing.T) {
	started := make(chan struct{}, 1)
	blocking := &blockTool{started: started}
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("c1", "block", "{}"),
		replyText("不该走到第二轮"),
	}}
	sched, ts, _ := newAgentRig(t, prov, blocking)
	seedRunningTask(ts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := agentJob(model.NodeConfig{Model: "m", Extra: map[string]any{"agentic": true}}, "x")
	job.Ctx = ctx

	go func() {
		select {
		case <-started:
			cancel()
		case <-time.After(5 * time.Second):
		}
	}()

	type outcome struct {
		res worker.NodeResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := sched.ExecuteNode(ctx, job)
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		if o.err == nil {
			t.Fatal("任务取消后节点必须失败，否则'取消'只是改了个数据库状态")
		}
		if o.err.Error() != "节点已取消" {
			t.Errorf("错误 = %q，期望「节点已取消」（取消不应被当成失败）", o.err.Error())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后节点没有及时返回，工具调用没有被真正打断")
	}

	// 关键：取消后不能继续下一轮 —— 否则模型会一直要工具、一直被执行
	if prov.callCount() != 1 {
		t.Errorf("模型调用次数 = %d，期望 1（取消后不应进入下一轮）", prov.callCount())
	}
	status, _ := ts.nodeStatus(1, "agent")
	if status != model.NodeCancelled {
		t.Errorf("节点状态 = %q，期望 cancelled", status)
	}
}

// ---------- 事件 ----------

func TestAgentNodeEmitsToolCallAndResultEvents(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		replyTool("c1", "calculator", `{"expr":"1+1"}`),
		replyText("2"),
	}}
	sched, ts, hub := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	events, unsub := hub.Subscribe(1)
	defer unsub()

	if _, err := sched.ExecuteNode(context.Background(),
		agentJob(model.NodeConfig{Model: "m", Extra: map[string]any{"agentic": true}}, "x")); err != nil {
		t.Fatalf("失败: %v", err)
	}

	var calls, results []task.Event
	deadline := time.After(2 * time.Second)
	for len(calls) == 0 || len(results) == 0 {
		select {
		case ev := <-events:
			switch ev.Type {
			case task.EventToolCall:
				calls = append(calls, ev)
			case task.EventToolResult:
				results = append(results, ev)
			}
		case <-deadline:
			t.Fatalf("未收到工具调用事件：call=%d result=%d", len(calls), len(results))
		}
	}

	if calls[0].ToolName != "calculator" {
		t.Errorf("tool_call 事件缺少工具名：%+v", calls[0])
	}
	if calls[0].NodeKey != "agent" {
		t.Errorf("tool_call 事件应带节点 key：%+v", calls[0])
	}
	if !strings.Contains(calls[0].Content, "expr") {
		t.Errorf("tool_call 事件应带上模型给的参数，实际 %q", calls[0].Content)
	}
	if results[0].ToolName != "calculator" {
		t.Errorf("tool_result 事件缺少工具名：%+v", results[0])
	}
	if results[0].Content != "2" {
		t.Errorf("tool_result 事件内容 = %q，期望 2", results[0].Content)
	}
	if !strings.Contains(results[0].Message, "成功") {
		t.Errorf("tool_result 事件应说明结果，实际 %q", results[0].Message)
	}
}

// ---------- 空轮次 ----------

// 模型给了既无内容又无工具调用的空轮次：必须失败，不能静默返回空回答。
// 空回答会让下游拿到空输入继续跑，最终输出一片空白却看不到任何报错。
func TestAgentNodeEmptyRoundFails(t *testing.T) {
	prov := &scriptProvider{replies: []*llm.ChatResponse{
		{Provider: "script", Model: "test-model"}, // 空
		{Provider: "script", Model: "test-model"}, // 重试仍是空
	}}
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", MaxRetry: 1, Extra: map[string]any{"agentic": true}}
	_, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x"))
	if err == nil {
		t.Fatal("空轮次必须失败")
	}
	if !strings.Contains(err.Error(), "empty llm response") {
		t.Errorf("错误信息应指向空响应，实际 %v", err)
	}
	// 空响应要按重试策略重试，而不是立刻放弃
	if prov.callCount() != 2 {
		t.Errorf("调用次数 = %d，期望 2（1 次 + 1 次重试）", prov.callCount())
	}
}

// 模型调用彻底失败时，错误里必须带上轮次信息，便于定位是哪一轮出的问题。
func TestAgentNodeLLMFailureReportsRound(t *testing.T) {
	prov := &scriptProvider{} // 没有任何预设响应 → next() 直接报错
	sched, ts, _ := newAgentRig(t, prov, tool.Calculator{})
	seedRunningTask(ts)

	cfg := model.NodeConfig{Model: "m", Extra: map[string]any{"agentic": true}}
	_, err := sched.ExecuteNode(context.Background(), agentJob(cfg, "x"))
	if err == nil {
		t.Fatal("模型调用失败时节点应失败")
	}
	if !strings.Contains(err.Error(), "round 1") {
		t.Errorf("错误信息应带轮次，实际 %v", err)
	}
}

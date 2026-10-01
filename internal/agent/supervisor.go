package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SubTask 是 Supervisor 拆解出的子任务。
type SubTask struct {
	ID          string   `json:"id"`          // 子任务编号，如 "task_1"
	Title       string   `json:"title"`       // 子任务标题
	Description string   `json:"description"` // 子任务详细描述
	AgentName   string   `json:"agent"`       // 分配给哪个 Agent（名称或 ID）
	DependsOn   []string `json:"depends_on"`  // 依赖哪些子任务完成
}

// SubTaskResult 是子任务的执行结果。
type SubTaskResult struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Agent    string        `json:"agent"`
	Output   string        `json:"output"`
	Duration time.Duration `json:"duration_ms"`
	Error    string        `json:"error,omitempty"`
}

// SupervisorPlan 是 Supervisor 拆解任务的计划。
type SupervisorPlan struct {
	Goal     string    `json:"goal"`     // 原始目标
	SubTasks []SubTask `json:"subtasks"` // 子任务列表
}

// SupervisorAgent 实现 Multi-Agent 协作：
//
//	Supervisor (LLM)
//	     │
//	┌────┼────┐
//	│    │    │
//	Agent1 Agent2 Agent3
//	│    │    │
//	└────┼────┘
//	     │
//	Summary (LLM)
//
// 流程：
//  1. Supervisor LLM 分析用户任务，拆解为多个子任务
//  2. 为每个子任务分配合适的 Agent（从注册中心查找）
//  3. 并发执行所有子任务（按依赖关系拓扑排序）
//  4. Supervisor LLM 汇总所有子任务结果，给出最终回答
//
// 这比单个 Agent 工具调用更强大的地方：
//   - 任务并行化：多个子任务同时执行，总耗时 ≈ 最长子任务
//   - 专业分工：不同子任务用不同专长的 Agent
//   - 结果质量：Supervisor 做"质检员"，汇总时可发现并修正子任务的问题
type SupervisorAgent struct {
	// SupervisorRuntime 是负责拆解任务和汇总结果的 Agent
	Supervisor Runtime
	// Registry 用于查找子 Agent
	Registry *Registry
	// UserID 是当前用户（用于从注册中心查找可见的 Agent）
	UserID int64
	// MaxSubTasks 是最大子任务数，防止拆得太碎
	MaxSubTasks int
}

// SupervisorConfig 是 SupervisorAgent 的配置。
type SupervisorConfig struct {
	Supervisor  Runtime   // 主管 Agent，负责任务拆解与汇总
	Registry    *Registry // Agent 注册中心
	UserID      int64     // 当前用户 ID
	MaxSubTasks int       // 最大子任务数（默认 5）
}

// NewSupervisorAgent 创建一个 Multi-Agent 协作主管。
func NewSupervisorAgent(cfg SupervisorConfig) *SupervisorAgent {
	max := cfg.MaxSubTasks
	if max <= 0 {
		max = 5
	}
	return &SupervisorAgent{
		Supervisor:  cfg.Supervisor,
		Registry:    cfg.Registry,
		UserID:      cfg.UserID,
		MaxSubTasks: max,
	}
}

// Execute 执行一次 Multi-Agent 协作任务。
func (s *SupervisorAgent) Execute(ctx context.Context, input Input) (Result, error) {
	start := time.Now()
	totalInTokens := 0
	totalOutTokens := 0

	// ---- Step 1: 任务拆解 ----
	plan, err := s.decomposeTask(ctx, input)
	if err != nil {
		return Result{}, fmt.Errorf("supervisor: decompose task: %w", err)
	}
	if len(plan.SubTasks) == 0 {
		// 拆解不出子任务 → 直接让 supervisor 回答
		return s.Supervisor.Execute(ctx, input)
	}

	// ---- Step 2: 为子任务分配 Agent ----
	agentMap, err := s.assignAgents(ctx, plan.SubTasks)
	if err != nil {
		return Result{}, fmt.Errorf("supervisor: assign agents: %w", err)
	}

	// ---- Step 3: 并发执行子任务 ----
	results := s.executeSubTasks(ctx, plan.SubTasks, agentMap, input)

	// ---- Step 4: 汇总结果 ----
	summary, sumTokensIn, sumTokensOut, err := s.summarizeResults(ctx, input, plan, results)
	if err != nil {
		return Result{}, fmt.Errorf("supervisor: summarize: %w", err)
	}
	totalInTokens += sumTokensIn
	totalOutTokens += sumTokensOut

	// 统计成功/失败的子任务
	toolCalls := make([]ToolCallRecord, 0, len(results))
	for _, r := range results {
		toolCalls = append(toolCalls, ToolCallRecord{
			Name:     r.Agent,
			Args:     r.Title,
			Result:   r.Output,
			Duration: r.Duration,
			Error:    r.Error,
		})
	}

	return Result{
		Output:       summary,
		Rounds:       1, // Supervisor 算一轮（内部子任务各自有轮数）
		ToolCalls:    toolCalls,
		InputTokens:  totalInTokens,
		OutputTokens: totalOutTokens,
		Provider:     "supervisor",
		Model:        s.Supervisor.Name(),
		Duration:     time.Since(start),
	}, nil
}

// decomposeTask 让 Supervisor LLM 把大任务拆成多个子任务。
func (s *SupervisorAgent) decomposeTask(ctx context.Context, input Input) (SupervisorPlan, error) {
	// 构建拆解提示词
	system := `你是一个任务拆解专家。请将用户的复杂任务拆解为若干个子任务，每个子任务由专门的 Agent 执行。

要求：
1. 子任务数量控制在 2 到 ` + fmt.Sprintf("%d", s.MaxSubTasks) + ` 个之间
2. 每个子任务要有清晰的 id、title、description
3. agent 字段填写最合适的 Agent 类型（如 "research"、"code"、"writer"、"analyst"）
4. depends_on 字段标明子任务之间的依赖关系（没有依赖留空数组）
5. 只输出 JSON，不要有其他文字

JSON 格式：
{
  "goal": "原始目标",
  "subtasks": [
    {
      "id": "task_1",
      "title": "子任务标题",
      "description": "详细描述",
      "agent": "agent类型",
      "depends_on": []
    }
  ]
}`

	result, err := s.Supervisor.Execute(ctx, Input{
		Prompt:      input.Prompt,
		System:      system,
		MaxRounds:   1,
		Model:       input.Model,
		Metadata:    input.Metadata,
	})
	if err != nil {
		return SupervisorPlan{}, err
	}

	// 解析 JSON
	plan, err := parseSupervisorPlan(result.Output)
	if err != nil {
		// 解析失败，回退到一个子任务（让 supervisor 自己干）
		return SupervisorPlan{
			Goal: input.Prompt,
			SubTasks: []SubTask{{
				ID: "task_1", Title: "完整任务", Description: input.Prompt,
				AgentName: "default", DependsOn: nil,
			}},
		}, nil
	}
	return plan, nil
}

// assignAgents 为每个子任务从注册中心找最合适的 Agent。
// 优先按名称精确匹配，找不到则按能力匹配，再不行用第一个可用的。
func (s *SupervisorAgent) assignAgents(ctx context.Context, subtasks []SubTask) (map[string]Runtime, error) {
	agentMap := make(map[string]Runtime)

	if s.Registry == nil {
		// 没有注册中心，全部用 supervisor 自己
		for _, st := range subtasks {
			agentMap[st.ID] = s.Supervisor
		}
		return agentMap, nil
	}

	// 列出所有可用 Agent 一次，避免重复查库
	allAgents, err := s.Registry.List(ctx, s.UserID, "", "")
	if err != nil {
		// 查不到就都用 supervisor
		for _, st := range subtasks {
			agentMap[st.ID] = s.Supervisor
		}
		return agentMap, nil
	}
	if len(allAgents) == 0 {
		for _, st := range subtasks {
			agentMap[st.ID] = s.Supervisor
		}
		return agentMap, nil
	}

	for _, st := range subtasks {
		// 先按名称精确匹配
		if a, err := s.Registry.GetByName(ctx, s.UserID, st.AgentName); err == nil {
			if rt, err := s.Registry.GetRuntime(ctx, a.ID); err == nil {
				agentMap[st.ID] = rt
				continue
			}
		}

		// 再按能力匹配（根据 agent 名称推断能力）
		cap := inferCapability(st.AgentName)
		if cap != "" {
			if matches, err := s.Registry.FindByCapability(ctx, s.UserID, cap); err == nil && len(matches) > 0 {
				if rt, err := s.Registry.GetRuntime(ctx, matches[0].ID); err == nil {
					agentMap[st.ID] = rt
					continue
				}
			}
		}

		// 兜底：用第一个可用的 Agent
		if rt, err := s.Registry.GetRuntime(ctx, allAgents[0].ID); err == nil {
			agentMap[st.ID] = rt
		} else {
			agentMap[st.ID] = s.Supervisor
		}
	}

	return agentMap, nil
}

// executeSubTasks 并发执行所有子任务（暂不处理依赖，全部并发执行）。
// 简单起见，先实现全并发；依赖关系可以后续用 Kahn 算法做调度。
func (s *SupervisorAgent) executeSubTasks(ctx context.Context, subtasks []SubTask, agentMap map[string]Runtime, input Input) []SubTaskResult {
	results := make([]SubTaskResult, len(subtasks))
	var wg sync.WaitGroup

	for i, st := range subtasks {
		wg.Add(1)
		go func(idx int, task SubTask) {
			defer wg.Done()
			start := time.Now()

			rt, ok := agentMap[task.ID]
			if !ok {
				results[idx] = SubTaskResult{
					ID: task.ID, Title: task.Title, Agent: "unknown",
					Error: "no agent assigned", Duration: time.Since(start),
				}
				return
			}

			subInput := Input{
				Prompt:    task.Description,
				System:    input.System,
				MaxRounds: input.MaxRounds,
				Model:     input.Model,
				Metadata:  input.Metadata,
			}

			result, err := rt.Execute(ctx, subInput)
			duration := time.Since(start)
			if err != nil {
				results[idx] = SubTaskResult{
					ID: task.ID, Title: task.Title, Agent: rt.Name(),
					Error: err.Error(), Duration: duration,
				}
				return
			}
			results[idx] = SubTaskResult{
				ID: task.ID, Title: task.Title, Agent: rt.Name(),
				Output: result.Output, Duration: duration,
			}
		}(i, st)
	}

	wg.Wait()
	return results
}

// summarizeResults 让 Supervisor LLM 汇总所有子任务的结果，给出最终回答。
func (s *SupervisorAgent) summarizeResults(ctx context.Context, input Input, plan SupervisorPlan, results []SubTaskResult) (string, int, int, error) {
	// 构建汇总提示词
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("原始任务：%s\n\n", plan.Goal))
	sb.WriteString("以下是各子任务的执行结果：\n\n")
	for i, r := range results {
		fmt.Fprintf(&sb, "【子任务 %d】%s（Agent: %s）\n", i+1, r.Title, r.Agent)
		if r.Error != "" {
			fmt.Fprintf(&sb, "状态：失败 - %s\n\n", r.Error)
		} else {
			fmt.Fprintf(&sb, "结果：\n%s\n\n", r.Output)
		}
	}
	sb.WriteString("请综合以上子任务的结果，给出一个完整、连贯的最终答案。")

	result, err := s.Supervisor.Execute(ctx, Input{
		Prompt:    sb.String(),
		System:    "你是一个严谨的编辑和质检员。请综合各子任务的结果，整合成一份高质量的最终答案。注意发现子任务之间的矛盾并修正。",
		MaxRounds: 1,
		Model:     input.Model,
		Metadata:  input.Metadata,
	})
	if err != nil {
		// 汇总失败，直接拼接结果返回
		var fallback strings.Builder
		for _, r := range results {
			fmt.Fprintf(&fallback, "## %s\n\n%s\n\n", r.Title, r.Output)
		}
		return fallback.String(), 0, 0, nil
	}
	return result.Output, result.InputTokens, result.OutputTokens, nil
}

// parseSupervisorPlan 从 LLM 输出中解析任务拆解计划。
func parseSupervisorPlan(output string) (SupervisorPlan, error) {
	// 尝试提取 JSON 部分（LLM 可能在 JSON 前后加了文字）
	jsonStr := extractJSON(output)
	if jsonStr == "" {
		return SupervisorPlan{}, fmt.Errorf("no JSON found in output")
	}

	var plan SupervisorPlan
	if err := json.Unmarshal([]byte(jsonStr), &plan); err != nil {
		return SupervisorPlan{}, fmt.Errorf("parse plan JSON: %w", err)
	}
	return plan, nil
}

// extractJSON 从文本中提取 JSON 对象（找最外层的 {}）。
func extractJSON(text string) string {
	start := strings.Index(text, "{")
	if start == -1 {
		return ""
	}
	depth := 0
	end := -1
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
	}
	if end == -1 {
		return ""
	}
	return text[start:end]
}

// inferCapability 根据 Agent 名称推断其核心能力。
func inferCapability(agentName string) string {
	name := strings.ToLower(agentName)
	switch {
	case strings.Contains(name, "search") || strings.Contains(name, "research"):
		return string(CapSearch)
	case strings.Contains(name, "code") || strings.Contains(name, "dev"):
		return string(CapCode)
	case strings.Contains(name, "rag") || strings.Contains(name, "knowledge"):
		return string(CapRAG)
	case strings.Contains(name, "tool"):
		return string(CapToolCall)
	case strings.Contains(name, "image") || strings.Contains(name, "vision") || strings.Contains(name, "multi"):
		return string(CapMultimodal)
	default:
		return ""
	}
}

// 确保 SupervisorAgent 也满足 Runtime 接口（可以被注册到 Registry 里）
var _ Runtime = (*SupervisorAgent)(nil)

func (s *SupervisorAgent) Name() string        { return "supervisor-agent" }
func (s *SupervisorAgent) Version() string     { return "1.0.0" }
func (s *SupervisorAgent) Description() string { return "Multi-Agent 协作主管，负责任务拆解、Agent 分配与结果汇总" }
func (s *SupervisorAgent) Capabilities() []Capability {
	return []Capability{CapToolCall, CapRAG, CapCode, CapSearch}
}

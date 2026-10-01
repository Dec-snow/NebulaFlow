package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ---------- 路由策略接口 ----------

// RouteRequest 是动态路由的输入：描述"我要什么样的 Agent"。
//
// 路由策略根据这些条件从注册中心找最合适的 Agent：
//   - AgentID：直接指定（最高优先级，跳过策略匹配）
//   - AgentName：按名称精确匹配
//   - Capabilities：必须具备的能力列表（AND 关系，全部满足才行）
//   - Tags：标签过滤（OR 关系，匹配任一标签即可）
//   - RuntimeType：Runtime 类型过滤（native / langchain / http）
//   - PreferHealthy：是否优先选择健康的 Agent（默认 true）
type RouteRequest struct {
	AgentID      int64
	AgentName    string
	Capabilities []string
	Tags         []string
	RuntimeType  model.AgentRuntimeType
	PreferHealthy bool
}

// RouteResult 是路由结果。
type RouteResult struct {
	Agent   *model.AgentRegistry
	Runtime Runtime
	Reason  string // 为什么选了这个 Agent（用于日志和审计）
}

// Router 是 Agent 动态路由策略接口。
//
// 设计类似 Kubernetes Service 的负载均衡器：
// 工作流节点不绑定具体 Agent，而是描述"我要什么能力的 Agent"，
// Router 从 Registry 里找最合适的返回。
//
// 不同的 Router 实现可以用不同的策略：
//   - SmartRouter：综合策略（名称 → 能力 → 标签 → 健康度 → 兜底）
//   - RandomRouter：随机（负载均衡场景）
//   - RoundRobinRouter：轮询（简单的负载均衡）
//   - LeastLoadRouter：最低负载（需要监控指标支撑，未来扩展）
type Router interface {
	// Route 根据路由请求找到最合适的 Agent Runtime。
	// 找不到时返回 fallback（兜底 Runtime），不返回 error（避免中断工作流）。
	Route(ctx context.Context, req RouteRequest) (RouteResult, error)
	// Name 返回策略名称（用于日志和 API 展示）。
	Name() string
}

// ---------- SmartRouter（默认策略） ----------

// SmartRouter 是默认的动态路由策略。
//
// 路由优先级（从高到低）：
//  1. AgentID 精确指定 → 直接返回（如果健康）
//  2. AgentName 精确匹配 → 返回匹配的（如果健康）
//  3. 能力 + 标签组合匹配 → 从候选中选健康的
//  4. 兜底：返回第一个可用的 Agent
//
// 健康检查策略：
//   - PreferHealthy=true（默认）：优先返回健康的，但如果没有健康的，
//     仍然返回一个不健康的（让调用方决定是否使用，而不是直接失败）
//   - PreferHealthy=false：不做健康检查，直接返回第一个匹配的
type SmartRouter struct {
	registry *Registry
	logger   *slog.Logger
}

// NewSmartRouter 创建默认路由策略。
func NewSmartRouter(registry *Registry, logger *slog.Logger) *SmartRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &SmartRouter{registry: registry, logger: logger}
}

func (r *SmartRouter) Name() string { return "smart" }

// Route 执行动态路由。
func (r *SmartRouter) Route(ctx context.Context, req RouteRequest) (RouteResult, error) {
	// 1. AgentID 精确指定（最高优先级）
	if req.AgentID > 0 {
		a, err := r.registry.Get(ctx, req.AgentID)
		if err != nil {
			return RouteResult{Reason: fmt.Sprintf("agent_id=%d not found: %v", req.AgentID, err)}, err
		}
		if a.Status != model.AgentActive {
			return RouteResult{Reason: fmt.Sprintf("agent_id=%d status=%s", req.AgentID, a.Status)},
				fmt.Errorf("agent %d is not active", req.AgentID)
		}
		rt, err := r.registry.GetRuntime(ctx, a.ID)
		if err != nil {
			return RouteResult{Agent: a, Reason: fmt.Sprintf("agent_id=%d runtime error: %v", a.ID, err)}, err
		}
		return RouteResult{Agent: a, Runtime: rt, Reason: fmt.Sprintf("matched by agent_id=%d", a.ID)}, nil
	}

	// 2. AgentName 精确匹配
	if req.AgentName != "" {
		a, err := r.registry.GetByName(ctx, r.registryUserID(), req.AgentName)
		if err == nil && a.Status == model.AgentActive {
			rt, err := r.registry.GetRuntime(ctx, a.ID)
			if err == nil {
				return RouteResult{Agent: a, Runtime: rt, Reason: fmt.Sprintf("matched by name=%q", req.AgentName)}, nil
			}
		}
		r.logger.Debug("router: name match failed, falling through",
			"name", req.AgentName, "error", err)
	}

	// 3. 能力 + 标签组合匹配
	candidates := r.findCandidates(ctx, req)
	if len(candidates) > 0 {
		// 健康优先选择：只检查前 3 个候选，避免延迟过高
		best := r.selectHealthy(ctx, candidates)
		if best != nil {
			rt, err := r.registry.GetRuntime(ctx, best.ID)
			if err == nil {
				return RouteResult{Agent: best, Runtime: rt, Reason: r.matchReason(req, best)}, nil
			}
		}
		// 没有健康的，但仍然返回第一个（让调用方决定）
		best = candidates[0]
		rt, err := r.registry.GetRuntime(ctx, best.ID)
		if err == nil {
			return RouteResult{Agent: best, Runtime: rt, Reason: r.matchReason(req, best) + " (unhealthy)"}, nil
		}
	}

	// 4. 兜底：返回第一个可用的 Agent
	allAgents, err := r.registry.List(ctx, r.registryUserID(), "", "")
	if err == nil && len(allAgents) > 0 {
		a := &allAgents[0]
		if a.Status == model.AgentActive {
			rt, err := r.registry.GetRuntime(ctx, a.ID)
			if err == nil {
				return RouteResult{Agent: a, Runtime: rt, Reason: "fallback: first available agent"}, nil
			}
		}
	}

	return RouteResult{Reason: "no agent available"}, fmt.Errorf("router: no agent available for request %+v", req)
}

// findCandidates 根据能力和标签从注册中心查找候选 Agent。
func (r *SmartRouter) findCandidates(ctx context.Context, req RouteRequest) []*model.AgentRegistry {
	var candidates []*model.AgentRegistry

	// 按能力过滤（如果有能力要求，用第一个能力做粗筛，再在内存里做 AND）
	if len(req.Capabilities) > 0 {
		// Registry.List 支持 capability 过滤（只支持一个）
		primaryCap := req.Capabilities[0]
		agents, err := r.registry.List(ctx, r.registryUserID(), req.RuntimeType, primaryCap)
		if err != nil || len(agents) == 0 {
			return nil
		}
		// 在内存里做 AND 过滤（如果有多能力要求）
		for i := range agents {
			if r.hasAllCapabilities(&agents[i], req.Capabilities) {
				candidates = append(candidates, &agents[i])
			}
		}
	} else {
		// 没有能力要求，列出全部
		agents, err := r.registry.List(ctx, r.registryUserID(), req.RuntimeType, "")
		if err != nil || len(agents) == 0 {
			return nil
		}
		for i := range agents {
			candidates = append(candidates, &agents[i])
		}
	}

	// 标签过滤（OR 关系）
	if len(req.Tags) > 0 {
		filtered := make([]*model.AgentRegistry, 0, len(candidates))
		for _, a := range candidates {
			if r.hasAnyTag(a, req.Tags) {
				filtered = append(filtered, a)
			}
		}
		candidates = filtered
	}

	// 只保留活跃的
	active := make([]*model.AgentRegistry, 0, len(candidates))
	for _, a := range candidates {
		if a.Status == model.AgentActive {
			active = append(active, a)
		}
	}

	return active
}

// selectHealthy 从候选中选择第一个健康的 Agent。
//
// 健康检查可能比较慢（远程 Runtime 要发 HTTP 请求），
// 所以只检查前 3 个候选，避免延迟过高。
func (r *SmartRouter) selectHealthy(ctx context.Context, candidates []*model.AgentRegistry) *model.AgentRegistry {
	maxCheck := 3
	if len(candidates) < maxCheck {
		maxCheck = len(candidates)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for i := 0; i < maxCheck; i++ {
		a := candidates[i]
		rt, err := r.registry.GetRuntime(checkCtx, a.ID)
		if err != nil {
			continue
		}
		if rt.HealthCheck(checkCtx) == nil {
			return a
		}
	}
	return nil
}

// hasAllCapabilities 检查 Agent 是否具备所有要求的能力。
func (r *SmartRouter) hasAllCapabilities(a *model.AgentRegistry, caps []string) bool {
	capMap := make(map[string]bool, len(a.Capabilities))
	for _, c := range a.Capabilities {
		capMap[c] = true
	}
	for _, c := range caps {
		if !capMap[c] {
			return false
		}
	}
	return true
}

// hasAnyTag 检查 Agent 是否包含任一标签（OR 关系）。
func (r *SmartRouter) hasAnyTag(a *model.AgentRegistry, tags []string) bool {
	// AgentRegistry 没有 Tags 字段，用 Capabilities 近似（标签和能力在当前模型里是同构的）
	// 未来 AgentRegistry 可以加 Tags 字段，这个函数不用改
	capMap := make(map[string]bool, len(a.Capabilities))
	for _, c := range a.Capabilities {
		capMap[c] = true
	}
	for _, t := range tags {
		if capMap[t] {
			return true
		}
	}
	return false
}

// matchReason 生成人类可读的匹配原因。
func (r *SmartRouter) matchReason(req RouteRequest, a *model.AgentRegistry) string {
	parts := []string{}
	if len(req.Capabilities) > 0 {
		parts = append(parts, "capabilities="+strings.Join(req.Capabilities, ","))
	}
	if len(req.Tags) > 0 {
		parts = append(parts, "tags="+strings.Join(req.Tags, ","))
	}
	if string(req.RuntimeType) != "" {
		parts = append(parts, "runtime_type="+string(req.RuntimeType))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("matched agent %s (id=%d)", a.Name, a.ID)
	}
	return fmt.Sprintf("matched agent %s (id=%d) by %s", a.Name, a.ID, strings.Join(parts, ", "))
}

// registryUserID 返回 Registry 操作用的 user_id。
// SmartRouter 不绑定具体用户，用 0 表示系统级（能看到所有用户 + 系统内置的 Agent）。
func (r *SmartRouter) registryUserID() int64 { return 0 }

// ---------- RoundRobinRouter（轮询策略） ----------

// RoundRobinRouter 是轮询路由策略，用于简单的负载均衡。
//
// 适用场景：多个同类 Agent 实例之间均摊请求，
// 避免单个 Agent 被打满。
type RoundRobinRouter struct {
	registry *Registry
	counter  int64
	logger   *slog.Logger
}

// NewRoundRobinRouter 创建轮询路由策略。
func NewRoundRobinRouter(registry *Registry, logger *slog.Logger) *RoundRobinRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &RoundRobinRouter{registry: registry, logger: logger}
}

func (r *RoundRobinRouter) Name() string { return "round_robin" }

func (r *RoundRobinRouter) Route(ctx context.Context, req RouteRequest) (RouteResult, error) {
	// AgentID 精确指定优先
	if req.AgentID > 0 {
		a, err := r.registry.Get(ctx, req.AgentID)
		if err != nil {
			return RouteResult{}, err
		}
		rt, err := r.registry.GetRuntime(ctx, a.ID)
		if err != nil {
			return RouteResult{}, err
		}
		return RouteResult{Agent: a, Runtime: rt, Reason: "round_robin: by agent_id"}, nil
	}

	// 按能力过滤候选
	primaryCap := ""
	if len(req.Capabilities) > 0 {
		primaryCap = req.Capabilities[0]
	}
	agents, err := r.registry.List(ctx, 0, req.RuntimeType, primaryCap)
	if err != nil || len(agents) == 0 {
		return RouteResult{Reason: "no candidates"}, fmt.Errorf("round_robin: no candidates")
	}

	// 轮询选择
	idx := int(r.counter) % len(agents)
	r.counter++

	a := agents[idx]
	if a.Status != model.AgentActive {
		// 跳过非活跃的
		for i := 1; i < len(agents); i++ {
			a = agents[(int(r.counter)+i)%len(agents)]
			if a.Status == model.AgentActive {
				break
			}
		}
	}

	rt, err := r.registry.GetRuntime(ctx, a.ID)
	if err != nil {
		return RouteResult{Agent: &a}, err
	}
	return RouteResult{Agent: &a, Runtime: rt, Reason: fmt.Sprintf("round_robin: index=%d/%d", idx, len(agents))}, nil
}

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/cache"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// CachedWorkflows 给工作流读取加一层缓存。
//
// 为什么这条路径值得缓存：调度器每执行一个任务就要读一次工作流
// （scheduler.executeTask → workflows.GetWorkflow），而 WorkflowRepo.GetWorkflow
// 是 3 次查询——工作流行 + 节点 + 边。P0-0 把任务吞吐提到 464.9 tasks/s 之后，
// 这条路径等于每秒约 1400 次查询，读的却是同一份几乎不变的数据。
//
// 为什么做成 WorkflowStore 的装饰器，而不是只包住 GetWorkflow：
// 缓存最怕的是「某条写路径绕过了失效逻辑」。装饰器把读写收进同一个对象，
// 装配处一次替换 `st.Workflows` 就覆盖了全部调用点（API 与调度器共用同一个 *Store），
// 将来新增调用方也不会漏掉失效——因为它拿到的就是这个装饰器。
//
// ⚠ 已知边界：缓存是**进程内**的。多实例部署时，实例 A 更新了工作流，
// 实例 B 的缓存不会被失效，要等 TTL 过期才一致。因此 TTL 是必须的兜底，
// 不能只依赖主动失效。要跨实例强一致得把失效消息发到 Redis Pub/Sub，
// 那是下一步的事（当前单实例部署，TTL 足够）。
type CachedWorkflows struct {
	next   WorkflowStore
	cache  cache.Cache
	ttl    time.Duration
	logger *slog.Logger

	hits   atomic.Int64
	misses atomic.Int64
	errs   atomic.Int64
}

// NewCachedWorkflows 包装一个 WorkflowStore。
// c 为 nil 或 ttl <= 0 时退化为直通（不缓存），这样装配处可以无条件包一层，
// 不必在 main 里写「有没有缓存」的分支。
func NewCachedWorkflows(next WorkflowStore, c cache.Cache, ttl time.Duration, logger *slog.Logger) *CachedWorkflows {
	if logger == nil {
		logger = slog.Default()
	}
	return &CachedWorkflows{next: next, cache: c, ttl: ttl, logger: logger}
}

// enabled 报告缓存是否真的生效。
func (c *CachedWorkflows) enabled() bool {
	return c.cache != nil && c.ttl > 0
}

// workflowCacheKey 构造缓存键。
//
// 键**必须**包含 userID。GetWorkflow(id, userID) 在 userID 与 owner 不匹配时
// 返回 ErrWorkflowNotFound，这是归属校验。如果键只用 id，用户 B 请求用户 A 的
// 工作流时就会命中用户 A 那次请求写下的缓存，**直接拿到别人的工作流**——
// 缓存绝不能变成绕过鉴权的后门。这个错误非常隐蔽：单用户测试永远不会发现。
func workflowCacheKey(userID, id int64) string {
	return fmt.Sprintf("nebulaflow:wf:%d:%d", userID, id)
}

// GetWorkflow 先查缓存，未命中回源。
//
// 三条「缓存不该让请求失败」的规则，都在这里：
//  1. 读缓存出错（Redis 抖动）→ 当作未命中回源，不把错误抛给调用方；
//  2. 值反序列化失败 → 当作未命中，并顺手删掉这个键（可能是旧版本结构体写的）；
//  3. 写缓存失败 → 忽略，本次结果照常返回。
//
// 缓存是优化，不是依赖。它挂掉时系统应该变慢，而不是变坏。
func (c *CachedWorkflows) GetWorkflow(ctx context.Context, id, userID int64) (*model.Workflow, error) {
	if !c.enabled() {
		return c.next.GetWorkflow(ctx, id, userID)
	}
	key := workflowCacheKey(userID, id)

	if raw, ok, err := c.cache.Get(ctx, key); err == nil && ok {
		var wf model.Workflow
		if json.Unmarshal([]byte(raw), &wf) == nil {
			c.hits.Add(1)
			return &wf, nil
		}
		// 解析不了就丢弃这个键，让本次请求回源，而不是把一个坏值反复喂给上层
		c.errs.Add(1)
		_ = c.cache.Del(ctx, key)
	}

	c.misses.Add(1)
	wf, err := c.next.GetWorkflow(ctx, id, userID)
	if err != nil {
		// 不缓存失败。理由：ErrWorkflowNotFound 只在「工作流被删」或「userID 不匹配」
		// 时出现，前者回源代价很低，后者缓存下来等于把一次越权探测的结果固化。
		// 负缓存的收益（挡住不存在的 ID 风暴）在这条路径上不存在——这里读的都是
		// 任务自己引用的 workflow_id，本来就应该存在。
		return nil, err
	}

	if b, err := json.Marshal(wf); err == nil {
		if err := c.cache.Set(ctx, key, string(b), c.ttl); err != nil {
			c.errs.Add(1)
		}
	}
	return wf, nil
}

// Invalidate 主动失效某个工作流的缓存。
// 导出是为了给运维/测试一个强制驱逐的入口（例如手工改库之后）。
func (c *CachedWorkflows) Invalidate(ctx context.Context, userID, id int64) {
	if !c.enabled() {
		return
	}
	if err := c.cache.Del(ctx, workflowCacheKey(userID, id)); err != nil {
		// 删不掉只能靠 TTL 兜底。这里必须留日志：
		// 缓存删不掉是"数据看起来没更新"这类灵异问题的唯一线索。
		c.logger.Warn("invalidate workflow cache failed",
			"workflow_id", id, "user_id", userID, "error", err)
		c.errs.Add(1)
	}
}

// ---------- 写路径：先落库，再失效 ----------

// 顺序是「先写库、后失效」，不能反过来。
// 反过来的话，在「失效完成」与「写库提交」之间有一个窗口：
// 并发读会把**旧的**值重新灌进缓存，而写库随后才生效——缓存里留下一个
// 谁都不会再改的陈旧值，直到 TTL 过期。先写库后失效则最坏情况只是
// 缓存多活一个 TTL 周期（因为下一次读回源拿到的已经是最新值）。

func (c *CachedWorkflows) CreateWorkflow(ctx context.Context, wf *model.Workflow) error {
	if err := c.next.CreateWorkflow(ctx, wf); err != nil {
		return err
	}
	// 新建的 ID 此前不可能被缓存过，这次失效是防御性的：
	// ID 复用（清库重放、序列回退）时它能挡住一个陈旧值。
	c.Invalidate(ctx, wf.UserID, wf.ID)
	return nil
}

func (c *CachedWorkflows) UpdateWorkflow(ctx context.Context, wf *model.Workflow) error {
	if err := c.next.UpdateWorkflow(ctx, wf); err != nil {
		return err
	}
	// 用 wf.UserID 而不是查库拿 owner：调用方（API 层）是从
	// GetWorkflow(id, u.ID) 拿到的对象，UserID 必然已填充。
	c.Invalidate(ctx, wf.UserID, wf.ID)
	return nil
}

func (c *CachedWorkflows) DeleteWorkflow(ctx context.Context, id, userID int64) error {
	if err := c.next.DeleteWorkflow(ctx, id, userID); err != nil {
		return err
	}
	c.Invalidate(ctx, userID, id)
	return nil
}

// ---------- 直通：不做缓存的方法 ----------

// ListWorkflows 刻意不缓存。
// 它是前端列表页的调用（低频），而失效成本很高——任何一次创建/更新/删除
// 都会让列表变脏，得额外维护一个按用户维度的版本号才能正确失效。
// 缓存要花在真正热的路径上（GetWorkflow 是每个任务执行一次），
// 把低频调用也缓存起来只会增加失效面，不增加收益。
func (c *CachedWorkflows) ListWorkflows(ctx context.Context, userID int64) ([]model.Workflow, error) {
	return c.next.ListWorkflows(ctx, userID)
}

// GetWorkflowNodes / GetWorkflowEdges 是 GetWorkflow 的内部组成部分，
// 外部调用方为零（已核对全仓库）。直通即可——单独缓存它们
// 反而会和 GetWorkflow 的缓存产生两份可能不一致的数据。
func (c *CachedWorkflows) GetWorkflowNodes(ctx context.Context, workflowID int64) ([]model.WorkflowNode, error) {
	return c.next.GetWorkflowNodes(ctx, workflowID)
}

func (c *CachedWorkflows) GetWorkflowEdges(ctx context.Context, workflowID int64) ([]model.WorkflowEdge, error) {
	return c.next.GetWorkflowEdges(ctx, workflowID)
}

// CacheStats 是缓存命中情况快照，用于上报指标。
// 命中率长期为 0 说明缓存没接上（键不匹配、TTL 太短、或写入一直失败）。
type CacheStats struct {
	Hits   int64
	Misses int64
	Errors int64
}

func (c *CachedWorkflows) CacheStats() CacheStats {
	return CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load(), Errors: c.errs.Load()}
}

// 编译期确认装饰器满足 WorkflowStore。
var _ WorkflowStore = (*CachedWorkflows)(nil)

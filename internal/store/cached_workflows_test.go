package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/cache"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ---------- 测试替身 ----------

// fakeWorkflows 是一个记账用的 WorkflowStore。
// 它刻意复刻真实仓储的**归属校验**：userID 与 owner 不符时返回 ErrWorkflowNotFound。
// 缓存用例必须建在这条语义之上，否则「缓存绕过鉴权」这个缺陷根本不会被触发。
type fakeWorkflows struct {
	mu        sync.Mutex
	workflows map[int64]*model.Workflow
	getCalls  int
	nextID    int64
}

func newFakeWorkflows() *fakeWorkflows {
	return &fakeWorkflows{workflows: map[int64]*model.Workflow{}, nextID: 1}
}

func (f *fakeWorkflows) seed(id, userID int64, name string) *model.Workflow {
	f.mu.Lock()
	defer f.mu.Unlock()
	wf := &model.Workflow{ID: id, UserID: userID, Name: name, Status: "published"}
	f.workflows[id] = wf
	if id >= f.nextID {
		f.nextID = id + 1
	}
	return wf
}

func (f *fakeWorkflows) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

func (f *fakeWorkflows) CreateWorkflow(_ context.Context, wf *model.Workflow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	wf.ID = f.nextID
	f.nextID++
	f.workflows[wf.ID] = wf
	return nil
}

func (f *fakeWorkflows) UpdateWorkflow(_ context.Context, wf *model.Workflow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workflows[wf.ID] = wf
	return nil
}

func (f *fakeWorkflows) DeleteWorkflow(_ context.Context, id, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if wf, ok := f.workflows[id]; ok && wf.UserID == userID {
		delete(f.workflows, id)
	}
	return nil
}

func (f *fakeWorkflows) GetWorkflow(_ context.Context, id, userID int64) (*model.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	wf, ok := f.workflows[id]
	if !ok || wf.UserID != userID {
		return nil, ErrWorkflowNotFound
	}
	cp := *wf
	return &cp, nil
}

func (f *fakeWorkflows) ListWorkflows(_ context.Context, userID int64) ([]model.Workflow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Workflow
	for _, wf := range f.workflows {
		if wf.UserID == userID {
			out = append(out, *wf)
		}
	}
	return out, nil
}

func (f *fakeWorkflows) GetWorkflowNodes(context.Context, int64) ([]model.WorkflowNode, error) {
	return nil, nil
}

func (f *fakeWorkflows) GetWorkflowEdges(context.Context, int64) ([]model.WorkflowEdge, error) {
	return nil, nil
}

func newCached(next WorkflowStore, ttl time.Duration) *CachedWorkflows {
	return NewCachedWorkflows(next, cache.NewMemoryCache(), ttl, nil)
}

// ---------- 基本行为 ----------

// 同一用户重复读取同一工作流：第二次必须命中缓存，不再回源。
func TestCachedWorkflowsHitsOnSecondRead(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "简历分析")
	c := newCached(f, time.Minute)

	for i := 0; i < 3; i++ {
		wf, err := c.GetWorkflow(ctx, 1, 100)
		if err != nil {
			t.Fatalf("第 %d 次读取失败: %v", i+1, err)
		}
		if wf.Name != "简历分析" {
			t.Fatalf("第 %d 次读到的名字不对: %s", i+1, wf.Name)
		}
	}

	if got := f.calls(); got != 1 {
		t.Fatalf("回源次数应为 1（后两次命中缓存），实际 %d", got)
	}
	st := c.CacheStats()
	if st.Hits != 2 || st.Misses != 1 {
		t.Fatalf("期望 1 次未命中 + 2 次命中，实际 hits=%d misses=%d", st.Hits, st.Misses)
	}
}

// 【安全用例】缓存绝不能成为绕过归属校验的后门。
//
// GetWorkflow(id, userID) 在 userID 与 owner 不符时返回 ErrWorkflowNotFound，
// 这是归属校验。如果缓存键只用 id，那么用户 200 请求工作流 1 时会命中
// 用户 100 那次请求写下的缓存，**直接拿到别人的工作流**。
//
// 这个缺陷在单用户测试里永远不会暴露——必须先让 A 读一次把缓存喂热，
// 再用 B 去读同一个 ID。
func TestCachedWorkflowsDoesNotLeakAcrossUsers(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "用户100的工作流")
	c := newCached(f, time.Minute)

	// 用户 100 读一次，把缓存喂热
	if _, err := c.GetWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("用户 100 读取失败: %v", err)
	}
	if f.calls() != 1 {
		t.Fatalf("首次读取应回源 1 次，实际 %d", f.calls())
	}

	// 用户 200 请求同一个 workflow id：必须拿不到，而且必须回源校验
	wf, err := c.GetWorkflow(ctx, 1, 200)
	if !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("用户 200 读用户 100 的工作流应返回 ErrWorkflowNotFound，实际 err=%v wf=%+v", err, wf)
	}
	if wf != nil {
		t.Fatalf("越权读取不得返回任何工作流内容，实际返回 %+v", wf)
	}
	if f.calls() != 2 {
		t.Fatalf("用户 200 的请求必须回源做归属校验（不能命中缓存），回源次数应为 2，实际 %d", f.calls())
	}
}

// 更新之后必须读到新内容——失效逻辑写错的话这里会读到旧名字。
func TestCachedWorkflowsInvalidatesOnUpdate(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "旧名字")
	c := newCached(f, time.Hour) // TTL 故意设很长：证明读到新值靠的是失效而不是过期

	if _, err := c.GetWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("预热读取失败: %v", err)
	}

	updated := &model.Workflow{ID: 1, UserID: 100, Name: "新名字", Status: "published"}
	if err := c.UpdateWorkflow(ctx, updated); err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}

	wf, err := c.GetWorkflow(ctx, 1, 100)
	if err != nil {
		t.Fatalf("更新后读取失败: %v", err)
	}
	if wf.Name != "新名字" {
		t.Fatalf("更新后应读到「新名字」，实际读到「%s」——缓存没有被失效", wf.Name)
	}
}

// 删除之后不能再返回已删除的工作流（否则任务会跑在一个不存在的 DAG 上）。
func TestCachedWorkflowsInvalidatesOnDelete(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "待删除")
	c := newCached(f, time.Hour)

	if _, err := c.GetWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("预热读取失败: %v", err)
	}
	if err := c.DeleteWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("DeleteWorkflow: %v", err)
	}

	if wf, err := c.GetWorkflow(ctx, 1, 100); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("删除后应返回 ErrWorkflowNotFound，实际 err=%v wf=%+v", err, wf)
	}
}

// 失败结果不入缓存：否则一次越权探测（userID 不匹配）会把
// 「不存在」这个结论固化下来，之后合法的读取也会被它挡住。
func TestCachedWorkflowsDoesNotCacheNotFound(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "存在")
	c := newCached(f, time.Hour)

	// 用错误的 userID 读 → 未找到
	if _, err := c.GetWorkflow(ctx, 1, 999); !errors.Is(err, ErrWorkflowNotFound) {
		t.Fatalf("期望 ErrWorkflowNotFound，实际 %v", err)
	}
	// 正确的 userID 读 → 必须能拿到（说明上一次的失败没有被缓存）
	wf, err := c.GetWorkflow(ctx, 1, 100)
	if err != nil {
		t.Fatalf("合法读取被上一次的失败结果挡住了: %v", err)
	}
	if wf.Name != "存在" {
		t.Fatalf("读到的名字不对: %s", wf.Name)
	}
}

// ---------- 降级：缓存挂掉时系统应变慢，而不是变坏 ----------

// 读缓存报错（Redis 抖动）必须当作未命中回源，不能把错误抛给调用方。
func TestCachedWorkflowsFallsThroughOnCacheReadError(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "兜底")
	c := NewCachedWorkflows(f, &brokenCache{getErr: errors.New("redis down")}, time.Minute, nil)

	wf, err := c.GetWorkflow(ctx, 1, 100)
	if err != nil {
		t.Fatalf("缓存读失败时应回源，实际报错: %v", err)
	}
	if wf.Name != "兜底" {
		t.Fatalf("回源结果不对: %s", wf.Name)
	}
	if f.calls() != 1 {
		t.Fatalf("应回源 1 次，实际 %d", f.calls())
	}
}

// 写缓存报错同样不能影响请求结果。
func TestCachedWorkflowsFallsThroughOnCacheWriteError(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "兜底")
	c := NewCachedWorkflows(f, &brokenCache{setErr: errors.New("redis down")}, time.Minute, nil)

	if _, err := c.GetWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("缓存写失败不应影响请求，实际报错: %v", err)
	}
	if c.CacheStats().Errors == 0 {
		t.Fatal("写缓存失败应计入 errors 指标（否则缓存静默失效，没人会发现）")
	}
}

// 缓存里是坏值（旧版本结构体写的 / 被污染）时应当作未命中并顺手删掉，
// 而不是把解析错误抛出去。
func TestCachedWorkflowsDiscardsCorruptedValue(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "正常")
	mc := cache.NewMemoryCache()
	// 手工塞一个解析不了的值
	if err := mc.Set(ctx, workflowCacheKey(100, 1), "{not json", time.Minute); err != nil {
		t.Fatalf("预置坏值失败: %v", err)
	}
	c := NewCachedWorkflows(f, mc, time.Minute, nil)

	wf, err := c.GetWorkflow(ctx, 1, 100)
	if err != nil {
		t.Fatalf("坏值应触发回源而不是报错: %v", err)
	}
	if wf.Name != "正常" {
		t.Fatalf("回源结果不对: %s", wf.Name)
	}
	if c.CacheStats().Errors == 0 {
		t.Fatal("反序列化失败应计入 errors 指标")
	}
	// 坏值应已被删除，下一次读能正常命中
	if _, err := c.GetWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("第二次读取失败: %v", err)
	}
	if c.CacheStats().Hits == 0 {
		t.Fatal("坏值清除后第二次读应命中缓存")
	}
}

// TTL 过期后必须回源（这是多实例部署下唯一的正确性保证）。
func TestCachedWorkflowsExpiresAfterTTL(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "v1")
	c := newCached(f, 30*time.Millisecond)

	if _, err := c.GetWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("首次读取失败: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	// 直接改库（模拟"别的实例改了工作流，本实例收不到失效通知"）
	f.seed(1, 100, "v2")

	wf, err := c.GetWorkflow(ctx, 1, 100)
	if err != nil {
		t.Fatalf("TTL 过期后读取失败: %v", err)
	}
	if wf.Name != "v2" {
		t.Fatalf("TTL 过期后应回源读到 v2，实际读到 %s", wf.Name)
	}
}

// 关闭缓存（cache=nil 或 ttl<=0）时必须退化成直通，而不是报错。
// 装配处因此可以无条件包一层，不必写「有没有缓存」的分支。
func TestCachedWorkflowsPassthroughWhenDisabled(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "直通")

	for _, tc := range []struct {
		name string
		c    *CachedWorkflows
	}{
		{"cache 为 nil", NewCachedWorkflows(f, nil, time.Minute, nil)},
		{"ttl 为 0", NewCachedWorkflows(f, cache.NewMemoryCache(), 0, nil)},
		{"ttl 为负", NewCachedWorkflows(f, cache.NewMemoryCache(), -time.Second, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.calls()
			for i := 0; i < 2; i++ {
				if _, err := tc.c.GetWorkflow(ctx, 1, 100); err != nil {
					t.Fatalf("读取失败: %v", err)
				}
			}
			if got := f.calls() - before; got != 2 {
				t.Fatalf("缓存关闭时应每次回源（2 次），实际 %d", got)
			}
		})
	}
}

// ---------- 写路径的先后顺序 ----------

// 写路径必须是「先落库、后失效」。
// 顺序反了的话，在「失效完成」与「写库提交」之间有一个窗口：
// 并发读会把**旧值**重新灌进缓存，而写库随后才生效——
// 缓存里留下一个谁都不会再改的陈旧值，直到 TTL 过期。
//
// 这里用一个能观察到"失效时刻库里的值"的缓存来验证顺序。
func TestCachedWorkflowsWritesThenInvalidates(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "旧值")
	probe := &orderProbeCache{store: f}
	c := NewCachedWorkflows(f, probe, time.Hour, nil)

	// 先预热，让缓存里有值，这样失效才"有事可做"
	if _, err := c.GetWorkflow(ctx, 1, 100); err != nil {
		t.Fatalf("预热读取失败: %v", err)
	}

	if err := c.UpdateWorkflow(ctx, &model.Workflow{ID: 1, UserID: 100, Name: "新值"}); err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}

	if probe.delSawName != "新值" {
		t.Fatalf("失效发生在落库之前：删除缓存时库里还是「%s」。"+
			"顺序必须是先写库再失效，否则并发读会把旧值重新灌回缓存", probe.delSawName)
	}
}

// orderProbeCache 在 Del 被调用时记录"此刻库里是什么"。
type orderProbeCache struct {
	store      *fakeWorkflows
	delSawName string
}

func (c *orderProbeCache) Get(context.Context, string) (string, bool, error) { return "", false, nil }
func (c *orderProbeCache) Set(context.Context, string, string, time.Duration) error {
	return nil
}
func (c *orderProbeCache) Del(context.Context, string) error {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	c.delSawName = c.store.workflows[1].Name
	return nil
}

// ---------- 直通方法的覆盖 ----------

func TestCachedWorkflowsPassesThroughNonCachedMethods(t *testing.T) {
	ctx := context.Background()
	f := newFakeWorkflows()
	f.seed(1, 100, "x")
	c := newCached(f, time.Minute)

	// ListWorkflows 刻意不缓存（低频 + 失效成本高）
	if _, err := c.ListWorkflows(ctx, 100); err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if _, err := c.GetWorkflowNodes(ctx, 1); err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	if _, err := c.GetWorkflowEdges(ctx, 1); err != nil {
		t.Fatalf("GetWorkflowEdges: %v", err)
	}
	// 这些方法都不该产生缓存命中
	if st := c.CacheStats(); st.Hits != 0 || st.Misses != 0 {
		t.Fatalf("直通方法不应触碰缓存，实际 hits=%d misses=%d", st.Hits, st.Misses)
	}
}

// brokenCache 用于模拟 Redis 故障。
type brokenCache struct {
	getErr error
	setErr error
}

func (c *brokenCache) Get(context.Context, string) (string, bool, error) {
	return "", false, c.getErr
}
func (c *brokenCache) Set(context.Context, string, string, time.Duration) error { return c.setErr }
func (c *brokenCache) Del(context.Context, string) error                        { return nil }

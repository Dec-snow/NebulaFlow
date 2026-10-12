# NebulaFlow 后端代码审查报告

> 审查范围：`internal/api/`、`internal/scheduler/`、`internal/worker/`、`internal/llm/`、`internal/config/`、`cmd/server/main.go`、`internal/store/`、`internal/database/`、`internal/observability/`、`internal/tool/`

---

## 一、安全问题

### 1. HTTP Tool 存在 SSRF 风险
- **严重程度**：高
- **所在文件**：`internal/tool/registry.go`（第 376-427 行，`HTTPTool.Call`）
- **问题描述**：
  `HTTPTool` 允许模型/用户请求任意 URL，未做任何内网地址过滤。攻击者可以通过构造包含 `http://169.254.169.254/`（云元数据服务）、`http://localhost:6379/`（Redis）、`http://10.x.x.x/`（内网服务）等 URL 的工作流节点，探测或攻击内网资源。由于工具由 LLM 自主调用（Agent 模式），即使前端限制了 URL，模型也可以自主生成内网 URL 发起请求。
- **修复建议**：
  1. 增加 URL 安全校验：解析 hostname，拒绝以下情况：
     - 私有 IP 段（10.0.0.0/8、172.16.0.0/12、192.168.0.0/16、127.0.0.0/8、169.254.0.0/16）
     - localhost / .local 等保留域名
     - 非 http/https scheme
     - 非标准端口（80/443 以外的端口默认拒绝，可配置白名单）
  2. 使用专用的出站 HTTP 客户端，绑定到非特权网络命名空间（生产环境可考虑用 eBPF 或代理做出口控制）
  3. 在配置中增加 `HTTP_TOOL_ALLOWED_DOMAINS` 白名单选项

### 2. LLM Provider API Key 明文存储在数据库
- **严重程度**：中
- **所在文件**：`internal/store/providers.go`（第 14-26 行，`UpsertProvider`）
- **问题描述**：
  LLM Provider 的 API Key 以明文形式存储在 `llm_providers.api_key` 字段中。一旦数据库泄露（SQL 注入、备份泄露、运维人员越权），所有供应商的密钥将直接暴露，可能导致巨额 LLM 账单。目前 list 接口虽然做了掩码（`maskKey`），但存储层面是完全明文的。
- **修复建议**：
  1. 使用 AES-256-GCM 加密存储密钥，主密钥从环境变量或 KMS 获取
  2. 密钥写入时加密，读取时解密（在内存中解密，不落盘）
  3. 增加密钥轮换机制：修改密钥时旧密钥立即失效
  4. 审计日志：记录谁在什么时候修改了 Provider 配置

### 3. Provider API 缺少租户/权限隔离
- **严重程度**：高
- **所在文件**：`internal/api/handlers_provider.go`（全文）
- **问题描述**：
  Provider 的 CRUD 接口（`GET/POST/PUT/DELETE /api/providers`）仅通过 `authMiddleware` 校验了登录状态，但没有任何用户归属校验或角色校验。任何登录用户都可以：
  - 查看所有 Provider 的配置（包括掩码后的 key）
  - 修改/删除任意 Provider
  - 添加新的 Provider 并让所有用户使用
  这意味着普通用户可以篡改系统级 LLM 配置（如把 API key 换成自己的、关闭主 provider 让系统降级）。
- **修复建议**：
  1. 引入角色系统（admin/user），只有 admin 能管理 Provider
  2. 如果设计为"每个用户有自己的 Provider"，则在 `llm_providers` 表增加 `user_id` 字段并在查询时过滤
  3. 当前代码中 `ListProviders` 没有 `user_id` 过滤，与其他表（workflows/tasks/kb）的设计不一致

### 4. SSE 流端点 CORS 硬编码为通配符
- **严重程度**：中
- **所在文件**：`internal/api/handlers_task.go`（第 143 行，`stream` 方法）
- **问题描述**：
  SSE 流端点硬编码了 `Access-Control-Allow-Origin: *`，绕过了全局 `corsMiddleware` 的配置。即使管理员将 `CORS_ORIGINS` 设为特定域名，SSE 端点仍然允许任意来源访问。虽然认证走 Authorization header（不携带 cookie），不构成 CSRF 式的凭据泄露，但扩大了攻击面——恶意网站可以建立 SSE 连接读取流式输出（如果用户 token 被 XSS 窃取的话）。
- **修复建议**：
  移除硬编码，让全局 CORS 中间件统一处理。SSE 响应会经过 `corsMiddleware`，无需重复设置。

### 5. mustID 解析失败静默返回 0
- **严重程度**：中
- **所在文件**：`internal/api/handlers_workflow.go`（第 222-224 行）
- **问题描述**：
  `mustID` 函数忽略了 `strconv.ParseInt` 的错误，解析失败时返回 0。如果用户传入非数字的 `:id`（如 `abc`、`-`），会被当成 ID=0 去查询。虽然大多数查询有 user_id 过滤，一般会返回 404，但在某些场景下可能产生意外行为（例如未来如果存在 ID=0 的特殊记录）。这也让 API 行为不一致：传非法 ID 返回 404 而不是 400，客户端难以区分"不存在"和"参数非法"。
- **修复建议**：
  解析失败时返回 `http.StatusBadRequest`，并在 handler 中检查。可以改成 `func mustID(c *gin.Context) (int64, bool)` 或在解析失败时直接 `c.AbortWithStatusJSON(400, ...)`。

### 6. 模板详情接口未校验用户归属
- **严重程度**：低
- **所在文件**：`internal/api/handlers_workflow.go`（第 160-169 行，`getTemplate`）
- **问题描述**：
  `GET /api/templates/:id` 不校验用户归属，注释中说明"系统模板(user_id=0)所有人可见"。但 `GetTemplate` 实际返回所有 `is_template=true` 的工作流，包括其他用户创建的私有模板（`user_id != 0` 的模板）。其他用户可以通过遍历 ID 来枚举别人的私有模板及其节点配置。
- **修复建议**：
  在 `GetTemplate` 的 SQL 中增加过滤条件：`AND (user_id = 0 OR user_id = $2)`，确保用户只能看到系统模板和自己创建的模板。

---

## 二、性能问题

### 1. 节点级重试创建大量 goroutine，存在失控风险
- **严重程度**：中
- **所在文件**：`internal/scheduler/scheduler.go`（第 821-831 行，`submitNode` 重试逻辑）
- **问题描述**：
  每个节点失败后，如果需要重试，就启动一个新的 goroutine 等待延迟后重新提交。当大量节点同时失败时（例如 LLM provider 全线故障），会创建大量 goroutine。虽然每个 goroutine 最终会退出（任务结束时 runCtx 取消），但在故障期间 goroutine 数量会瞬时膨胀。更严重的是：如果任务正常完成（非取消），已启动的重试 goroutine 不会被提前清理，会等到延迟到期后才发现 `runCtx` 已取消而退出——浪费了内存和调度开销。
- **修复建议**：
  1. 使用统一的时间轮（timing wheel）或延迟队列管理所有重试，避免每节点一个 goroutine
  2. 或者使用 `time.AfterFunc` + 任务级的 `sync.Map` 跟踪待重试的节点，任务完成时主动取消所有待重试的定时器
  3. 增加节点级重试并发度上限，避免故障时所有节点同时重试打爆 provider

### 2. 模板分类列表全表扫描 + 内存去重
- **严重程度**：中
- **所在文件**：`internal/api/handlers_workflow.go`（第 195-213 行，`listCategories`）
- **问题描述**：
  `listCategories` 先调用 `ListTemplates` 加载全部模板（包含所有字段），然后在 Go 代码中遍历去重提取分类。当模板数量大时，会传输和加载大量不必要的数据，只为了拿到一个字符串列表。这是典型的"把计算搬到应用层"的反模式。
- **修复建议**：
  在 store 层增加 `ListTemplateCategories(ctx, userID)` 方法，SQL 用 `SELECT DISTINCT category FROM workflows WHERE is_template=true AND (user_id=0 OR user_id=$1) AND category IS NOT NULL`，数据库层直接返回去重后的分类列表。

### 3. Trace 按任务反查为 O(n) 线性扫描
- **严重程度**：低
- **所在文件**：`internal/observability/tracing/store.go`（第 249-261 行，`TraceForTask`）
- **问题描述**：
  `TraceForTask(taskID)` 通过遍历 `s.traces` map 的所有记录来查找匹配的 taskID，时间复杂度 O(n)。当 `maxTraces` 设为较大值（如 10000）时，每次调用 `/api/tasks/:id/trace` 都要遍历全部 traces。虽然 tracing 主要用于调试，调用频率不高，但这是一个不必要的低效实现。
- **修复建议**：
  维护一个 `taskID -> traceID` 的反向索引 map（`taskIndex map[int64]string`），`Link` 时更新，`TraceForTask` 时直接 O(1) 查找。注意任务重跑/重试时可能有多条 trace 对应同一 task，需要考虑覆盖策略。

### 4. GetWorkflow 3 次查询，缓存未命中时有性能损耗
- **严重程度**：低
- **所在文件**：`internal/store/workflows.go`（第 89-112 行，`GetWorkflow`）
- **问题描述**：
  每次获取工作流需要 3 次数据库 round-trip：workflow 基本信息 + nodes + edges。虽然已有 `CachedWorkflows` 装饰器（缓存命中率高时没问题），但缓存未命中时仍是 3 次查询。对于调度器热路径（每秒几百次读），缓存命中时性能没问题，但冷启动或缓存失效风暴时会有压力。
- **修复建议**（优先级低）：
  可以用单条 SQL + JOIN 一次性获取（如 `SELECT w.*, n.*, e.* FROM workflows w LEFT JOIN workflow_nodes n ON ... LEFT JOIN workflow_edges e ON ... WHERE w.id=$1`），然后在应用层拼装。但当前有缓存，收益不大，不建议优先优化。

---

## 三、架构问题

### 1. Scheduler 职责过重，违反单一职责原则
- **严重程度**：中
- **所在文件**：`internal/scheduler/scheduler.go`、`internal/scheduler/executor.go`
- **问题描述**：
  `Scheduler` 结构体有 15+ 个字段，同时承担以下职责：
  - 队列消费（consumeLoop）
  - DAG 调度（executeTask 主循环）
  - 节点执行（ExecuteNode 及各类型节点的 run* 方法）
  - 任务取消管理（cancels/cancelled map）
  - 队列维护（maintainQueue）
  - 分布式锁（wfLock）
  - tracing 关联
  这使得 Scheduler 难以测试和维护，任何一个职责的变更都可能影响其他部分。
- **修复建议**：
  1. 将节点执行逻辑（`ExecuteNode` 及 `runLLMNode`、`runRAGNode`、`runToolNode` 等）抽到独立的 `NodeExecutor` 结构中
  2. 将队列维护抽到独立的 `QueueMaintainer`
  3. Scheduler 只负责 DAG 调度逻辑，通过接口依赖其他组件

### 2. 错误定义分散，错误映射不完整
- **严重程度**：中
- **所在文件**：`internal/api/middleware.go`（`abortWithError`）、`internal/store/errors.go`、`internal/auth/service.go`
- **问题描述**：
  错误 sentinel 分散在多个包中（store 包、auth 包、task 包、queue 包）。`abortWithError` 只匹配了有限的几种错误类型，大量 `fmt.Errorf` 包装的错误会落入 default 分支返回 500。例如：
  - RAG 检索失败 → 500
  - 向量化失败 → 500
  - 工具执行失败 → 500
  这些情况中有些应该是 4xx（如参数错误导致的工具调用失败）。
- **修复建议**：
  1. 定义统一的 `apperror` 包，包含错误类型（Code + Message +  HTTP 状态码）
  2. 领域层返回带类型的错误，API 层按 Code 映射 HTTP 状态
  3. 避免到处使用 `fmt.Errorf` 传递业务错误

### 3. main.go run 函数过长，装配逻辑耦合
- **严重程度**：低
- **所在文件**：`cmd/server/main.go`（第 88-384 行）
- **问题描述**：
  `run` 函数接近 300 行，包含了从基础设施到 API 的全部装配逻辑。虽然注释很详细，但函数过长导致：
  - 难以编写集成测试
  - 新增模块时容易放错位置（注释里写的是 "5. LLM Gateway" 然后 "4. Worker Pool + Scheduler"，编号已经乱了）
  - 依赖关系不清晰
- **修复建议**：
  拆分成多个子函数，例如：
  ```go
  func setupTracing(...) (*tracing.Provider, error)
  func setupInfra(...) (*database.DB, *redis.Client, error)
  func setupStore(...) (*store.Store, error) 
  func setupServices(...) (*task.Service, *llm.Gateway, ...)
  func setupScheduler(...) (*scheduler.Scheduler, error)
  func setupServer(...) *http.Server
  ```

### 4. Provider 管理与运行时 Gateway 配置不同步
- **严重程度**：中
- **所在文件**：`cmd/server/main.go`（`buildLLM` 函数）、`internal/api/handlers_provider.go`
- **问题描述**：
  Gateway 在启动时从数据库加载 Provider 配置并初始化。但之后用户通过 API 增删改 Provider 时，Gateway 不会动态更新——必须重启服务才能生效。对于一个有 UI 管理 Provider 的平台来说，这是一个明显的功能缺陷。
- **修复建议**：
  1. Gateway 支持动态热更新：Provider 变更后通知 Gateway 重新加载
  2. 或在 Gateway 中增加 provider 的定期刷新机制（如每 60 秒从 DB reload）
  3. 或在 API 层维护一个通知通道，变更后立即触发 Gateway reload

---

## 四、可靠性问题

### 1. 文档向量化同步执行，大文件会导致 HTTP 超时
- **严重程度**：高
- **所在文件**：`internal/api/handlers_knowledge.go`（第 135-157 行，`uploadDoc` → `indexDocument`）
- **问题描述**：
  上传文档后，同步执行分块 + 向量化 + 入库。对于大文档（几十 MB 的 PDF 或长文本），向量化过程可能耗时几十秒甚至几分钟，远超 HTTP 请求的合理超时时间。客户端会收到超时错误，但服务端可能仍在后台执行（连接已断但 goroutine 可能还在跑），导致状态不一致。
- **修复建议**：
  1. 改为异步索引：上传文档后立即返回 201（状态为 pending），将索引任务提交到后台 worker 执行
  2. 前端通过轮询或 SSE 等待索引完成
  3. 索引失败时更新文档状态为 failed 并记录错误信息
  4. 增加索引任务队列，控制并发度，避免同时索引大量文档打垮 LLM/Embedding 服务

### 2. LLM Gateway 缺少熔断机制
- **严重程度**：中
- **所在文件**：`internal/llm/gateway.go`
- **问题描述**：
  Gateway 的故障转移策略是"每次请求依次尝试所有 provider"。当主 provider 持续故障时，每个请求都会先尝试主 provider（超时/失败）再试备用 provider，导致：
  - 每次请求都白白浪费主 provider 的超时时间（30-60 秒首 token 等待）
  - 主 provider 持续被打（虽然它已经挂了）
  - 系统整体延迟大幅上升
  缺少熔断器模式：连续失败 N 次后暂时跳过该 provider，一段时间后半开试探。
- **修复建议**：
  实现熔断器（Circuit Breaker）模式：
  1. 每个 provider 维护连续失败计数
  2. 连续失败超过阈值（如 5 次）后进入 Open 状态，请求直接跳过
  3. 经过冷却时间（如 30 秒）后进入 Half-Open 状态，放一个请求试探
  4. 成功则 Close，失败则继续 Open
  5. 可使用 `sony/gobreaker` 或自研轻量实现

### 3. SSE 连接无上限，可能耗尽服务器资源
- **严重程度**：中
- **所在文件**：`internal/api/handlers_task.go`（`stream` 方法）、`internal/task/hub.go`
- **问题描述**：
  SSE 流连接没有数量限制。恶意客户端可以建立大量 SSE 连接（每个任务一个连接，甚至同一任务多个连接），消耗服务器内存和 goroutine。每个 SSE 连接占一个 goroutine + 一个 channel（128 容量的 Event），1 万个连接约消耗几十 MB 内存和调度开销，虽不致命，但缺少防护。
- **修复建议**：
  1. 限制每用户的并发 SSE 连接数（如每用户最多 10 个）
  2. 设置最大连接时长（如 2 小时后主动断开，让客户端重连）
  3. 在 Hub 中增加总订阅者数的监控和告警

### 4. Redis 限流器 INCR + EXPIRE 非原子，极端场景下窗口永不过期
- **严重程度**：低
- **所在文件**：`internal/ratelimit/limiter.go`（第 53-66 行，`RedisLimiter.Allow`）
- **问题描述**：
  `INCR` 和 `EXPIRE` 是两条独立命令，中间如果 Redis 故障或进程崩溃，可能出现 INCR 成功但 EXPIRE 失败的情况。此时 key 没有 TTL，计数永远不会重置，导致该 key 的请求被永久拒绝（因为计数只增不减）。虽然代码中对 EXPIRE 失败做了"降级放行"的处理（第 62-65 行），但这只覆盖了 EXPIRE 命令报错的情况。如果 EXPIRE 命令执行成功但效果异常（极端场景），或者 Redis 主从切换导致 TTL 丢失，仍可能出现永久计数。
- **修复建议**（优先级低）：
  用 Lua 脚本保证 INCR + EXPIRE 的原子性。当前代码已经有"EXPIRE 失败则放行"的兜底，实际风险较低。

### 5. 优雅关闭顺序：先关 pool 再关 HTTP server
- **严重程度**：低
- **所在文件**：`cmd/server/main.go`（第 370-376 行）
- **问题描述**：
  优雅关闭时，先执行 `pool.Shutdown(10 * time.Second)` 再执行 `httpSrv.Shutdown(shutdownCtx)`。这意味着在 pool 关闭后的一段时间里，HTTP server 仍在接收请求，新提交的任务会因为 worker pool 已关闭而失败（`Submit` 返回 false，但任务已经落库了）。正确的顺序应该是先停止接收新请求，再排空在途任务。
- **修复建议**：
  调换顺序：先 `httpSrv.Shutdown`（停止接收新请求，等待在途请求完成），再 `pool.Shutdown`（排空在途任务）。

---

## 五、可观测性问题

### 1. 流式 LLM 调用未记录到 duration metrics
- **严重程度**：中
- **所在文件**：`internal/scheduler/executor.go`（`runLLMNode` 流式分支）
- **问题描述**：
  非流式 LLM 调用（`s.llm.Chat`）会通过 `s.recordUsage` 最终调用 `metrics.ObserveLLM` 记录耗时和 token。但流式 LLM 调用（`s.llm.Stream`）只记录了 token，没有记录调用耗时到 `LLMRequestDuration` histogram。这意味着监控面板上看不到流式调用的延迟分布，而流式恰恰是用户主要使用的模式（SSE 实时输出）。
- **修复建议**：
  在流式调用完成后（stream 结束时）同样调用 `s.metrics.ObserveLLM`，记录总耗时和 token 用量。注意流式的"总耗时"是从开始到最后一个 token 的时间，需要在 `runLLMNode` 中自己计时。

### 2. 日志级别使用缺乏统一规范
- **严重程度**：低
- **所在文件**：多处
- **问题描述**：
  代码中日志级别的使用有一定的随意性。例如：
  - Redis 连接失败降级为内存实现：`logger.Warn`（合理）
  - 队列维护失败：`logger.Warn`（合理）
  - worker 节点失败：`p.logger.Warn`（业务失败用 Warn 是否合适？）
  - 任务执行失败：`s.logger.Error`（引擎错误 vs 业务失败未区分）
  缺少明确的级别规范，导致告警配置困难——无法简单地"所有 ERROR 都告警"，因为有些 ERROR 是预期内的业务失败。
- **修复建议**：
  制定并遵循日志级别规范：
  - **ERROR**：系统级故障，需要人工介入（数据库挂了、Redis 连不上、内部状态不一致）
  - **WARN**：异常但自动恢复了/降级了（provider 切换、限流触发、池接近饱和）
  - **INFO**：正常生命周期事件（启动、关闭、配置变更）
  - **DEBUG**：详细调试信息（每个节点的开始结束等）
  业务失败（任务失败、节点失败）应该用 INFO 或 WARN 级别，而不是 ERROR。

### 3. Hub 丢弃事件数未上报到 Prometheus
- **严重程度**：低
- **所在文件**：`internal/task/hub.go`（`dropped` 字段）
- **问题描述**：
  `Hub` 有 `dropped` 原子计数器记录被丢弃的事件数（订阅者消费太慢时），也有 `subCount` 记录订阅者总数。但这些指标只通过 `Stats()` 方法暴露给 dashboard handler，没有上报到 Prometheus。这意味着无法在 Grafana 中配置"事件丢弃率过高"的告警——而事件丢弃恰恰是前端卡顿/消息丢失的早期信号。
- **修复建议**：
  在 `observability.Metrics` 中增加 Hub 相关指标（SSE 连接数、事件丢弃数），由 dashboard stats 接口或单独的 goroutine 周期性上报。

### 4. 缺少慢查询日志
- **严重程度**：低
- **所在文件**：`internal/database/database.go`
- **问题描述**：
  数据库层没有慢查询日志。当出现性能问题时，无法快速定位是哪条 SQL 导致的。pgx 本身支持通过 `QueryTracer` 接口追踪查询，但当前代码没有利用。
- **修复建议**：
  实现一个简单的查询 tracer，超过阈值（如 500ms）的查询记录 WARN 日志，包含 SQL 语句和耗时。生产环境可以采样记录（如 1%）避免日志量过大。

### 5. trace list 接口返回所有用户的 trace（归属校验不完整）
- **严重程度**：中
- **所在文件**：`internal/api/handlers_trace.go`（需验证）、`internal/observability/tracing/store.go`（`Recent` 方法）
- **问题描述**：
  `SpanStore.Recent` 方法支持 `ownerID` 过滤（`ownerID > 0` 时只返回该用户的 trace）。但 `/api/traces` 列表接口是否正确传递了当前用户的 ID 需要确认。如果列表接口没有做归属过滤，用户 A 可以看到用户 B 的 trace（包括 span 中的错误信息，可能包含 SQL 语句、内网地址等敏感信息）。
- **修复建议**：
  确认 `traceHandler.list` 调用 `Recent` 时传入了当前用户的 `userID`。如果没有，必须加上——trace 中可能包含敏感的内部错误详情。

---

## 六、其他发现（非问题，设计亮点记录）

在审查过程中，也发现了许多设计精良的地方，值得肯定：

1. **SQL 注入防护到位**：所有数据库查询都使用参数化查询（pgx 的 $1, $2 占位符），没有字符串拼接 SQL 的情况。
2. **JWT 安全设计完善**：密钥强度校验、HS256 算法白名单、时间侧信道防护（dummy hash）都有实现。
3. **连接池配置经过深思熟虑**：显式配置 MaxConns/MinConns，并有启动时的配置警告。
4. **队列背压设计合理**：Admit 预检 + Enqueue 内部检查 + MAXLEN 护栏 + TrimConsumed 回收，四层防护。
5. **DAG 调度并发模型清晰**：入度归零调度 + 失败传播 + 取消链路，设计正确。
6. **Worker Pool 优雅关闭逻辑严谨**：CAS 闸门 + inflight WaitGroup + 两阶段关闭，避免了常见的 "send on closed channel" panic。
7. **Tracing 设计全面**：跨进程传递、队列等待计算、自耗时、瓶颈识别都有考虑。
8. **配置校验 fail-safe**：非 dev 环境严格校验，默认值不安全时拒绝启动。

---

## 总结

| 类别 | 高严重 | 中严重 | 低严重 | 合计 |
|------|-------|-------|-------|------|
| 安全问题 | 2 | 3 | 1 | 6 |
| 性能问题 | 0 | 2 | 2 | 4 |
| 架构问题 | 0 | 2 | 2 | 4 |
| 可靠性问题 | 1 | 3 | 1 | 5 |
| 可观测性问题 | 0 | 2 | 3 | 5 |
| **总计** | **3** | **12** | **9** | **24** |

**优先修复建议（按影响程度排序）**：
1. HTTP Tool SSRF 防护（安全-高）
2. Provider API 权限隔离（安全-高）
3. 文档索引异步化（可靠性-高）
4. LLM Gateway 熔断机制（可靠性-中）
5. Provider API Key 加密存储（安全-中）
6. 节点重试 goroutine 收敛（性能-中）
7. SSE 连接数限制与超时（可靠性-中）

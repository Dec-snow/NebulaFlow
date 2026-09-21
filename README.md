# NebulaFlow · Enterprise AI Agent Workflow Platform

> **高并发 AI Agent 工作流平台**：基于 Go 构建，支持 DAG 工作流编排、RAG 知识库、多 LLM Provider、异步任务调度与实时执行监控。

NebulaFlow 是一个偏**系统工程**的项目，核心不是"调用大模型"，而是**自己实现了一套可讲 30 分钟的 Workflow Engine**：

- 基于 DAG 的任务依赖调度，无依赖节点并行执行
- Worker Pool + 背压的并发控制与优雅退出
- Redis Stream 任务队列 + Consumer Group + ACK + 崩溃恢复 + DLQ
- 统一 LLM Gateway（Provider 抽象 + 故障转移 + 流式输出）
- **Agent 自主工具调用**（function calling：模型自己决定调哪个工具、传什么参数、要不要再来一轮）
- RAG 知识库（上传 → 分块 → 向量化 → Top-K 检索）
- SSE 实时推送任务/节点/Token/工具调用状态
- Prometheus + Grafana 业务级可观测性
- **全链路 Trace**（W3C traceparent 随队列消息跨进程续接，回答"这个任务慢在哪一步"）
- Docker Compose 一键启动，GitHub Actions CI/CD

---

## 一、系统架构

```text
                         Browser
                            │
                      HTTPS / SSE
                            ▼
                    ┌───────────────┐
                    │    Nginx      │
                    └───────┬───────┘
                            ▼
                    ┌───────────────┐
                    │   Go Server   │
                    │  (Modular     │
                    │   Monolith)   │
                    │               │
                    │ API / Auth    │
                    │ Workflow      │
                    │ Scheduler     │
                    │ LLM Gateway   │
                    │ RAG           │
                    └───┬───────┬───┘
                        │       │
              ┌─────────┘       └─────────┐
              ▼                           ▼
       ┌─────────────┐              ┌───────────┐
       │ PostgreSQL  │              │   Redis   │
       │ Business DB │              │ Stream    │
       │ Task Data   │              │ RateLimit │
       │ Workflow    │              │ Lock/Cache│
       └─────────────┘              └─────┬─────┘
                                          ▼
                                  ┌──────────────┐
                                  │ Worker Pool  │
                                  │ Worker 1..N  │
                                  └──────┬───────┘
                                         │
                           ┌─────────────┼─────────────┐
                           ▼             ▼             ▼
                         LLM           RAG           Tools
```

**执行流程（1000 个用户同时提交）：**

```text
1000 Requests
      ↓
 API Server (JWT + 限流)
      ↓
 Redis Stream (workflow_tasks)
      ↓
 ┌───────────────┐
 │ Task Scheduler│  ← 消费任务，构建 DAG，按依赖调度
 └───────┬───────┘
         ↓
 Worker Pool (20 workers)
         ↓
    LLM / RAG / Tool
```

## 二、核心设计

### 1. DAG 调度引擎（scheduler/dag.go + scheduler.go）

```text
       A
      / \
     B   C        →  B、C 无依赖，并行执行
      \ /
       D          →  D 等待 B、C 全部完成（入度归零）后执行
```

- 节点/边存数据库（`workflow_nodes` / `workflow_edges`），不存一坨 JSON
- Kahn 拓扑排序做环检测，保存时即校验（`A→B→C→A` 直接拒绝）
- 执行期用入度计数驱动：就绪节点提交 Worker Pool，完成后回传结果并递减下游入度
- 任务级超时（context.WithTimeout）+ 节点级超时 + 取消（用户可 cancel）

### 2. Worker Pool（worker/pool.go）

- 固定 20 个 worker，从带缓冲 channel 取节点任务
- 队列满时 `Submit` 阻塞 = **背压**（不会无限堆积内存）
- 优雅关闭：读锁保证提交/关闭互斥 → 关闭 job channel → 排空在途任务 → 超时兜底

### 3. Redis Stream 任务队列（queue/queue.go）

| 能力 | 实现 |
|---|---|
| 入队 | `XADD workflow_tasks MAXLEN ~ QUEUE_MAXLEN` |
| 消费 | `XREADGROUP GROUP nebula_workers` |
| 完成 | `XACK` |
| 容量回收 | 调度器每 10s `XTRIM MINID` 回收「已确认前缀」（`last-delivered-id` 与 `XPENDING.Lower` 取小） |
| **提交侧背压** | `XLEN ≥ QUEUE_MAXLEN × QUEUE_ADMIT_RATIO`（默认 0.8）时返回 `503` + `Retry-After: 1`，**在落库之前**拒绝，不留任何任务行 |
| Worker 崩溃恢复 | PEL + `XAUTOCLAIM` 认领超时 pending 消息 |
| 失败 | 指数重投（ZSET 延迟队列 + 回流），重试用尽进 `DLQ`（同样带容量上界） |

> **`MAXLEN` 是护栏，不是队列大小旋钮。** `XACK` 只把消息移出 PEL、不删消息本体，
> 而 `MAXLEN` 在 `XADD` 时裁掉的是**尚未投递的最老条目**——会静默丢任务。
> 日常回收靠 `XTRIM MINID` 删「已投递且已确认」的前缀（不丢任务），
> `MAXLEN` 只在消费端整体停摆时兜底。压测实测：单靠 `MAXLEN` 时
> `QUEUE_MAXLEN=10000` 只落地 52.68%、9488/20000 个任务永久卡在 `pending`。
> 加上背压后落地率 100%、`pending` 为 0，代价是提交吞吐 -13%（多一次 `XLEN` 往返）。
> 详见 [benchmark/README.md 第 6、7 节](benchmark/README.md)。

### 4. LLM Gateway + 故障转移（llm/provider.go + gateway.go）

```go
type Provider interface {
    Chat(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request) (<-chan Chunk, error)
}
```

- `DeepSeekProvider` / `OpenAIProvider` / `MiMoProvider`（OpenAI 兼容协议）+ `OllamaProvider`（原生 `/api/chat`）+ `MockProvider`（离线演示）
- Gateway 按优先级依次尝试：`DeepSeek → Ollama → Mock`，失败自动切换，前端无感，并推送 `fallback` 事件
- 每次调用记录 `provider/model/tokens/latency` 到 `usage_records` 与 Prometheus

### 5. RAG（rag/service.go + store/knowledge.go）

```text
Upload → 文本抽取 → Chunk(600字/80字重叠) → Embedding → 存储
Question → Embedding → pgvector HNSW 检索 Top-K → 注入 Prompt → LLM
```

- Embedding 抽象：`OpenAI / Ollama` 优先，失败降级到本地确定性 hash 向量（离线可用）
- **向量检索完全下推到 PostgreSQL**：`embedding_v vector(384)` + HNSW 索引（`vector_cosine_ops`），
  查询走 `ORDER BY embedding_v <=> $q LIMIT k`，只回传 k 行且不回传向量本身
- 首版是「全量拉进进程 + 应用层算余弦」，5 万分块下单次检索要传 200MB、耗时 1.6s；
  改造后单次 ~10ms、回传 395B（**提速 162×，回传数据量降 53.8 万倍**，见 [benchmark/README.md](benchmark/README.md)）
- **多租户漏召回修复**：HNSW 索引是全局的，而检索按 `knowledge_base_id` 过滤。
  pgvector 默认 `hnsw.iterative_scan=off` 时索引扫描只吐 `hnsw.ef_search`(=40) 行候选就结束，
  被过滤后**返回不足 k 条**（实测 30 次查询 28 次不足）。`SearchChunks` 用事务 +
  `SET LOCAL hnsw.iterative_scan = strict_order` 让索引继续往外找，直到凑满 k
- 检索入口收窄为 `SearchChunks(ctx, kbID, queryVec, k)`——**没有「列出全部分块」的方法，这是刻意的**

### 6. SSE 实时执行流（task/hub.go）

```text
GET /api/tasks/:id/stream
event: task_running
event: node_started   {node: analyst}
event: token          {content: "根据"}
event: node_completed {node: writer, duration_ms: 842}
event: task_completed {output: "..."}
```

- 订阅-发布中枢，节点状态/LLM Token 逐字推送
- 15s 心跳防代理断开；断线重连先发任务快照

### 7. 可观测性（observability/metrics.go）

```
nebulaflow_http_requests_total
nebulaflow_http_request_duration_seconds
nebulaflow_workflow_tasks_total / _running / _duration_seconds
nebulaflow_worker_active / _queue_size
nebulaflow_llm_requests_total / _duration_seconds / _tokens_total
nebulaflow_queue_stream_len / _max_len / _admit_limit / _dlq_len / _rejected_total / _reclaimed_entries_total
nebulaflow_db_pool_total_conns / _idle_conns / _acquired_conns / _constructing_conns / _max_conns
nebulaflow_db_pool_empty_acquire_total / _canceled_acquire_total / _new_conns_total
nebulaflow_workflow_cache_hits_total / _misses_total / _errors_total
nebulaflow_agent_tool_calls_total{tool,result} / _duration_seconds{tool}
nebulaflow_agent_rounds{result}
```

其中 `db_pool_empty_acquire_total`（池内无空闲连接而必须等待的累计次数）是回答
「连接池该配多大」的决定性指标——池满但没人等说明够用，池没满却有人等说明瓶颈在别处。

Grafana 面板随 Docker Compose 自动 provisioning。

### 8. Agent 自主工具调用（scheduler/agent.go + llm/provider.go）

LLM 节点默认是"一次问答"；开启 `extra.agentic` 后变成 **Agent 循环**：
**问模型 → 模型要求调工具 → 执行 → 结果回灌 → 再问**，直到给出最终回答或撞上轮数上限。

```jsonc
{
  "model": "deepseek-chat",
  "system": "你可以使用工具来完成任务",
  "prompt": "请计算 (12+34)*5/2",
  "extra": {
    "agentic": true,
    "tools": ["calculator", "time"], // 可选：限制可用工具（不写 = 全部）
    "max_tool_rounds": 4,            // 可选：默认 4，硬上限 16
    "tool_timeout_sec": 20           // 可选
  }
}
```

与**工具节点**的区别：工具节点是用户手动编排（画布上写死"这一步调 calculator"），
Agent 节点是模型在运行期自主决定调哪个、传什么参数、要不要再来一轮、报错了换什么做法。

四个刻意的设计选择（详见[优化任务清单](优化任务清单.md) P2-11）：

- **非流式**：`tool_calls` 在 SSE 里是增量分片，且"这一轮有没有工具调用"必须等本轮结束才知道。
  带 `Tools` 时 `Stream` **直接报错**而不是返回空流——空流会被网关判成"该 provider 无产出"
  从而触发一次毫无意义的 fallback，把调用方错误掩盖成"模型不听话"
- **工具失败不致命**：报错以 `错误：…` 回灌给模型，由它决定换做法。
  唯一例外是任务取消——区分二者靠**父 ctx 的状态**，因为工具超时与任务取消返回的都是 `context` 错误
- **白名单校验两次**：交给模型的 `tools[]` 阻止不了它说出别的名字（提示词注入/幻觉），
  所以执行前再校验一次。**这条是写测试时抓出来的真缺陷**（详见任务清单）
- **轮数硬上限**，撞上限算失败而非返回半成品答案

工具通过两个**可选**接口自述能力，第三方工具不实现也能用（走保守兜底）：

```go
type SchemaProvider interface { Parameters() json.RawMessage }                 // 参数 schema
type ArgumentCaller interface { CallWithArguments(ctx, args string) (string, error) } // JSON 入参翻译
```

**离线可端到端验证**（`STORAGE_MODE=memory`，零外部依赖）：

```bash
python .bench/p211-agent-e2e.py     # 15/15 断言，含 4 项装置自检；证据写入 .bench/p211-agent-evidence.txt
```

### 9. 全链路 Trace（observability/tracing/）

指标只能告诉你"平均轮数 2.3"，说不出**某一个慢任务**卡在哪一轮、哪个工具。
P2-12 补上跨 `API → Task → Node → LLM/Tool` 的调用链。

| 层 | 做法 |
| --- | --- |
| 进程内 | OpenTelemetry SDK，9 类 span：`task.create` / `queue.enqueue` / `task.execute` / `node.execute` / `llm.chat` / `rag.retrieve` / `tool.call` / `agent.round` / `agent.tool` |
| **跨进程** | **W3C `traceparent` 随队列消息传递**——提交进程与执行进程之间只有队列消息这一条通路 |
| 导出 | 默认写**进程内环形缓冲**（离线可查、有容量上界）；配 `OTEL_EXPORTER_OTLP_ENDPOINT` 时**同时**用 Batch 推 collector |

```bash
OTEL_ENABLED=true go run ./cmd/server
curl -H "Authorization: Bearer $TOKEN" localhost:8080/api/tasks/11/trace
```

响应头回写 `X-Trace-Id`，`GET /api/tasks/:id/trace` 是最常用的入口
（另有两个：`GET /api/traces?limit=`、`GET /api/traces/:trace_id`）。

实测一条链路（任务 #11，14 个 span、7 层、整链 40.9ms）：

```text
HTTP POST /api/tasks  0.00ms (self 0.00ms) kind=server
  task.create  0.00ms (self 0.00ms) kind=internal
    queue.enqueue  0.00ms (self 0.00ms) kind=internal
      task.execute  40.92ms (self 0.50ms) kind=consumer
        node.execute  40.42ms (self 0.00ms) kind=internal
          agent.round  20.19ms (self 0.00ms) kind=internal
            llm.chat  20.19ms (self 20.19ms) kind=internal
            agent.tool  0.00ms (self 0.00ms) kind=internal
          agent.round  20.23ms (self 0.00ms) kind=internal
            llm.chat  20.23ms (self 20.23ms) kind=internal
        node.execute ...（另 3 个节点，其中一个含 tool.call）
瓶颈：llm.chat 自身耗时 20.2ms（整条链 40.9ms）
```

`task.execute` 的父 span 是 `queue.enqueue` ⇒ 跨进程父子关系成立。

四个刻意的设计选择（详见[优化任务清单](优化任务清单.md) P2-12）：

- **`ParentBased` 采样 + 未采样也传播**：采样决策只在链路入口做一次；未采样的链路
  依然要传 traceparent（flags 结尾 `-00`），否则下游会当作"没有上游"而自起新链
- **自耗时是子区间的并集，不是求和**：DAG 同层节点并行，相加会让父 span 自耗时变负
- **瓶颈取"自耗时最长"而非最外层**：只看 `DurationMS` 只能得出"最外面那个最慢"这个废话
- **队列等待单独算**：任务在队列里时没有代码在跑，这段空档不落在任何 span 里；
  锚点不能是 `queue.enqueue` 的结束时刻（真实竞态会让它变成负数），
  而是"worker 开始之前、最后一个结束的非 worker 侧 span"

另外：`/metrics` 与 `/healthz` 被中间件排除——Prometheus 每 15s 抓一次，
一天 5760 条 trace 会把业务 trace 全挤出环形缓冲（监控接口把监控数据淹掉）。

**离线可端到端验证**：

```bash
python .bench/p212-trace-e2e.py     # 55/55 断言；证据写入 .bench/p212-trace-evidence.txt
```

**开销**（`go test -run '^$' -bench . -benchmem ./internal/observability/tracing/`）：
每个 span 约 **+1.8µs**（关闭态 noop 基线 111.5ns/op，开启态 1896ns/op）；
一个 agentic 任务 14 个 span ≈ 27µs，占该任务 40.9ms 的 **0.07%~0.21%**。
**默认关闭（`OTEL_ENABLED=false`）的理由是"不改变 P0-0/P0-4 基线数字的含义"，不是开销不可接受。**

## 三、技术栈

| 层 | 技术 |
|---|---|
| 前端 | React · TypeScript · Vite · TailwindCSS · Zustand · React Flow |
| 后端 | Go · Gin · pgx · go-redis · JWT · SSE |
| 存储 | PostgreSQL 16 · **pgvector 0.8+（HNSW）** · Redis 7 |
| AI | LLM Gateway · RAG · Tool Calling · Ollama |
| 工程 | Docker · Docker Compose · GitHub Actions · Prometheus · Grafana |

## 四、目录结构

```text
NebulaFlow
├── cmd
│   ├── server/          # 服务入口（装配全部模块）
│   └── seed/            # 演示数据初始化
├── internal
│   ├── api/             # 路由 / 中间件 / handlers（含 SSE）
│   ├── auth/            # JWT + bcrypt
│   ├── cache/           # Redis 缓存抽象
│   ├── config/          # 环境配置（含 StorageMode）
│   ├── database/        # pgx 连接 + 内嵌迁移
│   ├── llm/             # LLM Gateway + Provider + Embedding
│   ├── model/           # 领域模型
│   ├── observability/   # Prometheus 指标
│   ├── queue/           # Redis Stream / 内存队列
│   ├── rag/             # 分块 / 向量化 / 检索
│   ├── ratelimit/       # 固定窗口限流（Redis / 内存双实现）
│   ├── scheduler/       # ★ DAG 调度引擎
│   ├── store/           # 仓储接口 + PostgreSQL 实现 + 内存实现
│   ├── task/            # 任务服务 + SSE Hub
│   ├── tool/            # Calculator / HTTP / Time / CodeRunner
│   ├── worker/          # ★ Worker Pool
│   └── workflow/        # 工作流 CRUD 服务
├── web/                 # React 前端
├── deploy/              # Prometheus / Grafana provisioning
├── benchmark/           # 自研压测工具（loadgen 客户端 + matrix 编排器
│                        #   + ragbench 检索基准 + streamstat 队列水位 + 实测报告）
├── migrations/          # 见 internal/database/migrations
├── docker-compose.yml
├── Dockerfile
└── .github/workflows/ci.yml
```

## 五、快速开始

### Docker Compose（推荐，全栈一键）

```bash
export JWT_SECRET=$(openssl rand -hex 32)   # 生产形态必填，见下方「启动校验」
docker compose up -d --build
# 服务：app:8080 · postgres:5432 · redis:6379 · prometheus:9090 · grafana:3000(admin/admin)
# postgres 镜像用 pgvector/pgvector:pg16（官方 postgres 镜像不含 vector 扩展）
```

> 国内网络拉取基础镜像较慢时，可在 `~/.docker/daemon.json` 配置镜像加速：
> `"registry-mirrors": ["https://docker.m.daocloud.io", "https://hub-mirror.c.163.com"]`，然后重启 Docker Desktop。

**前端已内置在镜像中**：后端单端口托管 `web/dist`，浏览器直接访问 `http://localhost:8080` 即可使用完整前端（Workflow 编辑器 / Dashboard / 任务监控 / 知识库 / 模型页），无需额外启动前端服务。

### 启动校验（P0-3）

服务在建立任何连接之前先做一次**静态配置校验**（`internal/config` 的 `Validate`）：
不连数据库、不连 Redis，只看环境变量本身。命中**致命项**就打印原因并退出（退出码 1）。

| 检查项 | 非 dev 环境 | dev 环境 |
| --- | --- | --- |
| `JWT_SECRET` 为空 / 公开占位值 / < 32 字节 / 单字符重复 | **致命** | 提醒 |
| `STORAGE_MODE=memory`（重启丢全部数据） | **致命** | 不检查 |
| `QUEUE_ADMIT_RATIO < 0`（关闭背压，会静默丢任务） | **致命** | 不检查 |
| `USE_REDIS_QUEUE=false`（重启丢在途任务） | 提醒 | 不检查 |
| `RATE_LIMIT_PER_MIN` / `TASK_RATE_LIMIT_PER_MIN` ≤ 0（关闭限流） | 提醒 | 不检查 |
| `CORS_ORIGINS` 为 `*` | 提醒 | 不检查 |
| `APP_ENV` 取值不在 `{dev, prod, staging}` | 提醒 | — |

三个刻意的设计选择：

1. **严格模式用 `APP_ENV != "dev"` 判定，而不是 `== "prod"`。**
   这是 fail-safe：`APP_ENV` 拼错（`Development`、`PROD`、`prod `）时会落入**严格**分支。
   宽松分支判错的代价是"带着一个公开密钥上线"，严格分支判错的代价只是"启动被拒 + 一条说明该怎么改的日志"——后者便宜得多。
2. **只有真正无法安全运行的配置才致命，其余一律降级为提醒。**
   如果一律拒绝启动，运维会习惯性绕过校验，校验就只剩心理安慰作用了。
   同理，`CORS_ORIGINS=*` 只是提醒而非致命：认证走 `Authorization` 头而不是 Cookie，
   浏览器不会自动携带凭据，`*` 不构成 CSRF 式的凭据暴露，实际代价只是攻击面变大。
3. **"占位值"按子串匹配，而不是相等。**
   `change-me-in-production` 只有 24 字节，长度规则恰好能兜住它；
   但 `change-me-in-production-abcdefgh` 有 33 字节，**只有子串规则能拦住**。
   这一点是变异测试发现的——原先的用例里只有前者，把整张占位名单删掉测试依然全绿。

部署前可以干跑一遍，不启动服务、不连任何依赖：

```bash
make secret                        # 生成一个可用的密钥
./bin/nebulaflow -check-config     # 或 make check-config
```

### 初始化演示数据

```bash
docker compose exec app nebulaflow-seed   # 或：go run ./cmd/seed
# 演示账号：demo / demo123456
# 幂等：重复执行会复用已有用户 / Provider / 知识库 / 工作流，并自愈 RAG 绑定与向量维度
```

### 本地开发

```bash
# 依赖：Go 1.27+、Node 22+、Docker（起 postgres/redis）
docker compose up -d postgres redis
cp .env.example .env
go run ./cmd/server          # 后端 :8080（web/dist 存在时同端口托管前端）
cd web && npm ci && npm run dev  # 仅需前端热更新时 :5173（/api 代理到 8080）
```

### 零依赖启动（无需 Docker / PostgreSQL / Redis）

想直接跑起来看效果时，用内存模式——存储、队列、限流全部走进程内实现：

```bash
STORAGE_MODE=memory go run ./cmd/server
# 浏览器打开 http://localhost:8080，注册任意账号即可使用全部功能
```

内存实现（`internal/store/memory*.go`）的语义严格对齐 PostgreSQL：领域错误码、分页、
`ON CONFLICT` upsert、级联删除、时间戳 `COALESCE` 行为均一致，因此同一套上层代码
在两种后端下表现相同。数据不持久化，进程退出即清空。

上层通过 `internal/store/interfaces.go` 的 5 个窄接口依赖存储，
`StorageMode` 只影响 `cmd/server/main.go` 的装配分支。

> 内存模式下 `APP_ENV` 必须是 `dev`：`STORAGE_MODE=memory` 在非 dev 环境是**致命配置**，
> 服务会在建立任何连接之前拒绝启动（见"启动校验"一节）。

### 测试与压测

```bash
go test ./...                # DAG / Worker / Fallback / RAG / Tool / 存储语义 单元测试
go vet ./...

# 压测：提交吞吐 + 任务落地的端到端时延，一次跑出 Workers 矩阵
go build -o bin/nebulaflow ./cmd/server
go build -o bin/loadgen    ./benchmark/loadgen
go build -o bin/matrix     ./benchmark/matrix
./bin/matrix -workers 1,5,10,20 -n 1200 -c 50 -warmup 100

# 向量检索基准：旧路径（全量加载 + 应用层余弦） vs 新路径（HNSW 索引下推）
go build -o bin/ragbench   ./benchmark/ragbench
./bin/ragbench -dsn "$DSN" -chunks 50000 -dim 384 -queries 30 -shards 20 -iterative all

# 队列水位观测：自检近似裁剪是否生效 / 压测期间连续采样 / 对照实验前归零
go build -o bin/streamstat ./benchmark/streamstat
./bin/streamstat -addr 127.0.0.1:6379 -selftest
./bin/streamstat -addr 127.0.0.1:6379 -watch -interval 500ms -out samples.txt

# 背压对照实验：同一二进制只改 QUEUE_ADMIT_RATIO（-1 = 关闭准入）
bash .bench/p05-run.sh F1-off 10000 -1  20000 60s   # 复现「静默丢任务」
bash .bench/p05-run.sh F2-on  10000 0.8 20000 60s   # 验证背压

# Agent 自主工具调用的端到端验证：内存模式起真实服务端，实验组 + 对照组，15 项断言
# （含 4 项装置自检，用来排除"对照组也这样"）→ 证据写入 .bench/p211-agent-evidence.txt
python .bench/p211-agent-e2e.py

# 变异测试：向源码注入缺陷、跑应该抓住它的那个测试、字节级还原，22 组全部被抓住
python ~/.workbuddy-ai/skills/mutation-test-verify/scripts/mutate.py --spec .bench/mutations-p211.json

# 全链路 Trace 的端到端验证：内存模式零外部依赖，55 项断言
# 头号断言是 GET /api/tasks/:id/trace 的 trace_id == POST /api/tasks 响应头 X-Trace-Id
# （traceparent 没进队列消息 / 没被消费者还原，worker 就会自起一条新链，断言立刻失败）
python .bench/p212-trace-e2e.py
python ~/.workbuddy-ai/skills/mutation-test-verify/scripts/mutate.py --spec .bench/mutations-p212.json

# Trace 的开销基准（微基准：进程内 span 生命周期 / 传播编解码 / 查询侧聚合）
go test -run '^$' -bench . -benchmem ./internal/observability/tracing/

# 需要真实 PostgreSQL + pgvector 的集成回归测试（默认跳过）
NEBULA_TEST_DSN="$DSN" go test ./internal/store -run MultiTenant -v
NEBULA_TEST_REDIS_ADDR=127.0.0.1:6379 go test ./internal/queue -run RedisStream -v

# 需要真实 Redis 且**虚拟时钟必须推进**的测试（限流窗口翻页 / TTL 不续期）
# 本机 Redis 是 miniredis 包装（Windows 无 Redis 6.2+ 原生构建），不推进时钟则 TTL 永不过期：
# 另起一个带 -tick 1s 的实例，不要动 6379 那个
cd ../nebula-infra/redis-srv && ./redis-srv.exe -addr 127.0.0.1:6399 -tick 1s
NEBULA_TEST_REDIS_ADDR=127.0.0.1:6399 go test ./internal/ratelimit -run Redis -v
```

完整方法论、环境说明与瓶颈分析见 **[benchmark/README.md](benchmark/README.md)**。

#### 实测结果（内存存储 + Redis Stream 队列，5 节点 DAG）

**消费循环并发化之后**（`CONSUMER_COUNT` 跟随 `WORKER_COUNT`）：

| workers | 提交吞吐 (req/s) | 成功率 | 任务吞吐 (tasks/s) | 扩展比 | 端到端 p50 (ms) | 端到端 p95 (ms) |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 15658.5 | 100% | 24.0 | 1.00× | 27068 | 51470 |
| 5 | 16063.5 | 100% | 119.2 | 4.97× | 5361 | 10187 |
| 10 | 16219.1 | 100% | 242.6 | 10.11× | 2651 | 5071 |
| 20 | 15366.9 | 100% | **464.9** | **19.37×** | 1322 | **2492** |

每档位 1300 个任务，8 次运行全部 100% 落地、0 失败。

#### 压测发现并修复的核心瓶颈：调度器串行消费

首轮压测（修复前）发现任务吞吐**精确恒定在 `24.0 tasks/s`，与 `WORKER_COUNT` 完全无关**——四个档位数值一模一样，说明是结构性问题而非噪声。

根因是 `scheduler.Run` 为**单 goroutine 串行消费**：一次 `Dequeue` 一条，`executeTask` 同步跑完整个 DAG 才取第二条。`WorkerPool` 的并发能力只作用在「单个任务内部的节点」上，而这条 DAG 是一条链，任一时刻只有一个节点就绪，于是节点级并发也帮不上忙。提交入队约 15,600 req/s、执行 24 tasks/s，**相差约 650 倍**，突发流量下端到端 p95 堆到约 51s。

**修复**：`Run` 改为起 N 个消费者 goroutine，各自以独立 consumer name 共享同一 Redis Stream 消费组（竞争消费）；新增 `CONSUMER_COUNT` 与 `WORKER_COUNT` 两个解耦旋钮。并发安全已核实——`executeTask` 的可变状态除 `s.mu` 保护的 map 与原子计数器外全部是函数局部变量。

**效果**：任务吞吐近似线性扩展（4.97× / 10.11× / 19.37×），端到端 p95 从 51.5s 降到 2.5s，提交路径不受影响。验证方式是**受控对照实验**：同一个二进制，只把 `CONSUMER_COUNT` 钉死为 1 就精确复现出修复前的 24.0 tasks/s。

> 附：`benchmark/submit.lua` 是早期的 wrk 脚本，仅能压提交接口且 Windows 上无 wrk 可用，已被 `benchmark/loadgen` + `benchmark/matrix` 取代。

#### 向量检索下推 pgvector（P0-1，真实 PostgreSQL 16 + pgvector 0.8.6）

50000 分块 × 384 维 / 197MB 表 / Top-K=5 / 30 次查询：

| 场景 | 路径 | 单次耗时 | 回传字节 | 不足k次数 |
| --- | --- | ---: | ---: | ---: |
| 单租户 | 旧：全量加载 + 应用层余弦 | 2099.8ms | 202.8 MB | 0 |
| 单租户 | 新：HNSW 索引下推 | **2.55ms** | **390 B** | 0 |
| 多租户（20 库，目标库占 5%） | 旧 | 85.5ms | 10.1 MB | 0 |
| 多租户 | 新（默认配置） | 3.95ms | 244 B | **25/30** |
| 多租户 | 新 + `hnsw.iterative_scan=strict_order` | 6.26ms | 391 B | **0** |

单租户提速 **824×**、回传数据量降 **54.5 万倍**。多租户那一行是过程中实测抓出的
**静默召回缺陷**：HNSW 索引是全局的，而查询按 `knowledge_base_id` 过滤，默认
`hnsw.iterative_scan=off` 时索引扫描只吐 `ef_search`(=40) 行候选就结束，被过滤后
**返回不足 k 条**（30 次里 25 次不足，平均只拿到 3.1 条）。修复方式是
`SET LOCAL hnsw.iterative_scan = strict_order`，并配了走真实代码路径的集成回归测试。

单租户下三种扫描模式耗时完全一致（2.42 / 2.43 / 2.55ms）——**没有过滤时迭代扫描不触发，
修复零开销**。完整方法论、测量陷阱（高维随机数据上 recall@k 是噪声）与遗留项见
**[benchmark/README.md](benchmark/README.md)** 第 5 节。

#### Redis Stream 容量治理（P0-2）：一次被压测推翻的修复

根因是 `XACK` **只把消息从 PEL 移除、不删消息本体**，而全项目没有任何 `MAXLEN` / `XDEL` / `XTRIM`，
stream 只进不出。第一版修复给所有 `XAdd` 加上 `MAXLEN ~`——看起来干净利落，但压测把它推翻了。

**受控对照实验**（同一二进制，只改 `QUEUE_MAXLEN`；内存存储 + Redis Stream，n=20000、c=50，
提交阶段约 1.1 秒打完，消费者约 385 tasks/s）：

| 组 | `QUEUE_MAXLEN` | XLEN 稳态 | 落地率 | 未完成 |
| --- | ---: | ---: | ---: | ---: |
| A（≈无上限，修复前形态） | 100000000 | 20200（**不回落**） | 100% | 0 |
| B | 1000 | 1000（钉死） | **7.63%** | 18658 |
| C | 10000（默认值） | 10000（钉死） | **52.28%** | 9640 |

落地数与 `MaxLen` 的关系极其整齐（`B: 1542 ≈ 1000 + 542`、`C: 10560 ≈ 10000 + 560`），
差额**全部**是「还没来得及被任何消费者读走就被裁掉」的消息：`MAXLEN` 裁的是最老条目，
而那正是最可能还没被投递的那批。任务永久停在 `pending`，且 Redis 侧不留痕迹——
`XAUTOCLAIM` 会把已删除的 PEL 条目当成「已删除」静默移出 PEL，连一条错误日志都没有。
**单靠 `MAXLEN`，等于把「内存泄漏」换成了「静默丢任务」。**

**第二版修复**改删「已确认前缀」而不是「最老条目」：`XINFO GROUPS` 的 `last-delivered-id`
（≤ 它的都已投递）与 `XPENDING` 的 `Lower`（PEL 最小 ID，小于它的都已 ACK）取小，
删这个位点之前的条目不会丢任何任务。调度器每 10 秒跑一轮，XLEN 从 20200 阶梯式降到 **0**
（阶梯间隔正好是维护周期、每级约 3800 条），而 A 组永远停在 20200 不动。

`MAXLEN` 保留为「消费端整体停摆」时的兜底，但护栏必须可见：新增 6 个指标
（`nebulaflow_queue_stream_len` / `_max_len` / `_admit_limit` / `_dlq_len` /
`_reclaimed_entries_total` / `_rejected_total`）与 90% 水位 WARN，
实测在 `stream_len=10000 max_len=10000` 时精确触发一次。

### 提交侧背压（P0-5）：把静默丢弃换成明确拒绝

第 6 节的遗留就是这一项：突发场景下 `MAXLEN` 必然先触发，周期回收救不了——
生产端 1 秒推 2 万条，任何 10 秒粒度的回收都追不上。

**受控对照**（同一二进制，只改 `QUEUE_ADMIT_RATIO`；`QUEUE_MAXLEN=10000`，n=20000、c=50）：

| | F1 准入关（`-1`） | F2 准入开（`0.8`） |
|---|---:|---:|
| `201` / `503` | 20000 / 0 | 7954 / **12046** |
| `rejected_total` | 0 | **12046** |
| XLEN 峰值 | 10000（钉死） | **8004** |
| 饱和告警 | 1 次 | **0 次** |
| 落地率 | 52.68% | **100.00%** |
| 卡在 `pending` | **9488** | **0** |

F1 精确复现缺陷：**20000 个请求全拿到 201，其中 9488 个任务永远停在 `pending`**——
队列里查不到、DLQ 里也没有，`rejected_total=0` 说明系统当时**完全没有感知**。

两个设计要点，都是被实测逼出来的：

1. **准入预检放在落库之前**，而不是「插入后回滚/标记失败」。否则每个被拒绝的请求
   仍要付一次 INSERT + UPDATE——过载时拒绝得越多，往数据库压的写越多，
   而数据库恰恰是背压要保护的那一环。F2 的 12046 次拒绝里，12045 次的开销降到一次 `XLEN`。
2. **`Enqueue` 内部的检查不能删**：预检与实际入队之间有 TOCTOU 窗口，
   实测撞上 1 次（约 0.008%）。预检是优化，它才是正确性保证。

账目必须能对上，这是「没有幽灵任务」的硬证据：
`201 数 7954 + 预热 50 + 慢路径 1 = 8005 = 落地 8004 + failed 1`。

完整数据与复现命令见 **[benchmark/README.md](benchmark/README.md)** 第 6、7 节。

#### 连接池与工作流缓存（P0-4 / P1-5）：一个被实测否定的优化假设

**起因**是任务清单里的推断：「20 个 worker 抢 4 条连接必然排队，表现为 worker 都在跑但吞吐上不去」。
修复前 `pgxpool` 的 6 个参数**一个都没配**，用的是默认值——而默认 `MaxConns` 是
`max(4, NumCPU)`，**会随部署机器的核数变化**（实测本机 32 核时是 32），且与 `WORKER_COUNT`
毫无关系；`MinConns=0` 让池会缩到 0（冷启动要现付握手）；`ConnectTimeout=0` 实测语义是
**没有超时**，PG 不可达时靠 OS 的 TCP 重传兜底（约 2 分钟），表现为请求长时间挂住。

**修复**：6 个参数全部显式化为环境变量，加启动校验（`MaxConns < WORKER_COUNT` 等 5 类问题会打 WARN），
并新增 8 个池指标。同时把 `internal/cache/` 从死代码接上热路径——`CachedWorkflows` 装饰器包住
`WorkflowStore`，`GetWorkflow`（一次调用含 3 条查询，且每个任务执行都要读一次）走缓存，
键含 userID（防越权）、写路径先落库后失效（防旧值回灌）、缓存故障降级为「变慢不变坏」。

**受控对照链**（每次只改一个变量，n=4000，池/缓存都只有一处差异）：

| 组 | 池 | 缓存 | 任务吞吐 (tasks/s) | `empty_acquire_total` |
| --- | ---: | --- | ---: | ---: |
| G1（3 轮） | 4 | 关 | 326.3 / 331.4 / 320.4 | 94523 / 96187 / 97790 |
| G4 | 8 | 关 | **330.6** | 77816 |
| G5 | 8 | 30s | 326.3 | 73648 |
| G2（5 轮） | 25 | 关 | 不可用（见下） | 3723 ~ 4183 |

**结论是「吞吐没有变化」**：池翻倍（4→8）差 −0.2%，开缓存差 −1.3%，都在噪声带内
（G1 三次独立运行的极差本身就有 3.4%）。但这一轮交付了三样更有价值的东西：

1. **把「池该配多大」变成可观测**：池 4 时每轮约 **9.6 万次**等待（等待率≈100%，饱和告警每轮都响），
   池 25 时降到约 **3900 次**（≈5%）。这个计数器**对环境噪声不敏感**——池 25 的 5 轮跨越了
   从 9 次到 98 次环境干扰，读数稳定在 3723~4183。
2. **一个能精确对上的账目**：缓存命中 **4095** 次 ↔ 连接获取减少 **4168** 次。
   顺带实测出一个光看代码看不出的细节：`GetWorkflow` 的 3 条查询**复用同一条连接**，
   所以每次缓存命中恰好省掉 **1 次**连接获取，而不是 3 次。
3. **一个被数据否定的假设**：池在 100% 等待率下都不影响吞吐，说明临界路径是**节点执行**
   而不是数据库。所以下一步不该继续加池，而该减少数据库往返或降低数据库并发度。

**为什么没有池 25 的吞吐数字**：池 ≥ 25 会**稳定触发本机 OS 层对 `postgres.exe` 的文件访问拒绝**
（池 4 的 3 轮、池 8 的 2 轮全部通过门控，池 25 的 8 轮全部失败）。本轮排除了沙箱、非 ASCII 路径、
单个坏文件、残留进程占句柄、空闲时也失败等假设，判定为环境限制。
那些被污染的数字（238.7~298.5 tasks/s）**一律不报**——拿它们和池 4 的 331.4 对比会得出
完全错误的结论。

完整数据、装置自检的 5 道断言、环境故障的诊断过程见 **[benchmark/README.md](benchmark/README.md)** 第 8 节。

> 这一轮最值得讲的是方法论：前几版脚本跑出的数字「看起来完全正常」，实际却属于**另一组配置**——
> 根因是 `taskkill //F` 在 Git Bash 下静默失效导致旧服务端存活，`/metrics` 和压测请求
> 都打到了旧进程上。现在有 5 道断言兜底（端口空闲、进程唯一、配置生效、缓存开关自洽、冷启动可见），
> 任何一道不过整轮作废。


## 六、REST API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /api/auth/register · /login | 注册 / 登录（JWT） |
| GET/POST | /api/workflows | 工作流列表 / 创建（含 DAG 校验） |
| GET/PUT/DELETE | /api/workflows/:id | 工作流详情 / 更新 / 删除 |
| POST | /api/tasks | 创建任务并入队（队列饱和时 **503** + `Retry-After`） |
| GET | /api/tasks | 任务列表 |
| GET | /api/tasks/:id | 任务详情（含节点执行明细） |
| GET | /api/tasks/:id/stream | **SSE 实时执行流** |
| POST | /api/tasks/:id/cancel | 取消任务 |
| GET | /api/tasks/:id/logs | 节点日志 |
| POST | /api/knowledge-bases | 创建知识库 |
| POST | /api/knowledge-bases/:id/documents | 上传文档并索引（RAG） |
| GET | /api/knowledge-bases/:id/retrieve | RAG 检索测试 |
| GET/POST/PUT/DELETE | /api/providers | LLM Provider 配置 |
| GET | /api/dashboard/stats | 系统实时指标 |
| GET | /metrics | Prometheus 指标 |

## 七、数据库设计

10 张核心表（`internal/database/migrations/001_init.sql`）：

```text
users ─┬─ workflows ──┬─ workflow_nodes
       │              └─ workflow_edges
       ├─ tasks ──────┬─ task_nodes
       │              └─ task_logs
       ├─ knowledge_bases ── documents ── document_chunks
       ├─ llm_providers ── llm_models
       └─ usage_records
```

迁移 `002_pgvector.sql` 把 `document_chunks.embedding` 从 JSONB 迁到原生向量列
`embedding_v vector(N)`，维度由 `EMBED_DIM` 在启动时收敛（`EnsureVectorSchema`）——
Embedder 维度可配置，SQL 里写死维度没法参数化。启动时还会核对配置维度与库中维度是否一致，
不一致直接拒绝启动，避免"写入 384 维、索引 768 维"这类静默错配。

```sql
CREATE INDEX idx_chunks_embedding_hnsw
    ON document_chunks USING hnsw (embedding_v vector_cosine_ops);
```

注意两点：**无维度的 `vector` 列建不了 HNSW 索引**（报 `column does not have dimensions`），
必须先 `ALTER COLUMN TYPE vector(N)`；另外 `vector.control` 没有 `trusted = true`，
所以 `CREATE EXTENSION vector` 需要超级用户。

## 八、演进路线

- [x] ~~pgvector 原生向量索引（替换应用层检索）~~ → HNSW + 检索下推，单次 1.6s → ~10ms（162×）
- [ ] `document_chunks` 按 `knowledge_base_id` 分区（多租户下每个租户独立 HNSW 索引，
      替代"全局索引 + 后置过滤 + 迭代扫描"）
- [x] ~~OpenTelemetry 全链路 Trace（API → Task → Node → LLM/Tool）~~ → W3C traceparent
      **随队列消息跨进程续接**（提交进程与执行进程之间唯一的数据通路）+ 进程内环形缓冲
      （离线可验证）+ 可选 OTLP 导出；9 类 span、3 个查询接口；自耗时用**区间并集**、
      瓶颈取**自耗时最长**、队列等待单独算。端到端 **55/55** 断言、
      变异测试 **25 组 23 抓 2 等价**、开销基准**每 span +1.8µs（占任务时长 0.07%~0.21%）**
- [x] ~~Tool Calling（LLM 自主决定调用工具，第一版为手动编排）~~ → 多轮 Agent 循环 +
      执行前白名单二次校验 + 轮数硬上限；协议层用假 OpenAI 服务端做线上格式一致性测试，
      离线端到端 15/15 断言通过，**22 组变异测试全部被抓住**
- [x] ~~压测矩阵 + Benchmark 报告页~~ → `benchmark/loadgen` + `benchmark/matrix`，实测报告见 [benchmark/README.md](benchmark/README.md)
- [x] ~~调度器消费循环并发化~~ → 任务吞吐 24.0 → 464.9 tasks/s（19.4×）
- [x] ~~Redis Stream 容量治理~~ → 容量上界 + 已确认前缀回收（XLEN 20200 → 0）+ 水位指标与 90% 告警
- [x] ~~提交侧背压~~ → 落库前准入预检 + `503 Retry-After`；实测落地率 52.68% → 100%、`pending` 9488 → 0
- [x] ~~连接池参数显式化 + 启动校验 + 池指标~~ → 6 个参数提为环境变量、5 类配置问题启动告警；
      `empty_acquire_total` 量出池内排队：池 4 约 9.6 万次 → 池 25 约 3900 次
- [x] ~~`internal/cache/` 接线（原为死代码）~~ → `CachedWorkflows` 装饰器接在 `GetWorkflow` 热路径，
      键含 userID、先落库后失效；实测命中 4095 次 ↔ 连接获取减少 4168 次
- [ ] 减少数据库往返（批量更新节点状态 / 在 worker 内串行化 DB 操作）——
      实测证明池不是吞吐瓶颈（池 4→8 吞吐无变化），真正的上限在节点执行
- [ ] 多实例下缓存的跨实例失效（当前是进程内缓存 + TTL 兜底，强一致需 Redis Pub/Sub）
- [ ] 背压的拒绝响应带排队信息（当前只给 `Retry-After: 1`，可改成基于水位的估算值，见任务清单 P0-5 遗留）
- [x] ~~JWT 密钥的启动校验~~ → `config.Validate()` 静态校验（不连依赖）+ `-check-config` 干跑；
      非 dev 环境拒绝空值/公开占位值/<32 字节/单字符重复的密钥，`APP_ENV` 拼错走严格分支（fail-safe）
- [x] ~~`auth` / `ratelimit` 的单元测试~~ → 补齐两个包的测试，并**顺带找出 3 个真实缺陷**：
      重复注册返回 500 而非 409（`auth` 与 `store` 两个同名哨兵错误，`errors.Is` 永不匹配）、
      数据库故障被伪装成「用户名或密码错误」（监控上不出现任何 5xx）、
      `Authorization: bearer <token>` 因大小写敏感被拒（RFC 7235 §2.1）
- [x] ~~分布式锁（防止同一工作流在多实例并发执行）~~ → Redis `SET NX EX` + Lua `DEL`
      （token 校验防误删 / TTL > taskTimeout / 本地短路 / Nack 重投），9 组变异 8 抓 1 等价
- [ ] 多实例横向扩容验证（scheduler 已按 Consumer Group 设计 + 分布式锁已落地，待做跨实例实测）

## License

MIT

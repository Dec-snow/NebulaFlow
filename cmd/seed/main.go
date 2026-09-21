// NebulaFlow 演示数据初始化：
//   - 创建演示用户 demo / demo123456
//   - 写入 mock LLM Provider（离线可用）+ deepseek 占位
//   - 创建演示知识库「岗位JD库」并索引一份岗位要求文档（供 RAG 节点检索）
//   - 创建一个示例工作流（简历分析 Agent：Parser → RAG → Analyst → Writer → Output）
//
// 用法：go run ./cmd/seed
// 幂等：重复执行会复用已存在的用户/Provider/知识库/工作流。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/config"
	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	cfg := config.FromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// seed 是一次性批量写入工具：不需要服务端那么大的池，
	// 但也不该依赖 pgxpool 的默认值（随机器核数变化）。
	// 显式给一个小池 + 短生命周期，跑完就退出。
	db, err := database.Connect(ctx, cfg.DatabaseURL, database.PoolOptions{
		MaxConns:        4,
		MinConns:        1,
		ConnectTimeout:  5 * time.Second,
		MaxConnLifetime: time.Minute,
	})
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	st := store.New(db)

	// 1. 演示用户
	hash, _ := bcrypt.GenerateFromPassword([]byte("demo123456"), bcrypt.DefaultCost)
	u := &model.User{Username: "demo", Email: "demo@nebulaflow.dev", PasswordHash: string(hash)}
	if err := st.Users.CreateUser(ctx, u); err != nil {
		// 用 errors.Is 判断而不是比对错误字符串，避免文案一改就失效
		if errors.Is(err, store.ErrUserExists) {
			// 已存在则取出真实 ID（幂等）
			existing, gerr := st.Users.GetUserByUsername(ctx, u.Username)
			if gerr != nil {
				log.Fatalf("get demo user: %v", gerr)
			}
			u = existing
			log.Printf("demo user exists (id=%d), reuse", u.ID)
		} else {
			log.Fatalf("create user: %v", err)
		}
	} else {
		log.Printf("created user demo (id=%d, password=demo123456)", u.ID)
	}

	// 2. LLM Provider：mock（离线）+ deepseek（可选）
	mock := &model.LLMProvider{Name: "mock", BaseURL: "http://localhost:1", Enabled: true, Priority: 10}
	_ = st.Providers.UpsertProvider(ctx, mock)
	deepseek := &model.LLMProvider{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1",
		APIKey: os.Getenv("DEEPSEEK_API_KEY"), Enabled: os.Getenv("DEEPSEEK_API_KEY") != "", Priority: 20}
	_ = st.Providers.UpsertProvider(ctx, deepseek)
	ollama := &model.LLMProvider{Name: "ollama", BaseURL: os.Getenv("OLLAMA_BASE_URL"),
		Enabled: os.Getenv("OLLAMA_BASE_URL") != "", Priority: 30}
	_ = st.Providers.UpsertProvider(ctx, ollama)
	log.Println("providers seeded: mock / deepseek(optional) / ollama(optional)")

	// 3. 演示知识库「岗位JD库」+ 岗位要求文档（幂等）
	kbID := int64(0)
	kbs, _ := st.Knowledge.ListKBs(ctx, u.ID)
	for _, kb := range kbs {
		if kb.Name == "岗位JD库" {
			kbID = kb.ID
			break
		}
	}
	if kbID == 0 {
		kb := &model.KnowledgeBase{UserID: u.ID, Name: "岗位JD库", Description: "目标岗位 JD 与任职要求，供简历匹配分析"}
		if err := st.Knowledge.CreateKB(ctx, kb); err != nil {
			log.Fatalf("create kb: %v", err)
		}
		kbID = kb.ID
		log.Printf("created knowledge base %q (id=%d)", kb.Name, kb.ID)
	} else {
		log.Printf("knowledge base exists (id=%d), reuse", kbID)
	}

	doc := &model.Document{KnowledgeBaseID: kbID, Filename: "go-backend-jd.md", Status: model.DocIndexed, Content: jdContent}
	docs, _ := st.Knowledge.ListDocuments(ctx, kbID)
	exists := false
	for _, d := range docs {
		if d.Filename == doc.Filename {
			exists = true
			doc.ID = d.ID
			break
		}
	}
	if !exists {
		if err := st.Knowledge.CreateDocument(ctx, doc); err != nil {
			log.Fatalf("create doc: %v", err)
		}
	}
	// 无论新旧文档都重新索引：删除旧 chunk 后重建，保证向量维度与服务端一致（自愈旧数据）
	if err := st.Knowledge.DeleteChunksByDocument(ctx, doc.ID); err != nil {
		log.Fatalf("delete old chunks: %v", err)
	}
	// 与 server 保持一致：NewLocalHashEmbedder(0) → 384 维
	svc := rag.NewService(llm.NewEmbeddingGateway(nil, llm.NewLocalHashEmbedder(0)))
	chunks, err := svc.IndexDocument(ctx, doc)
	if err != nil {
		log.Fatalf("index doc: %v", err)
	}
	// 批量插入分块，减少 DB round-trip
	if len(chunks) > 0 {
		if err := st.Knowledge.BatchInsertChunks(ctx, chunks); err != nil {
			log.Fatalf("insert chunks: %v", err)
		}
	}
	log.Printf("document %q (id=%d) indexed %d chunks (dim=384)", doc.Filename, doc.ID, len(chunks))

	// 4. 示例工作流：简历分析 Agent（幂等：同名已存在则跳过）
	providers, _ := st.Providers.ListProviders(ctx)
	mockID := int64(0)
	for _, p := range providers {
		if p.Name == "mock" {
			mockID = p.ID
		}
	}
	_ = mockID

	existsWF := false
	wfs, _ := st.Workflows.ListWorkflows(ctx, u.ID)
	var oldWF *model.Workflow
	for _, w := range wfs {
		if w.Name == "简历分析 Agent" {
			existsWF = true
			oldWF = &w
			break
		}
	}

	buildWF := func() *model.Workflow {
		return &model.Workflow{
			UserID: u.ID, Name: "简历分析 Agent", Description: "解析简历 → 检索岗位要求知识库 → 分析匹配 → 生成报告",
			Status: model.WorkflowPublished,
			Nodes: []model.WorkflowNode{
				{NodeKey: "parser", NodeType: model.NodeInput, PositionX: 50, PositionY: 100,
					Config: model.NodeConfig{System: "你是简历解析器。"}},
				{NodeKey: "rag", NodeType: model.NodeRAG, PositionX: 50, PositionY: 260,
					Config: model.NodeConfig{KnowledgeBaseID: kbID}},
				{NodeKey: "analyst", NodeType: model.NodeLLM, PositionX: 320, PositionY: 180,
					Config: model.NodeConfig{
						Model: "mock-chat", System: "你是资深招聘分析师。",
						Prompt:   "根据简历与岗位要求资料，分析候选人与岗位的匹配度，给出 3 条优势与 2 条风险。",
						MaxRetry: 2, TimeoutSec: 60,
					}},
				{NodeKey: "writer", NodeType: model.NodeLLM, PositionX: 590, PositionY: 180,
					Config: model.NodeConfig{
						Model: "mock-chat", System: "你是技术写作助手。",
						Prompt:   "把分析结果整理成结构化的面试报告（优势/风险/追问建议）。",
						MaxRetry: 2, TimeoutSec: 60,
					}},
				{NodeKey: "output", NodeType: model.NodeOutput, PositionX: 860, PositionY: 180},
			},
			Edges: []model.WorkflowEdge{
				{SourceNode: "parser", TargetNode: "rag"},
				{SourceNode: "parser", TargetNode: "analyst"},
				{SourceNode: "rag", TargetNode: "analyst"},
				{SourceNode: "analyst", TargetNode: "writer"},
				{SourceNode: "writer", TargetNode: "output"},
			},
		}
	}

	if !existsWF {
		wf := buildWF()
		if err := st.Workflows.CreateWorkflow(ctx, wf); err != nil {
			log.Fatalf("create workflow: %v", err)
		}
		log.Printf("created workflow %q (id=%d)", wf.Name, wf.ID)
		fmt.Printf("\nDemo 登录：demo / demo123456\nWorkflow ID: %d\n", wf.ID)
	} else {
		// 幂等：已存在则检查 rag 节点是否已绑定知识库，未绑定则修复（旧数据迁移）
		detail, err := st.Workflows.GetWorkflow(ctx, oldWF.ID, u.ID)
		if err != nil {
			log.Fatalf("get workflow: %v", err)
		}
		needFix := false
		for _, n := range detail.Nodes {
			if n.NodeType == model.NodeRAG && n.Config.KnowledgeBaseID != kbID {
				needFix = true
				break
			}
		}
		if needFix {
			wf := buildWF()
			wf.ID = detail.ID
			if err := st.Workflows.UpdateWorkflow(ctx, wf); err != nil {
				log.Fatalf("fix workflow rag binding: %v", err)
			}
			log.Printf("workflow %q (id=%d) rag binding fixed -> kb=%d", wf.Name, wf.ID, kbID)
		} else {
			log.Printf("workflow %q exists, skip", "简历分析 Agent")
		}
		fmt.Printf("\nDemo 登录：demo / demo123456\nWorkflow ID: %d\n", detail.ID)
	}
}

// jdContent 是演示用的岗位要求文档，关键词覆盖 Go/DAG/高并发等，
// 便于本地 Embedder（CJK bigram）与用户输入的简历内容做匹配检索。
const jdContent = `# Go 后端工程师岗位要求

## 岗位职责
1. 负责高并发 AI Agent 工作流平台的架构设计与核心模块开发，包括 DAG 任务调度、工作流编排与执行引擎。
2. 设计并实现 Worker Pool 与任务队列，支撑大规模并行任务的高吞吐执行与背压控制。
3. 建设 LLM Gateway 多模型接入层，实现模型故障自动切换（fallback）与统一观测。
4. 参与 RAG 知识库检索链路建设，优化文档分块、向量化与检索召回质量。
5. 负责平台可观测性建设：指标采集（Prometheus）、日志、追踪，保障系统稳定性。
6. 与前端、算法团队协作，通过 SSE 实时推送任务执行进度与模型流式输出。

## 任职要求
1. 计算机相关专业，本科及以上学历，3 年以上后端开发经验。
2. 精通 Go 语言，熟悉 goroutine 并发模型、channel 通信与 context 生命周期管理。
3. 熟悉 Docker、Kubernetes、CI/CD 流水线，有容器化部署与云原生实践经验。
4. 熟练使用 PostgreSQL、Redis，理解事务、索引与缓存一致性。
5. 了解主流 LLM API（OpenAI 兼容协议）与 RAG 技术栈，有实际落地经验者优先。
6. 有任务调度、工作流引擎或事件驱动架构设计经验者优先。

## 加分项
- 熟悉 React / TypeScript 前端开发，能独立完成全栈功能。
- 有 Prometheus / Grafana 监控体系搭建经验。
- 参与过开源项目或有技术博客沉淀。
`

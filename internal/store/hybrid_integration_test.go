// 本文件是**外部测试包**（store_test），因为 internal/rag 反过来 import 了
// internal/store（Indexer 依赖 KnowledgeStore 接口），放在 package store 里会构成
// import cycle。store.New(db) 暴露的 *Store 足够构造真实仓储。
package store_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/rag"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

const hybridTestDim = 384

// TestKnowledgeHybridRetrieve_KeywordPathContributes 证明"关键词那一路"在真实 PG 上确实生效，
// 而且它的结果确实进入了 RRF 融合——而不是被静默降级掉。
//
// 做法是把关键词块做成**向量路结构上不可能返回**的数据：`embedding_v IS NULL`
// （写入侧向量化失败时会留下这种行），而 SearchChunks 的 WHERE 里恒有
// `c.embedding_v IS NOT NULL`。于是：
//
//   - 向量路无论怎么排、无论 HNSW 索引状态如何，都拿不到这一块；
//   - 它出现在 HybridRetrieve 的结果里，只可能来自关键词路。
//
// 为什么不用"比较 RRF 名次"来判定：那依赖向量路的返回集合与顺序，而 HNSW 是全局
// 索引且受 hnsw.max_scan_tuples（默认 20000）+ 死元组影响——实测在同一张表上先跑过
// 5 万分块的多租户用例后，针对小知识库的 `LIMIT 3` 只返回了 2 行。用"存在性"判定
// 就不受这些因素影响，判据也更硬。
//
// 修复前的事实：KeywordSearch 的 SQL 引用了两个不存在的列
// （document_chunks.knowledge_base_id、documents.indexed），在真实 PG 上 100% 报错；
// HybridRetrieve 只在"两路都失败"时才返回 error，于是这个错误被吞掉，
// RRF 退化成纯向量检索 → 这一块永远不出现 → 本用例失败。
func TestKnowledgeHybridRetrieve_KeywordPathContributes(t *testing.T) {
	dsn := os.Getenv("NEBULA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NEBULA_TEST_DSN，跳过需要真实 PostgreSQL 的集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db, err := database.Connect(ctx, dsn, database.PoolOptions{})
	if err != nil {
		t.Fatalf("连接 PostgreSQL: %v", err)
	}
	defer db.Pool.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	if err := db.EnsureVectorSchema(ctx, hybridTestDim); err != nil {
		t.Fatalf("准备向量 schema: %v", err)
	}

	st := store.New(db)

	var userID int64
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO users (username, email, password_hash)
		VALUES ('store-int-hybrid', 'store-int-hybrid@example.invalid', 'x')
		ON CONFLICT (username) DO UPDATE SET updated_at = now()
		RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("准备测试用户: %v", err)
	}

	kb := &model.KnowledgeBase{UserID: userID, Name: "store-int-hybrid", Description: "混合检索集成测试"}
	if err := st.Knowledge.CreateKB(ctx, kb); err != nil {
		t.Fatalf("CreateKB: %v", err)
	}
	defer func() {
		if _, err := db.Pool.Exec(context.Background(),
			`DELETE FROM knowledge_bases WHERE id=$1`, kb.ID); err != nil {
			t.Logf("清理测试知识库 %d 失败: %v", kb.ID, err)
		}
	}()

	doc := &model.Document{
		KnowledgeBaseID: kb.ID, Filename: "hybrid.md",
		Status: model.DocIndexed, Content: "混合检索集成测试文档。",
	}
	if err := st.Knowledge.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	const query = "蓝鲸协议"
	const kwChunkIndex = 1

	// 向量路上的两块：内容里没有关键词，embedding 正常。
	if err := st.Knowledge.BatchInsertChunks(ctx, []model.DocumentChunk{
		{DocumentID: doc.ID, Content: "甲 无关内容 工作流编排 节点执行",
			ChunkIndex: 0, Embedding: basisVec(0)},
		{DocumentID: doc.ID, Content: "丙 无关内容 工具调用 条件分支",
			ChunkIndex: 2, Embedding: basisVec(1)},
	}); err != nil {
		t.Fatalf("BatchInsertChunks: %v", err)
	}
	// 关键词块：embedding_v 显式写 NULL（模拟向量化失败留下的行）。
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO document_chunks (document_id, content, chunk_index, embedding_v)
		 VALUES ($1, $2, $3, NULL)`,
		doc.ID, "乙 关键术语 蓝鲸协议 跨租户召回一致性约束", kwChunkIndex); err != nil {
		t.Fatalf("写入关键词块（embedding_v = NULL）: %v", err)
	}

	embedder := llm.NewEmbeddingGateway(nil, llm.NewLocalHashEmbedder(hybridTestDim))
	svc := rag.NewService(embedder)
	qvecs, err := embedder.Embed(ctx, []string{query})
	if err != nil || len(qvecs) == 0 {
		t.Fatalf("计算查询向量失败: %v", err)
	}

	// ---- 前提检查：向量路结构上拿不到关键词块 ----
	vecHits, err := st.Knowledge.SearchChunks(ctx, kb.ID, qvecs[0], 10)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	for _, h := range vecHits {
		if h.ChunkIndex == kwChunkIndex {
			t.Fatalf("前提不成立：向量路返回了 embedding_v IS NULL 的关键词块 %+v", h)
		}
	}

	// ---- 关键词路必须单独可用 ----
	kwHits, err := st.Knowledge.KeywordSearch(ctx, kb.ID, query, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v\n修复前关键词那一路 100%% 报错（column dc.knowledge_base_id does not exist）", err)
	}
	if len(kwHits) != 1 || kwHits[0].ChunkIndex != kwChunkIndex ||
		kwHits[0].DocumentID != doc.ID || kwHits[0].Filename != "hybrid.md" {
		t.Fatalf("关键词路命中 %d 条（chunk_index=%v），期望恰好命中 document_id=%d chunk_index=%d 且带 filename",
			len(kwHits), chunkIndexes(kwHits), doc.ID, kwChunkIndex)
	}

	// ---- 正式断言：关键词块必须出现在 RRF 融合结果里 ----
	fused, err := svc.HybridRetrieve(ctx, st.Knowledge, st.Knowledge, kb.ID, query, 10, nil, 0)
	if err != nil {
		t.Fatalf("HybridRetrieve: %v", err)
	}
	found := false
	for _, h := range fused {
		if h.DocumentID == doc.ID && h.ChunkIndex == kwChunkIndex {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RRF 融合结果里没有关键词块（document_id=%d chunk_index=%d，共 %d 条）。\n"+
			"向量路结构上不可能返回它（embedding_v IS NULL），所以它只能来自关键词路——\n"+
			"说明关键词路的结果没有进入融合：修复前 KeywordSearch 报错被吞，"+
			"HybridRetrieve 退化成纯向量检索", doc.ID, kwChunkIndex, len(fused))
	}
}

// TestKnowledgeHybridRetrieve_SinglePathFailureIsObservable 覆盖"降级必须可观测"这条要求，
// 不需要数据库：直接给两路塞桩实现。
//
// 两件事必须同时成立：
//  1. 单路失败**不改变返回语义**——另一路有结果就照常返回，不报错（保持原 API 行为）；
//  2. 这条降级必须在日志里留下 WARN，并带上原始错误——修复前是完全静默的。
func TestKnowledgeHybridRetrieve_SinglePathFailureIsObservable(t *testing.T) {
	svc := rag.NewService(llm.NewEmbeddingGateway(nil, llm.NewLocalHashEmbedder(hybridTestDim)))

	vecOK := stubVecSearcher{hits: []model.DocumentChunk{
		{ID: 1, DocumentID: 7, ChunkIndex: 0, Content: "向量路命中", Score: 0.9, Filename: "a.md"},
	}}
	vecFail := stubVecSearcher{err: errors.New("vector path down")}
	kwOK := stubKeywordSearcher{hits: []model.DocumentChunk{
		{ID: 2, DocumentID: 8, ChunkIndex: 0, Content: "关键词路命中", Score: 0.8, Filename: "b.md"},
	}}
	kwFail := stubKeywordSearcher{err: errors.New("column dc.knowledge_base_id does not exist")}

	cases := []struct {
		name     string
		vec      stubVecSearcher
		kw       stubKeywordSearcher
		wantDoc  int64
		wantWarn string
	}{
		{"关键词路失败（本次修复的缺陷场景）", vecOK, kwFail, 7, "关键词路失败"},
		{"向量路失败", vecFail, kwOK, 8, "向量路失败"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(old)

			hits, err := svc.HybridRetrieve(context.Background(), c.vec, c.kw, 42, "蓝鲸协议", 3, nil, 0)
			if err != nil {
				t.Fatalf("单路失败不应让整个检索失败（降级语义不能变）: %v", err)
			}
			if len(hits) != 1 || hits[0].DocumentID != c.wantDoc {
				t.Fatalf("失败那一路之外的命中应被保留，实际得到 %+v", hits)
			}

			logged := buf.String()
			if !strings.Contains(logged, c.wantWarn) {
				t.Fatalf("降级没有留下 WARN 日志：期望包含 %q，实际日志为 %q", c.wantWarn, logged)
			}
			if !strings.Contains(logged, "level=WARN") {
				t.Fatalf("降级日志不是 WARN 级别: %q", logged)
			}
		})
	}
}

// basisVec 返回 384 维的第 i 个单位基向量（确定性，不依赖随机数）。
func basisVec(i int) []float64 {
	v := make([]float64, hybridTestDim)
	v[i%hybridTestDim] = 1
	return v
}

// ---------- 桩实现 ----------

type stubVecSearcher struct {
	hits []model.DocumentChunk
	err  error
}

func (s stubVecSearcher) SearchChunks(_ context.Context, _ int64, _ []float64, _ int) ([]model.DocumentChunk, error) {
	return s.hits, s.err
}

type stubKeywordSearcher struct {
	hits []model.DocumentChunk
	err  error
}

func (s stubKeywordSearcher) KeywordSearch(_ context.Context, _ int64, _ string, _ int) ([]model.DocumentChunk, error) {
	return s.hits, s.err
}

func chunkIndexes(chunks []model.DocumentChunk) []int {
	out := make([]int, len(chunks))
	for i, c := range chunks {
		out[i] = c.ChunkIndex
	}
	return out
}

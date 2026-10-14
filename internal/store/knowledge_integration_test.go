package store

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/jackc/pgx/v5"
)

// ---------- PG 集成用例的公共脚手架 ----------
//
// 这一组用例存在的理由：PG 路径长期零覆盖。documents.chunks / size_kb /
// error_message 三列从来没进过 001_init.sql，而 ListDocuments / GetDocument /
// UpdateDocumentIndexResult 一直在 SELECT/UPDATE 它们；KeywordSearch 更是引用了
// document_chunks.knowledge_base_id 与 documents.indexed 两个不存在的列。
// 内存实现（memory_knowledge.go）里这些字段都在，所以单测全绿、只有 PG 路径烂。
//
// 因此下面每个用例都走**真实 KnowledgeRepo / TaskRepo + 真实 PostgreSQL**，
// 断言的是"SQL 能在 PG 上跑通且语义正确"，而不是"代码看起来对"。

// pgTestDB 连接真实 PostgreSQL、执行迁移并把向量列收敛到 dim 维。
// 未设置 NEBULA_TEST_DSN 时 skip，保持与既有用例一致的"默认不跑"语义。
func pgTestDB(t *testing.T, ctx context.Context, dim int) *database.DB {
	t.Helper()
	dsn := os.Getenv("NEBULA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NEBULA_TEST_DSN，跳过需要真实 PostgreSQL 的集成测试")
	}
	db, err := database.Connect(ctx, dsn, database.PoolOptions{})
	if err != nil {
		t.Fatalf("连接 PostgreSQL: %v", err)
	}
	t.Cleanup(db.Pool.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("执行迁移: %v\n"+
			"提示：若失败于 “column does not have dimensions (SQLSTATE 22023)”，"+
			"说明向量列被建成了无维度的 vector；维度的收敛属于 Go 侧 "+
			"database.EnsureVectorSchema 的职责，SQL 迁移文件里不应建 HNSW 索引。"+
			"空库迁移的回归用例见 internal/database/TestMigrateOnBrandNewDatabase", err)
	}
	if err := db.EnsureVectorSchema(ctx, dim); err != nil {
		t.Fatalf("准备向量 schema: %v", err)
	}
	return db
}

// pgTestUserID 准备一个测试用户（幂等复用），返回 id。
func pgTestUserID(t *testing.T, ctx context.Context, db *database.DB, suffix string) int64 {
	t.Helper()
	var id int64
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO users (username, email, password_hash)
		VALUES ($1, $2, 'x')
		ON CONFLICT (username) DO UPDATE SET updated_at = now()
		RETURNING id`,
		"store-int-"+suffix, "store-int-"+suffix+"@example.invalid").Scan(&id); err != nil {
		t.Fatalf("准备测试用户: %v", err)
	}
	return id
}

// pgCleanupKB 在用例结束时删除测试知识库（级联删除文档与分块）。
func pgCleanupKB(t *testing.T, db *database.DB, kbID int64) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := db.Pool.Exec(context.Background(),
			`DELETE FROM knowledge_bases WHERE id=$1`, kbID); err != nil {
			t.Logf("清理测试知识库 %d 失败: %v", kbID, err)
		}
	})
}

// TestKnowledgeDocuments_ListGetUpdateIndexResult 覆盖"PG 模式文档列表/详情 500、
// 索引收尾写不进状态"这条缺陷。
//
// 修复前的事实（真实 PG 16 上复现）：
//
//	GET /documents → 500，服务端日志 column "chunks" does not exist (SQLSTATE 42703)
//	UpdateDocumentIndexResult → 同样报错，文档永远停在 indexing，
//	而 SearchChunks 有 d.status='indexed' 过滤 → 检索永远为空。
func TestKnowledgeDocuments_ListGetUpdateIndexResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgTestDB(t, ctx, 384)
	repo := &KnowledgeRepo{db: db}
	userID := pgTestUserID(t, ctx, db, "documents")

	kb := &model.KnowledgeBase{UserID: userID, Name: "store-int-documents", Description: "集成测试"}
	if err := repo.CreateKB(ctx, kb); err != nil {
		t.Fatalf("CreateKB: %v", err)
	}
	pgCleanupKB(t, db, kb.ID)

	doc := &model.Document{
		KnowledgeBaseID: kb.ID, Filename: "int.md", Status: model.DocPending,
		Content: "集成测试文档内容：工作流编排与向量检索。",
	}
	if err := repo.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	// ---- ListDocuments：修复前直接 500（column "chunks" does not exist）----
	list, err := repo.ListDocuments(ctx, kb.ID)
	if err != nil {
		t.Fatalf("ListDocuments: %v\n"+
			"修复前这里是 column \"chunks\" does not exist（documents 表缺 chunks/size_kb/error_message 三列）", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListDocuments 返回 %d 条，期望 1 条", len(list))
	}
	if got := list[0]; got.ID != doc.ID || got.Filename != "int.md" ||
		got.Status != model.DocPending || got.Chunks != 0 || got.SizeKB != 0 || got.ErrorMessage != "" {
		t.Fatalf("ListDocuments 返回的文档字段不符（新文档应为零值统计）: %+v", got)
	}

	// ---- GetDocument：修复前同样 500 ----
	got, err := repo.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetDocument: %v（修复前同样报 column \"chunks\" does not exist）", err)
	}
	if got.ID != doc.ID || got.Content != doc.Content || got.KnowledgeBaseID != kb.ID {
		t.Fatalf("GetDocument 返回的文档不符: %+v", got)
	}

	// ---- UpdateDocumentIndexResult：修复前写不进，状态永远停在 indexing ----
	if err := repo.UpdateDocumentIndexResult(ctx, doc.ID, model.DocIndexed, 7, 3, ""); err != nil {
		t.Fatalf("UpdateDocumentIndexResult(indexed): %v\n"+
			"修复前这里失败 → 索引收尾写不进状态 → 文档永远 indexing → 检索永远为空", err)
	}
	got, err = repo.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetDocument(indexed 后): %v", err)
	}
	if got.Status != model.DocIndexed || got.Chunks != 7 || got.SizeKB != 3 || got.ErrorMessage != "" {
		t.Fatalf("索引结果未落库：status=%q chunks=%d size_kb=%d error=%q（期望 indexed/7/3/空）",
			got.Status, got.Chunks, got.SizeKB, got.ErrorMessage)
	}

	// ---- failed + error_message 也要能落库（前端靠它显示失败原因）----
	if err := repo.UpdateDocumentIndexResult(ctx, doc.ID, model.DocFailed, 0, 0, "embedding 失败"); err != nil {
		t.Fatalf("UpdateDocumentIndexResult(failed): %v", err)
	}
	got, err = repo.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatalf("GetDocument(failed 后): %v", err)
	}
	if got.Status != model.DocFailed || got.ErrorMessage != "embedding 失败" || got.Chunks != 0 {
		t.Fatalf("失败结果未落库：status=%q chunks=%d error=%q（期望 failed/0/embedding 失败）",
			got.Status, got.Chunks, got.ErrorMessage)
	}
}

// TestKnowledgeKeywordSearch_ReturnsHits 覆盖"关键词检索引用了不存在的列"这条缺陷。
//
// 修复前事实：KeywordSearch 的 SQL 用了 dc.knowledge_base_id 与 d.indexed，
// 真实 PG 上 100% 报错：
//
//	ERROR: column dc.knowledge_base_id does not exist (SQLSTATE 42703)
//
// 后果不是 500 而是静默降级：HybridRetrieve 只在两路都失败时才报错，
// 于是关键词那一路全挂时 RRF 悄悄退化成纯向量检索。
func TestKnowledgeKeywordSearch_ReturnsHits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgTestDB(t, ctx, 384)
	repo := &KnowledgeRepo{db: db}
	userID := pgTestUserID(t, ctx, db, "keyword")

	kb := &model.KnowledgeBase{UserID: userID, Name: "store-int-keyword", Description: "集成测试"}
	if err := repo.CreateKB(ctx, kb); err != nil {
		t.Fatalf("CreateKB: %v", err)
	}
	pgCleanupKB(t, db, kb.ID)

	doc := &model.Document{
		KnowledgeBaseID: kb.ID, Filename: "kw.md", Status: model.DocIndexed,
		Content: "关键词检索集成测试文档。",
	}
	if err := repo.CreateDocument(ctx, doc); err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}

	rng := rand.New(rand.NewSource(11))
	chunks := []model.DocumentChunk{
		{DocumentID: doc.ID, Content: "甲 无关内容 工作流编排", ChunkIndex: 0, Embedding: randVec(rng, 384)},
		{DocumentID: doc.ID, Content: "乙 关键术语 蓝鲸协议 跨租户召回一致性", ChunkIndex: 1, Embedding: randVec(rng, 384)},
		{DocumentID: doc.ID, Content: "丙 无关内容 工具调用", ChunkIndex: 2, Embedding: randVec(rng, 384)},
	}
	if err := repo.BatchInsertChunks(ctx, chunks); err != nil {
		t.Fatalf("BatchInsertChunks: %v", err)
	}

	// ---- 正文：中文关键词必须命中，且只命中含该词的那一块 ----
	hits, err := repo.KeywordSearch(ctx, kb.ID, "蓝鲸协议", 5)
	if err != nil {
		t.Fatalf("KeywordSearch: %v\n"+
			"修复前这里是 column \"dc.knowledge_base_id\" does not exist（关键词那一路 100%% 失败）", err)
	}
	if len(hits) != 1 {
		t.Fatalf("KeywordSearch 命中 %d 条，期望 1 条（只有 chunk_index=1 含「蓝鲸协议」）", len(hits))
	}
	if got := hits[0]; got.ChunkIndex != 1 || !strings.Contains(got.Content, "蓝鲸协议") ||
		got.Filename != "kw.md" || got.DocumentID != doc.ID {
		t.Fatalf("KeywordSearch 命中内容不符: %+v", got)
	}
	if hits[0].Score <= 0 || hits[0].Score > 1.000001 {
		t.Fatalf("归一化后的 Score 越界: %v（应在 (0,1]）", hits[0].Score)
	}

	// ---- 反例：查询库里不存在的词必须返回 0 条（防止"总是全返回"的假绿）----
	none, err := repo.KeywordSearch(ctx, kb.ID, "完全不存在词条ZZZ", 5)
	if err != nil {
		t.Fatalf("KeywordSearch(无命中): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("无命中查询返回了 %d 条，期望 0 条", len(none))
	}

	// ---- 隔离：另一个知识库里的同一关键词不应被检索到 ----
	other := &model.KnowledgeBase{UserID: userID, Name: "store-int-keyword-other", Description: "隔离性对照"}
	if err := repo.CreateKB(ctx, other); err != nil {
		t.Fatalf("CreateKB(other): %v", err)
	}
	pgCleanupKB(t, db, other.ID)
	otherDoc := &model.Document{
		KnowledgeBaseID: other.ID, Filename: "other.md", Status: model.DocIndexed,
		Content: "对照文档。",
	}
	if err := repo.CreateDocument(ctx, otherDoc); err != nil {
		t.Fatalf("CreateDocument(other): %v", err)
	}
	if err := repo.BatchInsertChunks(ctx, []model.DocumentChunk{
		{DocumentID: otherDoc.ID, Content: "丁 关键术语 蓝鲸协议 出现在另一个知识库", ChunkIndex: 0, Embedding: randVec(rng, 384)},
	}); err != nil {
		t.Fatalf("BatchInsertChunks(other): %v", err)
	}
	hits, err = repo.KeywordSearch(ctx, kb.ID, "蓝鲸协议", 5)
	if err != nil {
		t.Fatalf("KeywordSearch(隔离性): %v", err)
	}
	if len(hits) != 1 || hits[0].DocumentID != doc.ID {
		t.Fatalf("跨知识库隔离失效：命中 %d 条，第一条 document_id=%d（期望仅本库文档 %d）",
			len(hits), hits[0].DocumentID, doc.ID)
	}
}

// TestKnowledgeKeywordSearch_FailureIsLogged 覆盖缺陷 2 的第二半：关键词那一路失败必须可观测。
//
// 修复前它是"静默失败"——HybridRetrieve 只在两路都失败时才报错，所以关键词路
// 100% 报错时，RRF 会悄悄退化成纯向量检索，日志里一个字都没有。
// 这里不用 mock：直接给一个已取消的 context，让真实的 PG 查询返回
// context.Canceled，然后断言存储层留下了 WARN 日志并带上原始错误。
func TestKnowledgeKeywordSearch_FailureIsLogged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db := pgTestDB(t, ctx, 384)
	repo := &KnowledgeRepo{db: db}
	userID := pgTestUserID(t, ctx, db, "keyword-log")

	kb := &model.KnowledgeBase{UserID: userID, Name: "store-int-keyword-log", Description: "可观测性"}
	if err := repo.CreateKB(ctx, kb); err != nil {
		t.Fatalf("CreateKB: %v", err)
	}
	pgCleanupKB(t, db, kb.ID)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	cancelled, cancelQuery := context.WithCancel(ctx)
	cancelQuery()

	_, err := repo.KeywordSearch(cancelled, kb.ID, "蓝鲸协议", 5)
	if err == nil {
		t.Fatal("已取消的 context 竟然没有让 KeywordSearch 失败")
	}
	logged := buf.String()
	if !strings.Contains(logged, "keyword search failed") {
		t.Fatalf("关键词检索失败没有留下日志: %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("日志级别不是 WARN: %q", logged)
	}
	if !strings.Contains(logged, "kb_id=") {
		t.Fatalf("日志里应带上知识库 id 便于定位: %q", logged)
	}
}

// TestSearchChunks_MultiTenantReturnsK 覆盖一个单元测试覆盖不到、但实测确实踩到过的缺陷。
//
// 背景：pgvector 的 HNSW 索引是**全局**的，而检索要按 knowledge_base_id 过滤。
// pgvector 默认 hnsw.iterative_scan=off 时，索引扫描最多只吐 hnsw.ef_search
// （默认 40）行候选就结束了；这些候选绝大多数属于别的知识库、会被过滤掉，
// 于是查询返回的行数**不足 k 条**。实测（benchmark/ragbench -shards 20，5 万分块 /
// 20 个知识库）30 次查询里 29 次不足 k，平均只返回 1~3 条。
//
// 这个用例造出「一堆噪声知识库 + 一个只占 1/shards 的目标知识库」，
// 然后走**真实的 KnowledgeRepo.SearchChunks**，断言能拿满 k 条。
//
// 需要真实 PostgreSQL + pgvector，默认跳过：
//
//	NEBULA_TEST_DSN="postgres://nebula:nebula@127.0.0.1:5432/nebulaflow?sslmode=disable" \
//	  go test ./internal/store -run MultiTenant -v
//
// 规模可用 NEBULA_TEST_RAG_ROWS（默认 50000）与 NEBULA_TEST_RAG_SHARDS（默认 20）调整。
// 默认值是实测标定出来的：2 万分块 / 20 库时规划器会放弃 HNSW、改走
// idx_chunks_doc + Sort（此时用例会因「前提不成立」而失败，而不是静默通过）；
// 5 万分块 / 20 库时才会稳定选中 HNSW。整个用例约 3 分钟，因此默认跳过。
//
// 关于「前提检查」：分块数不够时规划器会放弃 HNSW，那样用例就失去意义了。
// 所以用例会先 EXPLAIN 一次，确认计划里确实有 hnsw，否则直接失败——
// 让用例「空过」比让它失败更糟。
func TestSearchChunks_MultiTenantReturnsK(t *testing.T) {
	dsn := os.Getenv("NEBULA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NEBULA_TEST_DSN，跳过需要真实 pgvector 的集成测试")
	}

	const dim = 384
	const k = 5
	rows := envIntOr("NEBULA_TEST_RAG_ROWS", 50000)
	shards := envIntOr("NEBULA_TEST_RAG_SHARDS", 20)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// 零值 PoolOptions = 沿用 pgxpool 默认值。集成测试不需要调池，
	// 保持默认反而更接近"未调优"的真实形态。
	db, err := database.Connect(ctx, dsn, database.PoolOptions{})
	if err != nil {
		t.Fatalf("连接 PostgreSQL: %v", err)
	}
	defer db.Pool.Close()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	if err := db.EnsureVectorSchema(ctx, dim); err != nil {
		t.Fatalf("准备向量 schema: %v", err)
	}

	repo := &KnowledgeRepo{db: db}

	kbIDs, err := seedTenantFixture(ctx, db, shards, rows, dim)
	if err != nil {
		t.Fatalf("造测试数据: %v", err)
	}
	defer func() {
		if _, err := db.Pool.Exec(context.Background(),
			`DELETE FROM knowledge_bases WHERE id = ANY($1)`, kbIDs); err != nil {
			t.Logf("清理测试数据失败（可手动删 knowledge_bases id in %v）: %v", kbIDs, err)
		}
	}()

	target := kbIDs[0]

	// ---- 前提检查：本次规模下规划器必须真的选择 HNSW 索引 ----
	plan, actualRows, err := explainSearch(ctx, db, target, k)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	if !strings.Contains(plan, "hnsw") {
		t.Fatalf("前提不成立：规划器没有选择 HNSW 索引，本用例失去意义。\n"+
			"请调大 NEBULA_TEST_RAG_ROWS（当前 %d）后重跑。查询计划：\n%s", rows, plan)
	}
	t.Logf("查询计划确认走 HNSW；目标知识库占全表 1/%d，EXPLAIN 实跑返回 %d 行（期望 %d）",
		shards, actualRows, k)

	// ---- 对照：关掉迭代扫描时能返回几条（让用例输出自己说明它有没有牙齿）----
	if n, err := countWithoutIterativeScan(ctx, db, target, k); err != nil {
		t.Logf("对照查询失败（不影响结论）: %v", err)
	} else {
		t.Logf("对照：hnsw.iterative_scan=off 时返回 %d 条；本次断言要求 %d 条", n, k)
	}

	// ---- 正式断言：走真实的 SearchChunks ----
	queryVec := randVec(rand.New(rand.NewSource(7)), dim)
	hits, err := repo.SearchChunks(ctx, target, queryVec, k)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != k {
		t.Fatalf("多租户检索只返回 %d 条，期望 %d 条。\n"+
			"这是 HNSW 全局索引 + knowledge_base_id 后置过滤导致的漏召回：\n"+
			"索引扫描默认只吐 hnsw.ef_search(=40) 行候选，被过滤后凑不满 k。\n"+
			"修复方式是 SET LOCAL hnsw.iterative_scan（见 SearchChunks 注释）。\n"+
			"查询计划：\n%s", len(hits), k, plan)
	}

	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("第 %d 条 Score=%.6f 高于前一条 %.6f，结果未按相似度降序",
				i, hits[i].Score, hits[i-1].Score)
		}
	}
	for _, h := range hits {
		if h.ID == 0 || h.DocumentID == 0 || h.Filename == "" || h.Content == "" {
			t.Fatalf("命中结果字段不完整: %+v", h)
		}
	}
	if hits[0].Score <= 0 || hits[0].Score > 1.000001 {
		t.Fatalf("Score 越界: %.6f（余弦相似度应在 (0,1]）", hits[0].Score)
	}
}

// ---------- 夹具 ----------

// seedTenantFixture 建 shards 个知识库（各 1 篇文档），把 rows 个分块轮转分配进去，
// 返回全部知识库 id（第 0 个是检索目标）。
func seedTenantFixture(ctx context.Context, db *database.DB, shards, rows, dim int) ([]int64, error) {
	if shards < 1 {
		shards = 1
	}
	var userID int64
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO users (username, email, password_hash)
		VALUES ('store-integration-fixture', 'store-integration-fixture@example.invalid', 'x')
		ON CONFLICT (username) DO UPDATE SET updated_at = now()
		RETURNING id`).Scan(&userID); err != nil {
		return nil, fmt.Errorf("准备测试用户: %w", err)
	}

	kbIDs := make([]int64, 0, shards)
	docIDs := make([]int64, 0, shards)
	for i := 0; i < shards; i++ {
		var kbID, docID int64
		if err := db.Pool.QueryRow(ctx,
			`INSERT INTO knowledge_bases (user_id, name, description) VALUES ($1,$2,$3) RETURNING id`,
			userID, fmt.Sprintf("store-integration-%d", i), "多租户召回回归测试").Scan(&kbID); err != nil {
			return kbIDs, err
		}
		if err := db.Pool.QueryRow(ctx,
			`INSERT INTO documents (knowledge_base_id, filename, status, content)
			 VALUES ($1,$2,'indexed','fixture') RETURNING id`,
			kbID, "fixture.md").Scan(&docID); err != nil {
			return kbIDs, err
		}
		kbIDs = append(kbIDs, kbID)
		docIDs = append(docIDs, docID)
	}

	// 批量写入走「COPY 到临时表 → 转型插入」两段式：
	// rows × dim 拼成多行 INSERT 时 SQL 文本会有几百 MB。
	if _, err := db.Pool.Exec(ctx,
		`CREATE TEMP TABLE _fixture_vec (document_id bigint, content text, chunk_index int, vec text)`); err != nil {
		return kbIDs, err
	}
	rng := rand.New(rand.NewSource(20260919))
	const batch = 2000
	buf := make([][]any, 0, batch)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		_, err := db.Pool.CopyFrom(ctx, pgx.Identifier{"_fixture_vec"},
			[]string{"document_id", "content", "chunk_index", "vec"},
			pgx.CopyFromRows(buf))
		buf = buf[:0]
		return err
	}
	for i := 0; i < rows; i++ {
		buf = append(buf, []any{
			docIDs[i%shards],
			fmt.Sprintf("分块 %d：多租户召回回归测试的占位文本。", i),
			i,
			randVecLiteral(rng, dim),
		})
		if len(buf) >= batch {
			if err := flush(); err != nil {
				return kbIDs, err
			}
		}
	}
	if err := flush(); err != nil {
		return kbIDs, err
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO document_chunks (document_id, content, chunk_index, embedding_v)
		 SELECT document_id, content, chunk_index, vec::vector FROM _fixture_vec`); err != nil {
		return kbIDs, err
	}
	// 统计信息是必须的：HNSW 是索引扫描，但外层 JOIN/过滤的代价估算依赖统计信息
	for _, tbl := range []string{"document_chunks", "documents"} {
		if _, err := db.Pool.Exec(ctx, "ANALYZE "+tbl); err != nil {
			return kbIDs, err
		}
	}
	return kbIDs, nil
}

// explainSearch 打印检索语句的查询计划，并返回 EXPLAIN 实跑时的返回行数。
func explainSearch(ctx context.Context, db *database.DB, kbID int64, k int) (string, int, error) {
	var probe string
	if err := db.Pool.QueryRow(ctx, `
		SELECT c.embedding_v::text FROM document_chunks c
		  JOIN documents d ON d.id = c.document_id
		 WHERE d.knowledge_base_id = $1 LIMIT 1`, kbID).Scan(&probe); err != nil {
		return "", 0, err
	}
	rows, err := db.Pool.Query(ctx, `
		EXPLAIN (ANALYZE, BUFFERS)
		SELECT c.id FROM document_chunks c
		  JOIN documents d ON d.id = c.document_id
		 WHERE d.knowledge_base_id = $1 AND d.status = 'indexed' AND c.embedding_v IS NOT NULL
		 ORDER BY c.embedding_v <=> $2::vector LIMIT $3`, kbID, probe, k)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var lines []string
	actual := 0
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", 0, err
		}
		lines = append(lines, s)
		// 顶层 Limit 节点的 actual rows 就是真实返回行数
		if actual == 0 && strings.HasPrefix(strings.TrimSpace(s), "Limit") {
			if i := strings.Index(s, "rows="); i >= 0 {
				rest := s[i+len("rows="):]
				if j := strings.Index(rest, " "); j > 0 {
					actual, _ = strconv.Atoi(rest[:j])
				}
			}
		}
	}
	return strings.Join(lines, "\n"), actual, rows.Err()
}

// countWithoutIterativeScan 用 pgvector 的默认行为（iterative_scan=off）跑同一条件，
// 返回实际拿到的行数。它的作用是让用例的输出自带对照：
// 如果这里就是 k，说明本次数据规模没有触发后置过滤问题，用例的断言是空过的。
func countWithoutIterativeScan(ctx context.Context, db *database.DB, kbID int64, k int) (int, error) {
	var probe string
	if err := db.Pool.QueryRow(ctx, `
		SELECT c.embedding_v::text FROM document_chunks c
		  JOIN documents d ON d.id = c.document_id
		 WHERE d.knowledge_base_id = $1 LIMIT 1`, kbID).Scan(&probe); err != nil {
		return 0, err
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT c.id FROM document_chunks c
		  JOIN documents d ON d.id = c.document_id
		 WHERE d.knowledge_base_id = $1 AND d.status = 'indexed' AND c.embedding_v IS NOT NULL
		 ORDER BY c.embedding_v <=> $2::vector LIMIT $3`, kbID, probe, k)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		n++
	}
	return n, rows.Err()
}

// ---------- 小工具 ----------

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func randVec(rng *rand.Rand, dim int) []float64 {
	v := make([]float64, dim)
	for i := range v {
		v[i] = rng.NormFloat64()
	}
	return v
}

// randVecLiteral 输出 pgvector 的文本字面量，与 knowledge.go 的 vecLiteral 同构，
// 但直接由随机数生成（省掉一次 []float64 中转）。
func randVecLiteral(rng *rand.Rand, dim int) string {
	var sb strings.Builder
	sb.Grow(dim * 10)
	sb.WriteByte('[')
	for i := 0; i < dim; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(rng.NormFloat64(), 'g', -1, 64))
	}
	sb.WriteByte(']')
	return sb.String()
}

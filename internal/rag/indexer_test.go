package rag

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/hoarfrost/nebulaflow/internal/store"
)

// testLogger 返回一个丢弃日志的 logger，避免测试输出被刷屏。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestIndexer 创建一个用于测试的索引器：内存存储 + 本地 hash embedder。
func newTestIndexer(t *testing.T, concurrency, queueSize int) (*Indexer, *store.MemoryStore) {
	t.Helper()
	ms := store.NewMemory()
	ragSvc := NewService(llm.NewEmbeddingGateway(nil, llm.NewLocalHashEmbedder(64)))
	idx := NewIndexer(ragSvc, ms.Knowledge, concurrency, queueSize, testLogger())
	return idx, ms
}

// 辅助：创建一个待索引的文档并写入 store。
func createTestDoc(t *testing.T, ms *store.MemoryStore, kbID int64, content string) *model.Document {
	t.Helper()
	doc := &model.Document{
		KnowledgeBaseID: kbID,
		Filename:        "test.txt",
		Status:          model.DocPending,
		Content:         content,
	}
	if err := ms.Knowledge.CreateDocument(context.Background(), doc); err != nil {
		t.Fatalf("create doc: %v", err)
	}
	return doc
}

// 辅助：等待文档达到目标状态，最多等待 2 秒。
func waitForDocStatus(t *testing.T, ms *store.MemoryStore, docID int64, want model.DocumentStatus) *model.Document {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, err := ms.Knowledge.GetDocument(context.Background(), docID)
		if err != nil {
			t.Fatalf("get doc: %v", err)
		}
		if d.Status == want {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	d, _ := ms.Knowledge.GetDocument(context.Background(), docID)
	t.Fatalf("doc %d never reached status %s, last status=%s", docID, want, d.Status)
	return nil
}

// ---------- 状态流转测试 ----------

// TestIndexer_SuccessFlow 验证成功路径的状态流转：pending → indexing → indexed。
func TestIndexer_SuccessFlow(t *testing.T) {
	idx, ms := newTestIndexer(t, 1, 16)
	idx.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = idx.Shutdown(ctx)
	}()

	content := "这是一段用于测试的文档内容。" +
		"它包含足够多的文字，以便分块器至少产生一个块。" +
		"NebulaFlow 是一个高并发 AI Agent 工作流平台。"
	doc := createTestDoc(t, ms, 1, content)

	// 初始状态应为 pending
	if doc.Status != model.DocPending {
		t.Fatalf("initial status should be pending, got %s", doc.Status)
	}

	if err := idx.Enqueue(doc); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// 最终状态应为 indexed
	d := waitForDocStatus(t, ms, doc.ID, model.DocIndexed)
	if d.Chunks == 0 {
		t.Fatal("indexed doc should have chunks > 0")
	}
	if d.SizeKB == 0 && len(content) > 1024 {
		t.Fatal("indexed doc with long content should have size_kb > 0")
	}
	if d.ErrorMessage != "" {
		t.Fatalf("indexed doc should have empty error_message, got %q", d.ErrorMessage)
	}
}

// TestIndexer_FailureFlow 验证失败路径的状态流转。
// 使用一个始终报错的 embedder 来触发失败。
func TestIndexer_FailureFlow(t *testing.T) {
	ms := store.NewMemory()
	// 构造一个会失败的 embedder：primary 和 fallback 都失败，
	// 这样 EmbeddingGateway 就没有退路，一定会返回错误。
	failEmbedder := &failingEmbedder{err: errEmbeddingFailed}
	ragSvc := NewService(llm.NewEmbeddingGateway(failEmbedder, failEmbedder))
	idx := NewIndexer(ragSvc, ms.Knowledge, 1, 16, testLogger())
	idx.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = idx.Shutdown(ctx)
	}()

	doc := createTestDoc(t, ms, 1, "测试文档内容，用于验证失败路径。")

	if err := idx.Enqueue(doc); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// 最终状态应为 failed
	d := waitForDocStatus(t, ms, doc.ID, model.DocFailed)
	if d.ErrorMessage == "" {
		t.Fatal("failed doc should have non-empty error_message")
	}
	if d.Chunks != 0 {
		t.Fatalf("failed doc should have chunks=0, got %d", d.Chunks)
	}
}

// failingEmbedder 是一个始终返回错误的 Embedder，用于测试失败路径。
type failingEmbedder struct{ err error }

func (f *failingEmbedder) Name() string { return "failing" }
func (f *failingEmbedder) Embed(_ context.Context, _ []string) ([][]float64, error) {
	return nil, f.err
}

var errEmbeddingFailed = &testEmbedError{msg: "simulated embedding failure"}

type testEmbedError struct{ msg string }

func (e *testEmbedError) Error() string { return e.msg }

// ---------- 并发度限制测试 ----------

// TestIndexer_ConcurrencyLimit 验证索引并发度确实受到限制。
// 思路：用一个很慢的 embedder（阻塞到被释放），入队 N 个文档，
// 检查"正在执行"的数量不超过并发度。
func TestIndexer_ConcurrencyLimit(t *testing.T) {
	concurrency := 2
	totalDocs := 6

	ms := store.NewMemory()
	slow := &slowEmbedder{delay: 50 * time.Millisecond}
	ragSvc := NewService(llm.NewEmbeddingGateway(slow, nil))
	idx := NewIndexer(ragSvc, ms.Knowledge, concurrency, 32, testLogger())
	idx.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = idx.Shutdown(ctx)
	}()

	// 记录"同时在执行"的最大并发数
	var maxActive int32
	slow.onStart = func() {
		cur := atomic.AddInt32(&slow.active, 1)
		for {
			prev := atomic.LoadInt32(&maxActive)
			if cur <= prev {
				break
			}
			if atomic.CompareAndSwapInt32(&maxActive, prev, cur) {
				break
			}
		}
	}
	slow.onEnd = func() {
		atomic.AddInt32(&slow.active, -1)
	}

	// 批量入队
	for i := 0; i < totalDocs; i++ {
		doc := createTestDoc(t, ms, 1, "并发限制测试文档内容。"+string(rune('A'+i)))
		if err := idx.Enqueue(doc); err != nil {
			t.Fatalf("enqueue doc %d: %v", i, err)
		}
	}

	// 等待所有文档完成
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		docs, err := ms.Knowledge.ListDocuments(context.Background(), 1)
		if err != nil {
			t.Fatalf("list docs: %v", err)
		}
		allDone := true
		for _, d := range docs {
			if d.Status != model.DocIndexed && d.Status != model.DocFailed {
				allDone = false
				break
			}
		}
		if allDone && len(docs) == totalDocs {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&maxActive); int(got) > concurrency {
		t.Fatalf("max active embeddings (%d) exceeded concurrency limit (%d)", got, concurrency)
	}
	if got := atomic.LoadInt32(&maxActive); got < int32(concurrency) && totalDocs > concurrency {
		// 理论上如果有足够多的文档且并发度>1，应该达到上限
		// 但因为测试环境调度的不确定性，这条不是硬性断言，只做提醒
		t.Logf("note: max active (%d) never reached concurrency (%d); may be due to fast execution",
			got, concurrency)
	}
}

// slowEmbedder 是一个可控延迟的 Embedder，用于并发测试。
type slowEmbedder struct {
	delay   time.Duration
	active  int32
	onStart func()
	onEnd   func()
}

func (s *slowEmbedder) Name() string { return "slow" }

func (s *slowEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if s.onStart != nil {
		s.onStart()
	}
	defer func() {
		if s.onEnd != nil {
			s.onEnd()
		}
	}()

	// 用 select 让它可被 context 取消
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 返回全零向量，维度 64
	out := make([][]float64, len(texts))
	for i := range out {
		out[i] = make([]float64, 64)
	}
	return out, nil
}

// ---------- 队列满测试 ----------

func TestIndexer_QueueFull(t *testing.T) {
	idx, ms := newTestIndexer(t, 1, 1) // 队列大小=1
	// 不启动 worker，让队列永远不会被消费

	// 第一个文档应该能入队
	doc1 := createTestDoc(t, ms, 1, "doc1")
	if err := idx.Enqueue(doc1); err != nil {
		t.Fatalf("first enqueue should succeed, got %v", err)
	}

	// 第二个文档应该因队列满而失败
	doc2 := createTestDoc(t, ms, 1, "doc2")
	if err := idx.Enqueue(doc2); err != ErrIndexQueueFull {
		t.Fatalf("second enqueue should return ErrIndexQueueFull, got %v", err)
	}
}

// ---------- 优雅关闭测试 ----------

func TestIndexer_ShutdownWaitsForInFlight(t *testing.T) {
	ms := store.NewMemory()
	slow := &slowEmbedder{delay: 100 * time.Millisecond}
	ragSvc := NewService(llm.NewEmbeddingGateway(slow, nil))
	idx := NewIndexer(ragSvc, ms.Knowledge, 1, 16, testLogger())
	idx.Start()

	// 入队 3 个文档
	var docIDs []int64
	for i := 0; i < 3; i++ {
		doc := createTestDoc(t, ms, 1, "优雅关闭测试文档 "+string(rune('A'+i)))
		docIDs = append(docIDs, doc.ID)
		if err := idx.Enqueue(doc); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// 立即调用 Shutdown，给足够时间完成
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := idx.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
	elapsed := time.Since(start)

	// 验证：3 个文档都完成了索引
	for _, id := range docIDs {
		d, err := ms.Knowledge.GetDocument(context.Background(), id)
		if err != nil {
			t.Fatalf("get doc %d: %v", id, err)
		}
		if d.Status != model.DocIndexed {
			t.Fatalf("doc %d should be indexed after shutdown, got status=%s", id, d.Status)
		}
	}

	// 验证：确实花了一些时间（至少接近 1 个文档的处理时间 * 3）
	// 因为并发度=1，3 个文档串行，应该 >= 200ms
	if elapsed < 150*time.Millisecond {
		t.Fatalf("shutdown returned too fast (%v), in-flight tasks may not have completed", elapsed)
	}
}

func TestIndexer_ShutdownTimeout(t *testing.T) {
	ms := store.NewMemory()
	slow := &slowEmbedder{delay: 500 * time.Millisecond}
	ragSvc := NewService(llm.NewEmbeddingGateway(slow, nil))
	idx := NewIndexer(ragSvc, ms.Knowledge, 1, 16, testLogger())
	idx.Start()

	doc := createTestDoc(t, ms, 1, "超时测试文档")
	if err := idx.Enqueue(doc); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 给一个很短的超时，确保触发超时
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := idx.Shutdown(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("shutdown should return error on timeout")
	}
	// 验证大致在超时时间附近返回（50ms + 一些调度开销）
	if elapsed > 200*time.Millisecond {
		t.Fatalf("shutdown took too long (%v) despite short timeout", elapsed)
	}
}

// ---------- Shutdown 后入队测试 ----------

func TestIndexer_EnqueueAfterShutdown(t *testing.T) {
	idx, ms := newTestIndexer(t, 1, 16)
	idx.Start()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = idx.Shutdown(ctx)

	doc := createTestDoc(t, ms, 1, "post-shutdown doc")
	if err := idx.Enqueue(doc); err != ErrIndexerStopped {
		t.Fatalf("enqueue after shutdown should return ErrIndexerStopped, got %v", err)
	}
}

// ---------- 短文档索引测试 ----------

func TestIndexer_ShortDocument(t *testing.T) {
	idx, ms := newTestIndexer(t, 1, 16)
	idx.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = idx.Shutdown(ctx)
	}()

	doc := createTestDoc(t, ms, 1, "短文本")
	if err := idx.Enqueue(doc); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	d := waitForDocStatus(t, ms, doc.ID, model.DocIndexed)
	if d.Chunks == 0 {
		t.Fatal("short document should still have at least 1 chunk")
	}
}

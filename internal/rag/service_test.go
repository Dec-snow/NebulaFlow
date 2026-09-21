package rag

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

func newTestService() *Service {
	// 用本地 hash embedder 保证测试确定性、不依赖外网
	return NewService(llm.NewEmbeddingGateway(nil, llm.NewLocalHashEmbedder(64)))
}

// fakeSearcher 是 ChunkSearcher 的内存实现，语义与 store 的内存后端一致
// （余弦排序 + Top-K 截断），另外记录被请求的 k 以便断言下推行为。
type fakeSearcher struct {
	chunks    []model.DocumentChunk
	requested []int // 每次调用收到的 k
}

func (f *fakeSearcher) SearchChunks(_ context.Context, _ int64, queryVec []float64, k int) ([]model.DocumentChunk, error) {
	f.requested = append(f.requested, k)
	if k <= 0 {
		k = 5
	}
	if len(queryVec) == 0 {
		return nil, nil
	}
	out := make([]model.DocumentChunk, 0, len(f.chunks))
	for _, c := range f.chunks {
		if len(c.Embedding) == 0 {
			continue
		}
		c.Score = llm.Cosine(queryVec, c.Embedding)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// 分块：短文本单块；长文按窗口切分且数量正确。
func TestChunk(t *testing.T) {
	s := newTestService()
	short := "第一段。\n第二段。"
	chunks := s.Chunk(short)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for short text, got %d: %v", len(chunks), chunks)
	}

	long := strings.Repeat("这是一个用于测试分块的长文本片段，包含足够多的字词。", 50)
	chunks = s.Chunk(long)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks for long text, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len([]rune(c)) > DefaultChunkSize+50 {
			t.Fatalf("chunk %d too long: %d", i, len([]rune(c)))
		}
	}
}

// 检索：语义相近的 chunk 排在最前。
func TestRetrieve_Ranking(t *testing.T) {
	s := newTestService()
	// 重复次数要保证文本超过 2 个 chunk（按 rune 计），这样检索才有“排序”可言。
	// 注意：分块长度按 rune 计（中文 1 字 = 1 rune），不要按字节估算。
	text := strings.Repeat("Redis Stream 是 Redis 5.0 引入的持久化消息队列，支持消费者组与崩溃恢复。", 10) + "\n" +
		strings.Repeat("NebulaFlow 使用 Redis Stream 实现任务队列、限流与分布式锁。", 10) + "\n" +
		strings.Repeat("PostgreSQL 用于存储用户、工作流与任务数据。", 12)
	if utf8.RuneCountInString(text) < 2*DefaultChunkSize {
		t.Fatalf("test fixture too short: %d runes, need >= %d",
			utf8.RuneCountInString(text), 2*DefaultChunkSize)
	}
	chunks := s.Chunk(text)
	if len(chunks) < 2 {
		t.Fatalf("need >=2 chunks, got %d", len(chunks))
	}
	doc := &model.Document{ID: 1, Content: text}
	indexed, err := s.IndexDocument(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed) != len(chunks) {
		t.Fatalf("expected %d indexed chunks, got %d", len(chunks), len(indexed))
	}
	searcher := &fakeSearcher{chunks: indexed}
	hits, err := s.Retrieve(context.Background(), searcher, doc.ID, "任务队列", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits")
	}
	if hits[0].Score < 0.2 {
		t.Fatalf("top hit score too low: %f", hits[0].Score)
	}
	// 命中内容应含关键词
	if !strings.Contains(hits[0].Content, "队列") && !strings.Contains(hits[0].Content, "Redis") {
		t.Fatalf("top hit not relevant: %s", hits[0].Content)
	}
	// 结果应按相似度降序
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Fatalf("hits not sorted desc at %d: %f < %f", i, hits[i-1].Score, hits[i].Score)
		}
	}
}

// P0-1 回归：Retrieve 必须把 Top-K 下推给存储层，而不是"拿全量再自己裁剪"。
//
// 这是本次改造的接口契约。改造前 Retrieve(ctx, query, chunks, k) 要求调用方
// 先把整个知识库传进来——签名本身就逼着调用方做全量加载。
// 现在 Retrieve 只把 k 透传给 searcher，自己不再持有任何候选集。
// 断言 k 被原样透传（且不为 0/负数），防止将来有人把它改回"全量"。
func TestRetrieve_PushesDownTopK(t *testing.T) {
	s := newTestService()
	text := strings.Repeat("Redis Stream 支持消费者组与崩溃恢复。", 120)
	indexed, err := s.IndexDocument(context.Background(), &model.Document{ID: 1, Content: text})
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed) < 3 {
		t.Fatalf("fixture too small: %d chunks", len(indexed))
	}

	searcher := &fakeSearcher{chunks: indexed}
	hits, err := s.Retrieve(context.Background(), searcher, 1, "消费者组", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(searcher.requested) != 1 {
		t.Fatalf("searcher 应被调用恰好 1 次，实际 %d 次", len(searcher.requested))
	}
	if got := searcher.requested[0]; got != 2 {
		t.Fatalf("k 应原样下推为 2，实际 %d", got)
	}
	if len(hits) > 2 {
		t.Fatalf("返回条数应受 k 约束（<=2），实际 %d", len(hits))
	}
}

// 空查询/空库安全返回。
func TestRetrieve_Empty(t *testing.T) {
	s := newTestService()
	hits, err := s.Retrieve(context.Background(), &fakeSearcher{}, 1, "", 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("expected empty, got %v %v", hits, err)
	}
	hits, err = s.Retrieve(context.Background(), &fakeSearcher{}, 1, "anything", 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("expected empty for empty corpus, got %v %v", hits, err)
	}
	// searcher 为 nil 时不应 panic（例如内存模式下知识库未接线）
	hits, err = s.Retrieve(context.Background(), nil, 1, "anything", 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("expected empty for nil searcher, got %v %v", hits, err)
	}
}

// 余弦相似度：相同向量为 1，正交为 0。
func TestCosine(t *testing.T) {
	if got := llm.Cosine([]float64{1, 0}, []float64{1, 0}); got != 1 {
		t.Fatalf("same vector should have cos=1, got %f", got)
	}
	if got := llm.Cosine([]float64{1, 0}, []float64{0, 1}); got != 0 {
		t.Fatalf("orthogonal should have cos=0, got %f", got)
	}
}

// 分块长度必须按 rune 计：中文按字节切会把块切得过小（一个汉字 3 字节），
// 导致知识库被切成大量零碎片段，检索召回质量显著下降。
func TestChunk_UsesRuneNotByte(t *testing.T) {
	s := newTestService()
	// 600 个汉字 ≈ 1800 字节。若按字节计会被切成 3 块以上，按 rune 计应为 1 块。
	text := strings.Repeat("中", DefaultChunkSize)
	chunks := s.Chunk(text)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for %d CJK runes, got %d", DefaultChunkSize, len(chunks))
	}
	if utf8.RuneCountInString(chunks[0]) != DefaultChunkSize {
		t.Fatalf("chunk lost content: %d runes", utf8.RuneCountInString(chunks[0]))
	}
}

// overlap >= chunkSize 时滑动窗口步长会变成 0/负数，原实现会死循环。
// 这里用超时保护确保修复后配置写错也不会把上传接口卡死。
func TestChunk_OverlapLargerThanSize_NoHang(t *testing.T) {
	s := &Service{embedder: nil, chunkSize: 100, overlap: 200}
	done := make(chan []string, 1)
	go func() {
		done <- s.Chunk(strings.Repeat("测试文本内容用于分块。", 100))
	}()
	select {
	case chunks := <-done:
		if len(chunks) == 0 {
			t.Fatal("expected non-empty chunks")
		}
		for i, c := range chunks {
			if utf8.RuneCountInString(c) > 100+50 {
				t.Fatalf("chunk %d exceeds size: %d", i, utf8.RuneCountInString(c))
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Chunk hung: sliding window step must be positive")
	}
}

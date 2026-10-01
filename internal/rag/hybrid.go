package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hoarfrost/nebulaflow/internal/model"
)

// Reranker 是重排序器接口。
//
// 输入：查询 + 候选文档列表
// 输出：按相关性重新排序后的文档列表（带新的分数）
//
// 为什么需要 Reranker：
//   - 向量检索召回 Top-50，召回率高但精度一般
//   - 关键词检索召回 Top-50，精确但语义理解差
//   - Reranker 用更强大的模型（cross-encoder）对融合后的结果重新打分
//   - 最终只取 Top-5 给 LLM，质量大幅提升
//
// 业界常用方案：BGE Reranker、Cohere Rerank、Jina Reranker
type Reranker interface {
	// Name 返回 reranker 名称。
	Name() string
	// Rerank 对候选文档重新排序，返回排序后的结果（按分数降序）。
	// query 是查询文本，docs 是候选文档内容，topK 是返回前 K 个。
	Rerank(ctx context.Context, query string, docs []string, topK int) ([]RerankResult, error)
}

// RerankResult 是重排序后的单条结果。
type RerankResult struct {
	Index int     // 原候选列表中的索引
	Score float64 // 重排序后的相关性分数（0~1）
}

// ---------- RRF 融合（Reciprocal Rank Fusion） ----------

// RRFFusion 用 RRF 算法融合多路检索结果。
//
// RRF 公式：score(d) = Σ 1 / (k + rank_i(d))
//   - rank_i(d) 是文档 d 在第 i 路检索中的排名（从 1 开始）
//   - k 是常数，通常取 60
//
// 为什么用 RRF 而不是加权平均：
//   - 不需要知道各路分数的量纲（向量相似度 vs BM25 分数完全不同）
//   - 不需要调权重（对不同查询、不同语料都很鲁棒）
//   - 业界标准：Elasticsearch、Solr、Vespa 都内置了 RRF
//
// 参考文献：Cormack et al., "Reciprocal Rank Fusion" (2009)
const rrfK = 60

// RRFFusion 融合多路检索结果。
// 输入：每一路是一组 Hit（按分数降序排列）
// 输出：融合后的 Hit（按 RRF 分数降序排列）
func RRFFusion(paths ...[]Hit) []Hit {
	if len(paths) == 0 {
		return nil
	}
	if len(paths) == 1 {
		return paths[0]
	}

	// 用 document_id + chunk_index 作为唯一标识
	type docKey struct {
		docID   int64
		chunkIdx int
	}

	scores := make(map[docKey]float64)
	docs := make(map[docKey]Hit)

	for _, path := range paths {
		for rank, hit := range path {
			key := docKey{docID: hit.DocumentID, chunkIdx: hit.ChunkIndex}
			// RRF 分数：1 / (k + rank)，rank 从 1 开始
			scores[key] += 1.0 / float64(rrfK+rank+1)
			if _, exists := docs[key]; !exists {
				docs[key] = hit
			}
		}
	}

	// 按 RRF 分数降序排列
	type scored struct {
		key   docKey
		score float64
	}
	var list []scored
	for k, v := range scores {
		list = append(list, scored{key: k, score: v})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].score > list[j].score
	})

	result := make([]Hit, 0, len(list))
	for _, s := range list {
		hit := docs[s.key]
		hit.Score = s.score
		result = append(result, hit)
	}
	return result
}

// ---------- Hybrid Search ----------

// KeywordSearcher 是关键词检索接口。
//
// 由 store 层实现（PostgreSQL 走 tsvector），rag.Service 只负责调用。
type KeywordSearcher interface {
	// KeywordSearch 在知识库内做关键词全文检索。
	// 返回按相关性降序排列的 Top-K 分块。
	KeywordSearch(ctx context.Context, kbID int64, query string, k int) ([]model.DocumentChunk, error)
}

// HybridRetrieve 执行混合检索：向量检索 + 关键词检索 → RRF 融合 → 可选 Rerank。
//
// 流程：
//   1. 向量检索召回 Top-N（如 50）
//   2. 关键词检索召回 Top-N（如 50）
//   3. RRF 融合两路结果
//   4. 如果配置了 Reranker，对融合结果做重排序
//   5. 返回最终 Top-K
func (s *Service) HybridRetrieve(
	ctx context.Context,
	vecSearcher ChunkSearcher,
	kwSearcher KeywordSearcher,
	kbID int64,
	query string,
	topK int,
	reranker Reranker,
	recallN int,
) ([]Hit, error) {
	if topK <= 0 {
		topK = 5
	}
	if recallN <= 0 {
		recallN = topK * 10 // 默认召回 10 倍，给 reranker 留足候选
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}

	// ---- 并行执行两路召回 ----
	var (
		vecHits   []model.DocumentChunk
		vecErr    error
		kwHits    []model.DocumentChunk
		kwErr     error
		vecReady  = make(chan struct{})
		kwReady   = make(chan struct{})
	)

	// 向量检索
	go func() {
		defer close(vecReady)
		if vecSearcher == nil {
			return
		}
		qvecs, err := s.embedder.Embed(ctx, []string{query})
		if err != nil {
			vecErr = err
			return
		}
		if len(qvecs) == 0 {
			return
		}
		vecHits, vecErr = vecSearcher.SearchChunks(ctx, kbID, qvecs[0], recallN)
	}()

	// 关键词检索
	go func() {
		defer close(kwReady)
		if kwSearcher == nil {
			return
		}
		kwHits, kwErr = kwSearcher.KeywordSearch(ctx, kbID, query, recallN)
	}()

	// 等待两路完成
	<-vecReady
	<-kwReady

	// 如果两路都失败，返回错误
	if vecErr != nil && kwErr != nil {
		return nil, fmt.Errorf("hybrid search: vector: %w, keyword: %w", vecErr, kwErr)
	}

	// 转成 Hit 格式
	vecHitsList := chunksToHits(vecHits)
	kwHitsList := chunksToHits(kwHits)

	// ---- RRF 融合 ----
	fused := RRFFusion(vecHitsList, kwHitsList)

	// ---- Rerank（如果配置了）----
	if reranker != nil && len(fused) > topK {
		fused = s.rerankHits(ctx, reranker, query, fused, topK)
	}

	// 截断到 topK
	if len(fused) > topK {
		fused = fused[:topK]
	}
	return fused, nil
}

// rerankHits 用 Reranker 对候选结果重排序。
func (s *Service) rerankHits(ctx context.Context, reranker Reranker, query string, hits []Hit, topK int) []Hit {
	docs := make([]string, len(hits))
	for i, h := range hits {
		docs[i] = h.Content
	}

	results, err := reranker.Rerank(ctx, query, docs, topK)
	if err != nil {
		// rerank 失败就用原来的结果（降级）
		return hits
	}

	reranked := make([]Hit, len(results))
	for i, r := range results {
		if r.Index >= 0 && r.Index < len(hits) {
			reranked[i] = hits[r.Index]
			reranked[i].Score = r.Score
		}
	}
	return reranked
}

// chunksToHits 把 DocumentChunk 转成 Hit。
func chunksToHits(chunks []model.DocumentChunk) []Hit {
	if len(chunks) == 0 {
		return nil
	}
	hits := make([]Hit, len(chunks))
	for i, c := range chunks {
		hits[i] = Hit{
			Content:    c.Content,
			Score:      c.Score,
			DocumentID: c.DocumentID,
			ChunkIndex: c.ChunkIndex,
			Filename:   c.Filename,
		}
	}
	return hits
}

// ---------- Mock Reranker（无 Reranker 时的降级） ----------

// NoopReranker 是空实现，直接返回原顺序（用于测试和降级）。
type NoopReranker struct{}

func (NoopReranker) Name() string { return "noop" }

func (NoopReranker) Rerank(_ context.Context, _ string, docs []string, topK int) ([]RerankResult, error) {
	n := len(docs)
	if topK > 0 && topK < n {
		n = topK
	}
	results := make([]RerankResult, n)
	for i := 0; i < n; i++ {
		results[i] = RerankResult{Index: i, Score: float64(n-i) / float64(n)}
	}
	return results, nil
}

// ---------- 并发安全的 Reranker 包装 ----------

// 确保 NoopReranker 实现 Reranker 接口
var _ Reranker = NoopReranker{}

// CachedReranker 是带缓存的 Reranker 包装（相同 query + docs 直接返回缓存结果）。
// 适用于测试和开发环境，生产环境慎用（内存可能膨胀）。
type CachedReranker struct {
	inner Reranker
	mu    sync.Mutex
	cache map[string][]RerankResult
}

func NewCachedReranker(inner Reranker) *CachedReranker {
	return &CachedReranker{inner: inner, cache: make(map[string][]RerankResult)}
}

func (c *CachedReranker) Name() string { return "cached:" + c.inner.Name() }

func (c *CachedReranker) Rerank(ctx context.Context, query string, docs []string, topK int) ([]RerankResult, error) {
	key := fmt.Sprintf("%s|%d|%s", query, topK, strings.Join(docs, "\x00"))
	c.mu.Lock()
	if cached, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	results, err := c.inner.Rerank(ctx, query, docs, topK)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cache[key] = results
	c.mu.Unlock()
	return results, nil
}

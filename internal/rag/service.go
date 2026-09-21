// Package rag 实现知识库的文档处理流水线：
// 解析（txt/md 文本抽取）→ 分块（chunking）→ 向量化（Embedding）→ 存储，
// 以及检索（把 query 向量化后交给 ChunkSearcher 做 Top-K）。
//
// 检索的候选集裁剪在存储层完成：PostgreSQL 走 pgvector 的 HNSW 索引，
// 内存后端走进程内余弦。本包只负责"把查询变成向量"这一段，
// 不持有也不遍历全量分块——见 Retrieve 的注释。
package rag

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ChunkSize / Overlap：默认分块参数，可按文档类型调整。
const (
	DefaultChunkSize    = 600
	DefaultChunkOverlap = 80
)

type Service struct {
	embedder  *llm.EmbeddingGateway
	chunkSize int
	overlap   int
}

func NewService(embedder *llm.EmbeddingGateway) *Service {
	return &Service{embedder: embedder, chunkSize: DefaultChunkSize, overlap: DefaultChunkOverlap}
}

// ParseText 从原始文本（documents.content）抽取可索引内容。
// 上传侧已完成 txt/md 的抽取；PDF 在 API 层做文本流抽取后进入此流程。
func ParseText(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\x00", "")
	return s
}

// Chunk 按滑动窗口切分文本。
//
// 两处修正：
//  1. 统一按 rune 计数（原实现第一段按字节、第二段按 rune，
//     中文文本下两段的块大小差 3 倍，切出来的块忽大忽小）；
//  2. 滑动窗口步长做下限保护——若 overlap >= chunkSize 会导致 i 不前进，
//     直接死循环（配置写错时整个上传接口会卡死）。
func (s *Service) Chunk(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	size, overlap := s.chunkSize, s.overlap
	if size <= 0 {
		size = DefaultChunkSize
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		// 保证窗口一定前进
		overlap = size / 4
	}

	// 优先按段落/行切分，再合并到目标长度
	units := strings.Split(text, "\n")
	var out []string
	var buf strings.Builder
	bufLen := 0 // 以 rune 计数
	for _, u := range units {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		ul := utf8.RuneCountInString(u)
		if bufLen > 0 && bufLen+ul > size {
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
			bufLen = 0
		}
		if bufLen > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(u)
		bufLen += ul
	}
	if bufLen > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}

	// 若单块仍超长，按固定窗口再切
	var final []string
	for _, c := range out {
		runes := []rune(c)
		if len(runes) <= size {
			final = append(final, c)
			continue
		}
		step := size - overlap
		for i := 0; i < len(runes); {
			end := i + size
			if end > len(runes) {
				end = len(runes)
			}
			final = append(final, string(runes[i:end]))
			if end == len(runes) {
				break
			}
			i += step
		}
	}
	return final
}

// IndexDocument 对文档执行 解析 → 分块 → 向量化 → 入库。
func (s *Service) IndexDocument(ctx context.Context, doc *model.Document) ([]model.DocumentChunk, error) {
	text := ParseText(doc.Content)
	chunks := s.Chunk(text)
	if len(chunks) == 0 {
		return nil, nil
	}
	vecs, err := s.embedder.Embed(ctx, chunks)
	if err != nil {
		return nil, err
	}
	out := make([]model.DocumentChunk, 0, len(chunks))
	for i, c := range chunks {
		var vec []float64
		if i < len(vecs) {
			vec = vecs[i]
		}
		out = append(out, model.DocumentChunk{
			DocumentID: doc.ID, Content: c, Embedding: vec, ChunkIndex: i,
		})
	}
	return out, nil
}

// ChunkSearcher 是检索侧的最小依赖：给它知识库 ID、查询向量和 k，回 Top-K 分块。
//
// 定义在这里而不是直接依赖 store 包，有两个好处：
//   - rag 包不再知道"分块存在哪、怎么存"，只表达"我要最相似的 k 个"；
//   - 单测可以注入内存 fake，不需要起 PostgreSQL。
//
// 实现方（store.KnowledgeRepo / memKnowledgeRepo）负责保证：
// 结果按相似度降序、长度不超过 k、Score 是余弦相似度（越大越相似）。
type ChunkSearcher interface {
	SearchChunks(ctx context.Context, kbID int64, queryVec []float64, k int) ([]model.DocumentChunk, error)
}

// Hit 是检索结果，附带来源文档名便于前端展示 Sources。
type Hit struct {
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
	DocumentID int64   `json:"document_id"`
	Filename   string  `json:"filename,omitempty"`
	ChunkIndex int     `json:"chunk_index"`
}

// Retrieve 把 query 向量化后交给 searcher 做 Top-K 检索。
//
// P0-1 改造的关键：本函数不再接收 chunks 参数。
// 原签名是 Retrieve(ctx, query, chunks []model.DocumentChunk, k)，
// 调用方必须先 SearchChunks(kbID) 把整个知识库拉进内存——
// 于是"检索"这个名字下面藏着一次全量加载 + 全量 JSON 反序列化 + 全量余弦。
// 现在向量只算一次（query 侧），候选集由数据库的向量索引裁剪。
func (s *Service) Retrieve(ctx context.Context, searcher ChunkSearcher, kbID int64, query string, k int) ([]Hit, error) {
	if k <= 0 {
		k = 5
	}
	if strings.TrimSpace(query) == "" || searcher == nil {
		return nil, nil
	}
	qvecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(qvecs) == 0 {
		return nil, nil
	}

	chunks, err := searcher.SearchChunks(ctx, kbID, qvecs[0], k)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(chunks))
	for _, c := range chunks {
		hits = append(hits, Hit{
			Content: c.Content, Score: c.Score,
			DocumentID: c.DocumentID, ChunkIndex: c.ChunkIndex,
			Filename: c.Filename, // 来源文档名，前端 Sources 直接展示
		})
	}
	return hits, nil
}

// BuildContext 把检索命中拼成注入 LLM 的上下文块。
func BuildContext(hits []Hit) string {
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("以下是参考资料：\n\n")
	for i, h := range hits {
		sb.WriteString("【资料 ")
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString("】\n")
		sb.WriteString(h.Content)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hoarfrost/nebulaflow/internal/database"
	"github.com/hoarfrost/nebulaflow/internal/model"
	"github.com/jackc/pgx/v5"
)

type KnowledgeRepo struct{ db *database.DB }

func (r *KnowledgeRepo) CreateKB(ctx context.Context, kb *model.KnowledgeBase) error {
	err := r.db.Pool.QueryRow(ctx,
		`INSERT INTO knowledge_bases (user_id, name, description) VALUES ($1, $2, $3)
		 RETURNING id, created_at`, kb.UserID, kb.Name, kb.Description,
	).Scan(&kb.ID, &kb.CreatedAt)
	return err
}

func (r *KnowledgeRepo) ListKBs(ctx context.Context, userID int64) ([]model.KnowledgeBase, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, user_id, name, description, created_at
		 FROM knowledge_bases WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.KnowledgeBase
	for rows.Next() {
		var kb model.KnowledgeBase
		if err := rows.Scan(&kb.ID, &kb.UserID, &kb.Name, &kb.Description, &kb.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, kb)
	}
	return out, rows.Err()
}

func (r *KnowledgeRepo) DeleteKB(ctx context.Context, id, userID int64) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM knowledge_bases WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrKBNotFound
	}
	return nil
}

func (r *KnowledgeRepo) CreateDocument(ctx context.Context, d *model.Document) error {
	err := r.db.Pool.QueryRow(ctx,
		`INSERT INTO documents (knowledge_base_id, filename, status, content)
		 VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		d.KnowledgeBaseID, d.Filename, d.Status, d.Content,
	).Scan(&d.ID, &d.CreatedAt)
	return err
}

func (r *KnowledgeRepo) ListDocuments(ctx context.Context, kbID int64) ([]model.Document, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, knowledge_base_id, filename, status, created_at
		 FROM documents WHERE knowledge_base_id=$1 ORDER BY created_at DESC`, kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Document
	for rows.Next() {
		var d model.Document
		if err := rows.Scan(&d.ID, &d.KnowledgeBaseID, &d.Filename, &d.Status, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *KnowledgeRepo) GetDocument(ctx context.Context, docID int64) (*model.Document, error) {
	d := &model.Document{}
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, knowledge_base_id, filename, status, content, created_at
		 FROM documents WHERE id=$1`, docID,
	).Scan(&d.ID, &d.KnowledgeBaseID, &d.Filename, &d.Status, &d.Content, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (r *KnowledgeRepo) UpdateDocumentStatus(ctx context.Context, docID int64, status model.DocumentStatus) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE documents SET status=$2 WHERE id=$1`, docID, status)
	return err
}

func (r *KnowledgeRepo) DeleteChunksByDocument(ctx context.Context, docID int64) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM document_chunks WHERE document_id=$1`, docID)
	return err
}

func (r *KnowledgeRepo) InsertChunk(ctx context.Context, c *model.DocumentChunk) error {
	_, err := r.db.Pool.Exec(ctx,
		`INSERT INTO document_chunks (document_id, content, embedding_v, chunk_index)
		 VALUES ($1, $2, $3::vector, $4)`,
		c.DocumentID, c.Content, vecLiteral(c.Embedding), c.ChunkIndex)
	return err
}

// BatchInsertChunks 批量插入文档分块（单条 SQL，减少 round-trip）。
// RAG 索引一个文档可能产生数十到数百个 chunk，逐条 INSERT 会产生大量 DB 往返。
func (r *KnowledgeRepo) BatchInsertChunks(ctx context.Context, chunks []model.DocumentChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO document_chunks (document_id, content, embedding_v, chunk_index) VALUES ")
	args := make([]any, 0, len(chunks)*4)
	for i, c := range chunks {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d::vector,$%d)", i*4+1, i*4+2, i*4+3, i*4+4)
		args = append(args, c.DocumentID, c.Content, vecLiteral(c.Embedding), c.ChunkIndex)
	}
	_, err := r.db.Pool.Exec(ctx, sb.String(), args...)
	return err
}

// vecLiteral 把 float64 切片格式化成 pgvector 的文本字面量，例如 [1,2,3]。
//
// 不引入 pgvector-go 客户端类型：pgvector 本身接受文本输入，
// 配一句 $n::vector 显式转换即可，少一个依赖。
// FormatFloat 用 'g'/-1 输出最短往返表示，可能带科学计数法（如 1e-05），
// pgvector 的解析器接受这种写法（已实测）。
func vecLiteral(v []float64) string {
	if len(v) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.Grow(len(v) * 12)
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	}
	sb.WriteByte(']')
	return sb.String()
}

// hnswIterativeScan 是检索时使用的 pgvector 迭代索引扫描模式。
//
// 为什么必须显式设置：HNSW 索引是**全局**的，而查询要按 d.knowledge_base_id 过滤。
// 默认 hnsw.iterative_scan=off 时，索引扫描只吐出 hnsw.ef_search（默认 40）行候选
// 就结束了；这些候选里绝大多数属于别的知识库、会被过滤掉，于是查询返回的行数
// **不足 k 条**。
//
// 这不是推测，是实测出来的：`benchmark/ragbench -shards 20`（5 万分块 / 20 个知识库，
// 目标库只占 5%）下，30 次查询里有 29 次返回不足 k=5 条，平均只有 1~3 条；
// EXPLAIN 显示索引扫描恰好返回 40 行（= ef_search），随后
// `Rows Removed by Join Filter: 39`。也就是说 RAG 节点会静默地少拿一半以上上下文——
// 这是**召回缺陷**，不是性能问题，而且默认配置下不会报任何错。
//
// strict_order 让索引在过滤后继续往外找，直到凑满 k 或触及 hnsw.max_scan_tuples
// （默认 20000），并保证结果仍严格按距离有序，SQL 里 `ORDER BY ... LIMIT k` 的语义
// 因此成立。relaxed_order 更快但可能轻微乱序，需要调用方自己重排。
//
// 代价：多租户下迭代扫描会多访问索引节点，单次检索延迟略有上升（见
// benchmark/README.md）；单租户（目标库占满全表）时第一轮候选就已满足 LIMIT，
// 迭代扫描根本不会触发，因此没有额外开销。
//
// 需要注意的前提：hnsw.iterative_scan 是 pgvector 0.8.0 引入的，更老的版本会把它
// 当成未定义的占位 GUC 而静默忽略（不报错），退化成默认的漏召回行为。
//
// 规模再上一个量级时，更彻底的做法是按 knowledge_base_id 分区，让每个租户拥有
// 自己的 HNSW 索引，而不是「全局索引 + 后置过滤」。见《优化任务清单.md》P0-1 遗留项。
const hnswIterativeScan = "strict_order"

// SearchChunks 在指定知识库内做向量检索，返回与 queryVec 余弦相似度最高的 k 个分块。
//
// 这是 P0-1 的核心改动。原实现是 `SearchChunks(ctx, kbID)`——SQL 没有 LIMIT，
// 把整个知识库的分块（含 JSONB 序列化的 embedding）全量拉进进程，
// 再由 rag.Service 逐行 json.Unmarshal 成 []float64 算余弦。
// 1000 文档 × 50 分块时单次检索的传输与解析量在 300MB 量级，
// 而每次 RAG 节点执行都要重复一遍。
//
// 现在检索完全下推到数据库：HNSW 索引 + `ORDER BY <=> LIMIT k`，
// 只回传 k 行，且不回传向量本身（进程侧不再需要它）。
//
// Score 由数据库算：余弦相似度 = 1 - 余弦距离（<=> 返回的是距离）。
// 这样量纲与应用层实现（llm.Cosine）一致，前端展示无需区分后端。
func (r *KnowledgeRepo) SearchChunks(ctx context.Context, kbID int64, queryVec []float64, k int) ([]model.DocumentChunk, error) {
	if k <= 0 {
		k = 5
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	// 用显式事务包住查询：hnsw.iterative_scan 只能用 SET LOCAL 设置，
	// 会话级 SET 会随连接池的连接复用泄漏到同一连接上的其他查询。
	// 事务只读，结束直接回滚，不开销 WAL。
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = "+hnswIterativeScan); err != nil {
		return nil, fmt.Errorf("设置 hnsw.iterative_scan=%s: %w", hnswIterativeScan, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT c.id, c.document_id, c.content, c.chunk_index, c.created_at, d.filename,
		       1 - (c.embedding_v <=> $2::vector) AS score
		  FROM document_chunks c
		  JOIN documents d ON d.id = c.document_id
		 WHERE d.knowledge_base_id = $1
		   AND d.status = 'indexed'
		   AND c.embedding_v IS NOT NULL
		 ORDER BY c.embedding_v <=> $2::vector
		 LIMIT $3`, kbID, vecLiteral(queryVec), k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.DocumentChunk
	for rows.Next() {
		var c model.DocumentChunk
		if err := rows.Scan(&c.ID, &c.DocumentID, &c.Content, &c.ChunkIndex, &c.CreatedAt, &c.Filename, &c.Score); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// KB 归属校验：确认知识库属于该用户。
func (r *KnowledgeRepo) BelongsToUser(ctx context.Context, kbID, userID int64) (bool, error) {
	var one int
	err := r.db.Pool.QueryRow(ctx,
		`SELECT 1 FROM knowledge_bases WHERE id=$1 AND user_id=$2`, kbID, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

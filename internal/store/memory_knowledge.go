package store

import (
	"context"
	"sort"
	"time"

	"github.com/hoarfrost/nebulaflow/internal/llm"
	"github.com/hoarfrost/nebulaflow/internal/model"
)

// ---------- 知识库 ----------

type memKnowledgeRepo struct{ m *MemoryStore }

func (r *memKnowledgeRepo) CreateKB(_ context.Context, kb *model.KnowledgeBase) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	kb.ID = m.nextID()
	kb.CreatedAt = time.Now()
	c := *kb
	m.kbs[kb.ID] = &c
	return nil
}

func (r *memKnowledgeRepo) ListKBs(_ context.Context, userID int64) ([]model.KnowledgeBase, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.KnowledgeBase, 0, len(m.kbs))
	for _, kb := range m.kbs {
		if kb.UserID == userID {
			out = append(out, *kb)
		}
	}
	// ORDER BY created_at DESC
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// DeleteKB 级联清理文档与分块（对齐 PostgreSQL 的 ON DELETE CASCADE）。
func (r *memKnowledgeRepo) DeleteKB(_ context.Context, id, userID int64) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	kb, ok := m.kbs[id]
	if !ok || kb.UserID != userID {
		return ErrKBNotFound
	}
	delete(m.kbs, id)
	for docID, d := range m.docs {
		if d.KnowledgeBaseID == id {
			delete(m.docs, docID)
			delete(m.chunks, docID)
		}
	}
	return nil
}

func (r *memKnowledgeRepo) BelongsToUser(_ context.Context, kbID, userID int64) (bool, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	kb, ok := m.kbs[kbID]
	return ok && kb.UserID == userID, nil
}

// ---------- 文档 ----------

func (r *memKnowledgeRepo) CreateDocument(_ context.Context, d *model.Document) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	d.ID = m.nextID()
	d.CreatedAt = time.Now()
	m.docs[d.ID] = copyDoc(d)
	return nil
}

// ListDocuments 不返回 content（对齐 PostgreSQL 版只 SELECT 元信息列）。
func (r *memKnowledgeRepo) ListDocuments(_ context.Context, kbID int64) ([]model.Document, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.Document, 0)
	for _, d := range m.docs {
		if d.KnowledgeBaseID == kbID {
			c := *d
			c.Content = "" // 列表不返回正文
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (r *memKnowledgeRepo) GetDocument(_ context.Context, docID int64) (*model.Document, error) {
	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	d, ok := m.docs[docID]
	if !ok {
		return nil, ErrDocNotFound
	}
	return copyDoc(d), nil
}

func (r *memKnowledgeRepo) UpdateDocumentStatus(_ context.Context, docID int64, status model.DocumentStatus) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.docs[docID]
	if !ok {
		return ErrDocNotFound
	}
	d.Status = status
	return nil
}

// ---------- 分块 ----------

func (r *memKnowledgeRepo) DeleteChunksByDocument(_ context.Context, docID int64) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.chunks, docID)
	return nil
}

func (r *memKnowledgeRepo) InsertChunk(_ context.Context, c *model.DocumentChunk) error {
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	c.ID = m.nextID()
	c.CreatedAt = time.Now()
	m.chunks[c.DocumentID] = append(m.chunks[c.DocumentID], copyChunk(*c))
	return nil
}

func (r *memKnowledgeRepo) BatchInsertChunks(_ context.Context, chunks []model.DocumentChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	m := r.m
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for _, c := range chunks {
		c.ID = m.nextID()
		c.CreatedAt = now
		m.chunks[c.DocumentID] = append(m.chunks[c.DocumentID], copyChunk(c))
	}
	return nil
}

// SearchChunks 是 PostgreSQL 版的进程内等价实现：同样只返回 Top-K 且带 Score。
//
// 内存后端保留应用层余弦是刻意的，不是"还没迁移"：
// 它的定位是零外部依赖的演示/测试后端，数据量在演示规模（几百个分块），
// 进程内扫描比维护一份索引更简单，也没有网络传输成本。
// 但**接口语义必须与 PostgreSQL 版完全一致**（Top-K、按相似度降序、Score 量纲相同），
// 否则同一份业务代码在两个后端下行为不同，测试就失去了意义。
func (r *memKnowledgeRepo) SearchChunks(_ context.Context, kbID int64, queryVec []float64, k int) ([]model.DocumentChunk, error) {
	if k <= 0 {
		k = 5
	}
	if len(queryVec) == 0 {
		return nil, nil
	}

	m := r.m
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.DocumentChunk, 0)
	// 按文档 ID 升序遍历，保证同分时顺序稳定（与 PostgreSQL 的 ORDER BY 语义对齐）
	docIDs := make([]int64, 0, len(m.docs))
	for id := range m.docs {
		docIDs = append(docIDs, id)
	}
	sort.Slice(docIDs, func(i, j int) bool { return docIDs[i] < docIDs[j] })

	for _, docID := range docIDs {
		d := m.docs[docID]
		if d.KnowledgeBaseID != kbID || d.Status != model.DocIndexed {
			continue
		}
		for _, c := range m.chunks[docID] {
			if len(c.Embedding) == 0 {
				continue
			}
			cc := copyChunk(c)
			cc.Filename = d.Filename
			cc.Score = llm.Cosine(queryVec, c.Embedding)
			// 检索结果不携带向量本身：PostgreSQL 版按列不回传 embedding_v，
			// 这里也必须一致，否则"两个后端行为相同"的承诺就破了。
			cc.Embedding = nil
			out = append(out, cc)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

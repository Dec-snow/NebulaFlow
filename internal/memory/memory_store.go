package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// MemoryShortTermStore 是内存版短期记忆（测试 / 无 Redis 环境用）。
type MemoryShortTermStore struct {
	mu      sync.RWMutex
	sessions map[string][]MemoryMessage
}

func NewMemoryShortTermStore() *MemoryShortTermStore {
	return &MemoryShortTermStore{
		sessions: make(map[string][]MemoryMessage),
	}
}

func (s *MemoryShortTermStore) Append(_ context.Context, sessionID string, msg MemoryMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	s.sessions[sessionID] = append(s.sessions[sessionID], msg)
	// 最多 100 条
	if len(s.sessions[sessionID]) > 100 {
		s.sessions[sessionID] = s.sessions[sessionID][len(s.sessions[sessionID])-100:]
	}
	return nil
}

func (s *MemoryShortTermStore) GetAll(_ context.Context, sessionID string) ([]MemoryMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := s.sessions[sessionID]
	out := make([]MemoryMessage, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (s *MemoryShortTermStore) GetLastN(_ context.Context, sessionID string, n int) ([]MemoryMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msgs := s.sessions[sessionID]
	if n <= 0 || n >= len(msgs) {
		out := make([]MemoryMessage, len(msgs))
		copy(out, msgs)
		return out, nil
	}
	out := make([]MemoryMessage, n)
	copy(out, msgs[len(msgs)-n:])
	return out, nil
}

func (s *MemoryShortTermStore) Clear(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
	return nil
}

func (s *MemoryShortTermStore) SetTTL(_ context.Context, _ string, _ time.Duration) error {
	// 内存版不支持 TTL（简单起见），忽略即可
	return nil
}

// ---------- 长期记忆内存版 ----------

// MemoryLongTermStore 是内存版长期记忆（测试用）。
// 用余弦相似度做向量检索，逻辑和 pgvector 一致。
type MemoryLongTermStore struct {
	mu    sync.RWMutex
	items []longTermEntry
}

type longTermEntry struct {
	item      MemoryItem
	embedding []float32
}

func NewMemoryLongTermStore() *MemoryLongTermStore {
	return &MemoryLongTermStore{}
}

func (s *MemoryLongTermStore) Store(_ context.Context, item MemoryItem, embedding []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.ID == "" {
		item.ID = fmt.Sprintf("mem_%d", len(s.items)+1)
	}
	if item.Timestamp.IsZero() {
		item.Timestamp = time.Now()
	}
	s.items = append(s.items, longTermEntry{
		item:      item,
		embedding: append([]float32(nil), embedding...),
	})
	return nil
}

func (s *MemoryLongTermStore) Search(_ context.Context, userID int64, queryEmbedding []float32, topK int) ([]MemoryItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type scored struct {
		item  MemoryItem
		score float64
	}

	var results []scored
	for _, e := range s.items {
		if e.item.UserID != userID {
			continue
		}
		cos := cosineSimilarity(queryEmbedding, e.embedding)
		results = append(results, scored{item: e.item, score: cos})
	}

	// 按分数降序
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if topK <= 0 || topK > len(results) {
		topK = len(results)
	}
	out := make([]MemoryItem, topK)
	for i := 0; i < topK; i++ {
		out[i] = results[i].item
		out[i].Score = results[i].score
	}
	return out, nil
}

func (s *MemoryLongTermStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.items {
		if e.item.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *MemoryLongTermStore) ListByCategory(_ context.Context, userID int64, category string, limit int) ([]MemoryItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []MemoryItem
	for _, e := range s.items {
		if e.item.UserID != userID {
			continue
		}
		if category != "" && e.item.Category != category {
			continue
		}
		out = append(out, e.item)
	}
	// 按时间倒序
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// cosineSimilarity 计算两个向量的余弦相似度。
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

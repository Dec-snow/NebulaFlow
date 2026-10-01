package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisShortTermStore 是基于 Redis List 的短期记忆实现。
//
// 数据结构：
//   Key:   memory:st:{sessionID}
//   Value: JSON 数组（每条消息一个 JSON）
//   TTL:   会话过期时间
//
// 为什么用 List 而不是 Hash/Set：
//   - 消息是有序的（按时间顺序）
//   - 我们经常需要"最近 N 条"（LRANGE -N -1）
//   - List 的 LPUSH + LTRIM 组合非常适合这种场景
type RedisShortTermStore struct {
	rdb     *redis.Client
	keyPrefix string
}

// NewRedisShortTermStore 创建一个 Redis 短期记忆存储。
func NewRedisShortTermStore(rdb *redis.Client, keyPrefix string) *RedisShortTermStore {
	if keyPrefix == "" {
		keyPrefix = "memory:st"
	}
	return &RedisShortTermStore{
		rdb:       rdb,
		keyPrefix: keyPrefix,
	}
}

func (s *RedisShortTermStore) key(sessionID string) string {
	return fmt.Sprintf("%s:%s", s.keyPrefix, sessionID)
}

// Append 追加一条消息到会话记忆（LPUSH + LTRIM 保持最多 100 条）。
func (s *RedisShortTermStore) Append(ctx context.Context, sessionID string, msg MemoryMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal memory message: %w", err)
	}

	key := s.key(sessionID)
	// LPUSH 到左边（最新的在最前面）
	if err := s.rdb.LPush(ctx, key, data).Err(); err != nil {
		return fmt.Errorf("redis LPUSH: %w", err)
	}
	// 最多保留 100 条（防止无限增长）
	s.rdb.LTrim(ctx, key, 0, 99)
	return nil
}

// GetAll 获取会话的全部消息（按时间顺序：旧 → 新）。
func (s *RedisShortTermStore) GetAll(ctx context.Context, sessionID string) ([]MemoryMessage, error) {
	return s.GetLastN(ctx, sessionID, -1)
}

// GetLastN 获取最近的 N 条消息（按时间顺序：旧 → 新）。
// n = -1 表示全部。
func (s *RedisShortTermStore) GetLastN(ctx context.Context, sessionID string, n int) ([]MemoryMessage, error) {
	key := s.key(sessionID)

	var raw []string
	var err error
	if n <= 0 {
		raw, err = s.rdb.LRange(ctx, key, 0, -1).Result()
	} else {
		// LPUSH 是往左边加，所以最新的在 index 0
		// 要"最近 N 条"就是 0 ~ N-1
		raw, err = s.rdb.LRange(ctx, key, 0, int64(n)-1).Result()
	}
	if err != nil {
		return nil, fmt.Errorf("redis LRANGE: %w", err)
	}

	// raw 是新 → 旧，要翻成旧 → 新
	msgs := make([]MemoryMessage, len(raw))
	for i, r := range raw {
		var msg MemoryMessage
		if err := json.Unmarshal([]byte(r), &msg); err != nil {
			// 坏数据就跳过，不影响整体
			continue
		}
		// 倒序放：raw[0] 是最新的，应该在最后面
		msgs[len(raw)-1-i] = msg
	}
	return msgs, nil
}

// Clear 清空会话记忆。
func (s *RedisShortTermStore) Clear(ctx context.Context, sessionID string) error {
	return s.rdb.Del(ctx, s.key(sessionID)).Err()
}

// SetTTL 设置会话的过期时间。
func (s *RedisShortTermStore) SetTTL(ctx context.Context, sessionID string, ttl time.Duration) error {
	return s.rdb.Expire(ctx, s.key(sessionID), ttl).Err()
}

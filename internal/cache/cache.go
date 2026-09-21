// Package cache 提供热点配置的缓存抽象（Redis / 内存）。
//
// 用法：workflow:123 / agent:123 / provider 列表等只读热数据。
package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

type RedisCache struct {
	rdb redis.UniversalClient
}

func NewRedisCache(rdb redis.UniversalClient) *RedisCache {
	return &RedisCache{rdb: rdb}
}

func (c *RedisCache) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (c *RedisCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *RedisCache) Del(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, key).Err()
}

type MemoryCache struct {
	mu   chan struct{}
	data map[string]cacheItem
}

type cacheItem struct {
	value  string
	expire time.Time
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{mu: make(chan struct{}, 1), data: map[string]cacheItem{}}
}

func (c *MemoryCache) Get(_ context.Context, key string) (string, bool, error) {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	it, ok := c.data[key]
	if !ok {
		return "", false, nil
	}
	if time.Now().After(it.expire) {
		delete(c.data, key)
		return "", false, nil
	}
	return it.value, true, nil
}

func (c *MemoryCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	c.data[key] = cacheItem{value: value, expire: time.Now().Add(ttl)}
	return nil
}

func (c *MemoryCache) Del(_ context.Context, key string) error {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	delete(c.data, key)
	return nil
}

package service

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisTranslationCache 使用 Redis 缓存翻译结果
type RedisTranslationCache struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisTranslationCache 创建 Redis 翻译缓存
func NewRedisTranslationCache(client *redis.Client, ttlSeconds int) *RedisTranslationCache {
	ttl := 24 * time.Hour
	if ttlSeconds > 0 {
		ttl = time.Duration(ttlSeconds) * time.Second
	}
	return &RedisTranslationCache{
		client: client,
		ttl:    ttl,
	}
}

// GetTranslation 获取缓存的翻译结果
func (c *RedisTranslationCache) GetTranslation(ctx context.Context, key string) (string, error) {
	val, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

// SetTranslation 缓存翻译结果
func (c *RedisTranslationCache) SetTranslation(ctx context.Context, key, value string) error {
	return c.client.Set(ctx, key, value, c.ttl).Err()
}

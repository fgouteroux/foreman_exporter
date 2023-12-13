package memcache

import (
	"sync"
	"time"
)

type MemCache struct {
	data map[string]CacheValue
	lock sync.Mutex
}

type CacheValue struct {
	Value     interface{}
	ExpiresAt time.Time
}

func NewLocalCache() *MemCache {
	return &MemCache{
		data: make(map[string]CacheValue),
	}
}

func (c *MemCache) Set(key string, value interface{}, expiration time.Duration) {
	c.lock.Lock()
	defer c.lock.Unlock()

	expires := time.Now().Add(expiration)
	c.data[key] = CacheValue{
		Value:     value,
		ExpiresAt: expires,
	}
}

func (c *MemCache) Get(key string) (CacheValue, bool) {
	c.lock.Lock()
	defer c.lock.Unlock()

	value, found := c.data[key]
	return value, found
}

func (c *MemCache) Del(key string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.data, key)
}

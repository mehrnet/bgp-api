package api

import (
	"container/list"
	"sync"
)

const (
	defaultCompactResponseCacheBytes = 256 << 20
	cacheEntryOverhead               = 128
	compactResponseCacheShards       = 32
)

type compactResponseCache struct {
	shards [compactResponseCacheShards]compactResponseCacheShard
}

type compactResponseCacheShard struct {
	mu      sync.Mutex
	budget  int
	used    int
	entries map[string]*list.Element
	lru     *list.List
}

type compactResponseCacheEntry struct {
	key   string
	value []byte
	size  int
}

func newCompactResponseCache(budget int) *compactResponseCache {
	if budget == 0 {
		budget = defaultCompactResponseCacheBytes
	}
	if budget < 1 {
		return nil
	}
	cache := &compactResponseCache{}
	perShard := budget / compactResponseCacheShards
	remainder := budget % compactResponseCacheShards
	for index := range cache.shards {
		shardBudget := perShard
		if index < remainder {
			shardBudget++
		}
		cache.shards[index] = compactResponseCacheShard{
			budget:  shardBudget,
			entries: make(map[string]*list.Element),
			lru:     list.New(),
		}
	}
	return cache
}

func (cache *compactResponseCache) Get(key string) ([]byte, bool) {
	if cache == nil {
		return nil, false
	}
	shard := cache.shard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	element, ok := shard.entries[key]
	if !ok {
		return nil, false
	}
	shard.lru.MoveToFront(element)
	return element.Value.(compactResponseCacheEntry).value, true
}

func (cache *compactResponseCache) Add(key string, value []byte) {
	if cache == nil {
		return
	}
	shard := cache.shard(key)
	size := len(key) + len(value) + cacheEntryOverhead
	if size > shard.budget {
		return
	}
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if existing, ok := shard.entries[key]; ok {
		entry := existing.Value.(compactResponseCacheEntry)
		shard.used -= entry.size
		shard.lru.Remove(existing)
	}
	entry := compactResponseCacheEntry{key: key, value: value, size: size}
	shard.entries[key] = shard.lru.PushFront(entry)
	shard.used += size
	for shard.used > shard.budget {
		tail := shard.lru.Back()
		entry := tail.Value.(compactResponseCacheEntry)
		delete(shard.entries, entry.key)
		shard.used -= entry.size
		shard.lru.Remove(tail)
	}
}

func (cache *compactResponseCache) Len() int {
	if cache == nil {
		return 0
	}
	count := 0
	for index := range cache.shards {
		shard := &cache.shards[index]
		shard.mu.Lock()
		count += shard.lru.Len()
		shard.mu.Unlock()
	}
	return count
}

func (cache *compactResponseCache) shard(key string) *compactResponseCacheShard {
	var hash uint32 = 2_166_136_261
	for index := 0; index < len(key); index++ {
		hash ^= uint32(key[index])
		hash *= 16_777_619
	}
	return &cache.shards[hash&(compactResponseCacheShards-1)]
}

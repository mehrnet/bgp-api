package api

import (
	"container/list"
	"sync"
	"sync/atomic"
)

const (
	defaultCompactResponseCacheBytes   = 256 << 20
	defaultResourceResponseCacheBytes  = 64 << 20
	maxCompactResponseCacheEntryBytes  = 16 << 10
	maxResourceResponseCacheEntryBytes = 64 << 10
	cacheEntryOverhead                 = 128
	responseCacheShards                = 32
)

// responseCache stores immutable serialized API responses. It is sharded for
// concurrent hits and has a tiny per-key flight table to collapse cache misses.
type responseCache struct {
	shards   [responseCacheShards]responseCacheShard
	flights  map[string]*responseCacheFlight
	flightsM sync.Mutex
	enabled  atomic.Bool
}

type responseCacheShard struct {
	mu       sync.Mutex
	budget   int
	used     int
	maxEntry int
	entries  map[string]*list.Element
	lru      *list.List
}

type responseCacheEntry struct {
	key   string
	value []byte
	size  int
}

type responseCacheFlight struct{ done chan struct{} }

type responseCacheUsage struct {
	Entries int
	Bytes   int
}

func newCompactResponseCache(budget int) *responseCache {
	return newResponseCache(budget, defaultCompactResponseCacheBytes, maxCompactResponseCacheEntryBytes)
}

func newResourceResponseCache(budget int) *responseCache {
	return newResponseCache(budget, defaultResourceResponseCacheBytes, maxResourceResponseCacheEntryBytes)
}

func newResponseCache(budget, fallback, maxEntry int) *responseCache {
	if budget == 0 {
		budget = fallback
	}
	if budget < 1 {
		return nil
	}
	cache := &responseCache{flights: make(map[string]*responseCacheFlight)}
	cache.enabled.Store(true)
	perShard := budget / responseCacheShards
	remainder := budget % responseCacheShards
	for index := range cache.shards {
		shardBudget := perShard
		if index < remainder {
			shardBudget++
		}
		cache.shards[index] = responseCacheShard{
			budget: shardBudget, maxEntry: maxEntry, entries: make(map[string]*list.Element), lru: list.New(),
		}
	}
	return cache
}

func (cache *responseCache) Get(key string) ([]byte, bool) {
	if cache == nil || !cache.enabled.Load() {
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
	return element.Value.(responseCacheEntry).value, true
}

// Acquire returns a cached response or makes the caller the single producer
// for key. Call the returned release function exactly once for a producer.
func (cache *responseCache) Acquire(key string) (value []byte, cached bool, release func()) {
	if cache == nil || !cache.enabled.Load() {
		return nil, false, func() {}
	}
	for {
		if !cache.enabled.Load() {
			return nil, false, func() {}
		}
		if value, cached := cache.Get(key); cached {
			return value, true, nil
		}
		cache.flightsM.Lock()
		if !cache.enabled.Load() {
			cache.flightsM.Unlock()
			return nil, false, func() {}
		}
		// A leader can publish between the first cache check and this lock.
		// Recheck while holding the flight lock so a late arrival cannot create
		// a second producer after the completed flight has been removed.
		if value, cached := cache.Get(key); cached {
			cache.flightsM.Unlock()
			return value, true, nil
		}
		flight, waiting := cache.flights[key]
		if !waiting {
			flight = &responseCacheFlight{done: make(chan struct{})}
			cache.flights[key] = flight
			cache.flightsM.Unlock()
			return nil, false, func() {
				cache.flightsM.Lock()
				delete(cache.flights, key)
				close(flight.done)
				cache.flightsM.Unlock()
			}
		}
		cache.flightsM.Unlock()
		<-flight.done
	}
}

func (cache *responseCache) Add(key string, value []byte) {
	if cache == nil || !cache.enabled.Load() {
		return
	}
	shard := cache.shard(key)
	size := len(key) + len(value) + cacheEntryOverhead
	if len(value) > shard.maxEntry || size > shard.budget {
		return
	}
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if !cache.enabled.Load() {
		return
	}
	if existing, ok := shard.entries[key]; ok {
		entry := existing.Value.(responseCacheEntry)
		shard.used -= entry.size
		shard.lru.Remove(existing)
	}
	entry := responseCacheEntry{key: key, value: value, size: size}
	shard.entries[key] = shard.lru.PushFront(entry)
	shard.used += size
	for shard.used > shard.budget {
		tail := shard.lru.Back()
		entry := tail.Value.(responseCacheEntry)
		delete(shard.entries, entry.key)
		shard.used -= entry.size
		shard.lru.Remove(tail)
	}
}

func (cache *responseCache) Len() int {
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

func (cache *responseCache) Enable() {
	if cache != nil {
		cache.enabled.Store(true)
	}
}

// DisableAndClear prevents new entries and discards all serialized responses.
// Existing requests continue safely; their producer may finish, but it cannot
// repopulate a disabled cache.
func (cache *responseCache) DisableAndClear() responseCacheUsage {
	if cache == nil {
		return responseCacheUsage{}
	}
	cache.enabled.Store(false)
	var usage responseCacheUsage
	for index := range cache.shards {
		shard := &cache.shards[index]
		shard.mu.Lock()
		usage.Entries += shard.lru.Len()
		usage.Bytes += shard.used
		shard.entries = make(map[string]*list.Element)
		shard.lru = list.New()
		shard.used = 0
		shard.mu.Unlock()
	}
	return usage
}

func (cache *responseCache) shard(key string) *responseCacheShard {
	var hash uint32 = 2_166_136_261
	for index := 0; index < len(key); index++ {
		hash ^= uint32(key[index])
		hash *= 16_777_619
	}
	return &cache.shards[hash&(responseCacheShards-1)]
}

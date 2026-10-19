package main

import (
	"sync"
	"time"
)

// boxes JPEG LRU-ish cache (serve-time only): key is built by the caller as
// path+"|"+mtime+"|"+vision_at. Bounded at 64 entries with a 30s TTL;
// eviction deletes expired entries first, else one arbitrary (oldest via
// range) entry.

type boxesCacheEntry struct {
	data []byte
	exp  time.Time
}

type boxesCache struct {
	mu      sync.Mutex
	entries map[string]boxesCacheEntry
}

var globalBoxesCache = &boxesCache{entries: map[string]boxesCacheEntry{}}

const boxesCacheMax = 64
const boxesCacheTTL = 30 * time.Second

func boxesCacheGet(key string) ([]byte, bool) {
	globalBoxesCache.mu.Lock()
	defer globalBoxesCache.mu.Unlock()
	e, ok := globalBoxesCache.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.exp) {
		delete(globalBoxesCache.entries, key)
		return nil, false
	}
	return e.data, true
}

func boxesCacheSet(key string, data []byte) {
	globalBoxesCache.mu.Lock()
	defer globalBoxesCache.mu.Unlock()
	var now = time.Now()
	for k, e := range globalBoxesCache.entries {
		if now.After(e.exp) {
			delete(globalBoxesCache.entries, k)
		}
	}
	for len(globalBoxesCache.entries) >= boxesCacheMax {
		for k := range globalBoxesCache.entries {
			delete(globalBoxesCache.entries, k)
			break
		}
	}
	if globalBoxesCache.entries == nil {
		globalBoxesCache.entries = map[string]boxesCacheEntry{}
	}
	globalBoxesCache.entries[key] = boxesCacheEntry{data: data, exp: now.Add(boxesCacheTTL)}
}

package main

import (
	"sync"
	"time"
)

// boxes JPEG cache (serve-time only): key is built by the caller as
// path+"|"+mtime+"|"+vision_at (+ optional max size). Bounded by both entry
// count and total bytes: a handful of 4K annotated snapshots can otherwise
// pin hundreds of MB of RAM. Entries expire after 30s.
//
// A small semaphore also bounds concurrent JPEG decode/draw operations: each
// full-size RGBA frame is tens of MB, so a page loading many thumbnails must
// not decode them all at once.
type boxesCacheEntry struct {
	data []byte
	exp  time.Time
	at   time.Time
}

type boxesCache struct {
	mu      sync.Mutex
	entries map[string]boxesCacheEntry
	bytes   int64
}

var globalBoxesCache = &boxesCache{entries: map[string]boxesCacheEntry{}}

const (
	boxesCacheMax      = 128
	boxesCacheMaxBytes = 32 << 20 // 32 MB
	boxesCacheMaxEntry = 16 << 20 // don't cache a single >16 MB render
	boxesCacheTTL      = 30 * time.Second
)

// boxesRenderSem caps concurrent snapshot decodes/draws at 2: ~2 full-size
// RGBA buffers in flight, even when the browser requests many thumbnails.
var boxesRenderSem = make(chan struct{}, 2)

func boxesRenderAcquire() { boxesRenderSem <- struct{}{} }
func boxesRenderRelease() { <-boxesRenderSem }

func boxesCacheGet(key string) ([]byte, bool) {
	globalBoxesCache.mu.Lock()
	defer globalBoxesCache.mu.Unlock()
	e, ok := globalBoxesCache.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.exp) {
		delete(globalBoxesCache.entries, key)
		globalBoxesCache.bytes -= int64(len(e.data))
		return nil, false
	}
	return e.data, true
}

func boxesCacheSet(key string, data []byte) {
	if len(data) == 0 || len(data) > boxesCacheMaxEntry {
		return
	}
	globalBoxesCache.mu.Lock()
	defer globalBoxesCache.mu.Unlock()
	var now = time.Now()
	// Drop expired entries first.
	for k, e := range globalBoxesCache.entries {
		if now.After(e.exp) {
			delete(globalBoxesCache.entries, k)
			globalBoxesCache.bytes -= int64(len(e.data))
		}
	}
	// Evict the oldest entries until the new one fits.
	for len(globalBoxesCache.entries) >= boxesCacheMax ||
		globalBoxesCache.bytes+int64(len(data)) > boxesCacheMaxBytes {
		var oldestKey string
		var oldestAt time.Time
		for k, e := range globalBoxesCache.entries {
			if oldestKey == "" || e.at.Before(oldestAt) {
				oldestKey, oldestAt = k, e.at
			}
		}
		if oldestKey == "" {
			break
		}
		globalBoxesCache.bytes -= int64(len(globalBoxesCache.entries[oldestKey].data))
		delete(globalBoxesCache.entries, oldestKey)
	}
	if globalBoxesCache.entries == nil {
		globalBoxesCache.entries = map[string]boxesCacheEntry{}
	}
	globalBoxesCache.entries[key] = boxesCacheEntry{data: data, exp: now.Add(boxesCacheTTL), at: now}
	globalBoxesCache.bytes += int64(len(data))
}

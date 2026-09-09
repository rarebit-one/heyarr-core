package opensubtitles

import (
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// searchCache memoises a subtitle SEARCH for a short window (ADR-0085).
//
// A search is cheap but not free against opensubtitles.com's request budget, and
// a library-scoped subtitle backfill re-asks the same query as it retries a want
// — the same episode's search runs again on the next reconcile. Caching the
// candidates for a few minutes turns that repetition into one call, the same
// argument internal/indexers makes for caching its capabilities handshake, the
// one piece of response-cache prior art in the tree.
//
// Only the SEARCH is cached, never the resolve: a resolve mints a per-fetch
// download link and spends the daily quota, so it must reach the service every
// time. The cache holds neutral candidates, not the wire body, so nothing that
// leaves it is coupled to OpenSubtitles' JSON.
type searchCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]searchCacheEntry
}

type searchCacheEntry struct {
	candidates []providers.SubtitleCandidate
	at         time.Time
}

func newSearchCache(ttl time.Duration, now func() time.Time) *searchCache {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &searchCache{ttl: ttl, now: now, entries: make(map[string]searchCacheEntry)}
}

// get returns the cached candidates for a key and whether they are still fresh.
// A miss and an expired entry both report ok=false; an expired entry is dropped
// so the map does not grow without bound.
func (c *searchCache) get(key string) ([]providers.SubtitleCandidate, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if c.now().Sub(e.at) >= c.ttl {
		delete(c.entries, key)
		return nil, false
	}
	// Copy out, so a caller that sorts or trims its result cannot mutate the
	// slice a later cache hit will hand to someone else.
	out := make([]providers.SubtitleCandidate, len(e.candidates))
	copy(out, e.candidates)
	return out, true
}

// put stores candidates under a key with the current time. A copy is kept for
// the same reason get copies out.
func (c *searchCache) put(key string, candidates []providers.SubtitleCandidate) {
	if c.ttl <= 0 {
		return
	}
	stored := make([]providers.SubtitleCandidate, len(candidates))
	copy(stored, candidates)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = searchCacheEntry{candidates: stored, at: c.now()}
}

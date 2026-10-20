// Package cache is a small TTL cache for packed upstream DNS responses plus a
// minimal singleflight, both stdlib-only.
package cache

import (
	"sync"
	"time"
)

// Entry is a cached, packed DNS message.
type Entry struct {
	Msg     []byte    // packed wire-format response
	Stored  time.Time // when it was stored; used to decrement TTLs on read
	Expires time.Time
}

// Cache is a bounded TTL cache. When full, expired entries are evicted first;
// if none are expired, the entry closest to expiry is evicted.
type Cache struct {
	mu   sync.Mutex
	max  int
	m    map[string]Entry
	now  func() time.Time
	hits uint64
	miss uint64
}

// New returns a cache holding at most max entries (max <= 0 means 10000).
func New(max int) *Cache {
	if max <= 0 {
		max = 10000
	}
	return &Cache{max: max, m: make(map[string]Entry, 256), now: time.Now}
}

// Get returns a live entry.
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		c.miss++
		return Entry{}, false
	}
	if !c.now().Before(e.Expires) {
		delete(c.m, key)
		c.miss++
		return Entry{}, false
	}
	c.hits++
	return e, true
}

// Set stores msg for ttl. A zero or negative ttl is ignored.
func (c *Cache) Set(key string, msg []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[key]; !exists && len(c.m) >= c.max {
		c.evictLocked(now)
	}
	c.m[key] = Entry{Msg: msg, Stored: now, Expires: now.Add(ttl)}
}

func (c *Cache) evictLocked(now time.Time) {
	removed := 0
	for k, e := range c.m {
		if !now.Before(e.Expires) {
			delete(c.m, k)
			removed++
		}
	}
	if removed > 0 {
		return
	}
	var victim string
	var soonest time.Time
	for k, e := range c.m {
		if victim == "" || e.Expires.Before(soonest) {
			victim, soonest = k, e.Expires
		}
	}
	delete(c.m, victim)
}

// Sweep removes expired entries. Call periodically.
func (c *Cache) Sweep() int {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.m {
		if !now.Before(e.Expires) {
			delete(c.m, k)
			n++
		}
	}
	return n
}

// Flush removes everything.
func (c *Cache) Flush() {
	c.mu.Lock()
	c.m = make(map[string]Entry, 256)
	c.mu.Unlock()
}

// Stats returns size, hits and misses.
func (c *Cache) Stats() (size int, hits, misses uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m), c.hits, c.miss
}

// Group deduplicates concurrent calls with the same key (singleflight).
type Group[T any] struct {
	mu sync.Mutex
	m  map[string]*call[T]
}

type call[T any] struct {
	wg  sync.WaitGroup
	val T
	err error
}

// Do runs fn once per key among concurrent callers; shared reports whether the
// result came from another caller's execution.
func (g *Group[T]) Do(key string, fn func() (T, error)) (v T, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*call[T]{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &call[T]{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		c.wg.Done()
	}()
	c.val, c.err = fn()
	return c.val, c.err, false
}

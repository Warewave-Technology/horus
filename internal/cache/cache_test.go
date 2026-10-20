package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	c := New(10)
	c.now = func() time.Time { return now }

	c.Set("a", []byte("x"), 10*time.Second)
	c.Set("zero", []byte("x"), 0)
	if _, ok := c.Get("zero"); ok {
		t.Fatal("zero ttl must not be cached")
	}
	if e, ok := c.Get("a"); !ok || string(e.Msg) != "x" || !e.Stored.Equal(now) {
		t.Fatalf("expected hit, got %+v %v", e, ok)
	}
	now = now.Add(10 * time.Second)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expected expiry")
	}
	if size, hits, misses := c.Stats(); size != 0 || hits != 1 || misses != 2 {
		t.Fatalf("stats: %d %d %d", size, hits, misses)
	}
}

func TestCacheEviction(t *testing.T) {
	now := time.Unix(1000, 0)
	c := New(2)
	c.now = func() time.Time { return now }
	c.Set("long", nil, time.Hour)
	c.Set("short", nil, time.Minute)
	c.Set("new", nil, time.Hour) // evicts "short" (closest to expiry)
	if _, ok := c.Get("short"); ok {
		t.Fatal("short should have been evicted")
	}
	if _, ok := c.Get("long"); !ok {
		t.Fatal("long should remain")
	}
	now = now.Add(2 * time.Hour)
	if n := c.Sweep(); n != 2 {
		t.Fatalf("sweep removed %d", n)
	}
}

func TestGroupDedup(t *testing.T) {
	var g Group[int]
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]int, 10)
	started := make(chan struct{})
	var once sync.Once
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, _, _ := g.Do("k", func() (int, error) {
				calls.Add(1)
				once.Do(func() { close(started) })
				<-release
				return 42, nil
			})
			results[i] = v
		}(i)
	}
	<-started
	time.Sleep(20 * time.Millisecond) // let the others pile up on the in-flight call
	close(release)
	wg.Wait()
	if calls.Load() > 2 { // scheduling may let a late goroutine start a 2nd call
		t.Fatalf("fn called %d times", calls.Load())
	}
	for _, v := range results {
		if v != 42 {
			t.Fatalf("results: %v", results)
		}
	}
}

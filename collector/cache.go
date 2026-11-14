package collector

import (
	"context"
	"sync"
	"time"
)

// cache keeps the last result of a fetch for a fixed time.
//
// The Amber API allows 50 requests in 5 minutes, and Prometheus can scrape
// more often than that. Each collector therefore asks the API for new data
// only when its cached data is older than the TTL.
//
// A failed fetch is also remembered, for the shorter retry time, so that a
// broken API is not asked again on every scrape. Expired data is never
// served: when a fetch fails, the collector reports the failure.
type cache[T any] struct {
	ttl   time.Duration
	retry time.Duration
	now   func() time.Time

	mu      sync.Mutex
	value   T
	fetched time.Time
	valid   bool
	err     error
	failed  time.Time
}

// maxRetry is the longest time a failed fetch is remembered.
const maxRetry = 30 * time.Second

func newCache[T any](ttl time.Duration, now func() time.Time) *cache[T] {
	return &cache[T]{ttl: ttl, retry: min(ttl, maxRetry), now: now}
}

// get returns the cached value when it is fresh. If it is not, get calls
// fetch. Concurrent callers wait for one fetch.
func (c *cache[T]) get(ctx context.Context, fetch func(context.Context) (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.valid && now.Sub(c.fetched) < c.ttl {
		return c.value, nil
	}
	var zero T
	if c.err != nil && now.Sub(c.failed) < c.retry {
		return zero, c.err
	}

	v, err := fetch(ctx)
	if err != nil {
		c.valid = false
		c.value = zero
		// A cancelled scrape says nothing about the API, so do not remember it.
		if ctx.Err() == nil {
			c.err, c.failed = err, c.now()
		}
		return zero, err
	}
	c.value, c.fetched, c.valid, c.err = v, c.now(), true, nil
	return v, nil
}

// siteCaches holds one cache for each site.
type siteCaches[T any] struct {
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	caches map[string]*cache[T]
}

func newSiteCaches[T any](ttl time.Duration, now func() time.Time) *siteCaches[T] {
	return &siteCaches[T]{ttl: ttl, now: now, caches: map[string]*cache[T]{}}
}

func (s *siteCaches[T]) get(ctx context.Context, siteID string, fetch func(context.Context) (T, error)) (T, error) {
	s.mu.Lock()
	c, ok := s.caches[siteID]
	if !ok {
		c = newCache[T](s.ttl, s.now)
		s.caches[siteID] = c
	}
	s.mu.Unlock()
	return c.get(ctx, fetch)
}

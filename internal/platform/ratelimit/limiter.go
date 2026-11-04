// Package ratelimit limits requests per client IP with an in-memory token bucket.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type Limiter struct {
	rate  float64
	burst float64
	now   func() time.Time

	maxKeys int

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

const (
	sweepEvery     = time.Minute
	defaultMaxKeys = 10000
)

type Option func(*Limiter)

// WithMaxKeys caps how many clients are tracked at once. When the map is
// full of clients that have not refilled yet, new clients are refused
// instead of growing memory.
func WithMaxKeys(n int) Option { return func(l *Limiter) { l.maxKeys = n } }

// New refills rate tokens per second up to burst; each Allow spends one.
func New(rate float64, burst int, now func() time.Time, opts ...Option) *Limiter {
	l := &Limiter{rate: rate, burst: float64(burst), now: now, maxKeys: defaultMaxKeys, buckets: map[string]*bucket{}, lastSweep: now()}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			return false
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = l.refilled(b, now)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *Limiter) refilled(b *bucket, now time.Time) float64 {
	return min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
}

// sweep drops buckets that have refilled completely: a missing bucket starts
// full, so forgetting them changes nothing and bounds memory.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		if l.refilled(b, now) >= l.burst {
			delete(l.buckets, key)
		}
	}
}

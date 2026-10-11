// Package limit is a token-bucket rate limiter keyed by string: a token,
// a user, a client address. It is in memory, so a budget is per process;
// blygd runs one.
package limit

import (
	"math"
	"sync"
	"time"
)

// Rate is a budget: Burst requests at once, refilled at PerMinute.
type Rate struct {
	PerMinute float64
	Burst     float64
}

// Limiter holds one bucket per key.
type Limiter struct {
	Rate Rate
	// MaxKeys bounds memory. When it is reached, buckets that have refilled
	// completely are dropped (they carry no state worth keeping); if every
	// bucket is still in debt, a new key is let through rather than evict
	// someone mid-penalty, and Full reports it.
	MaxKeys int
	Now     func() time.Time // nil means time.Now

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New returns a limiter for r holding at most maxKeys buckets.
func New(r Rate, maxKeys int) *Limiter {
	return &Limiter{Rate: r, MaxKeys: maxKeys}
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow spends one request from key's bucket. When it is empty it returns
// false and how long until one request is available.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	b, ok := l.buckets[key]
	if !ok {
		if l.MaxKeys > 0 && len(l.buckets) >= l.MaxKeys {
			l.sweep(now)
			if len(l.buckets) >= l.MaxKeys {
				return true, 0
			}
		}
		b = &bucket{tokens: l.Rate.Burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = l.refill(b, now)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, l.waitFor(b.tokens)
}

// Wait reports how long until key may make a request, without spending
// one: zero if it may now.
func (l *Limiter) Wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return 0
	}
	if t := l.refill(b, l.now()); t < 1 {
		return l.waitFor(t)
	}
	return 0
}

// waitFor is how long a bucket holding tokens takes to reach one.
func (l *Limiter) waitFor(tokens float64) time.Duration {
	perSec := l.Rate.PerMinute / 60
	if perSec <= 0 {
		return time.Hour
	}
	return time.Duration(math.Ceil((1-tokens)/perSec*1e9)) * time.Nanosecond
}

// Full reports whether the limiter is at MaxKeys with every bucket in
// debt, so new keys are going through unlimited.
func (l *Limiter) Full() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.MaxKeys <= 0 || len(l.buckets) < l.MaxKeys {
		return false
	}
	l.sweep(l.now())
	return len(l.buckets) >= l.MaxKeys
}

func (l *Limiter) refill(b *bucket, now time.Time) float64 {
	t := b.tokens + now.Sub(b.at).Seconds()*l.Rate.PerMinute/60
	return math.Min(t, l.Rate.Burst)
}

// sweep drops buckets that have refilled to the brim.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if l.refill(b, now) >= l.Rate.Burst {
			delete(l.buckets, k)
		}
	}
}

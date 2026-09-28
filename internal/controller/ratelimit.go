package controller

import (
	"math"
	"sync"
	"time"
)

// RateLimiter is a per-key token bucket.
//
// It complements the per-account lockout rather than replacing it: lockout
// protects one account, this protects against one password tried against
// every account in a practice, which never trips a per-account counter.
//
// State is in-process, so the limit is per instance. Behind a load balancer
// with several instances, move it to a shared store.
type RateLimiter struct {
	mu        sync.Mutex
	perSecond float64
	burst     float64
	buckets   map[string]*bucket
	now       func() time.Time
	lastSweep time.Time
	maxKeys   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows perMinute requests per key per minute on average,
// with bursts of up to burst.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return &RateLimiter{
		perSecond: float64(perMinute) / 60,
		burst:     float64(burst),
		buckets:   map[string]*bucket{},
		now:       time.Now,
		maxKeys:   100_000,
	}
}

// Allow takes a token for key. When none is left it reports how long until
// one is.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.perSecond)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
	return false, wait
}

// sweep drops buckets that have refilled completely, so memory stays
// proportional to recent traffic rather than to every address ever seen.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute && len(l.buckets) < l.maxKeys {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.burst / l.perSecond * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
	// Under a flood of distinct addresses, shed the oldest state rather
	// than grow without bound. Losing a bucket only resets that key.
	if len(l.buckets) >= l.maxKeys {
		for k := range l.buckets {
			delete(l.buckets, k)
			if len(l.buckets) < l.maxKeys/2 {
				break
			}
		}
	}
}

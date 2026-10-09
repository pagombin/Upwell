package auth

import (
	"sync"
	"time"
)

// RateLimiter is an in-memory token bucket per key (a client IP for sign-in
// attempts). Each key holds up to Burst tokens and regains one token every
// Per/Burst; an attempt spends one token.
type RateLimiter struct {
	Burst int
	Per   time.Duration // time to refill a full bucket

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastPrune time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// NewRateLimiter allows burst attempts per key, refilled over per.
func NewRateLimiter(burst int, per time.Duration) *RateLimiter {
	return &RateLimiter{Burst: burst, Per: per, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow spends one token for key and reports whether one was available.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	rate := float64(l.Burst) / l.Per.Seconds() // tokens per second
	if now.Sub(l.lastPrune) > l.Per {
		// Buckets idle for a full refill period are full again; drop them.
		for k, b := range l.buckets {
			if now.Sub(b.at) >= l.Per {
				delete(l.buckets, k)
			}
		}
		l.lastPrune = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.Burst), at: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > float64(l.Burst) {
		b.tokens = float64(l.Burst)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

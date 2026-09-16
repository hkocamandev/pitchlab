package auth

import (
	"context"
	"sync"
	"time"
)

// Attempt limits, per five-minute window.
//
// Two different numbers, because the two keys mean different things.
//
// AccountAttemptLimit is the one that protects a password: five tries is
// hopeless for online guessing and survives somebody mistyping twice before
// opening their password manager.
//
// AddressAttemptLimit is deliberately much higher, because an address is not
// an identity. An office, a household or a phone network is one address for
// everybody behind it, and five attempts there would lock out the sixth
// colleague to sign in this morning -- an outage caused by the protection
// rather than by an attack. Its job is only to bound credential stuffing
// sprayed across many accounts, which the account limit cannot see.
const (
	AccountAttemptLimit = 5
	AddressAttemptLimit = 50
	LoginAttemptWindow  = 5 * time.Minute
)

// RemoteCounter is the slice of Redis the limiter uses.
//
// An interface so this package does not import the cache, and so the limiter
// is testable without a server.
type RemoteCounter interface {
	// Increment returns the count within the window, and whether the remote
	// store answered at all. The boolean is what lets the caller fall back
	// rather than guess: a zero count from an unreachable store would look
	// like a fresh window and allow everything.
	Increment(ctx context.Context, key string, window time.Duration) (int64, bool)
	// Forget clears a counter after a successful login.
	Forget(ctx context.Context, key string)
}

// Limiter throttles login attempts.
//
// Two layers, because neither alone is acceptable here.
//
// Redis alone would make the cache load-bearing: if it were the only limiter,
// then failing open means an attacker who can disturb Redis also unlocks
// brute force, and failing closed means a cache outage takes login down --
// which contradicts the rule that Redis degrades performance and nothing else.
//
// So Redis is the shared counter when it is there, and an in-process counter
// takes over when it is not. The fallback is weaker: it is per instance, so N
// replicas allow N times the attempts. That is a real gap and it is written
// down rather than hidden -- but N times five attempts per five minutes is
// still not a viable attack, and the alternative was choosing between two
// worse failures.
type Limiter struct {
	remote RemoteCounter

	mu    sync.Mutex
	local map[string]*window
}

type window struct {
	count int
	reset time.Time
}

// NewLimiter builds a limiter. A nil remote counter is valid: the limiter then
// runs entirely in process.
func NewLimiter(remote RemoteCounter) *Limiter {
	return &Limiter{remote: remote, local: make(map[string]*window)}
}

// Allow reports whether an attempt may proceed, and how many remain.
//
// The limit is an argument rather than a constant on the limiter because the
// same limiter serves keys with different meanings; see the constants above.
func (l *Limiter) Allow(ctx context.Context, key string, limit int) (bool, int) {
	if l.remote != nil {
		if count, ok := l.remote.Increment(ctx, key, LoginAttemptWindow); ok {
			return int(count) <= limit, remaining(int(count), limit)
		}
	}
	return l.allowLocal(key, limit)
}

func (l *Limiter) allowLocal(key string, limit int) (bool, int) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.local[key]
	if !ok || now.After(w.reset) {
		w = &window{reset: now.Add(LoginAttemptWindow)}
		l.local[key] = w
	}
	w.count++

	// Keys are unbounded over a long run -- one per attempted username and
	// address -- so expired windows are swept rather than left to accumulate.
	if len(l.local) > 4096 {
		for k, v := range l.local {
			if now.After(v.reset) {
				delete(l.local, k)
			}
		}
	}

	return w.count <= limit, remaining(w.count, limit)
}

// Reset clears a key after a successful login, so a person who eventually
// remembers their password is not still throttled.
func (l *Limiter) Reset(ctx context.Context, key string) {
	if l.remote != nil {
		l.remote.Forget(ctx, key)
	}
	l.mu.Lock()
	delete(l.local, key)
	l.mu.Unlock()
}

func remaining(count, limit int) int {
	if left := limit - count; left > 0 {
		return left
	}
	return 0
}

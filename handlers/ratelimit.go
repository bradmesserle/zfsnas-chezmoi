package handlers

import (
	"sync"
	"time"
)

// loginRateLimiter tracks per-IP failed login attempts with exponential backoff.
//
// Policy:
//   - 5 failures within a 5-minute window → 30-second lockout
//   - Each subsequent lockout doubles the duration (30s → 60s → 120s → … → 15 min cap)
//   - A successful login resets the entry for that IP
var loginLimiter = newRateLimiter()

const (
	rlMaxFailures  = 5
	rlWindow       = 5 * time.Minute
	rlBaseLockout  = 30 * time.Second
	rlMaxLockout   = 15 * time.Minute
	rlCleanupEvery = 10 * time.Minute
)

type ipEntry struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
	lockCount   int // increments on each lockout, drives exponential backoff
}

type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]*ipEntry
}

func newRateLimiter() *rateLimiter {
	rl := &rateLimiter{entries: make(map[string]*ipEntry)}
	go func() {
		t := time.NewTicker(rlCleanupEvery)
		defer t.Stop()
		for range t.C {
			rl.cleanup()
		}
	}()
	return rl
}

// check returns whether ip is currently locked out and how long until it clears.
func (rl *rateLimiter) check(ip string) (locked bool, retryAfter time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e := rl.entries[ip]
	if e == nil {
		return false, 0
	}
	if remaining := time.Until(e.lockedUntil); remaining > 0 {
		return true, remaining
	}
	return false, 0
}

// recordFailure registers a failed attempt for ip.
// If the failure threshold is reached within the window, a lockout is applied.
func (rl *rateLimiter) recordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	e := rl.entries[ip]
	if e == nil {
		e = &ipEntry{windowStart: now}
		rl.entries[ip] = e
	}
	// Reset failure counter when the window has expired.
	if now.Sub(e.windowStart) > rlWindow {
		e.failures = 0
		e.windowStart = now
	}
	e.failures++
	if e.failures >= rlMaxFailures {
		e.lockCount++
		// Exponential backoff: 30s * 2^(lockCount-1), capped at rlMaxLockout.
		shift := e.lockCount - 1
		if shift > 9 {
			shift = 9 // prevents overflow; 2^9 * 30s = 256 min, well above cap
		}
		dur := rlBaseLockout << shift
		if dur > rlMaxLockout {
			dur = rlMaxLockout
		}
		e.lockedUntil = now.Add(dur)
		e.failures = 0
		e.windowStart = now
	}
}

// recordSuccess clears the rate-limit state for ip after a successful login.
func (rl *rateLimiter) recordSuccess(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.entries, ip)
}

// cleanup removes entries that are no longer locked and whose failure window has expired.
func (rl *rateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for ip, e := range rl.entries {
		notLocked := now.After(e.lockedUntil)
		windowExpired := now.Sub(e.windowStart) > rlWindow*2
		if notLocked && windowExpired {
			delete(rl.entries, ip)
		}
	}
}

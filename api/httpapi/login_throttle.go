package httpapi

import (
	"sync"
	"time"
)

// Login throttling, per account.
//
// bcrypt makes each guess slow; it does not make guessing stop. Without a limit
// an attacker can try passwords against one account for as long as they like.
// This caps attempts per email address: loginMaxAttempts within loginWindow,
// then every attempt answers 429 until the window that started with the first
// one runs out.
//
// Three decisions that are not obvious:
//
//   - It counts ATTEMPTS, reserved before the password is checked, not failures
//     recorded after. Recording afterwards lets a burst of parallel requests all
//     pass the check before any of them fails, which is exactly how a guessing
//     script behaves. A successful login clears the count.
//   - The limit applies while throttled even to the CORRECT password. Letting it
//     through would tell the attacker which guess was right, and the limit would
//     only slow them down rather than stop them.
//   - It is keyed by the normalised address whether or not an account exists, so
//     a known and an unknown address are throttled identically. Anything else
//     turns the 429 into a directory of who has an account — the same oracle
//     forgot-password is careful not to be.
//
// The state is in memory and per API process. With several replicas an attacker
// gets the limit once per replica, which still bounds guessing; nginx adds a
// per-IP limit in front (deploy/nginx/nginx.conf) for spraying many accounts
// from one address, which a per-account limit cannot see.
const (
	loginMaxAttempts = 5
	loginWindow      = 15 * time.Minute
	// loginMaxKeys bounds memory against a flood of made-up addresses. At the
	// cap, expired entries go first and then the oldest; evicting a live entry
	// resets that key's count, so the cap is set far above any real user base.
	loginMaxKeys = 50_000
)

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginEntry
	now     func() time.Time
}

type loginEntry struct {
	start    time.Time
	attempts int
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: map[string]*loginEntry{}, now: time.Now}
}

// reserve records an attempt for key. ok is false when the key has used its
// allowance, and retryAfter is then how long until the window resets.
func (l *loginLimiter) reserve(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	e, found := l.entries[key]
	if found && now.Sub(e.start) >= loginWindow {
		found = false
	}
	if !found {
		if _, exists := l.entries[key]; !exists && len(l.entries) >= loginMaxKeys {
			l.evictLocked(now)
		}
		e = &loginEntry{start: now}
		l.entries[key] = e
	}

	if e.attempts >= loginMaxAttempts {
		return false, e.start.Add(loginWindow).Sub(now)
	}
	e.attempts++
	return true, 0
}

// reset forgets a key after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *loginLimiter) evictLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, e := range l.entries {
		if now.Sub(e.start) >= loginWindow {
			delete(l.entries, k)
			continue
		}
		if oldestKey == "" || e.start.Before(oldest) {
			oldestKey, oldest = k, e.start
		}
	}
	if len(l.entries) >= loginMaxKeys && oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}

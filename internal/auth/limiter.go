package auth

import (
	"sync"
	"time"
)

const (
	loginWindow      = 15 * time.Minute
	loginAttempts    = 10
	maxLoginAttempts = 1000
	loginCapacity    = 4096
	maxLoginKeyBytes = 256
)

type loginEntry struct {
	expires  time.Time
	attempts int
}

// LoginLimiter bounds login attempts per key in a fixed fifteen-minute window.
// It holds at most 4096 active keys; new keys are denied at capacity.
// It is local to one server process and should be shared by all login handlers.
// The zero value is ready to use.
type LoginLimiter struct {
	mu      sync.Mutex
	entries map[string]loginEntry
	now     func() time.Time
	limit   int
}

// NewLoginLimiter allows ten attempts per key in each fifteen-minute window.
func NewLoginLimiter() *LoginLimiter {
	return NewLoginLimiterWithLimit(loginAttempts)
}

// NewLoginLimiterWithLimit sets a quota from 1 through 1000 attempts per key.
// An out-of-range value uses the default quota of ten attempts.
func NewLoginLimiterWithLimit(attempts int) *LoginLimiter {
	if attempts < 1 || attempts > maxLoginAttempts {
		attempts = loginAttempts
	}
	return &LoginLimiter{entries: make(map[string]loginEntry), now: time.Now, limit: attempts}
}

// Allow consumes an attempt for key, normally the remote client's IP address.
// Oversized or empty keys are denied so the entry count also bounds memory.
func (l *LoginLimiter) Allow(key string) bool {
	if key == "" || len(key) > maxLoginKeyBytes {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]loginEntry)
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.limit < 1 || l.limit > maxLoginAttempts {
		l.limit = loginAttempts
	}
	now := l.now()
	for savedKey, entry := range l.entries {
		if !now.Before(entry.expires) {
			delete(l.entries, savedKey)
		}
	}
	entry, exists := l.entries[key]
	if !exists {
		if len(l.entries) >= loginCapacity {
			return false
		}
		entry = loginEntry{expires: now.Add(loginWindow)}
	}
	if entry.attempts >= l.limit {
		return false
	}
	entry.attempts++
	l.entries[key] = entry
	return true
}

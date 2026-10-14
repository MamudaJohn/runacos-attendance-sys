package auth

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginLimiterWindow(t *testing.T) {
	now := time.Unix(1_000, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return now }
	for attempt := range loginAttempts {
		if !l.Allow("192.0.2.1") {
			t.Fatalf("attempt %d was denied", attempt+1)
		}
	}
	if l.Allow("192.0.2.1") || !l.Allow("192.0.2.2") {
		t.Fatal("quota must be independent per key")
	}
	now = now.Add(loginWindow - time.Nanosecond)
	if l.Allow("192.0.2.1") {
		t.Fatal("window expired early")
	}
	now = now.Add(time.Nanosecond)
	if !l.Allow("192.0.2.1") || len(l.entries) != 1 || l.entries["192.0.2.1"].attempts != 1 {
		t.Fatal("quota did not reset exactly at expiry or expired keys were retained")
	}
}

func TestLoginLimiterCapacity(t *testing.T) {
	now := time.Unix(1_000, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return now }
	for n := range loginCapacity {
		if !l.Allow(fmt.Sprintf("key-%d", n)) {
			t.Fatalf("capacity exhausted after %d keys", n)
		}
	}
	if l.Allow("one-key-too-many") || len(l.entries) != loginCapacity {
		t.Fatal("new keys must be denied at capacity")
	}
	for range loginAttempts - 1 {
		if !l.Allow("key-0") {
			t.Fatal("capacity must preserve existing keys' remaining quota")
		}
	}
	if l.Allow("key-0") {
		t.Fatal("existing exhausted key was admitted")
	}
	now = now.Add(loginWindow)
	if !l.Allow("one-key-too-many") || len(l.entries) != 1 {
		t.Fatal("expired entries did not free capacity")
	}
}

func TestLoginLimiterConcurrentAttempts(t *testing.T) {
	l := NewLoginLimiter()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if l.Allow("192.0.2.1") {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if got := accepted.Load(); got != loginAttempts {
		t.Fatalf("accepted %d concurrent attempts, want %d", got, loginAttempts)
	}
}

func TestLoginLimiterZeroValueAndKeyBounds(t *testing.T) {
	var l LoginLimiter
	if !l.Allow("192.0.2.1") {
		t.Fatal("zero value is unusable")
	}
	if l.Allow("") || l.Allow(strings.Repeat("x", maxLoginKeyBytes+1)) || len(l.entries) != 1 {
		t.Fatal("invalid keys consumed memory")
	}
}

func TestLoginLimiterConfiguredQuota(t *testing.T) {
	for _, limit := range []int{1, 25, 1000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			l := NewLoginLimiterWithLimit(limit)
			for attempt := range limit {
				if !l.Allow("192.0.2.1") {
					t.Fatalf("attempt %d of %d was denied", attempt+1, limit)
				}
			}
			if l.Allow("192.0.2.1") || !l.Allow("192.0.2.2") {
				t.Fatal("configured quota was not enforced independently per key")
			}
		})
	}
}

func TestLoginLimiterInvalidQuotaUsesDefault(t *testing.T) {
	for _, limit := range []int{-1, 0, maxLoginAttempts + 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			l := NewLoginLimiterWithLimit(limit)
			for range loginAttempts {
				if !l.Allow("192.0.2.1") {
					t.Fatal("default quota was reduced")
				}
			}
			if l.Allow("192.0.2.1") {
				t.Fatal("invalid quota did not use the default")
			}
		})
	}
	var zero LoginLimiter
	for range loginAttempts {
		if !zero.Allow("192.0.2.1") {
			t.Fatal("zero-value quota was reduced")
		}
	}
	if zero.Allow("192.0.2.1") {
		t.Fatal("zero value did not use the default quota")
	}
}

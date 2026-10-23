package api

import (
	"testing"
	"time"
)

// A stale window must not leak map entries, and eviction must keep the map and
// the insertion queue in step: the bug left orphaned addresses behind forever.
func TestAuthLimiterExpiresAndEvicts(t *testing.T) {
	limiter := newAuthLimiter(1000, time.Minute)
	now := time.Now()
	if retry := limiter.Failed("10.0.0.1", now); retry != 0 {
		t.Fatalf("first failure should not trip the limit: %d", retry)
	}
	// After the window the entry restarts instead of accumulating.
	later := now.Add(2 * time.Minute)
	limiter.Failed("10.0.0.1", later)
	if len(limiter.items) != 1 || len(limiter.order) != 1 {
		t.Fatalf("expired entry duplicated: items=%d order=%d", len(limiter.items), len(limiter.order))
	}
	// Overflow evicts and still leaves the map bounded.
	for i := 0; i < limiterMaxAddresses+10; i++ {
		limiter.Failed(fmtAddress(i), later)
	}
	if len(limiter.items) > limiterMaxAddresses {
		t.Fatalf("limiter grew past the cap: %d", len(limiter.items))
	}
	if len(limiter.order) != len(limiter.items) {
		t.Fatalf("order and items diverged: order=%d items=%d", len(limiter.order), len(limiter.items))
	}
}

func fmtAddress(i int) string {
	return "10.1." + string(rune('0'+i/250)) + "." + string(rune('0'+i%250))
}

// The limit itself still applies within one window.
func TestAuthLimiterBlocksWithinWindow(t *testing.T) {
	limiter := newAuthLimiter(2, time.Minute)
	now := time.Now()
	if retry := limiter.Failed("10.0.0.9", now); retry != 0 {
		t.Fatalf("failure 1: %d", retry)
	}
	if retry := limiter.Failed("10.0.0.9", now); retry != 0 {
		t.Fatalf("failure 2 at the limit must pass: %d", retry)
	}
	if retry := limiter.Failed("10.0.0.9", now); retry < 1 {
		t.Fatalf("failure over the limit must carry a retry delay: %d", retry)
	}
}

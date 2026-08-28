package local

import (
	"testing"
)

func TestLoginLimiter_Allow(t *testing.T) {
	l := newLoginLimiter()

	// First 5 calls should be allowed.
	for i := 0; i < 5; i++ {
		if !l.Allow("alice") {
			t.Errorf("call %d: expected allowed, got rate limited", i+1)
		}
	}

	// 6th call should be rate-limited.
	if l.Allow("alice") {
		t.Error("6th call: expected rate limited, got allowed")
	}

	// Different user should still be allowed.
	if !l.Allow("bob") {
		t.Error("different user should not be rate limited")
	}
}

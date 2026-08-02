package apikey

import (
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestCacheConcurrentExpiry exercises concurrent get/set on expiring entries.
// Run with -race: a mutation under the read lock (e.g. delete on expiry) is a
// concurrent map write and fails here.
func TestCacheConcurrentExpiry(t *testing.T) {
	c := newVerifiedCache(time.Millisecond)
	claims := &auth.Claims{Subject: "u"}
	hash := Hash("s3cret")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.set("p", hash, claims)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = c.get("p", "s3cret")
			}
		}()
	}
	wg.Wait()
}

func TestCacheExpiredReturnsNil(t *testing.T) {
	c := newVerifiedCache(time.Millisecond)
	c.set("p", Hash("s3cret"), &auth.Claims{Subject: "u"})
	time.Sleep(5 * time.Millisecond)
	if got := c.get("p", "s3cret"); got != nil {
		t.Errorf("expected nil for expired entry, got %v", got)
	}
}

func TestCacheWrongSecretReturnsNil(t *testing.T) {
	c := newVerifiedCache(time.Minute)
	c.set("p", Hash("s3cret"), &auth.Claims{Subject: "u"})
	if got := c.get("p", "wrong"); got != nil {
		t.Errorf("expected nil for wrong secret, got %v", got)
	}
	if got := c.get("p", "s3cret"); got == nil {
		t.Error("expected hit for correct secret")
	}
}

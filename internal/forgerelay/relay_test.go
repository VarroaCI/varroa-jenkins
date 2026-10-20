package forgerelay

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// TestRequestDeadline_DefaultIsTimeout checks that a context without a
// deadline gets the given timeout.
func TestRequestDeadline_DefaultIsTimeout(t *testing.T) {
	now := time.Now()
	got := requestDeadline(context.Background(), now, forgeapi.PullRequestTimeout)
	if want := now.Add(forgeapi.PullRequestTimeout); !got.Equal(want) {
		t.Fatalf("deadline %v, want %v", got, want)
	}
}

// TestRequestDeadline_EarlierContextWins checks that an earlier caller
// deadline replaces the given timeout.
func TestRequestDeadline_EarlierContextWins(t *testing.T) {
	now := time.Now()
	want := now.Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if got := requestDeadline(ctx, now, forgeapi.PullRequestTimeout); !got.Equal(want) {
		t.Fatalf("deadline %v, want %v", got, want)
	}
}

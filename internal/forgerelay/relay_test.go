package forgerelay

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// TestPullRequestDeadline_DefaultIsRequesterTimeout checks that a context
// without a deadline gets the requester timeout.
func TestPullRequestDeadline_DefaultIsRequesterTimeout(t *testing.T) {
	now := time.Now()
	got := pullRequestDeadline(context.Background(), now)
	if want := now.Add(forgeapi.PullRequestTimeout); !got.Equal(want) {
		t.Fatalf("deadline %v, want %v", got, want)
	}
}

// TestPullRequestDeadline_EarlierContextWins checks that an earlier caller
// deadline replaces the requester timeout.
func TestPullRequestDeadline_EarlierContextWins(t *testing.T) {
	now := time.Now()
	want := now.Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if got := pullRequestDeadline(ctx, now); !got.Equal(want) {
		t.Fatalf("deadline %v, want %v", got, want)
	}
}

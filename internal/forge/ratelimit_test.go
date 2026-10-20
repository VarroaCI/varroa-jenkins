package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRateLimit_Classify(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   bool
		reset  time.Time
	}{
		{
			name:   "403 with exhausted quota and reset",
			status: http.StatusForbidden,
			header: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1800000000"}},
			want:   true,
			reset:  time.Unix(1800000000, 0),
		},
		{
			name:   "403 with Retry-After seconds",
			status: http.StatusForbidden,
			header: http.Header{"Retry-After": {"60"}},
			want:   true,
			reset:  now.Add(60 * time.Second),
		},
		{
			name:   "403 with Retry-After HTTP date",
			status: http.StatusForbidden,
			header: http.Header{"Retry-After": {"Tue, 20 Oct 2026 13:00:00 GMT"}},
			want:   true,
			reset:  time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC),
		},
		{
			name:   "403 with a secondary rate limit body and no headers",
			status: http.StatusForbidden,
			body:   "You have exceeded a secondary rate limit",
			want:   true,
		},
		{
			name:   "429 with no headers",
			status: http.StatusTooManyRequests,
			want:   true,
		},
		{
			name:   "403 with quota left and a permission body",
			status: http.StatusForbidden,
			header: http.Header{"X-Ratelimit-Remaining": {"4999"}},
			body:   `{"message":"Resource not accessible"}`,
		},
		{
			name:   "404 with Retry-After",
			status: http.StatusNotFound,
			header: http.Header{"Retry-After": {"60"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.header
			if h == nil {
				h = http.Header{}
			}
			rl := rateLimit(tc.status, h, []byte(tc.body), now)
			if (rl != nil) != tc.want {
				t.Fatalf("rateLimit = %v, want rate limit %v", rl, tc.want)
			}
			if rl != nil && !rl.Reset.Equal(tc.reset) {
				t.Fatalf("reset = %v, want %v", rl.Reset, tc.reset)
			}
		})
	}
}

func TestGitHub_RateLimit403(t *testing.T) {
	c := githubClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1800000000")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("canary-tok rate limit exceeded"))
		},
	})
	_, err := c.Repository(context.Background(), testRepo)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("got %v, want RateLimitError", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatal("a rate limit matched ErrForbidden")
	}
	if strings.Contains(err.Error(), "canary-tok") {
		t.Fatalf("error carries the body: %q", err.Error())
	}
}

func TestGitea_RateLimit429(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
		},
	})
	_, err := c.Repository(context.Background(), testRepo)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Status != http.StatusTooManyRequests {
		t.Fatalf("got %v, want RateLimitError with status 429", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatal("a rate limit matched ErrForbidden")
	}
}

func TestGitea_Forbidden403StaysForbidden(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r": textReply(http.StatusForbidden, "canary-tok"),
	})
	_, err := c.Repository(context.Background(), testRepo)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
}

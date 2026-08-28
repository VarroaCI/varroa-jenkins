package jenkins

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestRetryOn429(t *testing.T) {
	// Server returns 429 once, then 200.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	ctx := context.Background()
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	resp.Body.Close()

	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
}

// TestCrumbFetchOmitsSessionCookie is a regression test for the anonymous-403
// bug: the mite's long-lived client must NOT carry a JSESSIONID on crumb
// fetches. A cookie jar would auto-attach a stale (post-restart) JSESSIONID,
// tripping the plugin's hasExistingSession check so it skips JWT validation and
// returns anonymous 403. The crumb endpoint here rejects any request carrying a
// Cookie header, mimicking that behavior.
func TestCrumbFetchOmitsSessionCookie(t *testing.T) {
	var crumbSawCookie atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response plants a JSESSIONID — a cookie jar would store and
		// replay it on subsequent requests, including the crumb fetch.
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID.abcd1234", Value: "stalesessionvalue"})
		if r.URL.Path == "/crumbIssuer/api/json" {
			if r.Header.Get("Cookie") != "" {
				crumbSawCookie.Store(true)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"crumb":"abc","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// "Bearer" username + JWT-looking token forces Bearer auth.
	c := NewClient(srv.URL, "Bearer", "eyJhbGciOiJSUzI1NiJ9.payload.signature")
	ctx := context.Background()

	// Prime a session cookie via a GET (would be jarred if a jar existed).
	resp, err := c.do(ctx, http.MethodGet, "/login", nil, "")
	if err != nil {
		t.Fatalf("prime request: %v", err)
	}
	resp.Body.Close()

	// Force a fresh crumb fetch on the next state-changing request.
	c.crumbMu.Lock()
	c.crumb = ""
	c.crumbMu.Unlock()

	resp2, err := c.do(ctx, http.MethodPost, "/some/action", strings.NewReader("x"), "text/plain")
	if err != nil {
		t.Fatalf("post request: %v", err)
	}
	resp2.Body.Close()

	if crumbSawCookie.Load() {
		t.Fatal("crumb fetch carried a session cookie; a stale JSESSIONID would trip hasExistingSession and cause anonymous 403")
	}
}

func TestRetryExhaustedOn503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	ctx := context.Background()
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		t.Fatalf("expected response (not error) after exhausted retries, got: %v", err)
	}
	resp.Body.Close()

	// 1 initial + 3 retries = 4 total.
	if got := attempts.Load(); got != 4 {
		t.Fatalf("expected 4 attempts, got %d", got)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

func TestNoRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	ctx := context.Background()
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected 1 attempt (no retry on 400), got %d", got)
	}
}

func TestRetryOnTransportError(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			// Simulate transport error by hijacking the connection.
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	ctx := context.Background()
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		t.Fatalf("expected success on retry 3, got: %v", err)
	}
	resp.Body.Close()

	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestRateLimiterWaits(t *testing.T) {
	// Token bucket: rate=2/s, burst=2. Three immediate calls should
	// cause the third to wait ~500ms.
	c := NewClient("http://localhost:1", "user", "token")
	c.limiter = rate.NewLimiter(rate.Limit(2), 2)

	// Consume burst immediately — these should be instant.
	ctx := context.Background()
	start := time.Now()
	if err := c.waitRateLimit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.waitRateLimit(ctx); err != nil {
		t.Fatal(err)
	}
	// Third call should wait.
	if err := c.waitRateLimit(ctx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// At rate=2/s, token refill is ~500ms. Allow wide tolerance.
	if elapsed < 200*time.Millisecond {
		t.Fatalf("rate limiter did not delay (elapsed=%v)", elapsed)
	}
}

func TestRateLimiterContextCancel(t *testing.T) {
	c := NewClient("http://localhost:1", "user", "token")
	// Rate=1/hour so any call blocks.
	c.limiter = rate.NewLimiter(rate.Limit(1.0/3600), 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := c.waitRateLimit(ctx)
	if err == nil {
		t.Fatal("expected context.Canceled error")
	}
}

func TestBodyBufferedForRetry(t *testing.T) {
	// Server reads the body on POST requests to verify it's present across retries.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't check body on crumb fetch (GET) or non-state-changing requests.
		if r.Method == http.MethodPost && r.URL.Path != "/crumbIssuer/api/json" {
			n := attempts.Add(1)
			body, _ := io.ReadAll(r.Body)
			if len(body) == 0 {
				t.Errorf("attempt %d: empty body", n)
			}
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		// Crumb endpoint.
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"x","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	ctx := context.Background()
	body := strings.NewReader("<jenkins/>")
	resp, err := c.do(ctx, http.MethodPost, "/quietDown", body, "text/xml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 POST attempts, got %d", got)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	c.retryBaseWait = 50 * time.Millisecond
	c.retryMaxWait = 200 * time.Millisecond

	ctx := context.Background()
	start := time.Now()
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	// Retry-After: 1s overrides our 50ms base backoff.
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("expected Retry-After to cause ~1s delay, got %v", elapsed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"empty", "", 0, 0},
		{"seconds", "120", 120 * time.Second, 120 * time.Second},
		{"zero", "0", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{"Retry-After": []string{tt.header}}}
			d := parseRetryAfter(resp)
			if d < tt.wantMin || d > tt.wantMax {
				t.Fatalf("parseRetryAfter(%q) = %v, want [%v, %v]", tt.header, d, tt.wantMin, tt.wantMax)
			}
		})
	}

	// HTTP-date test.
	t.Run("httpdate", func(t *testing.T) {
		future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
		resp := &http.Response{Header: http.Header{"Retry-After": []string{future}}}
		d := parseRetryAfter(resp)
		if d < 4*time.Second || d > 6*time.Second {
			t.Fatalf("parseRetryAfter(httpdate) = %v, want ~5s", d)
		}
	})
}

func TestRetryableStatusCode(t *testing.T) {
	tests := []struct {
		code      int
		retryable bool
	}{
		{200, false}, {201, false}, {301, false}, {302, false},
		{400, false}, {401, false}, {403, false}, {404, false},
		{409, false}, {422, false},
		{429, true},
		{500, false},
		{502, true}, {503, true}, {504, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.code), func(t *testing.T) {
			if got := retryableStatusCode(tt.code); got != tt.retryable {
				t.Fatalf("retryableStatusCode(%d) = %v, want %v", tt.code, got, tt.retryable)
			}
		})
	}
}

func TestCalcBackoffRange(t *testing.T) {
	c := NewClient("http://localhost:1", "user", "token")
	c.retryBaseWait = 100 * time.Millisecond
	c.retryMaxWait = 500 * time.Millisecond

	// Run many times to ensure jitter stays within ±25%.
	for attempt := 1; attempt <= 3; attempt++ {
		for i := 0; i < 100; i++ {
			b := c.calcBackoff(attempt)
			expectedBase := c.retryBaseWait * time.Duration(1<<(attempt-1))
			if expectedBase > c.retryMaxWait {
				expectedBase = c.retryMaxWait
			}
			if b < expectedBase*3/4 || b > expectedBase*5/4 {
				t.Fatalf("attempt %d: backoff %v outside ±25%% of %v", attempt, b, expectedBase)
			}
		}
	}
}

func TestCrumbNotReFetchedOnRetry(t *testing.T) {
	var crumbFetches atomic.Int32
	var requestAttempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			crumbFetches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"abc123","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		n := requestAttempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Verify crumb header on retry.
		if got := r.Header.Get("Jenkins-Crumb"); got != "abc123" {
			t.Errorf("expected crumb header on retry, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	ctx := context.Background()
	resp, err := c.do(ctx, http.MethodPost, "/someEndpoint", strings.NewReader("data"), "text/plain")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resp.Body.Close()

	if got := crumbFetches.Load(); got != 1 {
		t.Fatalf("expected 1 crumb fetch, got %d", got)
	}
	if got := requestAttempts.Load(); got != 2 {
		t.Fatalf("expected 2 request attempts, got %d", got)
	}
}

func TestDoContextCancellation(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	c.retryBaseWait = 500 * time.Millisecond
	c.retryMaxWait = 1 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// --- Test dynamic token source and 401/403 recovery ---

func TestSessionReusedAcrossCalls(t *testing.T) {
	var crumbReqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			crumbReqCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"test-crumb","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJtest-token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	ctx := context.Background()

	// First POST fetches a crumb.
	resp, err := c.do(ctx, http.MethodPost, "/some-api", strings.NewReader("body"), "text/plain")
	if err != nil {
		t.Fatalf("first POST: %v", err)
	}
	resp.Body.Close()

	// Second POST should reuse the cached crumb (no additional fetch).
	resp, err = c.do(ctx, http.MethodPost, "/some-api", strings.NewReader("body2"), "text/plain")
	if err != nil {
		t.Fatalf("second POST: %v", err)
	}
	resp.Body.Close()

	if got := crumbReqCount.Load(); got != 1 {
		t.Errorf("expected 1 crumb fetch, got %d", got)
	}
}

func Test401TriggersSessionReestablish(t *testing.T) {
	var attemptCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"re-crumb","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		n := attemptCount.Add(1)
		// First main request fails with 401, second succeeds.
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJvalid-token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	ctx := context.Background()
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		t.Fatalf("expected success after session recovery: %v", err)
	}
	resp.Body.Close()

	// Should have made 2 main attempts (first 401, retry 200).
	if got := attemptCount.Load(); got != 2 {
		t.Errorf("expected 2 main attempts, got %d", got)
	}
}

func Test401WithTokenRefreshCallback(t *testing.T) {
	var refreshedToken atomic.Bool
	var tokenValue atomic.Value

	tokenValue.Store("eyJold-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		// First attempt fails with 401 using old token.
		auth := r.Header.Get("Authorization")
		if auth == "Bearer eyJold-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJold-token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond
	c.SetTokenFunc(func() string { return tokenValue.Load().(string) })
	c.OnTokenExpired = func() {
		refreshedToken.Store(true)
		tokenValue.Store("eyJfresh-token")
	}

	ctx := context.Background()
	resp, err := c.do(ctx, http.MethodGet, "/api/json", nil, "")
	if err != nil {
		t.Fatalf("expected success after token refresh: %v", err)
	}
	resp.Body.Close()

	if !refreshedToken.Load() {
		t.Error("expected OnTokenExpired callback to be called")
	}
}

// Test401RecoveryRefetchesCrumbForPOST verifies that after a 401 clears the
// cached crumb, a retried state-changing request re-fetches and carries a
// crumb (otherwise the retry would fail CSRF validation again).
func Test401RecoveryRefetchesCrumbForPOST(t *testing.T) {
	var postAttempts atomic.Int32
	var secondPostHadCrumb atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"the-crumb","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		n := postAttempts.Add(1)
		if n == 1 {
			// First POST: simulate a lost session.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Retry after recovery must carry the re-fetched crumb.
		if r.Header.Get("Jenkins-Crumb") == "the-crumb" {
			secondPostHadCrumb.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJvalid-token")
	c.retryBaseWait = 1 * time.Millisecond
	c.retryMaxWait = 10 * time.Millisecond

	resp, err := c.do(context.Background(), http.MethodPost, "/createItem", nil, "application/xml")
	if err != nil {
		t.Fatalf("expected success after session recovery: %v", err)
	}
	resp.Body.Close()

	if postAttempts.Load() != 2 {
		t.Errorf("expected 2 POST attempts, got %d", postAttempts.Load())
	}
	if !secondPostHadCrumb.Load() {
		t.Error("retried POST after 401 recovery did not carry a re-fetched crumb")
	}
}

// TestSessionID verifies the X-Jenkins-Session response header (regenerated
// on every Jenkins JVM boot) is surfaced, and that its absence is an error.
func TestSessionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.Header().Set("X-Jenkins-Session", "91ee47ae")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	got, err := c.SessionID(context.Background())
	if err != nil {
		t.Fatalf("SessionID: %v", err)
	}
	if got != "91ee47ae" {
		t.Errorf("SessionID = %q, want 91ee47ae", got)
	}
}

func TestSessionIDMissingHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	if _, err := c.SessionID(context.Background()); err == nil {
		t.Error("expected error when X-Jenkins-Session header is absent")
	}
}

func TestCheckConfig_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/check":
			if r.Method != http.MethodPost {
				t.Errorf("unexpected method: %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "user", "token")
	if err := c.CheckConfig(context.Background(), "jenkins:\n  systemMessage: hello\n"); err != nil {
		t.Errorf("expected nil for 200 OK, got: %v", err)
	}
}

func TestCheckConfig_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/crumbIssuer"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`)
		case r.URL.Path == "/configuration-as-code/check":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid YAML"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "user", "token")
	err := c.CheckConfig(context.Background(), "bad yaml")
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid YAML") {
		t.Errorf("expected error containing status and body, got: %v", err)
	}
}

func TestCheckConfig_LooksLikeCheckFailureStub(t *testing.T) {
	if looksLikeCheckFailure([]byte("some error")) {
		t.Error("stub should return false")
	}
	if looksLikeCheckFailure(nil) {
		t.Error("stub should return false on nil")
	}
}

func TestQuietDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"crumb":"abc","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/quietDown" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Jenkins-Crumb") == "" {
			t.Error("expected crumb header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	if err := c.QuietDown(context.Background()); err != nil {
		t.Fatalf("QuietDown: %v", err)
	}
}

func TestCancelQuietDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"crumb":"abc","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/cancelQuietDown" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Jenkins-Crumb") == "" {
			t.Error("expected crumb header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	if err := c.CancelQuietDown(context.Background()); err != nil {
		t.Fatalf("CancelQuietDown: %v", err)
	}
}

func TestQuietDownNon2xx(t *testing.T) {
	var crumbCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			crumbCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"crumb":"abc","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		if !crumbCalled {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	err := c.QuietDown(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error, got: %v", err)
	}
}

func TestExportConfig(t *testing.T) {
	const exportBody = "jenkins:\n  systemMessage: hello\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"crumb":"abc","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/configuration-as-code/export" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Jenkins-Crumb") == "" {
			t.Error("expected crumb header")
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(exportBody))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	body, err := c.ExportConfig(context.Background())
	if err != nil {
		t.Fatalf("ExportConfig: %v", err)
	}
	if body != exportBody {
		t.Errorf("expected %q, got %q", exportBody, body)
	}
}

func TestExportConfigNon2xx(t *testing.T) {
	var crumbCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			crumbCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"crumb":"abc","crumbRequestField":"Jenkins-Crumb"}`)
			return
		}
		if !crumbCalled {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "user", "token")
	_, err := c.ExportConfig(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error, got: %v", err)
	}
}

func TestScriptConsoleOnce_NoRetryOnTransportError(t *testing.T) {
	var scriptTextAttempts atomic.Int32
	var crumbAttempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			crumbAttempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"x","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		if r.URL.Path == "/scriptText" {
			scriptTextAttempts.Add(1)
			// Force a transport error by hijacking and closing the connection.
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJhbGciOiJSUzI1NiJ9.payload.signature")
	ctx := context.Background()
	_, err := c.ScriptConsoleOnce(ctx, "println 'hello'")
	if err == nil {
		t.Fatal("expected error on transport failure")
	}

	if got := crumbAttempts.Load(); got != 1 {
		t.Errorf("expected 1 crumb fetch, got %d", got)
	}
	if got := scriptTextAttempts.Load(); got != 1 {
		t.Errorf("expected exactly 1 /scriptText attempt (no retry), got %d", got)
	}
}

func TestScriptConsoleOnce_NoRetryOn5xx(t *testing.T) {
	var scriptTextAttempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"x","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		if r.URL.Path == "/scriptText" {
			scriptTextAttempts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJhbGciOiJSUzI1NiJ9.payload.signature")
	ctx := context.Background()
	_, err := c.ScriptConsoleOnce(ctx, "println 'hello'")
	if err == nil {
		t.Fatal("expected error on 503")
	}

	if got := scriptTextAttempts.Load(); got != 1 {
		t.Errorf("expected exactly 1 /scriptText attempt (no retry), got %d", got)
	}
}

func TestScriptConsoleOnce_NonTwoXXReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"x","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error: something broke"))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJhbGciOiJSUzI1NiJ9.payload.signature")
	ctx := context.Background()
	_, err := c.ScriptConsoleOnce(ctx, "println 'hello'")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error message to include status code 500, got: %v", err)
	}
	if !strings.Contains(err.Error(), "something broke") {
		t.Errorf("expected error message to include body detail, got: %v", err)
	}
}

func TestScriptConsoleOnce_RespectsContextDeadlineBeyond30s(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crumbIssuer/api/json" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"crumb":"x","crumbRequestField":"Jenkins-Crumb"}`))
			return
		}
		// Sleep 150ms — exceeds the shortened 50ms httpClient timeout but
		// ScriptConsoleOnce uses a request-scoped client with Timeout:0 so the
		// 2s context deadline should be the only bound.
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "Bearer", "eyJhbGciOiJSUzI1NiJ9.payload.signature")
	// Overwrite the httpClient with a deliberately short timeout to prove the
	// mutating POST runs on a separate request-scoped client, not this one.
	c.httpClient = &http.Client{Timeout: 50 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := c.ScriptConsoleOnce(ctx, "println 'hello'")
	if err != nil {
		t.Fatalf("expected success with 2s context deadline, got: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected body 'ok', got %q", result)
	}
}

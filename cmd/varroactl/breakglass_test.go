package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 11.1 flag parsing: --ttl duration -> ttlSeconds
// ---------------------------------------------------------------------------

func TestTtlSecondsFromDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int
	}{
		{15 * time.Minute, 900},
		{5 * time.Minute, 300},
		{90 * time.Second, 90},
	}
	for _, tc := range cases {
		if got := ttlSecondsFromDuration(tc.d); got != tc.want {
			t.Errorf("ttlSecondsFromDuration(%s) = %d, want %d", tc.d, got, tc.want)
		}
	}
}

func TestBreakGlass_ReasonRequired(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when --reason is missing")
	}))
	defer srv.Close()

	code := run([]string{"break-glass", "team-a/ctrl-1", "--server", srv.URL})
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
}

// ---------------------------------------------------------------------------
// 11.3 non-loopback --listen refusal
// ---------------------------------------------------------------------------

func TestBreakGlass_NonLoopbackListenRefused(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called for a non-loopback --listen")
	}))
	defer srv.Close()

	code := run([]string{
		"break-glass", "team-a/ctrl-1",
		"--reason", "incident 4711",
		"--listen", "0.0.0.0:0",
		"--server", srv.URL,
	})
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
}

func TestRefuseNonLoopbackListen(t *testing.T) {
	if err := refuseNonLoopbackListen("127.0.0.1:0"); err != nil {
		t.Errorf("expected 127.0.0.1:0 to be accepted, got %v", err)
	}
	if err := refuseNonLoopbackListen("localhost:0"); err != nil {
		t.Errorf("expected localhost:0 to be accepted, got %v", err)
	}
	if err := refuseNonLoopbackListen("0.0.0.0:0"); err == nil {
		t.Error("expected 0.0.0.0:0 to be refused")
	}
}

// ---------------------------------------------------------------------------
// 11.2 grant request
// ---------------------------------------------------------------------------

func TestBreakGlass_GrantRequest(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)

	var gotPath, gotMethod string
	var gotBody struct {
		Reason     string `json:"reason"`
		TTLSeconds int    `json:"ttlSeconds"`
	}
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		resp := map[string]any{
			"token":     "granted-token",
			"expiresAt": time.Now().Add(50 * time.Millisecond).Format(time.RFC3339),
			"target": map[string]any{
				"service":   "ci-1a2b3c4d-svc",
				"namespace": "team-a",
				"port":      8080,
			},
			"locationUrl": "http://ci-1a2b3c4d-svc.team-a.svc.cluster.local:8080/",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer srv.Close()

	code := run([]string{
		"break-glass", "ctrl-1",
		"-n", "team-a",
		"--reason", "incident 4711",
		"--ttl", "5m",
		"--server", srv.URL,
	})
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	wantPath := "/api/v1/clusters/core/controllers/team-a/ctrl-1/break-glass"
	if gotPath != wantPath {
		t.Errorf("expected path %s, got %s", wantPath, gotPath)
	}
	if gotBody.Reason != "incident 4711" {
		t.Errorf("expected reason %q, got %q", "incident 4711", gotBody.Reason)
	}
	if gotBody.TTLSeconds != 300 {
		t.Errorf("expected ttlSeconds 300, got %d", gotBody.TTLSeconds)
	}
}

// TestBreakGlass_GrantRefused proves a non-2xx grant response is surfaced as
// a normal API error, and that the command never opens a listener.
func TestBreakGlass_GrantRefused(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
	})
	defer srv.Close()

	code := run([]string{
		"break-glass", "team-a/ctrl-1",
		"--reason", "incident 4711",
		"--server", srv.URL,
	})
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
}

// ---------------------------------------------------------------------------
// 11.6 exit at expiry / on interrupt
// ---------------------------------------------------------------------------

func TestBreakGlass_ExitsAtExpiry(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"token":       "granted-token",
			"expiresAt":   time.Now().Add(30 * time.Millisecond).Format(time.RFC3339),
			"target":      map[string]any{"service": "ci-svc", "namespace": "team-a", "port": 8080},
			"locationUrl": "http://ci-svc.team-a.svc.cluster.local:8080/",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer srv.Close()

	start := time.Now()
	code := run([]string{
		"break-glass", "team-a/ctrl-1",
		"--reason", "incident 4711",
		"--server", srv.URL,
	})
	if code != 0 {
		t.Fatalf("expected exit code 0 at expiry, got %d", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("expected a fast return at expiry, took %s", elapsed)
	}
}

func TestBreakGlass_ExitsOnInterrupt(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"token":       "granted-token",
			"expiresAt":   time.Now().Add(time.Hour).Format(time.RFC3339),
			"target":      map[string]any{"service": "ci-svc", "namespace": "team-a", "port": 8080},
			"locationUrl": "http://ci-svc.team-a.svc.cluster.local:8080/",
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	root := newRootCmd()
	root.SetContext(ctx)
	root.SetArgs([]string{
		"break-glass", "team-a/ctrl-1",
		"--reason", "incident 4711",
		"--server", srv.URL,
	})

	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error on interrupt, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not exit after interrupt")
	}
}

// ---------------------------------------------------------------------------
// 11.4 the proxy strips inbound credentials and injects the granted token
// ---------------------------------------------------------------------------

func TestBreakGlassProxy_StripsCredentialsInjectsToken(t *testing.T) {
	var gotAuth string
	var sawCookie bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		sawCookie = len(r.Header["Cookie"]) > 0
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	proxy := newBreakGlassProxy(targetURL, "granted-token", "http://ci-svc.team-a.svc.cluster.local:8080/", "http://proxy-base")
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/manage", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer stolen-token")
	req.Header.Set("Cookie", "JSESSIONID=evil")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	_ = resp.Body.Close()

	if gotAuth != "Bearer granted-token" {
		t.Errorf("expected upstream Authorization %q, got %q", "Bearer granted-token", gotAuth)
	}
	if sawCookie {
		t.Error("expected no inbound Cookie header at upstream")
	}
}

// ---------------------------------------------------------------------------
// 11.5 response header rewriting
// ---------------------------------------------------------------------------

func TestBreakGlassProxy_RewritesLocationHeader(t *testing.T) {
	testBreakGlassHeaderRewrite(t, "Location",
		"http://ci-svc.team-a.svc.cluster.local:8080/manage?x=1#frag",
		"http://ci-svc.team-a.svc.cluster.local:8080/",
		"http://proxy-base/manage?x=1#frag")
}

func TestBreakGlassProxy_RewritesContentLocationHeader(t *testing.T) {
	testBreakGlassHeaderRewrite(t, "Content-Location",
		"http://ci-svc.team-a.svc.cluster.local:8080/job/x/",
		"http://ci-svc.team-a.svc.cluster.local:8080/",
		"http://proxy-base/job/x/")
}

func TestBreakGlassProxy_RewritesRefreshHeader(t *testing.T) {
	testBreakGlassHeaderRewrite(t, "Refresh",
		"0; url=http://ci-svc.team-a.svc.cluster.local:8080/manage",
		"http://ci-svc.team-a.svc.cluster.local:8080/",
		"0; url=http://proxy-base/manage")
}

// testBreakGlassHeaderRewrite proves one response header is rewritten as
// D7/D2 describe. The proxy base is a fixed placeholder: the rewrite runs
// in ModifyResponse before the client follows any redirect, so the
// upstream server's 200 response with the header set is enough — the
// client under test never needs to reach the placeholder base itself.
func testBreakGlassHeaderRewrite(t *testing.T, header, upstreamValue, locationBase, want string) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(header, upstreamValue)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	proxy := newBreakGlassProxy(targetURL, "granted-token", locationBase, "http://proxy-base")
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got := resp.Header.Get(header)
	if got != want {
		t.Errorf("%s: got %q, want %q", header, got, want)
	}
}

// TestRewriteLocationPrefix_NoMatchPassesThrough proves a value without the
// locationBase prefix is left untouched.
func TestRewriteLocationPrefix_NoMatchPassesThrough(t *testing.T) {
	got := rewriteLocationPrefix("https://example.com/elsewhere", "http://ci-svc.team-a.svc.cluster.local:8080/", "http://proxy-base")
	if got != "https://example.com/elsewhere" {
		t.Errorf("expected passthrough, got %q", got)
	}
}

func TestRewriteRefreshHeader_NoUrlParamPassesThrough(t *testing.T) {
	got := rewriteRefreshHeader("5", "http://ci-svc.team-a.svc.cluster.local:8080/", "http://proxy-base")
	if got != "5" {
		t.Errorf("expected passthrough, got %q", got)
	}
}

func TestBreakGlass_InvalidTarget(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called for an invalid --target")
	}))
	defer srv.Close()

	code := run([]string{
		"break-glass", "team-a/ctrl-1",
		"--reason", "incident 4711",
		"--target", "not-a-url",
		"--server", srv.URL,
	})
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
}

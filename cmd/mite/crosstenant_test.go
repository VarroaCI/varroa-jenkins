package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// --- 6.1: routes, unknown kind, wrong token ---

func TestCrossTenantUnknownKindRejected400(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	var sent int32
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/build", strings.NewReader(`{"kind":"delete"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.crossTenantHandler("build")(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown kind, got %d: %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&sent) != 0 {
		t.Errorf("expected no send for a rejected kind, got %d", sent)
	}
}

// TestCrossTenantKindOutsideOwnRouteRejected400 proves a kind that belongs
// to a different cross-tenant route is refused on this one: each route
// accepts only the kinds design.md D2's table names for it.
func TestCrossTenantKindOutsideOwnRouteRejected400(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/build", strings.NewReader(`{"kind":"lock_acquire"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.crossTenantHandler("build")(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a kind from another route, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCrossTenantWrongTokenRejected401(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/status", strings.NewReader(`{"kind":"status"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	agent.crossTenantHandler("status")(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a wrong token, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v (%s)", err, rec.Body.String())
	}
	if payload["code"] != unavailableCode {
		t.Errorf("expected the %s code, got %+v", unavailableCode, payload)
	}
}

// TestCrossTenantRoutesRegistered proves the four routes are wired on the
// one loopback listener, alongside the existing three.
func TestCrossTenantRoutesRegistered(t *testing.T) {
	agent, port := startTestLoopback(t)
	for _, route := range []string{"build", "status", "artifact", "lock"} {
		url := fmt.Sprintf("http://127.0.0.1:%d/v1/cross-tenant/%s", port, route)
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"kind":"not-a-kind"}`))
		req.Header.Set("Authorization", "Bearer "+agent.loopbackToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request to %s failed: %v", url, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("route %s: expected 400 for an unrecognized kind, got %d", route, resp.StatusCode)
		}
	}
}

// --- 6.2: correlation through the waiter map ---

func crossTenantBody(kind string) string {
	return fmt.Sprintf(`{"kind":%q,"caller_run_id":"r"}`, kind)
}

// TestCrossTenantTwoConcurrentKindsDoNotCross is 6.2's acceptance test: a
// status poll and a lock renew wait on one mite; resolving one reply must
// release only its own caller.
func TestCrossTenantTwoConcurrentKindsDoNotCross(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.crossTenantTimeout = 2 * time.Second

	captured := make(chan struct {
		id   uint64
		kind string
	}, 2)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		req := m.GetCrossTenantRequest()
		captured <- struct {
			id   uint64
			kind string
		}{*req.RequestId, req.Kind}
		return nil
	}})

	results := make(chan *httptest.ResponseRecorder, 2)
	post := func(route, kind string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/"+route, strings.NewReader(crossTenantBody(kind)))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		agent.crossTenantHandler(route)(rec, req)
		results <- rec
	}
	go post("status", "status")
	go post("lock", "lock_renew")

	first := <-captured
	second := <-captured
	if first.id == second.id {
		t.Fatalf("expected distinct request ids, got the same value twice: %d", first.id)
	}

	// Resolve the lock_renew reply first; only its caller may be released.
	var lockID, statusID uint64
	if first.kind == "lock_renew" {
		lockID, statusID = first.id, second.id
	} else {
		lockID, statusID = second.id, first.id
	}
	agent.crossTenantWaiters.resolve(&mitev1.CrossTenantResponse{
		RequestId: &lockID, Kind: "lock_renew", PayloadJson: `{"state":"held"}`,
	})

	// The body is the flat payload, so the released reply is told apart
	// by the state its payload carries: the lock_renew reply says held.
	var releasedFirst map[string]string
	select {
	case rec := <-results:
		if err := json.Unmarshal(rec.Body.Bytes(), &releasedFirst); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the released cross-tenant reply")
	}
	if releasedFirst["state"] != "held" {
		t.Errorf("expected the lock_renew reply (state held) released first, got %+v", releasedFirst)
	}

	// Release the status waiter so its goroutine does not leak past the test.
	agent.crossTenantWaiters.resolve(&mitev1.CrossTenantResponse{
		RequestId: &statusID, Kind: "status", PayloadJson: `{"state":"running"}`,
	})
	select {
	case <-results:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the status reply to finish")
	}
}

// TestCrossTenantLockForwardsTargetTenantVerbatim asserts the lock route
// forwards a lock body as sent. The recorder resolves the lock owner from
// target_tenant and the bare name, so the mite must neither drop nor join
// the two fields.
func TestCrossTenantLockForwardsTargetTenantVerbatim(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.crossTenantTimeout = 2 * time.Second

	sent := make(chan *mitev1.CrossTenantRequest, 1)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sent <- m.GetCrossTenantRequest()
		return nil
	}})

	body := `{"kind":"lock_acquire","caller_job_path":"deploy","name":"deploy-prod",` +
		`"target_tenant":"team-pay/ci","run_id":"r","lease_seconds":300}`
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/lock", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		agent.crossTenantHandler("lock")(rec, req)
		done <- rec
	}()

	var got *mitev1.CrossTenantRequest
	select {
	case got = <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the forwarded lock request")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.PayloadJson), &payload); err != nil {
		t.Fatalf("decode the forwarded payload: %v", err)
	}
	if payload["name"] != "deploy-prod" || payload["target_tenant"] != "team-pay/ci" {
		t.Fatalf("forwarded name %v and target_tenant %v, want deploy-prod and team-pay/ci",
			payload["name"], payload["target_tenant"])
	}

	id := *got.RequestId
	agent.crossTenantWaiters.resolve(&mitev1.CrossTenantResponse{
		RequestId: &id, Kind: "lock_acquire", PayloadJson: `{"state":"held","tenant":"team-pay/ci"}`,
	})
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the lock reply")
	}
}

func TestCrossTenant504Timeout(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.crossTenantTimeout = 30 * time.Millisecond
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/artifact", strings.NewReader(crossTenantBody("artifact_list")))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.crossTenantHandler("artifact")(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != unavailableCode {
		t.Errorf("expected the %s code, got %+v", unavailableCode, payload)
	}
}

func TestCrossTenant429Cap(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	for i := uint64(0); i < maxCrossTenantWaiters; i++ {
		if _, ok := agent.crossTenantWaiters.register(i + 1000); !ok {
			t.Fatalf("failed to fill the waiter map at entry %d", i)
		}
	}
	var sent int32
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error {
		atomic.AddInt32(&sent, 1)
		return nil
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/lock", strings.NewReader(crossTenantBody("lock_acquire")))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.crossTenantHandler("lock")(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 above the waiter cap, got %d", rec.Code)
	}
	if atomic.LoadInt32(&sent) != 0 {
		t.Errorf("expected no send above the cap, got %d", sent)
	}
}

func TestCrossTenant503StreamDown(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	// The agent holds no session: the command stream never connected.

	req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/build", strings.NewReader(crossTenantBody("build")))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.crossTenantHandler("build")(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the stream is down, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestProcessCommandsResolvesCrossTenantResponse proves the command-stream
// dispatch in agent.go delivers an OperatorMessage.cross_tenant_response to
// the matching waiter.
func TestProcessCommandsResolvesCrossTenantResponse(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()

	ch, ok := agent.crossTenantWaiters.register(42)
	if !ok {
		t.Fatal("register failed")
	}

	reqID := uint64(42)
	msgs := []*mitev1.OperatorMessage{
		{Message: &mitev1.OperatorMessage_CrossTenantResponse{CrossTenantResponse: &mitev1.CrossTenantResponse{
			RequestId: &reqID, Kind: "build", PayloadJson: `{"run_id":"r1"}`,
		}}},
	}
	idx := 0
	setTestStream(agent, &concurrentSendStream{
		sendFn: func(*mitev1.MiteMessage) error { return nil },
		recvFn: func() (*mitev1.OperatorMessage, error) {
			if idx < len(msgs) {
				m := msgs[idx]
				idx++
				return m, nil
			}
			return nil, io.EOF
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		agent.processCommands(ctx)
		close(done)
	}()

	select {
	case resp := <-ch:
		if resp.PayloadJson != `{"run_id":"r1"}` {
			t.Errorf("expected the forwarded payload, got %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the cross-tenant response")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processCommands did not return after the stream ended")
	}
}

// TestProcessCommandsTeardownReleasesCrossTenantWaiters mirrors the claim
// route's teardown test: no goroutine remains blocked on a cross-tenant
// call after a stream teardown with waiters open.
func TestProcessCommandsTeardownReleasesCrossTenantWaiters(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()

	recvBlock := make(chan struct{})
	setTestStream(agent, &concurrentSendStream{
		sendFn: func(*mitev1.MiteMessage) error { return nil },
		recvFn: func() (*mitev1.OperatorMessage, error) {
			<-recvBlock
			return nil, io.EOF
		},
	})

	ch1, ok1 := agent.crossTenantWaiters.register(1)
	ch2, ok2 := agent.crossTenantWaiters.register(2)
	if !ok1 || !ok2 {
		t.Fatal("failed to register the two waiters")
	}

	released := make(chan uint64, 2)
	waitFor := func(id uint64, ch chan *mitev1.CrossTenantResponse) {
		<-ch // blocks until releaseAll delivers a value
		released <- id
	}
	go waitFor(1, ch1)
	go waitFor(2, ch2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		agent.processCommands(ctx)
		close(done)
	}()

	close(recvBlock) // let Recv return io.EOF: stream teardown

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processCommands did not return after stream teardown")
	}

	got := map[uint64]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-released:
			got[id] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for waiter release (%d of 2 released)", i)
		}
	}
	if !got[1] || !got[2] {
		t.Errorf("expected both waiters released on teardown, got %v", got)
	}
}

// --- 5.4 (mite's share): VARROA_CROSSTENANT_MITE_TIMEOUT_MS ---

func TestCrossTenantMiteTimeoutAbsentKeepsDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	got := crossTenantMiteTimeoutFromEnv(logger)
	want := time.Duration(defaultCrossTenantMiteTimeoutMS) * time.Millisecond
	if got != want {
		t.Errorf("expected the default %v, got %v", want, got)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output for an absent variable, got %q", buf.String())
	}
}

func TestCrossTenantMiteTimeoutOverride(t *testing.T) {
	t.Setenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS", "8000")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	got := crossTenantMiteTimeoutFromEnv(logger)
	if want := 8 * time.Second; got != want {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestCrossTenantMiteTimeoutMalformedKeepsDefaultAndWarns(t *testing.T) {
	t.Setenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS", "9.5s")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	got := crossTenantMiteTimeoutFromEnv(logger)
	want := time.Duration(defaultCrossTenantMiteTimeoutMS) * time.Millisecond
	if got != want {
		t.Errorf("expected the default %v kept for a malformed value, got %v", want, got)
	}
	if strings.Count(buf.String(), "level=WARN") != 1 {
		t.Errorf("expected exactly one WARNING for a malformed value, got: %q", buf.String())
	}
}

func TestCrossTenantMiteTimeoutClampedAboveWaitCap(t *testing.T) {
	t.Setenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS", "20000")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	got := crossTenantMiteTimeoutFromEnv(logger)
	if got != loopbackWaitCap {
		t.Errorf("expected the clamped %v, got %v", loopbackWaitCap, got)
	}
	if strings.Count(buf.String(), "level=WARN") != 1 || !strings.Contains(buf.String(), "clamping") {
		t.Errorf("expected exactly one clamp WARNING, got: %q", buf.String())
	}
}

// A value at the listener's write timeout is above the wait cap, so it is
// clamped. The clamp is the only warning: no plugin-budget comparison
// remains, because the cap is below the plugin read default.
func TestCrossTenantMiteTimeoutAtWriteTimeoutIsClamped(t *testing.T) {
	t.Setenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS", "10000")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	got := crossTenantMiteTimeoutFromEnv(logger)
	if got != loopbackWaitCap {
		t.Errorf("expected the clamped %v, got %v", loopbackWaitCap, got)
	}
	logged := buf.String()
	if strings.Count(logged, "level=WARN") != 1 {
		t.Errorf("expected exactly one clamp WARNING, got: %q", logged)
	}
	if strings.Contains(logged, "plugin read budget") {
		t.Errorf("expected no plugin-budget warning, got: %q", logged)
	}
}

func TestCrossTenantMiteTimeoutAtWaitCapKept(t *testing.T) {
	t.Setenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS", "9000")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	if got := crossTenantMiteTimeoutFromEnv(logger); got != 9*time.Second {
		t.Errorf("expected 9s, got %v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output at the wait cap, got %q", buf.String())
	}
}

func TestCrossTenantMiteTimeoutBelowWaitCapLogsNothing(t *testing.T) {
	t.Setenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS", "8999")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	_ = crossTenantMiteTimeoutFromEnv(logger)
	if buf.Len() != 0 {
		t.Errorf("expected no warning below the wait cap, got %q", buf.String())
	}
}

// TestCrossTenantMiteTimeout_StaysUnderWriteTimeout proves no accepted
// value lets the wait reach the listener's write deadline.
func TestCrossTenantMiteTimeout_StaysUnderWriteTimeout(t *testing.T) {
	if loopbackWaitCap >= loopbackWriteTimeout {
		t.Fatalf("wait cap %v must stay below the write timeout %v", loopbackWaitCap, loopbackWriteTimeout)
	}
	for ms := 1000; ms <= 30000; ms += 250 {
		t.Setenv("VARROA_CROSSTENANT_MITE_TIMEOUT_MS", strconv.Itoa(ms))
		got := crossTenantMiteTimeoutFromEnv(slog.New(slog.NewTextHandler(io.Discard, nil)))
		if got <= 0 || got > loopbackWaitCap {
			t.Errorf("value %dms resolved to %v, want (0, %v]", ms, got, loopbackWaitCap)
		}
	}
}

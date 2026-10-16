package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestLoopbackWaitLeft(t *testing.T) {
	entry := time.Unix(0, 0)
	cases := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
		expect  time.Duration
	}{
		{"no time spent, under cap", 0, 4500 * time.Millisecond, 4500 * time.Millisecond},
		{"no time spent, over cap", 0, 20 * time.Second, loopbackWaitCap},
		{"time spent, under cap", 2 * time.Second, 4500 * time.Millisecond, 2500 * time.Millisecond},
		{"time spent, over cap", 2 * time.Second, 20 * time.Second, 7 * time.Second},
		{"budget used exactly", 4500 * time.Millisecond, 4500 * time.Millisecond, 0},
		{"budget overrun", 6 * time.Second, 4500 * time.Millisecond, 0},
		{"cap used up", 9500 * time.Millisecond, 9 * time.Second, 0},
		{"zero want", 0, 0, 0},
		{"negative want", 0, -time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := loopbackWaitLeft(entry, entry.Add(tc.elapsed), tc.want)
			if got != tc.expect {
				t.Fatalf("loopbackWaitLeft(elapsed=%v, want=%v) = %v, want %v", tc.elapsed, tc.want, got, tc.expect)
			}
		})
	}
	for _, e := range []time.Duration{0, time.Second, 5 * time.Second, loopbackWaitCap} {
		if total := e + loopbackWaitLeft(entry, entry.Add(e), 20*time.Second); total > loopbackWaitCap {
			t.Errorf("elapsed %v plus the wait = %v, past the cap %v", e, total, loopbackWaitCap)
		}
	}
}

// TestClaimSlowSendConsumesWaitBudget covers a send that takes longer than
// the claim budget. The budget counts from handler entry, so the handler
// answers 504 at about the end of the send, and the late reply is dropped.
func TestClaimSlowSendConsumesWaitBudget(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.claimTimeout = 300 * time.Millisecond

	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		id := m.Message.(*mitev1.MiteMessage_ClaimRequest).ClaimRequest.RequestId
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(650 * time.Millisecond)
			agent.claimWaiters.resolve(&mitev1.ClaimResponse{RequestId: id, Decision: "granted"})
		}()
		time.Sleep(500 * time.Millisecond)
		return nil
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/claim", strings.NewReader(claimRequestBody()))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.handleClaim(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", rec.Code, rec.Body.String())
	}
	assertUnavailableWireShape(t, rec.Body.Bytes())
	agent.claimWaiters.mu.Lock()
	n := len(agent.claimWaiters.waiters)
	agent.claimWaiters.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d claim waiters left after the timeout", n)
	}
}

// TestCrossTenantSlowSendConsumesWaitBudget covers a send that takes longer
// than the cross-tenant budget. The budget counts from handler entry.
func TestCrossTenantSlowSendConsumesWaitBudget(t *testing.T) {
	agent := mustNewAgent(t, Config{})
	agent.Logger = slog.Default()
	agent.loopbackToken = "tok"
	agent.crossTenantTimeout = 300 * time.Millisecond

	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		id := *m.Message.(*mitev1.MiteMessage_CrossTenantRequest).CrossTenantRequest.RequestId
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(650 * time.Millisecond)
			agent.crossTenantWaiters.resolve(&mitev1.CrossTenantResponse{
				RequestId: &id, Kind: "status", PayloadJson: `{"state":"done"}`,
			})
		}()
		time.Sleep(500 * time.Millisecond)
		return nil
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/cross-tenant/status", strings.NewReader(crossTenantBody("status")))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	agent.crossTenantHandler("status")(rec, req)

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
	agent.crossTenantWaiters.mu.Lock()
	n := len(agent.crossTenantWaiters.waiters)
	agent.crossTenantWaiters.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d cross-tenant waiters left after the timeout", n)
	}
}

// TestHandleRunState_RestartSourceSlowSendConsumesAckWait covers a send that
// takes longer than the ack wait. The wait counts from handler entry.
func TestHandleRunState_RestartSourceSlowSendConsumesAckWait(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAckWaitOverride = 300 * time.Millisecond

	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		c := m.Message.(*mitev1.MiteMessage_RunStateChunk).RunStateChunk
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(650 * time.Millisecond)
			agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
				RunId: c.RunId, Relpath: c.Relpath, Seq: c.Seq, RestartSource: true, Status: ackStatusStored,
			})
		}()
		time.Sleep(500 * time.Millisecond)
		return nil
	}})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "1", "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get(hdrStateReason); got != stateReasonAckTimeout {
		t.Fatalf("reason = %q, want ack-timeout", got)
	}
	agent.runStateAckWaiters.mu.Lock()
	n := len(agent.runStateAckWaiters.waiters)
	agent.runStateAckWaiters.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d waiters left after the timeout", n)
	}
}

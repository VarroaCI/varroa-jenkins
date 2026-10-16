package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// ackingStream answers every acked chunk, a restart-copy chunk or the final
// chunk of a mesh file, with an ack of the given status, as the gateway
// does. The ack resolves before the send returns, so the waiter is already
// registered.
func ackingStream(agent *Agent, status, reason string) {
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		c := m.GetRunStateChunk()
		if c == nil {
			return nil
		}
		agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
			RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
			Status: status, Reason: reason, RestartSource: c.RestartSource,
		})
		return nil
	}})
}

// mesh final-chunk tests post a single-chunk mesh file, seq 0 of total 1.
func meshFinalChunkRequest() *http.Request {
	return newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
}

func TestHandleRunState_RestartSourceWaitsForStoredAck(t *testing.T) {
	agent := newRunStateTestAgent(t)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		c := m.GetRunStateChunk()
		go func() {
			time.Sleep(100 * time.Millisecond)
			agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
				RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
				Status: ackStatusStored, RestartSource: true,
			})
		}()
		return nil
	}})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "1", "4096")
	rec := httptest.NewRecorder()
	start := time.Now()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 after the stored ack, got %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("answered in %v, before the ack arrived", elapsed)
	}
}

func TestHandleRunState_RestartSourceRejectedFinalChunkIs503(t *testing.T) {
	agent := newRunStateTestAgent(t)
	ackingStream(agent, ackStatusRejected, "malformed")

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "1", "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get(hdrStateReason); got != "malformed" {
		t.Fatalf("reason header = %q, want malformed", got)
	}
	if _, refused := agent.runStateAcks.take("run-1", true); refused {
		t.Fatal("a transient refusal was stored; the plugin's retry would be refused twice")
	}
}

func TestHandleRunState_RestartSourceStateCapIsSticky(t *testing.T) {
	agent := newRunStateTestAgent(t)
	sends := 0
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		sends++
		c := m.GetRunStateChunk()
		agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
			RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
			Status: ackStatusRejected, Reason: stateReasonStateCap, RestartSource: true,
		})
		return nil
	}})

	for i := 0; i < 2; i++ {
		req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
		setRestartSourceHeaders(req, "1", "4096")
		rec := httptest.NewRecorder()
		agent.handleRunState(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("post %d: expected 503, got %d", i, rec.Code)
		}
		if got := rec.Header().Get(hdrStateReason); got != stateReasonStateCap {
			t.Fatalf("post %d: reason = %q, want state-cap", i, got)
		}
	}
	if sends != 1 {
		t.Fatalf("sends = %d, want 1: the sticky refusal must answer without a send", sends)
	}
}

func TestHandleRunState_RestartSourceBusyCarriesPause(t *testing.T) {
	agent := newRunStateTestAgent(t)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		c := m.GetRunStateChunk()
		agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
			RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
			Status: ackStatusBusy, Reason: stateReasonBudgetFull, PauseMs: 1500, RestartSource: true,
		})
		return nil
	}})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "1", "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get(hdrStateReason); got != stateReasonBudgetFull {
		t.Fatalf("reason = %q, want gateway-budget-full", got)
	}
	if got := rec.Header().Get(hdrPauseMs); got != "1500" {
		t.Fatalf("pause header = %q, want 1500", got)
	}
}

func TestHandleRunState_RestartSourceAckTimeoutIs503(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAckWaitOverride = 50 * time.Millisecond
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

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
	if got := rec.Header().Get(hdrPauseMs); got != "" {
		t.Fatalf("pause = %q, want none on ack-timeout", got)
	}
	agent.runStateAckWaiters.mu.Lock()
	n := len(agent.runStateAckWaiters.waiters)
	agent.runStateAckWaiters.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d waiters left after the timeout", n)
	}
}

func TestHandleRunState_RestartSourceLateAckGoesToStore(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAckWaitOverride = 50 * time.Millisecond
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	req := newRunStateRequest("run-1", "jobs/team/builds/1/build.xml", 1, 1, 0, 1, 5, 5, "hello")
	setRestartSourceHeaders(req, "1", "4096")
	rec := httptest.NewRecorder()
	agent.handleRunState(rec, req)

	late := &mitev1.RunStateAck{
		RunId: "run-1", Relpath: "jobs/team/builds/1/build.xml", StateGen: 1, Seq: 0,
		Status: ackStatusRejected, Reason: "malformed", RestartSource: true,
	}
	if agent.runStateAckWaiters.resolve(late) {
		t.Fatal("a late ack matched a waiter that already timed out")
	}
	agent.runStateAcks.record(late)
	if r, refused := agent.runStateAcks.take("run-1", true); !refused || r.reason != "malformed" {
		t.Fatalf("late ack not stored: refused=%v reason=%q", refused, r.reason)
	}
}

func TestHandleRunState_MeshNonFinalChunkAnswersAtOnce(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAckWaitOverride = 10 * time.Second
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	// Seq 0 of total 2 is not the file's final chunk, so it gets no wait.
	req := newRunStateRequest("run-1", "jobs/team/builds/1/program.dat", 1, 1, 0, 2, 5, 5, "hello")
	rec := httptest.NewRecorder()
	start := time.Now()
	agent.handleRunState(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a non-final mesh chunk waited %v for an ack", elapsed)
	}
}

func TestHandleRunState_MeshFinalChunkWaitsForStoredAck(t *testing.T) {
	agent := newRunStateTestAgent(t)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		c := m.GetRunStateChunk()
		go func() {
			time.Sleep(100 * time.Millisecond)
			agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
				RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
				Status: ackStatusStored,
			})
		}()
		return nil
	}})

	rec := httptest.NewRecorder()
	start := time.Now()
	agent.handleRunState(rec, meshFinalChunkRequest())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 after the stored ack, got %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("answered in %v, before the ack arrived", elapsed)
	}
}

func TestHandleRunState_MeshFinalChunkBusyCarriesPause(t *testing.T) {
	agent := newRunStateTestAgent(t)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		c := m.GetRunStateChunk()
		agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
			RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
			Status: ackStatusBusy, Reason: stateReasonBudgetFull, PauseMs: 1500,
		})
		return nil
	}})

	rec := httptest.NewRecorder()
	agent.handleRunState(rec, meshFinalChunkRequest())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get(hdrStateReason); got != stateReasonBudgetFull {
		t.Fatalf("reason = %q, want gateway-budget-full", got)
	}
	if got := rec.Header().Get(hdrPauseMs); got != "1500" {
		t.Fatalf("pause header = %q, want 1500", got)
	}
}

func TestHandleRunState_MeshFinalChunkTenantUnresolvedCarriesPause(t *testing.T) {
	agent := newRunStateTestAgent(t)
	setTestStream(agent, &concurrentSendStream{sendFn: func(m *mitev1.MiteMessage) error {
		c := m.GetRunStateChunk()
		agent.runStateAckWaiters.resolve(&mitev1.RunStateAck{
			RunId: c.RunId, Relpath: c.Relpath, StateGen: c.StateGen, Seq: c.Seq,
			Status: ackStatusBusy, Reason: stateReasonTenantUnresolved, PauseMs: 1500,
		})
		return nil
	}})

	rec := httptest.NewRecorder()
	agent.handleRunState(rec, meshFinalChunkRequest())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get(hdrStateReason); got != stateReasonTenantUnresolved {
		t.Fatalf("reason = %q, want tenant-unresolved", got)
	}
	if got := rec.Header().Get(hdrPauseMs); got != "1500" {
		t.Fatalf("pause header = %q, want 1500", got)
	}
}

func TestHandleRunState_MeshFinalChunkAckTimeoutIs503(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.runStateAckWaitOverride = 50 * time.Millisecond
	setTestStream(agent, &concurrentSendStream{sendFn: func(*mitev1.MiteMessage) error { return nil }})

	rec := httptest.NewRecorder()
	agent.handleRunState(rec, meshFinalChunkRequest())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if got := rec.Header().Get(hdrStateReason); got != stateReasonAckTimeout {
		t.Fatalf("reason = %q, want ack-timeout", got)
	}
	if got := rec.Header().Get(hdrPauseMs); got != "" {
		t.Fatalf("pause = %q, want none on ack-timeout", got)
	}
	agent.runStateAckWaiters.mu.Lock()
	n := len(agent.runStateAckWaiters.waiters)
	agent.runStateAckWaiters.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d waiters left after the timeout", n)
	}
}

func TestRestartAckWaiterMap_ResolveMatchesSeq(t *testing.T) {
	m := newRunStateAckWaiterMap()
	key := runStateAckWaitKey("run-1", "jobs/x", 2, true)
	ch, ok := m.register(key)
	if !ok {
		t.Fatal("register refused under the cap")
	}
	if m.resolve(&mitev1.RunStateAck{RunId: "run-1", Relpath: "jobs/x", Seq: 1, RestartSource: true}) {
		t.Fatal("an ack with another seq resolved the waiter")
	}
	if m.resolve(&mitev1.RunStateAck{RunId: "run-1", Relpath: "jobs/x", Seq: 2, RestartSource: false}) {
		t.Fatal("a mesh ack resolved a restart waiter")
	}
	if !m.resolve(&mitev1.RunStateAck{RunId: "run-1", Relpath: "jobs/x", Seq: 2, RestartSource: true, Status: ackStatusStored}) {
		t.Fatal("the matching restart ack did not resolve the waiter")
	}
	if got := <-ch; got.Status != ackStatusStored {
		t.Fatalf("delivered status = %q", got.Status)
	}
}

func TestRunStateAckWaiterMap_MeshWaiterResolvesMeshAck(t *testing.T) {
	m := newRunStateAckWaiterMap()
	key := runStateAckWaitKey("run-1", "jobs/x", 2, false)
	ch, ok := m.register(key)
	if !ok {
		t.Fatal("register refused under the cap")
	}
	if m.resolve(&mitev1.RunStateAck{RunId: "run-1", Relpath: "jobs/x", Seq: 2, RestartSource: true}) {
		t.Fatal("a restart ack resolved a mesh waiter")
	}
	if !m.resolve(&mitev1.RunStateAck{RunId: "run-1", Relpath: "jobs/x", Seq: 2, Status: ackStatusStored}) {
		t.Fatal("the matching mesh ack did not resolve the waiter")
	}
	if got := <-ch; got.Status != ackStatusStored {
		t.Fatalf("delivered status = %q", got.Status)
	}
}

func TestRestartAckWaiterMap_ReleaseAll(t *testing.T) {
	m := newRunStateAckWaiterMap()
	key := runStateAckWaitKey("run-1", "jobs/x", 0, true)
	ch, _ := m.register(key)
	m.releaseAll()
	if ack := m.cancel(key, ch); ack != nil {
		t.Fatalf("released waiter delivered %+v", ack)
	}
	if _, open := <-ch; open {
		t.Fatal("released waiter channel is still open")
	}
	m.mu.Lock()
	n := len(m.waiters)
	m.mu.Unlock()
	if n != 0 {
		t.Fatal("releaseAll left waiters in the map")
	}
}

func TestRestartAckWaitFor(t *testing.T) {
	cases := []struct {
		name         string
		claimTimeout time.Duration
		want         time.Duration
	}{
		{"default budget", 4500 * time.Millisecond, 4000 * time.Millisecond},
		{"operator budget 5s", 5500 * time.Millisecond, 5000 * time.Millisecond},
		{"small budget keeps half", 1000 * time.Millisecond, 500 * time.Millisecond},
		{"margin below half is raised to half", 800 * time.Millisecond, 400 * time.Millisecond},
		{"large budget capped", 20 * time.Second, 9 * time.Second},
		{"zero uses default budget", 0, 4000 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runStateAckWaitFor(tc.claimTimeout); got != tc.want {
				t.Fatalf("runStateAckWaitFor(%v) = %v, want %v", tc.claimTimeout, got, tc.want)
			}
		})
	}
}

func TestRestartAckWaitFor_NestsInsideBudget(t *testing.T) {
	for budget := 100 * time.Millisecond; budget <= 30*time.Second; budget += 50 * time.Millisecond {
		w := runStateAckWaitFor(budget)
		if w <= 0 || w >= budget {
			t.Fatalf("runStateAckWaitFor(%v) = %v, want a wait inside the budget", budget, w)
		}
		if w > runStateAckWaitCap {
			t.Fatalf("runStateAckWaitFor(%v) = %v, above the cap %v", budget, w, runStateAckWaitCap)
		}
	}
}

func TestRestartAckWaitOrDefault_DerivesFromClaimTimeout(t *testing.T) {
	agent := newRunStateTestAgent(t)
	agent.claimTimeout = 2 * time.Second
	if got := agent.runStateAckWaitOrDefault(); got != 1500*time.Millisecond {
		t.Fatalf("runStateAckWaitOrDefault() = %v, want 1.5s", got)
	}
	agent.runStateAckWaitOverride = 50 * time.Millisecond
	if got := agent.runStateAckWaitOrDefault(); got != 50*time.Millisecond {
		t.Fatalf("runStateAckWaitOrDefault() with override = %v, want 50ms", got)
	}
}

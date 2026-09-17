package mite

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// --- test helpers ---

// newTailTestHandler builds a bare BusHandler wired for the tail-lease
// logic alone, with the lease and ack windows shrunk so a timer test does
// not have to wait tens of seconds.
func newTailTestHandler(t *testing.T, leaseTimeout, ackTimeout time.Duration) *BusHandler {
	t.Helper()
	return &BusHandler{
		cluster:          "core",
		tailLeases:       make(map[string]*runTailLease),
		tailLeaseTimeout: leaseTimeout,
		tailAckTimeout:   ackTimeout,
		tailErrTTL:       500 * time.Millisecond,
	}
}

// tailControlsSent returns every TailControl the sender recorded.
func tailControlsSent(sender *recordingSender) []*mitev1.TailControl {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	var out []*mitev1.TailControl
	for _, m := range sender.sent {
		if tc := m.GetTailControl(); tc != nil {
			out = append(out, tc)
		}
	}
	return out
}

func decodeTailReply(t *testing.T, data []byte) tailReply {
	t.Helper()
	var r tailReply
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode tail reply: %v", err)
	}
	return r
}

// --- 4.1: the lease table ---

// TestTailLeaseExpiryPushesStop proves the 90-second no-renewal window:
// a lease started and never renewed clears itself and pushes a stop
// TailControl once the window passes.
func TestTailLeaseExpiryPushesStop(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, 30*time.Millisecond, time.Second)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()

	reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 3,
	}))
	if !reply.OK || reply.LeaseSeconds != tailLeaseSeconds {
		t.Fatalf("unexpected start reply: %+v", reply)
	}
	if !h.isTailActive(runID) {
		t.Fatal("expected the lease active immediately after start")
	}

	// The lease clears under the lock and the stop is pushed after the
	// lock is released, so the stop is the signal to wait on.
	stops := func() int {
		n := 0
		for _, tc := range tailControlsSent(sender) {
			if tc.RunId == runID && tc.Action == "stop" {
				n++
			}
		}
		return n
	}
	waitFor(t, func() bool { return stops() > 0 }, time.Second, "timed out waiting for the stop TailControl on lease expiry")
	if h.isTailActive(runID) {
		t.Fatal("expected the lease inactive once the stop was pushed")
	}
	if n := stops(); n != 1 {
		t.Fatalf("expected exactly one stop TailControl on lease expiry, got %d", n)
	}
}

// TestTailStaleControlIDAcknowledgementIgnored proves a TailAck naming a
// superseded control_id never clears the current lease, and that a
// matching TailAck does.
func TestTailStaleControlIDAcknowledgementIgnored(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, time.Minute)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()
	req := tailRequest{RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1}

	_ = h.handleTailStart(ns, name, sender.send, req)
	lease := h.getExistingTailLease(runID)
	lease.mu.Lock()
	staleControlID := lease.controlID
	lease.mu.Unlock()

	// A renewal mints a fresh control_id, superseding the first.
	_ = h.handleTailStart(ns, name, sender.send, req)
	lease.mu.Lock()
	freshControlID := lease.controlID
	lease.mu.Unlock()
	if freshControlID == staleControlID {
		t.Fatal("expected the renewal to mint a fresh control_id")
	}

	// The stale ack must not clear the lease or record an error.
	h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: staleControlID, Active: false, Error: "stale failure"})
	if !h.isTailActive(runID) {
		t.Fatal("a stale-control_id ack must not clear the current lease")
	}
	if errMsg, ok := h.getKeptTailError(runID); ok {
		t.Fatalf("a stale-control_id ack must not record an error, got %q", errMsg)
	}

	// The matching ack does take effect.
	h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: freshControlID, Active: false, Error: "real failure"})
	if h.isTailActive(runID) {
		t.Fatal("expected the current-control_id ack to clear the lease")
	}
}

// --- 4.3: chunks relayed only while a lease is live ---

// TestOnLogChunkPublishesOnlyWhileLeased proves the gateway publishes a
// stored chunk on LogSubject only while its run's lease is live, and
// exactly once per chunk.
func TestOnLogChunkPublishesOnlyWhileLeased(t *testing.T) {
	natsSrv := startNATS(t)
	busConn, err := bus.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	const ns, name = "ns1", "cell1"
	sender := &recordingSender{}
	owner := newFakeOwnerRequester()
	h := newLogTestHandler(t, owner, sender)
	h.conn = busConn

	// An unleased run: zero publishes.
	unleasedRun := newTestRunID()
	owner.setAnswer(unleasedRun, ownerLookupResponse{Found: true, Cluster: "core", Namespace: ns, Cell: name, Instance: "inst1"})
	var unleasedCount int32
	subUnleased, err := busConn.SubscribeData(bus.LogSubject("core", ns, name, unleasedRun), func([]byte) {
		atomic.AddInt32(&unleasedCount, 1)
	})
	if err != nil {
		t.Fatalf("subscribe unleased: %v", err)
	}
	defer subUnleased.Unsubscribe()

	h.OnLogChunk(name, ns, &mitev1.LogChunk{
		RunId: unleasedRun, JobPath: "j", BuildNumber: 1, Seq: 0, Data: []byte("a"), CellInstance: "inst1",
	})
	waitForLogAcks(t, sender, 1)
	// Give a stray publish (there should be none) a chance to arrive.
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&unleasedCount); got != 0 {
		t.Fatalf("expected zero publishes for an unleased run, got %d", got)
	}

	// A leased run: one publish per chunk.
	leasedRun := newTestRunID()
	owner.setAnswer(leasedRun, ownerLookupResponse{Found: true, Cluster: "core", Namespace: ns, Cell: name, Instance: "inst1"})
	lease := h.getOrCreateTailLease(leasedRun, ns, name)
	lease.mu.Lock()
	lease.active = true
	lease.mu.Unlock()

	var leasedCount int32
	subLeased, err := busConn.SubscribeData(bus.LogSubject("core", ns, name, leasedRun), func([]byte) {
		atomic.AddInt32(&leasedCount, 1)
	})
	if err != nil {
		t.Fatalf("subscribe leased: %v", err)
	}
	defer subLeased.Unsubscribe()

	h.OnLogChunk(name, ns, &mitev1.LogChunk{
		RunId: leasedRun, JobPath: "j", BuildNumber: 1, Seq: 0, Data: []byte("a"), CellInstance: "inst1",
	})
	h.OnLogChunk(name, ns, &mitev1.LogChunk{
		RunId: leasedRun, JobPath: "j", BuildNumber: 1, Seq: 1, Data: []byte("b"), CellInstance: "inst1",
	})
	waitFor(t, func() bool { return atomic.LoadInt32(&leasedCount) >= 2 }, 2*time.Second, "timed out waiting for two tail publishes")
	if got := atomic.LoadInt32(&leasedCount); got != 2 {
		t.Fatalf("expected exactly one publish per chunk (2 chunks), got %d", got)
	}
}

// --- 4.4: every terminal condition clears the lease ---

// TestTailLeaseClearsOnEveryTerminalCondition is the log-live-tail spec's
// terminal-condition table as one test: a stop request, a 90-second gap,
// the final chunk, a mite disconnect, an active=false TailAck, and a
// missing acknowledgement after 10 seconds. Only the two request-driven
// terminations (stop, and no renewal) push a further TailControl.
func TestTailLeaseClearsOnEveryTerminalCondition(t *testing.T) {
	const longTimeout = 5 * time.Second

	cases := []struct {
		name           string
		leaseTimeout   time.Duration
		ackTimeout     time.Duration
		trigger        func(h *BusHandler, sender *recordingSender, ns, name, runID, controlID string)
		wantStopPushed bool
	}{
		{
			name:         "stop request",
			leaseTimeout: longTimeout,
			ackTimeout:   longTimeout,
			trigger: func(h *BusHandler, sender *recordingSender, ns, name, runID, controlID string) {
				_ = h.handleTailStop(ns, name, sender.send, tailRequest{RunID: runID, Action: "stop"})
			},
			wantStopPushed: true,
		},
		{
			name:           "no renewal for 90 seconds",
			leaseTimeout:   30 * time.Millisecond,
			ackTimeout:     longTimeout,
			trigger:        func(h *BusHandler, sender *recordingSender, ns, name, runID, controlID string) {},
			wantStopPushed: true,
		},
		{
			name:         "chunk with final",
			leaseTimeout: longTimeout,
			ackTimeout:   longTimeout,
			trigger: func(h *BusHandler, sender *recordingSender, ns, name, runID, controlID string) {
				h.clearTailLeaseOnFinal(runID)
			},
			wantStopPushed: false,
		},
		{
			name:         "mite disconnect",
			leaseTimeout: longTimeout,
			ackTimeout:   longTimeout,
			trigger: func(h *BusHandler, sender *recordingSender, ns, name, runID, controlID string) {
				h.clearTailLeasesForController(ns, name)
			},
			wantStopPushed: false,
		},
		{
			name:         "TailAck active false",
			leaseTimeout: longTimeout,
			ackTimeout:   longTimeout,
			trigger: func(h *BusHandler, sender *recordingSender, ns, name, runID, controlID string) {
				h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: controlID, Active: false, Error: "plugin refused"})
			},
			wantStopPushed: false,
		},
		{
			name:           "no acknowledgement in 10 seconds",
			leaseTimeout:   longTimeout,
			ackTimeout:     30 * time.Millisecond,
			trigger:        func(h *BusHandler, sender *recordingSender, ns, name, runID, controlID string) {},
			wantStopPushed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &recordingSender{}
			h := newTailTestHandler(t, tc.leaseTimeout, tc.ackTimeout)
			const ns, name = "ns1", "cell1"
			runID := newTestRunID()

			reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
				RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
			}))
			if !reply.OK {
				t.Fatalf("setup start failed: %+v", reply)
			}
			lease := h.getExistingTailLease(runID)
			lease.mu.Lock()
			controlID := lease.controlID
			lease.mu.Unlock()

			tc.trigger(h, sender, ns, name, runID, controlID)

			waitFor(t, func() bool { return !h.isTailActive(runID) }, time.Second, "timed out waiting for the lease to clear")

			// Every terminal condition removes the entry outright, not
			// just deactivates it, so a long-lived gateway holds no
			// lease past the run it belonged to.
			waitFor(t, func() bool {
				h.tailMu.Lock()
				defer h.tailMu.Unlock()
				return len(h.tailLeases) == 0
			}, time.Second, "timed out waiting for the lease entry to be deleted")

			// The lease clears under the lock and the stop is pushed after
			// the lock is released, so an expected stop is waited for.
			stopPushed := func() bool {
				for _, m := range tailControlsSent(sender) {
					if m.RunId == runID && m.Action == "stop" {
						return true
					}
				}
				return false
			}
			if tc.wantStopPushed {
				waitFor(t, stopPushed, time.Second, "timed out waiting for the stop TailControl")
			} else if stopPushed() {
				t.Fatal("stop TailControl pushed = true, want false")
			}
		})
	}
}

// TestTailLeaseExpiryRacingRenewalKeepsRenewedLease proves the exact
// race a validate-then-unlock-then-delete implementation loses: a
// renewal that lands after the expiry timer's callback has already
// validated its stale control_id, but before that callback deletes the
// lease, must not have its own fresh lease destroyed or a stale stop
// pushed over it (design.md D5's lease table: a renewal keeps the lease
// alive). h.onTailLeaseExpired is called directly with the pre-renewal
// control_id to model a real timer that fired before the renewal but is
// only now getting the CPU to run its callback body.
func TestTailLeaseExpiryRacingRenewalKeepsRenewedLease(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, time.Minute)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()

	reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("setup start failed: %+v", reply)
	}
	lease := h.getExistingTailLease(runID)
	lease.mu.Lock()
	staleControlID := lease.controlID
	lease.mu.Unlock()

	// The renewal lands first.
	reply = decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("renewal failed: %+v", reply)
	}
	lease.mu.Lock()
	freshControlID := lease.controlID
	lease.mu.Unlock()
	if freshControlID == staleControlID {
		t.Fatal("expected the renewal to mint a fresh control_id")
	}

	// The stale timer's callback body runs now, naming the pre-renewal
	// control_id — exactly what a real timer racing the renewal above
	// would do.
	h.onTailLeaseExpired(sender.send, runID, ns, name, staleControlID)

	if !h.isTailActive(runID) {
		t.Fatal("expected the renewed lease to stay active despite a racing stale expiry")
	}
	if h.getExistingTailLease(runID) == nil {
		t.Fatal("expected the renewed lease to remain in the lease table")
	}
	for _, tc := range tailControlsSent(sender) {
		if tc.RunId == runID && tc.Action == "stop" {
			t.Fatal("expected no stop TailControl pushed by the stale expiry")
		}
	}
}

// TestTailLeaseExpiryRacingRenewalStressNeverLosesAnActiveLease is a
// concurrency stress test for the same invariant as the test above,
// this time under real goroutine scheduling and -race, rather than a
// hand-sequenced call: with the last operation always a renewal, the
// lease must never end up cleared by a racing expiry.
//
// Each iteration races a fresh renewal against onTailLeaseExpired
// called with the control_id the lease held the instant before that
// renewal — the same information a real timer's closure would have
// captured moments earlier. expireTailLeaseIfCurrent only clears a
// lease whose control_id still matches, so once both calls finish the
// lease is live either way: the expiry wins and clears it, so the
// renewal then finds none to reuse and starts a fresh one; or the
// renewal wins first, so the expiry's now-stale control_id no-ops.
// Joining the pair before the next iteration keeps the race live for
// the whole run. A real timer would instead need a lease window
// comfortably above any scheduler stall, closing the window between
// the loop's last renewal and the assertion below — which then leaves
// the timer no realistic chance of firing near a renewal at all.
func TestTailLeaseExpiryRacingRenewalStressNeverLosesAnActiveLease(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, time.Minute)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()

	reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("setup start failed: %+v", reply)
	}

	stop := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(stop) {
		lease := h.getExistingTailLease(runID)
		lease.mu.Lock()
		controlID := lease.controlID
		lease.mu.Unlock()

		var wg sync.WaitGroup
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			h.onTailLeaseExpired(sender.send, runID, ns, name, id)
		}(controlID)
		_ = h.handleTailStart(ns, name, sender.send, tailRequest{
			RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
		})
		wg.Wait()
	}

	if !h.isTailActive(runID) {
		t.Fatal("expected the lease to stay active: the last operation was always a renewal")
	}
}

// TestTailLeaseRenewalRacingExpiryNeverOrphans proves the renewal path
// (handleTailStart on an already-leased run) and expireTailLeaseIfCurrent
// share one atomic lookup-and-mutate section, so a renewal racing an
// expiry can never activate a lease object that the expiry has already
// removed from h.tailLeases. A separate check-then-lock renewal would
// leave such an orphan behind: it reports success, but nothing in
// h.tailLeases holds it any more, so a later acknowledgement can never
// find it. This races the two real handlers, back to back with no timer
// delay, for 200ms under -race, then proves whatever lease the race
// settles on is the one an acknowledgement actually reaches.
func TestTailLeaseRenewalRacingExpiryNeverOrphans(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, time.Minute)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()

	reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("setup start failed: %+v", reply)
	}

	stop := time.Now().Add(200 * time.Millisecond)
	var wg sync.WaitGroup

	// Renewals, back to back — the same shape a reconnected mite's
	// repeated tail-start requests produce.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			_ = h.handleTailStart(ns, name, sender.send, tailRequest{
				RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
			})
		}
	}()

	// Expiries, each naming whatever control_id the lease holds the
	// instant it reads it — the same information a real 90-second
	// timer's closure would have captured moments earlier. Firing them
	// back to back with no timer delay maximizes the chance of landing
	// inside the exact window expireTailLeaseIfCurrent's one-critical-
	// section design closes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			lease := h.getExistingTailLease(runID)
			if lease == nil {
				continue
			}
			lease.mu.Lock()
			controlID := lease.controlID
			lease.mu.Unlock()
			h.onTailLeaseExpired(sender.send, runID, ns, name, controlID)
		}
	}()

	wg.Wait()

	// The race has settled. At most one live lease for runID, ever —
	// two would mean a renewal created a second entry instead of
	// reusing or replacing the map's own.
	h.tailMu.Lock()
	liveCount := len(h.tailLeases)
	lease := h.tailLeases[runID]
	h.tailMu.Unlock()
	if liveCount > 1 {
		t.Fatalf("expected at most one live lease for runID after the race, got %d", liveCount)
	}
	if lease == nil {
		return // the last race went to the expiry: nothing left to probe.
	}

	lease.mu.Lock()
	controlID := lease.controlID
	lease.mu.Unlock()

	// An acknowledgement for the lease's own current control_id must
	// reach it and record its error. An orphaned lease — activated after
	// expireTailLeaseIfCurrent already deleted it from the map — would
	// leave this acknowledgement unmatched: OnTailAck looks the lease up
	// through h.tailLeases, never through a pointer the renewal call
	// itself held on to.
	h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: controlID, Active: false, Error: "probe"})
	if _, ok := h.getKeptTailError(runID); !ok {
		t.Fatal("expected the acknowledgement for the map's own current control_id to reach it and keep the probe error")
	}
}

// TestTailStaleAckAfterExpiryAndRenewalNeverClearsRenewedLease proves the
// exact interleaving a reviewer's finding on OnTailAck flagged: an
// acknowledgement captures a lease's control_id, the lease then expires,
// a renewal installs a fresh lease for the same run_id, and only then
// does the acknowledgement's own handling run, naming the now-superseded
// control_id. The renewed lease must stay live and findable — never
// deleted under the strength of a stale ack's own validation — and a
// fresh acknowledgement naming the renewed lease's own control_id must
// still reach and clear it.
func TestTailStaleAckAfterExpiryAndRenewalNeverClearsRenewedLease(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, time.Minute)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()

	// The ack that will arrive late names this control_id: whatever the
	// lease held right after start.
	reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("setup start failed: %+v", reply)
	}
	lease := h.getExistingTailLease(runID)
	lease.mu.Lock()
	staleControlID := lease.controlID
	lease.mu.Unlock()

	// The lease expires — modeling the 90-second timer firing — and is
	// removed from h.tailLeases.
	h.onTailLeaseExpired(sender.send, runID, ns, name, staleControlID)
	if h.getExistingTailLease(runID) != nil {
		t.Fatal("setup: expected expiry to remove the lease")
	}

	// A renewal installs a fresh lease for the same run_id, minting a new
	// control_id.
	reply = decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("renewal failed: %+v", reply)
	}
	renewed := h.getExistingTailLease(runID)
	renewed.mu.Lock()
	freshControlID := renewed.controlID
	renewed.mu.Unlock()
	if freshControlID == staleControlID {
		t.Fatal("expected the renewal to mint a fresh control_id")
	}

	// The old acknowledgement, naming the pre-expiry control_id, is
	// handled last.
	h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: staleControlID, Active: false, Error: "stale failure"})

	if !h.isTailActive(runID) {
		t.Fatal("a stale acknowledgement must not clear the renewed lease")
	}
	if h.getExistingTailLease(runID) != renewed {
		t.Fatal("expected the renewed lease to remain the map's entry for this run_id")
	}
	if _, ok := h.getKeptTailError(runID); ok {
		t.Fatal("a stale acknowledgement must not record an error over the renewed lease")
	}

	// A fresh acknowledgement naming the renewed lease's own control_id
	// must still reach it.
	h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: freshControlID, Active: false, Error: "real failure"})
	if h.isTailActive(runID) {
		t.Fatal("expected the fresh-control_id acknowledgement to clear the renewed lease")
	}
	if errMsg, ok := h.getKeptTailError(runID); !ok || errMsg != "real failure" {
		t.Fatalf("expected the renewed lease's own acknowledgement to record its error, got %q, ok=%v", errMsg, ok)
	}
}

// TestTailAckExpiryRenewalStopStressNeverClearsANewerLease is a
// concurrency stress test for the same invariant
// TestTailStaleAckAfterExpiryAndRenewalNeverClearsRenewedLease checks by
// hand: OnTailAck, stopTailLease (behind handleTailStop), and
// expireTailLeaseIfCurrent (behind onTailLeaseExpired) must never delete a
// lease a renewal installed after they looked one up. It races
// acknowledgements, expiries, renewals, and stops against one run_id under
// -race for a sustained period, then proves the race settles on at most
// one live lease, and that a fresh renewal plus a matching acknowledgement
// still work end to end.
func TestTailAckExpiryRenewalStopStressNeverClearsANewerLease(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, time.Minute)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()

	reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("setup start failed: %+v", reply)
	}

	stop := time.Now().Add(300 * time.Millisecond)
	var wg sync.WaitGroup

	// Renewals, back to back — the shape a reconnected or polling BFF
	// produces.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			_ = h.handleTailStart(ns, name, sender.send, tailRequest{
				RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
			})
		}
	}()

	// Expiries, each naming whatever control_id the lease holds the
	// instant it reads it — the same information a real 90-second timer's
	// closure would have captured moments earlier.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			lease := h.getExistingTailLease(runID)
			if lease == nil {
				continue
			}
			lease.mu.Lock()
			controlID := lease.controlID
			lease.mu.Unlock()
			h.onTailLeaseExpired(sender.send, runID, ns, name, controlID)
		}
	}()

	// Acknowledgements, each naming whatever control_id the lease holds
	// right now — the same information a real mite's TailAck carries for
	// whatever TailControl push it last saw.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			lease := h.getExistingTailLease(runID)
			if lease == nil {
				continue
			}
			lease.mu.Lock()
			controlID := lease.controlID
			lease.mu.Unlock()
			h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: controlID, Active: false, Error: "stress"})
		}
	}()

	// Stops, racing the same run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			_ = h.handleTailStop(ns, name, sender.send, tailRequest{RunID: runID, Action: "stop"})
		}
	}()

	wg.Wait()

	// The race has settled. At most one live lease for runID, ever — two
	// would mean some path installed a second entry instead of reusing or
	// replacing the map's own.
	h.tailMu.Lock()
	liveCount := len(h.tailLeases)
	h.tailMu.Unlock()
	if liveCount > 1 {
		t.Fatalf("expected at most one live lease for runID after the race, got %d", liveCount)
	}

	// A kept error from the stress run's last acknowledgement would block
	// the verification start below; clear it so the invariant checked
	// here is the lease table, not the kept-error gate (covered by
	// TestTailKeptErrorSurvivesLeaseDeletion).
	h.clearKeptTailError(runID)

	reply = decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("post-race renewal failed: %+v", reply)
	}
	final := h.getExistingTailLease(runID)
	if final == nil {
		t.Fatal("expected the post-race renewal to install a lease")
	}
	final.mu.Lock()
	finalControlID := final.controlID
	final.mu.Unlock()

	h.OnTailAck(name, ns, &mitev1.TailAck{RunId: runID, ControlId: finalControlID, Active: false, Error: "final probe"})
	if h.isTailActive(runID) {
		t.Fatal("expected the final acknowledgement to clear the post-race lease")
	}
	if errMsg, ok := h.getKeptTailError(runID); !ok || errMsg != "final probe" {
		t.Fatalf("expected the final acknowledgement's own error kept, got %q, ok=%v", errMsg, ok)
	}
}

// TestTailKeptErrorSurvivesLeaseDeletion proves the kept error a failed
// tail leaves behind is still answered on the next start request even
// though its lease is deleted outright, not merely deactivated
// (design.md D5: "The gateway returns a kept error on the next request
// for that run").
func TestTailKeptErrorSurvivesLeaseDeletion(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, 30*time.Millisecond)
	const ns, name = "ns1", "cell1"
	runID := newTestRunID()

	reply := decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if !reply.OK {
		t.Fatalf("setup start failed: %+v", reply)
	}

	// No TailAck ever arrives: the ack-wait timer fires, deletes the
	// lease, and keeps the error.
	waitFor(t, func() bool {
		h.tailMu.Lock()
		defer h.tailMu.Unlock()
		return len(h.tailLeases) == 0
	}, time.Second, "timed out waiting for the ack-timeout to delete the lease")

	reply = decodeTailReply(t, h.handleTailStart(ns, name, sender.send, tailRequest{
		RunID: runID, Action: "start", JobPath: "folder/job", BuildNumber: 1,
	}))
	if reply.OK || reply.Error == "" {
		t.Fatalf("expected the kept error answered on the next start request despite the deleted lease, got %+v", reply)
	}
}

// TestSweepExpiredTailErrorsRemovesEveryExpiredEntry proves
// h.tailErrors does not accumulate one entry per run that ever failed
// its tail: getKeptTailError only prunes the one run_id a caller asks
// about, so a run that never asks again would otherwise keep its entry
// forever. Many runs' errors, all expired, are gone after one sweep;
// one still-live entry survives it.
func TestSweepExpiredTailErrorsRemovesEveryExpiredEntry(t *testing.T) {
	h := newTailTestHandler(t, time.Minute, time.Minute)

	const expiredCount = 50
	for i := 0; i < expiredCount; i++ {
		h.setKeptTailError(newTestRunID(), "tail failed", -time.Second) // already expired
	}
	liveRunID := newTestRunID()
	h.setKeptTailError(liveRunID, "tail failed", time.Minute)

	h.tailMu.Lock()
	before := len(h.tailErrors)
	h.tailMu.Unlock()
	if before != expiredCount+1 {
		t.Fatalf("setup: len(tailErrors) = %d, want %d", before, expiredCount+1)
	}

	h.sweepExpiredTailErrors()

	h.tailMu.Lock()
	after := len(h.tailErrors)
	_, liveSurvived := h.tailErrors[liveRunID]
	h.tailMu.Unlock()
	if after != 1 {
		t.Fatalf("len(tailErrors) after sweep = %d, want 1 (only the live entry)", after)
	}
	if !liveSurvived {
		t.Fatal("expected the still-live entry to survive the sweep")
	}
}

// TestTailStartRequestSweepsExpiredErrors proves the sweep also runs
// from ordinary request traffic, per handleTailRequest's contract, so a
// gateway that only ever sees start/stop calls — never an idle timer —
// still bounds the map.
func TestTailStartRequestSweepsExpiredErrors(t *testing.T) {
	sender := &recordingSender{}
	h := newTailTestHandler(t, time.Minute, time.Minute)
	const ns, name = "ns1", "cell1"

	expiredRunID := newTestRunID()
	h.setKeptTailError(expiredRunID, "tail failed", -time.Second)

	// handleTailRequest, not handleTailStart directly: the sweep runs at
	// the request-dispatch boundary every real TailSubject message
	// crosses, not inside the start/stop handlers themselves.
	body, err := json.Marshal(tailRequest{RunID: newTestRunID(), Action: "start", JobPath: "folder/job", BuildNumber: 1})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	_ = h.handleTailRequest(ns, name, sender.send, body)

	h.tailMu.Lock()
	_, stillThere := h.tailErrors[expiredRunID]
	h.tailMu.Unlock()
	if stillThere {
		t.Fatal("expected the expired entry swept by an unrelated start request")
	}
}

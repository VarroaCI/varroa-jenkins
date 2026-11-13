package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// fairnessImp returns an imperative work item. Its type 0 is unknown, so
// the worker answers it at once with a failure result.
func fairnessImp(id string) commandWork {
	return commandWork{
		imperativeCmd: &mitev1.ImperativeCommand{CommandId: id},
		ctx:           context.Background(),
	}
}

func fairnessDesired(id string) commandWork {
	return commandWork{
		desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: id},
		ctx:             context.Background(),
	}
}

// takeAll takes up to n commands. After each imperative take, refill
// queues the next imperative, which models the gateway refill.
func takeAll(mb *commandMailbox, n int, refill func() (commandWork, bool)) []string {
	var ids []string
	for i := 0; i < n; i++ {
		w, ok := mb.take()
		if !ok {
			break
		}
		if w.imperativeCmd != nil {
			ids = append(ids, w.imperativeCmd.CommandId)
			if refill != nil {
				if next, ok := refill(); ok {
					mb.putImperative(next)
				}
			}
		} else {
			ids = append(ids, w.desiredStateCmd.CommandId)
		}
	}
	return ids
}

func TestCommandMailboxDesiredWaitsAtMostSkipLimit(t *testing.T) {
	mb := newCommandMailbox()
	mb.putDesired(fairnessDesired("d"))
	for i := 0; i < desiredSkipLimit; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("i%d", i)))
	}
	next := desiredSkipLimit
	ids := takeAll(mb, 40, func() (commandWork, bool) {
		w := fairnessImp(fmt.Sprintf("i%d", next))
		next++
		return w, true
	})

	if len(ids) < desiredSkipLimit+1 || ids[desiredSkipLimit] != "d" {
		t.Fatalf("take order = %v, want the desired state at index %d", ids, desiredSkipLimit)
	}
	want := 0
	for _, id := range ids {
		if id == "d" {
			continue
		}
		if id != fmt.Sprintf("i%d", want) {
			t.Fatalf("imperative order = %v, want FIFO", ids)
		}
		want++
	}
}

func TestCommandMailboxDesiredStreamDoesNotStarveImperatives(t *testing.T) {
	mb := newCommandMailbox()
	mb.putDesired(fairnessDesired("d0"))
	for i := 0; i < desiredSkipLimit; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("i%d", i)))
	}
	next, pushes := desiredSkipLimit, 0
	var order []string
	for take := 0; take < 90; take++ {
		w, ok := mb.take()
		if !ok {
			t.Fatalf("take %d found no command", take)
		}
		if w.imperativeCmd != nil {
			order = append(order, "i")
			mb.putImperative(fairnessImp(fmt.Sprintf("i%d", next)))
			next++
		} else {
			order = append(order, "d")
		}
		// A newer desired state arrives after every take, as a push stream would.
		pushes++
		mb.putDesired(fairnessDesired(fmt.Sprintf("d%d", pushes)))
	}

	imperatives, run := 0, 0
	var runs []int
	for _, s := range order {
		if s == "i" {
			imperatives++
			run++
			continue
		}
		if run > 0 {
			runs = append(runs, run)
		}
		run = 0
	}
	for _, r := range runs {
		if r != desiredSkipLimit {
			t.Fatalf("imperatives between desired takes = %v, want each %d", runs, desiredSkipLimit)
		}
	}
	if len(runs) < 9 {
		t.Fatalf("desired takes = %d, want at least 9 over 90 takes", len(runs))
	}
	if imperatives < 80 {
		t.Fatalf("imperatives taken = %d, want at least 80", imperatives)
	}
}

func TestCommandMailboxSkipCountResetsOnServedDesired(t *testing.T) {
	mb := newCommandMailbox()
	mb.putDesired(fairnessDesired("d"))
	for i := 0; i < 14; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("i%d", i)))
	}
	if got := takeAll(mb, 5, nil); len(got) != 5 || got[4] != "i4" {
		t.Fatalf("first takes = %v, want i0 to i4", got)
	}
	served, ok := mb.serveDesired()
	if !ok || served.desiredStateCmd.CommandId != "d" {
		t.Fatalf("serveDesired = %+v, %v, want d", served, ok)
	}
	mb.putDesired(fairnessDesired("d2"))

	got := takeAll(mb, desiredSkipLimit+1, nil)
	for i := 0; i < desiredSkipLimit; i++ {
		if want := fmt.Sprintf("i%d", 5+i); got[i] != want {
			t.Fatalf("takes after the served state = %v, want %d imperatives before d2", got, desiredSkipLimit)
		}
	}
	if got[desiredSkipLimit] != "d2" {
		t.Fatalf("take %d after the served state = %q, want d2", desiredSkipLimit+1, got[desiredSkipLimit])
	}
}

// TestCommandMailboxInlineRestoreKeepsWait verifies that an inline take that
// restores the command unserved does not restart its wait. Trigger waits do
// this, and each one answers an imperative in between.
func TestCommandMailboxInlineRestoreKeepsWait(t *testing.T) {
	mb := newCommandMailbox()
	mb.putDesired(fairnessDesired("d"))
	for i := 0; i < 20; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("i%d", i)))
	}
	got := takeAll(mb, 3, nil)
	for i := 0; i < 3; i++ {
		inline, ok := mb.takeDesired()
		if !ok || inline.desiredStateCmd.CommandId != "d" {
			t.Fatalf("inline takeDesired = %+v, %v, want d", inline, ok)
		}
		mb.restoreDesired(inline)
		got = append(got, takeAll(mb, 1, nil)...)
	}
	// Six imperatives ran while d waited. d is due at index desiredSkipLimit.
	for len(got) < desiredSkipLimit+1 {
		got = append(got, takeAll(mb, 1, nil)...)
	}
	if got[desiredSkipLimit] != "d" {
		t.Fatalf("takes = %v, want d at index %d", got, desiredSkipLimit)
	}
}

func TestCommandMailboxReplacedDesiredKeepsSkipCount(t *testing.T) {
	mb := newCommandMailbox()
	mb.putDesired(fairnessDesired("d1"))
	for i := 0; i < 14; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("i%d", i)))
	}
	if got := takeAll(mb, 5, nil); len(got) != 5 {
		t.Fatalf("first takes = %v, want five imperatives", got)
	}
	mb.putDesired(fairnessDesired("d2"))

	got := takeAll(mb, 4, nil)
	if len(got) != 4 || got[3] != "d2" {
		t.Fatalf("takes after the replace = %v, want three imperatives and then d2", got)
	}
}

// idleGateJenkins answers the job summary that the idle check reads. Call n
// (1-based) is busy when busy(n) is true.
type idleGateJenkins struct {
	srv          *httptest.Server
	summaryCalls atomic.Int32
	busy         func(call int32) bool
}

func newIdleGateJenkins(t *testing.T, busy func(call int32) bool) *idleGateJenkins {
	t.Helper()
	f := &idleGateJenkins{busy: busy}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Jenkins-Session", "sess-1")
		if r.URL.Path == "/api/json" && strings.HasPrefix(r.URL.Query().Get("tree"), "jobs[name,color") {
			n := f.summaryCalls.Add(1)
			color := "blue"
			if f.busy(n) {
				color = "blue_anime"
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jobs":[{"name":"j","color":%q}]}`, color)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// newIdleGateWorker starts a command worker on mb against the fake Jenkins.
// Every result is recorded by command identifier, in arrival order. onResult
// runs after each result is recorded, so a test can refill the queue.
func newIdleGateWorker(t *testing.T, f *idleGateJenkins, mb *commandMailbox, onResult func(id string)) func() []string {
	t.Helper()
	var mu sync.Mutex
	var results []string
	stream := &concurrentSendStream{
		sendFn: func(m *mitev1.MiteMessage) error {
			cr := m.GetCommandResult()
			if cr == nil {
				return nil
			}
			mu.Lock()
			results = append(results, cr.CommandId)
			mu.Unlock()
			if onResult != nil {
				onResult(cr.CommandId)
			}
			return nil
		},
		recvFn: func() (*mitev1.OperatorMessage, error) { return nil, context.Canceled },
	}
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: f.srv.URL})
	agent.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	setTestStream(agent, stream)
	agent.miteDir = t.TempDir()
	agent.writeMarker(appliedMarker{composite: "hash-1", session: "sess-1"})
	agent.markItemsApplied()
	agent.heartbeatInterval = time.Hour
	mb.desiredFirst = func() bool { return !agent.itemsApplied() }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go agent.commandWorker(ctx, mb)

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), results...)
	}
}

func idleGateDesired(id string) commandWork {
	return commandWork{
		desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: id, DesiredStateHash: "hash-1", ApplyWhen: "idle"},
		ctx:             context.Background(),
	}
}

// waitForFairness polls cond until it holds or the test fails.
func waitForFairness(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCommandWorkerIdleDeferralUnderImperativeBacklog verifies that a deferred
// idle apply is checked again during a backlog, and that it runs within a
// bounded number of imperatives once Jenkins is idle.
func TestCommandWorkerIdleDeferralUnderImperativeBacklog(t *testing.T) {
	var idle atomic.Bool
	f := newIdleGateJenkins(t, func(int32) bool { return !idle.Load() })
	mb := newCommandMailbox()

	const phase1, phase2 = 44, 48
	var p1Queued, p2Queued atomic.Int32
	p1Queued.Store(desiredSkipLimit)
	got := newIdleGateWorker(t, f, mb, func(id string) {
		switch {
		case strings.HasPrefix(id, "p1-") && p1Queued.Load() < phase1:
			mb.putImperative(fairnessImp(fmt.Sprintf("p1-%d", p1Queued.Add(1))))
		case strings.HasPrefix(id, "p2-") && p2Queued.Load() < phase2:
			mb.putImperative(fairnessImp(fmt.Sprintf("p2-%d", p2Queued.Add(1))))
		}
	})

	// Phase 1: Jenkins busy. The apply defers while the backlog runs.
	mb.putDesired(idleGateDesired("d"))
	for i := 0; i < desiredSkipLimit; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("p1-%d", i)))
	}
	waitForFairness(t, "44 phase-1 results", func() bool { return len(got()) >= phase1 })
	for _, id := range got() {
		if id == "d" {
			t.Fatalf("desired state applied while Jenkins was busy: %v", got())
		}
	}
	if calls := f.summaryCalls.Load(); calls < phase1/(desiredSkipLimit+1) {
		t.Fatalf("summary calls = %d during the backlog, want at least %d", calls, phase1/(desiredSkipLimit+1))
	}

	// Phase 2: Jenkins idle. The deferred apply must run within the bound.
	idle.Store(true)
	p2Queued.Store(desiredSkipLimit)
	for i := 0; i < desiredSkipLimit; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("p2-%d", i)))
	}
	waitForFairness(t, "all phase-2 results", func() bool { return len(got()) >= phase1+phase2+1 })

	results := got()
	dAt := -1
	for i, id := range results {
		if id == "d" {
			dAt = i
		}
	}
	if dAt < phase1 || dAt-phase1 > desiredSkipLimit+1 {
		t.Fatalf("desired state at result %d, want at most %d phase-2 results before it", dAt, desiredSkipLimit+1)
	}
	if dAt == len(results)-1 {
		t.Fatal("no phase-2 imperative ran after the desired state")
	}
}

// TestCommandWorkerDeferredIdleApplyLetsQueuedImperativesRun verifies that a
// deferred idle apply does not hold queued imperatives. The worker wakes at
// once, so the imperatives run in order and the apply stays deferred.
func TestCommandWorkerDeferredIdleApplyLetsQueuedImperativesRun(t *testing.T) {
	f := newIdleGateJenkins(t, func(int32) bool { return true })
	mb := newCommandMailbox()
	mb.putDesired(idleGateDesired("d"))
	const n = 12
	for i := 0; i < n; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("i%d", i)))
	}
	var atI8 atomic.Int32
	atI8.Store(-1)
	got := newIdleGateWorker(t, f, mb, func(id string) {
		if id == "i8" {
			atI8.Store(f.summaryCalls.Load())
		}
	})

	waitForFairness(t, "12 imperative results", func() bool { return len(got()) >= n })
	results := got()
	for i, id := range results {
		if id != fmt.Sprintf("i%d", i) {
			t.Fatalf("results = %v, want the 12 imperatives in FIFO order and no desired state", results)
		}
	}
	if calls := atI8.Load(); calls < 1 {
		t.Fatalf("summary calls at the i8 result = %d, want at least 1 (the worker must wake on the queue)", calls)
	}
}

// TestCommandWorkerIdleDeferralKeepsNewerDesired verifies that a deferred idle
// apply does not overwrite a newer desired state that arrived during the
// deferral.
func TestCommandWorkerIdleDeferralKeepsNewerDesired(t *testing.T) {
	release := make(chan struct{})
	f := newIdleGateJenkins(t, func(call int32) bool {
		if call == 1 {
			<-release
			return true
		}
		return false
	})
	mb := newCommandMailbox()
	mb.putDesired(idleGateDesired("d1"))
	got := newIdleGateWorker(t, f, mb, nil)

	waitForFairness(t, "the first summary call", func() bool { return f.summaryCalls.Load() >= 1 })
	mb.putDesired(idleGateDesired("d2"))
	mb.putImperative(fairnessImp("i1"))
	close(release)

	waitForFairness(t, "two results", func() bool { return len(got()) >= 2 })
	results := got()
	if len(results) != 2 || results[0] != "i1" || results[1] != "d2" {
		t.Fatalf("results = %v, want [i1 d2]", results)
	}
}

// TestCommandMailboxPushAfterInlineTakeKeepsWait verifies that a newer push
// that lands while an inline take holds the command keeps the wait.
func TestCommandMailboxPushAfterInlineTakeKeepsWait(t *testing.T) {
	mb := newCommandMailbox()
	mb.putDesired(fairnessDesired("d1"))
	for i := 0; i < 14; i++ {
		mb.putImperative(fairnessImp(fmt.Sprintf("i%d", i)))
	}
	if got := takeAll(mb, 5, nil); len(got) != 5 {
		t.Fatalf("first takes = %v, want five imperatives", got)
	}
	if _, ok := mb.takeDesired(); !ok {
		t.Fatal("inline takeDesired found no command")
	}
	mb.putDesired(fairnessDesired("d2"))

	got := takeAll(mb, 4, nil)
	if len(got) != 4 || got[3] != "d2" {
		t.Fatalf("takes after the push = %v, want three imperatives and then d2", got)
	}
}

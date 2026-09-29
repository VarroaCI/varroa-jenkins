package main

import (
	"context"
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

// itemsGateJenkins is a fake Jenkins for the items gate tests. It answers
// /login with 200 and itemsGateSession, and counts every trigger
// call. A trigger call answers 404 until itemsReady is set, so a call
// that runs before the items apply fails as it does on a real cell.
// itemsGateSession is the boot session the fake Jenkins answers.
const itemsGateSession = "boot-1"

type itemsGateJenkins struct {
	srv          *httptest.Server
	triggerCalls atomic.Int32
	itemsReady   atomic.Bool
	// loginDelay delays every /login answer, in nanoseconds.
	loginDelay atomic.Int64
}

func newItemsGateJenkins(t *testing.T) *itemsGateJenkins {
	t.Helper()
	f := &itemsGateJenkins{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			time.Sleep(time.Duration(f.loginDelay.Load()))
			w.Header().Set("X-Jenkins-Session", itemsGateSession)
			w.WriteHeader(http.StatusOK)
		case "/varroa-run/trigger":
			f.triggerCalls.Add(1)
			if !f.itemsReady.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// itemsGateAgent builds an agent against the fake. Every result the agent
// sends is recorded in order. A desired-state result marks the fake's items
// as ready, because the desired-state apply is what creates the jobs.
func itemsGateAgent(t *testing.T, f *itemsGateJenkins) (*Agent, func() []*mitev1.CommandResult) {
	t.Helper()
	var mu sync.Mutex
	var results []*mitev1.CommandResult
	stream := &concurrentSendStream{
		sendFn: func(m *mitev1.MiteMessage) error {
			if cr := m.GetCommandResult(); cr != nil {
				if cr.GetDesiredState() != nil {
					f.itemsReady.Store(true)
				}
				mu.Lock()
				results = append(results, cr)
				mu.Unlock()
			}
			return nil
		},
		recvFn: func() (*mitev1.OperatorMessage, error) { return nil, context.Canceled },
	}
	agent := NewAgent(Config{ControllerName: "test", Namespace: "ns", JenkinsURL: f.srv.URL})
	agent.Logger = slog.Default()
	agent.stream = stream
	agent.miteDir = t.TempDir()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()
	return agent, func() []*mitev1.CommandResult {
		mu.Lock()
		defer mu.Unlock()
		return append([]*mitev1.CommandResult(nil), results...)
	}
}

// TestTriggerBuildWaitsForItemsApply verifies a trigger on a Jenkins with
// no items apply makes no call, runs the desired state that arrives in its
// mailbox inline, and then makes exactly one trigger call.
func TestTriggerBuildWaitsForItemsApply(t *testing.T) {
	f := newItemsGateJenkins(t)
	agent, results := itemsGateAgent(t, f)
	mb := newCommandMailbox()

	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runCommand(context.Background(), commandWork{
			imperativeCmd: triggerCommand(),
			ctx:           context.Background(),
			mailbox:       mb,
		})
	}()

	time.Sleep(150 * time.Millisecond)
	if got := f.triggerCalls.Load(); got != 0 {
		t.Fatalf("trigger calls = %d before the items apply, want 0", got)
	}
	mb.putDesired(commandWork{
		desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: "desired-1"},
		ctx:             context.Background(),
	})

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the trigger did not finish after the desired state arrived")
	}
	if got := f.triggerCalls.Load(); got != 1 {
		t.Fatalf("trigger calls = %d, want 1", got)
	}
	got := results()
	if len(got) != 2 || got[0].GetDesiredState() == nil {
		t.Fatalf("results = %+v, want the desired-state result and then the trigger result", got)
	}
	if !got[1].Success {
		t.Fatalf("trigger result = %+v, want success", got[1])
	}
	if !agent.itemsApplied() {
		t.Fatal("the items gate is closed after the apply")
	}
}

// TestTriggerBuildItemsWaitExpires verifies a trigger whose items wait
// expires makes no call and answers the status 0 with an error that names
// the items.
func TestTriggerBuildItemsWaitExpires(t *testing.T) {
	f := newItemsGateJenkins(t)
	agent, results := itemsGateAgent(t, f)
	agent.triggerReadyWait = 100 * time.Millisecond

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: triggerCommand(),
		ctx:           context.Background(),
		mailbox:       newCommandMailbox(),
	})

	if got := f.triggerCalls.Load(); got != 0 {
		t.Fatalf("trigger calls = %d, want 0", got)
	}
	got := results()
	if len(got) != 1 {
		t.Fatalf("results = %+v, want one", got)
	}
	if got[0].Success || got[0].HttpStatus != 0 {
		t.Fatalf("result = %+v, want a failure with the status 0", got[0])
	}
	if !strings.Contains(got[0].Error, "items not applied") {
		t.Fatalf("error = %q, want it to name the items", got[0].Error)
	}
}

// TestTriggerBuildMissingJobAfterApply verifies a job that is missing after
// the items apply still answers the plugin's 404, which the gateway maps
// onto JOB_NOT_FOUND.
func TestTriggerBuildMissingJobAfterApply(t *testing.T) {
	f := newItemsGateJenkins(t)
	agent, results := itemsGateAgent(t, f)
	agent.markItemsApplied()

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: triggerCommand(),
		ctx:           context.Background(),
	})

	if got := f.triggerCalls.Load(); got != 1 {
		t.Fatalf("trigger calls = %d, want 1", got)
	}
	got := results()
	if len(got) != 1 || got[0].Success || got[0].HttpStatus != http.StatusNotFound {
		t.Fatalf("results = %+v, want one failure with the status 404", got)
	}
}

// TestTriggerBuildGateOpenFromMarker verifies a convergence marker from the
// live Jenkins boot opens the items gate, and a marker from another boot
// does not.
func TestTriggerBuildGateOpenFromMarker(t *testing.T) {
	tests := []struct {
		name          string
		markerSession string
		wantCalls     int32
	}{
		{name: "marker from this boot", markerSession: "boot-1", wantCalls: 1},
		{name: "marker from another boot", markerSession: "boot-0", wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newItemsGateJenkins(t)
			f.itemsReady.Store(true)
			agent, results := itemsGateAgent(t, f)
			agent.triggerReadyWait = 200 * time.Millisecond
			agent.writeMarker(appliedMarker{composite: "hash-1", session: tt.markerSession})

			agent.runCommand(context.Background(), commandWork{
				imperativeCmd: triggerCommand(),
				ctx:           context.Background(),
			})

			if got := f.triggerCalls.Load(); got != tt.wantCalls {
				t.Fatalf("trigger calls = %d, want %d", got, tt.wantCalls)
			}
			got := results()
			if len(got) != 1 || got[0].Success != (tt.wantCalls == 1) {
				t.Fatalf("results = %+v, want one result with success %v", got, tt.wantCalls == 1)
			}
		})
	}
}

// TestFailedItemsApplyOpensGate verifies an items apply that fails still
// opens the items gate, so one broken item does not hold back the triggers
// of every other tenant on the cell.
func TestFailedItemsApplyOpensGate(t *testing.T) {
	f := newItemsGateJenkins(t)
	agent, _ := itemsGateAgent(t, f)

	result := agent.handleDesiredState(context.Background(), &mitev1.DesiredStateCommand{
		CommandId: "desired-bad-items",
		ItemsYaml: "items: [unterminated",
	})

	if result.ItemsSuccess {
		t.Fatal("items apply succeeded, want a failure")
	}
	if !agent.itemsApplied() {
		t.Fatal("the items gate is closed after a failed items apply")
	}
}

// TestDesiredStateJenkinsDownKeepsGateClosed verifies an apply that never
// reaches Jenkins leaves the items gate closed.
func TestDesiredStateJenkinsDownKeepsGateClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	agent := NewAgent(Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
	agent.Logger = slog.Default()
	agent.miteDir = t.TempDir()
	agent.jenkinsTokenMu.Lock()
	agent.jenkinsToken = "test-token"
	agent.jenkinsTokenMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	agent.handleDesiredState(ctx, &mitev1.DesiredStateCommand{CommandId: "desired-down"})

	if agent.itemsApplied() {
		t.Fatal("the items gate is open after an apply that never reached Jenkins")
	}
}

// TestCommandMailboxDesiredFirstWhileItemsPending verifies take answers the
// desired slot before the imperative queue while the items gate is closed.
// TestCommandMailboxImperativePriority keeps the order for an open gate.
func TestCommandMailboxDesiredFirstWhileItemsPending(t *testing.T) {
	agent := NewAgent(Config{ControllerName: "test", Namespace: "ns"})
	mb := newCommandMailbox()
	mb.desiredFirst = func() bool { return !agent.itemsApplied() }

	mb.putImperative(commandWork{imperativeCmd: &mitev1.ImperativeCommand{CommandId: "i"}})
	mb.putDesired(commandWork{desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: "d"}})

	first, ok := mb.take()
	if !ok || first.desiredStateCmd == nil || first.desiredStateCmd.CommandId != "d" {
		t.Fatalf("first take = %+v, want the desired command", first)
	}
	second, ok := mb.take()
	if !ok || second.imperativeCmd == nil || second.imperativeCmd.CommandId != "i" {
		t.Fatalf("second take = %+v, want the imperative command", second)
	}

	agent.markItemsApplied()
	mb.putDesired(commandWork{desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: "d2"}})
	mb.putImperative(commandWork{imperativeCmd: &mitev1.ImperativeCommand{CommandId: "i2"}})
	third, _ := mb.take()
	if third.imperativeCmd == nil || third.imperativeCmd.CommandId != "i2" {
		t.Fatalf("take with an open gate = %+v, want the imperative command", third)
	}
}

// TestTriggerBuildInlineApplyOutlastsBudget verifies the trigger budget
// bounds a stalled inline apply: the trigger finishes near its budget and
// makes no call.
func TestTriggerBuildInlineApplyOutlastsBudget(t *testing.T) {
	f := newItemsGateJenkins(t)
	agent, results := itemsGateAgent(t, f)
	agent.triggerReadyWait = 300 * time.Millisecond
	mb := newCommandMailbox()

	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runCommand(context.Background(), commandWork{
			imperativeCmd: triggerCommand(),
			ctx:           context.Background(),
			mailbox:       mb,
		})
	}()

	time.Sleep(100 * time.Millisecond)
	f.loginDelay.Store(int64(2 * time.Second))
	start := time.Now()
	mb.putDesired(commandWork{
		desiredStateCmd: &mitev1.DesiredStateCommand{CommandId: "desired-slow"},
		ctx:             context.Background(),
	})

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the trigger did not finish")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("the trigger finished after %s, want the budget to cut the stalled apply", elapsed)
	}
	if got := f.triggerCalls.Load(); got != 0 {
		t.Fatalf("trigger calls = %d, want 0 after the budget expired", got)
	}
	got := results()
	if len(got) != 2 || got[1].Success || got[1].HttpStatus != 0 ||
		!strings.Contains(got[1].Error, "items not applied") {
		t.Fatalf("results = %+v, want the apply result and then an items failure with the status 0", got)
	}
}

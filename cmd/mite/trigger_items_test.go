package main

import (
	"context"
	"encoding/json"
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

	// In tenant mode, a trigger call answers 404 unless the job's tenant
	// is in readyTenants. A desired-state result replaces readyTenants
	// with nextTenants, because that apply creates the tenant folders.
	tenantMu     sync.Mutex
	tenantMode   bool
	nextTenants  []string
	readyTenants map[string]bool
}

// useTenants turns on tenant mode with ready as the tenants that hold a
// folder now and next as the tenants of the next applied desired state.
func (f *itemsGateJenkins) useTenants(ready, next []string) {
	f.tenantMu.Lock()
	defer f.tenantMu.Unlock()
	f.tenantMode = true
	f.readyTenants = map[string]bool{}
	for _, t := range ready {
		f.readyTenants[t] = true
	}
	f.nextTenants = next
}

// applyTenants replaces readyTenants with nextTenants.
func (f *itemsGateJenkins) applyTenants() {
	f.tenantMu.Lock()
	defer f.tenantMu.Unlock()
	f.readyTenants = map[string]bool{}
	for _, t := range f.nextTenants {
		f.readyTenants[t] = true
	}
}

// jobReady reports whether the fake holds the job of a trigger call.
func (f *itemsGateJenkins) jobReady(r *http.Request) bool {
	f.tenantMu.Lock()
	defer f.tenantMu.Unlock()
	if !f.tenantMode {
		return f.itemsReady.Load()
	}
	var body struct {
		JobPath string `json:"jobPath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return false
	}
	return f.readyTenants[jobTenant(body.JobPath)]
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
			if !f.jobReady(r) {
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
					f.applyTenants()
				}
				mu.Lock()
				results = append(results, cr)
				mu.Unlock()
			}
			return nil
		},
		recvFn: func() (*mitev1.OperatorMessage, error) { return nil, context.Canceled },
	}
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: f.srv.URL})
	agent.Logger = slog.Default()
	setTestStream(agent, stream)
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
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns", JenkinsURL: srv.URL})
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
	agent := mustNewAgent(t, Config{ControllerName: "test", Namespace: "ns"})
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

// tenantTrigger answers a TRIGGER_BUILD command for jobPath.
func tenantTrigger(jobPath string) *mitev1.ImperativeCommand {
	return &mitev1.ImperativeCommand{
		CommandId:    "trigger-tenant-" + jobPath,
		Type:         mitev1.CommandTypeTriggerBuild,
		TriggerBuild: &mitev1.TriggerBuildPayload{RunId: "run-t", JobPath: jobPath},
	}
}

// cellDesired answers a cell's desired state that holds tenants.
func cellDesired(id string, tenants ...string) *mitev1.DesiredStateCommand {
	cmd := &mitev1.DesiredStateCommand{CommandId: id, Cell: true}
	for _, t := range tenants {
		cmd.TenantHomes = append(cmd.TenantHomes, &mitev1.TenantHomes{Tenant: t, Role: "active"})
	}
	return cmd
}

// runTriggerAsync runs a trigger for jobPath on the worker's behalf and
// answers a channel that closes when the trigger finishes.
func runTriggerAsync(agent *Agent, mb *commandMailbox, jobPath string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.runCommand(context.Background(), commandWork{
			imperativeCmd: tenantTrigger(jobPath),
			ctx:           context.Background(),
			mailbox:       mb,
		})
	}()
	return done
}

// TestTriggerWaitsForNewTenantOnRunningCell verifies a trigger for a
// tenant that the cell does not hold yet makes no call, runs the queued
// desired state that adds the tenant inline, and then makes one call.
func TestTriggerWaitsForNewTenantOnRunningCell(t *testing.T) {
	f := newItemsGateJenkins(t)
	f.useTenants([]string{"a"}, []string{"a", "b"})
	agent, results := itemsGateAgent(t, f)
	agent.setAppliedTenants(true, []string{"a"})
	agent.markItemsApplied()
	mb := newCommandMailbox()

	done := runTriggerAsync(agent, mb, "b/build")
	time.Sleep(150 * time.Millisecond)
	if got := f.triggerCalls.Load(); got != 0 {
		t.Fatalf("trigger calls = %d before the tenant apply, want 0", got)
	}
	mb.putDesired(commandWork{desiredStateCmd: cellDesired("desired-ab", "a", "b"), ctx: context.Background()})

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
	if !got[1].Success || got[1].HttpStatus != http.StatusAccepted {
		t.Fatalf("trigger result = %+v, want success with the status 202", got[1])
	}
}

// TestTriggerFirstTenantOnEmptyCell verifies a trigger on a cell with no
// tenant waits for the desired state that places the first tenant.
func TestTriggerFirstTenantOnEmptyCell(t *testing.T) {
	f := newItemsGateJenkins(t)
	f.useTenants(nil, []string{"a"})
	agent, results := itemsGateAgent(t, f)
	agent.setAppliedTenants(true, nil)
	agent.markItemsApplied()
	mb := newCommandMailbox()

	done := runTriggerAsync(agent, mb, "a/build")
	time.Sleep(150 * time.Millisecond)
	if got := f.triggerCalls.Load(); got != 0 {
		t.Fatalf("trigger calls = %d before the tenant apply, want 0", got)
	}
	mb.putDesired(commandWork{desiredStateCmd: cellDesired("desired-a", "a"), ctx: context.Background()})

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the trigger did not finish after the desired state arrived")
	}
	got := results()
	if f.triggerCalls.Load() != 1 || len(got) != 2 || !got[1].Success {
		t.Fatalf("calls = %d, results = %+v, want one call that succeeds", f.triggerCalls.Load(), got)
	}
}

// TestTriggerNonCellSkipsTenantGate verifies a controller that is not a
// cell makes the call at once for any job path.
func TestTriggerNonCellSkipsTenantGate(t *testing.T) {
	f := newItemsGateJenkins(t)
	f.itemsReady.Store(true)
	agent, results := itemsGateAgent(t, f)
	agent.triggerReadyWait = 100 * time.Millisecond
	agent.setAppliedTenants(false, nil)
	agent.markItemsApplied()

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: tenantTrigger("unknown/build"),
		ctx:           context.Background(),
		mailbox:       newCommandMailbox(),
	})

	got := results()
	if f.triggerCalls.Load() != 1 || len(got) != 1 || !got[0].Success {
		t.Fatalf("calls = %d, results = %+v, want one call that succeeds", f.triggerCalls.Load(), got)
	}
}

// TestTriggerTenantWaitExpires verifies a trigger whose tenant never
// arrives makes no call and answers the status 0 with an error that
// names the tenant and the items.
func TestTriggerTenantWaitExpires(t *testing.T) {
	f := newItemsGateJenkins(t)
	f.useTenants([]string{"a"}, []string{"a"})
	agent, results := itemsGateAgent(t, f)
	agent.triggerReadyWait = 100 * time.Millisecond
	agent.setAppliedTenants(true, []string{"a"})
	agent.markItemsApplied()

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: tenantTrigger("b/build"),
		ctx:           context.Background(),
		mailbox:       newCommandMailbox(),
	})

	if got := f.triggerCalls.Load(); got != 0 {
		t.Fatalf("trigger calls = %d, want 0", got)
	}
	got := results()
	if len(got) != 1 || got[0].Success || got[0].HttpStatus != 0 {
		t.Fatalf("results = %+v, want one failure with the status 0", got)
	}
	if !strings.Contains(got[0].Error, "tenant b items not applied") {
		t.Fatalf("error = %q, want it to name the tenant and the items", got[0].Error)
	}
}

// TestTriggerTenantGateHonorsIdleDeferral verifies that, with the items
// gate open, a waiting trigger does not apply an apply_when idle desired
// state inline while builds run. The desired state stays in the mailbox.
func TestTriggerTenantGateHonorsIdleDeferral(t *testing.T) {
	f := newItemsGateJenkins(t)
	f.useTenants([]string{"a"}, []string{"a", "b"})
	agent, results := itemsGateAgent(t, f)
	agent.triggerReadyWait = 300 * time.Millisecond
	agent.setAppliedTenants(true, []string{"a"})
	agent.markItemsApplied()
	mb := newCommandMailbox()
	idle := cellDesired("desired-idle", "a", "b")
	idle.ApplyWhen = "idle"
	mb.putDesired(commandWork{desiredStateCmd: idle, ctx: context.Background()})

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: tenantTrigger("b/build"),
		ctx:           context.Background(),
		mailbox:       mb,
	})

	if got := f.triggerCalls.Load(); got != 0 {
		t.Fatalf("trigger calls = %d, want 0", got)
	}
	got := results()
	if len(got) != 1 || got[0].GetDesiredState() != nil || got[0].Success ||
		!strings.Contains(got[0].Error, "items not applied") {
		t.Fatalf("results = %+v, want one trigger failure and no apply result", got)
	}
	left, ok := mb.takeDesired()
	if !ok || left.desiredStateCmd.CommandId != "desired-idle" {
		t.Fatalf("mailbox desired = %+v, %v, want the deferred desired state", left, ok)
	}
}

// TestTenantGateRestoredFromMarker verifies a marker from the live boot
// restores the tenant set, so a trigger for a held tenant makes its call
// without a push, and a tenant absent from the marker stays closed.
func TestTenantGateRestoredFromMarker(t *testing.T) {
	f := newItemsGateJenkins(t)
	f.useTenants([]string{"a"}, []string{"a"})
	agent, results := itemsGateAgent(t, f)
	agent.triggerReadyWait = 200 * time.Millisecond
	agent.writeMarker(appliedMarker{
		composite: "hash-1", session: itemsGateSession, cell: true, tenants: []string{"a"},
	})

	agent.runCommand(context.Background(), commandWork{
		imperativeCmd: tenantTrigger("a/x"),
		ctx:           context.Background(),
	})

	got := results()
	if f.triggerCalls.Load() != 1 || len(got) != 1 || !got[0].Success {
		t.Fatalf("calls = %d, results = %+v, want one call that succeeds", f.triggerCalls.Load(), got)
	}
	if agent.tenantItemsReady("b/x") {
		t.Fatal("tenant b is ready, want only the marker's tenants")
	}
}

// TestTenantSetReplacedNotMerged verifies each apply replaces the tenant
// set, so a tenant that left the cell leaves the set.
func TestTenantSetReplacedNotMerged(t *testing.T) {
	f := newItemsGateJenkins(t)
	agent, _ := itemsGateAgent(t, f)

	agent.handleDesiredState(context.Background(), cellDesired("desired-ab", "a", "b"))
	if !agent.tenantItemsReady("a/x") || !agent.tenantItemsReady("b/x") {
		t.Fatal("tenants a and b are not ready after the first apply")
	}
	agent.handleDesiredState(context.Background(), cellDesired("desired-b", "b"))
	if agent.tenantItemsReady("a/x") {
		t.Fatal("tenant a is ready after an apply without it")
	}
	if !agent.tenantItemsReady("b/x") {
		t.Fatal("tenant b is not ready after an apply with it")
	}
}

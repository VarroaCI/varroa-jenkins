package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// fakeCommitStatusWriter records the requests that the subscriber maps. The
// subscriber runs each call in its own goroutine, so the record is locked.
type fakeCommitStatusWriter struct {
	mu       sync.Mutex
	reqs     []forgeapi.CommitStatusRequest
	restores []forgeapi.NewestStatusRequest
	err      error
}

func (f *fakeCommitStatusWriter) EndPendingStatus(_ context.Context, req forgeapi.CommitStatusRequest) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return 1, f.err
}

func (f *fakeCommitStatusWriter) RestoreNewestStatus(_ context.Context, req forgeapi.NewestStatusRequest) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, req)
	return 1, f.err
}

// restoreCalls returns a copy of the recorded restore requests.
func (f *fakeCommitStatusWriter) restoreCalls() []forgeapi.NewestStatusRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgeapi.NewestStatusRequest(nil), f.restores...)
}

// calls returns a copy of the recorded requests.
func (f *fakeCommitStatusWriter) calls() []forgeapi.CommitStatusRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgeapi.CommitStatusRequest(nil), f.reqs...)
}

// waitCalls waits until the ender records a request, or fails the test.
func waitCalls(t *testing.T, f *fakeCommitStatusWriter) []forgeapi.CommitStatusRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.calls(); len(got) > 0 {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("forge calls = 0, want 1")
	return nil
}

func marshalLedgerAction(t *testing.T, a recordstore.LedgerAction) []byte {
	t.Helper()
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// endDoors is the door prefix of the cluster in the end tests, and endCellURL
// is the Jenkins location URL of the cell.
const (
	endDoors   = "https://varroa.example.org/headless/core/"
	endCellURL = endDoors + "cells-ns/cell-a/"
)

// endRunner returns a runner that names the location URL of the cell of
// endCommitStatusAction and the door prefix of the cluster.
func endRunner() *fakeLedgerActionRunner {
	return &fakeLedgerActionRunner{locationURLs: map[string]string{"cells-ns/cell-a": endCellURL}, doors: endDoors}
}

func endCommitStatusAction(t *testing.T) []byte {
	t.Helper()
	bn := int64(7)
	return marshalLedgerAction(t, recordstore.LedgerAction{
		Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a", Actor: recordstore.LedgerActionRecorder,
		RunID: "run-1", JobPath: "payments/app/main", BuildNumber: &bn, Tenant: "payments",
		Repo: "https://git.example.org/team/app", Revision: "0123456789abcdef0123456789abcdef01234567",
		Reason: "cell-lost",
	})
}

// TestHandleLedgerAction_EndCommitStatusCallsForge checks the field mapping
// from the action to the forge request, with the location URL of the
// action's cell and the door prefix of the cluster.
func TestHandleLedgerAction_EndCommitStatusCallsForge(t *testing.T) {
	ender := &fakeCommitStatusWriter{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", endRunner(), ender, endCommitStatusAction(t))
	reqs := waitCalls(t, ender)
	want := forgeapi.CommitStatusRequest{
		Tenant:      "payments",
		Repo:        "https://git.example.org/team/app",
		SHA:         "0123456789abcdef0123456789abcdef01234567",
		JobPath:     "payments/app/main",
		BuildNumber: 7,
		Reason:      "cell-lost",
		JenkinsURL:  endCellURL,
		CellDoors:   endDoors,
	}
	if reqs[0] != want {
		t.Fatalf("request = %+v, want %+v", reqs[0], want)
	}
}

// TestHandleLedgerAction_EndCommitStatusAfterAMoveNamesTheCellDoors checks a
// run that a resume moved from cell-a to cell-b. The action names cell-b, and
// the request carries the door prefix. The forge answerer then also matches
// the pending status that cell-a wrote.
func TestHandleLedgerAction_EndCommitStatusAfterAMoveNamesTheCellDoors(t *testing.T) {
	bn := int64(7)
	action := marshalLedgerAction(t, recordstore.LedgerAction{
		Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-b", Actor: recordstore.LedgerActionRecorder,
		RunID: "run-1", JobPath: "payments/app/main", BuildNumber: &bn, Tenant: "payments",
		Repo: "https://git.example.org/acme/app", Revision: "0123456789abcdef0123456789abcdef01234567",
		Reason: "resume-failed",
	})
	rec := &fakeLedgerActionRunner{
		locationURLs: map[string]string{"cells-ns/cell-b": endDoors + "cells-ns/cell-b/"},
		doors:        endDoors,
	}
	ender := &fakeCommitStatusWriter{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", rec, ender, action)
	reqs := waitCalls(t, ender)
	if reqs[0].JenkinsURL != endDoors+"cells-ns/cell-b/" || reqs[0].CellDoors != endDoors || reqs[0].Reason != "resume-failed" {
		t.Fatalf("request = %+v, want the door of cell-b, the door prefix, and the reason", reqs[0])
	}
}

// TestHandleLedgerAction_EndCommitStatusOfADeletedCellUsesItsDoor checks a
// cell whose Controller object is gone. The end write takes the door that
// EndStatusLocation rebuilds. The restore write reads JenkinsLocationURL
// alone, so it writes nothing for that cell.
func TestHandleLedgerAction_EndCommitStatusOfADeletedCellUsesItsDoor(t *testing.T) {
	setRestoreSettleDelay(t, 10*time.Millisecond)
	const goneDoor = endDoors + "cells-ns/cell-a/"
	rec := &fakeLedgerActionRunner{
		endLocations: map[string]string{"cells-ns/cell-a": goneDoor},
		doors:        endDoors,
	}
	writer := &fakeCommitStatusWriter{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", rec, writer, endCommitStatusAction(t))
	reqs := waitCalls(t, writer)
	if reqs[0].JenkinsURL != goneDoor {
		t.Fatalf("end request JenkinsURL = %q, want %q", reqs[0].JenkinsURL, goneDoor)
	}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", rec, writer, marshalLedgerAction(t, restoreAction()))
	time.Sleep(100 * time.Millisecond)
	if n := len(writer.restoreCalls()); n != 0 {
		t.Fatalf("restore calls = %d, want 0", n)
	}
}

// TestHandleLedgerAction_EndCommitStatusSkipsACellWithNoLocationURL checks
// that a cell whose location URL the operator cannot name, because it is
// gone, cannot be read, or is not headless, writes no status.
func TestHandleLedgerAction_EndCommitStatusSkipsACellWithNoLocationURL(t *testing.T) {
	ender := &fakeCommitStatusWriter{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", &fakeLedgerActionRunner{}, ender, endCommitStatusAction(t))
	time.Sleep(100 * time.Millisecond)
	if n := len(ender.calls()); n != 0 {
		t.Fatalf("forge calls = %d, want 0", n)
	}
}

// TestHandleLedgerAction_EndCommitStatusMissingFieldsSkips checks that an
// action without a repository, a commit, a tenant, a build number, or a
// cell never reaches the forge, also when the cell has a location URL.
func TestHandleLedgerAction_EndCommitStatusMissingFieldsSkips(t *testing.T) {
	bn := int64(7)
	complete := func() recordstore.LedgerAction {
		return recordstore.LedgerAction{
			Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core",
			Namespace: "cells-ns", Controller: "cell-a", Tenant: "payments",
			Repo: "https://git.example.org/team/app", Revision: "abc", BuildNumber: &bn,
			Reason: "cell-lost",
		}
	}
	cases := map[string]func(*recordstore.LedgerAction){
		"no repo":         func(a *recordstore.LedgerAction) { a.Repo = "" },
		"no revision":     func(a *recordstore.LedgerAction) { a.Revision = "" },
		"no tenant":       func(a *recordstore.LedgerAction) { a.Tenant = "" },
		"no build number": func(a *recordstore.LedgerAction) { a.BuildNumber = nil },
		"no namespace":    func(a *recordstore.LedgerAction) { a.Namespace = "" },
		"no controller":   func(a *recordstore.LedgerAction) { a.Controller = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			action := complete()
			mutate(&action)
			ender := &fakeCommitStatusWriter{}
			handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
				"varroa.ledger.actions.core", "core", endRunner(), ender, marshalLedgerAction(t, action))
			time.Sleep(50 * time.Millisecond)
			if n := len(ender.calls()); n != 0 {
				t.Fatalf("forge calls = %d, want 0", n)
			}
		})
	}
}

// TestHandleLedgerAction_EndCommitStatusNilForge checks that an operator
// without a forge answerer logs and takes no action.
func TestHandleLedgerAction_EndCommitStatusNilForge(t *testing.T) {
	rec := &fakeLedgerActionRunner{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", rec, nil, endCommitStatusAction(t))
	if len(rec.sent) != 0 || rec.wokeCalls != 0 {
		t.Fatalf("sent %d, woke %d; want no effect", len(rec.sent), rec.wokeCalls)
	}
}

// TestHandleLedgerAction_EndCommitStatusForgeErrorIsLogged checks that a
// forge error does not panic and does not reach the imperative path.
func TestHandleLedgerAction_EndCommitStatusForgeErrorIsLogged(t *testing.T) {
	ender := &fakeCommitStatusWriter{err: errors.New("forge_unreachable")}
	rec := endRunner()
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", rec, ender, endCommitStatusAction(t))
	waitCalls(t, ender)
	if len(rec.sent) != 0 {
		t.Fatalf("sent %d imperative commands, want 0", len(rec.sent))
	}
}

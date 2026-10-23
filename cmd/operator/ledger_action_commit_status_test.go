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

// fakeCommitStatusEnder records the request that the subscriber maps. The
// subscriber runs the call in its own goroutine, so the record is locked.
type fakeCommitStatusEnder struct {
	mu   sync.Mutex
	reqs []forgeapi.CommitStatusRequest
	err  error
}

func (f *fakeCommitStatusEnder) EndPendingStatus(_ context.Context, req forgeapi.CommitStatusRequest) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return 1, f.err
}

// calls returns a copy of the recorded requests.
func (f *fakeCommitStatusEnder) calls() []forgeapi.CommitStatusRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgeapi.CommitStatusRequest(nil), f.reqs...)
}

// waitCalls waits until the ender records n requests, or fails the test.
func waitCalls(t *testing.T, f *fakeCommitStatusEnder, n int) []forgeapi.CommitStatusRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.calls(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("forge calls = %d, want %d", len(f.calls()), n)
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
// from the action to the forge request.
func TestHandleLedgerAction_EndCommitStatusCallsForge(t *testing.T) {
	ender := &fakeCommitStatusEnder{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", &fakeLedgerActionRunner{}, ender, endCommitStatusAction(t))
	reqs := waitCalls(t, ender, 1)
	want := forgeapi.CommitStatusRequest{
		Tenant:      "payments",
		Repo:        "https://git.example.org/team/app",
		SHA:         "0123456789abcdef0123456789abcdef01234567",
		JobPath:     "payments/app/main",
		BuildNumber: 7,
		Reason:      "cell-lost",
	}
	if reqs[0] != want {
		t.Fatalf("request = %+v, want %+v", reqs[0], want)
	}
}

// TestHandleLedgerAction_EndCommitStatusMissingFieldsSkips checks that an
// action without a repository, a commit, or a tenant never reaches the forge.
func TestHandleLedgerAction_EndCommitStatusMissingFieldsSkips(t *testing.T) {
	bn := int64(7)
	cases := map[string]recordstore.LedgerAction{
		"no repo": {Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core", Tenant: "payments",
			Revision: "abc", BuildNumber: &bn},
		"no revision": {Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core", Tenant: "payments",
			Repo: "https://git.example.org/team/app", BuildNumber: &bn},
		"no tenant": {Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core",
			Repo: "https://git.example.org/team/app", Revision: "abc", BuildNumber: &bn},
		"no build number": {Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core", Tenant: "payments",
			Repo: "https://git.example.org/team/app", Revision: "abc"},
	}
	for name, action := range cases {
		t.Run(name, func(t *testing.T) {
			ender := &fakeCommitStatusEnder{}
			handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
				"varroa.ledger.actions.core", "core", &fakeLedgerActionRunner{}, ender, marshalLedgerAction(t, action))
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
	ender := &fakeCommitStatusEnder{err: errors.New("forge_unreachable")}
	rec := &fakeLedgerActionRunner{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", rec, ender, endCommitStatusAction(t))
	waitCalls(t, ender, 1)
	if len(rec.sent) != 0 {
		t.Fatalf("sent %d imperative commands, want 0", len(rec.sent))
	}
}

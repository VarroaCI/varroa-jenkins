package main

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestHandleLedgerAction_EndCommitStatusPassesResult checks that a Result
// action reaches the forge with its result and without a reason.
func TestHandleLedgerAction_EndCommitStatusPassesResult(t *testing.T) {
	bn := int64(7)
	data := marshalLedgerAction(t, recordstore.LedgerAction{
		Kind: recordstore.LedgerActionEndCommitStatus, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a", Actor: recordstore.LedgerActionRecorder,
		RunID: "run-1", JobPath: "payments/app/main", BuildNumber: &bn, Tenant: "payments",
		Repo: "https://git.example.org/acme/app", Revision: "0123456789abcdef0123456789abcdef01234567",
		Result: "FAILURE",
	})
	ender := &fakeCommitStatusEnder{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", &fakeLedgerActionRunner{}, ender, data)
	reqs := waitCalls(t, ender, 1)
	want := forgeapi.CommitStatusRequest{
		Tenant:      "payments",
		Repo:        "https://git.example.org/acme/app",
		SHA:         "0123456789abcdef0123456789abcdef01234567",
		JobPath:     "payments/app/main",
		BuildNumber: 7,
		Result:      "FAILURE",
	}
	if reqs[0] != want {
		t.Fatalf("request = %+v, want %+v", reqs[0], want)
	}
}

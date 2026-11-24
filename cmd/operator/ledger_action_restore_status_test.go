package main

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// restoreCellURL is the Jenkins location URL of the cell in the restore tests.
const restoreCellURL = "https://varroa.example.org/headless/core/cells-ns/cell-a/"

// setRestoreSettleDelay sets restoreSettleDelay for one test and puts the
// old value back at cleanup. A test that calls it must not run in parallel.
func setRestoreSettleDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := restoreSettleDelay
	restoreSettleDelay = d
	t.Cleanup(func() { restoreSettleDelay = old })
}

// waitRestoreCalls waits until the writer records n restore requests, or
// fails the test.
func waitRestoreCalls(t *testing.T, f *fakeCommitStatusWriter, n int) []forgeapi.NewestStatusRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.restoreCalls(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("restore calls = %d, want %d", len(f.restoreCalls()), n)
	return nil
}

// restoreAction returns a complete restore_commit_status action for build 8
// over build 7.
func restoreAction() recordstore.LedgerAction {
	bn := int64(8)
	return recordstore.LedgerAction{
		Kind: recordstore.LedgerActionRestoreCommitStatus, Cluster: "core",
		Namespace: "cells-ns", Controller: "cell-a", Actor: recordstore.LedgerActionRecorder,
		RunID: "run-8", JobPath: "payments/app/main", BuildNumber: &bn, Tenant: "payments",
		Repo: "https://git.example.org/acme/app", Revision: "0123456789abcdef0123456789abcdef01234567",
		Result: "SUCCESS", SupersededBuilds: []int64{7},
	}
}

// restoreRunner returns a runner that names the location URL of the cell of
// restoreAction.
func restoreRunner() *fakeLedgerActionRunner {
	return &fakeLedgerActionRunner{locationURLs: map[string]string{"cells-ns/cell-a": restoreCellURL}}
}

// syncBuffer is a log sink that the timer goroutine and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestHandleLedgerAction_RestoreCommitStatusCallsForge checks the field
// mapping from the action to the forge request, with the location URL of
// the action's cell.
func TestHandleLedgerAction_RestoreCommitStatusCallsForge(t *testing.T) {
	setRestoreSettleDelay(t, 0)
	writer := &fakeCommitStatusWriter{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", restoreRunner(), writer, marshalLedgerAction(t, restoreAction()))
	reqs := waitRestoreCalls(t, writer, 1)
	want := forgeapi.NewestStatusRequest{
		Tenant:           "payments",
		Repo:             "https://git.example.org/acme/app",
		SHA:              "0123456789abcdef0123456789abcdef01234567",
		JobPath:          "payments/app/main",
		BuildNumber:      8,
		Result:           "SUCCESS",
		SupersededBuilds: []int64{7},
		JenkinsURL:       restoreCellURL,
	}
	if !reflect.DeepEqual(reqs[0], want) {
		t.Fatalf("request = %+v, want %+v", reqs[0], want)
	}
	if n := len(writer.calls()); n != 0 {
		t.Fatalf("end calls = %d, want 0", n)
	}
}

// TestHandleLedgerAction_RestoreCommitStatusSkipsACellWithNoLocationURL
// checks that a cell whose location URL the operator cannot name, because it
// is gone, moved, or not headless, writes no status.
func TestHandleLedgerAction_RestoreCommitStatusSkipsACellWithNoLocationURL(t *testing.T) {
	setRestoreSettleDelay(t, 0)
	writer := &fakeCommitStatusWriter{}
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", &fakeLedgerActionRunner{}, writer, marshalLedgerAction(t, restoreAction()))
	time.Sleep(100 * time.Millisecond)
	if n := len(writer.restoreCalls()); n != 0 {
		t.Fatalf("restore calls = %d, want 0", n)
	}
}

// TestHandleLedgerAction_RestoreCommitStatusWaitsForTheSettleDelay checks
// that the forge read starts only after the settle delay, and that the
// handler itself does not wait.
func TestHandleLedgerAction_RestoreCommitStatusWaitsForTheSettleDelay(t *testing.T) {
	setRestoreSettleDelay(t, 300*time.Millisecond)
	writer := &fakeCommitStatusWriter{}
	start := time.Now()
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", restoreRunner(), writer, marshalLedgerAction(t, restoreAction()))
	if elapsed := time.Since(start); elapsed >= 300*time.Millisecond {
		t.Fatalf("handler took %v, want it to return before the delay", elapsed)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(writer.restoreCalls()); n != 0 {
		t.Fatalf("restore calls before the delay = %d, want 0", n)
	}
	waitRestoreCalls(t, writer, 1)
}

// TestHandleLedgerAction_RestoreCommitStatusMissingFieldsSkips checks that an
// action without its facts never reaches the forge.
func TestHandleLedgerAction_RestoreCommitStatusMissingFieldsSkips(t *testing.T) {
	setRestoreSettleDelay(t, 0)
	cases := map[string]func(*recordstore.LedgerAction){
		"no repo":         func(a *recordstore.LedgerAction) { a.Repo = "" },
		"no revision":     func(a *recordstore.LedgerAction) { a.Revision = "" },
		"no tenant":       func(a *recordstore.LedgerAction) { a.Tenant = "" },
		"no namespace":    func(a *recordstore.LedgerAction) { a.Namespace = "" },
		"no controller":   func(a *recordstore.LedgerAction) { a.Controller = "" },
		"no build number": func(a *recordstore.LedgerAction) { a.BuildNumber = nil },
		"no result":       func(a *recordstore.LedgerAction) { a.Result = "" },
		"no superseded":   func(a *recordstore.LedgerAction) { a.SupersededBuilds = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			action := restoreAction()
			mutate(&action)
			writer := &fakeCommitStatusWriter{}
			handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
				"varroa.ledger.actions.core", "core", restoreRunner(), writer, marshalLedgerAction(t, action))
			time.Sleep(50 * time.Millisecond)
			if n := len(writer.restoreCalls()); n != 0 {
				t.Fatalf("restore calls = %d, want 0", n)
			}
		})
	}
}

// TestHandleLedgerAction_RestoreCommitStatusNilForge checks that an operator
// without a forge answerer logs and takes no action.
func TestHandleLedgerAction_RestoreCommitStatusNilForge(t *testing.T) {
	setRestoreSettleDelay(t, 0)
	rec := restoreRunner()
	handleLedgerAction(context.Background(), testLogger(), noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", rec, nil, marshalLedgerAction(t, restoreAction()))
	time.Sleep(50 * time.Millisecond)
	if len(rec.sent) != 0 || rec.wokeCalls != 0 {
		t.Fatalf("sent %d, woke %d; want no effect", len(rec.sent), rec.wokeCalls)
	}
}

// TestHandleLedgerAction_RestoreCommitStatusDropsWhenSlotsFull checks that a
// full set of forge slots drops the restore without a call, and logs the
// drop at Warn with the tenant, the job, and the revision.
func TestHandleLedgerAction_RestoreCommitStatusDropsWhenSlotsFull(t *testing.T) {
	setRestoreSettleDelay(t, 0)
	for i := 0; i < cap(endStatusSlots); i++ {
		endStatusSlots <- struct{}{}
	}
	defer func() {
		for len(endStatusSlots) > 0 {
			<-endStatusSlots
		}
	}()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	writer := &fakeCommitStatusWriter{}
	handleLedgerAction(context.Background(), logger, noopLedgerActionMetrics(t),
		"varroa.ledger.actions.core", "core", restoreRunner(), writer, marshalLedgerAction(t, restoreAction()))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "restore_commit_status dropped") {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(writer.restoreCalls()); n != 0 {
		t.Fatalf("restore calls = %d, want 0 when dropped", n)
	}
	line := logs.String()
	for _, want := range []string{
		`"level":"WARN"`, `"msg":"restore_commit_status dropped, forge writes at capacity"`,
		`"tenant":"payments"`, `"job_path":"payments/app/main"`,
		`"revision":"0123456789abcdef0123456789abcdef01234567"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("drop log %q lacks %s", line, want)
		}
	}
}

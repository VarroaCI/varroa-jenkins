package recordstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// assertDuplicateNotification fails the test unless got is the terminal run
// notification of one run that became DUPLICATE.
func assertDuplicateNotification(t *testing.T, got RunNotification, runID string, wantBuild int64) {
	t.Helper()
	if got.Kind != "run" || got.Status != StatusDuplicate || got.Result != "" {
		t.Fatalf("published %+v, want run/%q with no result", got, StatusDuplicate)
	}
	if got.RunID != runID || got.Cluster != testCluster || got.Tenant != testTenant || got.JobPath != testJobPath {
		t.Fatalf("published %+v, want run %s of the test job", got, runID)
	}
	if got.BuildNumber != wantBuild {
		t.Fatalf("build number = %d, want %d", got.BuildNumber, wantBuild)
	}
	if !isTerminalNotification(got) {
		t.Fatalf("published %+v, want a terminal notification", got)
	}
}

// TestDuplicateClaimPublishesOneDuplicateNotification asserts a claim the
// ledger answers duplicate publishes one terminal DUPLICATE notification
// for the accepted row it marks. A repeat claim marks no row, and a claim
// for a run with no row marks no row, so neither publishes.
func TestDuplicateClaimPublishesOneDuplicateNotification(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ledger := NewLedger(pool, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	ledger.SetRunNotifier(notifier)

	const key = "upstream:dup-notify-1:t0000"
	second := uuid.Must(uuid.NewV7())
	seedAcceptedTriggerRow(t, g, second, key)
	seedGrantedIntent(t, pool, key)

	req := newTestClaimRequest(key, 0)
	req.RunID = second.String()
	for _, step := range []string{"the claim", "a repeat claim"} {
		if resp := ledger.Claim(context.Background(), req); resp.Decision != DecisionDuplicate {
			t.Fatalf("%s: decision = %q, want duplicate", step, resp.Decision)
		}
		if got := sent.count(); got != 1 {
			t.Fatalf("%s: published %d notifications, want 1", step, got)
		}
	}
	assertDuplicateNotification(t, sent.payload(0), second.String(), 0)
	if got := runStatus(t, g, second); got != StatusDuplicate {
		t.Fatalf("status = %q, want DUPLICATE", got)
	}

	rowless := newTestClaimRequest(key, 0)
	if resp := ledger.Claim(context.Background(), rowless); resp.Decision != DecisionDuplicate {
		t.Fatalf("rowless claim: decision = %q, want duplicate", resp.Decision)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("a claim for a run with no row published %d notifications in total, want 1", got)
	}
}

// TestDuplicateStartMarkPublishesOneDuplicateNotification asserts a start
// mark for a run the intent no longer names publishes one terminal
// DUPLICATE notification for that run's row. A mark from a former owner of
// the same run marks no row and publishes nothing.
func TestDuplicateStartMarkPublishesOneDuplicateNotification(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	ledger.SetRunNotifier(notifier)

	req := newTestClaimRequest("manual:mark-dup-notify", 1)
	mustGrant(t, ledger, req)
	seedAcceptedRunRow(t, db, req)
	mustGrant(t, ledger, repeatFrom(newTestClaimRequest(req.IntentKey, 2), "pod-b"))

	old := req
	old.PlacementGeneration = 2
	if resp := ledger.Claim(context.Background(), startMarkOf(old)); resp.Decision != DecisionDuplicate {
		t.Fatalf("want duplicate, got %+v", resp)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications for the moved run, want 1", got)
	}
	assertDuplicateNotification(t, sent.payload(0), req.RunID, 0)

	same := newTestClaimRequest("manual:mark-dup-notify-owner", 1)
	mustGrant(t, ledger, same)
	seedAcceptedRunRow(t, db, same)
	if resp := ledger.Claim(context.Background(), startMarkOf(repeatFrom(same, "pod-b"))); resp.Decision != DecisionDuplicate {
		t.Fatalf("want duplicate from another owner, got %+v", resp)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("a mark from another owner published %d notifications in total, want 1", got)
	}
}

// TestDuplicateBackstopPublishesDuplicateForTheArrivingRun asserts a
// run.started whose duplicate backstop marks the arriving run publishes
// that run's notification as DUPLICATE, never RUNNING.
func TestDuplicateBackstopPublishesDuplicateForTheArrivingRun(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)
	ctx := context.Background()

	revision := "rev-dup-notify-arriving"
	runA := seedGrantedIntent(t, pool, "api:dup-notify-arriving-a")
	runB := seedGrantedIntent(t, pool, "api:dup-notify-arriving-b")
	buildA, buildB := int64(1), int64(2)
	if _, err := g.Apply(ctx, newTestEnvelope(t, runA, "api:dup-notify-arriving-a", KindRunStarted,
		RunStartedPayload{BuildNumber: &buildA, SCMRevision: &revision})); err != nil {
		t.Fatalf("apply run.started A: %v", err)
	}
	if _, err := g.Apply(ctx, newTestEnvelope(t, runB, "api:dup-notify-arriving-b", KindRunStarted,
		RunStartedPayload{BuildNumber: &buildB, SCMRevision: &revision})); err != nil {
		t.Fatalf("apply run.started B: %v", err)
	}

	if got := sent.count(); got != 2 {
		t.Fatalf("published %d notifications, want 2", got)
	}
	if got := sent.payload(0); got.RunID != runA.String() || got.Status != StatusRunning {
		t.Fatalf("first notification = %+v, want run A RUNNING", got)
	}
	assertDuplicateNotification(t, sent.payload(1), runB.String(), buildB)
	if got := runStatus(t, g, runB); got != StatusDuplicate {
		t.Fatalf("run B status = %q, want DUPLICATE", got)
	}
}

// TestDuplicateBackstopPublishesDuplicateForTheOtherRun asserts a
// run.started whose duplicate backstop marks another, newer run publishes
// one DUPLICATE notification for that run beside its own RUNNING one.
func TestDuplicateBackstopPublishesDuplicateForTheOtherRun(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)
	ctx := context.Background()

	revision := "rev-dup-notify-other"
	runOlder := seedGrantedIntent(t, pool, "api:dup-notify-other-a")
	runNewer := seedGrantedIntent(t, pool, "api:dup-notify-other-b")
	buildOlder, buildNewer := int64(1), int64(2)
	if _, err := g.Apply(ctx, newTestEnvelope(t, runNewer, "api:dup-notify-other-b", KindRunStarted,
		RunStartedPayload{BuildNumber: &buildNewer, SCMRevision: &revision})); err != nil {
		t.Fatalf("apply run.started of the newer run: %v", err)
	}
	if _, err := g.Apply(ctx, newTestEnvelope(t, runOlder, "api:dup-notify-other-a", KindRunStarted,
		RunStartedPayload{BuildNumber: &buildOlder, SCMRevision: &revision})); err != nil {
		t.Fatalf("apply run.started of the older run: %v", err)
	}

	if got := sent.count(); got != 3 {
		t.Fatalf("published %d notifications, want 3", got)
	}
	if got := sent.payload(1); got.RunID != runOlder.String() || got.Status != StatusRunning || got.BuildNumber != buildOlder {
		t.Fatalf("second notification = %+v, want the older run RUNNING", got)
	}
	assertDuplicateNotification(t, sent.payload(2), runNewer.String(), 0)
	if got := runStatus(t, g, runNewer); got != StatusDuplicate {
		t.Fatalf("newer run status = %q, want DUPLICATE", got)
	}
	if got := runStatus(t, g, runOlder); got != StatusRunning {
		t.Fatalf("older run status = %q, want RUNNING", got)
	}
}

// TestApplyTriggerAcceptedOfATakenKeyPublishesDuplicate asserts an accepted
// trigger whose intent key another run holds publishes the status its row
// stores, DUPLICATE, never QUEUED.
func TestApplyTriggerAcceptedOfATakenKeyPublishesDuplicate(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)

	const key = "upstream:dup-notify-accepted:t0000"
	seedGrantedIntent(t, pool, key)
	second := newTestUUIDv7String()
	res, err := g.Apply(context.Background(), newTriggerAcceptedEnvelope(t, second, key))
	if err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
	if !res.TriggerAcceptedStored {
		t.Fatal("the accepted trigger stored no row")
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications, want 1", got)
	}
	assertDuplicateNotification(t, sent.payload(0), second, 0)
	if got := runStatus(t, g, uuid.MustParse(second)); got != StatusDuplicate {
		t.Fatalf("status = %q, want DUPLICATE", got)
	}
}

// TestApplyOwnerCompletionOfADuplicateRunPublishesDuplicate asserts the
// owner's run.completed for a run the duplicate backstop marked publishes
// the status its row keeps, DUPLICATE, with the completion result.
func TestApplyOwnerCompletionOfADuplicateRunPublishesDuplicate(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	revision := "rev-dup-notify-completed"
	runA := seedGrantedIntent(t, pool, "api:dup-notify-completed-a")
	runB := seedGrantedIntent(t, pool, "api:dup-notify-completed-b")
	buildA, buildB := int64(1), int64(2)
	for _, started := range []IngestEnvelope{
		newTestEnvelope(t, runA, "api:dup-notify-completed-a", KindRunStarted,
			RunStartedPayload{BuildNumber: &buildA, SCMRevision: &revision}),
		newTestEnvelope(t, runB, "api:dup-notify-completed-b", KindRunStarted,
			RunStartedPayload{BuildNumber: &buildB, SCMRevision: &revision}),
	} {
		if _, err := g.Apply(ctx, started); err != nil {
			t.Fatalf("apply run.started: %v", err)
		}
	}

	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	g.SetRunNotifier(notifier)
	res, err := g.Apply(ctx, newTestEnvelope(t, runB, "api:dup-notify-completed-b", KindRunCompleted,
		RunCompletedPayload{Result: strPtr("ABORTED")}))
	if err != nil {
		t.Fatalf("apply run.completed B: %v", err)
	}
	if !res.CompletedOwner {
		t.Fatal("the result reports a foreign write, but pod-a owns the run")
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications, want 1", got)
	}
	if got := sent.payload(0); got.Kind != "run" || got.Status != StatusDuplicate || got.Result != "ABORTED" {
		t.Fatalf("published %+v, want run/%q/ABORTED", got, StatusDuplicate)
	}

	var status string
	var result *string
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runB).Scan(&status, &result); err != nil {
		t.Fatalf("read run B: %v", err)
	}
	if status != StatusDuplicate || result == nil || *result != "ABORTED" {
		t.Fatalf("run B = %s/%v, want DUPLICATE/ABORTED", status, result)
	}
}

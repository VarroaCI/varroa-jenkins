package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ingestBackstopFailure applies one run.trigger_failed envelope for the
// retry_run action a backstop fire published.
func ingestBackstopFailure(t *testing.T, g *Ingester, a LedgerAction, reasonCode string) {
	t.Helper()
	raw, err := json.Marshal(RunTriggerFailedPayload{
		Error:      "trigger build " + a.JobPath + ": ensure crumb: fetch crumb: HTTP 401",
		ReasonCode: reasonCode,
		Actor:      LedgerActionRecorder,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if _, err := g.Apply(context.Background(), IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindRunTriggerFailed,
		RunID:         a.RunID,
		IntentKey:     a.IntentKey,
		Cluster:       testCluster,
		Namespace:     a.Namespace,
		Cell:          a.Controller,
		CellInstance:  "pod-1",
		Tenant:        testTenant,
		JobPath:       a.JobPath,
		OccurredAt:    time.Now().UTC().Truncate(time.Microsecond),
		Payload:       raw,
	}); err != nil {
		t.Fatalf("apply run.trigger_failed: %v", err)
	}
}

// readFailedTrigger reads the stored error text and attempt of one failed
// run.
func readFailedTrigger(t *testing.T, db *sql.DB, runID string) (string, int) {
	t.Helper()
	var text string
	var attempt int
	if err := db.QueryRow(`
		SELECT payload->>'trigger_error', (payload->>'trigger_attempt')::int
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&text, &attempt); err != nil {
		t.Fatalf("read failed trigger %s: %v", runID, err)
	}
	return text, attempt
}

// retryOf answers the retry_run action of one fire's action pair.
func retryOf(t *testing.T, actions []LedgerAction) LedgerAction {
	t.Helper()
	for _, a := range actions {
		if a.Kind == LedgerActionRetryRun {
			return a
		}
	}
	t.Fatalf("no retry_run action in %+v", actions)
	return LedgerAction{}
}

// TestBackstopRefiresUnreachableTrigger asserts a fire that reached no
// Jenkins fires again on a later round, under the same intent key and on
// the job's home. Each re-fire needs the grace window since the last fire.
func TestBackstopRefiresUnreachableTrigger(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	g := NewIngester(pool, nil, nil)
	jobPath := "acme/cron-probe"
	ingestCronSchedule(t, g, testTenant, jobPath, 1, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	start := time.Now().UTC()
	round := func(at time.Time) {
		t.Helper()
		if _, err := RunBackstopRound(context.Background(), pool, conn,
			BackstopConfig{Now: func() time.Time { return at }}); err != nil {
			t.Fatalf("RunBackstopRound: %v", err)
		}
	}

	round(start)
	first := retryOf(t, wait(2))
	ingestBackstopFailure(t, g, first, reasonTriggerUnreachable)

	// Inside the grace window the round fires nothing.
	round(start.Add(time.Minute))
	wait(0)

	round(start.Add(6 * time.Minute))
	second := retryOf(t, wait(2))
	if second.IntentKey != first.IntentKey {
		t.Fatalf("re-fire intent key = %q, want %q", second.IntentKey, first.IntentKey)
	}
	if second.RunID == first.RunID {
		t.Fatal("re-fire reused the first fire's run id, want a fresh one")
	}
	if second.Namespace != jsTestNS || second.Controller != "cell-a" {
		t.Fatalf("re-fire address = %s/%s, want %s/cell-a", second.Namespace, second.Controller, jsTestNS)
	}
	if n := countBackstopActivity(t, db, jobPath); n != 2 {
		t.Fatalf("backstop activity rows = %d, want 2", n)
	}
	if _, attempt := readFailedTrigger(t, db, first.RunID); attempt != 1 {
		t.Fatalf("first failure attempt = %d, want 1", attempt)
	}
}

// TestBackstopDoesNotRefireAPermanentFailure asserts TRIGGER_REJECTED ends
// the occurrence: no later round fires it again.
func TestBackstopDoesNotRefireAPermanentFailure(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	g := NewIngester(pool, nil, nil)
	ingestCronSchedule(t, g, testTenant, "acme/rejected", 1, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	start := time.Now().UTC()
	if _, err := RunBackstopRound(context.Background(), pool, conn,
		BackstopConfig{Now: func() time.Time { return start }}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}
	first := retryOf(t, wait(2))
	ingestBackstopFailure(t, g, first, reasonTriggerRejected)

	later := start.Add(6 * time.Minute)
	if _, err := RunBackstopRound(context.Background(), pool, conn,
		BackstopConfig{Now: func() time.Time { return later }}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}
	wait(0)
}

// TestBackstopWaitsForAnAnsweredFire asserts a re-fire still waiting on a
// booting cell gets no further fire, even past the grace window, until
// backstopAnswerWait passes.
func TestBackstopWaitsForAnAnsweredFire(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	g := NewIngester(pool, nil, nil)
	ingestCronSchedule(t, g, testTenant, "acme/pending", 1, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	start := time.Now().UTC()
	round := func(at time.Time) {
		t.Helper()
		if _, err := RunBackstopRound(context.Background(), pool, conn,
			BackstopConfig{Now: func() time.Time { return at }}); err != nil {
			t.Fatalf("RunBackstopRound: %v", err)
		}
	}
	round(start)
	ingestBackstopFailure(t, g, retryOf(t, wait(2)), reasonTriggerUnreachable)
	round(start.Add(6 * time.Minute))
	retryOf(t, wait(2))

	// The re-fire has no answer yet.
	round(start.Add(12 * time.Minute))
	wait(0)

	// A fire with no answer after backstopAnswerWait never reached its cell.
	round(start.Add(6*time.Minute + backstopAnswerWait + time.Minute))
	retryOf(t, wait(2))
}

// TestBackstopStopsAfterTheRefireCap asserts an occurrence fires at most
// once plus maxBackstopRefires times, and the last failed run names the
// cap in its error text.
func TestBackstopStopsAfterTheRefireCap(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	g := NewIngester(pool, nil, nil)
	jobPath := "acme/capped"
	ingestCronSchedule(t, g, testTenant, jobPath, 1, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	start := time.Now().UTC()
	var last LedgerAction
	for i := 0; i <= maxBackstopRefires; i++ {
		at := start.Add(time.Duration(i) * 6 * time.Minute)
		if _, err := RunBackstopRound(context.Background(), pool, conn,
			BackstopConfig{Now: func() time.Time { return at }}); err != nil {
			t.Fatalf("RunBackstopRound: %v", err)
		}
		last = retryOf(t, wait(2))
		ingestBackstopFailure(t, g, last, reasonTriggerUnreachable)
	}

	text, attempt := readFailedTrigger(t, db, last.RunID)
	if attempt != maxBackstopRefires+1 {
		t.Fatalf("last attempt = %d, want %d", attempt, maxBackstopRefires+1)
	}
	if !strings.Contains(text, backstopRefireSpentNote) {
		t.Fatalf("last error = %q, want it to name the cap", text)
	}

	at := start.Add(time.Duration(maxBackstopRefires+1) * 6 * time.Minute)
	if _, err := RunBackstopRound(context.Background(), pool, conn,
		BackstopConfig{Now: func() time.Time { return at }}); err != nil {
		t.Fatalf("RunBackstopRound: %v", err)
	}
	wait(0)
	if n := countBackstopActivity(t, db, jobPath); n != maxBackstopRefires+1 {
		t.Fatalf("backstop activity rows = %d, want %d", n, maxBackstopRefires+1)
	}
}

// TestBackstopPacesAFireWithNoActivityRow asserts the grace window still
// paces a re-fire when the fire's activity row is missing: the failed
// run's own mint time stands in for it.
func TestBackstopPacesAFireWithNoActivityRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	g := NewIngester(pool, nil, nil)
	jobPath := "acme/no-activity"
	ingestCronSchedule(t, g, testTenant, jobPath, 1, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	start := time.Now().UTC()
	round := func(at time.Time) {
		t.Helper()
		if _, err := RunBackstopRound(context.Background(), pool, conn,
			BackstopConfig{Now: func() time.Time { return at }}); err != nil {
			t.Fatalf("RunBackstopRound: %v", err)
		}
	}
	round(start)
	ingestBackstopFailure(t, g, retryOf(t, wait(2)), reasonTriggerUnreachable)
	if _, err := db.Exec(`DELETE FROM varroa.activity WHERE item_path = $1`, jobPath); err != nil {
		t.Fatalf("delete the activity row: %v", err)
	}

	round(start.Add(time.Minute))
	wait(0)

	round(start.Add(6 * time.Minute))
	retryOf(t, wait(2))
}

// TestBackstopCountsUnansweredFires asserts a fire that never answers
// still spends its attempt, so unanswered re-fires stop at the limit too.
func TestBackstopCountsUnansweredFires(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedGrantPlacement(t, db, testCluster, testTenant, jsTestNS, "cell-a", 1, "{}")
	g := NewIngester(pool, nil, nil)
	jobPath := "acme/unanswered"
	ingestCronSchedule(t, g, testTenant, jobPath, 1, time.Now().UTC().Add(-10*time.Minute))

	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	wait := collectBackstopActions(t, conn)

	start := time.Now().UTC()
	round := func(at time.Time) {
		t.Helper()
		if _, err := RunBackstopRound(context.Background(), pool, conn,
			BackstopConfig{Now: func() time.Time { return at }}); err != nil {
			t.Fatalf("RunBackstopRound: %v", err)
		}
	}
	// The first fire answers; every re-fire stays unanswered.
	round(start)
	ingestBackstopFailure(t, g, retryOf(t, wait(2)), reasonTriggerUnreachable)
	step := backstopAnswerWait + time.Minute
	var last LedgerAction
	for i := 1; i <= maxBackstopRefires; i++ {
		round(start.Add(time.Duration(i) * step))
		last = retryOf(t, wait(2))
	}
	round(start.Add(time.Duration(maxBackstopRefires+1) * step))
	wait(0)

	// The last fire's late answer names the limit, although two fires
	// never answered.
	ingestBackstopFailure(t, g, last, reasonTriggerUnreachable)
	text, attempt := readFailedTrigger(t, db, last.RunID)
	if attempt != maxBackstopRefires+1 || !strings.Contains(text, backstopRefireSpentNote) {
		t.Fatalf("last failure = attempt %d, %q; want attempt %d naming the limit", attempt, text, maxBackstopRefires+1)
	}
	if n := countBackstopActivity(t, db, jobPath); n != maxBackstopRefires+1 {
		t.Fatalf("backstop activity rows = %d, want %d", n, maxBackstopRefires+1)
	}
}

// TestBackstopOccurrenceOfKey asserts the occurrence parse accepts only a
// key the backstop itself writes for the job.
func TestBackstopOccurrenceOfKey(t *testing.T) {
	occ := time.Date(2026, 9, 27, 20, 44, 0, 0, time.UTC)
	key := cronIntentKey("acme/app", occ)
	if got, ok := backstopOccurrenceOfKey(key, "acme/app"); !ok || !got.Equal(occ) {
		t.Fatalf("parse %q = %v %v, want %v", key, got, ok, occ)
	}
	for _, bad := range []string{
		"manual:" + key,
		"cron:acme/other:2026-09-27T20:44:00Z",
		"cron:acme/app:not-a-time",
		"cron:acme/app:2026-09-27T20:44:30Z",
	} {
		if _, ok := backstopOccurrenceOfKey(bad, "acme/app"); ok {
			t.Fatalf("parse %q succeeded, want a refusal", bad)
		}
	}
}

// TestMergeBackstopRetries asserts a re-fire yields to a newer first fire
// of the same job, and joins the round otherwise.
func TestMergeBackstopRetries(t *testing.T) {
	base := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	fires := []backstopFireCandidate{{cluster: "core", jobPath: "acme/a", occurrence: base.Add(10 * time.Minute)}}
	retries := []backstopFireCandidate{
		{cluster: "core", jobPath: "acme/a", occurrence: base, attempt: 2},
		{cluster: "core", jobPath: "acme/b", occurrence: base, attempt: 2},
	}
	got := mergeBackstopRetries(fires, retries)
	if len(got) != 2 || got[1].jobPath != "acme/b" {
		t.Fatalf("merged = %+v, want the first fire and the acme/b re-fire", got)
	}
}

package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// --- 9.7: the fire decision ---

// TestSelectFireCatchUpOneFiresNewestAndCountsRest asserts cronCatchUp
// "one" fires the newest candidate and counts the older ones missed.
func TestSelectFireCatchUpOneFiresNewestAndCountsRest(t *testing.T) {
	now := time.Now().UTC()
	oldest := now.Add(-40 * time.Minute)
	middle := now.Add(-25 * time.Minute)
	newest := now.Add(-10 * time.Minute)

	fire, missed := selectFire([]time.Time{oldest, newest, middle}, "one")
	if fire == nil || !fire.Equal(newest) {
		t.Fatalf("fire = %v, want the newest occurrence %v", fire, newest)
	}
	if missed != 2 {
		t.Fatalf("missed = %d, want 2", missed)
	}
}

// TestSelectFireCatchUpNoneFiresNothingAndCountsEveryMiss asserts
// cronCatchUp "none" fires nothing and counts every candidate missed.
func TestSelectFireCatchUpNoneFiresNothingAndCountsEveryMiss(t *testing.T) {
	now := time.Now().UTC()
	candidates := []time.Time{now.Add(-40 * time.Minute), now.Add(-25 * time.Minute), now.Add(-10 * time.Minute)}

	fire, missed := selectFire(candidates, cronCatchUpNone)
	if fire != nil {
		t.Fatalf("fire = %v, want nil for cronCatchUp none", fire)
	}
	if missed != len(candidates) {
		t.Fatalf("missed = %d, want %d", missed, len(candidates))
	}
}

// TestSelectFireEmptyCandidatesFiresNothing asserts an empty candidate
// list fires nothing and counts nothing, regardless of policy: this is
// the counter test the acceptance criteria names.
func TestSelectFireEmptyCandidatesFiresNothing(t *testing.T) {
	fire, missed := selectFire(nil, defaultCronCatchUp)
	if fire != nil || missed != 0 {
		t.Fatalf("selectFire(nil) = (%v, %d), want (nil, 0)", fire, missed)
	}
}

// --- 9.8: the fire path ---

// backstopSpreadTestJobPath names one of a same-minute batch of jobs
// TestSpreadDelaySpreadsAcrossGraceWindow and
// TestSelectReadyFiresBoundedAt100 use.
func backstopSpreadTestJobPath(i int) string {
	return fmt.Sprintf("acme/job-%03d", i)
}

// TestSpreadDelaySpreadsAcrossGraceWindow asserts a batch of jobs that
// all miss the same occurrence minute spread across the grace window,
// not all at delay zero.
func TestSpreadDelaySpreadsAcrossGraceWindow(t *testing.T) {
	grace := 5 * time.Minute
	seen := map[time.Duration]bool{}
	for i := 0; i < 60; i++ {
		d := spreadDelay(testCluster, testTenant, backstopSpreadTestJobPath(i), grace)
		if d < 0 || d >= grace {
			t.Fatalf("spreadDelay = %v, want within [0, %v)", d, grace)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatalf("want the 60 jobs' delays to spread across the grace window, got %d distinct value(s)", len(seen))
	}
}

// TestSelectReadyFiresBoundedAt100 asserts 250 ready candidates produce
// 100 ready fires, oldest occurrence first, carrying the rest forward.
func TestSelectReadyFiresBoundedAt100(t *testing.T) {
	now := time.Now().UTC()
	var candidates []backstopFireCandidate
	for i := 0; i < 250; i++ {
		candidates = append(candidates, backstopFireCandidate{
			cluster: testCluster, tenant: testTenant, jobPath: backstopSpreadTestJobPath(i),
			occurrence: now.Add(-time.Duration(300-i) * time.Minute), grace: 0,
		})
	}

	ready := selectReadyFires(candidates, now)
	if len(ready) != maxBackstopFiresPerRound {
		t.Fatalf("len(ready) = %d, want %d", len(ready), maxBackstopFiresPerRound)
	}
	for i := 1; i < len(ready); i++ {
		if ready[i].occurrence.Before(ready[i-1].occurrence) {
			t.Fatalf("ready fires are not ordered oldest occurrence first at index %d", i)
		}
	}
}

// TestFireOnePublishesWakeThenRetryAndWritesActivity asserts one fire
// publishes wake_cell then retry_run with the cron intent key, writes
// one build.scheduled.backstop activity row, and counts both
// recorder_cron_fired_total and recorder_actions_published_total.
func TestFireOnePublishesWakeThenRetryAndWritesActivity(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)

	var kinds []string
	received := make(chan LedgerAction, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()

	reader := useTestMeterProvider(t)
	backstopMetrics, err := NewBackstopMetrics()
	if err != nil {
		t.Fatalf("new backstop metrics: %v", err)
	}
	actionMetrics, err := NewActionMetrics()
	if err != nil {
		t.Fatalf("new action metrics: %v", err)
	}

	now := time.Now().UTC()
	occ := now.Add(-10 * time.Minute)
	c := backstopFireCandidate{
		cluster: testCluster, tenant: testTenant, namespace: testNamespace, cell: "cell-a",
		jobPath: "acme/fire-one", occurrence: occ, grace: backstopTestGrace,
	}
	fireOne(context.Background(), pool, conn, backstopMetrics, actionMetrics, c, now, nil)

	for i := 0; i < 2; i++ {
		select {
		case a := <-received:
			kinds = append(kinds, a.Kind)
			if a.Kind == LedgerActionRetryRun {
				if a.IntentKey != cronIntentKey(c.jobPath, occ) {
					t.Fatalf("retry_run intent key = %q, want %q", a.IntentKey, cronIntentKey(c.jobPath, occ))
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("want two actions published")
		}
	}
	if len(kinds) != 2 || kinds[0] != LedgerActionWakeCell || kinds[1] != LedgerActionRetryRun {
		t.Fatalf("published kinds = %v, want [wake_cell retry_run]", kinds)
	}

	var count int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.activity WHERE type = 'build.scheduled.backstop' AND item_path = $1
	`, c.jobPath).Scan(&count); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("activity rows = %d, want 1", count)
	}

	if got := metricValue(t, reader, "recorder_cron_fired_total"); got != 1 {
		t.Fatalf("recorder_cron_fired_total = %d, want 1", got)
	}
	if got := sumCounterByAttr(t, reader, "recorder_actions_published_total", "kind", LedgerActionWakeCell); got != 1 {
		t.Fatalf("recorder_actions_published_total{kind=wake_cell} = %d, want 1", got)
	}
	if got := sumCounterByAttr(t, reader, "recorder_actions_published_total", "kind", LedgerActionRetryRun); got != 1 {
		t.Fatalf("recorder_actions_published_total{kind=retry_run} = %d, want 1", got)
	}
}

// TestFireOnePublishesActionsEvenWhenActivityInsertFails asserts D11's
// retry rule (gap 9.8): the published action is at-most-once, so a lost
// or failed suppression row must never hold the actions back. This test
// forces the activity insert to fail (a timestamp outside every
// partition migration 00002 creates) and asserts wake_cell and
// retry_run still publish, and both counters still count, before the
// failed write is even attempted. The order stays publish-then-insert:
// keeping no row on a failed write makes the very next round retry the
// occurrence promptly, which is what design D11 asks for ("a lost
// action must be retried; a permanent skip would strand the
// occurrence"). Reversing the order would instead let a failed publish
// leave a row behind, suppressing a retry for the whole grace window.
func TestFireOnePublishesActionsEvenWhenActivityInsertFails(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestActionConn(t)

	var kinds []string
	received := make(chan LedgerAction, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()

	reader := useTestMeterProvider(t)
	backstopMetrics, err := NewBackstopMetrics()
	if err != nil {
		t.Fatalf("new backstop metrics: %v", err)
	}
	actionMetrics, err := NewActionMetrics()
	if err != nil {
		t.Fatalf("new action metrics: %v", err)
	}

	occ := time.Now().UTC().Add(-10 * time.Minute)
	// far holds no partition: migration 00002 creates only the previous
	// month, the current month, and the next two months.
	far := time.Now().UTC().AddDate(0, 6, 0)
	c := backstopFireCandidate{
		cluster: testCluster, tenant: testTenant, namespace: testNamespace, cell: "cell-a",
		jobPath: "acme/activity-write-fails", occurrence: occ, grace: backstopTestGrace,
	}
	fireOne(context.Background(), pool, conn, backstopMetrics, actionMetrics, c, far, nil)

	for i := 0; i < 2; i++ {
		select {
		case a := <-received:
			kinds = append(kinds, a.Kind)
		case <-time.After(3 * time.Second):
			t.Fatal("want two actions published even though the activity insert fails")
		}
	}
	if len(kinds) != 2 || kinds[0] != LedgerActionWakeCell || kinds[1] != LedgerActionRetryRun {
		t.Fatalf("published kinds = %v, want [wake_cell retry_run]", kinds)
	}

	var count int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.activity WHERE type = 'build.scheduled.backstop' AND item_path = $1
	`, c.jobPath).Scan(&count); err != nil {
		t.Fatalf("count activity rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("activity rows = %d, want 0: the insert should have failed against the missing partition", count)
	}

	if got := metricValue(t, reader, "recorder_cron_fired_total"); got != 1 {
		t.Fatalf("recorder_cron_fired_total = %d, want 1: the fire counts before the activity write is attempted", got)
	}
	if got := sumCounterByAttr(t, reader, "recorder_actions_published_total", "kind", LedgerActionRetryRun); got != 1 {
		t.Fatalf("recorder_actions_published_total{kind=retry_run} = %d, want 1", got)
	}
}

// TestFireOneAndOrdinaryFireShareOneIntentKey asserts a late ordinary
// fire and a backstop fire for the same job and occurrence minute share
// one intent key: whichever claims it second answers duplicate, so
// exactly one build runs.
func TestFireOneAndOrdinaryFireShareOneIntentKey(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	conn := newTestActionConn(t)

	received := make(chan LedgerAction, 4)
	sub, err := conn.Subscribe(bus.LedgerActionSubject(testCluster), func(msg *nats.Msg) {
		var a LedgerAction
		_ = json.Unmarshal(msg.Data, &a)
		received <- a
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	conn.NATSConn().Flush()

	now := time.Now().UTC()
	occ := now.Add(-10 * time.Minute)
	jobPath := "acme/shared-key"

	// The cell's own timer fires the occurrence first, an ordinary
	// claim with the deterministic cron intent key.
	ledger := NewLedger(pool, nil)
	ordinary := ClaimRequest{
		IntentKey: cronIntentKey(jobPath, occ), RunID: newTestUUIDv7String(), Cluster: testCluster,
		Namespace: testNamespace, Cell: "cell-a", CellInstance: "pod-a", Tenant: testTenant, JobPath: jobPath,
	}
	if resp := ledger.Claim(context.Background(), ordinary); resp.Decision != DecisionGranted {
		t.Fatalf("ordinary claim: want granted, got %+v", resp)
	}

	// The backstop round independently computes the same key for the
	// same job and occurrence, with no knowledge of the ordinary claim.
	backstopMetrics, err := NewBackstopMetrics()
	if err != nil {
		t.Fatalf("new backstop metrics: %v", err)
	}
	c := backstopFireCandidate{
		cluster: testCluster, tenant: testTenant, namespace: testNamespace, cell: "cell-a",
		jobPath: jobPath, occurrence: occ, grace: backstopTestGrace,
	}
	fireOne(context.Background(), pool, conn, backstopMetrics, nil, c, now, nil)

	var retryAction LedgerAction
	found := false
	for i := 0; i < 2 && !found; i++ {
		select {
		case a := <-received:
			if a.Kind == LedgerActionRetryRun {
				retryAction = a
				found = true
			}
		case <-time.After(3 * time.Second):
			t.Fatal("want a retry_run action from the backstop fire")
		}
	}
	if !found {
		t.Fatal("want a retry_run action from the backstop fire")
	}
	if retryAction.IntentKey != ordinary.IntentKey {
		t.Fatalf("backstop retry_run intent key = %q, want the ordinary fire's own key %q", retryAction.IntentKey, ordinary.IntentKey)
	}

	// Replaying the backstop's own action as a claim, as the cell would
	// when the operator dispatches it, answers duplicate: exactly one
	// build ran.
	late := ClaimRequest{
		IntentKey: retryAction.IntentKey, RunID: newTestUUIDv7String(), Cluster: testCluster,
		Namespace: testNamespace, Cell: "cell-a", CellInstance: "pod-a", Tenant: testTenant, JobPath: jobPath,
	}
	resp := ledger.Claim(context.Background(), late)
	if resp.Decision != DecisionDuplicate {
		t.Fatalf("backstop's claim: want duplicate, got %+v", resp)
	}
}

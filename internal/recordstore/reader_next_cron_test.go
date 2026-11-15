package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// seedCronRow inserts one job_schedules row with the given fire times.
func seedCronRow(t *testing.T, db *sql.DB, jobPath, triggerType string, disabled bool, fires ...time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.job_schedules (cluster, namespace, cell, tenant, job_path, trigger_type, disabled,
			next_fire_times, observed_at, payload)
		VALUES ($1, $2, 'cell-a', 'pay', $3, $4, $5, $6, now(), '{}')
	`, testCluster, testNamespace, jobPath, triggerType, disabled, fires); err != nil {
		t.Fatalf("seed schedule %s: %v", jobPath, err)
	}
}

// TestNextCronRunPicksEarliestFutureTime asserts the earliest future occurrence
// of any cron job in scope is answered.
func TestNextCronRunPicksEarliestFutureTime(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC().Truncate(time.Second)
	seedCronRow(t, db, "pay/nightly", "cron", false, now.Add(3*time.Hour), now.Add(27*time.Hour))
	seedCronRow(t, db, "pay/hourly", "cron", false, now.Add(time.Hour))

	got, ok, err := r.NextCronRun(context.Background(), failingScope, now)
	if err != nil || !ok {
		t.Fatalf("NextCronRun = %+v, %v, %v; want a run", got, ok, err)
	}
	if got.JobPath != "pay/hourly" || !got.At.Equal(now.Add(time.Hour)) {
		t.Fatalf("next = %+v, want pay/hourly at +1h", got)
	}
}

// TestNextCronRunSkipsDisabledAndOtherTriggers asserts a disabled job and a job
// with another trigger type are left out.
func TestNextCronRunSkipsDisabledAndOtherTriggers(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC().Truncate(time.Second)
	seedCronRow(t, db, "pay/off", "cron", true, now.Add(time.Hour))
	seedCronRow(t, db, "pay/upstream", "upstream", false, now.Add(time.Hour))

	_, ok, err := r.NextCronRun(context.Background(), failingScope, now)
	if err != nil || ok {
		t.Fatalf("NextCronRun ok = %v, err = %v; want no run", ok, err)
	}
}

// TestNextCronRunSkipsPastTimes asserts a fire time at or before now is not answered.
func TestNextCronRunSkipsPastTimes(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC().Truncate(time.Second)
	seedCronRow(t, db, "pay/past", "cron", false, now.Add(-time.Minute))

	_, ok, err := r.NextCronRun(context.Background(), failingScope, now)
	if err != nil || ok {
		t.Fatalf("NextCronRun ok = %v, err = %v; want no run", ok, err)
	}
}

// TestNextCronRunJobPatternsApply asserts a scope's job patterns narrow the answer.
func TestNextCronRunJobPatternsApply(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	r := NewReader(pool)
	now := time.Now().UTC().Truncate(time.Second)
	seedCronRow(t, db, "pay/hourly", "cron", false, now.Add(time.Hour))
	seedCronRow(t, db, "pay/nightly", "cron", false, now.Add(2*time.Hour))

	scopes := []RunScope{{Cluster: testCluster, Tenant: "pay", JobPatterns: []string{`pay/nightly`}}}
	got, ok, err := r.NextCronRun(context.Background(), scopes, now)
	if err != nil || !ok || got.JobPath != "pay/nightly" {
		t.Fatalf("NextCronRun = %+v, %v, %v; want pay/nightly", got, ok, err)
	}
}

// TestNextCronRunEmptyScopes asserts an empty scope list answers no run.
func TestNextCronRunEmptyScopes(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	r := NewReader(pool)
	_, ok, err := r.NextCronRun(context.Background(), nil, time.Now().UTC())
	if err != nil || ok {
		t.Fatalf("NextCronRun(no scope) ok = %v, err = %v; want no run", ok, err)
	}
}

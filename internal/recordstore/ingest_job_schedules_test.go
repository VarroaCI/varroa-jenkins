package recordstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// jobScheduleTestCell fixes the cell coordinates every job.schedules test
// shares, unless a test overrides them.
const (
	jsTestCluster = "core"
	jsTestNS      = "jenkins"
	jsTestCell    = "cell-a"
)

// newJobScheduleEnvelope builds a job.schedules IngestEnvelope for
// jsTestCluster/jsTestNS/cell, per design.md D8: an empty run_id,
// intent_key, job_path, and top-level tenant.
func newJobScheduleEnvelope(t *testing.T, cell string, occurredAt time.Time, payload JobSchedulesPayload) IngestEnvelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal job schedules payload: %v", err)
	}
	return IngestEnvelope{
		SchemaVersion: 1,
		Kind:          KindJobSchedules,
		Cluster:       jsTestCluster,
		Namespace:     jsTestNS,
		Cell:          cell,
		// Postgres timestamptz has microsecond precision; truncating here
		// keeps a round-tripped value exactly comparable with Equal.
		OccurredAt: occurredAt.Truncate(time.Microsecond),
		Payload:    raw,
	}
}

// randomHex returns a hex string of n bytes' worth of crypto/rand output
// (2n hex characters). Random bytes resist Postgres's TOAST compression,
// unlike a repeated character, so a string this size stays too large for
// a btree index entry.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(buf)
}

// countJobScheduleRows counts jsTestCell's rows. Every caller in this
// file shares the one test cell.
func countJobScheduleRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.job_schedules WHERE cluster = $1 AND cell = $2
	`, jsTestCluster, jsTestCell).Scan(&n); err != nil {
		t.Fatalf("count job_schedules: %v", err)
	}
	return n
}

// --- 10.4: no intent needed, two-tenant generations, fire-time round trip ---

func TestApplyJobSchedules_NoIntentNeeded(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	env := newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC(), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "H * * * *"},
		},
	})

	result, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply job.schedules: %v", err)
	}
	if result.Truncated || result.ScheduleStale {
		t.Fatalf("result = %+v, want a clean apply", result)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedules WHERE tenant = 'acme' AND job_path = 'acme/app'`).Scan(&n); err != nil {
		t.Fatalf("count row: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

func TestApplyJobSchedules_TwoTenantsTwoGenerations(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	env := newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC(), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "alpha", PlacementGeneration: 4, JobPath: "alpha/app", TriggerType: "cron"},
			{Tenant: "beta", PlacementGeneration: 9, JobPath: "beta/app", TriggerType: "cron"},
		},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply job.schedules: %v", err)
	}

	var genAlpha, genBeta int64
	if err := db.QueryRow(`SELECT placement_generation FROM varroa.job_schedules WHERE tenant = 'alpha'`).Scan(&genAlpha); err != nil {
		t.Fatalf("read alpha generation: %v", err)
	}
	if err := db.QueryRow(`SELECT placement_generation FROM varroa.job_schedules WHERE tenant = 'beta'`).Scan(&genBeta); err != nil {
		t.Fatalf("read beta generation: %v", err)
	}
	if genAlpha != 4 || genBeta != 9 {
		t.Fatalf("generations = alpha:%d beta:%d, want 4/9", genAlpha, genBeta)
	}
}

func TestApplyJobSchedules_FireTimesRoundTrip(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	g := NewIngester(pool, nil, nil)

	want := []string{
		"2026-09-05T13:07:00Z",
		"2026-09-05T13:22:00Z",
		"2026-09-05T13:37:00Z",
	}
	env := newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC(), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", NextFireTimes: want},
		},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply job.schedules: %v", err)
	}

	rows, err := reader.ListJobSchedules(context.Background(), jsTestCluster, "acme")
	if err != nil {
		t.Fatalf("list job schedules: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if len(rows[0].NextFireTimes) != len(want) {
		t.Fatalf("len(NextFireTimes) = %d, want %d", len(rows[0].NextFireTimes), len(want))
	}
	for i, wantStr := range want {
		wantT, err := time.Parse(time.RFC3339, wantStr)
		if err != nil {
			t.Fatalf("parse want[%d]: %v", i, err)
		}
		if !rows[0].NextFireTimes[i].Equal(wantT) {
			t.Errorf("NextFireTimes[%d] = %v, want %v", i, rows[0].NextFireTimes[i], wantT)
		}
	}
}

func TestApplyJobSchedules_RemovedTriggerDisappears(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	t1 := time.Now().UTC()
	env1 := newJobScheduleEnvelope(t, jsTestCell, t1, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "H * * * *"},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	t2 := t1.Add(time.Minute)
	env2 := newJobScheduleEnvelope(t, jsTestCell, t2, JobSchedulesPayload{
		Hash: "h2",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "none"},
		},
	})
	if _, err := g.Apply(ctx, env2); err != nil {
		t.Fatalf("apply env2: %v", err)
	}

	var cronCount, noneCount int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedules WHERE job_path = 'acme/app' AND trigger_type = 'cron'`).Scan(&cronCount); err != nil {
		t.Fatalf("count cron rows: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedules WHERE job_path = 'acme/app' AND trigger_type = 'none'`).Scan(&noneCount); err != nil {
		t.Fatalf("count none rows: %v", err)
	}
	if cronCount != 0 {
		t.Errorf("cronCount = %d, want 0 (removed trigger must disappear)", cronCount)
	}
	if noneCount != 1 {
		t.Errorf("noneCount = %d, want 1", noneCount)
	}
}

func TestApplyJobSchedules_TruncatedSetDeletesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	t1 := time.Now().UTC()
	env1 := newJobScheduleEnvelope(t, jsTestCell, t1, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app-a", TriggerType: "cron"},
			{Tenant: "acme", JobPath: "acme/app-b", TriggerType: "cron"},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	t2 := t1.Add(time.Minute)
	env2 := newJobScheduleEnvelope(t, jsTestCell, t2, JobSchedulesPayload{
		Hash:      "h2",
		Truncated: true,
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app-a", TriggerType: "cron"},
		},
	})
	result, err := g.Apply(ctx, env2)
	if err != nil {
		t.Fatalf("apply env2: %v", err)
	}
	if !result.Truncated {
		t.Error("result.Truncated = false, want true")
	}

	n := countJobScheduleRows(t, db)
	if n != 2 {
		t.Fatalf("row count = %d, want 2 (a truncated set must delete nothing, including app-b which env2 omitted)", n)
	}
}

// --- 10.5: the three upstream columns store and read back ---

// TestApplyJobSchedules_VarroaUpstreamRoundTrip asserts the three
// upstream columns store in the same transaction as every other column
// and the reader returns them, design.md D9.
func TestApplyJobSchedules_VarroaUpstreamRoundTrip(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	env := newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC(), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{
				Tenant: "beta", JobPath: "beta/rebuild", TriggerType: "varroa-upstream",
				UpstreamTenant: "alpha", UpstreamJob: "release", UpstreamResult: "SUCCESS",
			},
		},
	})
	if _, err := g.Apply(ctx, env); err != nil {
		t.Fatalf("apply env: %v", err)
	}

	rows, err := reader.ListJobSchedules(ctx, jsTestCluster, "beta")
	if err != nil {
		t.Fatalf("list job schedules: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.UpstreamTenant != "alpha" || got.UpstreamJob != "release" || got.UpstreamResult != "SUCCESS" {
		t.Errorf("row = %+v, want upstream tenant alpha, job release, result SUCCESS", got)
	}
}

// TestApplyJobSchedules_VarroaUpstreamRemovedTriggerDeletesRow asserts a
// removed varroa-upstream trigger stops firing on the next set: the row
// disappears from the table.
func TestApplyJobSchedules_VarroaUpstreamRemovedTriggerDeletesRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	t1 := time.Now().UTC()
	env1 := newJobScheduleEnvelope(t, jsTestCell, t1, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{
				Tenant: "beta", JobPath: "beta/rebuild", TriggerType: "varroa-upstream",
				UpstreamTenant: "alpha", UpstreamJob: "release", UpstreamResult: "SUCCESS",
			},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	t2 := t1.Add(time.Minute)
	env2 := newJobScheduleEnvelope(t, jsTestCell, t2, JobSchedulesPayload{
		Hash: "h2",
		Rows: []JobScheduleRowInput{
			{Tenant: "beta", JobPath: "beta/rebuild", TriggerType: "none"},
		},
	})
	if _, err := g.Apply(ctx, env2); err != nil {
		t.Fatalf("apply env2: %v", err)
	}

	var upstreamCount int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.job_schedules
		WHERE job_path = 'beta/rebuild' AND trigger_type = 'varroa-upstream'
	`).Scan(&upstreamCount); err != nil {
		t.Fatalf("count varroa-upstream rows: %v", err)
	}
	if upstreamCount != 0 {
		t.Errorf("upstreamCount = %d, want 0 (a removed trigger must disappear)", upstreamCount)
	}
}

// TestApplyJobSchedules_VarroaUpstreamTruncatedSetDeletesNone asserts a
// truncated set deletes no varroa-upstream row, even one the truncated
// envelope omitted.
func TestApplyJobSchedules_VarroaUpstreamTruncatedSetDeletesNone(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	t1 := time.Now().UTC()
	env1 := newJobScheduleEnvelope(t, jsTestCell, t1, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{
				Tenant: "beta", JobPath: "beta/rebuild-a", TriggerType: "varroa-upstream",
				UpstreamTenant: "alpha", UpstreamJob: "release", UpstreamResult: "SUCCESS",
			},
			{
				Tenant: "beta", JobPath: "beta/rebuild-b", TriggerType: "varroa-upstream",
				UpstreamTenant: "alpha", UpstreamJob: "release", UpstreamResult: "SUCCESS",
			},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	t2 := t1.Add(time.Minute)
	env2 := newJobScheduleEnvelope(t, jsTestCell, t2, JobSchedulesPayload{
		Hash:      "h2",
		Truncated: true,
		Rows: []JobScheduleRowInput{
			{
				Tenant: "beta", JobPath: "beta/rebuild-a", TriggerType: "varroa-upstream",
				UpstreamTenant: "alpha", UpstreamJob: "release", UpstreamResult: "SUCCESS",
			},
		},
	})
	result, err := g.Apply(ctx, env2)
	if err != nil {
		t.Fatalf("apply env2: %v", err)
	}
	if !result.Truncated {
		t.Error("result.Truncated = false, want true")
	}

	n := countJobScheduleRows(t, db)
	if n != 2 {
		t.Fatalf("row count = %d, want 2 (a truncated set must delete nothing, including rebuild-b which env2 omitted)", n)
	}
}

// TestApplyJobSchedules_LaterRowFailureRollsBackWholeEnvelope asserts a
// row that cannot be inserted rolls back the whole envelope: an earlier
// row's insert, run inside the same transaction, never commits, and the
// watermark stays at the previous envelope's value.
func TestApplyJobSchedules_LaterRowFailureRollsBackWholeEnvelope(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	t1 := time.Now().UTC()
	env1 := newJobScheduleEnvelope(t, jsTestCell, t1, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "h1-spec"},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	// job_schedules' PRIMARY KEY (migration 00003) is a btree index over
	// (cluster, tenant, job_path, trigger_type); a job_path long enough
	// pushes the combined key past Postgres's per-row btree index limit
	// (about 2704 bytes), so this row's INSERT fails at the database
	// level after the envelope's first row already inserted. The value
	// must resist compression, or Postgres's TOAST storage shrinks it
	// below the limit before indexing, so it comes from crypto/rand
	// rather than a repeated character.
	oversizedJobPath := randomHex(t, 3000)
	t2 := t1.Add(time.Minute)
	env2 := newJobScheduleEnvelope(t, jsTestCell, t2, JobSchedulesPayload{
		Hash: "h2",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/new-app", TriggerType: "cron"},
			{Tenant: "acme", JobPath: oversizedJobPath, TriggerType: "cron"},
		},
	})
	if _, err := g.Apply(ctx, env2); err == nil {
		t.Fatal("apply env2: want an error from the oversized primary key, got nil")
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.job_schedules WHERE job_path = 'acme/new-app'`).Scan(&n); err != nil {
		t.Fatalf("count acme/new-app: %v", err)
	}
	if n != 0 {
		t.Errorf("count(acme/new-app) = %d, want 0 (the envelope's earlier row must not commit)", n)
	}

	var spec string
	if err := db.QueryRow(`SELECT spec FROM varroa.job_schedules WHERE job_path = 'acme/app'`).Scan(&spec); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if spec != "h1-spec" {
		t.Errorf("spec = %q, want %q (env1's row must be unaffected)", spec, "h1-spec")
	}

	var watermark time.Time
	if err := db.QueryRow(`
		SELECT observed_at FROM varroa.job_schedule_cells WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, jsTestCell).Scan(&watermark); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if !watermark.Equal(env1.OccurredAt) {
		t.Errorf("watermark = %v, want %v (a failed envelope must not advance the watermark)", watermark, env1.OccurredAt)
	}
}

// --- 10.4b: the watermark guard ---

func TestApplyJobSchedules_StaleEnvelopeChangesNothing(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	newer := time.Now().UTC()
	older := newer.Add(-time.Minute)

	envNewer := newJobScheduleEnvelope(t, jsTestCell, newer, JobSchedulesPayload{
		Hash: "newer",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "newer-spec"},
		},
	})
	if _, err := g.Apply(ctx, envNewer); err != nil {
		t.Fatalf("apply newer: %v", err)
	}

	envOlder := newJobScheduleEnvelope(t, jsTestCell, older, JobSchedulesPayload{
		Hash: "older",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "older-spec"},
		},
	})
	result, err := g.Apply(ctx, envOlder)
	if err != nil {
		t.Fatalf("apply older (replay): %v", err)
	}
	if !result.ScheduleStale {
		t.Error("result.ScheduleStale = false, want true for an envelope older than the stored watermark")
	}

	var spec string
	if err := db.QueryRow(`SELECT spec FROM varroa.job_schedules WHERE job_path = 'acme/app'`).Scan(&spec); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if spec != "newer-spec" {
		t.Errorf("spec = %q, want %q (the stale replay must change nothing)", spec, "newer-spec")
	}
}

// TestApplyJobSchedules_EqualWatermarkNotApplied asserts design.md D8's
// strict-newer rule: an envelope whose occurred_at exactly equals the
// stored watermark is rejected the same as an older one, not applied.
func TestApplyJobSchedules_EqualWatermarkNotApplied(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	stamp := time.Now().UTC()

	env1 := newJobScheduleEnvelope(t, jsTestCell, stamp, JobSchedulesPayload{
		Hash: "first",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "first-spec"},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	envEqual := newJobScheduleEnvelope(t, jsTestCell, stamp, JobSchedulesPayload{
		Hash: "equal",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "equal-spec"},
		},
	})
	result, err := g.Apply(ctx, envEqual)
	if err != nil {
		t.Fatalf("apply equal-timestamp envelope: %v", err)
	}
	if !result.ScheduleStale {
		t.Error("result.ScheduleStale = false, want true for an envelope whose time equals the stored watermark")
	}

	var spec string
	if err := db.QueryRow(`SELECT spec FROM varroa.job_schedules WHERE job_path = 'acme/app'`).Scan(&spec); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if spec != "first-spec" {
		t.Errorf("spec = %q, want %q (an equal-timestamp envelope must change nothing)", spec, "first-spec")
	}
}

func TestApplyJobSchedules_EmptyUntruncatedDeletesAllKeepsWatermark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	t1 := time.Now().UTC()
	env1 := newJobScheduleEnvelope(t, jsTestCell, t1, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron"},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	t2 := t1.Add(time.Minute)
	env2 := newJobScheduleEnvelope(t, jsTestCell, t2, JobSchedulesPayload{Hash: "empty"})
	if _, err := g.Apply(ctx, env2); err != nil {
		t.Fatalf("apply empty env2: %v", err)
	}

	if n := countJobScheduleRows(t, db); n != 0 {
		t.Errorf("row count = %d, want 0 after an empty, untruncated set", n)
	}

	var watermark time.Time
	if err := db.QueryRow(`
		SELECT observed_at FROM varroa.job_schedule_cells WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, jsTestCell).Scan(&watermark); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if !watermark.Equal(env2.OccurredAt) {
		t.Errorf("watermark = %v, want %v (raised to the empty set's time)", watermark, env2.OccurredAt)
	}
}

func TestApplyJobSchedules_StaleAfterEmptySetRestoresNoRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	t1 := time.Now().UTC()
	env1 := newJobScheduleEnvelope(t, jsTestCell, t1, JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron"},
		},
	})
	if _, err := g.Apply(ctx, env1); err != nil {
		t.Fatalf("apply env1: %v", err)
	}

	t2 := t1.Add(time.Minute)
	empty := newJobScheduleEnvelope(t, jsTestCell, t2, JobSchedulesPayload{Hash: "empty"})
	if _, err := g.Apply(ctx, empty); err != nil {
		t.Fatalf("apply empty set: %v", err)
	}

	// Older than the empty set's watermark, but with the original row's
	// content: a redelivery replaying env1 out of order.
	stale := newJobScheduleEnvelope(t, jsTestCell, t1.Add(30*time.Second), JobSchedulesPayload{
		Hash: "stale-replay",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron"},
		},
	})
	result, err := g.Apply(ctx, stale)
	if err != nil {
		t.Fatalf("apply stale replay: %v", err)
	}
	if !result.ScheduleStale {
		t.Error("result.ScheduleStale = false, want true")
	}

	if n := countJobScheduleRows(t, db); n != 0 {
		t.Errorf("row count = %d, want 0 (a stale replay must restore no deleted row)", n)
	}
}

func TestApplyJobSchedules_ConcurrentAppliesSerializeOnWatermark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	// Seed an initial watermark row well before either concurrent
	// envelope, so both concurrent applies compete on an existing row
	// rather than on its creation.
	seedTime := time.Now().UTC().Add(-time.Hour)
	seed := newJobScheduleEnvelope(t, jsTestCell, seedTime, JobSchedulesPayload{Hash: "seed"})
	if _, err := NewIngester(pool, nil, nil).Apply(ctx, seed); err != nil {
		t.Fatalf("apply seed: %v", err)
	}

	older := seedTime.Add(time.Minute)
	newer := seedTime.Add(2 * time.Minute)
	envOlder := newJobScheduleEnvelope(t, jsTestCell, older, JobSchedulesPayload{
		Hash: "older",
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "older-spec"}},
	})
	envNewer := newJobScheduleEnvelope(t, jsTestCell, newer, JobSchedulesPayload{
		Hash: "newer",
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "newer-spec"}},
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	apply := func(env IngestEnvelope) {
		defer wg.Done()
		// Each concurrent replica uses its own Ingester (its own
		// transactions), matching two recorder replicas sharing one pool.
		if _, err := NewIngester(pool, nil, nil).Apply(ctx, env); err != nil {
			errs <- err
		}
	}
	wg.Add(2)
	go apply(envOlder)
	go apply(envNewer)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent apply: %v", err)
	}

	var spec string
	if err := db.QueryRow(`SELECT spec FROM varroa.job_schedules WHERE job_path = 'acme/app'`).Scan(&spec); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if spec != "newer-spec" {
		t.Errorf("spec = %q, want %q regardless of which transaction ran first", spec, "newer-spec")
	}
}

func TestApplyJobSchedules_ConcurrentFirstEnvelopesCreateOneWatermark(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	const cell = "cell-fresh"
	t1 := time.Now().UTC()
	t2 := t1.Add(time.Minute)
	env1 := newJobScheduleEnvelope(t, cell, t1, JobSchedulesPayload{
		Hash: "first",
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "first-spec"}},
	})
	env2 := newJobScheduleEnvelope(t, cell, t2, JobSchedulesPayload{
		Hash: "second",
		Rows: []JobScheduleRowInput{{Tenant: "acme", JobPath: "acme/app", TriggerType: "cron", Spec: "second-spec"}},
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	apply := func(env IngestEnvelope) {
		defer wg.Done()
		if _, err := NewIngester(pool, nil, nil).Apply(ctx, env); err != nil {
			errs <- err
		}
	}
	wg.Add(2)
	go apply(env1)
	go apply(env2)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent first apply: %v", err)
	}

	var watermarkCount int
	if err := db.QueryRow(`
		SELECT count(*) FROM varroa.job_schedule_cells WHERE cluster = $1 AND namespace = $2 AND cell = $3
	`, jsTestCluster, jsTestNS, cell).Scan(&watermarkCount); err != nil {
		t.Fatalf("count watermark rows: %v", err)
	}
	if watermarkCount != 1 {
		t.Fatalf("watermark row count = %d, want 1", watermarkCount)
	}

	var spec string
	if err := db.QueryRow(`SELECT spec FROM varroa.job_schedules WHERE job_path = 'acme/app' AND cell = $1`, cell).Scan(&spec); err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if spec != "second-spec" {
		t.Errorf("spec = %q, want %q (the newer envelope's content must stand)", spec, "second-spec")
	}
}

// --- 10.5: the reader ---

func TestListJobSchedules_ListsTenantRowsInOrder(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool)
	g := NewIngester(pool, nil, nil)

	env := newJobScheduleEnvelope(t, jsTestCell, time.Now().UTC(), JobSchedulesPayload{
		Hash: "h1",
		Rows: []JobScheduleRowInput{
			{Tenant: "acme", JobPath: "acme/b-app", TriggerType: "cron"},
			{Tenant: "acme", JobPath: "acme/a-app", TriggerType: "scm-poll"},
			{Tenant: "other-tenant", JobPath: "other/app", TriggerType: "cron"},
		},
	})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply job.schedules: %v", err)
	}

	rows, err := reader.ListJobSchedules(context.Background(), jsTestCluster, "acme")
	if err != nil {
		t.Fatalf("list job schedules: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (only the requested tenant's rows)", len(rows))
	}
	if rows[0].JobPath != "acme/a-app" || rows[1].JobPath != "acme/b-app" {
		t.Errorf("rows = %+v, want ordered by job_path", rows)
	}
}

// --- passed occurrences survive a later export ---

// applyCronEnvelope applies one envelope for jsTestCell at occurredAt
// carrying one acme/app cron row with spec and occurrences.
func applyCronEnvelope(t *testing.T, g *Ingester, occurredAt time.Time, spec string, occurrences ...time.Time) {
	t.Helper()
	fireTimes := make([]string, len(occurrences))
	for i, occ := range occurrences {
		fireTimes[i] = occ.UTC().Format(time.RFC3339)
	}
	env := newJobScheduleEnvelope(t, jsTestCell, occurredAt, JobSchedulesPayload{
		Hash: spec,
		Rows: []JobScheduleRowInput{{
			Tenant: testTenant, JobPath: "acme/app", TriggerType: "cron", Spec: spec, NextFireTimes: fireTimes,
		}},
	})
	result, err := g.Apply(context.Background(), env)
	if err != nil {
		t.Fatalf("apply job.schedules: %v", err)
	}
	if result.ScheduleStale {
		t.Fatalf("envelope at %v was stale", occurredAt)
	}
}

// readAppOccurrences reads acme/app's stored occurrences.
func readAppOccurrences(t *testing.T, pool *pgxpool.Pool) []time.Time {
	t.Helper()
	rows, err := NewReader(pool).ListJobSchedules(context.Background(), jsTestCluster, testTenant)
	if err != nil {
		t.Fatalf("list job schedules: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	return rows[0].NextFireTimes
}

func assertOccurrences(t *testing.T, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("occurrences = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("occurrences = %v, want %v", got, want)
		}
	}
}

// scheduleBase is a fixed minute the passed-occurrence tests count from.
func scheduleBase() time.Time {
	return time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)
}

func TestApplyJobSchedules_KeepsPassedOccurrences(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	t0 := scheduleBase()
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	applyCronEnvelope(t, g, t0, "*/2 * * * *", m(2), m(4), m(6))
	applyCronEnvelope(t, g, m(5), "*/2 * * * *", m(6), m(8), m(10))

	assertOccurrences(t, readAppOccurrences(t, pool), []time.Time{m(2), m(4), m(6), m(8), m(10)})
}

func TestApplyJobSchedules_DropsOccurrencesPastTheWindow(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	t0 := scheduleBase()
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	// The default grace is five minutes, so the window is twenty-two:
	// two graces, the ten-minute answer wait, and the slack.
	applyCronEnvelope(t, g, t0, "* * * * *", m(1), m(4))
	applyCronEnvelope(t, g, m(25), "* * * * *", m(26))

	assertOccurrences(t, readAppOccurrences(t, pool), []time.Time{m(4), m(26)})
}

func TestApplyJobSchedules_SpecChangeReplacesOccurrences(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	t0 := scheduleBase()
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	applyCronEnvelope(t, g, t0, "*/2 * * * *", m(2), m(4))
	applyCronEnvelope(t, g, m(5), "*/3 * * * *", m(6), m(9))

	assertOccurrences(t, readAppOccurrences(t, pool), []time.Time{m(6), m(9)})
}

func TestApplyJobSchedules_CapsPassedOccurrences(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	t0 := scheduleBase()
	s := func(n int) time.Time { return t0.Add(time.Duration(n) * 30 * time.Second) }

	var first []time.Time
	for i := 1; i <= 20; i++ {
		first = append(first, s(i))
	}
	applyCronEnvelope(t, g, t0, "* * * * *", first...)
	applyCronEnvelope(t, g, s(21), "* * * * *", s(22))

	want := append([]time.Time{}, first[4:]...)
	want = append(want, s(22))
	assertOccurrences(t, readAppOccurrences(t, pool), want)
}

func TestApplyJobSchedules_WindowFollowsTenantGrace(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementPolicy(t, db, `{"cronGrace":"1m"}`)
	if _, err := db.Exec(`UPDATE varroa.tenant_placements SET namespace = $1, cell = $2 WHERE cluster = $3 AND tenant = $4`,
		jsTestNS, jsTestCell, testCluster, testTenant); err != nil {
		t.Fatalf("place tenant on the test cell: %v", err)
	}
	g := NewIngester(pool, nil, nil)
	t0 := scheduleBase()
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	// A one-minute grace makes the window fourteen minutes, because the
	// ten-minute answer wait outlasts three graces.
	applyCronEnvelope(t, g, t0, "* * * * *", m(1), m(3))
	applyCronEnvelope(t, g, m(16), "* * * * *", m(17))

	assertOccurrences(t, readAppOccurrences(t, pool), []time.Time{m(3), m(17)})
}

func TestApplyJobSchedules_DisabledChangeReplacesOccurrences(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	t0 := scheduleBase()
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

	apply := func(at time.Time, disabled bool, occurrences ...time.Time) {
		t.Helper()
		fireTimes := make([]string, len(occurrences))
		for i, occ := range occurrences {
			fireTimes[i] = occ.UTC().Format(time.RFC3339)
		}
		env := newJobScheduleEnvelope(t, jsTestCell, at, JobSchedulesPayload{
			Hash: "h",
			Rows: []JobScheduleRowInput{{
				Tenant: testTenant, JobPath: "acme/app", TriggerType: "cron", Spec: "*/2 * * * *",
				Disabled: disabled, NextFireTimes: fireTimes,
			}},
		})
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply job.schedules: %v", err)
		}
	}
	// Jenkins skips the occurrences while the job is disabled, so a
	// re-enabled row must not keep them.
	apply(t0, true, m(2), m(4))
	apply(m(5), false, m(6), m(8))

	assertOccurrences(t, readAppOccurrences(t, pool), []time.Time{m(6), m(8)})
}

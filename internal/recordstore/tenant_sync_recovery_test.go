package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDeadInstancesTrimKeepsARunningRunAcceptedInstance asserts the trim
// keeps the dead_instances entry a RUNNING run names through its own
// accepted_instance payload value. The sweep resolves the owning instance
// of a run with no intent cell instance from that value, so the keep set
// reads the same source. Without it the trim drops the only evidence the
// sweep can mark that run with, and the run stays RUNNING for good.
func TestDeadInstancesTrimKeepsARunningRunAcceptedInstance(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	runID := newTestUUIDv7String()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs
			(run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status, payload)
		VALUES ($1, now(), $2, $3, $4, 'cell-a', $5, $6, 'RUNNING', '{"accepted_instance": "pod-referenced"}'::jsonb)
	`, runID, "manual:"+runID, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert running run: %v", err)
	}

	base := time.Now().UTC()
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current",
		DeadInstances: []TenantSyncDeadInstance{{Instance: "pod-referenced", LastSeenAt: base.Add(-time.Hour)}},
	})

	entries := make([]TenantSyncDeadInstance, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, TenantSyncDeadInstance{
			Instance:   "pod-extra-" + string(rune('a'+i)),
			LastSeenAt: base.Add(-time.Duration(i) * time.Minute),
		})
	}
	sendTenantSync(t, pool, conn, TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: 1, CellInstance: "pod-current", DeadInstances: entries,
	})

	stored := readTenantPlacement(t, db, testTenant)
	for _, d := range stored.deadInstances {
		if d.Instance == "pod-referenced" {
			return
		}
	}
	t.Fatalf("want pod-referenced kept because a RUNNING run names it as its accepted instance, got %v", stored.deadInstances)
}

// TestAnnounceMarksARunWhoseKeptDeadInstanceSurvivedTheTrim is the
// end-to-end half of the trim rule: a RUNNING run whose owning instance
// the trim dropped can never be marked again, because the sweep and the
// announce read that instance from dead_instances alone.
//
// The fixture moves heartbeat_received_at with last_heartbeat_at. The
// liveness reading measures the age of the recorder's own receive stamp,
// so a fixture that ages the pod stamp alone leaves the cell live.
func TestAnnounceMarksARunWhoseKeptDeadInstanceSurvivedTheTrim(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	runID := newTestUUIDv7String()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs
			(run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status,
			 last_heartbeat_at, heartbeat_received_at, payload)
		VALUES ($1, now(), $2, $3, $4, 'cell-a', $5, $6, 'RUNNING', now(), now(),
			'{"accepted_instance": "pod-referenced"}'::jsonb)
	`, runID, "manual:"+runID, testCluster, testNamespace, testTenant, testJobPath); err != nil {
		t.Fatalf("insert running run: %v", err)
	}

	base := time.Now().UTC()
	announce := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
	}
	first := announce
	first.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-referenced", LastSeenAt: base.Add(-time.Hour)}}
	if marked := sendTenantSync(t, pool, conn, first).InterruptedMarked; marked != 0 {
		t.Fatalf("a run whose heartbeat is fresh was marked %d times, want 0", marked)
	}

	entries := make([]TenantSyncDeadInstance, 0, 12)
	for i := 0; i < 12; i++ {
		entries = append(entries, TenantSyncDeadInstance{
			Instance:   "pod-extra-" + string(rune('a'+i)),
			LastSeenAt: base.Add(-time.Duration(i) * time.Minute),
		})
	}
	trimming := announce
	trimming.DeadInstances = entries
	sendTenantSync(t, pool, conn, trimming)

	if _, err := db.Exec(`
		UPDATE varroa.runs
		SET last_heartbeat_at = now() - interval '20 minutes',
		    heartbeat_received_at = now() - interval '20 minutes'
		WHERE run_id = $1
	`, runID); err != nil {
		t.Fatalf("age the heartbeat: %v", err)
	}

	rolled := announce
	rolled.CellInstance = "pod-2"
	if marked := sendTenantSync(t, pool, conn, rolled).InterruptedMarked; marked != 1 {
		t.Fatalf("runs marked after the cell rolled = %d, want 1", marked)
	}

	var status, result string
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &result); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if status != "COMPLETED" || result != "INTERRUPTED" {
		t.Fatalf("run state = %s/%s, want COMPLETED/INTERRUPTED", status, result)
	}
}

// TestTenantSyncAnnounceKeepsAnAppliedSeedOnAnEmptyReport asserts an
// announcement that reports no rehydrated instance never clears a stored
// one at the same generation and the same cell instance. The operator can
// read its own rehydration state one tick late, and an empty report would
// otherwise fence every build of the tenant until the next resync.
func TestTenantSyncAnnounceKeepsAnAppliedSeedOnAnEmptyReport(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	base := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
	}
	sendTenantSync(t, pool, conn, base)

	seeded := base
	seeded.RehydratedInstance = "pod-1"
	sendTenantSync(t, pool, conn, seeded)
	if got := readTenantPlacement(t, db, testTenant).rehydratedInstance; got != "pod-1" {
		t.Fatalf("rehydrated_instance after the seeded announce = %q, want pod-1", got)
	}

	sendTenantSync(t, pool, conn, base)
	if got := readTenantPlacement(t, db, testTenant).rehydratedInstance; got != "pod-1" {
		t.Fatalf("rehydrated_instance after an empty report = %q, want pod-1 kept", got)
	}
}

// TestTenantSyncAnnounceKeepsADeferredJobWhileItKeepsTheSeed asserts the
// two seed signals stay independent. rehydrated_instance answers whether
// the cell holds a seed for this instance; pending_jobs answers which
// single jobs are still deferred on it. A cell reports an applied bundle
// and a deferred job together, so keeping the instance never clears the
// job list, and the job keeps its own hold.
func TestTenantSyncAnnounceKeepsADeferredJobWhileItKeepsTheSeed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	base := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
	}
	sendTenantSync(t, pool, conn, base)

	seeded := base
	seeded.RehydratedInstance = "pod-1"
	sendTenantSync(t, pool, conn, seeded)

	deferredReport := base
	deferredReport.PendingJobs = []string{testJobPath}
	sendTenantSync(t, pool, conn, deferredReport)

	stored := readTenantPlacement(t, db, testTenant)
	if stored.rehydratedInstance != "pod-1" {
		t.Fatalf("rehydrated_instance = %q, want pod-1 kept", stored.rehydratedInstance)
	}
	if len(stored.pendingJobs) != 1 || stored.pendingJobs[0] != testJobPath {
		t.Fatalf("pending_jobs = %v, want the deferred job kept", stored.pendingJobs)
	}
}

// TestTenantSyncAnnounceKeepsADeferredMemberJobThroughAResend asserts the
// pre-send window never un-fences a deferred job. The cell clears its own
// deferred list and reports the bundle not applied at the moment it
// starts a fresh bundle for a job it already deferred. That announcement
// carries no seed and no deferred job, so the stored job list must
// survive it, or the job passes both seed orders with no record on the
// cell.
func TestTenantSyncAnnounceKeepsADeferredMemberJobThroughAResend(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	slot := int32(0)
	base := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
		Members: []member{{
			Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive,
			Slot: &slot, Instance: "pod-1",
		}},
	}
	sendTenantSync(t, pool, conn, base)

	applied := base
	applied.Members = []member{{
		Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive,
		Slot: &slot, Instance: "pod-1", RehydratedInstance: "pod-1",
		PendingJobs: []string{testJobPath},
	}}
	sendTenantSync(t, pool, conn, applied)

	// The re-send window: not applied, and no deferred job reported.
	sendTenantSync(t, pool, conn, base)

	row, exists, err := readTenantPlacementMembers(t.Context(), pool, testCluster, testTenant)
	if err != nil || !exists {
		t.Fatalf("read members: exists=%v err=%v", exists, err)
	}
	if len(row.members) != 1 {
		t.Fatalf("members = %v, want one", row.members)
	}
	got := row.members[0]
	if got.RehydratedInstance != "pod-1" {
		t.Fatalf("member seed = %q, want pod-1 kept", got.RehydratedInstance)
	}
	if len(got.PendingJobs) != 1 || got.PendingJobs[0] != testJobPath {
		t.Fatalf("member pending jobs = %v, want the deferred job kept", got.PendingJobs)
	}
}

// TestTenantSyncAnnounceTakesTheJobListOfAnAppliedReport asserts the
// union above never holds a job past the seed report that drops it. An
// announcement that reports a seed of its own replaces the whole list.
func TestTenantSyncAnnounceTakesTheJobListOfAnAppliedReport(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	base := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
	}
	sendTenantSync(t, pool, conn, base)

	deferredReport := base
	deferredReport.PendingJobs = []string{testJobPath}
	sendTenantSync(t, pool, conn, deferredReport)

	applied := base
	applied.RehydratedInstance = "pod-1"
	sendTenantSync(t, pool, conn, applied)

	stored := readTenantPlacement(t, db, testTenant)
	if stored.rehydratedInstance != "pod-1" {
		t.Fatalf("rehydrated_instance = %q, want pod-1", stored.rehydratedInstance)
	}
	if len(stored.pendingJobs) != 0 {
		t.Fatalf("pending_jobs = %v, want the applied report's own empty list", stored.pendingJobs)
	}
}

// TestTenantSyncAnnounceClearsTheSeedOnAnInstanceChange asserts the guard
// above never holds a seed past the cell instance it belongs to. The new
// pod holds no seed, so the row clears.
func TestTenantSyncAnnounceClearsTheSeedOnAnInstanceChange(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	base := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
	}
	sendTenantSync(t, pool, conn, base)
	seeded := base
	seeded.RehydratedInstance = "pod-1"
	sendTenantSync(t, pool, conn, seeded)

	rolled := base
	rolled.CellInstance = "pod-2"
	sendTenantSync(t, pool, conn, rolled)
	if got := readTenantPlacement(t, db, testTenant).rehydratedInstance; got != "" {
		t.Fatalf("rehydrated_instance after a pod roll = %q, want it cleared", got)
	}
}

// TestTenantSyncAnnounceKeepsAnAppliedMemberSeedOnAnEmptyReport is the
// per-member half of the same rule: a member whose instance is unchanged
// keeps the seed it already reported when the next announcement reports
// none.
func TestTenantSyncAnnounceKeepsAnAppliedMemberSeedOnAnEmptyReport(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	slot := int32(0)
	base := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Namespace: testNamespace, Cell: "cell-a", Generation: 1, CellInstance: "pod-1",
		Members: []member{{
			Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive,
			Slot: &slot, Instance: "pod-1",
		}},
	}
	sendTenantSync(t, pool, conn, base)

	seeded := base
	seeded.Members = []member{{
		Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive,
		Slot: &slot, Instance: "pod-1", RehydratedInstance: "pod-1",
	}}
	sendTenantSync(t, pool, conn, seeded)

	sendTenantSync(t, pool, conn, base)
	row, exists, err := readTenantPlacementMembers(t.Context(), pool, testCluster, testTenant)
	if err != nil || !exists {
		t.Fatalf("read members: exists=%v err=%v", exists, err)
	}
	if len(row.members) != 1 || row.members[0].RehydratedInstance != "pod-1" {
		t.Fatalf("member seed after an empty report = %v, want pod-1 kept", row.members)
	}
}

// seedStaleRunOn inserts a RUNNING run of testTenant on cell whose owning
// instance is instance and whose last heartbeat arrived 20 minutes ago.
func seedStaleRunOn(t *testing.T, db *sql.DB, cell, instance string) string {
	t.Helper()
	runID := newTestUUIDv7String()
	if _, err := db.Exec(`
		INSERT INTO varroa.runs
			(run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path, status,
			 last_heartbeat_at, heartbeat_received_at, payload)
		VALUES ($1, now(), $2, $3, $4, $5, $6, $7, 'RUNNING',
			now() - interval '20 minutes', now() - interval '20 minutes',
			jsonb_build_object('accepted_instance', $8::text))
	`, runID, "manual:"+runID, testCluster, testNamespace, cell, testTenant, testJobPath, instance); err != nil {
		t.Fatalf("insert running run: %v", err)
	}
	return runID
}

// sweepAfterMovedWindow runs one sweep round six minutes from now, past
// the moved window of an entry derived now.
func sweepAfterMovedWindow(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	conn, cleanup := newSweepTestBus(t)
	defer cleanup()
	freshenSweepConsumer(t, conn)
	if _, err := SweepInterruptedRuns(context.Background(), pool, conn, nil, nil, nil,
		DefaultSweepMaxPending, time.Now().Add(6*time.Minute), testGrace, nil); err != nil {
		t.Fatalf("SweepInterruptedRuns: %v", err)
	}
}

// assertRunInterrupted asserts runID ended INTERRUPTED.
func assertRunInterrupted(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	var status, result string
	if err := db.QueryRow(`SELECT status, result FROM varroa.runs WHERE run_id = $1`, runID).
		Scan(&status, &result); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if status != StatusCompleted || result != "INTERRUPTED" {
		t.Fatalf("run state = %s/%s, want COMPLETED/INTERRUPTED", status, result)
	}
}

// TestAnnounceMarksARunOnTheOlderOfTwoRolls asserts a cell that rolled
// two times while the recorder was down still ends the run of its first
// pod. The operator names only the newer replaced pod, and the recorder
// derives the older one from its own member list.
func TestAnnounceMarksARunOnTheOlderOfTwoRolls(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	runID := seedStaleRunOn(t, db, "cell-a", "pod-a1")

	sendTenantSync(t, pool, conn, announceMembers(1, []member{activeMember("cell-a", 0, "pod-a1")}))

	confirmed := time.Now()
	afterOutage := announceMembers(1, []member{activeMember("cell-a", 0, "pod-a3")})
	afterOutage.DeadInstances = []TenantSyncDeadInstance{{Instance: "pod-a2", LastSeenAt: confirmed, ConfirmedGoneAt: &confirmed}}
	if marked := sendTenantSync(t, pool, conn, afterOutage).InterruptedMarked; marked != 0 {
		t.Fatalf("a run was marked inside the moved window: %d", marked)
	}

	sweepAfterMovedWindow(t, pool)
	assertRunInterrupted(t, db, runID)
}

// TestAnnounceMarksARunOnADeletedCellWithNoOperatorEntry asserts a member
// cell that left the tenant with no operator entry still ends the run of
// its last pod.
func TestAnnounceMarksARunOnADeletedCellWithNoOperatorEntry(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	runID := seedStaleRunOn(t, db, "cell-b", "pod-b1")

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "pod-a1"), activeMember("cell-b", 1, "pod-b1"),
	}))
	sendTenantSync(t, pool, conn, announceMembers(2, []member{activeMember("cell-a", 0, "pod-a1")}))

	sweepAfterMovedWindow(t, pool)
	assertRunInterrupted(t, db, runID)
}

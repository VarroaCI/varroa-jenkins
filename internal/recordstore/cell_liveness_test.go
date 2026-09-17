package recordstore

import (
	"testing"
	"time"
)

// testGrace is the confirmed-gone grace every reading test applies.
const testGrace = 30 * time.Second

// resolveLivenessMembers rebuilds a hand-built row's member view from its
// own scalar columns, the way readTenantPlacementLivenessRow does after a
// read. A test that builds a row directly calls it, so the reading runs
// against the same member view production resolves.
func resolveLivenessMembers(row tenantPlacementLivenessRow) tenantPlacementLivenessRow {
	resolved, err := decodePlacementMembers(nil, nil, placementScalars{
		cell: row.cell, cellInstance: row.cellInstance,
	})
	if err != nil {
		panic(err)
	}
	row.members = resolved
	return row
}

// TestCellLivenessGoneMovedReading asserts an old instance listed dead in
// dead_instances, with a last_seen_at older than the moved window, counts
// as gone.
func TestCellLivenessGoneMovedReading(t *testing.T) {
	now := time.Now()
	row := tenantPlacementLivenessRow{
		cellInstance:  "pod-new",
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: now.Add(-6 * time.Minute)}},
	}
	anchor := now.Add(-6 * time.Minute)
	if got := cellLivenessGone(now, anchor, "pod-old", resolveLivenessMembers(row), true, testGrace); got != livenessMoved {
		t.Fatalf("reading = %d, want livenessMoved: the old instance is listed dead past the moved window", got)
	}
}

// TestCellLivenessGoneChangedInstanceAloneIsNotAReading asserts a changed
// cell instance that never appears in dead_instances answers not gone,
// no matter how old the anchor is.
func TestCellLivenessGoneChangedInstanceAloneIsNotAReading(t *testing.T) {
	now := time.Now()
	row := tenantPlacementLivenessRow{cellInstance: "pod-new"}
	anchor := now.Add(-20 * time.Minute)
	if got := cellLivenessGone(now, anchor, "pod-old", resolveLivenessMembers(row), true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: a changed placement alone is not a reading", got)
	}
}

// TestCellLivenessGoneFreshDeadEntryIsNotGoneYet asserts an instance
// listed dead with a last_seen_at inside the moved window is not gone
// yet.
func TestCellLivenessGoneFreshDeadEntryIsNotGoneYet(t *testing.T) {
	now := time.Now()
	row := tenantPlacementLivenessRow{
		cellInstance:  "pod-new",
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: now.Add(-10 * time.Second)}},
	}
	anchor := now.Add(-6 * time.Minute)
	if got := cellLivenessGone(now, anchor, "pod-old", resolveLivenessMembers(row), true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: the dead-instance entry is only ten seconds old", got)
	}
}

// TestCellLivenessGoneMissingRowAnswersFalse asserts no tenant_placements
// row gives no reading.
func TestCellLivenessGoneMissingRowAnswersFalse(t *testing.T) {
	now := time.Now()
	got := cellLivenessGone(now, now.Add(-time.Hour), "pod-old", tenantPlacementLivenessRow{}, false, testGrace)
	if got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: a missing placement row gives no reading", got)
	}
}

// TestCellLivenessGoneSilentReading asserts the current instance, with
// cell_alive false and a stale-enough cell_last_seen_at and a fresh
// cell_alive_at, counts as gone.
func TestCellLivenessGoneSilentReading(t *testing.T) {
	now := time.Now()
	lastSeen := now.Add(-20 * time.Minute)
	aliveAt := now.Add(-time.Minute)
	row := tenantPlacementLivenessRow{
		cellInstance:   "pod-a",
		cellAlive:      false,
		cellAliveAt:    &aliveAt,
		cellLastSeenAt: &lastSeen,
	}
	anchor := now.Add(-20 * time.Minute)
	if got := cellLivenessGone(now, anchor, "pod-a", resolveLivenessMembers(row), true, testGrace); got != livenessSilent {
		t.Fatalf("reading = %d, want livenessSilent: the current instance is not alive for the whole window", got)
	}
}

// TestCellLivenessGoneSilentReadingLiveCellIsNotGone asserts a current
// instance reported alive is never gone, however old the anchor is.
func TestCellLivenessGoneSilentReadingLiveCellIsNotGone(t *testing.T) {
	now := time.Now()
	aliveAt := now.Add(-time.Minute)
	row := tenantPlacementLivenessRow{cellInstance: "pod-a", cellAlive: true, cellAliveAt: &aliveAt}
	anchor := now.Add(-20 * time.Minute)
	if got := cellLivenessGone(now, anchor, "pod-a", resolveLivenessMembers(row), true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: cell_alive is true", got)
	}
}

// TestCellLivenessGoneStaleCellAliveAtAnswersFalse asserts a
// cell_alive_at older than twice the announce interval counts the whole
// reading as absent, even with an old cell_last_seen_at.
func TestCellLivenessGoneStaleCellAliveAtAnswersFalse(t *testing.T) {
	now := time.Now()
	lastSeen := now.Add(-time.Hour)
	staleAliveAt := now.Add(-cellAliveStaleAfter - time.Minute)
	row := tenantPlacementLivenessRow{
		cellInstance:   "pod-a",
		cellAlive:      false,
		cellAliveAt:    &staleAliveAt,
		cellLastSeenAt: &lastSeen,
	}
	anchor := now.Add(-time.Hour)
	if got := cellLivenessGone(now, anchor, "pod-a", resolveLivenessMembers(row), true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: a stale cell_alive_at counts as an absent signal", got)
	}
}

// TestCellLivenessGoneAnchorTooFreshHoldsOffTheSilentReading asserts a
// recent anchor never counts as gone, even with a confirmed-dead
// reading, because the silent window has not passed yet.
func TestCellLivenessGoneAnchorTooFreshHoldsOffTheSilentReading(t *testing.T) {
	now := time.Now()
	lastSeen := now.Add(-time.Hour)
	aliveAt := now.Add(-time.Minute)
	row := tenantPlacementLivenessRow{
		cellInstance:   "pod-a",
		cellAlive:      false,
		cellAliveAt:    &aliveAt,
		cellLastSeenAt: &lastSeen,
	}
	anchor := now.Add(-time.Minute)
	if got := cellLivenessGone(now, anchor, "pod-a", resolveLivenessMembers(row), true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: the run's own heartbeat is only a minute old", got)
	}
}

// TestCellLivenessGoneConfirmedReading asserts a pod-read confirmation
// older than the grace answers livenessConfirmedGone, with a heartbeat
// far younger than every other window. The confirmed test applies no
// anchor: a run's heartbeat age proves nothing once the pod is proved
// gone.
func TestCellLivenessGoneConfirmedReading(t *testing.T) {
	now := time.Now()
	confirmed := now.Add(-45 * time.Second)
	row := tenantPlacementLivenessRow{
		cellInstance: "pod-new",
		deadInstances: []TenantSyncDeadInstance{{
			Instance: "pod-old", LastSeenAt: now.Add(-40 * time.Second), ConfirmedGoneAt: &confirmed,
		}},
	}
	anchor := now.Add(-40 * time.Second)
	if got := cellLivenessGone(now, anchor, "pod-old", resolveLivenessMembers(row), true, testGrace); got != livenessConfirmedGone {
		t.Fatalf("reading = %d, want livenessConfirmedGone: the confirmation is older than the grace", got)
	}
}

// TestCellLivenessGoneFreshConfirmationWaitsOutTheGrace asserts a
// confirmation younger than the grace gives no reading yet.
func TestCellLivenessGoneFreshConfirmationWaitsOutTheGrace(t *testing.T) {
	now := time.Now()
	confirmed := now.Add(-5 * time.Second)
	row := tenantPlacementLivenessRow{
		cellInstance: "pod-new",
		deadInstances: []TenantSyncDeadInstance{{
			Instance: "pod-old", LastSeenAt: now.Add(-5 * time.Second), ConfirmedGoneAt: &confirmed,
		}},
	}
	anchor := now.Add(-5 * time.Second)
	if got := cellLivenessGone(now, anchor, "pod-old", resolveLivenessMembers(row), true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: the confirmation is five seconds old", got)
	}
}

// TestCellLivenessGoneNoConfirmationKeepsTheMovedWindow asserts an entry
// with no confirmation keeps the five-minute window exactly. The same
// entry one minute old gives no reading; six minutes old it answers
// livenessMoved.
func TestCellLivenessGoneNoConfirmationKeepsTheMovedWindow(t *testing.T) {
	now := time.Now()
	fresh := tenantPlacementLivenessRow{
		cellInstance:  "pod-new",
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: now.Add(-time.Minute)}},
	}
	if got := cellLivenessGone(now, now.Add(-time.Minute), "pod-old", resolveLivenessMembers(fresh), true, testGrace); got != livenessAlive {
		t.Fatalf("reading = %d, want livenessAlive: the entry is one minute old and carries no confirmation", got)
	}

	aged := tenantPlacementLivenessRow{
		cellInstance:  "pod-new",
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: now.Add(-6 * time.Minute)}},
	}
	if got := cellLivenessGone(now, now.Add(-6*time.Minute), "pod-old", resolveLivenessMembers(aged), true, testGrace); got != livenessMoved {
		t.Fatalf("reading = %d, want livenessMoved: the entry is six minutes old", got)
	}
}

// TestReadTenantPlacementLivenessRowCarriesConfirmation asserts the
// liveness row decodes ConfirmedGoneAt from the stored dead_instances
// jsonb, and answers nil for an entry that holds none. The field lives
// inside the existing column, so this needs no migration.
func TestReadTenantPlacementLivenessRowCarriesConfirmation(t *testing.T) {
	pool, db := setupLedgerPool(t)
	confirmed := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements (cluster, tenant, namespace, cell, generation, cell_instance,
			policy, rehydrated_instance, pending_jobs, cell_alive, dead_instances, announced_at)
		VALUES ($1, $2, $3, 'cell-a', 1, 'pod-new', '{}'::jsonb, '', '[]'::jsonb, false, $4::jsonb, now())
	`, testCluster, testTenant, testNamespace, `[
		{"instance":"pod-confirmed","last_seen_at":"`+confirmed.Format(time.RFC3339)+`","confirmed_gone_at":"`+
		confirmed.Format(time.RFC3339)+`"},
		{"instance":"pod-plain","last_seen_at":"`+confirmed.Format(time.RFC3339)+`"}
	]`); err != nil {
		t.Fatalf("insert tenant_placements row: %v", err)
	}

	row, exists, err := readTenantPlacementLivenessRow(t.Context(), pool, testCluster, testTenant)
	if err != nil {
		t.Fatalf("read liveness row: %v", err)
	}
	if !exists {
		t.Fatal("want the placement row to exist")
	}
	byInstance := map[string]TenantSyncDeadInstance{}
	for _, d := range row.deadInstances {
		byInstance[d.Instance] = d
	}
	got, ok := byInstance["pod-confirmed"]
	if !ok {
		t.Fatalf("want an entry for pod-confirmed, got %v", row.deadInstances)
	}
	if got.ConfirmedGoneAt == nil || !got.ConfirmedGoneAt.Equal(confirmed) {
		t.Fatalf("pod-confirmed ConfirmedGoneAt = %v, want %v", got.ConfirmedGoneAt, confirmed)
	}
	plain, ok := byInstance["pod-plain"]
	if !ok {
		t.Fatalf("want an entry for pod-plain, got %v", row.deadInstances)
	}
	if plain.ConfirmedGoneAt != nil {
		t.Fatalf("pod-plain ConfirmedGoneAt = %v, want nil", plain.ConfirmedGoneAt)
	}
}

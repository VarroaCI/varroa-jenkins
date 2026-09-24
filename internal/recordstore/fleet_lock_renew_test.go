package recordstore

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// readLockGeneration reads one fleet_locks row's stored generation.
func readLockGeneration(t *testing.T, db *sql.DB, name, runID string) int64 {
	t.Helper()
	var generation int64
	if err := db.QueryRow(`
		SELECT placement_generation FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2
	`, name, runID).Scan(&generation); err != nil {
		t.Fatalf("read the lock generation: %v", err)
	}
	return generation
}

// TestFleetLockRenew_GenerationRaiseKeepsALiveHold asserts a build that
// holds a lock across a slot takeover keeps it. The tenant generation
// rose after the acquire, and the renew still extends the lease and
// raises the lock's stored generation.
//
// A generation test would refuse this renew, and the build would lose a
// lock it still holds.
func TestFleetLockRenew_GenerationRaiseKeepsALiveHold(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 6, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET placement_generation = 3 WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1"); err != nil {
		t.Fatalf("stamp the acquire generation: %v", err)
	}

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-alpha",
		CallerTenant:        "alpha",
		PlacementGeneration: 6,
		Name:                "deploy-prod",
		RunID:               "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want an extended lease (message %q)", resp.ReasonCode, resp.Message)
	}
	if resp.ExpiresAt == nil {
		t.Fatal("the renew answered no lease deadline")
	}
	if got := readLockGeneration(t, db, "alpha/deploy-prod", "run-1"); got != 6 {
		t.Fatalf("the stored generation is %d, want 6: a successful renew raises it", got)
	}
}

// TestFleetLockRenew_LowerCallerGenerationRefusesNothing asserts a caller
// whose resolver cache is behind still renews. The generation carries no
// identity the lock's own cell_instance does not already carry.
func TestFleetLockRenew_LowerCallerGenerationRefusesNothing(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 5, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))
	if _, err := db.Exec(`UPDATE varroa.fleet_locks SET placement_generation = 5 WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1"); err != nil {
		t.Fatalf("stamp the acquire generation: %v", err)
	}

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-alpha",
		CallerTenant:        "alpha",
		PlacementGeneration: 5,
		Name:                "deploy-prod",
		RunID:               "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want an extended lease", resp.ReasonCode)
	}
	if got := readLockGeneration(t, db, "alpha/deploy-prod", "run-1"); got != 5 {
		t.Fatalf("the stored generation is %d, want 5: a renew never lowers it", got)
	}
}

// TestFleetLockRenew_MovedTenantIsStillFenced asserts the placement
// orders still run first: a renew from a cell the tenant holds no member
// on answers the permanent FENCED, and the lease lapses.
func TestFleetLockRenew_MovedTenantIsStillFenced(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 5, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:             testCluster,
		Namespace:           "ns-alpha",
		Cell:                "cell-old",
		CallerTenant:        "alpha",
		PlacementGeneration: 5,
		Name:                "deploy-prod",
		RunID:               "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != ReasonFenced {
		t.Fatalf("reason_code: got %q, want %q", resp.ReasonCode, ReasonFenced)
	}
}

// TestFleetLockRenew_RevokedGrantAnswersCrossTenant covers task 3.10's
// second acceptance: a renew after the owning tenant revoked the
// cross-prefix grant is refused.
func TestFleetLockRenew_RevokedGrantAnswersCrossTenant(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)
	// beta exports nothing: the grant alpha once held is gone.
	seedTenantPlacement(t, db, "beta", "ns-beta", "cell-beta", 0, "pod-b", "pod-b", nil)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "beta/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		TargetTenant: "beta",
		Name:         "deploy-prod",
		RunID:        "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != ReasonCrossTenant {
		t.Fatalf("reason_code: got %q, want %q (message %q)", resp.ReasonCode, ReasonCrossTenant, resp.Message)
	}
}

// TestFleetLockRenew_NoMatchingRowAnswersLost covers task 3.10's third
// acceptance.
func TestFleetLockRenew_NoMatchingRowAnswersLost(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         "deploy-prod",
		RunID:        "run-gone",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.State != FleetLockStateLost {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateLost)
	}
}

// TestFleetLockRelease_IsIdempotent covers task 3.10's fourth
// acceptance: a release answers released even when no row remains, and
// a repeated release changes nothing.
func TestFleetLockRelease_IsIdempotent(t *testing.T) {
	deps, db := lockTestDeps(t)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "held", now.Add(-time.Minute), now.Add(5*time.Minute))

	first := handleCrossTenantLockReleaseForTest(t, deps, "run-1")
	if first.State != FleetLockStateReleased {
		t.Fatalf("first release state: got %q, want %q", first.State, FleetLockStateReleased)
	}

	second := handleCrossTenantLockReleaseForTest(t, deps, "run-1")
	if second.State != FleetLockStateReleased {
		t.Fatalf("second release state: got %q, want %q", second.State, FleetLockStateReleased)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"alpha/deploy-prod", "run-1").Scan(&count); err != nil {
		t.Fatalf("count fleet_locks: %v", err)
	}
	if count != 0 {
		t.Fatalf("fleet_locks rows: got %d, want 0", count)
	}
}

// TestFleetLockRenew_ReleasingRowExtendsTheLease covers task 3.10's
// fifth acceptance: a renew of a releasing row succeeds, keeps its
// state, and extends the lease.
func TestFleetLockRenew_ReleasingRowExtendsTheLease(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	now := time.Now().UTC()
	originalExpiry := now.Add(30 * time.Second)
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", "releasing", now.Add(-time.Minute), originalExpiry)

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         "deploy-prod",
		RunID:        "run-1",
		LeaseSeconds: 300,
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.State != FleetLockStateReleasing {
		t.Fatalf("state: got %q, want %q", resp.State, FleetLockStateReleasing)
	}
	if resp.ExpiresAt == nil || !resp.ExpiresAt.After(originalExpiry) {
		t.Fatalf("expires_at: got %v, want an extension past %v", resp.ExpiresAt, originalExpiry)
	}
}

// TestFleetLockRelease_PublishesOnceAndNoOpPublishesNothing asserts an
// explicit release of an existing row publishes one released
// notification, and a second release, which finds no row, publishes
// nothing.
func TestFleetLockRelease_PublishesOnceAndNoOpPublishesNothing(t *testing.T) {
	deps, db := lockTestDeps(t)

	now := time.Now().UTC()
	insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", FleetLockStateHeld,
		now.Add(-time.Minute), now.Add(5*time.Minute))

	notifier, sent, _ := newCaptureNotifier(t)
	defer notifier.Close()
	deps.LockNotifier = notifier

	if resp := handleCrossTenantLockReleaseForTest(t, deps, "run-1"); resp.State != FleetLockStateReleased {
		t.Fatalf("release state: got %q, want %q", resp.State, FleetLockStateReleased)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications for the release, want 1", got)
	}
	got := sent.payload(0)
	if got.Kind != "lock" || got.State != FleetLockStateReleased {
		t.Fatalf("notification = kind %q state %q, want lock/released", got.Kind, got.State)
	}
	if got.LockName != "alpha/deploy-prod" || got.RunID != "run-1" || got.JobPath != "alpha/release" {
		t.Fatalf("notification names %s/%s/%s, want the deleted row's own values",
			got.LockName, got.RunID, got.JobPath)
	}

	if resp := handleCrossTenantLockReleaseForTest(t, deps, "run-1"); resp.State != FleetLockStateReleased {
		t.Fatalf("second release state: got %q, want %q", resp.State, FleetLockStateReleased)
	}
	if got := sent.count(); got != 1 {
		t.Fatalf("published %d notifications after a no-op release, want 1", got)
	}
}

// TestFleetLockRenew_PublishesNothing asserts a renew publishes nothing,
// whatever it answers: it changes only values no reader renders.
func TestFleetLockRenew_PublishesNothing(t *testing.T) {
	cases := []struct {
		name  string
		state string
	}{
		{name: "a held row", state: FleetLockStateHeld},
		{name: "a releasing row", state: FleetLockStateReleasing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, db := lockTestDeps(t)
			seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

			now := time.Now().UTC()
			insertFleetLockRow(t, db, "alpha/deploy-prod", "run-1", tc.state,
				now.Add(-time.Minute), now.Add(5*time.Minute))

			notifier, sent, _ := newCaptureNotifier(t)
			defer notifier.Close()
			deps.LockNotifier = notifier

			resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
				Cluster:      testCluster,
				Namespace:    "ns-alpha",
				Cell:         "cell-alpha",
				CallerTenant: "alpha",
				Name:         "deploy-prod",
				RunID:        "run-1",
				LeaseSeconds: 300,
			})
			if err != nil {
				t.Fatalf("handleCrossTenantLockRenew: %v", err)
			}
			if resp.State != tc.state {
				t.Fatalf("state: got %q, want %q", resp.State, tc.state)
			}
			notifier.flush("run-1")
			if got := sent.count(); got != 0 {
				t.Fatalf("a renew published %d notifications, want 0", got)
			}
		})
	}
}

// TestFleetLockRenew_ControllerTenantRenewsItsOwnLock asserts a non-cell
// controller renews its own lock with no grant and no target tenant.
func TestFleetLockRenew_ControllerTenantRenewsItsOwnLock(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, controllerTenant, "team-pay", "ci", 0, "ci-0", "ci-0", nil)

	req := CrossTenantRequest{
		Cluster:       testCluster,
		Namespace:     "team-pay",
		Cell:          "ci",
		CallerTenant:  controllerTenant,
		CallerJobPath: "deploy",
		Name:          "deploy-prod",
		RunID:         "run-1",
	}
	acquired, err := handleCrossTenantLockAcquire(context.Background(), deps, req)
	if err != nil || acquired.State != FleetLockStateHeld {
		t.Fatalf("acquire: state %q, reason %q, err %v", acquired.State, acquired.ReasonCode, err)
	}

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != "" {
		t.Fatalf("reason_code: got %q, want no veto (message %q)", resp.ReasonCode, resp.Message)
	}
	if resp.State != FleetLockStateHeld || resp.FenceToken != acquired.FenceToken {
		t.Fatalf("renew: state %q token %d, want held with token %d", resp.State, resp.FenceToken, acquired.FenceToken)
	}
}

// TestFleetLockRenew_SlashInNameAnswersInvalidName asserts a renew whose
// name holds a "/" is vetoed with INVALID_NAME.
func TestFleetLockRenew_SlashInNameAnswersInvalidName(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "alpha", "ns-alpha", "cell-alpha", 0, "pod-a", "pod-a", nil)

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-alpha",
		Cell:         "cell-alpha",
		CallerTenant: "alpha",
		Name:         "alpha/deploy-prod",
		RunID:        "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.ReasonCode != ReasonInvalidName {
		t.Fatalf("reason_code: got %q, want %q", resp.ReasonCode, ReasonInvalidName)
	}
}

// TestFleetLockRenew_OtherTenantRunIDAnswersLost asserts a renew that
// names another tenant's run id reads no fence token and extends no
// lease: the row belongs to another tenant, so the caller gets lost.
func TestFleetLockRenew_OtherTenantRunIDAnswersLost(t *testing.T) {
	deps, db := lockTestDeps(t)
	seedTenantPlacement(t, db, "beta", "ns-beta", "cell-beta", 0, "pod-b", "pod-b", nil)

	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	insertFleetLockRow(t, db, "beta/deploy-prod", "run-1", "held", now.Add(-time.Minute), expires)

	resp, err := handleCrossTenantLockRenew(context.Background(), deps, CrossTenantRequest{
		Cluster:      testCluster,
		Namespace:    "ns-beta",
		Cell:         "cell-beta",
		CallerTenant: "beta",
		Name:         "deploy-prod",
		RunID:        "run-1",
	})
	if err != nil {
		t.Fatalf("handleCrossTenantLockRenew: %v", err)
	}
	if resp.State != FleetLockStateLost || resp.FenceToken != 0 {
		t.Fatalf("renew: state %q token %d, want lost with no token", resp.State, resp.FenceToken)
	}
	var got time.Time
	if err := db.QueryRow(`SELECT expires_at FROM varroa.fleet_locks WHERE name = $1 AND run_id = $2`,
		"beta/deploy-prod", "run-1").Scan(&got); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if !got.Equal(expires.Truncate(time.Microsecond)) {
		t.Fatalf("expires_at moved from %s to %s", expires, got)
	}
}

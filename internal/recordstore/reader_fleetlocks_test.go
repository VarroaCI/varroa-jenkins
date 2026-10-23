package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// readerLockClock is the fixed instant the lock-read tests stamp from, so
// one case reads the same order as another.
var readerLockClock = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

// seedReaderLock inserts one fleet_locks row directly, with full control
// over the identity columns the two lock reads filter on. The owner
// tenant is the name's first segment, which holds for every cell tenant
// name this file uses; seedReaderLockOwned names any other owner.
func seedReaderLock(t *testing.T, db *sql.DB, name, runID, cluster, tenant, jobPath, state string, waitingSince time.Time) {
	t.Helper()
	owner, _, _ := strings.Cut(name, "/")
	seedReaderLockOwned(t, db, name, owner, runID, cluster, tenant, jobPath, state, waitingSince)
}

// seedReaderLockOwned is seedReaderLock with an explicit owner tenant.
func seedReaderLockOwned(t *testing.T, db *sql.DB, name, owner, runID, cluster, tenant, jobPath, state string, waitingSince time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO varroa.fleet_locks (name, owner_tenant, run_id, cluster, tenant, job_path, state, waiting_since, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, name, owner, runID, cluster, tenant, jobPath, state, waitingSince, waitingSince.Add(time.Hour)); err != nil {
		t.Fatalf("seed fleet lock %s/%s: %v", name, runID, err)
	}
}

// seedLockReadFixture inserts the rows both read tests share: three rows
// of alpha's own prefix, one row of beta's prefix that alpha's own run
// holds, one row of beta's own prefix that beta holds, and one row of
// another cluster.
func seedLockReadFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	seedReaderLock(t, db, "alpha/deploy-prod", "run-alpha-held", testCluster, "alpha", "alpha/deploy-prod", "held", readerLockClock)
	seedReaderLock(t, db, "alpha/deploy-prod", "run-alpha-wait-1", testCluster, "alpha", "alpha/deploy-prod", "waiting", readerLockClock.Add(time.Minute))
	seedReaderLock(t, db, "alpha/deploy-prod", "run-alpha-wait-2", testCluster, "alpha", "alpha/deploy-prod", "waiting", readerLockClock.Add(2*time.Minute))
	seedReaderLock(t, db, "beta/deploy-prod", "run-alpha-under-beta", testCluster, "alpha", "beta/deploy-prod", "waiting", readerLockClock.Add(3*time.Minute))
	seedReaderLock(t, db, "beta/deploy-prod", "run-beta-held", testCluster, "beta", "beta/deploy-prod", "held", readerLockClock.Add(4*time.Minute))
	seedReaderLock(t, db, "alpha/deploy-prod", "run-other-cluster", "other", "alpha", "alpha/deploy-prod", "waiting", readerLockClock.Add(5*time.Minute))
}

// TestReaderListFleetLocks_TenantScope asserts the tenant read answers
// the locks alpha owns, answers the lock alpha's own run holds under
// beta's prefix, withholds beta's own row, withholds another cluster's
// row, and orders a holder before its waiters.
func TestReaderListFleetLocks_TenantScope(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedLockReadFixture(t, db)

	locks, err := NewReader(pool).ListFleetLocks(context.Background(), testCluster, "alpha")
	if err != nil {
		t.Fatalf("ListFleetLocks: %v", err)
	}

	want := []struct {
		name, runID, tenant, jobPath, state string
	}{
		{"alpha/deploy-prod", "run-alpha-held", "alpha", "alpha/deploy-prod", "held"},
		{"alpha/deploy-prod", "run-alpha-wait-1", "alpha", "alpha/deploy-prod", "waiting"},
		{"alpha/deploy-prod", "run-alpha-wait-2", "alpha", "alpha/deploy-prod", "waiting"},
		{"beta/deploy-prod", "run-alpha-under-beta", "alpha", "beta/deploy-prod", "waiting"},
	}
	if len(locks) != len(want) {
		t.Fatalf("ListFleetLocks answered %d rows, want %d: %+v", len(locks), len(want), locks)
	}
	for i, tc := range want {
		got := locks[i]
		if got.Name != tc.name || got.RunID != tc.runID || got.Tenant != tc.tenant ||
			got.JobPath != tc.jobPath || got.State != tc.state {
			t.Errorf("row %d = %s/%s/%s/%s/%s, want %s/%s/%s/%s/%s", i,
				got.Name, got.RunID, got.Tenant, got.JobPath, got.State,
				tc.name, tc.runID, tc.tenant, tc.jobPath, tc.state)
		}
		if got.Cluster != testCluster {
			t.Errorf("row %d cluster = %q, want %q", i, got.Cluster, testCluster)
		}
		if !got.WaitingSince.Equal(readerLockClock.Add(time.Duration(i) * time.Minute)) {
			t.Errorf("row %d waitingSince = %s, want the seeded instant", i, got.WaitingSince)
		}
	}
	if locks[0].State != "held" {
		t.Errorf("first row of alpha/deploy-prod = %q, want the holder first", locks[0].State)
	}
	if locks[3].State != "waiting" {
		t.Errorf("last row = %q, want alpha's own waiting row under beta's prefix", locks[3].State)
	}
}

// TestReaderListAllFleetLocks_EveryCluster asserts the fleet-wide read
// answers every tenant's rows of every cluster as one ordered list, and
// each row carries its own cluster.
func TestReaderListAllFleetLocks_EveryCluster(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedLockReadFixture(t, db)

	locks, err := NewReader(pool).ListAllFleetLocks(context.Background())
	if err != nil {
		t.Fatalf("ListAllFleetLocks: %v", err)
	}
	want := []struct {
		name, runID, cluster string
	}{
		{"alpha/deploy-prod", "run-alpha-held", testCluster},
		{"alpha/deploy-prod", "run-alpha-wait-1", testCluster},
		{"alpha/deploy-prod", "run-alpha-wait-2", testCluster},
		{"alpha/deploy-prod", "run-other-cluster", "other"},
		{"beta/deploy-prod", "run-alpha-under-beta", testCluster},
		{"beta/deploy-prod", "run-beta-held", testCluster},
	}
	if len(locks) != len(want) {
		t.Fatalf("ListAllFleetLocks answered %d rows, want %d: %+v", len(locks), len(want), locks)
	}
	for i, tc := range want {
		got := locks[i]
		if got.Name != tc.name || got.RunID != tc.runID || got.Cluster != tc.cluster {
			t.Errorf("row %d = %s/%s@%s, want %s/%s@%s", i,
				got.Name, got.RunID, got.Cluster, tc.name, tc.runID, tc.cluster)
		}
	}
}

// TestReaderListFleetLocks_RowCarriesNoQueuePosition asserts both reads
// answer the row's own state and its own waitingSince, and answer no
// rank, position, or holder field.
func TestReaderListFleetLocks_RowCarriesNoQueuePosition(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedReaderLock(t, db, "alpha/deploy-prod", "run-alpha-held", testCluster, "alpha", "alpha/deploy-prod", "held", readerLockClock)
	seedReaderLock(t, db, "alpha/deploy-prod", "run-alpha-wait-1", testCluster, "alpha", "alpha/deploy-prod", "waiting", readerLockClock.Add(time.Minute))
	seedReaderLock(t, db, "alpha/deploy-prod", "run-alpha-wait-2", testCluster, "alpha", "alpha/deploy-prod", "waiting", readerLockClock.Add(2*time.Minute))

	wantKeys := map[string]bool{
		"name":         true,
		"runId":        true,
		"cluster":      true,
		"tenant":       true,
		"jobPath":      true,
		"state":        true,
		"fenceToken":   true,
		"waitingSince": true,
		"acquiredAt":   true,
		"expiresAt":    true,
	}

	for _, read := range []struct {
		name string
		run  func() ([]FleetLock, error)
	}{
		{"ListFleetLocks", func() ([]FleetLock, error) {
			return NewReader(pool).ListFleetLocks(context.Background(), testCluster, "alpha")
		}},
		{"ListAllFleetLocks", func() ([]FleetLock, error) {
			return NewReader(pool).ListAllFleetLocks(context.Background())
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			locks, err := read.run()
			if err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			if len(locks) != 3 {
				t.Fatalf("%s answered %d rows, want 3", read.name, len(locks))
			}
			raw, err := json.Marshal(locks)
			if err != nil {
				t.Fatalf("marshal rows: %v", err)
			}
			var body []map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode rows: %v", err)
			}
			for i, row := range body {
				for key := range row {
					if !wantKeys[key] {
						t.Errorf("row %d carries the field %q, which no lock read answers", i, key)
					}
				}
				if row["state"] != locks[i].State {
					t.Errorf("row %d state = %v, want the row's own %q", i, row["state"], locks[i].State)
				}
				if _, present := row["waitingSince"]; !present {
					t.Errorf("row %d carries no waitingSince", i)
				}
			}
		})
	}
}

// TestReaderListFleetLocks_OwnerIsExactNotPrefix asserts the tenant read
// matches the owner tenant exactly. Tenant team-pay does not read a lock
// the controller tenant team-pay/ci owns, and a "_" in a tenant matches
// no other character.
func TestReaderListFleetLocks_OwnerIsExactNotPrefix(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	seedReaderLockOwned(t, db, "team-pay/deploy-prod", "team-pay", "run-pay", testCluster,
		"team-pay", "team-pay/deploy", "held", readerLockClock)
	seedReaderLockOwned(t, db, "team-pay/ci/deploy-prod", "team-pay/ci", "run-ci", testCluster,
		"team-pay/ci", "deploy", "held", readerLockClock.Add(time.Minute))
	seedReaderLockOwned(t, db, "tenantXpay/deploy-prod", "tenantXpay", "run-x", testCluster,
		"tenantXpay", "tenantXpay/deploy", "held", readerLockClock.Add(2*time.Minute))

	cases := []struct {
		tenant string
		want   []string
	}{
		{"team-pay", []string{"run-pay"}},
		{"team-pay/ci", []string{"run-ci"}},
		{"tenant_pay", nil},
	}
	for _, tc := range cases {
		t.Run(tc.tenant, func(t *testing.T) {
			locks, err := NewReader(pool).ListFleetLocks(context.Background(), testCluster, tc.tenant)
			if err != nil {
				t.Fatalf("ListFleetLocks: %v", err)
			}
			var got []string
			for _, lock := range locks {
				got = append(got, lock.RunID)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ListFleetLocks(%q) = %v, want %v", tc.tenant, got, tc.want)
			}
		})
	}
}

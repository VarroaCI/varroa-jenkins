package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// setPlacementMembers overwrites one stored placement row's member list
// and home map. The row's scalar columns do not change.
func setPlacementMembers(t *testing.T, db *sql.DB, tenant string, members []member, jobHomes map[string]int32) {
	t.Helper()
	membersJSON, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	homesJSON, err := json.Marshal(nonNilJobHomes(jobHomes))
	if err != nil {
		t.Fatalf("marshal job homes: %v", err)
	}
	res, err := db.Exec(`
		UPDATE varroa.tenant_placements SET members = $3::jsonb, job_homes = $4::jsonb
		WHERE cluster = $1 AND tenant = $2
	`, testCluster, tenant, string(membersJSON), string(homesJSON))
	if err != nil {
		t.Fatalf("set placement members for %s: %v", tenant, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("set placement members for %s: %d rows, want 1", tenant, n)
	}
}

// TestResolveJobHome covers the four answers of the home read: no row,
// an empty member list, a two-slot home map, and an unbound slot.
func TestResolveJobHome(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()

	got, err := resolveJobHome(ctx, pool, testCluster, "nobody", "nobody/job")
	if err != nil {
		t.Fatalf("resolve with no row: %v", err)
	}
	if got != (JobHome{}) {
		t.Fatalf("no row = %+v, want the zero home", got)
	}

	seedGrantPlacement(t, db, testCluster, "solo", "ns-solo", "cell-solo", 1, "{}")
	got, err = resolveJobHome(ctx, pool, testCluster, "solo", "solo/job")
	if err != nil {
		t.Fatalf("resolve with empty members: %v", err)
	}
	if want := (JobHome{Placed: true, Namespace: "ns-solo", Cell: "cell-solo"}); got != want {
		t.Fatalf("empty members = %+v, want %+v", got, want)
	}

	seedGrantPlacement(t, db, testCluster, "duo", "cells", "cell-d0", 1, "{}")
	setPlacementMembers(t, db, "duo",
		[]member{activeMember("cell-d0", 0, "i0"), activeMember("cell-d1", 1, "i1")},
		map[string]int32{"duo/one": 1})
	got, err = resolveJobHome(ctx, pool, testCluster, "duo", "duo/one")
	if err != nil {
		t.Fatalf("resolve slot 1: %v", err)
	}
	if want := (JobHome{Placed: true, Namespace: "cells", Cell: "cell-d1"}); got != want {
		t.Fatalf("slot 1 = %+v, want %+v", got, want)
	}

	setPlacementMembers(t, db, "duo",
		[]member{activeMember("cell-d0", 0, "i0")}, map[string]int32{"duo/one": 1})
	got, err = resolveJobHome(ctx, pool, testCluster, "duo", "duo/one")
	if err != nil {
		t.Fatalf("resolve unbound slot: %v", err)
	}
	if !got.Placed || got.Cell != "" {
		t.Fatalf("unbound slot = %+v, want Placed and an empty cell", got)
	}
}

// TestJobHomeCacheReadsEachPlacementOnce asserts the round cache answers
// a later call from the first read, so one round reads one row once.
func TestJobHomeCacheReadsEachPlacementOnce(t *testing.T) {
	pool, db := setupLedgerPool(t)
	ctx := context.Background()
	seedGrantPlacement(t, db, testCluster, "duo", "cells", "cell-d0", 1, "{}")
	setPlacementMembers(t, db, "duo",
		[]member{activeMember("cell-d0", 0, "i0"), activeMember("cell-d1", 1, "i1")},
		map[string]int32{"duo/one": 1})

	cache := newJobHomeCache()
	first, err := cache.resolve(ctx, pool, testCluster, "duo", "duo/one")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if first.Cell != "cell-d1" {
		t.Fatalf("first cell = %q, want cell-d1", first.Cell)
	}

	setPlacementMembers(t, db, "duo",
		[]member{activeMember("cell-d0", 0, "i0"), activeMember("cell-d2", 1, "i2")},
		map[string]int32{"duo/one": 1})
	cached, err := cache.resolve(ctx, pool, testCluster, "duo", "duo/one")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if cached.Cell != "cell-d1" {
		t.Fatalf("cached cell = %q, want the first read's cell-d1", cached.Cell)
	}

	fresh, err := newJobHomeCache().resolve(ctx, pool, testCluster, "duo", "duo/one")
	if err != nil {
		t.Fatalf("fresh resolve: %v", err)
	}
	if fresh.Cell != "cell-d2" {
		t.Fatalf("fresh cell = %q, want cell-d2", fresh.Cell)
	}
}

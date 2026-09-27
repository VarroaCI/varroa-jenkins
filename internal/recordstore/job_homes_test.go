package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
)

// seedJobHomePaths writes one job_numbers watermark row for each path, so
// the recompute finds the tenant's job paths.
func seedJobHomePaths(t *testing.T, db *sql.DB, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := db.Exec(`
			INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number, updated_at)
			VALUES ($1, $2, $3, 1, now())
			ON CONFLICT (cluster, tenant, job_path) DO NOTHING
		`, testCluster, testTenant, p); err != nil {
			t.Fatalf("seed job_numbers %s: %v", p, err)
		}
	}
}

// readStoredJobHomes reads one tenant's stored home map.
func readStoredJobHomes(t *testing.T, db *sql.DB) map[string]int32 {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`
		SELECT job_homes FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant).Scan(&raw); err != nil {
		t.Fatalf("read job_homes: %v", err)
	}
	out := map[string]int32{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode job_homes: %v", err)
	}
	return out
}

// readStoredMembers reads one tenant's stored member list.
func readStoredMembers(t *testing.T, db *sql.DB) []member {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(`
		SELECT members FROM varroa.tenant_placements WHERE cluster = $1 AND tenant = $2
	`, testCluster, testTenant).Scan(&raw); err != nil {
		t.Fatalf("read members: %v", err)
	}
	var out []member
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	return out
}

// announceMembers builds an announce request that carries a member list.
func announceMembers(generation int64, members []member) TenantSyncRequest {
	req := TenantSyncRequest{
		Kind: TenantSyncKindAnnounce, Cluster: testCluster, Tenant: testTenant,
		Generation: generation, Members: members,
	}
	if len(members) > 0 {
		req.Namespace = members[0].Namespace
		req.Cell = members[0].Cell
		req.CellInstance = members[0].Instance
	}
	return req
}

// activeMember builds one active member of the slot.
func activeMember(cell string, slot int32, instance string) member {
	s := slot
	return member{Cell: cell, Namespace: "cells", Role: MemberRoleActive, Slot: &s,
		Instance: instance, RehydratedInstance: instance}
}

// TestAnnounceStoresTheMemberListWhole asserts the announce writes the
// whole member list and recomputes the home map inside the same call.
func TestAnnounceStoresTheMemberListWhole(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	seedJobHomePaths(t, db, "acme/api", "acme/web")

	standby := member{Cell: "cell-c", Namespace: "cells", Role: MemberRoleStandby, Instance: "uid-c"}
	resp := sendTenantSync(t, pool, conn, announceMembers(4, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
		standby,
	}))
	if resp.Error != "" || !resp.Ok {
		t.Fatalf("announce: %+v", resp)
	}

	stored := readStoredMembers(t, db)
	if len(stored) != 3 {
		t.Fatalf("member count: want 3, got %d: %+v", len(stored), stored)
	}
	homes := readStoredJobHomes(t, db)
	if len(homes) != 2 {
		t.Fatalf("home map: want 2 entries, got %v", homes)
	}
	for path, slot := range homes {
		if slot != jobHomeSlot(path, 2) {
			t.Fatalf("%s homed on slot %d, want %d", path, slot, jobHomeSlot(path, 2))
		}
	}
}

// TestAnnounceRecomputeIsIdempotent asserts a repeat announce with the
// same member list writes the same map.
func TestAnnounceRecomputeIsIdempotent(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	seedJobHomePaths(t, db, "acme/api", "acme/web", "acme/ops")

	req := announceMembers(4, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
	})
	sendTenantSync(t, pool, conn, req)
	first := readStoredJobHomes(t, db)
	sendTenantSync(t, pool, conn, req)
	second := readStoredJobHomes(t, db)

	if !jobHomesEqual(first, second) {
		t.Fatalf("a repeat announce changed the map: %v then %v", first, second)
	}
}

// TestJobHomeScaleUpMovesOnlyOntoTheNewSlot asserts a scale from one slot
// to two re-homes only onto slot 1, and that a scale back re-homes only
// the jobs of the slot it removes.
func TestJobHomeScaleUpMovesOnlyOntoTheNewSlot(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	var paths []string
	for i := 0; i < 40; i++ {
		paths = append(paths, fmt.Sprintf("acme/job-%d", i))
	}
	seedJobHomePaths(t, db, paths...)

	sendTenantSync(t, pool, conn, announceMembers(1, []member{
		activeMember("cell-a", 0, "uid-a"),
	}))
	before := readStoredJobHomes(t, db)
	for path, slot := range before {
		if slot != 0 {
			t.Fatalf("a one-slot tenant homed %s on slot %d", path, slot)
		}
	}

	sendTenantSync(t, pool, conn, announceMembers(2, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
	}))
	after := readStoredJobHomes(t, db)
	moved := 0
	for path, slot := range after {
		if slot == before[path] {
			continue
		}
		moved++
		if slot != 1 {
			t.Fatalf("%s moved to slot %d, and the new slot is 1", path, slot)
		}
	}
	if moved == 0 {
		t.Fatal("a scale to two slots moved no job")
	}

	sendTenantSync(t, pool, conn, announceMembers(3, []member{
		activeMember("cell-a", 0, "uid-a"),
	}))
	shrunk := readStoredJobHomes(t, db)
	for path, slot := range shrunk {
		if slot != 0 {
			t.Fatalf("a scale-down left %s on slot %d", path, slot)
		}
		if after[path] == 0 && before[path] != 0 {
			t.Fatalf("%s moved between two surviving slots", path)
		}
	}
}

// TestJobHomesAnUnseenJobAnswersItsComputedHome asserts a path the
// recorder has never stored resolves to its hash slot through memberForJob.
func TestJobHomesAnUnseenJobAnswersItsComputedHome(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	seedJobHomePaths(t, db, "acme/api")

	sendTenantSync(t, pool, conn, announceMembers(2, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
	}))

	row, exists, err := readTenantPlacementMembers(context.Background(), pool, testCluster, testTenant)
	if err != nil || !exists {
		t.Fatalf("read members: exists=%v err=%v", exists, err)
	}
	want := []string{"cell-a", "cell-b"}[jobHomeSlot("acme/never-seen", 2)]
	if got := memberForJob(row, "acme/never-seen"); got.Cell != want {
		t.Fatalf("an unseen job resolved to %q, want its hash home %s", got.Cell, want)
	}
}

// TestRefreshJobHomesStoresANewJob asserts the backstop round stores the
// home of a job that first appears between two announces, and that a
// repeat round writes nothing.
func TestRefreshJobHomesStoresANewJob(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)
	seedJobHomePaths(t, db, "acme/api")

	sendTenantSync(t, pool, conn, announceMembers(2, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
	}))
	if len(readStoredJobHomes(t, db)) != 1 {
		t.Fatal("the announce stored the wrong number of homes")
	}

	seedJobHomePaths(t, db, "acme/new-job")
	changed, err := RefreshJobHomes(context.Background(), pool, nil, slog.Default())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if changed != 1 {
		t.Fatalf("the round changed %d tenants, want 1", changed)
	}
	homes := readStoredJobHomes(t, db)
	if _, ok := homes["acme/new-job"]; !ok {
		t.Fatalf("the round stored no home for the new job: %v", homes)
	}

	changed, err = RefreshJobHomes(context.Background(), pool, nil, slog.Default())
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if changed != 0 {
		t.Fatalf("a repeat round changed %d tenants, want 0", changed)
	}
}

// TestAnnounceClearsOnlyTheChangedMemberSeed asserts the per-member seed
// rule: a member whose instance changed loses its own seed state, and a
// member whose instance is unchanged keeps what the request reports.
func TestAnnounceClearsOnlyTheChangedMemberSeed(t *testing.T) {
	pool, db := setupLedgerPool(t)
	conn := newTestSyncConn(t)

	sendTenantSync(t, pool, conn, announceMembers(2, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b"),
	}))

	// Slot 1's pod is replaced. Slot 0 is unchanged and still reports its
	// applied seed.
	sendTenantSync(t, pool, conn, announceMembers(2, []member{
		activeMember("cell-a", 0, "uid-a"),
		activeMember("cell-b", 1, "uid-b2"),
	}))

	stored := readStoredMembers(t, db)
	byCell := map[string]member{}
	for _, m := range stored {
		byCell[m.Cell] = m
	}
	if got := byCell["cell-a"].RehydratedInstance; got != "uid-a" {
		t.Fatalf("slot 0 lost its seed: %q", got)
	}
	if got := byCell["cell-b"].RehydratedInstance; got != "" {
		t.Fatalf("slot 1 kept a seed across a pod replacement: %q", got)
	}
}

package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// seedTenantPlacementMembers inserts one tenant_placements row that
// carries a member list and a home map, so a fence test can drive a
// multi-slot tenant through the real read path.
func seedTenantPlacementMembers(t *testing.T, db *sql.DB, tenant string, generation int64,
	members []member, jobHomes map[string]int32) {
	t.Helper()
	membersJSON, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal members: %v", err)
	}
	homesJSON, err := json.Marshal(nonNilJobHomes(jobHomes))
	if err != nil {
		t.Fatalf("marshal job homes: %v", err)
	}
	slotZero := member{}
	for _, m := range members {
		if m.Role == MemberRoleActive && m.Slot != nil && *m.Slot == 0 {
			slotZero = m
		}
	}
	if _, err := db.Exec(`
		INSERT INTO varroa.tenant_placements
			(cluster, tenant, namespace, cell, generation, cell_instance, rehydrated_instance,
			 members, job_homes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb)
		ON CONFLICT (cluster, tenant) DO UPDATE SET
			namespace = EXCLUDED.namespace, cell = EXCLUDED.cell,
			generation = EXCLUDED.generation, cell_instance = EXCLUDED.cell_instance,
			rehydrated_instance = EXCLUDED.rehydrated_instance,
			members = EXCLUDED.members, job_homes = EXCLUDED.job_homes
	`, testCluster, tenant, slotZero.Namespace, slotZero.Cell, generation, slotZero.Instance,
		slotZero.RehydratedInstance, string(membersJSON), string(homesJSON)); err != nil {
		t.Fatalf("seed tenant placement members: %v", err)
	}
}

// fenceScenarioRow builds a two-slot, one-standby row for the canonical
// and new scenarios below. testJobPath is homed on slot 0.
func fenceScenarioMembers() []member {
	return []member{
		{Cell: "cell-a", Namespace: testNamespace, Role: MemberRoleActive, Slot: slotPtr(0),
			Instance: "pod-a", RehydratedInstance: "pod-a"},
		{Cell: "cell-b", Namespace: testNamespace, Role: MemberRoleActive, Slot: slotPtr(1),
			Instance: "pod-b", RehydratedInstance: "pod-b"},
		{Cell: "cell-c", Namespace: testNamespace, Role: MemberRoleStandby,
			Instance: "pod-c", RehydratedInstance: "pod-c"},
	}
}

// TestFenceCanonicalScenarios asserts the eleven canonical scenarios of
// the placement-fencing capability and the three the slot model adds.
// Each subtest names the scenario it proves.
func TestFenceCanonicalScenarios(t *testing.T) {
	singleMember := func() (tenantPlacementFenceRow, bool) {
		row, _ := baselinePlacementRow()
		return row, true
	}
	multiMember := func(generation int64) func() (tenantPlacementFenceRow, bool) {
		return func() (tenantPlacementFenceRow, bool) {
			return twoSlotFenceRow(generation), true
		}
	}

	tests := []struct {
		name       string
		row        func() (tenantPlacementFenceRow, bool)
		mutateReq  func(*ClaimRequest)
		wantAllow  bool
		wantReason string
	}{
		{
			// A stale cell is fenced.
			name: "a stale cell is fenced",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.cell = "cell-new"
				row.generation = 9
				resolveFenceRowMembers(&row)
				return row, true
			},
			mutateReq: func(req *ClaimRequest) {
				req.Cell = "cell-old"
				req.PlacementGeneration = 3
			},
			wantReason: ReasonFenced,
		},
		{
			// A lost announcement holds the build.
			name: "a lost announcement holds the build",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.generation = 4
				return row, true
			},
			mutateReq:  func(req *ClaimRequest) { req.PlacementGeneration = 5 },
			wantReason: ReasonNotReady,
		},
		{
			// A claim from the wrong cell is fenced.
			name: "a claim from the wrong cell is fenced",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.cell = "cell-b"
				resolveFenceRowMembers(&row)
				return row, true
			},
			mutateReq:  func(req *ClaimRequest) { req.Cell = "cell-a" },
			wantReason: ReasonFenced,
		},
		{
			// A single-tenant controller builds through its
			// generation-zero row.
			name: "a single-tenant controller builds through its generation-zero row",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.generation = 0
				return row, true
			},
			mutateReq: func(req *ClaimRequest) { req.PlacementGeneration = 0 },
			wantAllow: true,
		},
		{
			// A claim with no placement row is held.
			name:       "a claim with no placement row is held",
			row:        func() (tenantPlacementFenceRow, bool) { return tenantPlacementFenceRow{}, false },
			wantReason: ReasonNotReady,
		},
		{
			// A stale namespace is fenced.
			name: "a stale namespace is fenced",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.namespace = "cells-b"
				resolveFenceRowMembers(&row)
				return row, true
			},
			mutateReq:  func(req *ClaimRequest) { req.Namespace = "cells-a" },
			wantReason: ReasonFenced,
		},
		{
			// A rolled pod is held until the announcement lands.
			name: "a rolled pod is held until the announcement lands",
			row:  singleMember,
			mutateReq: func(req *ClaimRequest) {
				req.CellInstance = "uid-2"
			},
			wantReason: ReasonNotReady,
		},
		{
			// An unseeded cell holds the build.
			name: "an unseeded cell holds the build",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.rehydratedInstance = ""
				resolveFenceRowMembers(&row)
				return row, true
			},
			wantReason: ReasonNotReady,
		},
		{
			// A deferred job holds only its own builds.
			name: "a deferred job holds only its own builds",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.pendingJobs = []string{testJobPath}
				resolveFenceRowMembers(&row)
				return row, true
			},
			wantReason: ReasonNotReady,
		},
		{
			// A deferred job holds only its own builds: the second half.
			name: "another job of the same member is granted",
			row: func() (tenantPlacementFenceRow, bool) {
				row, _ := baselinePlacementRow()
				row.pendingJobs = []string{"acme/other"}
				resolveFenceRowMembers(&row)
				return row, true
			},
			wantAllow: true,
		},
		{
			// A cross-tenant claim is not retryable.
			name:       "a cross-tenant claim is not retryable",
			row:        singleMember,
			mutateReq:  func(req *ClaimRequest) { req.Actor = "svc-other" },
			wantReason: ReasonCrossTenant,
		},
		{
			// A standby's claim is not its home.
			name: "a standby's claim is not its home",
			row:  multiMember(3),
			mutateReq: func(req *ClaimRequest) {
				req.Cell = "cell-c"
				req.CellInstance = "pod-c"
			},
			wantReason: ReasonNotHome,
		},
		{
			// A non-home slot's claim is refused.
			name: "a non-home slot's claim is refused",
			row:  multiMember(3),
			mutateReq: func(req *ClaimRequest) {
				req.Cell = "cell-b"
				req.CellInstance = "pod-b"
			},
			wantReason: ReasonNotHome,
		},
		{
			// A takeover leaves another slot's builds alone: the
			// slot-0 claim carries the previous generation and is held.
			name:       "a takeover leaves another slot's builds alone",
			row:        multiMember(4),
			mutateReq:  func(req *ClaimRequest) { req.PlacementGeneration = 3 },
			wantReason: ReasonNotReady,
		},
		{
			// The same claim with the current generation is granted.
			name:      "the next claim from that cell is granted",
			row:       multiMember(4),
			mutateReq: func(req *ClaimRequest) { req.PlacementGeneration = 4 },
			wantAllow: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, exists := tc.row()
			req := newTestClaimRequest("key", 3)
			if tc.mutateReq != nil {
				tc.mutateReq(&req)
			}
			got := decidePlacement(row, exists, req)
			if got.Allow != tc.wantAllow {
				t.Fatalf("Allow = %v, want %v (result %+v)", got.Allow, tc.wantAllow, got)
			}
			if !tc.wantAllow && got.ReasonCode != tc.wantReason {
				t.Fatalf("ReasonCode = %q, want %q (result %+v)", got.ReasonCode, tc.wantReason, got)
			}
		})
	}
}

// TestFenceMessageNamesTheCellAndTheJob asserts the NOT_HOME message
// carries the cell and the job path, so the caller learns the reason.
func TestFenceMessageNamesTheCellAndTheJob(t *testing.T) {
	row := twoSlotFenceRow(3)
	req := newTestClaimRequest("key", 3)
	req.Cell = "cell-c"
	req.CellInstance = "pod-c"

	got := decidePlacement(row, true, req)
	want := "cell cell-c is not the home of job acme/app"
	if got.Message != want {
		t.Fatalf("Message = %q, want %q", got.Message, want)
	}
}

// TestFenceTakeoverIsolatesTheOtherSlot drives the takeover through the
// real read path. Slot 1 is rebound onto a new cell and the tenant
// generation rises. A slot-0 claim carrying the previous generation
// answers NOT_READY, not FENCED, and the next claim succeeds.
//
// This is the rule that makes a slot takeover safe: under FENCED the
// plugin cancels every in-flight build of every other slot.
func TestFenceTakeoverIsolatesTheOtherSlot(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementMembers(t, db, testTenant, 3, fenceScenarioMembers(),
		map[string]int32{testJobPath: 0})
	fence := NewPlacementFence(pool, nil)

	// Slot 1 moves to cell-d, and the generation rises.
	promoted := fenceScenarioMembers()
	promoted[1] = member{Cell: "cell-d", Namespace: testNamespace, Role: MemberRoleActive,
		Slot: slotPtr(1), Instance: "pod-d", RehydratedInstance: "pod-d"}
	seedTenantPlacementMembers(t, db, testTenant, 4, promoted, map[string]int32{testJobPath: 0})

	stale := newTestClaimRequest("manual:"+newTestUUIDv7String(), 3)
	result, err := fence.Check(context.Background(), stale)
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	if result.Allow || result.ReasonCode != ReasonNotReady {
		t.Fatalf("a slot-0 claim inside the resolver cache window answered %+v, want NOT_READY", result)
	}

	current := newTestClaimRequest("manual:"+newTestUUIDv7String(), 4)
	result, err = fence.Check(context.Background(), current)
	if err != nil {
		t.Fatalf("current claim: %v", err)
	}
	if !result.Allow {
		t.Fatalf("the next slot-0 claim answered %+v, want allow", result)
	}
}

// TestFenceRefusesANonHomeSlotThroughTheReadPath asserts the stored home
// map reaches the fence: a claim for a slot-0 job from slot 1's holder
// answers NOT_HOME.
func TestFenceRefusesANonHomeSlotThroughTheReadPath(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementMembers(t, db, testTenant, 3, fenceScenarioMembers(),
		map[string]int32{testJobPath: 0})
	fence := NewPlacementFence(pool, nil)

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 3)
	req.Cell = "cell-b"
	req.CellInstance = "pod-b"
	result, err := fence.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if result.Allow || result.ReasonCode != ReasonNotHome {
		t.Fatalf("a non-home slot answered %+v, want NOT_HOME", result)
	}
}

// TestFenceGrantsTheHomeSlotThroughTheReadPath asserts a claim from the
// job's own home slot, on a multi-slot tenant, is granted.
func TestFenceGrantsTheHomeSlotThroughTheReadPath(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementMembers(t, db, testTenant, 3, fenceScenarioMembers(),
		map[string]int32{testJobPath: 1})
	fence := NewPlacementFence(pool, nil)

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 3)
	req.Cell = "cell-b"
	req.CellInstance = "pod-b"
	result, err := fence.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !result.Allow {
		t.Fatalf("the home slot answered %+v, want allow", result)
	}
}

// TestReasonNotHomeIsExported asserts NOT_HOME joins the exported reason
// code list, which the gateway and the plugin both read.
func TestReasonNotHomeIsExported(t *testing.T) {
	found := false
	for _, code := range ReasonCodes {
		if code == ReasonNotHome {
			found = true
		}
	}
	if !found {
		t.Fatalf("NOT_HOME is absent from ReasonCodes: %v", ReasonCodes)
	}
	if ReasonNotHome != "NOT_HOME" {
		t.Fatalf("ReasonNotHome = %q, want NOT_HOME", ReasonNotHome)
	}
}

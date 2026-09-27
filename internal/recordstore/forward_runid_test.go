package recordstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// alphaClaim builds one claim of alpha/nightly from the named cell of the
// two-slot alpha that buildForwardTestDeps seeds.
func alphaClaim(runID, intentKey, cell, instance string) ClaimRequest {
	return ClaimRequest{
		IntentKey:           intentKey,
		RunID:               runID,
		Cluster:             "core",
		Namespace:           "ns-a",
		Cell:                cell,
		CellInstance:        instance,
		Tenant:              "alpha",
		JobPath:             "alpha/nightly",
		Actor:               "tester",
		PlacementGeneration: 1,
		RequestedAt:         time.Now().UTC(),
	}
}

// alphaEnvelope builds one build event of alpha/nightly from the named
// cell of the two-slot alpha.
func alphaEnvelope(t *testing.T, runID, intentKey, kind, cell, instance string,
	occurredAt time.Time, payload any) IngestEnvelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return IngestEnvelope{
		SchemaVersion: 1,
		Kind:          kind,
		RunID:         runID,
		IntentKey:     intentKey,
		Cluster:       "core",
		Namespace:     "ns-a",
		Cell:          cell,
		CellInstance:  instance,
		Tenant:        "alpha",
		JobPath:       "alpha/nightly",
		OccurredAt:    occurredAt.UTC().Truncate(time.Microsecond),
		Payload:       raw,
	}
}

// TestNotHomeForwardCompletesTheAPIRunID drives one API trigger through
// a NOT_HOME veto and a forward. The run id the API answered is the run
// that builds, and the store holds one row for it.
func TestNotHomeForwardCompletesTheAPIRunID(t *testing.T) {
	deps, db := buildForwardTestDeps(t)
	actions := subscribeLedgerActions(t, deps)
	g := NewIngester(deps.Pool, nil, nil)
	ledger := NewLedger(deps.Pool, NewPlacementFence(deps.Pool, nil))
	ctx := context.Background()

	runID := newTestUUIDv7String()
	key := "manual:" + runID
	accepted := map[string]any{"http_status": 201, "actor": "user:alice"}
	start := time.Now().UTC().Add(-time.Minute)

	// The dispatcher sent the trigger to slot 1, which is not the home.
	if _, err := g.Apply(ctx, alphaEnvelope(t, runID, key, KindRunTriggerAccepted, "cell-b", "pod-b",
		start, accepted)); err != nil {
		t.Fatalf("apply the first accept: %v", err)
	}
	veto := ledger.Claim(ctx, alphaClaim(runID, key, "cell-b", "pod-b"))
	if veto.Decision != DecisionVetoed || veto.ReasonCode != ReasonNotHome {
		t.Fatalf("claim from slot 1 answered %+v, want NOT_HOME", veto)
	}
	if veto.HomeSlot == nil || *veto.HomeSlot != 0 {
		t.Fatalf("NOT_HOME home slot = %v, want 0", veto.HomeSlot)
	}
	var intents int
	if err := db.QueryRow(`SELECT count(*) FROM varroa.run_intents WHERE intent_key = $1`, key).Scan(&intents); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if intents != 0 {
		t.Fatalf("a NOT_HOME veto wrote %d run_intents rows, want 0", intents)
	}

	// The claimer forwards with its own run id and key.
	req := forwardRequest(key)
	req.CallerRunID = runID
	resp, err := handleCrossTenantBuild(ctx, deps, req)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if resp.RunID != runID {
		t.Fatalf("forward answered run %q, want %s", resp.RunID, runID)
	}
	select {
	case action := <-actions:
		if action.RunID != runID || action.Controller != "cell-a" {
			t.Fatalf("action = %+v, want run %s on cell-a", action, runID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the trigger_run action")
	}

	// The home accepts, claims, starts, and completes the same run id.
	if _, err := g.Apply(ctx, alphaEnvelope(t, runID, key, KindRunTriggerAccepted, "cell-a", "pod-a",
		start.Add(10*time.Second), accepted)); err != nil {
		t.Fatalf("apply the home's accept: %v", err)
	}
	var cell string
	if err := db.QueryRow(`SELECT cell FROM varroa.runs WHERE run_id = $1`, runID).Scan(&cell); err != nil {
		t.Fatalf("read the accepted row: %v", err)
	}
	if cell != "cell-a" {
		t.Fatalf("the accepted-only row names %q, want the home cell-a", cell)
	}
	if grant := ledger.Claim(ctx, alphaClaim(runID, key, "cell-a", "pod-a")); grant.Decision != DecisionGranted {
		t.Fatalf("the home's claim answered %+v, want granted", grant)
	}
	buildNumber := int64(1)
	if _, err := g.Apply(ctx, alphaEnvelope(t, runID, key, KindRunStarted, "cell-a", "pod-a",
		start.Add(20*time.Second), RunStartedPayload{BuildNumber: &buildNumber})); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}
	result := "SUCCESS"
	if _, err := g.Apply(ctx, alphaEnvelope(t, runID, key, KindRunCompleted, "cell-a", "pod-a",
		start.Add(30*time.Second), RunCompletedPayload{Result: &result})); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}

	var (
		rows      int
		status    string
		gotResult sql.NullString
		gotRunID  string
	)
	if err := db.QueryRow(`SELECT count(*) FROM varroa.runs WHERE intent_key = $1`, key).Scan(&rows); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if rows != 1 {
		t.Fatalf("runs rows for the key = %d, want 1", rows)
	}
	if err := db.QueryRow(`SELECT run_id::text, status, result, cell FROM varroa.runs WHERE intent_key = $1`, key).
		Scan(&gotRunID, &status, &gotResult, &cell); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if gotRunID != runID || status != StatusCompleted || gotResult.String != result || cell != "cell-a" {
		t.Fatalf("run = %s %s %v %s, want %s COMPLETED SUCCESS cell-a", gotRunID, status, gotResult, cell, runID)
	}
}

// TestTriggerAccepted_SecondAcceptRebindsAnAcceptedOnlyRow asserts a
// newer accept on another cell moves an accepted-only row, and that an
// older accept redelivered after it never moves the row back.
func TestTriggerAccepted_SecondAcceptRebindsAnAcceptedOnlyRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()

	runID := newTestUUIDv7String()
	first := newTriggerAcceptedEnvelope(t, runID, "manual:"+runID)
	first.Cell, first.CellInstance = "cell-b", "pod-b"
	first.OccurredAt = first.OccurredAt.Add(-time.Minute)
	if _, err := g.Apply(ctx, first); err != nil {
		t.Fatalf("apply the first accept: %v", err)
	}

	second := newTriggerAcceptedEnvelope(t, runID, "manual:"+runID)
	res, err := g.Apply(ctx, second)
	if err != nil {
		t.Fatalf("apply the second accept: %v", err)
	}
	if !res.TriggerAcceptedStored {
		t.Fatal("the rebind reports no stored row")
	}
	assertAcceptedRow(t, db, runID, triggerAcceptedCell, "pod-a")

	again, err := g.Apply(ctx, first)
	if err != nil {
		t.Fatalf("apply the redelivered first accept: %v", err)
	}
	if again.TriggerAcceptedStored {
		t.Fatal("a redelivered older accept reports a stored row")
	}
	assertAcceptedRow(t, db, runID, triggerAcceptedCell, "pod-a")
}

// TestTriggerAccepted_SecondAcceptLeavesARunningRow asserts a late
// accept never moves a row that records a build.
func TestTriggerAccepted_SecondAcceptLeavesARunningRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	runID := seedGrantedIntent(t, pool, "manual:rebind-running")

	buildNumber := int64(3)
	started := newTestEnvelope(t, runID, "manual:rebind-running", KindRunStarted, RunStartedPayload{BuildNumber: &buildNumber})
	if _, err := g.Apply(ctx, started); err != nil {
		t.Fatalf("apply run.started: %v", err)
	}

	late := newTriggerAcceptedEnvelope(t, runID.String(), "manual:rebind-running")
	late.Cell, late.CellInstance = "cell-b", "pod-b"
	res, err := g.Apply(ctx, late)
	if err != nil {
		t.Fatalf("apply the late accept: %v", err)
	}
	if res.TriggerAcceptedStored {
		t.Fatal("a late accept reports a stored row over a running build")
	}
	var status, cell string
	if err := db.QueryRow(`SELECT status, cell FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &cell); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if status != StatusRunning || cell != "cell-a" {
		t.Fatalf("run = %s on %s, want RUNNING on cell-a", status, cell)
	}
	if _, ok := readRunPayload(t, db, runID.String())["accepted_at"]; ok {
		t.Fatal("a late accept wrote the accepted keys onto a running build")
	}
}

// TestTriggerAccepted_SecondAcceptLeavesADuplicateRow asserts a late
// accept never moves a terminal DUPLICATE row.
func TestTriggerAccepted_SecondAcceptLeavesADuplicateRow(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	key := "manual:rebind-duplicate"
	seedGrantedIntent(t, pool, key)

	runID := newTestUUIDv7String()
	first := newTriggerAcceptedEnvelope(t, runID, key)
	first.OccurredAt = first.OccurredAt.Add(-time.Minute)
	if _, err := g.Apply(ctx, first); err != nil {
		t.Fatalf("apply the first accept: %v", err)
	}
	second := newTriggerAcceptedEnvelope(t, runID, key)
	second.Cell, second.CellInstance = "cell-b", "pod-b"
	if _, err := g.Apply(ctx, second); err != nil {
		t.Fatalf("apply the second accept: %v", err)
	}
	var status, cell string
	if err := db.QueryRow(`SELECT status, cell FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &cell); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if status != StatusDuplicate || cell != triggerAcceptedCell {
		t.Fatalf("run = %s on %s, want DUPLICATE on %s", status, cell, triggerAcceptedCell)
	}
}

// assertAcceptedRow asserts an accepted-only row names cell and holds
// instance as its accepting instance.
func assertAcceptedRow(t *testing.T, db *sql.DB, runID, cell, instance string) {
	t.Helper()
	var (
		gotCell, status string
		gotInstance     sql.NullString
	)
	if err := db.QueryRow(`
		SELECT cell, status, payload->>'accepted_instance' FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&gotCell, &status, &gotInstance); err != nil {
		t.Fatalf("read the accepted row: %v", err)
	}
	if gotCell != cell || gotInstance.String != instance || status != StatusQueued {
		t.Fatalf("row = %s %s %s, want QUEUED on %s with instance %s", status, gotCell, gotInstance.String, cell, instance)
	}
}

// TestFenceNotHomeNamesTheHomeSlot asserts orders 5 and 6 carry the
// job's home slot, a new job's veto names its computed home, and every
// other answer carries none.
func TestFenceNotHomeNamesTheHomeSlot(t *testing.T) {
	row := twoSlotFenceRow(3)

	standby := newTestClaimRequest("key", 3)
	standby.Cell, standby.CellInstance = "cell-c", "pod-c"
	if got := decidePlacement(row, true, standby); got.HomeSlot == nil || *got.HomeSlot != 0 {
		t.Fatalf("order 5 home slot = %v, want 0", got.HomeSlot)
	}

	other := newTestClaimRequest("key", 3)
	other.Cell, other.CellInstance = "cell-b", "pod-b"
	if got := decidePlacement(row, true, other); got.HomeSlot == nil || *got.HomeSlot != 0 {
		t.Fatalf("order 6 home slot = %v, want 0", got.HomeSlot)
	}

	// acme/app has no stored entry here, and the jump hash homes it on
	// slot 1 of two.
	fresh := twoSlotFenceRow(3)
	fresh.members.jobHomes = map[string]int32{}
	home := newTestClaimRequest("key", 3)
	got := decidePlacement(fresh, true, home)
	if got.ReasonCode != ReasonNotHome || got.HomeSlot == nil || *got.HomeSlot != 1 {
		t.Fatalf("a new job's claim from slot 0 answered %+v, want NOT_HOME naming slot 1", got)
	}

	stale := newTestClaimRequest("key", 2)
	if got := decidePlacement(row, true, stale); got.Allow || got.HomeSlot != nil {
		t.Fatalf("a NOT_READY veto answered %+v, want no home slot", got)
	}
	if got := decidePlacement(row, true, newTestClaimRequest("key", 3)); !got.Allow || got.HomeSlot != nil {
		t.Fatalf("a grant answered %+v, want no home slot", got)
	}
}

// TestJobHome_NewJobAnswersItsComputedHome asserts the dispatcher reads
// the computed hash home of a job the map does not name yet.
func TestJobHome_NewJobAnswersItsComputedHome(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedScheduleMembers(t, db, "beta", map[string]int32{})

	home, err := NewReader(pool).JobHome(context.Background(), jsTestCluster, "beta", "beta/fresh")
	if err != nil {
		t.Fatalf("JobHome: %v", err)
	}
	if !home.Placed || home.Cell != "cell-b" {
		t.Fatalf("JobHome = %+v, want cell-b, the hash home of beta/fresh", home)
	}
}

// TestJobSchedules_NewJobRowIsStoredByTheHashHome asserts the schedule
// gate admits a new job's row from its hash home alone.
func TestJobSchedules_NewJobRowIsStoredByTheHashHome(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	seedScheduleMembers(t, db, "acme", map[string]int32{})
	row := JobScheduleRowInput{Tenant: "acme", JobPath: "acme/unseen", TriggerType: "cron", Spec: "H * * * *"}

	if _, err := g.Apply(context.Background(), newJobScheduleEnvelope(t, "cell-a", time.Now().UTC(),
		JobSchedulesPayload{Hash: "a1", Rows: []JobScheduleRowInput{row}})); err != nil {
		t.Fatalf("apply slot 0's export: %v", err)
	}
	if got := readScheduleCell(t, db, "acme", "acme/unseen"); got != "" {
		t.Fatalf("slot 0's export stored the row on %q, want no row", got)
	}

	if _, err := g.Apply(context.Background(), newJobScheduleEnvelope(t, "cell-b", time.Now().UTC(),
		JobSchedulesPayload{Hash: "b1", Rows: []JobScheduleRowInput{row}})); err != nil {
		t.Fatalf("apply slot 1's export: %v", err)
	}
	if got := readScheduleCell(t, db, "acme", "acme/unseen"); got != "cell-b" {
		t.Fatalf("stored cell = %q, want cell-b", got)
	}
}

// TestJobSchedules_NewJobHomeDoesNotFlap asserts a new job's stored home
// stays on its hash slot over two exports and two recomputes.
func TestJobSchedules_NewJobHomeDoesNotFlap(t *testing.T) {
	pool, db := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ctx := context.Background()
	seedScheduleMembers(t, db, "acme", map[string]int32{})
	row := JobScheduleRowInput{Tenant: "acme", JobPath: "acme/unseen", TriggerType: "cron", Spec: "H * * * *"}
	now := time.Now().UTC()

	exports := []struct {
		cell string
		hash string
	}{{"cell-a", "a1"}, {"cell-b", "b1"}, {"cell-a", "a2"}, {"cell-b", "b2"}}
	for i, e := range exports {
		if _, err := g.Apply(ctx, newJobScheduleEnvelope(t, e.cell, now.Add(time.Duration(i)*time.Second),
			JobSchedulesPayload{Hash: e.hash, Rows: []JobScheduleRowInput{row}})); err != nil {
			t.Fatalf("apply export %d: %v", i, err)
		}
		if _, err := RefreshJobHomes(ctx, pool, nil, nil); err != nil {
			t.Fatalf("recompute %d: %v", i, err)
		}
		var slot sql.NullInt32
		if err := db.QueryRow(`
			SELECT (job_homes->>'acme/unseen')::int FROM varroa.tenant_placements
			WHERE cluster = $1 AND tenant = 'acme'
		`, jsTestCluster).Scan(&slot); err != nil {
			t.Fatalf("read job homes: %v", err)
		}
		if slot.Valid && slot.Int32 != 1 {
			t.Fatalf("after export %d the stored home is slot %d, want 1 or none", i, slot.Int32)
		}
		if i >= 1 && (!slot.Valid || readScheduleCell(t, db, "acme", "acme/unseen") != "cell-b") {
			t.Fatalf("after export %d: stored home %v, row cell %q, want slot 1 on cell-b",
				i, slot, readScheduleCell(t, db, "acme", "acme/unseen"))
		}
	}
}

// TestLedgerClaimCarriesTheHomeSlot asserts the ledger copies the
// fence's home slot onto a NOT_HOME reply and onto no other reply.
func TestLedgerClaimCarriesTheHomeSlot(t *testing.T) {
	pool, db := setupLedgerPool(t)
	seedTenantPlacementMembers(t, db, testTenant, 3, fenceScenarioMembers(), map[string]int32{testJobPath: 0})
	ledger := NewLedger(pool, NewPlacementFence(pool, nil))

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 3)
	req.Cell, req.CellInstance = "cell-b", "pod-b"
	resp := ledger.Claim(context.Background(), req)
	if resp.ReasonCode != ReasonNotHome || resp.HomeSlot == nil || *resp.HomeSlot != 0 {
		t.Fatalf("claim from slot 1 answered %+v, want NOT_HOME naming slot 0", resp)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if slot, ok := wire["home_slot"]; !ok || slot != float64(0) {
		t.Fatalf("wire home_slot = %v (present %v), want 0", slot, ok)
	}

	granted := ledger.Claim(context.Background(), newTestClaimRequest("manual:"+newTestUUIDv7String(), 3))
	if granted.Decision != DecisionGranted || granted.HomeSlot != nil {
		t.Fatalf("a grant answered %+v, want no home slot", granted)
	}
	if _, err := uuid.Parse(granted.RunID); err != nil {
		t.Fatalf("grant run id %q: %v", granted.RunID, err)
	}
}

package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedAcceptedTriggerRow writes the runs row a run.trigger_accepted
// envelope produces for runID, exactly as applyTriggerAccepted writes it.
func seedAcceptedTriggerRow(t *testing.T, g *Ingester, runID uuid.UUID, intentKey string) {
	t.Helper()
	env := newTestEnvelope(t, runID, intentKey, KindRunTriggerAccepted, RunTriggerAcceptedPayload{})
	if _, err := g.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply run.trigger_accepted: %v", err)
	}
}

// runStatus answers one run row's stored status.
func runStatus(t *testing.T, g *Ingester, runID uuid.UUID) string {
	t.Helper()
	status, _ := runTerminalState(t, g, runID)
	return status
}

// runTerminalState answers one run row's stored status and end time.
// Retention reclaims a terminal run by ended_at, so a terminal status
// with none never leaves the store before its partition expires.
func runTerminalState(t *testing.T, g *Ingester, runID uuid.UUID) (string, *time.Time) {
	t.Helper()
	var (
		status  string
		endedAt *time.Time
	)
	if err := g.pool.QueryRow(context.Background(),
		`SELECT status, ended_at FROM varroa.runs WHERE run_id = $1`, runID).Scan(&status, &endedAt); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return status, endedAt
}

// TestDuplicateClaimMarksTheAcceptedRowTerminal asserts a claim the
// ledger refuses as a duplicate marks the requesting run's
// accepted-trigger row DUPLICATE.
//
// The requesting run never builds: the plugin cancels its queue item.
// The interrupted sweep leaves a QUEUED row alone while its accepting
// instance is alive, so without this mark a healthy cell holds the row
// at QUEUED for good.
func TestDuplicateClaimMarksTheAcceptedRowTerminal(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ledger := NewLedger(pool, nil)

	// The accepted row lands before any claim holds the key, so it is
	// written QUEUED. This is the race order the claim-side mark covers.
	second := uuid.Must(uuid.NewV7())
	seedAcceptedTriggerRow(t, g, second, "upstream:dup-1:t0000")

	first := seedGrantedIntent(t, pool, "upstream:dup-1:t0000")
	if got := runStatus(t, g, second); got != StatusQueued {
		t.Fatalf("status before the claim = %q, want QUEUED", got)
	}

	req := newTestClaimRequest("upstream:dup-1:t0000", 0)
	req.RunID = second.String()
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionDuplicate {
		t.Fatalf("decision = %q, want duplicate", resp.Decision)
	}
	if resp.RunID != first.String() {
		t.Fatalf("run_id = %q, want the winning run %q", resp.RunID, first)
	}
	status, endedAt := runTerminalState(t, g, second)
	if status != StatusDuplicate {
		t.Fatalf("status after the claim = %q, want DUPLICATE", status)
	}
	if endedAt == nil {
		t.Fatal("ended_at is null; retention reclaims a terminal run by ended_at")
	}
}

// TestAnAcceptedTriggerOfATakenKeyIsTerminalAtOnce asserts the other
// race order: the claim answers duplicate first, and the accepted-trigger
// row that follows is written DUPLICATE, never QUEUED.
func TestAnAcceptedTriggerOfATakenKeyIsTerminalAtOnce(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)
	ledger := NewLedger(pool, nil)

	seedGrantedIntent(t, pool, "upstream:dup-2:t0000")

	second := uuid.Must(uuid.NewV7())
	req := newTestClaimRequest("upstream:dup-2:t0000", 0)
	req.RunID = second.String()
	if resp := ledger.Claim(context.Background(), req); resp.Decision != DecisionDuplicate {
		t.Fatalf("decision = %q, want duplicate", resp.Decision)
	}

	seedAcceptedTriggerRow(t, g, second, "upstream:dup-2:t0000")

	status, endedAt := runTerminalState(t, g, second)
	if status != StatusDuplicate {
		t.Fatalf("status = %q, want DUPLICATE", status)
	}
	if endedAt == nil {
		t.Fatal("ended_at is null; retention reclaims a terminal run by ended_at")
	}
}

// TestAnAcceptedTriggerOfItsOwnKeyStaysQueued asserts the guard: a
// trigger whose key names its own run identifier is an ordinary queued
// run, and no mark touches it.
func TestAnAcceptedTriggerOfItsOwnKeyStaysQueued(t *testing.T) {
	pool, _ := setupLedgerPool(t)
	g := NewIngester(pool, nil, nil)

	runID := seedGrantedIntent(t, pool, "upstream:dup-3:t0000")
	seedAcceptedTriggerRow(t, g, runID, "upstream:dup-3:t0000")

	status, endedAt := runTerminalState(t, g, runID)
	if status != StatusQueued {
		t.Fatalf("status = %q, want QUEUED", status)
	}
	if endedAt != nil {
		t.Fatalf("ended_at = %v, want null on a queued run", endedAt)
	}

	// A real completion still supersedes the accepted row.
	result := "SUCCESS"
	completed := newTestEnvelope(t, runID, "upstream:dup-3:t0000", KindRunCompleted,
		RunCompletedPayload{Result: &result})
	if _, err := g.Apply(context.Background(), completed); err != nil {
		t.Fatalf("apply run.completed: %v", err)
	}
	if got := runStatus(t, g, runID); got != StatusCompleted {
		t.Fatalf("status = %q, want COMPLETED", got)
	}
}

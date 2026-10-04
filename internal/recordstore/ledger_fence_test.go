package recordstore

import (
	"context"
	"errors"
	"testing"
)

// TestReasonCodesHoldsAllSix asserts the exported reason-code set holds
// all six allocated names.
func TestReasonCodesHoldsAllSix(t *testing.T) {
	want := map[string]bool{
		ReasonInvalidKey:  false,
		ReasonFenced:      false,
		ReasonNotReady:    false,
		ReasonCrossTenant: false,
		ReasonThrottled:   false,
		ReasonNotHome:     false,
	}
	if len(ReasonCodes) != len(want) {
		t.Fatalf("ReasonCodes has %d entries, want %d: %v", len(ReasonCodes), len(want), ReasonCodes)
	}
	for _, code := range ReasonCodes {
		if _, ok := want[code]; !ok {
			t.Fatalf("ReasonCodes holds unexpected code %q", code)
		}
		want[code] = true
	}
	for code, seen := range want {
		if !seen {
			t.Fatalf("ReasonCodes is missing %q", code)
		}
	}
}

// TestDefaultFenceAllows asserts the allow-all default answers a
// well-formed claim with a granted decision.
func TestDefaultFenceAllows(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionGranted {
		t.Fatalf("want decision %q, got %+v", DecisionGranted, resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 1)
}

// denyingFence always denies with a fixed reason code.
type denyingFence struct {
	reasonCode string
}

func (f denyingFence) Check(_ context.Context, _ ClaimRequest) (FenceResult, error) {
	return FenceResult{Allow: false, ReasonCode: f.reasonCode, Message: "denied by test fence"}, nil
}

// TestFenceDenialPassesReasonCodeThrough asserts a Fence denial reaches
// the caller unchanged and writes no row.
func TestFenceDenialPassesReasonCodeThrough(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, denyingFence{reasonCode: ReasonNotReady})

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)

	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonNotReady {
		t.Fatalf("want vetoed/NOT_READY, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}

// erroringFence always fails.
type erroringFence struct{}

func (erroringFence) Check(_ context.Context, _ ClaimRequest) (FenceResult, error) {
	return FenceResult{}, errors.New("fence backend unreachable")
}

// TestFenceErrorFailsClosed asserts a Fence error answers with
// code=UNAVAILABLE and an empty decision.
func TestFenceErrorFailsClosed(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, erroringFence{})

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	resp := ledger.Claim(context.Background(), req)

	if resp.Code != CodeUnavailable {
		t.Fatalf("want code %q, got %+v", CodeUnavailable, resp)
	}
	if resp.Decision != "" {
		t.Fatalf("want empty decision, got %+v", resp)
	}
	assertIntentRowCount(t, db, req.IntentKey, 0)
}

// TestGenerationIsRecordedForLaterOwner asserts the stored intent holds
// the request's placement_generation.
func TestGenerationIsRecordedForLaterOwner(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	ledger := NewLedger(pool, nil)

	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 7)
	resp := ledger.Claim(context.Background(), req)
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}

	var generation int64
	if err := db.QueryRow(`
		SELECT placement_generation FROM varroa.run_intents WHERE intent_key = $1
	`, req.IntentKey).Scan(&generation); err != nil {
		t.Fatalf("read placement_generation: %v", err)
	}
	if generation != 7 {
		t.Fatalf("want placement_generation 7, got %d", generation)
	}
}

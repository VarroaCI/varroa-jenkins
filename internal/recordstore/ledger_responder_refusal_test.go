package recordstore

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestHandleClaimLogsRefusalReasonAndTenant asserts a refused claim
// logs its reason code and tenant at info level, once, with no request
// or response field beyond those two. It uses a malformed intent key,
// which Ledger.Claim vetoes with reason_code INVALID_KEY before it ever
// opens a database transaction, so the test needs no pool.
func TestHandleClaimLogsRefusalReasonAndTenant(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ledger := NewLedger(nil, nil)
	req := newTestClaimRequest("not-a-valid-key", 0)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	out := handleClaim(logger, ledger, body)

	var resp ClaimResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want vetoed/INVALID_KEY, got %+v", resp)
	}

	log := buf.String()
	if !strings.Contains(log, "claim refused") {
		t.Fatalf("log does not carry \"claim refused\":\n%s", log)
	}
	if !strings.Contains(log, "reason_code=INVALID_KEY") {
		t.Fatalf("log does not carry reason_code=INVALID_KEY:\n%s", log)
	}
	if !strings.Contains(log, "tenant="+testTenant) {
		t.Fatalf("log does not carry tenant=%s:\n%s", testTenant, log)
	}
	if strings.Count(log, "claim refused") != 1 {
		t.Fatalf("want exactly one refusal log line, got:\n%s", log)
	}
	// The refusal line names no other request or response field: the
	// job path and the malformed key stay out of the log.
	if strings.Contains(log, testJobPath) {
		t.Fatalf("log carries the job path, want reason code and tenant only:\n%s", log)
	}
}

// TestHandleClaimLogsNothingOnGrant asserts a granted claim logs no
// refusal line, so the new log statement fires only on a veto.
func TestHandleClaimLogsNothingOnGrant(t *testing.T) {
	pool, _ := setupLedgerPool(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	out := handleClaim(logger, ledger, body)

	var resp ClaimResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Decision != DecisionGranted {
		t.Fatalf("want granted, got %+v", resp)
	}
	if strings.Contains(buf.String(), "claim refused") {
		t.Fatalf("granted claim logged a refusal:\n%s", buf.String())
	}
}

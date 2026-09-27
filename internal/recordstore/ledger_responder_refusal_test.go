package recordstore

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// newTestClaimMetrics registers ClaimMetrics on a fresh manual reader.
func newTestClaimMetrics(t *testing.T) (*ClaimMetrics, func(reason string) int64) {
	t.Helper()
	reader := fanoutTestMeter(t)
	metrics, err := NewClaimMetrics()
	if err != nil {
		t.Fatalf("new claim metrics: %v", err)
	}
	count := func(reason string) int64 {
		return fanoutMetricSum(t, reader, "recorder_claims_vetoed_total", map[string]string{"reason": reason})
	}
	return metrics, count
}

// TestHandleClaimLogsAndCountsARefusal asserts a refused claim logs
// one info line with the reason code, the tenant, the run, the job
// path, the cell, and the message, and counts one veto under its
// reason. It uses a malformed intent key, which Ledger.Claim vetoes with
// reason_code INVALID_KEY before it opens a database transaction, so
// the test needs no pool.
func TestHandleClaimLogsAndCountsARefusal(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	metrics, count := newTestClaimMetrics(t)

	ledger := NewLedger(nil, nil)
	req := newTestClaimRequest("not-a-valid-key", 0)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	out := handleClaim(logger, ledger, metrics, body)

	var resp ClaimResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Decision != DecisionVetoed || resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want vetoed/INVALID_KEY, got %+v", resp)
	}

	log := buf.String()
	if strings.Count(log, "claim refused") != 1 {
		t.Fatalf("want exactly one refusal log line, got:\n%s", log)
	}
	for _, want := range []string{
		"reason_code=INVALID_KEY",
		"tenant=" + testTenant,
		"run_id=" + req.RunID,
		"job_path=" + testJobPath,
		"cell=" + req.Cell,
		"message=",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log does not carry %q:\n%s", want, log)
		}
	}
	// The line names no payload field beyond those six.
	for _, absent := range []string{"params_digest", "actor=", "cause="} {
		if strings.Contains(log, absent) {
			t.Fatalf("log carries %q, want it left out:\n%s", absent, log)
		}
	}
	if got := count(ReasonInvalidKey); got != 1 {
		t.Fatalf("INVALID_KEY veto count = %d, want 1", got)
	}
}

// TestHandleClaimCountsADecodeFailure asserts a body the decoder rejects
// counts one INVALID_KEY veto.
func TestHandleClaimCountsADecodeFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	metrics, count := newTestClaimMetrics(t)

	out := handleClaim(logger, NewLedger(nil, nil), metrics, []byte("{not json"))

	var resp ClaimResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want INVALID_KEY, got %+v", resp)
	}
	if got := count(ReasonInvalidKey); got != 1 {
		t.Fatalf("INVALID_KEY veto count = %d, want 1", got)
	}
}

// TestHandleClaimNilMetricsIsSafe asserts a responder built with no
// metrics answers a veto normally.
func TestHandleClaimNilMetricsIsSafe(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	body, err := json.Marshal(newTestClaimRequest("not-a-valid-key", 0))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var resp ClaimResponse
	if err := json.Unmarshal(handleClaim(logger, NewLedger(nil, nil), nil, body), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.ReasonCode != ReasonInvalidKey {
		t.Fatalf("want INVALID_KEY, got %+v", resp)
	}
}

// TestHandleClaimLogsNothingOnGrant asserts a granted claim logs no
// refusal line and counts no veto.
func TestHandleClaimLogsNothingOnGrant(t *testing.T) {
	pool, _ := setupLedgerPool(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	metrics, count := newTestClaimMetrics(t)

	ledger := NewLedger(pool, nil)
	req := newTestClaimRequest("manual:"+newTestUUIDv7String(), 0)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	out := handleClaim(logger, ledger, metrics, body)

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
	for _, reason := range ReasonCodes {
		if got := count(reason); got != 0 {
			t.Fatalf("%s veto count = %d, want 0", reason, got)
		}
	}
}

package recordstore

import (
	"context"
	"database/sql"
	"testing"
)

// applyLogSummaries applies one run.log envelope per summary to a fresh
// running run, in order, and answers the stored incomplete flag and
// incomplete_reason.
func applyLogSummaries(t *testing.T, summaries ...map[string]any) (bool, string) {
	t.Helper()
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	for _, s := range summaries {
		env := newTestEnvelope(t, runID, intentKey, KindRunLog, s)
		if _, err := g.Apply(context.Background(), env); err != nil {
			t.Fatalf("apply run.log %v: %v", s, err)
		}
	}
	var incomplete sql.NullBool
	var reason sql.NullString
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'incomplete')::bool, payload->'log_summary'->>'incomplete_reason'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&incomplete, &reason); err != nil {
		t.Fatalf("read log summary: %v", err)
	}
	return incomplete.Bool, reason.String
}

func incompleteWith(reason string) map[string]any {
	return map[string]any{"incomplete": true, "incomplete_reason": reason}
}

func assertLogReason(t *testing.T, incomplete bool, got, want string) {
	t.Helper()
	if !incomplete || got != want {
		t.Fatalf("incomplete=%v incomplete_reason=%q, want true/%q", incomplete, got, want)
	}
}

// TestApplyRunLogKeepsAPluginReasonAgainstAGenericOne asserts the
// gateway's finalize-deadline marker, which follows a boot-reconciled
// completion, keeps the plugin's jenkins-restart reason.
func TestApplyRunLogKeepsAPluginReasonAgainstAGenericOne(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t,
		map[string]any{"chunk_count": 4, "incomplete": true, "incomplete_reason": "jenkins-restart", "finalized": false},
		incompleteWith("finalize-deadline"))
	assertLogReason(t, incomplete, reason, "jenkins-restart")
}

// TestApplyRunLogKeepsAPluginReasonAgainstAGatewayOne asserts a gateway
// reason never replaces a stored plugin reason.
func TestApplyRunLogKeepsAPluginReasonAgainstAGatewayOne(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t, incompleteWith("jenkins-restart"), incompleteWith("gateway-gap"))
	assertLogReason(t, incomplete, reason, "jenkins-restart")
}

// TestApplyRunLogPluginReasonReplacesAGatewayOne asserts a plugin reason
// replaces a stored gateway reason, the order logSummaryPayload applies.
func TestApplyRunLogPluginReasonReplacesAGatewayOne(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t, incompleteWith("gateway-gap"), incompleteWith("plugin-drop"))
	assertLogReason(t, incomplete, reason, "plugin-drop")
}

// TestApplyRunLogPreciseReasonReplacesAGenericOne asserts a gateway reason
// replaces a stored generic reason.
func TestApplyRunLogPreciseReasonReplacesAGenericOne(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t, incompleteWith("finalize-deadline"), incompleteWith("gateway-gap"))
	assertLogReason(t, incomplete, reason, "gateway-gap")
}

// TestApplyRunLogKeepsTheFirstReasonInsideOneRank asserts the first
// stored reason wins against a later reason of the same rank.
func TestApplyRunLogKeepsTheFirstReasonInsideOneRank(t *testing.T) {
	t.Parallel()

	cases := []struct{ first, second string }{
		{"finalize-deadline", "not-finalized"},
		{"jenkins-restart", "plugin-drop"},
		{"rejected", "gateway-gap"},
	}
	for _, c := range cases {
		t.Run(c.first+"-then-"+c.second, func(t *testing.T) {
			incomplete, reason := applyLogSummaries(t, incompleteWith(c.first), incompleteWith(c.second))
			assertLogReason(t, incomplete, reason, c.first)
		})
	}
}

// TestApplyRunLogKeepsAReasonAgainstAMarkerWithNone asserts a run.log that
// carries no reason, such as the gateway's finalized marker, keeps the
// stored reason.
func TestApplyRunLogKeepsAReasonAgainstAMarkerWithNone(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t, incompleteWith("gateway-gap"), map[string]any{"finalized": true})
	assertLogReason(t, incomplete, reason, "gateway-gap")
}

// TestLogReasonRanksCoverEveryToken asserts the rank table holds every
// incomplete_reason token exactly once, in its rank.
func TestLogReasonRanksCoverEveryToken(t *testing.T) {
	want := map[string]int{
		"plugin-drop": 3, "terminal-response": 3, "jenkins-restart": 3,
		"rejected": 2, "gateway-gap": 2,
		"finalize-deadline": 1, "not-finalized": 1, "no-summary": 1,
	}
	if len(logReasonRanks) != len(want) {
		t.Fatalf("logReasonRanks holds %d tokens, want %d", len(logReasonRanks), len(want))
	}
	for _, r := range logReasonRanks {
		if want[r.token] != r.rank {
			t.Fatalf("token %q has rank %d, want %d", r.token, r.rank, want[r.token])
		}
	}
}

// TestLogReasonLossRankKeepsOnlyObservedLosses asserts the loss boundary
// splits the rank table at the plugin and gateway tokens: every token at
// or above logReasonLossRank records an observed loss, and every token
// below it is a generic timeout or backstop token.
func TestLogReasonLossRankKeepsOnlyObservedLosses(t *testing.T) {
	losses := map[string]bool{
		"plugin-drop": true, "terminal-response": true, "jenkins-restart": true,
		"rejected": true, "gateway-gap": true,
	}
	for _, r := range logReasonRanks {
		if got := r.rank >= logReasonLossRank; got != losses[r.token] {
			t.Fatalf("token %q at rank %d: kept against a complete log = %v, want %v", r.token, r.rank, got, losses[r.token])
		}
	}
}

// TestApplyRunLogMergesKeyByKey asserts a later run.log adds its own keys
// and keeps every stored key it does not carry, such as the chunk count
// of the completion's summary.
func TestApplyRunLogMergesKeyByKey(t *testing.T) {
	t.Parallel()

	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	for _, s := range []map[string]any{
		{"chunk_count": 4, "dropped_bytes": 7, "incomplete": true, "incomplete_reason": "jenkins-restart"},
		incompleteWith("finalize-deadline"),
		{"finalized": true},
	} {
		if _, err := g.Apply(context.Background(), newTestEnvelope(t, runID, intentKey, KindRunLog, s)); err != nil {
			t.Fatalf("apply run.log %v: %v", s, err)
		}
	}
	var chunkCount, droppedBytes int
	var incomplete, finalized bool
	var reason string
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'chunk_count')::int, (payload->'log_summary'->>'dropped_bytes')::int,
		       (payload->'log_summary'->>'incomplete')::bool, (payload->'log_summary'->>'finalized')::bool,
		       payload->'log_summary'->>'incomplete_reason'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&chunkCount, &droppedBytes, &incomplete, &finalized, &reason); err != nil {
		t.Fatalf("read log summary: %v", err)
	}
	if chunkCount != 4 || droppedBytes != 7 || !incomplete || !finalized || reason != "jenkins-restart" {
		t.Fatalf("chunk_count=%d dropped_bytes=%d incomplete=%v finalized=%v reason=%q, want 4/7/true/true/jenkins-restart",
			chunkCount, droppedBytes, incomplete, finalized, reason)
	}
}

// applyCompletions applies each completion to a fresh running run as the
// gateway publishes it: a run.log envelope, then a run.completed envelope.
// It then applies the gateway's finalize-deadline marker, and answers the
// stored incomplete flag and incomplete_reason.
func applyCompletions(t *testing.T, summaries ...map[string]any) (bool, string) {
	t.Helper()
	pool, db := setupLedgerPool(t)
	intentKey := "manual:" + newTestUUIDv7String()
	runID := startRunForFinalize(t, pool, intentKey)
	g := NewIngester(pool, nil, nil)
	result := "FAILURE"
	for _, s := range summaries {
		for _, env := range []IngestEnvelope{
			newTestEnvelope(t, runID, intentKey, KindRunLog, s),
			newTestEnvelope(t, runID, intentKey, KindRunCompleted, RunCompletedPayload{Result: &result}),
		} {
			if _, err := g.Apply(context.Background(), env); err != nil {
				t.Fatalf("apply %s %v: %v", env.Kind, s, err)
			}
		}
	}
	marker := newTestEnvelope(t, runID, intentKey, KindRunLog, incompleteWith("finalize-deadline"))
	if _, err := g.Apply(context.Background(), marker); err != nil {
		t.Fatalf("apply finalize-deadline marker: %v", err)
	}
	var incomplete sql.NullBool
	var reason sql.NullString
	if err := db.QueryRow(`
		SELECT (payload->'log_summary'->>'incomplete')::bool, payload->'log_summary'->>'incomplete_reason'
		FROM varroa.runs WHERE run_id = $1
	`, runID).Scan(&incomplete, &reason); err != nil {
		t.Fatalf("read log summary: %v", err)
	}
	return incomplete.Bool, reason.String
}

// TestTwoCompletionsKeepTheRestartReason asserts a run that reaches the
// store with two completions keeps the boot reconcile's jenkins-restart
// reason in both orders. The listener's completion reports a complete
// log, and the gateway's finalize-deadline marker follows both.
func TestTwoCompletionsKeepTheRestartReason(t *testing.T) {
	t.Parallel()

	boot := map[string]any{"incomplete": true, "incomplete_reason": "jenkins-restart", "finalized": false}
	listener := map[string]any{"chunk_count": 6, "dropped_bytes": 0, "incomplete": false, "finalized": false}
	cases := []struct {
		name  string
		order []map[string]any
	}{
		{"boot-then-listener", []map[string]any{boot, listener}},
		{"listener-then-boot", []map[string]any{listener, boot}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			incomplete, reason := applyCompletions(t, c.order...)
			assertLogReason(t, incomplete, reason, "jenkins-restart")
		})
	}
}

// TestApplyRunLogKeepsAGatewayReasonAgainstACompleteLog asserts a run.log
// that reports incomplete false keeps a stored gateway reason and the
// incomplete flag, because a later complete claim cannot undo a loss.
func TestApplyRunLogKeepsAGatewayReasonAgainstACompleteLog(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t, incompleteWith("gateway-gap"), map[string]any{"incomplete": false})
	assertLogReason(t, incomplete, reason, "gateway-gap")
}

// TestApplyRunLogCompleteLogDropsAFinalizeDeadlineReason asserts a
// run.log that reports incomplete false removes a stored generic reason.
func TestApplyRunLogCompleteLogDropsAFinalizeDeadlineReason(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t, incompleteWith("finalize-deadline"), map[string]any{"incomplete": false})
	if incomplete || reason != "" {
		t.Fatalf("incomplete=%v incomplete_reason=%q, want false and no reason", incomplete, reason)
	}
}

// TestApplyRunLogCompleteLogDropsTheReason asserts a run.log that reports
// incomplete false removes the stored reason, so the record never holds
// a reason for a complete log.
func TestApplyRunLogCompleteLogDropsTheReason(t *testing.T) {
	t.Parallel()

	incomplete, reason := applyLogSummaries(t, incompleteWith("not-finalized"), map[string]any{"incomplete": false})
	if incomplete || reason != "" {
		t.Fatalf("incomplete=%v incomplete_reason=%q, want false and no reason", incomplete, reason)
	}
}

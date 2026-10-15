package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunTests takes runs.tests_total, tests_failed, and tests_skipped
// from the payload's own aggregate counters. It never sums run_tests:
// the suite rows are a breakdown, and a capped suite list would undercount
// a sum. The payload's suites key, when present, replaces the run's suite
// set in replaceRunTests.
//
// The same transaction replaces the run's run_test_cases set when the
// payload carries a cases key, and stores the test report on the runs
// row. The runs upsert also removes the accepted keys from a stored row
// that still holds them.
func applyRunTests(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	var payload RunTestsPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("%w: decode run.tests payload: %v", ErrMalformedEnvelope, err)
	}

	if payload.Suites != nil {
		if err := replaceRunTests(ctx, tx, intent, dedupeSuites(payload.Suites)); err != nil {
			return err
		}
	}

	if payload.Cases != nil {
		if err := replaceRunTestCases(ctx, tx, intent, dedupeCases(payload.Cases)); err != nil {
			return err
		}
	}

	reportKey, reportPayload, err := testReportValues(payload.TestReport)
	if err != nil {
		return err
	}

	// The conflict arm wraps runs.payload in COALESCE: jsonb || jsonb is
	// strict, and a heartbeat-first placeholder row holds a NULL payload.
	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.runs (
			run_id, created_at, intent_key, cluster, namespace, cell, tenant, job_path,
			status, tests_total, tests_failed, tests_skipped, test_report_key, payload
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, 'QUEUED', $9, $10, $11, $12, $13::jsonb
		)
		ON CONFLICT (created_at, run_id) DO UPDATE SET
			tests_total     = EXCLUDED.tests_total,
			tests_failed    = EXCLUDED.tests_failed,
			tests_skipped   = EXCLUDED.tests_skipped,
			test_report_key = COALESCE(EXCLUDED.test_report_key, runs.test_report_key),
			payload         = (COALESCE(runs.payload, '{}'::jsonb) `+acceptedPayloadKeysSQL+`)
			                  || EXCLUDED.payload
	`,
		intent.runID, intent.createdAt, intent.intentKey, intent.cluster, intent.namespace,
		intent.cellValue(), intent.tenant, intent.jobPath, payload.Total, payload.Failed, payload.Skipped,
		reportKey, reportPayload,
	); err != nil {
		return fmt.Errorf("update runs test counters: %w", err)
	}
	return nil
}

// dedupeSuites keeps the first entry of each suite name and drops every
// later one. PostgreSQL refuses an INSERT ... ON CONFLICT whose own
// proposed rows hold one conflict key twice.
func dedupeSuites(in []RunTestSuitePayload) []RunTestSuitePayload {
	seen := make(map[string]struct{}, len(in))
	out := make([]RunTestSuitePayload, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s.Suite]; ok {
			continue
		}
		seen[s.Suite] = struct{}{}
		out = append(out, s)
	}
	return out
}

// replaceRunTests deletes the run's run_tests rows, then inserts suites.
// The payload carries the run's whole bounded suite set and never a
// delta, so a merge would keep a suite the run no longer reports. An
// empty suites list runs the delete alone. The rows take created_at from
// the intent row, so retention deletes a suite row on the run's own
// cutoff. suites must hold no suite name twice; the ON CONFLICT arm is a
// guard, never an update.
func replaceRunTests(ctx context.Context, tx pgx.Tx, intent *intentRow, suites []RunTestSuitePayload) error {
	if _, err := tx.Exec(ctx, `DELETE FROM varroa.run_tests WHERE run_id = $1`, intent.runID); err != nil {
		return fmt.Errorf("delete run_tests: %w", err)
	}
	if len(suites) == 0 {
		return nil
	}

	names := make([]string, len(suites))
	totals := make([]int32, len(suites))
	faileds := make([]int32, len(suites))
	skippeds := make([]int32, len(suites))
	durations := make([]*int64, len(suites))
	for i, s := range suites {
		names[i] = s.Suite
		totals[i] = int32(s.Total)
		faileds[i] = int32(s.Failed)
		skippeds[i] = int32(s.Skipped)
		durations[i] = s.DurationMs
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.run_tests (run_id, suite, created_at, total, failed, skipped, duration_ms)
		SELECT $1, s, $2, t, f, k, d
		FROM unnest($3::text[], $4::int[], $5::int[], $6::int[], $7::bigint[]) AS x(s, t, f, k, d)
		ON CONFLICT (run_id, suite) DO NOTHING
	`,
		intent.runID, intent.createdAt, names, totals, faileds, skippeds, durations,
	); err != nil {
		return fmt.Errorf("insert run_tests: %w", err)
	}
	return nil
}

// testReportValues answers the runs.test_report_key parameter and the
// runs.payload merge object for a run.tests report. The key is nil when
// the envelope carries no report or an empty blob key, so the upsert
// never clears a stored key. The merge object is empty when the envelope
// carries no report.
func testReportValues(report *RunTestReportPayload) (*string, string, error) {
	if report == nil {
		return nil, "{}", nil
	}
	var key *string
	if report.BlobKey != "" {
		k := report.BlobKey
		key = &k
	}
	merged, err := json.Marshal(map[string]interface{}{
		"test_report": map[string]interface{}{
			"size_bytes": report.SizeBytes,
			"case_count": report.CaseCount,
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("marshal test report: %w", err)
	}
	return key, string(merged), nil
}

// dedupeCases keeps the first case of each (suite, class_name, name) key
// and drops every later one. PostgreSQL refuses an INSERT ... ON CONFLICT
// whose own proposed rows hold one conflict key twice, and that refusal
// fails the whole transaction.
func dedupeCases(in []RunTestCasePayload) []RunTestCasePayload {
	type caseKey struct{ suite, className, name string }
	seen := make(map[caseKey]struct{}, len(in))
	out := make([]RunTestCasePayload, 0, len(in))
	for _, c := range in {
		k := caseKey{c.Suite, c.ClassName, c.Name}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	return out
}

// replaceRunTestCases deletes the run's run_test_cases rows, then inserts
// cases. The payload carries the run's whole bounded set and never a
// delta, so a merge would keep a case the run no longer reports. An empty
// cases list runs the delete alone. The rows take cluster, tenant,
// job_path, and created_at from the intent row, so retention deletes a
// case row on the run's own cutoff. cases must hold no key twice; the ON
// CONFLICT arm is a guard, never an update.
func replaceRunTestCases(ctx context.Context, tx pgx.Tx, intent *intentRow, cases []RunTestCasePayload) error {
	if _, err := tx.Exec(ctx, `DELETE FROM varroa.run_test_cases WHERE run_id = $1`, intent.runID); err != nil {
		return fmt.Errorf("delete run_test_cases: %w", err)
	}
	if len(cases) == 0 {
		return nil
	}

	suites := make([]string, len(cases))
	classes := make([]string, len(cases))
	names := make([]string, len(cases))
	statuses := make([]string, len(cases))
	durations := make([]*int64, len(cases))
	details := make([]string, len(cases))
	for i, c := range cases {
		suites[i] = c.Suite
		classes[i] = c.ClassName
		names[i] = c.Name
		statuses[i] = c.Status
		durations[i] = c.DurationMs
		details[i] = c.ErrorDetails
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO varroa.run_test_cases (
			run_id, suite, class_name, name, cluster, tenant, job_path,
			status, duration_ms, error_details, created_at)
		SELECT $1, s, c, n, $2, $3, $4, st, d, e, $5
		FROM unnest($6::text[], $7::text[], $8::text[], $9::text[], $10::bigint[], $11::text[])
		     AS t(s, c, n, st, d, e)
		ON CONFLICT (run_id, suite, class_name, name) DO NOTHING
	`,
		intent.runID, intent.cluster, intent.tenant, intent.jobPath, intent.createdAt,
		suites, classes, names, statuses, durations, details,
	); err != nil {
		return fmt.Errorf("insert run_test_cases: %w", err)
	}
	return nil
}

package recordstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyRunQuality writes the coverage and warnings summaries of a run. Each
// list follows its own key. A present key replaces the run's rows of that
// table. An absent key keeps them. An empty list clears them. The rows
// copy their scope and created_at from the intent, and the write never
// touches the runs row.
func applyRunQuality(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope) error {
	var payload RunQualityPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("%w: decode run.quality payload: %v", ErrMalformedEnvelope, err)
	}

	if payload.Coverage != nil {
		kept := dedupeCoverage(payload.Coverage)
		// The dedupe removes reports the run published. They count as dropped.
		dropped := droppedCount(payload.CoverageDropped) + (len(payload.Coverage) - len(kept))
		if err := replaceRunCoverage(ctx, tx, intent, kept, dropped); err != nil {
			return err
		}
	}
	if payload.Warnings != nil {
		kept := dedupeWarnings(payload.Warnings)
		dropped := droppedCount(payload.WarningsDropped) + (len(payload.Warnings) - len(kept))
		if err := replaceRunWarnings(ctx, tx, intent, kept, dropped); err != nil {
			return err
		}
	}
	return nil
}

// droppedCount clamps a dropped count from the payload to a column value.
// A negative count stores as zero.
func droppedCount(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// replaceRunCoverage replaces the run_coverage rows of one run with the
// given reports. A nil counter stores NULL in both of its columns, so a
// zero count stays a real value. The caller passes reports without empty
// or duplicate ids. Each row stores reportsDropped, the run's count of
// reports it did not store, and its own metricsDropped.
func replaceRunCoverage(ctx context.Context, tx pgx.Tx, intent *intentRow, reports []RunCoveragePayload, reportsDropped int) error {
	if _, err := tx.Exec(ctx, `DELETE FROM varroa.run_coverage WHERE run_id = $1`, intent.runID); err != nil {
		return fmt.Errorf("delete run_coverage: %w", err)
	}
	for _, r := range reports {
		metrics, err := metricsJSON(r.Metrics)
		if err != nil {
			return fmt.Errorf("encode run_coverage metrics: %w", err)
		}
		lineCovered, lineTotal := counterValues(r.Line)
		branchCovered, branchTotal := counterValues(r.Branch)
		modifiedCovered, modifiedTotal := counterValues(r.ModifiedLines)
		if _, err := tx.Exec(ctx, `
			INSERT INTO varroa.run_coverage (
				run_id, report_id, cluster, tenant, job_path, name,
				line_covered, line_total, branch_covered, branch_total,
				modified_line_covered, modified_line_total,
				metrics, gate_status, created_at, reports_dropped, metrics_dropped)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16, $17)
			ON CONFLICT (run_id, report_id) DO NOTHING
		`,
			intent.runID, r.ID, intent.cluster, intent.tenant, intent.jobPath, r.Name,
			lineCovered, lineTotal, branchCovered, branchTotal,
			modifiedCovered, modifiedTotal,
			metrics, nilIfEmpty(r.GateStatus), intent.createdAt,
			reportsDropped, droppedCount(r.MetricsDropped),
		); err != nil {
			return fmt.Errorf("insert run_coverage: %w", err)
		}
	}
	return nil
}

// replaceRunWarnings replaces the run_warnings rows of one run with the
// given reports. NewTotal and FixedTotal store NULL unless the plugin sent
// them, which it does only when it found a reference build. A zero total
// stays a real value. The caller passes reports without empty or duplicate
// ids. Each row stores reportsDropped, the run's count of warnings reports
// it did not store.
func replaceRunWarnings(ctx context.Context, tx pgx.Tx, intent *intentRow, reports []RunWarningsPayload, reportsDropped int) error {
	if _, err := tx.Exec(ctx, `DELETE FROM varroa.run_warnings WHERE run_id = $1`, intent.runID); err != nil {
		return fmt.Errorf("delete run_warnings: %w", err)
	}
	for _, w := range reports {
		var newTotal, fixedTotal *int32
		if w.NewTotal != nil {
			v := int32(*w.NewTotal)
			newTotal = &v
		}
		if w.FixedTotal != nil {
			v := int32(*w.FixedTotal)
			fixedTotal = &v
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO varroa.run_warnings (
				run_id, report_id, cluster, tenant, job_path, name,
				total, error, high, normal, low,
				new_total, fixed_total, gate_status, created_at, reports_dropped)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (run_id, report_id) DO NOTHING
		`,
			intent.runID, w.ID, intent.cluster, intent.tenant, intent.jobPath, w.Name,
			int32(w.Total), int32(w.Error), int32(w.High), int32(w.Normal), int32(w.Low),
			newTotal, fixedTotal, nilIfEmpty(w.GateStatus), intent.createdAt, reportsDropped,
		); err != nil {
			return fmt.Errorf("insert run_warnings: %w", err)
		}
	}
	return nil
}

// counterValues maps one counter onto its covered and total columns. A nil
// counter maps to two NULL values, so the pair is set or unset together.
func counterValues(c *RunCoverageCounterPayload) (*int32, *int32) {
	if c == nil {
		return nil, nil
	}
	covered, total := int32(c.Covered), int32(c.Total)
	return &covered, &total
}

// metricCount is one metric value inside the run_coverage metrics object.
type metricCount struct {
	Covered int `json:"covered"`
	Total   int `json:"total"`
}

// metricsJSON encodes the metrics of one report as a JSON object keyed by
// metric name. It skips a metric with an empty name, and the first of two
// metrics with the same name wins. An empty list encodes as an empty
// object.
func metricsJSON(metrics []RunCoverageCounterPayload) (string, error) {
	out := make(map[string]metricCount, len(metrics))
	for _, m := range metrics {
		if m.Metric == "" {
			continue
		}
		if _, seen := out[m.Metric]; seen {
			continue
		}
		out[m.Metric] = metricCount{Covered: m.Covered, Total: m.Total}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// nilIfEmpty maps an empty text to NULL, so an absent value stores NULL and
// not an empty string.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// dedupeCoverage drops a report with an empty id and keeps the first of
// each id. A duplicate id would violate the primary key of run_coverage.
func dedupeCoverage(in []RunCoveragePayload) []RunCoveragePayload {
	seen := make(map[string]bool, len(in))
	out := make([]RunCoveragePayload, 0, len(in))
	for _, r := range in {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	return out
}

// dedupeWarnings drops a report with an empty id and keeps the first of
// each id. A duplicate id would violate the primary key of run_warnings.
func dedupeWarnings(in []RunWarningsPayload) []RunWarningsPayload {
	seen := make(map[string]bool, len(in))
	out := make([]RunWarningsPayload, 0, len(in))
	for _, w := range in {
		if w.ID == "" || seen[w.ID] {
			continue
		}
		seen[w.ID] = true
		out = append(out, w)
	}
	return out
}

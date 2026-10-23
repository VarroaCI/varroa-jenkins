package recordstore

import (
	"context"
	"fmt"
	"time"
)

// runNoteRetentionBatch caps the notes one statement deletes.
const runNoteRetentionBatch = 1000

// runNoteOrphanGrace keeps a run note whose run row is missing for an
// hour after the run was created. A run row written by a racing ingest
// is never mistaken for a purged one.
const runNoteOrphanGrace = time.Hour

// deleteOrphanRunNotesSQL deletes one batch of run notes whose run row is
// gone. The run row was purged with its tenant or by the run window.
const deleteOrphanRunNotesSQL = `
DELETE FROM varroa.run_notes WHERE id IN (
  SELECT m.id FROM varroa.run_notes m
  WHERE m.run_created_at < $1
    AND NOT EXISTS (
      SELECT 1 FROM varroa.runs r
      WHERE r.run_id = m.run_id AND r.created_at = m.run_created_at)
  ORDER BY m.run_created_at
  LIMIT $2)`

// deleteJobRunNotesSQL deletes one batch of job notes older than the
// cutoff. The %s slot takes an optional tenant filter. Its placeholders
// are $1 for the cutoff, $2 for the batch size, and, in the filter, $3
// and $4.
const deleteJobRunNotesSQL = `
DELETE FROM varroa.run_notes WHERE id IN (
  SELECT id FROM varroa.run_notes
  WHERE run_id IS NULL AND created_at < $1 %s
  ORDER BY created_at
  LIMIT $2)`

// runNoteTenantFilterSQL narrows a job-note batch to one tenant.
const runNoteTenantFilterSQL = `AND cluster = $3 AND tenant = $4`

// deleteExpiredRunNotes deletes the notes that no retention window keeps.
// A run note follows its run, so it goes once its run row is gone. A job
// note follows the store-wide runs window, or the shorter tenant window
// of its tenant. It repeats each batch until one batch deletes fewer
// rows than the batch size.
func deleteExpiredRunNotes(ctx context.Context, db maintDB, now time.Time, storeRuns time.Duration) error {
	err := deleteRunNoteBatches(ctx, db, deleteOrphanRunNotesSQL, now.Add(-runNoteOrphanGrace))
	if err != nil {
		return fmt.Errorf("delete orphan run notes: %w", err)
	}
	if storeRuns <= 0 {
		return nil
	}

	rows, err := db.Query(ctx, `SELECT cluster, tenant, policy FROM varroa.tenant_placements`)
	if err != nil {
		return fmt.Errorf("select tenant note windows: %w", err)
	}
	var tenants []tenantPolicyRow
	for rows.Next() {
		var row tenantPolicyRow
		var raw []byte
		if err := rows.Scan(&row.cluster, &row.tenant, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("scan tenant note window: %w", err)
		}
		row.policy = decodeRetention(raw)
		if row.policy.Runs > 0 && row.policy.Runs < storeRuns {
			tenants = append(tenants, row)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("select tenant note windows: %w", err)
	}
	rows.Close()

	err = deleteRunNoteBatches(ctx, db, fmt.Sprintf(deleteJobRunNotesSQL, ""), now.Add(-storeRuns))
	if err != nil {
		return fmt.Errorf("delete expired job notes: %w", err)
	}
	for _, t := range tenants {
		sql := fmt.Sprintf(deleteJobRunNotesSQL, runNoteTenantFilterSQL)
		err := deleteRunNoteBatches(ctx, db, sql, now.Add(-t.policy.Runs), t.cluster, t.tenant)
		if err != nil {
			return fmt.Errorf("delete tenant job notes: %w", err)
		}
	}
	return nil
}

// deleteRunNoteBatches runs one note delete statement until a batch
// deletes fewer rows than runNoteRetentionBatch. The statement takes the
// cutoff, the batch size, and any extra arguments after them.
func deleteRunNoteBatches(ctx context.Context, db maintDB, sql string, cutoff time.Time, extra ...any) error {
	for {
		args := append([]any{cutoff, runNoteRetentionBatch}, extra...)
		tag, err := db.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		n := int(tag.RowsAffected())
		if n < runNoteRetentionBatch {
			return nil
		}
	}
}

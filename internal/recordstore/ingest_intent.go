package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrIntentMissing means no run_intents row exists for an ingest
// envelope's run_id. The store is written before the cell learns of the
// build, so this is a NAK-class condition: the claim may still be
// moments away, or retention already deleted an expired intent.
var ErrIntentMissing = errors.New("run_intents row is missing for this run_id")

// intentRow is the subset of run_intents an ingest write needs. Every
// ingest write reads cluster, namespace, tenant, job_path, cell, and
// created_at from this row rather than the envelope, because the ledger
// wrote them first (design commitment "store before cell").
type intentRow struct {
	runID        uuid.UUID
	createdAt    time.Time
	intentKey    string
	cluster      string
	namespace    string
	tenant       string
	jobPath      string
	cell         *string
	cellInstance *string
}

// loadIntent reads the run_intents row for runID inside tx. It returns
// ErrIntentMissing when the row is absent.
func loadIntent(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (*intentRow, error) {
	row := &intentRow{runID: runID}
	err := tx.QueryRow(ctx, `
		SELECT created_at, intent_key, cluster, namespace, tenant, job_path, cell, cell_instance
		FROM varroa.run_intents
		WHERE run_id = $1
	`, runID).Scan(&row.createdAt, &row.intentKey, &row.cluster, &row.namespace,
		&row.tenant, &row.jobPath, &row.cell, &row.cellInstance)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrIntentMissing
	case err != nil:
		return nil, fmt.Errorf("load run_intents: %w", err)
	}
	return row, nil
}

// cellValue returns the intent's cell, or an empty string when it is
// null. runs.cell is NOT NULL; a granted claim always names a cell.
func (r *intentRow) cellValue() string {
	if r.cell == nil {
		return ""
	}
	return *r.cell
}

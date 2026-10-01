package recordstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MarkStart answers one start mark: a varroa.ledger.claim request with
// StartMark set. The plugin sends it after the granted build number
// applies and before the queue item can leave the queue. The item stays
// queued until the mark answers granted, so an intent with no mark proves
// that its build did not start.
//
// The answers are:
//
//   - granted: the intent names this run and this owner, and the mark is
//     stored. A repeat mark, or a mark for a consumed intent of this
//     owner, also answers granted and writes nothing.
//   - duplicate: the intent names another run or another owner. The
//     caller cancels its queue item, because the grant moved. For another
//     run, the requesting run's accepted-trigger row ends DUPLICATE in the
//     same transaction.
//   - vetoed: the fence refused the request, the request's generation is
//     below the stored one, or no intent holds the key.
//   - an empty decision with a code: the database did not answer. The
//     caller keeps the item queued and claims again.
//
// The transaction takes its locks in the claim's order: the fence locks
// the placement row first, then the intent row is read FOR UPDATE.
func (l *Ledger) MarkStart(ctx context.Context, req ClaimRequest, runID uuid.UUID) ClaimResponse {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	result, err := l.checkFence(ctx, tx, req)
	if err != nil {
		return ClaimResponse{Code: CodeUnavailable, Error: err.Error()}
	}
	if !result.Allow {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: result.ReasonCode, Message: result.Message,
			HomeSlot: result.HomeSlot}
	}

	resp, err := markStartTx(ctx, tx, req, runID)
	if err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}
	}
	if err := tx.Commit(ctx); err != nil {
		return ClaimResponse{Code: classifyDBError(err), Error: err.Error()}
	}
	return resp
}

// markStartTx reads the intent under FOR UPDATE and stores the mark when
// the request names the stored run and the stored owner.
func markStartTx(ctx context.Context, tx pgx.Tx, req ClaimRequest, runID uuid.UUID) (ClaimResponse, error) {
	var row storedIntent
	err := tx.QueryRow(ctx, `
		SELECT run_id, cell_instance, placement_generation, state, build_number, start_marked_at
		FROM varroa.run_intents
		WHERE intent_key = $1
		FOR UPDATE
	`, req.IntentKey).Scan(&row.runID, &row.cellInstance, &row.placementGeneration, &row.state,
		&row.buildNumber, &row.startMarkedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonInvalidKey,
			Message: "no grant holds this intent key"}, nil
	case err != nil:
		return ClaimResponse{}, fmt.Errorf("read intent for start mark: %w", err)
	}

	if row.runID != runID {
		// The intent moved to another run, so the requesting run never
		// builds. Its accepted-trigger row ends DUPLICATE here, as a
		// duplicate claim's row does.
		_, createdAt, err := parseRunUUIDv7(req.RunID)
		if err != nil {
			return ClaimResponse{}, fmt.Errorf("parse run id of a duplicate start mark: %w", err)
		}
		if err := markDuplicateClaim(ctx, tx, runID, createdAt); err != nil {
			return ClaimResponse{}, err
		}
		return ClaimResponse{Decision: DecisionDuplicate, RunID: row.runID.String()}, nil
	}
	if storedOwner(row) != req.CellInstance {
		// The same run moved to another owner, which builds it. Its row is
		// that build's own row, so the answer writes nothing.
		return ClaimResponse{Decision: DecisionDuplicate, RunID: row.runID.String()}, nil
	}
	if req.PlacementGeneration < row.placementGeneration {
		return ClaimResponse{Decision: DecisionVetoed, ReasonCode: ReasonFenced, RunID: row.runID.String()}, nil
	}
	buildNumber := derefInt64(row.buildNumber)
	if row.state == stateConsumed || row.startMarkedAt != nil {
		return ClaimResponse{Decision: DecisionGranted, RunID: row.runID.String(), BuildNumber: buildNumber}, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.run_intents SET start_marked_at = now() WHERE intent_key = $1
	`, req.IntentKey); err != nil {
		return ClaimResponse{}, fmt.Errorf("store start mark: %w", err)
	}
	return ClaimResponse{Decision: DecisionGranted, RunID: row.runID.String(), BuildNumber: buildNumber}, nil
}

package recordstore

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// ownerQueueGroup is the NATS queue group the recorder's owner responder
// joins, so more than one recorder replica shares the subscription.
const ownerQueueGroup = "recorder"

// ownerLookupTimeout bounds one lookup's database work, so a stalled
// Postgres connection does not hold the responder goroutine open
// indefinitely.
const ownerLookupTimeout = 10 * time.Second

// ownerLookupRequest is the request body of a varroa.ledger.owner call.
type ownerLookupRequest struct {
	RunID string `json:"runId"`
}

// ownerLookupResponse is the reply body of a varroa.ledger.owner call.
// AnchorUnixMs follows the same anchor order the log-finalization sweep
// uses, so a restarted gateway learns the finalization deadline in the
// same round trip that proves ownership. Error is empty on a successful
// lookup, found or not. It carries a short reason only when the lookup
// itself failed, so the caller never mistakes a database error for a
// genuine miss.
type ownerLookupResponse struct {
	Found        bool   `json:"found"`
	Cluster      string `json:"cluster,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	Cell         string `json:"cell,omitempty"`
	Instance     string `json:"instance,omitempty"`
	Status       string `json:"status,omitempty"`
	AnchorUnixMs int64  `json:"anchorUnixMs,omitempty"`
	Error        string `json:"error,omitempty"`
}

// RegisterOwnerResponder subscribes to bus.LedgerOwnerSubject with queue
// group "recorder". The returned Sub stops the subscription when
// unsubscribed.
func RegisterOwnerResponder(conn *bus.Conn, pool *pgxpool.Pool, logger *slog.Logger) (*bus.Sub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := func(data []byte) []byte {
		return handleOwnerLookup(logger, pool, data)
	}
	return conn.SubscribeRequest(bus.LedgerOwnerSubject, ownerQueueGroup, handler)
}

// handleOwnerLookup decodes an ownerLookupRequest, reads the run's
// current owner from run_intents joined with its runs row, and encodes
// the answer. A body the decoder rejects, or a request naming no run,
// answers found=false with no error: that is a malformed request, not a
// database failure, and the caller treats it as a policy failure. A
// genuine database error answers found=false with a non-empty error, so
// the caller never caches it as a refusal.
func handleOwnerLookup(logger *slog.Logger, pool *pgxpool.Pool, data []byte) []byte {
	var req ownerLookupRequest
	if err := json.Unmarshal(data, &req); err != nil || req.RunID == "" {
		out, _ := json.Marshal(ownerLookupResponse{Found: false})
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), ownerLookupTimeout)
	defer cancel()

	resp, err := lookupOwner(ctx, pool, req.RunID)
	if err != nil {
		logger.Error("owner lookup failed", "run_id", req.RunID, "error", err)
		out, _ := json.Marshal(ownerLookupResponse{Found: false, Error: "owner lookup failed"})
		return out
	}
	out, err := json.Marshal(resp)
	if err != nil {
		logger.Error("owner lookup response encode failed", "run_id", req.RunID, "error", err)
		out, _ = json.Marshal(ownerLookupResponse{Found: false, Error: "owner lookup response encode failed"})
	}
	return out
}

// lookupOwner reads run_intents for runID, left-joined onto its varroa.runs
// row so a run that has not yet produced a run.started envelope still
// answers found=true with an empty status and no anchor. anchorExpr is
// the exact expression the log-finalization sweep uses, so the two
// callers can never disagree about which moment gates the finalize wait.
func lookupOwner(ctx context.Context, pool *pgxpool.Pool, runID string) (ownerLookupResponse, error) {
	row := pool.QueryRow(ctx, `
		SELECT ri.cluster, ri.namespace, ri.cell, ri.cell_instance, r.status,
		       `+anchorExpr+`
		FROM varroa.run_intents ri
		LEFT JOIN varroa.runs r ON r.run_id = ri.run_id
		WHERE ri.run_id = $1
	`, runID)

	var cluster, namespace string
	var cell, cellInstance, status *string
	var anchor *time.Time
	if err := row.Scan(&cluster, &namespace, &cell, &cellInstance, &status, &anchor); err != nil {
		if err == pgx.ErrNoRows {
			return ownerLookupResponse{Found: false}, nil
		}
		return ownerLookupResponse{}, err
	}

	resp := ownerLookupResponse{
		Found:     true,
		Cluster:   cluster,
		Namespace: namespace,
		Cell:      derefString(cell),
		Instance:  derefString(cellInstance),
		Status:    derefString(status),
	}
	if anchor != nil {
		resp.AnchorUnixMs = anchor.UnixMilli()
	}
	return resp, nil
}

// derefString returns "" for a nil pointer, and the pointed-to value
// otherwise.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

package recordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// crossTenantQueueGroup is the NATS queue group the recorder's
// cross-tenant responder joins, so more than one recorder replica shares
// the subscription.
const crossTenantQueueGroup = "recorder"

// crossTenantTimeout bounds one cross-tenant request's database work, so
// a stalled Postgres connection does not hold the responder goroutine
// open indefinitely. It follows the claim, owner, and tenant-sync
// responders' own bound; design.md D14's shorter wire-level budgets
// belong to the gateway and the plugin, not to this internal safety
// bound.
const crossTenantTimeout = 10 * time.Second

// crossTenantBlobSigner is the subset of blob.Store the artifact_url
// kind needs: minting a signed read URL for a recorded key. It stays a
// narrow local interface, matching ObjectDeleter's precedent
// (retention_objects.go), because this responder never puts, appends,
// reads, or deletes an object.
type crossTenantBlobSigner interface {
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// CrossTenantDeps carries every dependency the cross-tenant responder
// needs to answer one request.
type CrossTenantDeps struct {
	Pool        *pgxpool.Pool
	Evaluator   *GrantEvaluator
	Reader      Reader
	Blob        crossTenantBlobSigner
	Conn        *bus.Conn
	Metrics     *CrossTenantMetrics
	LockMetrics *FleetLockMetrics
}

// RegisterCrossTenantResponder subscribes to bus.LedgerCrossTenantSubject
// with queue group "recorder". The returned Sub stops the subscription
// when unsubscribed. It registers its own CrossTenantMetrics;
// registration failure is logged and the responder still starts, with
// that instrument counting nothing. lockMetrics is the one
// FleetLockMetrics the caller also wires into the maintenance round's
// MaintenanceConfig: the three gauges and the expired counter are one
// shared instrument set, registered once, never twice on the same
// meter.
func RegisterCrossTenantResponder(conn *bus.Conn, pool *pgxpool.Pool, store crossTenantBlobSigner, lockMetrics *FleetLockMetrics, logger *slog.Logger) (*bus.Sub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	metrics, err := NewCrossTenantMetrics()
	if err != nil {
		logger.Warn("cross-tenant metrics registration failed; the cross-tenant responder starts anyway", "error", err)
	}
	deps := CrossTenantDeps{
		Pool:        pool,
		Evaluator:   NewGrantEvaluator(pool),
		Reader:      NewReader(pool),
		Blob:        store,
		Conn:        conn,
		Metrics:     metrics,
		LockMetrics: lockMetrics,
	}
	handler := func(data []byte) []byte {
		return handleCrossTenantBytes(logger, deps, data)
	}
	return conn.SubscribeRequest(bus.LedgerCrossTenantSubject, crossTenantQueueGroup, handler)
}

// handleCrossTenantBytes decodes a CrossTenantRequest, dispatches it by
// Kind, and encodes the CrossTenantResponse. A body the decoder rejects
// answers code=INTERNAL, because a request the recorder cannot read
// carries no valid kind either.
func handleCrossTenantBytes(logger *slog.Logger, deps CrossTenantDeps, data []byte) []byte {
	var req CrossTenantRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logger.Warn("cross-tenant request decode failed", "error", err)
		out, _ := json.Marshal(CrossTenantResponse{Code: CodeInternal, Error: "malformed cross-tenant request"})
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), crossTenantTimeout)
	defer cancel()

	resp := dispatchCrossTenant(ctx, deps, req)
	out, err := json.Marshal(resp)
	if err != nil {
		logger.Error("cross-tenant response encode failed", "kind", req.Kind, "error", err)
		out, _ = json.Marshal(CrossTenantResponse{Kind: req.Kind, Code: CodeInternal, Error: "encode cross-tenant response"})
	}
	return out
}

// dispatchCrossTenant runs req against the kind its Kind field names.
// An error a handler returns is a system fault, never a policy veto:
// dispatchCrossTenant maps it through classifyDBError, the same mapping
// the claim path uses, so a Postgres outage answers UNAVAILABLE and
// every other fault answers INTERNAL. A kind outside the eight allocated
// values answers INTERNAL on its own, with no database read: the
// gateway relay already rejects it, but this subject accepts requests
// from no other guard.
func dispatchCrossTenant(ctx context.Context, deps CrossTenantDeps, req CrossTenantRequest) CrossTenantResponse {
	var (
		resp CrossTenantResponse
		err  error
	)
	switch req.Kind {
	case CrossTenantKindBuild:
		resp, err = handleCrossTenantBuild(ctx, deps, req)
	case CrossTenantKindStatus:
		resp, err = handleCrossTenantStatus(ctx, deps, req)
	case CrossTenantKindLastSuccessful:
		resp, err = handleCrossTenantLastSuccessful(ctx, deps, req)
	case CrossTenantKindArtifactList:
		resp, err = handleCrossTenantArtifactList(ctx, deps, req)
	case CrossTenantKindArtifactURL:
		resp, err = handleCrossTenantArtifactURL(ctx, deps, req)
	case CrossTenantKindLockAcquire:
		resp, err = handleCrossTenantLockAcquire(ctx, deps, req)
	case CrossTenantKindLockRenew:
		resp, err = handleCrossTenantLockRenew(ctx, deps, req)
	case CrossTenantKindLockRelease:
		resp, err = handleCrossTenantLockRelease(ctx, deps, req)
	default:
		resp = CrossTenantResponse{Kind: req.Kind, Code: CodeInternal,
			Error: fmt.Sprintf("unknown cross-tenant request kind %q", req.Kind)}
		deps.Metrics.addRequest(ctx, req.Kind, crossTenantRequestOutcome(resp))
		return resp
	}
	if err != nil {
		resp = CrossTenantResponse{Kind: req.Kind, Code: classifyDBError(err), Error: err.Error()}
		deps.Metrics.addRequest(ctx, req.Kind, crossTenantRequestOutcome(resp))
		return resp
	}
	resp.Kind = req.Kind
	if resp.ReasonCode == ReasonCrossTenant {
		deps.Metrics.addGrantDenied(ctx, crossTenantGrantDenialReason(resp.Message))
	}
	deps.Metrics.addRequest(ctx, req.Kind, crossTenantRequestOutcome(resp))
	return resp
}

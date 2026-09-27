package recordstore

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Fence decides whether a claim from a cell may proceed.
// C1 ships allowAllFence. C6 supplies the authoritative implementation.
type Fence interface {
	Check(ctx context.Context, req ClaimRequest) (FenceResult, error)
}

// TxFence is a Fence that can check inside the claim's own transaction.
// Claim calls CheckTx, not Check, on a Fence that implements this
// interface. The transaction-aware read locks the placement row for the
// whole claim, so a concurrent placement announcement serializes against
// the claim instead of racing it: the announce's generation bump either
// lands before the read, or waits for the claim transaction to commit.
type TxFence interface {
	Fence
	CheckTx(ctx context.Context, tx pgx.Tx, req ClaimRequest) (FenceResult, error)
}

// FenceResult is the answer a Fence gives for one claim.
type FenceResult struct {
	Allow      bool
	ReasonCode string // FENCED, NOT_READY, NOT_HOME, CROSS_TENANT, or THROTTLED
	Message    string
	// HomeSlot names the job's home slot on a NOT_HOME veto, and is nil
	// on every other answer. The claimer forwards to that slot.
	HomeSlot *int32
}

// allowAllFence is the default Fence. It allows every well-formed claim,
// because C1 ships no placement authority.
type allowAllFence struct{}

// Check always allows.
func (allowAllFence) Check(_ context.Context, _ ClaimRequest) (FenceResult, error) {
	return FenceResult{Allow: true}, nil
}

package bus

// OwnerMove is the body of one owner move signal on LedgerMoveSubject. The
// recorder publishes it after a transaction that moved a run's owner
// commits. It names the run alone: a gateway drops its cached owner of the
// run and reads the ledger again. A gateway never takes an owner from a
// signal, so the body carries none.
type OwnerMove struct {
	RunID string `json:"runId"`
}

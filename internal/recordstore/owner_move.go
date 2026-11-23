package recordstore

import (
	"encoding/json"
	"fmt"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// PublishOwnerMove publishes one owner move signal for runID on the move
// subject of cluster, the cluster that the run's ledger row names. Only a
// gateway of that cluster can hold a cached match for the run.
//
// The caller publishes after the transaction that moved the owner
// commits, never inside it: a gateway that reads the ledger on the signal
// must find the new owner, and a rolled-back move must send nothing. The
// signal is a core publish with no stream and no retry. A gateway replica
// that misses it keeps the owner cache lifetimes, so a lost signal is
// never worse than no signal. A nil conn publishes nothing.
func PublishOwnerMove(conn *bus.Conn, cluster, runID string) error {
	if conn == nil {
		return nil
	}
	if !natsSubjectToken(cluster) || runID == "" {
		return fmt.Errorf("publish owner move: unusable cluster %q or run %q", cluster, runID)
	}
	data, err := json.Marshal(bus.OwnerMove{RunID: runID})
	if err != nil {
		return fmt.Errorf("marshal owner move: %w", err)
	}
	if err := conn.Publish(bus.LedgerMoveSubject(cluster), data); err != nil {
		return fmt.Errorf("publish owner move: %w", err)
	}
	return nil
}

package mite

import (
	"encoding/json"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// WatchOwnerMoves subscribes this replica to the recorder's owner move
// signal of its cluster (bus.LedgerMoveSubject). Each signal drops the
// cached owner of the named run, so the next check of the run reads the
// ledger. cmd/gateway calls it once, after SetLogStore and before the
// gateway serves, because the listener reads h.ownerCache with no lock.
//
// The subscription has no queue group: every replica must drop its own
// entry. The listener subscribes again after a denial, at start or after
// an authorization reload. A signal that a replica misses leaves the entry
// to its own lifetime and to the per-caller re-read of needsRead, so a
// lost signal is never worse than no signal. A handler with no bus
// connection subscribes nothing.
func (h *BusHandler) WatchOwnerMoves() error {
	if h.conn == nil || h.ownerMoves != nil {
		return nil
	}
	l, err := h.conn.Listen(bus.LedgerMoveSubject(h.cluster), "", func(_ string, data []byte) {
		h.onOwnerMove(data)
	})
	if err != nil {
		return err
	}
	h.ownerMoves = l
	return nil
}

// stopOwnerMoves stops the owner move listener, if one runs.
func (h *BusHandler) stopOwnerMoves() {
	if h.ownerMoves != nil {
		_ = h.ownerMoves.Unsubscribe()
	}
}

// onOwnerMove applies one owner move signal. It drops the run's cached
// owner, its refusal marks, and refusedUntil, and it fails each ledger
// read of the run that is in flight. It sets no owner: the signal only
// forces a re-read, so a sender on the bus can never name an owner. A
// signal for a run with no entry and no read in flight changes nothing.
func (h *BusHandler) onOwnerMove(data []byte) {
	var move bus.OwnerMove
	if err := json.Unmarshal(data, &move); err != nil || move.RunID == "" {
		h.log().Warn("dropping an unreadable owner move signal", "error", err)
		return
	}
	if h.ownerCache == nil {
		return
	}
	h.ownerCache.invalidate(move.RunID)
	h.log().Debug("owner move signal dropped the cached owner", "run_id", move.RunID)
}

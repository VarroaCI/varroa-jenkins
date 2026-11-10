package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// replayLedgerTTL is the age to which the mite remembers an accepted replay.
// It equals the imperative stream MaxAge, so the operator cannot resend a
// command the stream still holds after the mite forgot it.
const replayLedgerTTL = 24 * time.Hour

// replayLedgerCap bounds the ledger. The oldest entry goes first.
const replayLedgerCap = 4096

// replayAccepted is the answer of an earlier accepted replay.
type replayAccepted struct {
	status int
	at     time.Time
}

// replayLedger remembers the replays that Jenkins accepted with a 2xx
// answer. A repeat of an accepted replay answers the earlier status and
// posts nothing, so one webhook delivery builds once.
//
// The ledger lives in memory only. A mite restart clears it, so a replay
// that the operator sends after a restart can still post a second time.
type replayLedger struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]replayAccepted
	order   []string
}

// newReplayLedger returns an empty ledger that reads time from now.
func newReplayLedger(now func() time.Time) *replayLedger {
	return &replayLedger{now: now, entries: make(map[string]replayAccepted)}
}

// accepted reports the status of an earlier accepted replay under key. It
// answers false for an unknown key, an expired entry, or a nil ledger.
func (l *replayLedger) accepted(key string) (int, bool) {
	if l == nil || key == "" {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || l.now().Sub(entry.at) >= replayLedgerTTL {
		return 0, false
	}
	return entry.status, true
}

// record remembers that the replay under key was accepted with status. It
// does nothing for a nil ledger or an empty key.
func (l *replayLedger) record(key string, status int) {
	if l == nil || key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	if _, ok := l.entries[key]; !ok {
		l.order = append(l.order, key)
	}
	l.entries[key] = replayAccepted{status: status, at: now}
	for len(l.order) > replayLedgerCap {
		delete(l.entries, l.order[0])
		l.order = l.order[1:]
	}
}

// prune drops expired entries from the front of the insertion order. The
// order follows the clock, so the front holds the oldest entry. The caller
// holds mu.
func (l *replayLedger) prune(now time.Time) {
	for len(l.order) > 0 {
		entry := l.entries[l.order[0]]
		if now.Sub(entry.at) < replayLedgerTTL {
			return
		}
		delete(l.entries, l.order[0])
		l.order = l.order[1:]
	}
}

// replayKey names one accepted replay. It joins the command identifier, the
// delivery identifier and a short hash of the body. The operator reuses the
// command identifier, which is the stream sequence, on every resend, so the
// key matches a resend of the same delivery and nothing else. It answers
// the empty key, which the ledger ignores, when the command has no
// identifier.
func replayKey(cmd *mitev1.ImperativeCommand) string {
	if cmd == nil || cmd.CommandId == "" || cmd.CommandId == "0" {
		return ""
	}
	var deliveryID string
	var body []byte
	if cmd.ReplayWebhook != nil {
		deliveryID = cmd.ReplayWebhook.DeliveryId
		body = cmd.ReplayWebhook.Body
	}
	sum := sha256.Sum256(body)
	return cmd.CommandId + "|" + deliveryID + "|" + hex.EncodeToString(sum[:8])
}

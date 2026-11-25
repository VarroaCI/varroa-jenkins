package mite

import (
	"context"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// readLoopRecv is the read loop state while the loop waits for the next
// mite message. Every other state is the oneof field name of the message
// that the loop handles, for example "log_chunk".
const readLoopRecv = bus.PresenceReadLoopRecv

// silentReadLoop is the read-loop state of a silent stream, as the presence
// record carries it: the state and the time at which the loop entered it.
type silentReadLoop struct {
	state string
	since time.Time
}

// presenceRefreshInterval returns the interval of the presence timer.
func (h *BusHandler) presenceRefreshInterval() time.Duration {
	if h.presenceRefreshEvery > 0 {
		return h.presenceRefreshEvery
	}
	return bus.PresenceRefreshInterval
}

// presenceSilentLimit returns the heartbeat age past which the presence
// timer reports a silent stream.
func (h *BusHandler) presenceSilentLimit() time.Duration {
	if h.presenceSilentAfter > 0 {
		return h.presenceSilentAfter
	}
	return bus.PresenceStaleAfter
}

// KeepPresence writes the presence record of one open stream on a timer,
// until ctx ends, token no longer names the live connection, or the key
// holds a record of a newer epoch. A write carries the time of the last
// mite message that refreshed liveness, never the time of the write. The
// record of a silent stream therefore stays readable and reads stale. A
// timer write is a compare-and-set write (casPresence), so the timer of a
// half-open old stream on one replica never replaces the record that
// another replica wrote for the newer stream.
//
// The server runs KeepPresence on the stream context and waits for it to
// return before OnDisconnect, so no write of this stream lands after the
// disconnect delete. gRPC ends the stream context when the peer goes away,
// also while a handler blocks the read loop. A gateway that lost the
// stream therefore stops the writes, and the record ages out after
// bus.PresenceTTL.
//
// loop reports what the read loop of the stream does, and since when. A
// stream with no heartbeat for longer than the silent limit puts that
// state in the presence record (bus.Presence.ReadLoop), so the operator can
// name it after the gateway log is gone. It also logs one WARN line that
// names the state, and one more line after each further limit. The first
// heartbeat after a WARN line logs one INFO line.
func (h *BusHandler) KeepPresence(ctx context.Context, name, ns string, token int64, loop func() (string, time.Time)) {
	every := h.presenceRefreshInterval()
	limit := h.presenceSilentLimit()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	key := ns + "/" + name
	var warnedAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// The select picks at random when both cases are ready, so an
		// ended stream must not write once more.
		if ctx.Err() != nil {
			return
		}
		seen, ok := h.refreshPresence(name, ns, token, every, limit, loop)
		if !ok {
			return
		}
		now := time.Now()
		silent := now.Sub(seen)
		if silent <= limit {
			if !warnedAt.IsZero() {
				h.log().Info("mite stream heard again", "controller", key, "lastHeartbeat", seen)
				warnedAt = time.Time{}
			}
			continue
		}
		if !warnedAt.IsZero() && now.Sub(warnedAt) < limit {
			continue
		}
		state, since := loop()
		h.log().Warn("mite stream open with no heartbeat",
			"controller", key, "silentFor", silent.Round(time.Second), "lastHeartbeat", seen,
			"readLoop", state, "readLoopFor", now.Sub(since).Round(time.Second))
		warnedAt = now
	}
}

// refreshPresence writes the presence record of the connection that token
// names, and returns the time of the last mite message that refreshed
// liveness. A heartbeat older than limit makes the stream silent: the
// record then carries the read-loop state that loop reports
// (noteSilentReadLoopLocked). It skips the write when an accepted write is
// younger than every and that state did not change. The write is guarded,
// so it never replaces a record of a newer epoch. ok is false when token no
// longer names the live connection, or when the key holds a record of a
// newer epoch: a newer stream of the mite is open, on this gateway or on
// another replica, and the timer stops for good.
func (h *BusHandler) refreshPresence(name, ns string, token int64, every, limit time.Duration, loop func() (string, time.Time)) (time.Time, bool) {
	key := ns + "/" + name
	state, since := loop()
	h.mu.Lock()
	current, claimed := h.connectEpoch[key]
	_, connected := h.cancels[key]
	live := connected && (token == 0 || (claimed && current == token))
	seen := h.lastHeartbeat[key]
	written, wrote := h.presenceWrittenAt[key]
	noted := false
	if live {
		noted = h.noteSilentReadLoopLocked(key, seen, limit, silentReadLoop{state: state, since: since})
	}
	h.mu.Unlock()
	if !live {
		return time.Time{}, false
	}
	if wrote && !noted && time.Since(written) < every {
		return seen, true
	}
	if h.writePresence(ns, name, token, presenceGuarded) == presenceNewer {
		h.log().Info("presence timer stopped: a newer stream holds the record", "controller", key, "epoch", token)
		return seen, false
	}
	return seen, true
}

// noteSilentReadLoopLocked keeps the read-loop note of key in step with
// the heartbeat age: a heartbeat older than limit records loop, and a
// fresher heartbeat clears the note. It reports whether the note changed.
// The caller holds h.mu, the lock that touchLiveness takes, so a heartbeat
// that arrives at the same time always clears the note.
func (h *BusHandler) noteSilentReadLoopLocked(key string, seen time.Time, limit time.Duration, loop silentReadLoop) bool {
	cur, had := h.silentReadLoop[key]
	if time.Since(seen) <= limit {
		if had {
			delete(h.silentReadLoop, key)
		}
		return had
	}
	if had && cur.state == loop.state && cur.since.Equal(loop.since) {
		return false
	}
	if h.silentReadLoop == nil {
		h.silentReadLoop = make(map[string]silentReadLoop)
	}
	h.silentReadLoop[key] = loop
	return true
}

package mite

import (
	"context"
	"sort"
	"time"
)

// Bounds of the finalized markers that the gateway sends again after the
// final STORED ack.
const (
	// logMarkerResendAge bounds how long the gateway keeps an owed marker.
	// Each idle sweep round (one minute) sends a marker at most once, so a
	// marker gets at most about 30 resends. After that, the recorder's
	// hourly finalizeStaleLogs sweep reads the final index entry and
	// finalizes the run.
	logMarkerResendAge = logTombstoneTTL

	// logMarkersOwedMax bounds the owed markers. The gateway keeps no
	// marker over the bound, and the recorder's sweep finalizes its run
	// from the stored index.
	logMarkersOwedMax = 10000
)

// owedFinalizedMarker is a finalized marker that did not reach the bus
// before the final STORED ack. The final index entry is durable, so the
// marker repeats a fact that the store holds.
type owedFinalizedMarker struct {
	ns, name, jobPath string
	// since is when the marker became owed, on h.logClock.
	since time.Time
	// lastTry is when a resend pass last tried the marker, on h.logClock.
	// A pass tries the markers with the oldest value first.
	lastTry time.Time
}

// logStopping reports whether Close ran.
func (h *BusHandler) logStopping() bool {
	select {
	case <-h.logStop:
		return true
	default:
		return false
	}
}

// oweFinalizedMarker records that the finalized marker of runID did not
// reach the bus before the final STORED ack. A gateway restart loses the
// record. The mite already released the final chunk, so the recorder's
// finalizeStaleLogs sweep then finalizes the run from the stored final
// index entry.
func (h *BusHandler) oweFinalizedMarker(ns, name, runID, jobPath string) {
	now := h.logClock()
	h.logMarkerMu.Lock()
	if h.logMarkersOwed == nil {
		h.logMarkersOwed = make(map[string]*owedFinalizedMarker)
	}
	_, known := h.logMarkersOwed[runID]
	full := !known && len(h.logMarkersOwed) >= logMarkersOwedMax
	if !known && !full {
		h.logMarkersOwed[runID] = &owedFinalizedMarker{ns: ns, name: name, jobPath: jobPath, since: now}
	}
	h.logMarkerMu.Unlock()
	if full {
		h.log().Warn("too many owed finalized markers, this marker is not sent again",
			"run_id", runID, "controller", ns+"/"+name, "max", logMarkersOwedMax)
		return
	}
	h.log().Warn("the finalized marker did not reach the bus before the final ack, the idle sweep sends it again",
		"run_id", runID, "controller", ns+"/"+name)
}

// resendOwedMarkers sends each owed finalized marker again, the marker
// tried longest ago first. It drops a marker older than
// logMarkerResendAge with no publish. The first failed publish ends the
// pass: the bus is most likely still down, and each failure can hold the
// pass for one publish wait. The pass stamps lastTry before each publish,
// so a marker that keeps failing goes behind each marker that the pass
// did not reach. It holds no lock while it publishes.
func (h *BusHandler) resendOwedMarkers(ctx context.Context) {
	now := h.logClock()
	type owed struct {
		runID string
		m     owedFinalizedMarker
	}
	var items []owed
	var expired []string
	h.logMarkerMu.Lock()
	for runID, m := range h.logMarkersOwed {
		if now.Sub(m.since) >= logMarkerResendAge {
			delete(h.logMarkersOwed, runID)
			expired = append(expired, runID)
			continue
		}
		items = append(items, owed{runID: runID, m: *m})
	}
	h.logMarkerMu.Unlock()

	for _, runID := range expired {
		h.log().Warn("an owed finalized marker reached its age bound, the recorder's sweep finalizes the run",
			"run_id", runID)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].m.lastTry.Equal(items[j].m.lastTry) {
			return items[i].m.lastTry.Before(items[j].m.lastTry)
		}
		return items[i].runID < items[j].runID
	})
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		h.logMarkerMu.Lock()
		if m, ok := h.logMarkersOwed[it.runID]; ok {
			m.lastTry = now
		}
		h.logMarkerMu.Unlock()
		if !h.publishRunLogBestEffort(it.m.ns, it.m.name, it.runID, it.m.jobPath, map[string]interface{}{"finalized": true}) {
			h.log().Warn("the resend of an owed finalized marker failed, the pass ends", "run_id", it.runID)
			return
		}
		h.logMarkerMu.Lock()
		delete(h.logMarkersOwed, it.runID)
		h.logMarkerMu.Unlock()
		h.log().Info("sent an owed finalized marker", "run_id", it.runID, "controller", it.m.ns+"/"+it.m.name)
	}
}

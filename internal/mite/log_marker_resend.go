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

	// logMarkerResendPassMax bounds the publishes of one resend pass. The
	// idle sweep round runs after the pass on the same goroutine, so the
	// bound also limits how long a slow bus holds the round. A full owed
	// set takes logMarkersOwedMax/logMarkerResendPassMax passes (20
	// minutes), which is less than logMarkerResendAge. A bus that takes
	// each publish therefore gets each owed marker before its age bound.
	logMarkerResendPassMax = 500
)

// owedFinalizedMarker is a finalized marker that did not reach the bus
// before the final STORED ack. The final index entry is durable, so the
// marker repeats a fact that the store holds.
type owedFinalizedMarker struct {
	ns, name, jobPath string
	// since is when the marker became owed, on h.logClock.
	since time.Time
	// lastTry is when a publish of the marker was last tried, on
	// h.logClock. oweFinalizedMarker sets it to since, because the drain
	// tried the marker until then. A pass tries the markers with the
	// oldest value first, so a marker owed later never goes ahead of a
	// marker that a pass tried before it.
	lastTry time.Time
}

// owedResend is one owed marker that a resend pass sends.
type owedResend struct {
	runID string
	m     owedFinalizedMarker
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
		h.logMarkersOwed[runID] = &owedFinalizedMarker{ns: ns, name: name, jobPath: jobPath, since: now, lastTry: now}
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

// resendOwedMarkers runs one resend pass. It drops each owed marker
// older than logMarkerResendAge with no publish. It then sends at most
// logMarkerResendPassMax owed markers again, the marker tried longest ago
// first, and keeps the others for the next pass. The first failed
// publish ends the pass: the bus is most likely still down, and each
// failure can hold the pass for one publish wait. The pass stamps lastTry
// before each publish, so a marker that keeps failing goes behind each
// marker that the pass did not reach. It holds no lock while it
// publishes. It writes one summary line, and no line for each marker.
func (h *BusHandler) resendOwedMarkers(ctx context.Context) {
	now := h.logClock()
	items, dropped := h.owedMarkersForPass(now)
	sent, failed := 0, 0
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		h.logMarkerMu.Lock()
		if m, ok := h.logMarkersOwed[it.runID]; ok {
			m.lastTry = now
		}
		h.logMarkerMu.Unlock()
		if !h.publishRunLogBestEffort(it.m.ns, it.m.name, it.runID, it.m.jobPath, map[string]interface{}{"finalized": true}) {
			failed++
			break
		}
		h.logMarkerMu.Lock()
		delete(h.logMarkersOwed, it.runID)
		h.logMarkerMu.Unlock()
		sent++
	}
	h.logResendPass(sent, failed, dropped)
}

// owedMarkersForPass deletes each owed marker older than
// logMarkerResendAge and returns how many it deleted. It returns at most
// logMarkerResendPassMax of the other markers, the marker tried longest
// ago first.
func (h *BusHandler) owedMarkersForPass(now time.Time) ([]owedResend, int) {
	var items []owedResend
	dropped := 0
	h.logMarkerMu.Lock()
	for runID, m := range h.logMarkersOwed {
		if now.Sub(m.since) >= logMarkerResendAge {
			delete(h.logMarkersOwed, runID)
			dropped++
			continue
		}
		items = append(items, owedResend{runID: runID, m: *m})
	}
	h.logMarkerMu.Unlock()

	sort.Slice(items, func(i, j int) bool {
		if !items[i].m.lastTry.Equal(items[j].m.lastTry) {
			return items[i].m.lastTry.Before(items[j].m.lastTry)
		}
		return items[i].runID < items[j].runID
	})
	if len(items) > logMarkerResendPassMax {
		items = items[:logMarkerResendPassMax]
	}
	return items, dropped
}

// logResendPass writes the summary line of one resend pass, with the
// owed markers left after it. A pass that dropped no marker and tried
// none writes no line. A failed publish or a dropped marker makes the
// line a warning: the recorder's sweep finalizes each dropped run from
// the stored index.
func (h *BusHandler) logResendPass(sent, failed, dropped int) {
	if sent+failed+dropped == 0 {
		return
	}
	h.logMarkerMu.Lock()
	owed := len(h.logMarkersOwed)
	h.logMarkerMu.Unlock()
	write := h.log().Info
	if failed > 0 || dropped > 0 {
		write = h.log().Warn
	}
	write("a resend pass of the owed finalized markers ended",
		"sent", sent, "failed", failed, "dropped_by_age", dropped, "owed", owed, "max", logMarkerResendPassMax)
}

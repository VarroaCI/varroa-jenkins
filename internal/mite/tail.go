package mite

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Live-tail bounds (design.md D5): the lease window, the wait for a
// TailAck after a push, and how long a tail error stays reportable.
const (
	tailLeaseSeconds  = 90
	tailLeaseDuration = tailLeaseSeconds * time.Second
	tailAckWait       = 10 * time.Second
	tailErrorTTL      = 5 * time.Minute

	// tailQueueGroup groups the gateway's per-connection TailSubject
	// subscription. Only one gateway process ever holds a given mite's
	// stream, so the group only matters for the brief overlap between a
	// superseded connection's teardown and its replacement's setup — it
	// keeps a request from reaching both at once.
	tailQueueGroup = "gateway"
)

// runTailLease is one run's live-tail lease: whether it is live, the
// control_id of the most recently pushed TailControl, and the job
// identity captured at lease creation (TailControl needs it on every
// push, but the BFF's request carries it only when it starts a lease).
// Every terminal condition removes the run's entry from h.tailLeases
// outright (design.md D5's lease table), so this struct never
// accumulates past the run it belongs to; a kept tail error survives
// that removal in h.tailErrors instead.
type runTailLease struct {
	ns, name string

	mu          sync.Mutex
	active      bool
	controlID   string
	jobPath     string
	buildNumber int64

	// expiryTimer fires tailLeaseDuration after the last renewal, with no
	// further renewal. ackTimer fires tailAckWait after a push with no
	// matching TailAck. Both are reset on every push and stopped on every
	// clear, so a stale firing is caught by the control_id/active guard in
	// its callback rather than acted on.
	expiryTimer *time.Timer
	ackTimer    *time.Timer
}

// tailErrorEntry is a kept tail error: the reason a lease last failed,
// and how long that reason stays reportable. It survives the lease
// being deleted, so the next start request for the same run_id still
// answers with it (design.md D5, "The gateway returns a kept error on
// the next request for that run").
type tailErrorEntry struct {
	msg     string
	expires time.Time
}

// tailRequest is the body of a BFF request on TailSubject. jobPath and
// buildNumber are carried on a start request so the gateway can populate
// TailControl's required fields; a stop request needs neither, because
// the gateway already holds them from the lease's last start.
type tailRequest struct {
	RunID       string `json:"runId"`
	Action      string `json:"action"`
	JobPath     string `json:"jobPath"`
	BuildNumber int64  `json:"buildNumber"`
}

// tailReply is the body the gateway answers a TailSubject request with.
type tailReply struct {
	OK           bool   `json:"ok"`
	LeaseSeconds int    `json:"leaseSeconds,omitempty"`
	Error        string `json:"error,omitempty"`
}

// tailLeaseTimeoutOrDefault returns the configured no-renewal expiry
// window, or the design default when the handler was never overridden.
func (h *BusHandler) tailLeaseTimeoutOrDefault() time.Duration {
	if h.tailLeaseTimeout <= 0 {
		return tailLeaseDuration
	}
	return h.tailLeaseTimeout
}

// tailAckTimeoutOrDefault returns the configured ack-wait window, or the
// design default when the handler was never overridden.
func (h *BusHandler) tailAckTimeoutOrDefault() time.Duration {
	if h.tailAckTimeout <= 0 {
		return tailAckWait
	}
	return h.tailAckTimeout
}

// tailErrTTLOrDefault returns the configured kept-error lifetime, or the
// design default when the handler was never overridden.
func (h *BusHandler) tailErrTTLOrDefault() time.Duration {
	if h.tailErrTTL <= 0 {
		return tailErrorTTL
	}
	return h.tailErrTTL
}

func marshalTailReply(r tailReply) []byte {
	data, err := json.Marshal(r)
	if err != nil {
		// tailReply has no field that can fail to marshal; this is
		// unreachable in practice.
		return []byte(`{"ok":false,"error":"internal encode failure"}`)
	}
	return data
}

// watchTail subscribes the connected mite's TailSubject with the gateway
// queue group and answers every start/stop request until ctx is
// cancelled, following the retry-until-connected pattern serveDesiredState
// and watchContent use for their own subscriptions. A server denial of the
// subscription rebuilds it through the pacer, because nats.go reports the
// denial only to the connection's error handler.
func (h *BusHandler) watchTail(ctx context.Context, ns, name string, send Sender) {
	if h.conn == nil {
		return
	}
	subject := bus.TailSubject(h.cluster, ns, name)
	pacer := h.newRebuildPacer()
	for {
		var (
			sub     *bus.Sub
			denied  <-chan struct{}
			release func()
		)
		established := h.retryEstablish(ctx, ns, name, "tail", func() error {
			d, rel := h.conn.WatchSubscribeDenied(subject)
			s, err := h.conn.SubscribeRequest(subject, tailQueueGroup, func(data []byte) []byte {
				return h.handleTailRequest(ns, name, send, data)
			})
			if err != nil {
				rel()
				return fmt.Errorf("tail subscribe: %w", err)
			}
			if err := h.conn.ConfirmSubscription(d); err != nil {
				_ = s.Unsubscribe()
				rel()
				return fmt.Errorf("tail subscribe %s: %w", subject, err)
			}
			sub, denied, release = s, d, rel
			return nil
		})
		if !established {
			return // context cancelled: the mite disconnected
		}
		start := time.Now()
		select {
		case <-ctx.Done():
			_ = sub.Unsubscribe()
			release()
			return
		case <-denied:
		}
		_ = sub.Unsubscribe()
		release()
		h.log().Error("tail subscription denied", "controller", ns+"/"+name)
		h.markWatchDegraded(ctx, ns, name, "tail", "tail subscription lost; rebuilding: "+bus.ErrSubscribeDenied.Error())
		if !pacer.wait(ctx, time.Since(start)) {
			return
		}
	}
}

// handleTailRequest decodes one TailSubject request and dispatches it.
// Every call also sweeps h.tailErrors of every entry whose ttl has
// passed: getKeptTailError only prunes the one run_id it is asked about,
// so without a sweep elsewhere a long-lived gateway accumulates one
// entry per run that ever failed its tail, forever.
func (h *BusHandler) handleTailRequest(ns, name string, send Sender, data []byte) []byte {
	h.sweepExpiredTailErrors()
	var req tailRequest
	if err := json.Unmarshal(data, &req); err != nil || req.RunID == "" {
		return marshalTailReply(tailReply{OK: false, Error: "invalid tail request"})
	}
	switch req.Action {
	case "start":
		return h.handleTailStart(ns, name, send, req)
	case "stop":
		return h.handleTailStop(ns, name, send, req)
	default:
		return marshalTailReply(tailReply{OK: false, Error: "unknown tail action"})
	}
}

// getOrCreateTailLease returns the run's lease, creating it on first
// touch. ns and name name the connection the lease belongs to.
func (h *BusHandler) getOrCreateTailLease(runID, ns, name string) *runTailLease {
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	if h.tailLeases == nil {
		h.tailLeases = make(map[string]*runTailLease)
	}
	lease, ok := h.tailLeases[runID]
	if !ok {
		lease = &runTailLease{ns: ns, name: name}
		h.tailLeases[runID] = lease
	}
	return lease
}

// startOrRenewTailLease looks the run's lease up (or creates it) and
// activates it with a fresh control_id, all under one h.tailMu critical
// section, with lease.mu nested inside — the same lock order
// expireTailLeaseIfCurrent uses. A start request and a renewal both call
// this.
//
// The lookup and the activation must share one critical section. A
// caller that released tailMu between getting the pointer and locking
// lease.mu to activate it would open a window: expireTailLeaseIfCurrent
// still reads the lease's old control_id in that window, finds it
// unchanged and the lease still active, and deletes the lease from
// h.tailLeases before the activation lands. The renewal would then
// activate an orphaned lease object and report success while
// h.tailLeases no longer holds it — invisible to OnTailAck,
// isTailActive, and every later expiry. Holding tailMu across both steps
// closes that window: expireTailLeaseIfCurrent cannot run inside it, so
// the lookup either finds the still-current lease, or finds none because
// a prior expiry already removed it, in which case this call creates a
// fresh one instead of reviving a stale pointer.
func (h *BusHandler) startOrRenewTailLease(runID, ns, name, jobPath string, buildNumber int64) (lease *runTailLease, controlID, outJobPath string, outBuildNumber int64) {
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	if h.tailLeases == nil {
		h.tailLeases = make(map[string]*runTailLease)
	}
	lease, ok := h.tailLeases[runID]
	if !ok {
		lease = &runTailLease{ns: ns, name: name}
		h.tailLeases[runID] = lease
	}

	lease.mu.Lock()
	if jobPath != "" {
		lease.jobPath = jobPath
	}
	if buildNumber != 0 {
		lease.buildNumber = buildNumber
	}
	controlID = uuid.NewString()
	lease.controlID = controlID
	lease.active = true
	outJobPath, outBuildNumber = lease.jobPath, lease.buildNumber
	lease.mu.Unlock()

	return lease, controlID, outJobPath, outBuildNumber
}

// getExistingTailLease returns the run's lease, or nil if the run has
// never been tailed on this connection, or its lease already cleared.
func (h *BusHandler) getExistingTailLease(runID string) *runTailLease {
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	return h.tailLeases[runID]
}

// setKeptTailError records runID's kept tail error, reportable for ttl.
func (h *BusHandler) setKeptTailError(runID, msg string, ttl time.Duration) {
	h.tailMu.Lock()
	if h.tailErrors == nil {
		h.tailErrors = make(map[string]tailErrorEntry)
	}
	h.tailErrors[runID] = tailErrorEntry{msg: msg, expires: time.Now().Add(ttl)}
	h.tailMu.Unlock()
}

// clearKeptTailError removes runID's kept tail error, if any: a later
// TailAck reporting active true clears it (design.md D5).
func (h *BusHandler) clearKeptTailError(runID string) {
	h.tailMu.Lock()
	delete(h.tailErrors, runID)
	h.tailMu.Unlock()
}

// getKeptTailError returns runID's kept tail error, if one is still
// live, evicting it in place once its ttl has passed.
func (h *BusHandler) getKeptTailError(runID string) (string, bool) {
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	e, ok := h.tailErrors[runID]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expires) {
		delete(h.tailErrors, runID)
		return "", false
	}
	return e.msg, true
}

// sweepExpiredTailErrors removes every h.tailErrors entry whose ttl has
// already passed, regardless of which run_id a caller is asking about.
// getKeptTailError prunes lazily, one run_id at a time, on query — a run
// that never asks again would otherwise keep its entry forever. Called
// from every tail start/stop request and from the two lease timers, so
// the map stays bounded whether or not read traffic ever touches a given
// entry again.
func (h *BusHandler) sweepExpiredTailErrors() {
	now := time.Now()
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	for runID, e := range h.tailErrors {
		if now.After(e.expires) {
			delete(h.tailErrors, runID)
		}
	}
}

// handleTailStart handles a start request: it records the 90-second
// lease, mints a control_id, pushes TailControl, and replies without
// waiting for the mite's acknowledgement. A caller that still holds a
// kept tail error for this run is answered that error instead — the
// gateway does not retry a push while an error is live for it.
func (h *BusHandler) handleTailStart(ns, name string, send Sender, req tailRequest) []byte {
	if errMsg, ok := h.getKeptTailError(req.RunID); ok {
		return marshalTailReply(tailReply{OK: false, Error: errMsg})
	}

	lease, controlID, jobPath, buildNumber := h.startOrRenewTailLease(req.RunID, ns, name, req.JobPath, req.BuildNumber)

	h.pushTailControl(ns, name, send, req.RunID, jobPath, buildNumber, "start", controlID)
	h.resetTailTimers(lease, send, req.RunID, ns, name, controlID)

	return marshalTailReply(tailReply{OK: true, LeaseSeconds: tailLeaseSeconds})
}

// handleTailStop handles a stop request: it clears the lease and pushes a
// stop TailControl using the job identity recorded at the lease's last
// start. A run with no live lease, or one already cleared by a race
// (expiry, ack timeout, or a concurrent stop), answers ok with nothing to
// do and pushes nothing further.
func (h *BusHandler) handleTailStop(ns, name string, send Sender, req tailRequest) []byte {
	jobPath, buildNumber, ok := h.stopTailLease(req.RunID)
	if !ok {
		return marshalTailReply(tailReply{OK: true, LeaseSeconds: tailLeaseSeconds})
	}
	h.pushTailControl(ns, name, send, req.RunID, jobPath, buildNumber, "stop", uuid.NewString())
	return marshalTailReply(tailReply{OK: true, LeaseSeconds: tailLeaseSeconds})
}

// stopTailLease marks runID's lease inactive and removes it from
// h.tailLeases, all under one h.tailMu critical section with lease.mu
// nested — the same lock order expireTailLeaseIfCurrent uses. ok is false
// when the run has no lease, or its lease is already inactive.
//
// A lookup that released h.tailMu before this section's delete would open
// the same window expireTailLeaseIfCurrent's doc comment describes for a
// renewal: another goroutine could delete this run's lease and a
// renewal could install a fresh one under the same run_id before this
// call's own delete ran, and an unconditional delete-by-run_id would then
// remove the fresh lease instead of the one this call actually looked at.
// Never releasing h.tailMu between the lookup and the delete closes that
// window: the entry this call deletes is always the one it just found.
func (h *BusHandler) stopTailLease(runID string) (jobPath string, buildNumber int64, ok bool) {
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	lease, exists := h.tailLeases[runID]
	if !exists {
		return "", 0, false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.active {
		return "", 0, false
	}
	lease.active = false
	jobPath, buildNumber = lease.jobPath, lease.buildNumber
	stopExpiryTimer(lease)
	stopAckTimer(lease)
	delete(h.tailLeases, runID)
	return jobPath, buildNumber, true
}

// pushTailControl sends one OperatorMessage.tail_control to the
// controller's current stream. It is a no-op when send is nil, which
// only happens if the mite disconnected between the subscription
// firing and this call.
func (h *BusHandler) pushTailControl(ns, name string, send Sender, runID, jobPath string, buildNumber int64, action, controlID string) {
	if send == nil {
		return
	}
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_TailControl{TailControl: &mitev1.TailControl{
			RunId:        runID,
			JobPath:      jobPath,
			BuildNumber:  buildNumber,
			Action:       action,
			LeaseSeconds: tailLeaseSeconds,
			ControlId:    controlID,
		}},
	}
	if err := send(msg); err != nil {
		h.log().Error("send tail control failed", "controller", ns+"/"+name, "run_id", runID, "action", action, "error", err)
	}
}

// resetTailTimers (re)starts the lease's 90-second no-renewal timer and
// its 10-second ack-wait timer, stopping whichever ones a prior push
// started. Caller must not hold lease.mu.
func (h *BusHandler) resetTailTimers(lease *runTailLease, send Sender, runID, ns, name, controlID string) {
	lease.mu.Lock()
	stopExpiryTimer(lease)
	lease.expiryTimer = time.AfterFunc(h.tailLeaseTimeoutOrDefault(), func() {
		h.onTailLeaseExpired(send, runID, ns, name, controlID)
	})
	stopAckTimer(lease)
	lease.ackTimer = time.AfterFunc(h.tailAckTimeoutOrDefault(), func() {
		h.onTailAckTimeout(runID, controlID)
	})
	lease.mu.Unlock()
}

// stopExpiryTimer stops and clears lease.expiryTimer. Caller must hold
// lease.mu.
func stopExpiryTimer(lease *runTailLease) {
	if lease.expiryTimer != nil {
		lease.expiryTimer.Stop()
		lease.expiryTimer = nil
	}
}

// stopAckTimer stops and clears lease.ackTimer. Caller must hold
// lease.mu.
func stopAckTimer(lease *runTailLease) {
	if lease.ackTimer != nil {
		lease.ackTimer.Stop()
		lease.ackTimer = nil
	}
}

// expireTailLeaseIfCurrent atomically validates runID's lease against
// controlID and, only if it is still the active one, marks it inactive
// and removes it from h.tailLeases in the same critical section. A
// separate validate-then-delete sequence leaves a window between them: a
// renewal can land in it, and an unconditional delete afterward would
// then destroy the renewed lease and let a stale stop message reach the
// mite. Holding tailMu across the whole check closes that window —
// startOrRenewTailLease needs the same lock to install a renewal, so a
// renewal racing this call either finishes first (and the controlID
// check below then sees it and refuses) or waits for this call to finish
// first (and finds no lease left to renew, so it starts a fresh one).
// Either ordering is correct; neither can interleave.
//
// otherTimer stops whichever of the lease's two timers did not just
// fire: onTailLeaseExpired passes stopAckTimer, and onTailAckTimeout
// passes stopExpiryTimer.
func (h *BusHandler) expireTailLeaseIfCurrent(runID, controlID string, otherTimer func(*runTailLease)) (jobPath string, buildNumber int64, ok bool) {
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	lease, exists := h.tailLeases[runID]
	if !exists {
		return "", 0, false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.controlID != controlID || !lease.active {
		return "", 0, false
	}
	lease.active = false
	jobPath, buildNumber = lease.jobPath, lease.buildNumber
	otherTimer(lease)
	delete(h.tailLeases, runID)
	return jobPath, buildNumber, true
}

// onTailLeaseExpired fires 90 seconds after a push with no renewal. It
// clears the lease and pushes a stop control message, unless a later
// push (a renewal, or a stop) has already superseded controlID — in
// which case a renewal that raced this exact timer keeps its own lease,
// and no stop reaches the mite for it.
func (h *BusHandler) onTailLeaseExpired(send Sender, runID, ns, name, controlID string) {
	h.sweepExpiredTailErrors()
	jobPath, buildNumber, ok := h.expireTailLeaseIfCurrent(runID, controlID, stopAckTimer)
	if !ok {
		return
	}
	h.pushTailControl(ns, name, send, runID, jobPath, buildNumber, "stop", uuid.NewString())
}

// onTailAckTimeout fires 10 seconds after a push with no matching
// TailAck. It clears the lease and records a kept error, unless a later
// push, a TailAck, or a clear has already superseded controlID.
func (h *BusHandler) onTailAckTimeout(runID, controlID string) {
	h.sweepExpiredTailErrors()
	if _, _, ok := h.expireTailLeaseIfCurrent(runID, controlID, stopExpiryTimer); !ok {
		return
	}
	h.setKeptTailError(runID, "no acknowledgement within 10 seconds", h.tailErrTTLOrDefault())
}

// OnTailAck implements StreamHandler for a mite's tail-control
// acknowledgement. A control_id that is not the run's current one is
// ignored, so a late acknowledgement never clears a newer lease. An
// active=false acknowledgement clears the lease and keeps the error for
// five minutes; an active=true acknowledgement clears any kept error.
//
// The lookup, the control_id check, the state update, and the delete all
// run under one h.tailMu critical section, with lease.mu nested inside —
// the same lock order expireTailLeaseIfCurrent and startOrRenewTailLease
// use. A version that released h.tailMu after the lookup and only
// re-acquired it for the later delete would open a window: an expiry
// could delete this run's lease from h.tailLeases and a renewal could
// then install a fresh lease under the same run_id, both before this
// call's delete ran. This call's own control_id check would still pass —
// it is checking the stale lease object it already holds, not the map's
// current entry — so an unconditional delete-by-run_id afterward would
// remove the renewal's fresh lease under a stale ack's authority. Holding
// h.tailMu across the whole sequence closes that window: the entry this
// call deletes is always the one it just validated.
func (h *BusHandler) OnTailAck(name, ns string, ack *mitev1.TailAck) {
	if ack == nil || ack.RunId == "" {
		return
	}

	h.tailMu.Lock()
	defer h.tailMu.Unlock()

	lease, exists := h.tailLeases[ack.RunId]
	if !exists {
		return
	}
	lease.mu.Lock()
	if lease.controlID != ack.ControlId {
		lease.mu.Unlock()
		return
	}
	stopAckTimer(lease)
	if ack.Active {
		lease.mu.Unlock()
		delete(h.tailErrors, ack.RunId)
		return
	}
	lease.active = false
	stopExpiryTimer(lease)
	lease.mu.Unlock()
	delete(h.tailLeases, ack.RunId)

	errMsg := ack.Error
	if errMsg == "" {
		errMsg = "tail failed"
	}
	if h.tailErrors == nil {
		h.tailErrors = make(map[string]tailErrorEntry)
	}
	h.tailErrors[ack.RunId] = tailErrorEntry{msg: errMsg, expires: time.Now().Add(h.tailErrTTLOrDefault())}
}

// isTailActive reports whether runID currently has a live tail lease.
func (h *BusHandler) isTailActive(runID string) bool {
	lease := h.getExistingTailLease(runID)
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.active
}

// clearTailLeaseOnFinal clears runID's lease because the run has ended.
// It pushes nothing: the build the mite would tail is already over.
//
// The lookup and the delete run under one h.tailMu critical section, with
// lease.mu nested inside, the same lock order stopTailLease and
// expireTailLeaseIfCurrent use. A separate fetch-then-later-delete would
// let a lease this call looked at get replaced under the same run_id by a
// fresh renewal before the delete ran, and the unconditional delete would
// then remove the renewal's lease instead of the one this call found.
func (h *BusHandler) clearTailLeaseOnFinal(runID string) {
	h.tailMu.Lock()
	defer h.tailMu.Unlock()
	lease, exists := h.tailLeases[runID]
	if !exists {
		return
	}
	lease.mu.Lock()
	lease.active = false
	stopExpiryTimer(lease)
	stopAckTimer(lease)
	lease.mu.Unlock()
	delete(h.tailLeases, runID)
}

// liveLogChunkPayload is the message the gateway publishes on
// bus.LogSubject for a watched run (design.md D5, "The live-chunk
// payload"; log-live-tail). Every field but RunID and Data repeats the
// chunk's own index entry, so a live viewer needs no index read. Data is
// the raw console bytes; encoding/json encodes a []byte field as base64,
// so the message stays valid JSON for any byte sequence.
type liveLogChunkPayload struct {
	RunID string `json:"runId"`
	LogIndexEntry
	Data []byte `json:"data"`
}

// publishLiveLogChunk relays one durably-stored chunk to LogSubject, for
// a run with an active tail lease. This is the live-tail fan-out:
// best-effort, never retried, and never a substitute for the blob store
// the chunk was just appended to. entry SHALL carry the offset the
// index entry was written with, so the published message matches the
// index line byte for byte.
func (h *BusHandler) publishLiveLogChunk(ns, name, runID string, entry LogIndexEntry, data []byte) {
	if h.conn == nil || !h.isTailActive(runID) {
		return
	}
	payload := liveLogChunkPayload{RunID: runID, LogIndexEntry: entry, Data: data}
	raw, err := json.Marshal(payload)
	if err != nil {
		h.log().Error("marshal live log chunk failed", "controller", ns+"/"+name, "run_id", runID, "seq", entry.Seq, "error", err)
		return
	}
	if err := h.conn.Publish(bus.LogSubject(h.cluster, ns, name, runID), raw); err != nil {
		h.log().Warn("publish live log chunk failed", "controller", ns+"/"+name, "run_id", runID, "seq", entry.Seq, "error", err)
	}
}

// clearTailLeasesForController clears every lease belonging to ns/name's
// connection. Called from OnDisconnect: the mite's own copy of the lease
// expires on its side after lease_seconds, so the gateway pushes nothing
// here — there is no connection left to push to.
func (h *BusHandler) clearTailLeasesForController(ns, name string) {
	h.tailMu.Lock()
	var staleRunIDs []string
	stale := make(map[string]*runTailLease)
	for runID, lease := range h.tailLeases {
		if lease.ns == ns && lease.name == name {
			staleRunIDs = append(staleRunIDs, runID)
			stale[runID] = lease
		}
	}
	for _, runID := range staleRunIDs {
		delete(h.tailLeases, runID)
	}
	h.tailMu.Unlock()

	for _, runID := range staleRunIDs {
		lease := stale[runID]
		lease.mu.Lock()
		lease.active = false
		stopExpiryTimer(lease)
		stopAckTimer(lease)
		lease.mu.Unlock()
	}
}

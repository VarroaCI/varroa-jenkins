package mite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

var (
	miteStreamConnections metric.Int64UpDownCounter
	miteStreamDisconnects metric.Int64Counter
	miteWatchFailures     metric.Int64Counter
)

func init() {
	meter := otel.Meter("varroa-gateway")
	// UpDownCounter, not a Gauge: these are +1/-1 deltas. A synchronous gauge
	// takes last-value, so it would report literally 1 or -1 rather than the
	// number of active connections.
	miteStreamConnections, _ = meter.Int64UpDownCounter("varroa.mite.stream.connections",
		metric.WithDescription("Active mite stream connections"),
	)
	miteStreamDisconnects, _ = meter.Int64Counter("varroa.mite.stream.disconnects",
		metric.WithDescription("Mite stream disconnect count"),
	)
	miteWatchFailures, _ = meter.Int64Counter("varroa.mite.watch.failures",
		metric.WithDescription("Bus watch setup failures per mite connection (bridge is degraded until it succeeds)"),
	)
}

// desiredWatcher is the seam over the desired-state KV bucket. *bus.KV
// satisfies it; tests substitute a fake that fails the Watch() setup call so
// the retry path is exercisable without a live JetStream.
type desiredWatcher interface {
	Watch(key string) (nats.KeyWatcher, error)
}

// BusHandler implements StreamHandler by bridging the mite gRPC stream to the
// NATS bus. It replaces the in-memory Registry for the gateway tier.
//
// Command delivery (operator→mite) uses the last-value mite_desired KV bucket:
// the gateway watches the mite's desired-state key and forwards the current
// value (on attach) plus every update to the gRPC stream. This means a
// reconnecting mite receives only the newest desired state, never a replay of
// stale ones, and there is no per-mite durable consumer to race over.
type BusHandler struct {
	cluster    string
	conn       *bus.Conn
	kv         *bus.KV // snapshot KV
	presenceKV *bus.KV // presence KV (written on connect/heartbeat, deleted on disconnect)
	// desiredKV is the desired-state KV (watched and forwarded to the mite).
	// Nil-valued when the gateway runs without a desired bucket — always
	// compare with desiredKV == nil, never against a typed nil *bus.KV.
	desiredKV desiredWatcher

	activityPub *activity.Publisher // publishes enriched events to activity.* subjects

	mu           sync.Mutex
	cancels      map[string]context.CancelFunc // keyed "ns/name"
	certExpiry   map[string]time.Time          // keyed "ns/name"; set at OnConnect to avoid a KV read on every heartbeat
	connectEpoch map[string]int64              // stable per-connection epoch (ns/name)
	// idleGauges caches the latest hibernation idle-gauge JSON per controller so
	// putPresence (a blind write also called from OnConnect/OnSnapshot) can carry
	// it forward without clobbering between heartbeats. Set from OnHeartbeat.
	idleGauges   map[string]string
	idleGaugesAt map[string]time.Time
	// installedPluginsHash caches the installed_plugins_hash from the most recent
	// heartbeat so putPresence (also called from OnConnect/OnSnapshot) carries it.
	installedPluginsHash map[string]string
	// pendingAcks holds un-acked imperative JetStream messages, keyed by
	// controller ("ns/name") then command_id. Scoping by controller ensures a
	// disconnect only naks that controller's in-flight commands, never another's.
	pendingAcks map[string]map[string]*nats.Msg

	// replayCmds records which in-flight imperative command_ids are
	// REPLAY_WEBHOOK, keyed by controller ("ns/name") then command_id. Their
	// results are routed to the dedicated whreply.* subject instead of the shared
	// mite inbound subject, so they never enter the reconciler's result buffer.
	replayCmds map[string]map[string]bool

	// pendingContent holds NATS reply messages for in-flight ContentRequests,
	// keyed by request_id. Entries are removed on reply, on disconnect of the
	// target mite, or by the TTL sweeper.
	pendingContent     map[string]*nats.Msg
	pendingContentNS   map[string]string    // request_id -> "ns/name" for disconnect cleanup
	pendingContentTime map[string]time.Time // request_id -> createdAt for TTL sweep
	contentTTL         time.Duration

	contentDone chan struct{} // closed in shutdown to stop the TTL sweeper

	// presenceLocks serializes a presence write against the OnDisconnect delete,
	// per controller ("ns/name"). putPresence must not hold h.mu across the KV
	// round trip (it is on the heartbeat path for every controller on this
	// gateway), but without any lock the connected-check and the Put straddle a
	// window in which OnDisconnect can delete the key — resurrecting a departed
	// mite as "connected". Keyed rather than handler-wide so one slow JetStream
	// write cannot stall every other controller's heartbeat.
	presenceLocks map[string]*sync.Mutex

	// watchDegraded records which of a controller's bus→stream watches are
	// currently broken: "ns/name" -> watch kind -> reason. Tracked per kind
	// because the three watchers are independent — a healthy content
	// subscription must not clear a broken desired-state watch. Carried into
	// the presence record so the operator can surface it as a Controller
	// condition; a retry that keeps failing must not stay invisible.
	watchDegraded map[string]map[string]string
	// miteVersion caches the version reported at connect so a degraded-state
	// change can re-write presence immediately instead of waiting a heartbeat.
	miteVersion map[string]string
	// lastHeartbeat records when the mite itself was last heard from. presence
	// writes triggered by gateway-side events (a watch degrading) must carry
	// this rather than time.Now(), or a mite whose stream half-dies keeps
	// getting its liveness refreshed and staleMiteThreshold never fires.
	lastHeartbeat map[string]time.Time

	// watchBackoff is the initial retry delay for re-establishing a bus watch.
	// Zero means defaultWatchBackoff; tests shrink it.
	watchBackoff time.Duration

	Logger *slog.Logger
}

const (
	// defaultWatchBackoff is the first retry delay after a failed watch setup.
	defaultWatchBackoff = 1 * time.Second
	// maxWatchBackoff caps the exponential retry delay. Retries never stop
	// while the mite is connected.
	maxWatchBackoff = 30 * time.Second
	// healthyWatchUptime is how long an established watch must survive before a
	// later death is treated as a fresh incident rather than a flapping one.
	healthyWatchUptime = 1 * time.Minute
)

// Ensure BusHandler implements StreamHandler.
var _ StreamHandler = (*BusHandler)(nil)

func (h *BusHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// NewBusHandler creates a handler that bridges to the NATS bus.
// snapshotKV: mite_snapshots. presenceKV: mite_presence. desiredKV: mite_desired.
// activityPub: publishes enriched activity events (may be nil; if nil, activity
// events are not published).
func NewBusHandler(cluster string, conn *bus.Conn, snapshotKV, presenceKV, desiredKV *bus.KV, activityPub *activity.Publisher) *BusHandler {
	h := &BusHandler{
		cluster:              cluster,
		conn:                 conn,
		kv:                   snapshotKV,
		presenceKV:           presenceKV,
		activityPub:          activityPub,
		cancels:              make(map[string]context.CancelFunc),
		certExpiry:           make(map[string]time.Time),
		idleGauges:           make(map[string]string),
		idleGaugesAt:         make(map[string]time.Time),
		connectEpoch:         make(map[string]int64),
		pendingAcks:          make(map[string]map[string]*nats.Msg),
		replayCmds:           make(map[string]map[string]bool),
		installedPluginsHash: make(map[string]string),
		pendingContent:       make(map[string]*nats.Msg),
		pendingContentNS:     make(map[string]string),
		pendingContentTime:   make(map[string]time.Time),
		contentTTL:           15 * time.Second,
		contentDone:          make(chan struct{}),
		presenceLocks:        make(map[string]*sync.Mutex),
		lastHeartbeat:        make(map[string]time.Time),
		watchDegraded:        make(map[string]map[string]string),
		miteVersion:          make(map[string]string),
	}
	// Guard the interface assignment: storing a typed nil *bus.KV would make
	// h.desiredKV non-nil and send watchDesiredState into a nil-pointer call.
	if desiredKV != nil {
		h.desiredKV = desiredKV
	}
	// Start the content TTL sweeper goroutine.
	go h.contentSweeper()
	return h
}

// Close shuts down the content sweeper goroutine. Must be called before the
// BusHandler is discarded to avoid goroutine leaks.
func (h *BusHandler) Close() {
	close(h.contentDone)
}

// putPresence writes a fresh presence record with a blind Put (no read-modify-
// write). CertExpiry comes from the in-memory cache populated at OnConnect.
// If OnDisconnect has already run (cancels entry gone), the write is skipped to
// avoid clobbering CertExpiry with a zero value from a stale in-flight call.
func (h *BusHandler) putPresence(ns, name, version string) {
	if h.presenceKV == nil {
		return
	}
	key := ns + "/" + name
	// Held across the connected-check and the Put so OnDisconnect cannot slip
	// its delete between them. No entry means the controller has no live
	// connection, so there is nothing to write.
	pl, ok := h.presenceLock(key)
	if !ok {
		return
	}
	pl.Lock()
	defer pl.Unlock()

	h.mu.Lock()
	_, connected := h.cancels[key]
	ce := h.certExpiry[key]
	h.mu.Unlock()
	if !connected {
		return
	}
	h.mu.Lock()
	ep := h.connectEpoch[key]
	gaugesJSON := h.idleGauges[key]
	gaugesAt := h.idleGaugesAt[key]
	installedHash := h.installedPluginsHash[key]
	degradedReason := degradedReasonLocked(h.watchDegraded[key])
	seen, ok := h.lastHeartbeat[key]
	h.mu.Unlock()
	if !ok {
		seen = time.Now()
	}
	data, err := json.Marshal(bus.Presence{
		Version:              version,
		LastHeartbeat:        seen,
		CertExpiry:           ce,
		Epoch:                ep,
		IdleGaugesJSON:       gaugesJSON,
		IdleGaugesReceivedAt: gaugesAt,
		InstalledPluginsHash: installedHash,
		StreamDegraded:       degradedReason != "",
		StreamDegradedReason: degradedReason,
	})
	if err != nil {
		h.log().Error("marshal presence failed", "controller", key, "error", err)
		return
	}
	if err := h.presenceKV.Put(bus.PresenceKey(h.cluster, ns, name), data); err != nil {
		h.log().Error("write presence failed", "controller", key, "error", err)
	}
}

// OnConnect caches cert expiry, writes initial presence, and starts the desired-
// state watch goroutine that forwards commands to the mite stream.
func (h *BusHandler) OnConnect(name, ns, version string, certExpiry interface{}, send Sender, _ interface{}) int64 {
	key := ns + "/" + name
	h.log().Info("mite connected", "controller", key, "version", version)

	miteStreamConnections.Add(context.Background(), 1)

	ce, _ := certExpiry.(time.Time)
	ctx, cancel := context.WithCancel(context.Background())

	h.mu.Lock()
	// Wall-clock alone can repeat within a tick, and a duplicate token would
	// make a superseded stream look current. Keep it timestamp-shaped (the
	// operator compares epochs to detect reconnects) but strictly increasing.
	epoch := time.Now().UnixNano()
	if prev, ok := h.connectEpoch[key]; ok && epoch <= prev {
		epoch = prev + 1
	}
	// The mite reconnected without its previous stream tearing down yet. That
	// stream's OnDisconnect will be ignored as superseded, so cancel its watch
	// goroutines here or they run for the life of the gateway pod, forwarding
	// to a dead stream.
	if prevCancel, ok := h.cancels[key]; ok {
		prevCancel()
	}
	// The new stream establishes its own watches, so drop the superseded
	// connection's degraded marks. Left in place they leak into this
	// connection's first presence write — and if the matching watcher never
	// runs setup, the stale mark survives for its whole life.
	delete(h.watchDegraded, key)
	if h.presenceLocks == nil {
		h.presenceLocks = make(map[string]*sync.Mutex)
	}
	// Reuse the existing mutex across a reconnect. Installing a fresh one would
	// leave an in-flight write from the superseded connection holding the old
	// mutex while the replacement writes under the new one — the stale write
	// then lands last and overwrites the live record.
	if _, ok := h.presenceLocks[key]; !ok {
		h.presenceLocks[key] = &sync.Mutex{}
	}
	h.cancels[key] = cancel
	h.certExpiry[key] = ce
	h.connectEpoch[key] = epoch
	h.miteVersion[key] = version
	h.lastHeartbeat[key] = time.Now()
	h.mu.Unlock()

	go h.watchDesiredState(ctx, ns, name, send)
	go h.watchImperative(ctx, ns, name, send)
	go h.watchContent(ctx, ns, name, send)

	h.publishBroodEvent("connected", ns, name, version)
	h.putPresence(ns, name, version)
	return epoch
}

// OnHeartbeat publishes the heartbeat to the bus and refreshes presence.
func (h *BusHandler) OnHeartbeat(name, ns string, hb *mitev1.Heartbeat) {
	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_Heartbeat{Heartbeat: hb},
	})
	if err != nil {
		h.log().Error("marshal heartbeat failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.conn.Publish(bus.MiteInSubject(h.cluster, ns, name), data)
	h.touchLiveness(ns, name)
	h.publishBroodEvent("heartbeat", ns, name, hb.Version)
	// Cache the hibernation idle gauges so putPresence carries them into the
	// presence record the operator reads for its hibernate-gate decision.
	if hb.Idle != nil {
		if gj, gerr := json.Marshal(hb.Idle); gerr == nil {
			key := ns + "/" + name
			h.mu.Lock()
			h.idleGauges[key] = string(gj)
			h.idleGaugesAt[key] = time.Now()
			h.mu.Unlock()
		}
	}
	// Cache the installed plugins hash so putPresence carries it.
	if hb.InstalledPluginsHash != "" {
		key := ns + "/" + name
		h.mu.Lock()
		h.installedPluginsHash[key] = hb.InstalledPluginsHash
		h.mu.Unlock()
	}
	h.putPresence(ns, name, hb.Version)
}

// OnSnapshot publishes the snapshot to the bus, writes it to KV, refreshes presence.
func (h *BusHandler) OnSnapshot(name, ns string, snap *mitev1.StateSnapshot) {
	h.touchLiveness(ns, name)
	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_StateSnapshot{StateSnapshot: snap},
	})
	if err != nil {
		h.log().Error("marshal snapshot failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.conn.Publish(bus.MiteInSubject(h.cluster, ns, name), data)

	snapData, err := json.Marshal(snap)
	if err != nil {
		h.log().Error("marshal snapshot KV failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.kv.Put(bus.SnapshotKey(h.cluster, ns, name), snapData)

	h.publishBroodEvent("snapshot", ns, name, snap.JenkinsVersion)
	h.putPresence(ns, name, snap.JenkinsVersion)
}

// OnCommandResult publishes the result to the mite inbound subject — or, for a
// REPLAY_WEBHOOK command, to the dedicated whreply.* subject so it bypasses the
// reconciler's shared command-result buffer. If the result matches a pending
// imperative command, the JetStream msg is acked so it is not redelivered after
// a gateway restart.
func (h *BusHandler) OnCommandResult(name, ns string, result *mitev1.CommandResult) {
	key := ns + "/" + name

	// Under lock: ack the pending JetStream imperative message and determine
	// whether this was a webhook-replay command (routing decision).
	var pendingMsg *nats.Msg
	var isReplay bool
	h.mu.Lock()
	if byCmd := h.pendingAcks[key]; byCmd != nil {
		if msg, ok := byCmd[result.CommandId]; ok {
			pendingMsg = msg
			delete(byCmd, result.CommandId)
			if len(byCmd) == 0 {
				delete(h.pendingAcks, key)
			}
		}
	}
	if byCmd := h.replayCmds[key]; byCmd != nil {
		if byCmd[result.CommandId] {
			isReplay = true
			delete(byCmd, result.CommandId)
			if len(byCmd) == 0 {
				delete(h.replayCmds, key)
			}
		}
	}
	h.mu.Unlock()

	if pendingMsg != nil {
		_ = pendingMsg.Ack()
	}

	if isReplay {
		data, err := json.Marshal(result)
		if err != nil {
			h.log().Error("marshal replay result failed", "controller", key, "error", err)
			return
		}
		_ = h.conn.Publish(bus.WebhookResultSubject(h.cluster, ns, name), data)
		return
	}

	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_CommandResult{CommandResult: result},
	})
	if err != nil {
		h.log().Error("marshal command result failed", "controller", key, "error", err)
		return
	}
	_ = h.conn.Publish(bus.MiteInSubject(h.cluster, ns, name), data)
}

// OnTokenRefreshRequest publishes the request to the mite inbound subject
// so the operator can respond with a TokenGrant through the bus.
func (h *BusHandler) OnTokenRefreshRequest(name, ns string, req *mitev1.TokenRefreshRequest) {
	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_TokenRefreshRequest{TokenRefreshRequest: req},
	})
	if err != nil {
		h.log().Error("marshal token refresh request failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.conn.Publish(bus.MiteInSubject(h.cluster, ns, name), data)
}

// OnObservabilityReport publishes the report over the bus and writes it to KV.
func (h *BusHandler) OnObservabilityReport(name, ns string, report *mitev1.ObservabilityReport) {
	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_ObservabilityReport{ObservabilityReport: report},
	})
	if err != nil {
		h.log().Error("marshal observability report failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.conn.Publish(bus.MiteInSubject(h.cluster, ns, name), data)

	reportData, err := json.Marshal(report)
	if err != nil {
		h.log().Error("marshal observability report for KV failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.kv.Put(bus.ObservabilityKey(h.cluster, ns, name), reportData)
}

// OnPluginInventory publishes the plugin inventory over the bus and writes it to KV.
func (h *BusHandler) OnPluginInventory(name, ns string, inv *mitev1.PluginInventory) {
	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_PluginInventory{PluginInventory: inv},
	})
	if err != nil {
		h.log().Error("marshal plugin inventory failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.conn.Publish(bus.MiteInSubject(h.cluster, ns, name), data)

	invData, err := json.Marshal(inv)
	if err != nil {
		h.log().Error("marshal plugin inventory for KV failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.kv.Put(bus.PluginInventoryKey(h.cluster, ns, name), invData)
}

// OnJenkinsActivity builds an enriched bus.ActivityPayload and publishes it
// to the per-controller activity subject via bus.ActivitySubject. The
// controller/namespace come from the mTLS stream identity (handler args),
// never from the event payload (anti-spoof).
func (h *BusHandler) OnJenkinsActivity(name, ns string, evt *mitev1.JenkinsActivityEvent) {
	p := bus.ActivityPayload{
		Event:       evt.Type,
		Name:        name,
		Namespace:   ns,
		Cluster:     h.cluster,
		Source:      "jenkins",
		Type:        evt.Type,
		Actor:       evt.Actor,
		Message:     evt.Message,
		Controller:  name,
		ItemPath:    evt.ItemPath,
		BuildNumber: evt.BuildNumber,
		Result:      evt.Result,
		URL:         evt.URL,
		Timestamp:   evt.Timestamp,
	}
	data, err := json.Marshal(p)
	if err != nil {
		h.log().Error("marshal jenkins activity failed", "error", err)
		return
	}
	subj := bus.ActivitySubject(h.cluster, ns, name)
	_ = h.conn.Publish(subj, data)
}

// IsCurrentConnection reports whether token still names the connection this
// handler holds for the controller. Mirrors the supersede check in
// OnDisconnect: a missing epoch entry means nothing has claimed the key yet, so
// the caller is current by default.
func (h *BusHandler) IsCurrentConnection(name, ns string, token int64) bool {
	if token == 0 {
		return true
	}
	key := ns + "/" + name
	h.mu.Lock()
	defer h.mu.Unlock()
	current, ok := h.connectEpoch[key]
	return !ok || current == token
}

// OnDisconnect stops the watch goroutine and clears volatile state. The desired
// KV key is intentionally NOT deleted here — it is the persistent desired state
// and must survive a mite/gateway reconnect. It is removed only on Controller
// deletion (operator-side ClearDesired).
func (h *BusHandler) OnDisconnect(name, ns string, token int64) {
	key := ns + "/" + name

	// A real stream ended either way, so the connection metrics move even when
	// the teardown below is skipped — otherwise a superseded stream leaves the
	// active-connection gauge permanently over-counted.
	miteStreamConnections.Add(context.Background(), -1)
	miteStreamDisconnects.Add(context.Background(), 1)

	// A superseded stream can tear down late — after the mite already
	// reconnected. Without this check that teardown cancels the *live*
	// connection's watch goroutines, leaving a Connected mite receiving no
	// desired state at all. The check and the teardown share one critical
	// section: releasing h.mu between them would let a reconnect land in the
	// gap and be torn down anyway.
	h.mu.Lock()
	if current, ok := h.connectEpoch[key]; ok && token != 0 && current != token {
		h.mu.Unlock()
		h.log().Info("ignoring disconnect from superseded stream", "controller", key, "token", token, "current", current)
		return
	}

	h.log().Info("mite disconnected", "controller", key)

	if cancel, ok := h.cancels[key]; ok {
		cancel()
		delete(h.cancels, key)
	}
	delete(h.certExpiry, key)
	// Nak only this controller's pending imperative acks so they can be
	// redelivered on reconnect — other controllers' in-flight commands are
	// untouched.
	if byCmd := h.pendingAcks[key]; byCmd != nil {
		for cmdID, msg := range byCmd {
			_ = msg.Nak()
			delete(byCmd, cmdID)
		}
		delete(h.pendingAcks, key)
	}
	// Redelivered imperative commands re-register their replay flags on the next
	// dispatch, so drop this controller's stale replay tracking.
	delete(h.replayCmds, key)
	delete(h.idleGauges, key)
	delete(h.idleGaugesAt, key)
	delete(h.installedPluginsHash, key)
	delete(h.watchDegraded, key)
	delete(h.miteVersion, key)
	delete(h.lastHeartbeat, key)
	// Purge any content requests associated with this mite (no reply coming).
	for reqID, nsKey := range h.pendingContentNS {
		if nsKey == key {
			if msg, ok := h.pendingContent[reqID]; ok {
				_ = msg.Nak()
				delete(h.pendingContent, reqID)
			}
			delete(h.pendingContentNS, reqID)
			delete(h.pendingContentTime, reqID)
		}
	}
	h.mu.Unlock()

	if h.kv != nil {
		_ = h.kv.Delete(bus.SnapshotKey(h.cluster, ns, name))
	}
	// Ordered against in-flight putPresence calls (e.g. a retrying watch
	// goroutine that has not yet observed the cancel): whichever takes the
	// controller's presence lock first completes, and any later putPresence
	// sees the cancels entry gone and skips.
	if pl, held := h.presenceLock(key); held {
		pl.Lock()
		// Go mutexes barge: if the mite reconnected while this delete was
		// queued behind a slow write, the new connection's putPresence can win
		// the lock ahead of us and we would delete a live record. A fresh
		// cancels entry means a newer connection owns this key — leave it be.
		h.mu.Lock()
		_, reconnected := h.cancels[key]
		h.mu.Unlock()
		if !reconnected && h.presenceKV != nil {
			_ = h.presenceKV.Delete(bus.PresenceKey(h.cluster, ns, name))
		}
		pl.Unlock()
		// Retire the entry once the delete has completed — unconditionally on
		// this path, not only when a presence bucket exists, or the map grows
		// with every controller the gateway has ever served. Skip it if the
		// mite reconnected meanwhile: OnConnect already installed a fresh mutex
		// for that connection and removing it here would strand the new one.
		h.mu.Lock()
		if _, live := h.cancels[key]; !live {
			delete(h.presenceLocks, key)
		}
		h.mu.Unlock()
	}
	h.publishBroodEvent("disconnected", ns, name, "")
}

// watchDesiredState watches the mite's desired-state KV key and forwards each
// value (current value on attach + every update) to the gRPC stream. A failed
// send is logged but not retried here: the reconciler re-Puts on hash divergence
// each tick, so a transient failure self-heals on the next reconcile.
func (h *BusHandler) watchDesiredState(ctx context.Context, ns, name string, send Sender) {
	if h.desiredKV == nil {
		return
	}
	key := bus.DesiredKey(h.cluster, ns, name)
	pacer := h.newRebuildPacer()
	for {
		w, ok := h.establishDesiredWatch(ctx, ns, name, key)
		if !ok {
			return // context cancelled: the mite disconnected
		}
		start := time.Now()
		reconnect := h.pumpDesired(ctx, w, ns, name, send)
		_ = w.Stop()
		if !reconnect {
			return // context cancelled
		}
		// The watcher died while the mite is still connected. Without
		// re-establishing here the connection is starved of desired state for
		// the rest of the pod's life.
		h.markWatchDegraded(ctx, ns, name, "desired", "desired-state watch closed unexpectedly")
		if !pacer.wait(ctx, time.Since(start)) {
			return
		}
	}
}

// establishDesiredWatch retries the KV watch setup with capped exponential
// backoff until it succeeds or ctx is cancelled. It never gives up while the
// mite is connected, so a transient failure cannot starve the connection of
// every desired-state push. Returns ok=false only when ctx is done.
func (h *BusHandler) establishDesiredWatch(ctx context.Context, ns, name, key string) (nats.KeyWatcher, bool) {
	var w nats.KeyWatcher
	ok := h.retryEstablish(ctx, ns, name, "desired", func() error {
		var err error
		w, err = h.desiredKV.Watch(key)
		return err
	})
	return w, ok
}

// retryEstablish runs setup until it succeeds or ctx is cancelled, backing off
// exponentially between attempts and marking the controller degraded for as
// long as it keeps failing. It is the shared guard for every bus→stream
// bridge: returning after a single transient setup error would silently
// starve a connected mite of desired state for its whole lifetime.
// Returns false only when ctx is done.
func (h *BusHandler) retryEstablish(ctx context.Context, ns, name, watchKind string, setup func() error) bool {
	delay := h.watchBackoff
	if delay <= 0 {
		delay = defaultWatchBackoff
	}
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		err := setup()
		if err == nil {
			if attempt > 1 {
				h.log().Info("bus watch re-established", "controller", ns+"/"+name, "watch", watchKind, "attempt", attempt)
			}
			h.clearWatchDegraded(ctx, ns, name, watchKind)
			return true
		}
		h.log().Error("bus watch setup failed",
			"controller", ns+"/"+name, "watch", watchKind, "attempt", attempt, "retryIn", delay, "error", err)
		miteWatchFailures.Add(ctx, 1, metric.WithAttributes(
			attribute.String("controller", ns+"/"+name),
			attribute.String("watch", watchKind),
		))
		h.markWatchDegraded(ctx, ns, name, watchKind, watchKind+" watch setup failing: "+err.Error())

		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		if delay *= 2; delay > maxWatchBackoff {
			delay = maxWatchBackoff
		}
	}
}

// pumpDesired forwards watcher updates to the mite stream. It returns true when
// the watcher died and should be re-established, false when ctx was cancelled.
func (h *BusHandler) pumpDesired(ctx context.Context, w nats.KeyWatcher, ns, name string, send Sender) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case entry, ok := <-w.Updates():
			// Channel closed: watcher stopped or server disconnected.
			if !ok {
				return true
			}
			// nil marks end of the initial replay; skip.
			if entry == nil {
				continue
			}
			// Only forward Puts; a Delete (Controller removed) clears state.
			if entry.Operation() != nats.KeyValuePut {
				continue
			}
			var op mitev1.OperatorMessage
			if err := json.Unmarshal(entry.Value(), &op); err != nil {
				h.log().Error("unmarshal desired failed", "controller", ns+"/"+name, "error", err)
				continue
			}
			if err := send(&op); err != nil {
				h.log().Error("send desired to mite failed", "controller", ns+"/"+name, "error", err)
			}
		}
	}
}

// touchLiveness records that the mite itself was just heard from. Only
// mite-originated events (connect, heartbeat, snapshot) may call this —
// gateway-side presence writes must not refresh liveness.
func (h *BusHandler) touchLiveness(ns, name string) {
	h.mu.Lock()
	if h.lastHeartbeat == nil {
		h.lastHeartbeat = make(map[string]time.Time)
	}
	h.lastHeartbeat[ns+"/"+name] = time.Now()
	h.mu.Unlock()
}

// presenceLock returns the per-controller mutex that orders presence writes
// against the disconnect delete. The entry's lifetime is the connection's:
// OnConnect installs it, OnDisconnect retires it. Lookup never creates, so a
// goroutine left over from a superseded connection cannot resurrect an entry
// that nothing would retire — it simply finds none and skips its write, which
// is correct because that controller has no live connection to describe.
func (h *BusHandler) presenceLock(key string) (*sync.Mutex, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl, ok := h.presenceLocks[key]
	return pl, ok
}

// rebuildPacer paces watcher rebuilds. retryEstablish backs off only while
// setup is *failing*; a watch that establishes fine and then dies immediately
// would otherwise spin the loop at full speed with no delay at all.
type rebuildPacer struct {
	delay time.Duration
	base  time.Duration
}

func (h *BusHandler) newRebuildPacer() *rebuildPacer {
	base := h.watchBackoff
	if base <= 0 {
		base = defaultWatchBackoff
	}
	return &rebuildPacer{delay: base, base: base}
}

// wait pauses before the next rebuild attempt. uptime is how long the watch
// that just died stayed up: a watch that ran healthily resets the backoff, a
// watch that died instantly grows it. Returns false if ctx was cancelled.
func (p *rebuildPacer) wait(ctx context.Context, uptime time.Duration) bool {
	if uptime >= healthyWatchUptime {
		p.delay = p.base
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(p.delay):
	}
	if p.delay *= 2; p.delay > maxWatchBackoff {
		p.delay = maxWatchBackoff
	}
	return true
}

// degradedReasonLocked joins the reasons for every broken watch of one
// controller into a single message, or "" when all watches are healthy. Kinds
// are visited in a fixed order so the presence record does not churn on Go's
// randomized map iteration. Caller must hold h.mu.
func degradedReasonLocked(kinds map[string]string) string {
	var reasons []string
	for _, kind := range []string{"desired", "imperative", "content"} {
		if r, ok := kinds[kind]; ok {
			reasons = append(reasons, r)
		}
	}
	return strings.Join(reasons, "; ")
}

// markWatchDegraded records that one of a controller's bus→stream watches is
// broken and refreshes its presence record so the operator sees it within a
// tick rather than at the next heartbeat. No-op if the reason is unchanged.
func (h *BusHandler) markWatchDegraded(ctx context.Context, ns, name, watchKind, reason string) {
	key := ns + "/" + name
	h.mu.Lock()
	// A watch goroutine from a previous connection can still be in flight after
	// a fast reconnect (a setup call blocks for seconds). Without this guard it
	// would stamp the *new* connection degraded and nothing would clear it until
	// that connection's own watch died — the exact false alarm this signal
	// exists to prevent. OnDisconnect cancels the ctx while holding h.mu, so
	// reading it here is a consistent snapshot.
	if ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	if h.watchDegraded == nil {
		h.watchDegraded = make(map[string]map[string]string)
	}
	if h.watchDegraded[key] == nil {
		h.watchDegraded[key] = make(map[string]string)
	}
	changed := h.watchDegraded[key][watchKind] != reason
	h.watchDegraded[key][watchKind] = reason
	version := h.miteVersion[key]
	h.mu.Unlock()
	if changed {
		h.putPresence(ns, name, version)
	}
}

// clearWatchDegraded clears the degraded mark for one watch kind once it is
// healthy again. Other kinds keep their own marks.
func (h *BusHandler) clearWatchDegraded(ctx context.Context, ns, name, watchKind string) {
	key := ns + "/" + name
	h.mu.Lock()
	if ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	kinds := h.watchDegraded[key]
	_, was := kinds[watchKind]
	delete(kinds, watchKind)
	if len(kinds) == 0 {
		delete(h.watchDegraded, key)
	}
	version := h.miteVersion[key]
	h.mu.Unlock()
	if was {
		h.putPresence(ns, name, version)
	}
}

// watchImperative watches the JetStream mite.<ns>.<name>.out subject via a
// durable pull consumer. Imperative commands are edge-triggered one-shots
// (e.g. "safe restart") — unlike desired state, they must NOT be replayed
// on reconnect, so each message is acked only after the mite returns a
// matching CommandResult.
func (h *BusHandler) watchImperative(ctx context.Context, ns, name string, send Sender) {
	if h.conn == nil {
		return
	}
	streamName := "varroa"
	consumer := "imp-" + h.cluster + "-" + ns + "-" + name

	// Setup is retried with backoff rather than abandoned on first error: a
	// permanent return here silently drops every imperative command (safe
	// restart, brood ops) for the life of the connection.
	pacer := h.newRebuildPacer()
	for {
		var sub *nats.Subscription
		established := h.retryEstablish(ctx, ns, name, "imperative", func() error {
			// Ensure the JetStream stream exists (idempotent). Replicate to the
			// NATS cluster size (Conn.Replicas()) so the imperative command stream
			// survives a single NATS pod loss.
			if err := h.conn.EnsureStream(bus.StreamConfig(streamName, h.conn.Replicas())); err != nil {
				return fmt.Errorf("ensure imperative stream: %w", err)
			}
			// Ensure a durable consumer per mite (idempotent).
			if err := h.conn.EnsureConsumer(streamName, consumer, &nats.ConsumerConfig{
				Durable:       consumer,
				FilterSubject: bus.MiteOutSubject(h.cluster, ns, name),
				AckPolicy:     nats.AckExplicitPolicy,
			}); err != nil {
				return fmt.Errorf("ensure imperative consumer: %w", err)
			}
			s, err := h.conn.PullSubscribe(bus.MiteOutSubject(h.cluster, ns, name), streamName, consumer)
			if err != nil {
				return fmt.Errorf("imperative pull subscribe: %w", err)
			}
			sub = s
			return nil
		})
		if !established {
			return // context cancelled: the mite disconnected
		}
		start := time.Now()
		if reconnect := h.pumpImperative(ctx, sub, ns, name, send); !reconnect {
			_ = sub.Unsubscribe()
			return
		}
		// The consumer went away underneath us (deleted, stream lost). Drop the
		// subscription and rebuild it rather than spinning on a dead one.
		_ = sub.Unsubscribe()
		h.markWatchDegraded(ctx, ns, name, "imperative", "imperative consumer lost; rebuilding")
		if !pacer.wait(ctx, time.Since(start)) {
			return
		}
	}
}

// pumpImperative drains and forwards imperative commands. It returns true when
// the subscription died and should be rebuilt, false when ctx was cancelled.
func (h *BusHandler) pumpImperative(ctx context.Context, sub *nats.Subscription, ns, name string, send Sender) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		default:
			msgs, err := h.conn.DrainConsumer(sub, 1)
			if err != nil {
				// A fetch timeout is the normal idle case. Anything else means
				// the consumer or connection is gone: spinning here forever
				// would silently drop every imperative command with no signal.
				if errors.Is(err, nats.ErrTimeout) {
					continue
				}
				h.log().Error("imperative fetch failed", "controller", ns+"/"+name, "error", err)
				return true
			}
			for _, m := range msgs {
				var op mitev1.OperatorMessage
				if err := json.Unmarshal(m.Data, &op); err != nil {
					h.log().Error("imperative unmarshal failed", "controller", ns+"/"+name, "error", err)
					_ = m.Nak()
					continue
				}
				imp := op.GetImperative()
				if imp == nil {
					// Non-imperative one-shots also flow on the out subject
					// (e.g. a TokenGrant answering a mite's refresh request).
					// They carry no CommandResult correlation, so forward to
					// the mite and ack immediately — delivery is idempotent
					// (the mite caches the newest token). Nak on send failure
					// so a disconnected mite gets the grant on reconnect.
					if op.GetTokenGrant() != nil {
						if err := send(&op); err != nil {
							h.log().Error("send token grant to mite failed", "controller", ns+"/"+name, "error", err)
							_ = m.Nak()
							continue
						}
					}
					_ = m.Ack() // not an imperative; ack and skip
					continue
				}
				// Track pending ack (scoped to this controller) — OnCommandResult
				// will ack it once the mite reports completion.
				key := ns + "/" + name
				h.mu.Lock()
				if h.pendingAcks[key] == nil {
					h.pendingAcks[key] = make(map[string]*nats.Msg)
				}
				h.pendingAcks[key][imp.CommandId] = m
				if imp.Type == mitev1.CommandTypeReplayWebhook {
					if h.replayCmds[key] == nil {
						h.replayCmds[key] = make(map[string]bool)
					}
					h.replayCmds[key][imp.CommandId] = true
				}
				h.mu.Unlock()

				if err := send(&op); err != nil {
					h.log().Error("send imperative to mite failed", "controller", ns+"/"+name, "cmdId", imp.CommandId, "error", err)
					h.mu.Lock()
					if byCmd := h.pendingAcks[key]; byCmd != nil {
						delete(byCmd, imp.CommandId)
						if len(byCmd) == 0 {
							delete(h.pendingAcks, key)
						}
					}
					h.mu.Unlock()
					_ = m.Nak()
				}
			}
		}
	}
}

// publishBroodEvent publishes an event to the brood subject and, via the
// activity Publisher, to the hierarchical activity subject family.
// activityMessageForBroodEvent decides which brood events become activity
// events, and with what message. Only lifecycle TRANSITIONS qualify:
// heartbeats and snapshots are periodic presence telemetry, already carried to
// live consumers by the raw brood event on events.brood.>, and the activity
// stream is a bounded audit log — persisting one event per mite per interval
// evicts every real audit event within days (observed live: ~20k events per
// controller in under two days against a 100k-message cap).
func activityMessageForBroodEvent(event, version string) (string, bool) {
	switch event {
	case "connected":
		return "mite " + version + " connected", true
	case "disconnected":
		return "mite disconnected", true
	default:
		return "", false
	}
}

func (h *BusHandler) publishBroodEvent(event, ns, name, version string) {
	data, err := json.Marshal(map[string]string{
		"event":     event,
		"name":      name,
		"namespace": ns,
		"cluster":   h.cluster,
	})
	if err != nil {
		h.log().Error("marshal brood event failed", "error", err)
		return
	}
	if h.conn != nil {
		_ = h.conn.Publish(bus.BroodSubject(h.cluster), data)
	}

	if h.activityPub != nil {
		msg, ok := activityMessageForBroodEvent(event, version)
		if !ok {
			return
		}
		h.activityPub.Publish(activity.Event{
			Type:       event,
			Source:     "mite",
			Controller: name,
			Namespace:  ns,
			Message:    msg,
		})
	}
}

// watchContent subscribes to the mite's content-request subject and bridges
// ContentRequests from the bus to the gRPC stream. Inbound requests come on
// MiteContentSubject via core NATS; the reply is deferred — a ContentRequest
// is sent over the stream and the incoming msg is stashed in pendingContent.
func (h *BusHandler) watchContent(ctx context.Context, ns, name string, send Sender) {
	if h.conn == nil || h.conn.NATSConn() == nil {
		return
	}
	// Retried for the same reason as the desired-state watch: giving up
	// after one setup error leaves the mite unable to serve content fetches for
	// the rest of the connection, with no operator-visible signal.
	pacer := h.newRebuildPacer()
	for {
		var sub *nats.Subscription
		established := h.retryEstablish(ctx, ns, name, "content", func() error {
			nc := h.conn.NATSConn()
			if nc == nil {
				return errors.New("nats connection unavailable")
			}
			s, err := nc.SubscribeSync(bus.MiteContentSubject(h.cluster, ns, name))
			if err != nil {
				return fmt.Errorf("content subscribe: %w", err)
			}
			sub = s
			return nil
		})
		if !established {
			return // context cancelled: the mite disconnected
		}
		start := time.Now()
		if reconnect := h.pumpContent(ctx, sub, ns, name, send); !reconnect {
			_ = sub.Unsubscribe()
			return
		}
		// The subscription died while the mite is still connected. Returning
		// here would leave it unable to serve any content fetch for the rest
		// of the connection, with no signal.
		_ = sub.Unsubscribe()
		h.markWatchDegraded(ctx, ns, name, "content", "content subscription lost; rebuilding")
		if !pacer.wait(ctx, time.Since(start)) {
			return
		}
	}
}

// pumpContent bridges ContentRequests from the bus to the gRPC stream. It
// returns true when the subscription died and should be rebuilt, false when
// ctx was cancelled.
func (h *BusHandler) pumpContent(ctx context.Context, sub *nats.Subscription, ns, name string, send Sender) bool {
	key := ns + "/" + name
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			if errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
				h.log().Error("content subscription lost", "controller", key, "error", err)
				return true
			}
			continue
		}
		var req mitev1.ContentRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			h.log().Error("content request unmarshal failed", "controller", key, "error", err)
			continue
		}
		if req.RequestId == "" {
			continue
		}
		h.mu.Lock()
		h.pendingContent[req.RequestId] = msg
		h.pendingContentNS[req.RequestId] = key
		h.pendingContentTime[req.RequestId] = time.Now()
		h.mu.Unlock()
		if err := send(&mitev1.OperatorMessage{
			Message: &mitev1.OperatorMessage_ContentRequest{ContentRequest: &req},
		}); err != nil {
			h.log().Error("send content request to mite failed", "controller", key, "error", err)
			h.mu.Lock()
			delete(h.pendingContent, req.RequestId)
			delete(h.pendingContentNS, req.RequestId)
			delete(h.pendingContentTime, req.RequestId)
			h.mu.Unlock()
			_ = msg.Nak()
		}
	}
}

// OnContentResponse resolves a pending content-fetch request. Only active in
// the gateway process (BusHandler); other StreamHandler implementations stub it.
func (h *BusHandler) OnContentResponse(requestID string, resp *mitev1.ContentResponse) {
	h.mu.Lock()
	msg, ok := h.pendingContent[requestID]
	if ok {
		delete(h.pendingContent, requestID)
		delete(h.pendingContentNS, requestID)
		delete(h.pendingContentTime, requestID)
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	data, err := json.Marshal(resp)
	if err != nil {
		h.log().Error("marshal content response failed", "error", err)
		_ = msg.Nak()
		return
	}
	if err := msg.Respond(data); err != nil {
		h.log().Error("respond content response failed", "requestID", requestID, "error", err)
	}
}

// contentSweeper periodically removes stale pendingContent entries that have
// not been resolved within contentTTL. Runs as a single goroutine per BusHandler.
func (h *BusHandler) contentSweeper() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.contentDone:
			return
		case <-ticker.C:
		}
		deadline := time.Now().Add(-h.contentTTL)
		var stale []string
		h.mu.Lock()
		for reqID, ts := range h.pendingContentTime {
			if ts.Before(deadline) {
				stale = append(stale, reqID)
			}
		}
		for _, reqID := range stale {
			if msg, ok := h.pendingContent[reqID]; ok {
				_ = msg.Nak()
			}
			delete(h.pendingContent, reqID)
			delete(h.pendingContentNS, reqID)
			delete(h.pendingContentTime, reqID)
		}
		h.mu.Unlock()
	}
}

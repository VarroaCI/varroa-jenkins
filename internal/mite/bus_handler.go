package mite

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

var (
	miteStreamConnections metric.Int64Gauge
	miteStreamDisconnects metric.Int64Counter
)

func init() {
	meter := otel.Meter("varroa-gateway")
	miteStreamConnections, _ = meter.Int64Gauge("varroa.mite.stream.connections",
		metric.WithDescription("Active mite stream connections"),
	)
	miteStreamDisconnects, _ = meter.Int64Counter("varroa.mite.stream.disconnects",
		metric.WithDescription("Mite stream disconnect count"),
	)
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
	desiredKV  *bus.KV // desired-state KV (watched and forwarded to the mite)

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

	Logger *slog.Logger
}

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
		desiredKV:            desiredKV,
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
	h.mu.Unlock()
	data, err := json.Marshal(bus.Presence{
		Version:              version,
		LastHeartbeat:        time.Now(),
		CertExpiry:           ce,
		Epoch:                ep,
		IdleGaugesJSON:       gaugesJSON,
		IdleGaugesReceivedAt: gaugesAt,
		InstalledPluginsHash: installedHash,
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
func (h *BusHandler) OnConnect(name, ns, version string, certExpiry interface{}, send Sender, _ interface{}) {
	key := ns + "/" + name
	h.log().Info("mite connected", "controller", key, "version", version)

	miteStreamConnections.Record(context.Background(), 1)

	ce, _ := certExpiry.(time.Time)
	ctx, cancel := context.WithCancel(context.Background())

	h.mu.Lock()
	h.cancels[key] = cancel
	h.certExpiry[key] = ce
	h.connectEpoch[key] = time.Now().UnixNano()
	h.mu.Unlock()

	go h.watchDesiredState(ctx, ns, name, send)
	go h.watchImperative(ctx, ns, name, send)
	go h.watchContent(ctx, ns, name, send)

	h.publishBroodEvent("connected", ns, name, version)
	h.putPresence(ns, name, version)
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

// OnDisconnect stops the watch goroutine and clears volatile state. The desired
// KV key is intentionally NOT deleted here — it is the persistent desired state
// and must survive a mite/gateway reconnect. It is removed only on Controller
// deletion (operator-side ClearDesired).
func (h *BusHandler) OnDisconnect(name, ns string) {
	key := ns + "/" + name
	h.log().Info("mite disconnected", "controller", key)

	miteStreamConnections.Record(context.Background(), -1)
	miteStreamDisconnects.Add(context.Background(), 1)

	h.mu.Lock()
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
	delete(h.installedPluginsHash, key) // Purge any content requests associated with this mite (no reply coming).
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

	_ = h.kv.Delete(bus.SnapshotKey(h.cluster, ns, name))
	if h.presenceKV != nil {
		_ = h.presenceKV.Delete(bus.PresenceKey(h.cluster, ns, name))
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
	w, err := h.desiredKV.Watch(bus.DesiredKey(h.cluster, ns, name))
	if err != nil {
		h.log().Error("watch desired failed", "controller", ns+"/"+name, "error", err)
		return
	}
	defer func() { _ = w.Stop() }()

	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-w.Updates():
			// Channel closed: watcher stopped or server disconnected.
			if !ok {
				return
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

// watchImperative watches the JetStream mite.<ns>.<name>.out subject via a
// durable pull consumer. Imperative commands are edge-triggered one-shots
// (e.g. "safe restart") — unlike desired state, they must NOT be replayed
// on reconnect, so each message is acked only after the mite returns a
// matching CommandResult.
func (h *BusHandler) watchImperative(ctx context.Context, ns, name string, send Sender) {
	streamName := "varroa"
	consumer := "imp-" + h.cluster + "-" + ns + "-" + name

	// Ensure the JetStream stream exists (idempotent). Replicate to the NATS
	// cluster size (Conn.Replicas()) so the imperative command stream survives
	// a single NATS pod loss.
	if err := h.conn.EnsureStream(bus.StreamConfig(streamName, h.conn.Replicas())); err != nil {
		h.log().Error("ensure imperative stream failed", "controller", ns+"/"+name, "error", err)
		return
	}

	// Ensure a durable consumer per mite (idempotent).
	if err := h.conn.EnsureConsumer(streamName, consumer, &nats.ConsumerConfig{
		Durable:       consumer,
		FilterSubject: bus.MiteOutSubject(h.cluster, ns, name),
		AckPolicy:     nats.AckExplicitPolicy,
	}); err != nil {
		h.log().Error("ensure imperative consumer failed", "controller", ns+"/"+name, "error", err)
		return
	}

	sub, err := h.conn.PullSubscribe(bus.MiteOutSubject(h.cluster, ns, name), streamName, consumer)
	if err != nil {
		h.log().Error("imperative pull subscribe failed", "controller", ns+"/"+name, "error", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			_ = sub.Unsubscribe()
			return
		default:
			msgs, err := h.conn.DrainConsumer(sub, 1)
			if err != nil {
				// Transient fetch timeout/error — retry.
				continue
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
	_ = h.conn.Publish(bus.BroodSubject(h.cluster), data)

	// Publish enriched activity event via the Publisher (mode-agnostic).
	if h.activityPub != nil {
		msg := ""
		switch event {
		case "connected":
			msg = "mite " + version + " connected"
		case "disconnected":
			msg = "mite disconnected"
		case "heartbeat":
			msg = "mite heartbeat"
		case "snapshot":
			msg = "mite snapshot received"
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
	nc := h.conn.NATSConn()
	if nc == nil {
		return
	}
	sub, err := nc.SubscribeSync(bus.MiteContentSubject(h.cluster, ns, name))
	if err != nil {
		h.log().Error("content subscribe failed", "controller", ns+"/"+name, "error", err)
		return
	}
	defer func() { _ = sub.Unsubscribe() }()

	key := ns + "/" + name
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			if errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrBadSubscription) {
				return
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

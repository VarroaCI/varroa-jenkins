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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// buildEventPublisher is the seam over the JetStream publish calls the
// build-event worker uses. *bus.Conn satisfies it; tests substitute a fake
// so a rejected publish, a retry, and a dropped heartbeat are exercisable
// without a live NATS server.
type buildEventPublisher interface {
	PublishJetStream(subject string, data []byte) (*nats.PubAck, error)
	PublishJetStreamAsync(subject string, data []byte) (nats.PubAckFuture, error)
}

// ledgerRequester is the seam over the core-NATS request-reply call the
// claim bridge uses. *bus.Conn satisfies it; tests substitute a fake so the
// granted, timeout, and unparsable-reply paths are exercisable without a
// live NATS server.
type ledgerRequester interface {
	RequestWithContext(ctx context.Context, subject string, data []byte, timeout time.Duration) (*nats.Msg, error)
}

// pendingImperative is one imperative JetStream message the gateway sent to a
// mite and still waits on a CommandResult for. token names the connection that
// received it. The gateway releases a delivery only on the teardown of that
// same connection, so a superseded stream never naks a command the replacement
// connection already runs.
type pendingImperative struct {
	msg   *nats.Msg
	token int64
}

// BusHandler implements StreamHandler by bridging the mite gRPC stream to the
// NATS bus. It replaces the in-memory Registry for the gateway tier.
//
// Desired-state delivery (operator→mite) is a core NATS request on
// bus.MiteDesiredSubject: the gateway that holds the stream forwards each
// request to the mite and answers the operator after the send succeeds.
// NATS stores no copy, so a reconnecting mite receives nothing until the
// operator pushes again on the new connection epoch.
type BusHandler struct {
	cluster    string
	conn       *bus.Conn
	metrics    gatewayMetrics
	kv         *bus.KV // snapshot KV
	presenceKV *bus.KV // presence KV (written on connect/heartbeat, deleted on disconnect)
	// desired is the desired-state request source. Nil when the handler
	// runs without a bus connection; serveDesiredState then returns at once.
	desired desiredSource

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
	// Each entry also carries the connection token that received the message, so
	// a superseded stream's teardown releases its own deliveries and leaves the
	// replacement connection's in-flight commands alone.
	pendingAcks map[string]map[string]pendingImperative

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
	// because the watchers are independent — a healthy content
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

	// cellInstances holds the pod unique identifier each controller's mite
	// presented at registration ("ns/name" -> cell_instance), set by
	// RecordCellInstance and read by OnBuildEvent. It is not cleared on
	// OnDisconnect: the identity belongs to the mite's pod, not to one
	// stream, and survives a reconnect on the same gateway replica.
	cellInstances map[string]string

	// watchBackoff is the initial retry delay for re-establishing a bus watch.
	// Zero means defaultWatchBackoff; tests shrink it.
	watchBackoff time.Duration

	// buildMu guards buildWorkers, kept separate from mu so a build-event
	// enqueue never contends with the heartbeat/presence path.
	buildMu      sync.Mutex
	buildWorkers map[string]*registeredBuildWorker // keyed "ns/name"

	// claimMu guards claimCounts, kept separate from mu for the same reason.
	claimMu     sync.Mutex
	claimCounts map[string]int // keyed "ns/name#epoch"

	// crossTenantMu guards crossTenantCounts, kept separate from mu for
	// the same reason claimMu is.
	crossTenantMu     sync.Mutex
	crossTenantCounts map[string]int // keyed "ns/name#epoch"

	// tenantResolver resolves a claim's or a job-schedule row's ledger
	// tenant and placement generation. Nil means the single-tenant
	// placeholder.
	tenantResolver CellTenantResolver

	// scheduleMu guards scheduleRetained and scheduleStreamMissingUntil,
	// kept separate from mu for the same reason buildMu and claimMu are.
	scheduleMu sync.Mutex
	// scheduleRetained holds, per connection ("ns/name"), the job-schedule
	// set the gateway has not yet published: a set OnSnapshot could not
	// resolve every row of, or a set whose publish attempt failed.
	// handleJobSchedules retries it on every snapshot tick until it
	// publishes, and replaces it when a later snapshot carries a new set.
	scheduleRetained map[string]*mitev1.JobScheduleSet
	// scheduleStreamMissingUntil holds, per connection, when the gateway
	// may next attempt a job.schedules publish after a publish failed. A
	// snapshot tick before this time skips the attempt entirely, so a
	// record-store-disabled install produces no round trip per snapshot.
	scheduleStreamMissingUntil map[string]time.Time
	// scheduleStreamMissingTTL overrides how long a publish failure
	// suppresses the next attempt. Zero means
	// defaultJobScheduleStreamMissingTTL; tests shrink it.
	scheduleStreamMissingTTL time.Duration
	// branchRetained holds, per connection ("ns/name"), the multibranch
	// inventory set that has not yet published as a job.branches
	// envelope. It shares scheduleMu and the job-schedule TTL.
	branchRetained map[string]*mitev1.JobBranchSet
	// branchStreamMissingUntil is the job.branches negative cache, with
	// the same meaning as scheduleStreamMissingUntil.
	branchStreamMissingUntil map[string]time.Time

	// recordStoreEnabled reports whether this install configured a record
	// store, so a builds stream exists to publish onto. cmd/gateway sets it
	// from the opened blob store, never from VARROA_RECORDSTORE_DSN: the
	// chart supplies that DSN only in postgres blob mode, and the blob URL
	// in every mode. The failed-trigger path reads this flag instead of the
	// schedule path's time-based cache: a cache cannot tell a missing
	// stream from a transient publish failure, and a failed trigger must
	// never be acknowledged without its record.
	recordStoreEnabled bool

	// publisher is the build-event JetStream publish seam. Defaults to
	// conn; tests substitute a fake.
	publisher buildEventPublisher
	// ledger is the claim request-reply seam. Defaults to conn; tests
	// substitute a fake.
	ledger ledgerRequester
	// buildPublishRetryDelay is the starting retry delay for a rejected
	// build-event publish. Zero means defaultPublishRetryDelay; tests
	// shrink it.
	buildPublishRetryDelay time.Duration

	// logStore is the blob store the log writer appends console-log
	// chunks and index entries to. Nil (the default) makes OnLogChunk a
	// no-op: the record store is disabled. Wired by SetLogStore.
	logStore blob.Store
	// logPendingMaxBytes is the gateway-wide pending budget
	// (VARROA_GATEWAY_LOG_PENDING_BYTES). <= 0 means the default.
	logPendingMaxBytes int64
	// logFinalizeWait is VARROA_RECORDSTORE_LOG_FINALIZE_WAIT, read on
	// the gateway so the writer heartbeat and the abandon deadline use
	// the same value the recorder's finalize sweep does. <= 0 means the
	// default.
	logFinalizeWait time.Duration
	// logOwnerRecheckAge overrides logOwnerRecheckAfter when above zero.
	// Only a test sets it.
	logOwnerRecheckAge time.Duration
	// ownerCache is the bounded run-ownership cache OnLogChunk consults
	// before it writes one byte.
	ownerCache *ownerCache

	// stateMu guards stateRuns and the gateway-wide run-state pending
	// counter. It is separate from logMu, so a run-state push never
	// contends with the console-log append path.
	stateMu                sync.Mutex
	stateRuns              map[string]*runStateEntry // keyed by run_id
	stateTotalPendingBytes int64
	// stateBudgetCrossedAt is when the gateway-wide run-state pending
	// bound was first crossed. It grows the busy pause on the log
	// path's own ladder.
	stateBudgetCrossedAt time.Time

	// tailMu guards tailLeases and tailErrors, kept separate from mu so a
	// tail-control push never contends with the heartbeat/presence path.
	// A terminal condition deletes a run's tailLeases entry outright
	// rather than leaving it deactivated, so a long-lived gateway holds
	// no lease past the run it belonged to. A lease timer validates its
	// control_id and deletes the entry under one unbroken hold of
	// tailMu (expireTailLeaseIfCurrent), so a renewal can never land in
	// a window between that validation and the delete and lose the
	// lease it just renewed. tailErrors holds a kept error
	// independently of the lease, keyed by run_id, so a cleared lease
	// still answers the next start request with the reason it failed
	// (design.md D5); sweepExpiredTailErrors prunes every expired entry
	// on each start/stop request and lease timer, not only the one
	// run_id a caller happens to ask about, so a run that never asks
	// again does not keep its entry forever.
	tailMu     sync.Mutex
	tailLeases map[string]*runTailLease
	tailErrors map[string]tailErrorEntry
	// tailLeaseTimeout, tailAckTimeout, and tailErrTTL override the
	// live-tail durations (design.md D5: 90s, 10s, 5m). Zero means the
	// package default; tests shrink them so a lease or ack timeout test
	// does not have to wait tens of seconds.
	tailLeaseTimeout time.Duration
	tailAckTimeout   time.Duration
	tailErrTTL       time.Duration

	// imperativeAckWait overrides imperativeAckWaitDefault. Zero means the
	// default; tests shrink it so a redelivery test runs in seconds.
	imperativeAckWait time.Duration

	// logMu guards logRuns and the gateway-wide pending-bytes counter,
	// kept separate from mu so a log append never contends with the
	// heartbeat/presence path.
	logMu                sync.Mutex
	logRuns              map[string]*runLogState // keyed by run_id
	logTotalPendingBytes int64
	// logBudgetCrossedAt is when the gateway-wide pending budget was
	// first found crossed, zeroed once it drops back under. While the
	// drain makes no progress, the BUSY pause grows from this moment.
	logBudgetCrossedAt time.Time
	// logDrain measures the bytes the drain goroutines free per second.
	// The gateway-budget BUSY pause is the time this rate needs to free
	// a refused chunk's bytes.
	logDrain logDrainMeter

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

	// imperativeAckWaitDefault is the ack wait of each per-mite imperative
	// consumer. The gateway marks each pending command in progress four
	// times per ack wait, so the ack wait must exceed that heartbeat
	// interval with margin. The value bounds how long a command that a
	// crashed gateway held waits before the stream delivers it again.
	imperativeAckWaitDefault = 60 * time.Second
)

// Ensure BusHandler implements StreamHandler.
var _ StreamHandler = (*BusHandler)(nil)

func (h *BusHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// NewBusHandler creates a handler that bridges to the NATS bus. It creates
// the gateway instruments on the "varroa-gateway" meter of mp and returns an
// error when an instrument cannot be created.
// snapshotKV: mite_snapshots. presenceKV: mite_presence.
// activityPub: publishes enriched activity events (may be nil; if nil, activity
// events are not published).
func NewBusHandler(mp metric.MeterProvider, cluster string, conn *bus.Conn, snapshotKV, presenceKV *bus.KV, activityPub *activity.Publisher) (*BusHandler, error) {
	metrics, err := newGatewayMetrics(mp)
	if err != nil {
		return nil, err
	}
	h := &BusHandler{
		cluster:                    cluster,
		conn:                       conn,
		metrics:                    metrics,
		kv:                         snapshotKV,
		presenceKV:                 presenceKV,
		activityPub:                activityPub,
		cancels:                    make(map[string]context.CancelFunc),
		certExpiry:                 make(map[string]time.Time),
		idleGauges:                 make(map[string]string),
		idleGaugesAt:               make(map[string]time.Time),
		connectEpoch:               make(map[string]int64),
		pendingAcks:                make(map[string]map[string]pendingImperative),
		replayCmds:                 make(map[string]map[string]bool),
		installedPluginsHash:       make(map[string]string),
		pendingContent:             make(map[string]*nats.Msg),
		pendingContentNS:           make(map[string]string),
		pendingContentTime:         make(map[string]time.Time),
		contentTTL:                 15 * time.Second,
		contentDone:                make(chan struct{}),
		presenceLocks:              make(map[string]*sync.Mutex),
		lastHeartbeat:              make(map[string]time.Time),
		watchDegraded:              make(map[string]map[string]string),
		miteVersion:                make(map[string]string),
		scheduleRetained:           make(map[string]*mitev1.JobScheduleSet),
		scheduleStreamMissingUntil: make(map[string]time.Time),
		cellInstances:              make(map[string]string),
		buildWorkers:               make(map[string]*registeredBuildWorker),
		claimCounts:                make(map[string]int),
		crossTenantCounts:          make(map[string]int),
		tenantResolver:             defaultCellTenantResolver{},
		logRuns:                    make(map[string]*runLogState),
		stateRuns:                  make(map[string]*runStateEntry),
		ownerCache:                 newOwnerCache(0),
		tailLeases:                 make(map[string]*runTailLease),
		tailErrors:                 make(map[string]tailErrorEntry),
	}
	// conn already satisfies both seams structurally; tests override these
	// two fields directly with a fake.
	h.publisher = conn
	h.ledger = conn
	// Guard the interface assignment: storing a source over a nil conn would
	// make h.desired non-nil and send serveDesiredState into a nil-pointer call.
	if conn != nil {
		h.desired = natsDesiredSource{conn: conn}
	}
	// Start the content TTL sweeper goroutine.
	go h.contentSweeper()
	return h, nil
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

// OnConnect caches cert expiry, writes initial presence, and starts the
// desired-state goroutine that forwards commands to the mite stream.
func (h *BusHandler) OnConnect(name, ns, version string, certExpiry interface{}, send Sender, _ interface{}) int64 {
	key := ns + "/" + name
	h.log().Info("mite connected", "controller", key, "version", version)

	h.metrics.miteStreamConnections.Add(context.Background(), 1)

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

	// The desired-state subscription opens before the connection is
	// announced, and presence is written before the connected event. The
	// operator wakes on that event, reads the new epoch from presence, and
	// pushes at once. Either step out of order loses that push.
	desiredInbox := h.subscribeDesired(ns, name)
	go h.serveDesiredState(ctx, ns, name, send, desiredInbox)
	go h.watchImperative(ctx, ns, name, epoch, send)
	go h.watchContent(ctx, ns, name, send)
	go h.watchTail(ctx, ns, name, send)
	h.startBuildEventWorker(ctx, ns, name, epoch, send)

	h.putPresence(ns, name, version)
	h.publishBroodEvent("connected", ns, name, version)
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

// OnSnapshot publishes the snapshot to the bus, writes it to KV, refreshes
// presence, and bridges the job-schedule set, if any, to the record
// store. design.md D8 "The gateway": the job-schedule field is stripped
// from both the mite-inbound fan-out and the snapshot KV before either
// publish, because those two paths carry one value every heartbeat
// interval for every controller and a schedule set would multiply their
// size.
func (h *BusHandler) OnSnapshot(name, ns string, snap *mitev1.StateSnapshot) {
	h.touchLiveness(ns, name)

	schedules := snap.JobSchedules
	branches := snap.JobBranches
	fanout := snap
	if schedules != nil || branches != nil {
		clone := *snap
		clone.JobSchedules = nil
		clone.JobBranches = nil
		fanout = &clone
	}

	data, err := json.Marshal(&mitev1.MiteMessage{
		Message: &mitev1.MiteMessage_StateSnapshot{StateSnapshot: fanout},
	})
	if err != nil {
		h.log().Error("marshal snapshot failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.conn.Publish(bus.MiteInSubject(h.cluster, ns, name), data)

	snapData, err := json.Marshal(fanout)
	if err != nil {
		h.log().Error("marshal snapshot KV failed", "controller", ns+"/"+name, "error", err)
		return
	}
	_ = h.kv.Put(bus.SnapshotKey(h.cluster, ns, name), snapData)

	h.publishBroodEvent("snapshot", ns, name, snap.JenkinsVersion)
	h.putPresence(ns, name, snap.JenkinsVersion)

	h.handleJobSchedules(ns, name, schedules)
	h.handleJobBranches(ns, name, branches)
}

// SetRecordStoreEnabled declares whether this install configured a record
// store. cmd/gateway calls it once at startup. An install with no record store
// publishes no run.trigger_failed envelope at all.
func (h *BusHandler) SetRecordStoreEnabled(enabled bool) {
	h.recordStoreEnabled = enabled
}

// recordCommandOutcome publishes the record of one command result, and
// reports whether the pending imperative message may be acknowledged.
//
// A refused CANCEL_BUILD, input answer, or REPLAY_BUILD logs one Warn line
// first, with or without a record store, and acknowledges: the caller
// already holds a 202. With a record store, a refused cancel or input answer
// then publishes one run.command_refused envelope for its run, and a refused
// replay publishes one run.trigger_failed envelope for the new run the
// replay named. These three acknowledge whatever the publish answers: a
// redelivered command runs the refused call again, and the record is only a
// notice for the run's reader. With no record store the gateway publishes
// nothing and acknowledges.
//
// A TRIGGER_BUILD result publishes, and ANY publish error blocks the
// acknowledgement. The stream then redelivers the command, the mite triggers
// again, and the next result publishes. It consults no time-based cache here:
// a cache cannot tell a missing stream from a transient failure, so an armed
// cache would acknowledge the redelivery and lose the record.
//
// A RESUME_BUILD result acknowledges whatever the publish answers, which is
// the opposite rule. A redelivered trigger builds the same job again and costs
// nothing. A redelivered resume can succeed on its second delivery, while the
// recorder holds no dispatched resume any more. Only the named target's own
// heartbeat clears a resume record, so that live build would read INTERRUPTED
// at the arm window with no cancel behind it. A lost publish instead leaves
// the run on its resume deadline, which is the path it takes today.
//
// Every other command type acknowledges, because it writes no record.
func (h *BusHandler) recordCommandOutcome(ns, name string, pendingMsg *nats.Msg, result *mitev1.CommandResult) bool {
	imp := pendingCommand(pendingMsg)
	if imp == nil {
		return true
	}
	switch imp.Type {
	case mitev1.CommandTypeCancelBuild:
		h.logRefusedRunCommand("cancel build command refused", ns, name, result)
		if p := imp.CancelBuild; p != nil && h.shouldRecordRefusal(result) {
			h.publishCommandRefused(ns, name, refusedCommandCancel, p.RunId, p.JobPath, p.Actor, result)
		}
		return true
	case mitev1.CommandTypeProceedInput:
		h.logRefusedRunCommand("run input command refused", ns, name, result)
		if p := imp.ProceedInput; p != nil && h.shouldRecordRefusal(result) {
			h.publishCommandRefused(ns, name, refusedCommandProceed, p.RunId, p.JobPath, actingUserName(p.ActingUser), result)
		}
		return true
	case mitev1.CommandTypeAbortInput:
		h.logRefusedRunCommand("run input command refused", ns, name, result)
		if p := imp.AbortInput; p != nil && h.shouldRecordRefusal(result) {
			h.publishCommandRefused(ns, name, refusedCommandAbort, p.RunId, p.JobPath, actingUserName(p.ActingUser), result)
		}
		return true
	case mitev1.CommandTypeReplayBuild:
		// A refused replay leaves no build. The failure record of the new
		// run is the only record the run's reader can find.
		h.logRefusedRunCommand("replay build command refused", ns, name, result)
		if p := imp.ReplayBuild; p != nil && h.shouldRecordRefusal(result) {
			h.publishTriggerFailed(ns, name, &mitev1.TriggerBuildPayload{
				RunId:     p.RunId,
				IntentKey: p.IntentKey,
				JobPath:   p.JobPath,
				Actor:     actingUserName(p.ActingUser),
			}, result)
		}
		return true
	}
	if !h.recordStoreEnabled {
		return true
	}
	switch imp.Type {
	case mitev1.CommandTypeTriggerBuild:
		if imp.TriggerBuild == nil {
			return true
		}
		if result.Success {
			return h.publishTriggerAccepted(ns, name, imp.TriggerBuild, result)
		}
		return h.publishTriggerFailed(ns, name, imp.TriggerBuild, result)
	case mitev1.CommandTypeResumeBuild:
		// A successful resume publishes nothing. The resumed build proves
		// itself by its own heartbeat, which the recorder already reads.
		if !result.Success {
			h.publishResumeFailed(ns, name, imp.ResumeBuild, result)
		}
		return true
	default:
		return true
	}
}

// logRefusedRunCommand logs one Warn line for a refused run command that
// writes no record. A success logs nothing.
func (h *BusHandler) logRefusedRunCommand(msg, ns, name string, result *mitev1.CommandResult) {
	if result.Success {
		return
	}
	h.log().Warn(msg,
		"controller", ns+"/"+name,
		"command_id", result.CommandId,
		"run_id", result.RunId,
		"actor", result.Actor,
		"http_status", result.HttpStatus,
		"error", result.Error)
}

// shouldRecordRefusal reports whether a run command result is a refusal the
// record store must hold.
func (h *BusHandler) shouldRecordRefusal(result *mitev1.CommandResult) bool {
	return h.recordStoreEnabled && !result.Success
}

// actingUserName answers the name of an input answer's or a replay's acting
// user, or an empty name when the command names none.
func actingUserName(user *mitev1.ActingUser) string {
	if user == nil {
		return ""
	}
	return user.Name
}

// publishCommandRefused publishes one run.command_refused envelope for a
// cancel or an input answer the plugin refused. It answers nothing: the
// caller acknowledges the command whatever this publish does. A command that
// names no run publishes nothing. An unresolved tenant publishes nothing and
// counts a rejection: a cell must not write a record under a tenant it does
// not host.
func (h *BusHandler) publishCommandRefused(ns, name, command, runID, jobPath, actor string, result *mitev1.CommandResult) {
	key := ns + "/" + name
	if runID == "" {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("refused run command names no run, publishing no envelope",
			"controller", key, "command", command)
		return
	}

	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, jobPath)
	if tenant == "" {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("refused run command unresolved, publishing no envelope",
			"controller", key, "command", command, "job_path", jobPath)
		return
	}

	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindRunCommandRefused,
		RunID:         runID,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		JobPath:       jobPath,
		// A run command payload carries no timestamp, so the gateway clock
		// is the only source.
		OccurredAt: time.Now().UTC(),
		Payload: map[string]interface{}{
			"command":     command,
			"actor":       actor,
			"http_status": result.HttpStatus,
			"error":       truncateError(result.Error),
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal command refusal envelope failed", "controller", key, "error", err)
		return
	}

	if _, err := h.publisher.PublishJetStream(bus.BuildSubject(h.cluster, ns, name), data); err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("command refusal publish failed, the run shows no refusal",
			"controller", key, "run_id", runID, "error", err)
		return
	}

	h.metrics.commandRefusedTotal.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("command", command)))
}

// publishTriggerFailed publishes one run.trigger_failed envelope for a
// TRIGGER_BUILD command Jenkins refused. It reports whether the pending
// imperative message may be acknowledged. An unresolved tenant publishes
// nothing and counts a rejection: a cell must not write a record under a tenant
// it does not host.
func (h *BusHandler) publishTriggerFailed(ns, name string, payload *mitev1.TriggerBuildPayload, result *mitev1.CommandResult) bool {
	key := ns + "/" + name

	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, payload.JobPath)
	if tenant == "" {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("failed trigger unresolved, publishing no envelope",
			"controller", key, "job_path", payload.JobPath)
		return true
	}

	reasonCode := reasonCodeForStatus(result.HttpStatus)
	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindRunTriggerFailed,
		RunID:         payload.RunId,
		IntentKey:     payload.IntentKey,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		JobPath:       payload.JobPath,
		// A TriggerBuildPayload carries no timestamp, so the gateway clock
		// is the only source.
		OccurredAt: time.Now().UTC(),
		Payload: map[string]interface{}{
			"error":       truncateError(result.Error),
			"reason_code": reasonCode,
			"http_status": result.HttpStatus,
			"actor":       payload.Actor,
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal trigger failure envelope failed", "controller", key, "error", err)
		return true
	}

	if _, err := h.publisher.PublishJetStream(bus.BuildSubject(h.cluster, ns, name), data); err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("trigger failure publish failed, leaving the command unacknowledged",
			"controller", key, "error", err)
		return false
	}

	h.metrics.triggerFailedTotal.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("reason_code", reasonCode)))
	return true
}

// publishTriggerAccepted publishes one run.trigger_accepted envelope for a
// TRIGGER_BUILD command Jenkins accepted onto its queue. It reports whether
// the pending imperative message may be acknowledged. An unresolved tenant
// publishes nothing and counts a rejection: a cell must not write a record
// under a tenant it does not host.
func (h *BusHandler) publishTriggerAccepted(ns, name string, payload *mitev1.TriggerBuildPayload, result *mitev1.CommandResult) bool {
	key := ns + "/" + name

	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, payload.JobPath)
	if tenant == "" {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("accepted trigger unresolved, publishing no envelope",
			"controller", key, "job_path", payload.JobPath)
		return true
	}

	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindRunTriggerAccepted,
		RunID:         payload.RunId,
		IntentKey:     payload.IntentKey,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		JobPath:       payload.JobPath,
		// A TriggerBuildPayload carries no timestamp, so the gateway clock
		// is the only source.
		OccurredAt: time.Now().UTC(),
		Payload: map[string]interface{}{
			"http_status": result.HttpStatus,
			"actor":       payload.Actor,
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal trigger acceptance envelope failed", "controller", key, "error", err)
		return true
	}

	if _, err := h.publisher.PublishJetStream(bus.BuildSubject(h.cluster, ns, name), data); err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("accepted trigger publish failed, leaving the command unacknowledged",
			"controller", key, "error", err)
		return false
	}

	h.metrics.triggerAcceptedTotal.Add(context.Background(), 1)
	return true
}

// publishResumeFailed publishes one run.resume_failed envelope for a
// RESUME_BUILD command the target could not apply. It answers nothing: the
// caller acknowledges the command whatever this publish does.
//
// The envelope carries the instance the command named, because the recorder's
// write is guarded by it. A command that names no run and a command that names
// no instance therefore publish nothing: the recorder could not apply either.
//
// An unresolved tenant publishes nothing and counts a rejection, which is the
// rule both trigger publishers follow: a cell must not write a record under a
// tenant it does not host.
func (h *BusHandler) publishResumeFailed(ns, name string, payload *mitev1.ResumeBuildPayload, result *mitev1.CommandResult) {
	key := ns + "/" + name

	if payload == nil || payload.RunId == "" || payload.Instance == "" {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("failed resume names no run or no instance, publishing no envelope",
			"controller", key)
		return
	}

	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, payload.JobPath)
	if tenant == "" {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("failed resume unresolved, publishing no envelope",
			"controller", key, "job_path", payload.JobPath)
		return
	}

	reasonCode := reasonCodeForResumeError(result.Error)
	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindRunResumeFailed,
		RunID:         payload.RunId,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		JobPath:       payload.JobPath,
		// A ResumeBuildPayload carries no timestamp, so the gateway clock
		// is the only source.
		OccurredAt: time.Now().UTC(),
		Payload: map[string]interface{}{
			"instance":    payload.Instance,
			"error":       truncateError(result.Error),
			"reason_code": reasonCode,
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal resume failure envelope failed", "controller", key, "error", err)
		return
	}

	if _, err := h.publisher.PublishJetStream(bus.BuildSubject(h.cluster, ns, name), data); err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("resume failure publish failed, the run keeps its resume deadline",
			"controller", key, "run_id", payload.RunId, "error", err)
		return
	}

	h.metrics.resumeFailedTotal.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("reason_code", reasonCode)))
	h.log().Info("failed resume recorded",
		"controller", key, "run_id", payload.RunId,
		"instance", payload.Instance, "reason_code", reasonCode)
}

// pendingCommand returns the imperative command a pending message carries, or
// nil when it carries none. The pending message's Data holds the marshaled
// OperatorMessage, so the gateway decodes it again rather than keeping a second
// map. One decode serves every command type the outcome switch answers.
func pendingCommand(msg *nats.Msg) *mitev1.ImperativeCommand {
	var op mitev1.OperatorMessage
	if err := json.Unmarshal(msg.Data, &op); err != nil {
		return nil
	}
	return op.GetImperative()
}

// OnCommandResult publishes the result to the mite inbound subject — or, for a
// REPLAY_WEBHOOK command, to the dedicated whreply.* subject so it bypasses the
// reconciler's shared command-result buffer. If the result matches a pending
// imperative command, the JetStream msg is acked so it is not redelivered after
// a gateway restart.
//
// A TRIGGER_BUILD result first publishes one run record: run.trigger_failed
// for a refused trigger, or run.trigger_accepted for an accepted one. Its
// pending message waits for that publish, so either outcome's failed publish
// leaves the stream to redeliver the command. A failed RESUME_BUILD result
// publishes one run.resume_failed record and acks whatever that publish
// answers. Every other case acks at once.
func (h *BusHandler) OnCommandResult(name, ns string, result *mitev1.CommandResult) {
	key := ns + "/" + name

	// Under lock: ack the pending JetStream imperative message and determine
	// whether this was a webhook-replay command (routing decision).
	var pendingMsg *nats.Msg
	var isReplay bool
	h.mu.Lock()
	if byCmd := h.pendingAcks[key]; byCmd != nil {
		if pending, ok := byCmd[result.CommandId]; ok {
			pendingMsg = pending.msg
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

	if pendingMsg != nil && h.recordCommandOutcome(ns, name, pendingMsg, result) {
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

// OnLogChunk is implemented in log_writer.go.

// OnTailAck is implemented in tail.go.

// releasePendingImperatives naks the imperative messages this connection
// received and never returned a result for, so the stream redelivers them at
// once. OnDisconnect defers it, which keeps it on the superseded-stream path
// too: a mite that reconnects before its old stream tears down still needs
// those commands. A release the superseded check skips leaves the message to
// wait out the JetStream ack timeout, and an operator sees a trigger vanish
// for the length of that timeout.
//
// The token scope is what makes the early release safe: pendingAcks is keyed by
// controller, so an unscoped nak from a superseded stream would also redeliver
// a command the replacement connection already runs. A zero token releases
// every entry, which matches the "a zero token is current" rule the rest of
// this handler follows.
//
// The bookkeeping moves under h.mu and the naks go out after the unlock. A nak
// is a buffered publish, but it can still wait on the outbound buffer, and h.mu
// is on every controller's heartbeat path. Releasing the lock first costs the
// caller no ordering: the cancel of this connection's pump already ran under
// h.mu before this call, so every nak still lands after that cancel, and the
// pump re-reads the cancelled context under h.mu before it files anything.
func (h *BusHandler) releasePendingImperatives(ns, name string, token int64) {
	key := ns + "/" + name
	var release []*nats.Msg

	h.mu.Lock()
	if byCmd := h.pendingAcks[key]; byCmd != nil {
		for cmdID, pending := range byCmd {
			if token != 0 && pending.token != token {
				continue
			}
			release = append(release, pending.msg)
			delete(byCmd, cmdID)
			// A redelivered command re-registers its replay flag on the next
			// dispatch, so drop the stale one.
			if flags := h.replayCmds[key]; flags != nil {
				delete(flags, cmdID)
				if len(flags) == 0 {
					delete(h.replayCmds, key)
				}
			}
		}
		if len(byCmd) == 0 {
			delete(h.pendingAcks, key)
		}
	}
	h.mu.Unlock()

	for _, msg := range release {
		_ = msg.Nak()
	}
}

// OnDisconnect stops the watch goroutines and clears volatile state.
func (h *BusHandler) OnDisconnect(name, ns string, token int64) {
	key := ns + "/" + name

	// A real stream ended either way, so the connection metrics move even when
	// the teardown below is skipped — otherwise a superseded stream leaves the
	// active-connection gauge permanently over-counted.
	h.metrics.miteStreamConnections.Add(context.Background(), -1)
	h.metrics.miteStreamDisconnects.Add(context.Background(), 1)

	// Scoped by token, so this is correct to run even for a superseded
	// stream's late teardown: it can only ever remove its own epoch's
	// entries, never a replacement connection's.
	h.clearBuildWorker(ns, name, token)
	h.clearClaimSlots(ns, name, token)
	h.clearCrossTenantSlots(ns, name, token)

	// Deferred, so the nak lands after this stream's watch goroutines are
	// cancelled — the superseded branch below returns early, and the live
	// branch cancels further down. A nak issued while this stream's own pump
	// still runs is fetched straight back by that dying pump, which then only
	// naks it again.
	defer h.releasePendingImperatives(ns, name, token)

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

	// tailMu is independent of h.mu, so this is safe to call while h.mu
	// is held; it never blocks on anything else in this critical section.
	h.clearTailLeasesForController(ns, name)

	// stateMu is independent of h.mu too. A disconnect ends every push
	// this cell had in flight, and without this drop each entry lives
	// for the whole gateway lifetime.
	h.dropCellStateEntries(ns, name)

	if cancel, ok := h.cancels[key]; ok {
		cancel()
		delete(h.cancels, key)
	}
	delete(h.certExpiry, key)
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
		h.metrics.miteWatchFailures.Add(ctx, 1, metric.WithAttributes(
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
	for _, kind := range []string{"desired", "imperative", "content", "tail"} {
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
func (h *BusHandler) watchImperative(ctx context.Context, ns, name string, token int64, send Sender) {
	if h.conn == nil {
		return
	}
	streamName := "varroa"
	consumer := "imp-" + h.cluster + "-" + ns + "-" + name

	go h.markImperativesInProgress(ctx, ns, name, token)

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
			// EnsureConsumer updates a live durable in place, so a changed
			// ack wait applies on the next mite connection. MaxDeliver stays
			// unbounded: a failed run-record publish relies on a repeat, and
			// the stream's 24-hour MaxAge is the bound.
			if err := h.conn.EnsureConsumer(streamName, consumer, &nats.ConsumerConfig{
				Durable:       consumer,
				FilterSubject: bus.MiteOutSubject(h.cluster, ns, name),
				AckPolicy:     nats.AckExplicitPolicy,
				AckWait:       h.imperativeAckWaitOrDefault(),
				MaxDeliver:    -1,
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
		if reconnect := h.pumpImperative(ctx, sub, ns, name, token, send); !reconnect {
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
func (h *BusHandler) pumpImperative(ctx context.Context, sub *nats.Subscription, ns, name string, token int64, send Sender) bool {
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
				h.forwardImperative(ctx, m, ns, name, token, send)
			}
		}
	}
}

// forwardImperative sends one fetched message to the mite and files the ack it
// waits on.
//
// A cancelled context ends the message here, naked and unfiled. The gateway
// cancels this pump before it naks the connection's pending imperatives, and a
// nak makes the message available at once, so this pump's outstanding fetch can
// receive back the very command the disconnect just released. Filing it again
// would put it under a token no later teardown releases, and sending it would
// hand it to a stream the mite no longer reads: the gRPC handler still holds an
// open transport while its deferred OnDisconnect runs, so the send can even
// succeed. Either way the replacement connection waits out the consumer's ack
// timeout, which is the failure this pump exists to prevent.
func (h *BusHandler) forwardImperative(ctx context.Context, m *nats.Msg, ns, name string, token int64, send Sender) {
	// Fast path only. h.mu is where a cancel becomes visible, so the filing
	// below re-reads it under that lock.
	if ctx.Err() != nil {
		_ = m.Nak()
		return
	}
	var op mitev1.OperatorMessage
	if err := json.Unmarshal(m.Data, &op); err != nil {
		// A message that does not decode never decodes. A nak would make
		// the stream deliver it again at once, for the life of the stream.
		h.log().Error("imperative unmarshal failed, terminating the message", "controller", ns+"/"+name, "error", err)
		_ = m.Term()
		return
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
				return
			}
		}
		_ = m.Ack() // not an imperative; ack and skip
		return
	}
	// Track pending ack (scoped to this controller) — OnCommandResult
	// will ack it once the mite reports completion.
	key := ns + "/" + name
	h.mu.Lock()
	// OnConnect and OnDisconnect both cancel this pump while they hold h.mu, so
	// this read is the consistent snapshot the fast path above is not. A
	// supersede that lands in the gap between the two files the command under a
	// token no teardown releases, and the replacement then waits out the
	// consumer's ack timeout. markWatchDegraded reads the context under h.mu for
	// the same reason.
	if ctx.Err() != nil {
		h.mu.Unlock()
		_ = m.Nak()
		return
	}
	if h.pendingAcks[key] == nil {
		h.pendingAcks[key] = make(map[string]pendingImperative)
	}
	h.pendingAcks[key][imp.CommandId] = pendingImperative{msg: m, token: token}
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
			// Only this connection's own delivery is dropped: a
			// replacement stream may already own the entry.
			if pending, ok := byCmd[imp.CommandId]; ok && pending.token == token {
				delete(byCmd, imp.CommandId)
			}
			if len(byCmd) == 0 {
				delete(h.pendingAcks, key)
			}
		}
		h.mu.Unlock()
		_ = m.Nak()
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
	// Retried for the same reason as the desired-state subscription: giving up
	// after one setup error leaves the mite unable to serve content fetches for
	// the rest of the connection, with no operator-visible signal.
	subject := bus.MiteContentSubject(h.cluster, ns, name)
	pacer := h.newRebuildPacer()
	for {
		var (
			sub     *nats.Subscription
			denied  <-chan struct{}
			release func()
		)
		established := h.retryEstablish(ctx, ns, name, "content", func() error {
			nc := h.conn.NATSConn()
			if nc == nil {
				return errors.New("nats connection unavailable")
			}
			// The watcher goes first: the server reports a denial only
			// after Subscribe returns, and only to the error handler.
			d, rel := h.conn.WatchSubscribeDenied(subject)
			s, err := nc.SubscribeSync(subject)
			if err != nil {
				rel()
				return fmt.Errorf("content subscribe: %w", err)
			}
			if err := h.conn.ConfirmSubscription(d); err != nil {
				_ = s.Unsubscribe()
				rel()
				return fmt.Errorf("content subscribe %s: %w", subject, err)
			}
			sub, denied, release = s, d, rel
			return nil
		})
		if !established {
			return // context cancelled: the mite disconnected
		}
		start := time.Now()
		cause := h.pumpContent(ctx, sub, denied, ns, name, send)
		_ = sub.Unsubscribe()
		release()
		if cause == nil {
			return
		}
		// The subscription died while the mite is still connected. Returning
		// here would leave it unable to serve any content fetch for the rest
		// of the connection, with no signal.
		h.markWatchDegraded(ctx, ns, name, "content", "content subscription lost; rebuilding: "+cause.Error())
		if !pacer.wait(ctx, time.Since(start)) {
			return
		}
	}
}

// pumpContent bridges ContentRequests from the bus to the gRPC stream. It
// returns the cause when the subscription died and must be rebuilt, and nil
// when ctx was cancelled. A signal on denied, a server denial of the
// subscription, is such a death.
func (h *BusHandler) pumpContent(ctx context.Context, sub *nats.Subscription, denied <-chan struct{}, ns, name string, send Sender) error {
	key := ns + "/" + name
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-denied:
			h.log().Error("content subscription denied", "controller", key)
			return bus.ErrSubscribeDenied
		default:
		}
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				continue
			}
			// Any other error rebuilds the subscription through the pacer.
			// A retry in place spins on an error that does not clear.
			h.log().Error("content subscription lost", "controller", key, "error", err)
			return err
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

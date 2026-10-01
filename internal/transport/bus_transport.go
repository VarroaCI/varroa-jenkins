package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// BusTransport implements Transport over NATS. It replaces the in-memory
// Registry so the operator can reach mites through the bus regardless of
// which gateway holds the socket.
type BusTransport struct {
	cluster    string
	conn       *bus.Conn
	snapshotKV *bus.KV
	presenceKV *bus.KV

	mu      sync.Mutex
	results map[string]chan *mitev1.CommandResult  // keyed by "ns/name"
	subs    map[string]*inboundSub                 // subscriptions per mite
	reports map[string]*mitev1.ObservabilityReport // latest report per mite

	// TokenGrantFunc mints a fresh Jenkins token for a mite. When set, a
	// TokenRefreshRequest received from a mite is answered with a TokenGrant
	// published to the mite's outbound subject. Nil disables the grant path.
	TokenGrantFunc func(ns, name string) (token string, exp int64, err error)

	Logger *slog.Logger
}

// Ensure BusTransport implements Transport.
var _ Transport = (*BusTransport)(nil)

func (b *BusTransport) log() *slog.Logger {
	if b.Logger != nil {
		return b.Logger
	}
	return slog.Default()
}

// NewBusTransport creates a BusTransport. snapshotKV holds mite snapshots and
// presenceKV holds liveness records.
func NewBusTransport(cluster string, conn *bus.Conn, snapshotKV, presenceKV *bus.KV) *BusTransport {
	return &BusTransport{
		cluster:    cluster,
		conn:       conn,
		snapshotKV: snapshotKV,
		presenceKV: presenceKV,
		results:    make(map[string]chan *mitev1.CommandResult),
		subs:       make(map[string]*inboundSub),
		reports:    make(map[string]*mitev1.ObservabilityReport),
	}
}

// inboundSub is the operator's subscription to one mite's inbound subject.
// The server can reject a subscription after the subscribe call returned,
// so denied holds the watcher signal for that subject. Only ensureSub reads
// it, under BusTransport.mu.
type inboundSub struct {
	sub     *bus.Sub
	denied  <-chan struct{}
	release func()
	// wasDenied records that this entry replaces a denied subscription.
	// ensureSub logs the first denial at Error and later rebuilds at Debug.
	// It logs Info once when the entry is older than inboundAcceptGrace
	// and has no denial signal.
	wasDenied bool
	// subscribedAt is when ensureSub sent this subscription. A denial
	// arrives asynchronously, so a call soon after the subscribe cannot
	// tell an accepted subscription from one that the server will deny.
	subscribedAt time.Time
}

// inboundAcceptGrace is how long a rebuilt subscription must stay without
// a denial signal before ensureSub reports it as accepted. The server
// answers a subscription in one round trip, so one second is ample.
const inboundAcceptGrace = time.Second

func (s *inboundSub) close() {
	_ = s.sub.Unsubscribe()
	s.release()
}

// ensureSub creates a NATS subscription for the given mite if none exists.
// It subscribes again when the server denied the current subscription.
// Incoming CommandResult messages go to a buffered channel that a rebuild
// keeps.
//
// Every read and write of b.subs and b.results happens under b.mu. A denial
// only queues a signal on the entry's buffered watcher channel, and only
// this function reads that signal and replaces the entry.
func (b *BusTransport) ensureSub(ns, name string) {
	key := ns + "/" + name
	subject := bus.MiteInSubject(b.cluster, ns, name)
	b.mu.Lock()
	defer b.mu.Unlock()

	wasDenied := false
	if cur, ok := b.subs[key]; ok {
		select {
		case <-cur.denied:
		default:
			if cur.wasDenied && time.Since(cur.subscribedAt) >= inboundAcceptGrace {
				cur.wasDenied = false
				b.log().Info("mite inbound subscription accepted again", "controller", key, "subject", subject)
			}
			return
		}
		cur.close()
		delete(b.subs, key)
		if cur.wasDenied {
			b.log().Debug("mite inbound subscription still denied; subscribing again", "controller", key, "subject", subject)
		} else {
			b.log().Error("mite inbound subscription denied; subscribing again", "controller", key, "subject", subject)
		}
		wasDenied = true
	}

	ch, kept := b.results[key]
	if !kept {
		ch = make(chan *mitev1.CommandResult, 64)
		b.results[key] = ch
	}

	denied, release := b.conn.WatchSubscribeDenied(subject)
	sub, err := b.conn.SubscribeData(subject, func(data []byte) {
		var mm mitev1.MiteMessage
		if err := json.Unmarshal(data, &mm); err != nil {
			b.log().Error("unmarshal mite message failed", "controller", key, "error", err)
			return
		}
		if cr := mm.GetCommandResult(); cr != nil {
			select {
			case ch <- cr:
			default:
				b.log().Error("results channel full, dropping message", "controller", key)
			}
			return
		}
		if report := mm.GetObservabilityReport(); report != nil {
			b.mu.Lock()
			b.reports[key] = report
			b.mu.Unlock()
			return
		}
		if mm.GetTokenRefreshRequest() != nil {
			b.handleTokenRefresh(ns, name)
		}
	})
	if err != nil {
		release()
		b.log().Error("subscribe failed", "subject", subject, "error", err)
		if !kept {
			delete(b.results, key)
		}
		return
	}
	b.subs[key] = &inboundSub{sub: sub, denied: denied, release: release, wasDenied: wasDenied, subscribedAt: time.Now()}
}

// handleTokenRefresh mints a fresh Jenkins token and publishes a TokenGrant to
// the mite's outbound subject (forwarded to the mite's gRPC stream by the
// gateway), mirroring SendImperative. No-op if no grant function is configured.
func (b *BusTransport) handleTokenRefresh(ns, name string) {
	if b.TokenGrantFunc == nil {
		b.log().Warn("token refresh request received but no grant function configured", "controller", ns+"/"+name)
		return
	}
	token, exp, err := b.TokenGrantFunc(ns, name)
	if err != nil {
		b.log().Error("token grant mint failed", "controller", ns+"/"+name, "error", err)
		return
	}
	b.log().Debug("token grant minted, publishing", "controller", ns+"/"+name, "exp", exp)
	data, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_TokenGrant{
			TokenGrant: &mitev1.TokenGrant{
				MiteJenkinsToken:    token,
				MiteJenkinsTokenExp: exp,
			},
		},
	})
	if err != nil {
		b.log().Error("marshal token grant failed", "controller", ns+"/"+name, "error", err)
		return
	}
	if _, err := b.conn.PublishJetStream(bus.MiteOutSubject(b.cluster, ns, name), data); err != nil {
		b.log().Error("publish token grant failed", "controller", ns+"/"+name, "error", err)
	}
}

// desiredRequestTimeout bounds how long Send waits for the gateway to
// confirm that it forwarded the desired state to the mite stream.
const desiredRequestTimeout = 5 * time.Second

// Send delivers the desired state to the mite as a core NATS request. The
// gateway that holds the mite stream answers after it forwards the command.
// NATS keeps no copy of the command, because the command carries credential
// values. When no gateway answers, Send returns ErrMiteUnreachable. The
// reconciler sends the command again on the next tick or on the reconnect.
func (b *BusTransport) Send(ctx context.Context, ns, name string, msg *mitev1.OperatorMessage) error {
	// Ensure the NATS subscription for this mite's inbound subject exists
	// before the mite connects, so TokenRefreshRequest messages are never
	// published without a subscriber. The subscription is idempotent.
	b.ensureSub(ns, name)

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := b.conn.RequestWithContext(ctx, bus.MiteDesiredSubject(b.cluster, ns, name), data, desiredRequestTimeout); err != nil {
		if bus.IsNoReply(err) {
			return fmt.Errorf("%w: %v", ErrMiteUnreachable, err)
		}
		return fmt.Errorf("desired-state request: %w", err)
	}
	return nil
}

// SendImperative publishes an edge-triggered one-shot command to the JetStream
// durable subject (NOT the desired-state request path), so the command survives
// gateway restarts. JetStream delivery is at-least-once (a lost ack triggers
// redelivery); the gateway only acks a command after the mite reports a matching
// CommandResult, and the mite's actions (e.g. safe restart) are idempotent, so a
// duplicate delivery is harmless.
func (b *BusTransport) SendImperative(_ context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	data, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{Imperative: cmd},
	})
	if err != nil {
		return err
	}
	_, err = b.conn.PublishJetStream(bus.MiteOutSubject(b.cluster, ns, name), data)
	return err
}

// ObservabilityReport returns the latest observability report seen on the mite's
// inbound subject or from the snapshot KV. The cache remains available after
// disconnect until replaced.
func (b *BusTransport) ObservabilityReport(ns, name string) *mitev1.ObservabilityReport {
	b.ensureSub(ns, name)
	key := ns + "/" + name
	b.mu.Lock()
	report := b.reports[key]
	b.mu.Unlock()
	if report != nil {
		return report
	}
	// Fallback: read from KV (written by the gateway's BusHandler).
	data, err := b.snapshotKV.Get(bus.ObservabilityKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return nil
	}
	var r mitev1.ObservabilityReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	report = &r
	b.mu.Lock()
	b.reports[key] = report
	b.mu.Unlock()
	return report
}

// DrainResults returns buffered command results received since the last
// drain. It subscribes to the mite's inbound subject on first call.
func (b *BusTransport) DrainResults(ns, name string) []*mitev1.CommandResult {
	b.ensureSub(ns, name)

	key := ns + "/" + name
	b.mu.Lock()
	ch := b.results[key]
	b.mu.Unlock()

	if ch == nil {
		return nil
	}

	var results []*mitev1.CommandResult
	for {
		select {
		case res := <-ch:
			results = append(results, res)
		default:
			return results
		}
	}
}

// Snapshot returns the latest state snapshot from KV, or nil.
func (b *BusTransport) Snapshot(ns, name string) *mitev1.StateSnapshot {
	data, err := b.snapshotKV.Get(bus.SnapshotKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return nil
	}
	var snap mitev1.StateSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil
	}
	return &snap
}

// Health returns the last reported Jenkins health, or "unreachable".
func (b *BusTransport) Health(ns, name string) string {
	snap := b.Snapshot(ns, name)
	if snap == nil {
		return "unreachable"
	}
	if snap.JenkinsHealth == "" {
		return "unknown"
	}
	return snap.JenkinsHealth
}

// Connected reports whether the mite has a live presence entry in KV.
func (b *BusTransport) Connected(ns, name string) bool {
	data, err := b.presenceKV.Get(bus.PresenceKey(b.cluster, ns, name))
	return err == nil && data != nil
}

// Info returns version, last heartbeat, and cert expiry for the mite
// from the gateway-written presence record.
func (b *BusTransport) Info(ns, name string) (version string, lastHeartbeat, certExpiry time.Time, ok bool) {
	data, err := b.presenceKV.Get(bus.PresenceKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return "", time.Time{}, time.Time{}, false
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return "", time.Time{}, time.Time{}, false
	}
	return p.Version, p.LastHeartbeat, p.CertExpiry, true
}

// IdleGauges returns idle gauges from the presence record, if available.
func (b *BusTransport) IdleGauges(ns, name string) (*mitev1.IdleGauges, time.Time, bool) {
	data, err := b.presenceKV.Get(bus.PresenceKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return nil, time.Time{}, false
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, time.Time{}, false
	}
	if p.IdleGaugesJSON == "" {
		return nil, time.Time{}, false
	}
	var gauges mitev1.IdleGauges
	if err := json.Unmarshal([]byte(p.IdleGaugesJSON), &gauges); err != nil {
		return nil, time.Time{}, false
	}
	return &gauges, p.IdleGaugesReceivedAt, true
}

// ConnEpoch returns the connection epoch from the presence record.
// The epoch is set once on registration and stable across heartbeats.
func (b *BusTransport) ConnEpoch(ns, name string) (int64, bool) {
	data, err := b.presenceKV.Get(bus.PresenceKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return 0, false
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return 0, false
	}
	return p.Epoch, true
}

// StreamDegraded reports whether the gateway's bus→gRPC bridge for this mite
// is broken, from the presence record the gateway writes. A missing or
// unreadable presence record reports not-degraded: absence of evidence is not
// evidence of breakage, and a disconnected mite is already handled by Info.
func (b *BusTransport) StreamDegraded(ns, name string) (string, bool) {
	data, err := b.presenceKV.Get(bus.PresenceKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return "", false
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	return p.StreamDegradedReason, p.StreamDegraded
}

// Close unsubscribes all mite subscriptions. The NATS connection is owned
// by the caller and must be closed separately.
func (b *BusTransport) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, sub := range b.subs {
		sub.close()
		delete(b.subs, key)
		delete(b.results, key)
	}
}

// FetchLastApplied requests the mite's last-applied content via a NATS
// request/reply bridged through the gateway to the mite stream.
func (b *BusTransport) FetchLastApplied(ctx context.Context, ns, name string) (*mitev1.ContentResponse, error) {
	reqID := uuid.New().String()
	payload, err := json.Marshal(&mitev1.ContentRequest{RequestId: reqID})
	if err != nil {
		return nil, err
	}
	reply, err := b.conn.RequestWithContext(ctx, bus.MiteContentSubject(b.cluster, ns, name), payload, 10*time.Second)
	if err != nil {
		return nil, ErrContentUnavailable
	}
	var resp mitev1.ContentResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PluginInventory returns the latest plugin inventory from KV, or nil.
func (b *BusTransport) PluginInventory(ns, name string) *mitev1.PluginInventory {
	data, err := b.snapshotKV.Get(bus.PluginInventoryKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return nil
	}
	var inv mitev1.PluginInventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil
	}
	return &inv
}

// InstalledPluginsHash returns the installed_plugins_hash from the presence record.
func (b *BusTransport) InstalledPluginsHash(ns, name string) (string, bool) {
	data, err := b.presenceKV.Get(bus.PresenceKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return "", false
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	if p.InstalledPluginsHash == "" {
		return "", false
	}
	return p.InstalledPluginsHash, true
}

// PluginClassification returns the stored classified inventory from KV.
func (b *BusTransport) PluginClassification(ns, name string) (*ClassifiedInventory, bool) {
	data, err := b.snapshotKV.Get(bus.PluginClassificationKey(b.cluster, ns, name))
	if err != nil || data == nil {
		return nil, false
	}
	var c ClassifiedInventory
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false
	}
	return &c, true
}

// PutPluginClassification stores a classified inventory in KV.
func (b *BusTransport) PutPluginClassification(_ context.Context, ns, name string, c *ClassifiedInventory) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return b.snapshotKV.Put(bus.PluginClassificationKey(b.cluster, ns, name), data)
}

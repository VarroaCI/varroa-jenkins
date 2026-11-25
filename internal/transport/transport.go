// Package transport defines the interface through which the operator
// communicates with mite sidecars without knowing where their gRPC sockets
// live. BusTransport is the operator implementation. BusReadModelTransport is
// the read-only BFF implementation. Both use NATS JetStream.
package transport

import (
	"context"
	"errors"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// ErrContentUnavailable is returned when the mite cannot deliver its
// last-applied content (offline, unsupported version, or timeout).
var ErrContentUnavailable = errors.New("mite last-applied content unavailable")

// ErrMiteUnreachable is returned by Send when no gateway confirms that it
// forwarded the desired state to a live mite stream. The command is not
// stored anywhere, so the caller must send it again.
var ErrMiteUnreachable = errors.New("no gateway forwarded the desired state to the mite")

// MiteInfo is what the presence record of one mite holds.
type MiteInfo struct {
	// Version is the version of the mite binary, never the Jenkins version.
	Version       string
	LastHeartbeat time.Time
	CertExpiry    time.Time
	// StreamOpen reports that the gateway that wrote the record holds an
	// open stream of the mite. With a stale LastHeartbeat, the stream is
	// open and silent.
	StreamOpen bool
	// ReadLoop names what the gateway read loop of a silent stream does
	// (bus.Presence.ReadLoop), and ReadLoopSince is when it started. Both
	// are empty while the heartbeat is fresh.
	ReadLoop      string
	ReadLoopSince time.Time
}

// miteInfoFromPresence returns the MiteInfo of one presence record. Every
// presence reader decodes through it, so the readers return the same
// fields.
func miteInfoFromPresence(p bus.Presence) MiteInfo {
	return MiteInfo{
		Version:       p.Version,
		LastHeartbeat: p.LastHeartbeat,
		CertExpiry:    p.CertExpiry,
		StreamOpen:    p.StreamOpen,
		ReadLoop:      p.ReadLoop,
		ReadLoopSince: p.ReadLoopSince,
	}
}

// HeartbeatStale reports whether LastHeartbeat is older than
// bus.PresenceStaleAfter at now. A stale record does not show a live mite,
// also when its stream is open.
func (i MiteInfo) HeartbeatStale(now time.Time) bool {
	return now.Sub(i.LastHeartbeat) > bus.PresenceStaleAfter
}

// Transport is how the operator talks to a mite. The interface uses
// (ns, name) argument order — the conventional Kubernetes ordering.
type Transport interface {
	// Send pushes an operator message toward the mite for ns/name.
	Send(ctx context.Context, ns, name string, msg *mitev1.OperatorMessage) error

	// SendImperative delivers an edge-triggered one-shot command to the mite.
	// Unlike Send (last-value desired state), this must not be replayed on reconnect.
	SendImperative(ctx context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error

	// DrainResults returns command results received since the last drain.
	DrainResults(ns, name string) []*mitev1.CommandResult

	// Snapshot returns the latest state snapshot, or nil.
	Snapshot(ns, name string) *mitev1.StateSnapshot

	// Health returns the last reported Jenkins health ("healthy", "unhealthy",
	// "unknown", or "unreachable").
	Health(ns, name string) string

	// Connected reports whether a mite stream is currently held (anywhere)
	// and its last heartbeat is no older than bus.PresenceStaleAfter.
	Connected(ns, name string) bool

	// Info returns the presence record of the mite of ns/name. ok is false
	// when no record exists. err is non-nil when the transport cannot read or
	// decode the record; ok is then false. A read error is no evidence that
	// the mite is disconnected.
	Info(ns, name string) (info MiteInfo, ok bool, err error)

	// IdleGauges returns the latest idle gauge snapshot from the mite's
	// heartbeat, along with the timestamp it was received. The final return
	// value is false if no gauges have been received yet.
	IdleGauges(ns, name string) (*mitev1.IdleGauges, time.Time, bool)

	// StreamDegraded reports whether the gateway's bus→gRPC bridge for this
	// mite is currently broken (a subscription or JetStream consumer failed and
	// is being retried), with the underlying reason. A degraded bridge means
	// operator-published desired state is NOT reaching the mite even though its
	// gRPC stream looks healthy. False for transports that do not
	// bridge over the bus.
	StreamDegraded(ns, name string) (reason string, degraded bool)

	// ConnEpoch returns a monotonically-increasing connection epoch for the
	// given mite key (ns/name). The epoch increments on each (re)registration
	// so callers can detect a restart. The second return value is false if
	// no mite has ever registered under this key.
	ConnEpoch(ns, name string) (int64, bool)

	// ObservabilityReport returns the latest mite observability report for
	// the controller, or nil if none has been received.
	ObservabilityReport(ns, name string) *mitev1.ObservabilityReport

	// EnsureInbound makes the transport receive what the mite of ns/name
	// sends to the operator, such as a token refresh request, before the
	// first desired-state push. Only the replica that owns the controller
	// calls it. A transport that needs no subscription does nothing.
	EnsureInbound(ns, name string)

	// FetchLastApplied requests the mite's actual last-applied content via an
	// on-demand content-fetch RPC bridged through NATS. Returns a
	// ContentResponse or ErrContentUnavailable if the mite is offline/times out.
	FetchLastApplied(ctx context.Context, ns, name string) (*mitev1.ContentResponse, error)

	// PluginInventory returns the latest plugin inventory pushed by the mite,
	// or nil if none has been received.
	PluginInventory(ns, name string) *mitev1.PluginInventory

	// InstalledPluginsHash returns the installed_plugins_hash from the mite's
	// most recent heartbeat, or ("", false) if not available.
	InstalledPluginsHash(ns, name string) (string, bool)

	// PluginClassification returns the stored classified inventory from the
	// read model, or nil if none has been stored.
	PluginClassification(ns, name string) (*ClassifiedInventory, bool)

	// PutPluginClassification stores a classified inventory in the read model.
	PutPluginClassification(ctx context.Context, ns, name string, c *ClassifiedInventory) error

	// Forget drops every per-mite entry that the transport keeps for ns/name:
	// an inbound subscription, buffered command results, and a cached report.
	// The reconciler calls it when the controller is deleted, so a new
	// controller of the same name drains no result of the old mite. A
	// transport that keeps no such entry does nothing.
	Forget(ns, name string)

	// ForgetMatching does what Forget does for every mite whose ns and name
	// match. The operator calls it for the mites of controllers that it no
	// longer owns, so only the owning replica answers a mite. match runs
	// under the transport lock and must not call the transport.
	ForgetMatching(match func(ns, name string) bool)
}

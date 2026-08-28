// Package transport defines the interface through which the operator
// communicates with mite sidecars without knowing where their gRPC sockets
// live. Implementations: LocalRegistry (today's in-process adapter) and
// BusTransport (NATS JetStream-backed, for the sharded architecture).
package transport

import (
	"context"
	"errors"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// ErrContentUnavailable is returned when the mite cannot deliver its
// last-applied content (offline, unsupported version, or timeout).
var ErrContentUnavailable = errors.New("mite last-applied content unavailable")

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

	// Connected reports whether a mite stream is currently held (anywhere).
	Connected(ns, name string) bool

	// Info returns version, last heartbeat, and cert expiry for the connected
	// mite. The final return value is false if no mite is connected.
	Info(ns, name string) (version string, lastHeartbeat, certExpiry time.Time, ok bool)

	// IdleGauges returns the latest idle gauge snapshot from the mite's
	// heartbeat, along with the timestamp it was received. The final return
	// value is false if no gauges have been received yet.
	IdleGauges(ns, name string) (*mitev1.IdleGauges, time.Time, bool)

	// StreamDegraded reports whether the gateway's bus→gRPC bridge for this
	// mite is currently broken (a KV watch or JetStream subscription failed and
	// is being retried), with the underlying reason. A degraded bridge means
	// operator-published desired state is NOT reaching the mite even though its
	// gRPC stream looks healthy — see #509. False for transports that do not
	// bridge over the bus.
	StreamDegraded(ns, name string) (reason string, degraded bool)

	// ConnEpoch returns a monotonically-increasing connection epoch for the
	// given mite key (ns/name). The epoch increments on each (re)registration
	// so callers can detect a restart. The second return value is false if
	// no mite has ever registered under this key.
	ConnEpoch(ns, name string) (int64, bool)

	// ClearDesired removes any stored desired state for the mite. Called on
	// Controller deletion so the value does not outlive the resource. No-op for
	// transports that do not persist desired state.
	ClearDesired(ctx context.Context, ns, name string) error

	// ObservabilityReport returns the latest mite observability report for
	// the controller, or nil if none has been received.
	ObservabilityReport(ns, name string) *mitev1.ObservabilityReport

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
}

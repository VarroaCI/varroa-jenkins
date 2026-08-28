package mite

import (
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/sse"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// RegistryHandler implements StreamHandler using the in-memory Registry.
// This preserves the single-process behavior where the reconciler and
// gRPC server share a local connection registry.
type RegistryHandler struct {
	reg         *Registry
	broadcaster *sse.Broadcaster

	Logger *slog.Logger
}

// Ensure RegistryHandler implements StreamHandler.
var _ StreamHandler = (*RegistryHandler)(nil)

func (h *RegistryHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// NewRegistryHandler creates a handler backed by the given registry.
func NewRegistryHandler(reg *Registry, b *sse.Broadcaster) *RegistryHandler {
	return &RegistryHandler{reg: reg, broadcaster: b}
}

// OnConnect registers the mite connection. The send function is not needed
// because operator messages are pushed via Registry.Send from the reconciler.
// The stream is stored in the Registry so Send can type-assert it back to
// the gRPC server stream.
func (h *RegistryHandler) OnConnect(name, ns, version string, certExpiry interface{}, _ Sender, stream interface{}) int64 {
	expiry, _ := certExpiry.(time.Time)
	// Take the epoch from the Register call itself: a follow-up GetEpoch can
	// observe a *newer* connection's epoch if the mite reconnects in between,
	// and this stream would then hold a token that outlives it.
	conn := h.reg.Register(name, ns, nil, stream, version, expiry)

	key := ns + "/" + name
	var epoch int64
	if conn != nil {
		epoch = conn.Epoch
	}
	h.log().Info("mite connected", "controller", key, "version", version)
	if h.broadcaster != nil {
		h.broadcaster.Notify(key, sse.Record{
			Event: "connected",
			Data: map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"version":   version,
			},
		})
	}
	return epoch
}

// OnHeartbeat updates the last-heartbeat timestamp and idle gauges.
func (h *RegistryHandler) OnHeartbeat(name, ns string, hb *mitev1.Heartbeat) {
	h.reg.UpdateHeartbeat(name, ns, hb.Version, nil, hb.Idle, hb.InstalledPluginsHash)

	key := ns + "/" + name
	if h.broadcaster != nil {
		h.broadcaster.Notify(key, sse.Record{
			Event: "heartbeat",
			Data: map[string]interface{}{
				"version": hb.Version,
			},
		})
	}
}

// OnSnapshot stores the state snapshot and publishes an SSE event.
func (h *RegistryHandler) OnSnapshot(name, ns string, snap *mitev1.StateSnapshot) {
	// UpdateHeartbeat stores the snapshot under the registry's write lock,
	// avoiding the race of directly mutating mc.Snapshot outside the lock.
	// Pass the existing version through so it is not cleared.
	mc, ok := h.reg.Get(name, ns)
	version := ""
	if ok {
		version = mc.Version
	}
	h.reg.UpdateHeartbeat(name, ns, version, snap, nil, "")

	key := ns + "/" + name
	if h.broadcaster != nil {
		h.broadcaster.Notify(key, sse.Record{
			Event: "snapshot",
			Data: map[string]interface{}{
				"jenkinsVersion": snap.JenkinsVersion,
				"jenkinsHealth":  snap.JenkinsHealth,
				"status":         snap.Status,
				"configHash":     snap.ConfigHash,
				"pluginsHash":    snap.PluginsHash,
				"rbacHash":       snap.RbacHash,
			},
		})
	}
}

// OnCommandResult pushes the result into the connection's Results channel.
func (h *RegistryHandler) OnCommandResult(name, ns string, result *mitev1.CommandResult) {
	mc, ok := h.reg.Get(name, ns)
	if !ok {
		return
	}
	select {
	case mc.Results <- result:
	default:
		key := ns + "/" + name
		h.log().Error("results channel full, dropping command result",
			"controller", key, "commandID", result.CommandId)
	}
}

// OnTokenRefreshRequest is a no-op for the registry handler; token grants
// are served directly by the Server's TokenGrantFunc callback.
func (h *RegistryHandler) OnTokenRefreshRequest(name, ns string, req *mitev1.TokenRefreshRequest) {}

// OnContentResponse is a no-op in the BFF process — content-fetch is only
// active in the gateway (BusHandler). The BFF never holds a mite stream and
// therefore never receives a ContentResponse from a mite.
func (h *RegistryHandler) OnContentResponse(requestID string, resp *mitev1.ContentResponse) {}

// OnJenkinsActivity is a no-op for the registry handler; it has no bus to
// publish to. Only BusHandler does real work.
func (h *RegistryHandler) OnJenkinsActivity(name, ns string, evt *mitev1.JenkinsActivityEvent) {}

// OnObservabilityReport stores the observability report in the registry.
func (h *RegistryHandler) OnObservabilityReport(name, ns string, report *mitev1.ObservabilityReport) {
	h.reg.SetObservabilityReport(name, ns, report)
}

// OnPluginInventory stores the plugin inventory in the registry.
func (h *RegistryHandler) OnPluginInventory(name, ns string, inv *mitev1.PluginInventory) {
	h.reg.SetPluginInventory(name, ns, inv)
}

// IsCurrentConnection reports whether token still matches the registered
// connection's epoch, so the read loop can stop feeding a superseded stream.
func (h *RegistryHandler) IsCurrentConnection(name, ns string, token int64) bool {
	return h.reg.IsCurrentEpoch(name, ns, token)
}

// OnDisconnect unregisters the mite and broadcasts.
func (h *RegistryHandler) OnDisconnect(name, ns string, token int64) {
	// Ignore a late teardown from a stream the mite has already replaced —
	// unregistering here would drop the live connection. Checked atomically
	// with the unregister so a reconnect cannot land between the two.
	if !h.reg.UnregisterIfEpoch(name, ns, token) {
		h.log().Info("ignoring disconnect from superseded stream", "controller", ns+"/"+name)
		return
	}

	key := ns + "/" + name
	if h.broadcaster != nil {
		h.broadcaster.Notify(key, sse.Record{
			Event: "disconnected",
			Data: map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
		})
	}
}

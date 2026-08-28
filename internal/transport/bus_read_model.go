package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// BusReadModel is a read-only view of mite state for components that need
// presence, snapshot, health, and observability reads without participating
// in mite inbound control handling. It does not subscribe to mite inbound
// subjects, does not allocate command-result channels, and does not drain
// command results.
type BusReadModel struct {
	cluster    string
	conn       *bus.Conn
	snapshotKV *bus.KV
	presenceKV *bus.KV
}

// NewBusReadModel creates a read-only bus view backed by the provided KV stores.
func NewBusReadModel(cluster string, conn *bus.Conn, snapshotKV, presenceKV *bus.KV) *BusReadModel {
	return &BusReadModel{
		cluster:    cluster,
		conn:       conn,
		snapshotKV: snapshotKV,
		presenceKV: presenceKV,
	}
}

// Snapshot returns the latest state snapshot from KV, or nil.
func (m *BusReadModel) Snapshot(ns, name string) *mitev1.StateSnapshot {
	data, err := m.snapshotKV.Get(bus.SnapshotKey(m.cluster, ns, name))
	if err != nil || data == nil {
		return nil
	}
	var snap mitev1.StateSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil
	}
	return &snap
}

// Connected reports whether the mite has a live presence entry in KV.
func (m *BusReadModel) Connected(ns, name string) bool {
	data, err := m.presenceKV.Get(bus.PresenceKey(m.cluster, ns, name))
	return err == nil && data != nil
}

// Info returns version, last heartbeat, and cert expiry for the mite
// from the gateway-written presence record.
func (m *BusReadModel) Info(ns, name string) (version string, lastHeartbeat, certExpiry time.Time, ok bool) {
	data, err := m.presenceKV.Get(bus.PresenceKey(m.cluster, ns, name))
	if err != nil || data == nil {
		return "", time.Time{}, time.Time{}, false
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return "", time.Time{}, time.Time{}, false
	}
	return p.Version, p.LastHeartbeat, p.CertExpiry, true
}

// StreamDegraded reports whether the gateway's bus→gRPC bridge for this mite
// is broken, from the presence record the gateway writes.
func (m *BusReadModel) StreamDegraded(ns, name string) (string, bool) {
	data, err := m.presenceKV.Get(bus.PresenceKey(m.cluster, ns, name))
	if err != nil || data == nil {
		return "", false
	}
	var p bus.Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	return p.StreamDegradedReason, p.StreamDegraded
}

// IdleGauges returns idle gauges from the presence record, if available.
func (m *BusReadModel) IdleGauges(ns, name string) (*mitev1.IdleGauges, time.Time, bool) {
	data, err := m.presenceKV.Get(bus.PresenceKey(m.cluster, ns, name))
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

// Health returns the last reported Jenkins health, or "unreachable".
func (m *BusReadModel) Health(ns, name string) string {
	snap := m.Snapshot(ns, name)
	if snap == nil {
		return "unreachable"
	}
	if snap.JenkinsHealth == "" {
		return "unknown"
	}
	return snap.JenkinsHealth
}

// ObservabilityReport returns the latest observability report from the
// snapshot KV, or nil if none has been received.
func (m *BusReadModel) ObservabilityReport(ns, name string) *mitev1.ObservabilityReport {
	data, err := m.snapshotKV.Get(bus.ObservabilityKey(m.cluster, ns, name))
	if err != nil || data == nil {
		return nil
	}
	var r mitev1.ObservabilityReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil
	}
	return &r
}

// PublishSafeRestart publishes a one-shot safe restart command to the mite
// outbound subject. It does not subscribe to the inbound subject, allocate
// a result buffer, or drain command results.
func (m *BusReadModel) PublishSafeRestart(ctx context.Context, ns, name string, cmd *mitev1.ImperativeCommand) error {
	data, err := json.Marshal(&mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_Imperative{Imperative: cmd},
	})
	if err != nil {
		return fmt.Errorf("marshal safe restart: %w", err)
	}
	_, err = m.conn.PublishJetStream(bus.MiteOutSubject(m.cluster, ns, name), data)
	if err != nil {
		return fmt.Errorf("publish safe restart: %w", err)
	}
	return nil
}

// FetchLastAppliedContent requests the mite's last-applied content via the
// content-fetch NATS bridge. Returns ErrContentUnavailable on timeout or offline mite.
func (m *BusReadModel) FetchLastAppliedContent(ctx context.Context, ns, name string) (*mitev1.ContentResponse, error) {
	reqID := uuid.New().String()
	payload, err := json.Marshal(&mitev1.ContentRequest{RequestId: reqID})
	if err != nil {
		return nil, err
	}
	reply, err := m.conn.RequestWithContext(ctx, bus.MiteContentSubject(m.cluster, ns, name), payload, 10*time.Second)
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
func (m *BusReadModel) PluginInventory(ns, name string) *mitev1.PluginInventory {
	data, err := m.snapshotKV.Get(bus.PluginInventoryKey(m.cluster, ns, name))
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
func (m *BusReadModel) InstalledPluginsHash(ns, name string) (string, bool) {
	data, err := m.presenceKV.Get(bus.PresenceKey(m.cluster, ns, name))
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
func (m *BusReadModel) PluginClassification(ns, name string) (*ClassifiedInventory, bool) {
	data, err := m.snapshotKV.Get(bus.PluginClassificationKey(m.cluster, ns, name))
	if err != nil || data == nil {
		return nil, false
	}
	var c ClassifiedInventory
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false
	}
	return &c, true
}

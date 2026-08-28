package mite

import (
	"fmt"
	"sync"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Connection tracks a connected mite agent and its current state.
type Connection struct {
	Conn          interface{}
	Stream        interface{}
	ConnectedAt   time.Time
	LastHeartbeat time.Time
	Done          chan struct{}
	Results       chan *mitev1.CommandResult
	Snapshot      *mitev1.StateSnapshot
	Version       string
	CertExpiry    time.Time
	Epoch         int64 // monotonically increasing connection epoch

	// ObservabilityReport is the latest observability report from this mite.
	// It is not affected by heartbeat or desired-state convergence.
	ObservabilityReport *mitev1.ObservabilityReport

	// IdleGauges is the latest idle gauge snapshot from the mite's heartbeat.
	// Set by UpdateHeartbeat when the heartbeat carries non-nil Idle field.
	IdleGauges *mitev1.IdleGauges

	// IdleGaugesReceivedAt is the timestamp when IdleGauges was last received.
	IdleGaugesReceivedAt time.Time

	// InstalledPluginsHash is the installed_plugins_hash from the mite's most
	// recent heartbeat. Set by UpdateHeartbeat.
	InstalledPluginsHash string

	// PluginInventory is the latest full plugin inventory from the mite.
	PluginInventory *mitev1.PluginInventory
}

// Registry tracks all connected mite agents. The map key format is
// "namespace/controllerName".
type Registry struct {
	mu          sync.RWMutex
	connections map[string]*Connection
	nextEpoch   int64 // incremented under mu on each Register call
}

// NewRegistry creates a new Registry.
func NewRegistry() *Registry {
	return &Registry{
		connections: make(map[string]*Connection),
	}
}

// Register adds a new mite connection to the registry and returns the
// created Connection. The Results channel is buffered with capacity 32.
func (r *Registry) Register(controllerName, namespace string, conn, stream interface{}, version string, certExpiry time.Time) *Connection {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey(controllerName, namespace)
	r.nextEpoch++
	epoch := r.nextEpoch

	var prevReport *mitev1.ObservabilityReport
	if existing, ok := r.connections[key]; ok {
		prevReport = existing.ObservabilityReport
	}

	mc := &Connection{
		Conn:          conn,
		Stream:        stream,
		ConnectedAt:   time.Now(),
		LastHeartbeat: time.Now(),
		Done:          make(chan struct{}),
		Results:       make(chan *mitev1.CommandResult, 32),
		Version:       version,
		CertExpiry:    certExpiry,
		Epoch:         epoch,
	}
	if prevReport != nil {
		mc.ObservabilityReport = prevReport
	}
	r.connections[key] = mc
	return mc
}

// Unregister removes a mite connection from the registry and closes its
// Done channel to signal goroutines watching for disconnection. The
// Connection record is retained with its ObservabilityReport so stale/fresh
// checks can continue after disconnect.
func (r *Registry) Unregister(controllerName, namespace string) {
	r.unregister(controllerName, namespace, 0)
}

// IsCurrentEpoch reports whether the registered entry still belongs to the
// connection identified by epoch. epoch 0 means the caller carries no
// connection identity and is always current; an absent entry is current too,
// since nothing has superseded it.
func (r *Registry) IsCurrentEpoch(controllerName, namespace string, epoch int64) bool {
	if epoch == 0 {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	mc, ok := r.connections[registryKey(controllerName, namespace)]
	return !ok || mc.Epoch == epoch
}

// UnregisterIfEpoch unregisters unless a newer connection already owns the
// entry, atomically. A stream that tears down after the mite reconnected must
// not clear its replacement's stream or close its Done channel; checking the
// epoch in a separate call would leave a window for exactly that. epoch 0
// means "unregister unconditionally".
//
// Returns false only when the entry belongs to a newer connection — an absent
// entry returns true, since there is nothing to protect and the caller should
// still run its normal teardown bookkeeping.
func (r *Registry) UnregisterIfEpoch(controllerName, namespace string, epoch int64) bool {
	return r.unregister(controllerName, namespace, epoch)
}

func (r *Registry) unregister(controllerName, namespace string, epoch int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok {
		return true // nothing registered; not a superseded stream
	}
	if epoch != 0 && mc.Epoch != epoch {
		return false
	}
	{
		if mc.Done != nil {
			select {
			case <-mc.Done:
			default:
				close(mc.Done)
			}
		}
		mc.Stream = nil
		mc.Conn = nil
	}
	return true
}

// Get retrieves a mite connection by controller name and namespace.
// The second return value is false if no such connection exists.
func (r *Registry) Get(controllerName, namespace string) (*Connection, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	return mc, ok
}

// List returns all registered connection keys in "namespace/name" format.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0, len(r.connections))
	for k := range r.connections {
		keys = append(keys, k)
	}
	return keys
}

// Send sends an OperatorMessage to the specified mite over its bidirectional
// command stream. It type-asserts the stored stream to the gRPC server stream
// interface.
func (r *Registry) Send(controllerName, namespace string, msg *mitev1.OperatorMessage) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok {
		return fmt.Errorf("mite %s not connected", key)
	}

	stream, ok := mc.Stream.(mitev1.Mite_CommandStreamServer)
	if !ok {
		return fmt.Errorf("mite %s: invalid stream type", key)
	}

	return stream.Send(msg)
}

// DrainResults drains all pending command results from the results channel
// without blocking. It returns all results that are immediately available.
func (r *Registry) DrainResults(controllerName, namespace string) []*mitev1.CommandResult {
	r.mu.RLock()
	mc, ok := r.connections[registryKey(controllerName, namespace)]
	r.mu.RUnlock()
	if !ok {
		return nil
	}

	var results []*mitev1.CommandResult
	for {
		select {
		case res := <-mc.Results:
			results = append(results, res)
		default:
			return results
		}
	}
}

// GetSnapshot returns the current state snapshot for a mite, or nil if
// the mite is not connected.
func (r *Registry) GetSnapshot(controllerName, namespace string) *mitev1.StateSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	if mc, ok := r.connections[key]; ok {
		return mc.Snapshot
	}
	return nil
}

// GetMiteInfo returns metadata for a connected mite. The last return value
// is false if no such mite is connected.
func (r *Registry) GetMiteInfo(controllerName, namespace string) (version string, lastHeartbeat, certExpiry time.Time, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	mc, exists := r.connections[key]
	if !exists {
		return "", time.Time{}, time.Time{}, false
	}
	return mc.Version, mc.LastHeartbeat, mc.CertExpiry, true
}

// GetIdleGauges returns the latest idle gauge snapshot from the mite's
// heartbeat, along with the timestamp it was received. The final return
// value is false if no gauges have been received yet.
func (r *Registry) GetIdleGauges(controllerName, namespace string) (*mitev1.IdleGauges, time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	mc, exists := r.connections[key]
	if !exists || mc.IdleGauges == nil {
		return nil, time.Time{}, false
	}
	return mc.IdleGauges, mc.IdleGaugesReceivedAt, true
}

// GetEpoch returns the connection epoch for a mite. The second return value
// is false if no such mite is connected (or ever was).
func (r *Registry) GetEpoch(controllerName, namespace string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok {
		return 0, false
	}
	return mc.Epoch, true
}

// IsConnected returns true if a mite is currently connected for the given controller.
func (r *Registry) IsConnected(controllerName, namespace string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mc, ok := r.connections[registryKey(controllerName, namespace)]
	return ok && mc.Stream != nil
}

// GetHealth returns the JenkinsHealth from the latest state snapshot, or
// "unreachable" if no mite is connected.
func (r *Registry) GetHealth(controllerName, namespace string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	mc, ok := r.connections[registryKey(controllerName, namespace)]
	if !ok || mc.Snapshot == nil {
		return "unreachable"
	}
	if mc.Snapshot.JenkinsHealth == "" {
		return "unknown"
	}
	return mc.Snapshot.JenkinsHealth
}

// UpdateHeartbeat updates the heartbeat timestamp, version, state
// snapshot, idle gauges, and installed plugins hash for a mite. If the
// mite is not found, this is a no-op.
func (r *Registry) UpdateHeartbeat(controllerName, namespace string, version string, snapshot *mitev1.StateSnapshot, idle *mitev1.IdleGauges, installedPluginsHash string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey(controllerName, namespace)
	if mc, ok := r.connections[key]; ok {
		mc.LastHeartbeat = time.Now()
		mc.Version = version
		if snapshot != nil {
			mc.Snapshot = snapshot
		}
		if idle != nil {
			mc.IdleGauges = idle
			mc.IdleGaugesReceivedAt = time.Now()
		}
		if installedPluginsHash != "" {
			mc.InstalledPluginsHash = installedPluginsHash
		}
	}
}

// registryKey returns the canonical map key for a controller.
func registryKey(controllerName, namespace string) string {
	return namespace + "/" + controllerName
}

// SetObservabilityReport stores the latest observability report for a
// controller. If no connection record exists, one is created so the
// report persists across mite disconnect for TTL/freshness checks.
func (r *Registry) SetObservabilityReport(controllerName, namespace string, report *mitev1.ObservabilityReport) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok {
		r.nextEpoch++
		mc = &Connection{
			ConnectedAt: time.Now(),
			Done:        make(chan struct{}),
			Epoch:       r.nextEpoch,
		}
		r.connections[key] = mc
	}
	mc.ObservabilityReport = report
}

// GetObservabilityReport returns the latest observability report for a
// controller. Returns nil if no report has been received.
func (r *Registry) GetObservabilityReport(controllerName, namespace string) *mitev1.ObservabilityReport {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok || mc.ObservabilityReport == nil {
		return nil
	}
	return mc.ObservabilityReport
}

// SetPluginInventory stores the latest plugin inventory for a controller.
func (r *Registry) SetPluginInventory(controllerName, namespace string, inv *mitev1.PluginInventory) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok {
		r.nextEpoch++
		mc = &Connection{
			ConnectedAt: time.Now(),
			Done:        make(chan struct{}),
			Epoch:       r.nextEpoch,
		}
		r.connections[key] = mc
	}
	mc.PluginInventory = inv
}

// GetPluginInventory returns the latest plugin inventory for a controller.
// Returns nil if no inventory has been received.
func (r *Registry) GetPluginInventory(controllerName, namespace string) *mitev1.PluginInventory {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok || mc.PluginInventory == nil {
		return nil
	}
	return mc.PluginInventory
}

// GetInstalledPluginsHash returns the installed_plugins_hash from the most
// recent heartbeat. Returns ("", false) if not available.
func (r *Registry) GetInstalledPluginsHash(controllerName, namespace string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := registryKey(controllerName, namespace)
	mc, ok := r.connections[key]
	if !ok || mc.InstalledPluginsHash == "" {
		return "", false
	}
	return mc.InstalledPluginsHash, true
}

package bus

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// ClusterHeartbeatInterval / ClusterEntryTTL are the frozen cadence values
// (coordinator registry §4.2): heartbeat 30s, TTL 90s.
const (
	ClusterHeartbeatInterval = 30 * time.Second
	ClusterEntryTTL          = 90 * time.Second
)

// Cluster lifecycle state constants.
const (
	ClusterStateActive   = "active"
	ClusterStateDraining = "draining"
	ClusterStateDrained  = "drained"
)

// ClusterInfo is the JSON value stored in KVClustersBucket (schema frozen
// by coordinator registry §4.2).
type ClusterInfo struct {
	Name            string     `json:"name"`
	OperatorVersion string     `json:"operatorVersion"`
	K8sVersion      string     `json:"k8sVersion"`
	ControllerCount int        `json:"controllerCount"`
	ConnectedCount  int        `json:"connectedCount"`
	LastHeartbeat   time.Time  `json:"lastHeartbeat"`            // RFC3339 via encoding/json
	State           string     `json:"state"`                    // "active" | "draining" | "drained"
	DrainStartedAt  *time.Time `json:"drainStartedAt,omitempty"` // set iff State != "active"
}

// PutClusterHeartbeat marshals info and stores it under key info.Name.
func PutClusterHeartbeat(kv *KV, info ClusterInfo) error {
	if info.Name == "" {
		return fmt.Errorf("cluster heartbeat: name is required")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("cluster heartbeat marshal: %w", err)
	}
	return kv.Put(info.Name, data)
}

// ListClusters returns all live cluster entries (TTL has already evicted
// dead ones). Unparseable entries are skipped, not fatal. Order: sorted by Name.
func ListClusters(kv *KV) ([]ClusterInfo, error) {
	keys, err := kv.Keys()
	if err != nil {
		return nil, fmt.Errorf("list clusters keys: %w", err)
	}
	var result []ClusterInfo
	for _, key := range keys {
		data, err := kv.Get(key)
		if err != nil || data == nil {
			// TTL race: key vanished between Keys and Get
			continue
		}
		var info ClusterInfo
		if err := json.Unmarshal(data, &info); err != nil {
			// Unparseable entry — skip, not fatal
			continue
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, nil
}

// GetCluster returns the entry for one cluster, or (nil, nil) when absent.
func GetCluster(kv *KV, name string) (*ClusterInfo, error) {
	data, err := kv.Get(name)
	if err != nil {
		return nil, fmt.Errorf("get cluster %s: %w", name, err)
	}
	if data == nil {
		return nil, nil
	}
	var info ClusterInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("unmarshal cluster %s: %w", name, err)
	}
	return &info, nil
}

// ClusterDirectory is the BFF-side read seam handed to internal/api
// (consumed by change C6). It mirrors the bucket into memory via a KV
// watcher so the frequent List/Get calls don't fan out `Keys()` + one `Get`
// per key to the KV on every read (issue #280). Bucket MaxAge expiry emits
// no watch event, so reads filter entries whose heartbeat is older than
// ClusterEntryTTL. When the watch cannot be (re)established, reads fall
// back to direct KV access.
type ClusterDirectory struct {
	kv *KV

	mu       sync.Mutex
	watching bool
	entries  map[string]ClusterInfo
	clock    func() time.Time // test seam
}

// NewClusterDirectory creates a new ClusterDirectory.
func NewClusterDirectory(kv *KV) *ClusterDirectory {
	return &ClusterDirectory{kv: kv, clock: time.Now}
}

// ensureWatchLocked starts the KV watcher and seeds the mirror from its
// initial replay. Returns false when the watch could not be established
// (callers fall back to direct KV reads). Caller holds d.mu.
func (d *ClusterDirectory) ensureWatchLocked() bool {
	if d.watching {
		return true
	}
	w, err := d.kv.WatchAll()
	if err != nil {
		return false
	}
	// The watcher replays current bucket state first, then sends a nil
	// marker; seed the mirror synchronously so the first read is complete.
	// If the channel closes before the marker (connection drop mid-replay),
	// the seed is partial — discard it and fall back to direct reads.
	entries := make(map[string]ClusterInfo)
	seeded := false
	for e := range w.Updates() {
		if e == nil {
			seeded = true
			break
		}
		applyClusterEntry(entries, e)
	}
	if !seeded {
		_ = w.Stop()
		return false
	}
	d.entries = entries
	d.watching = true
	go d.consume(w)
	return true
}

// consume applies live watch updates to the mirror until the watcher's
// channel closes (connection loss / shutdown), then drops the mirror so the
// next read re-establishes the watch or falls back to direct KV access.
func (d *ClusterDirectory) consume(w nats.KeyWatcher) {
	for e := range w.Updates() {
		if e == nil {
			continue
		}
		d.mu.Lock()
		applyClusterEntry(d.entries, e)
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.watching = false
	d.entries = nil
	d.mu.Unlock()
}

// applyClusterEntry folds one watch event into the mirror. Unparseable
// values are skipped, matching ListClusters.
func applyClusterEntry(entries map[string]ClusterInfo, e nats.KeyValueEntry) {
	switch e.Operation() {
	case nats.KeyValueDelete, nats.KeyValuePurge:
		delete(entries, e.Key())
	default:
		var info ClusterInfo
		if err := json.Unmarshal(e.Value(), &info); err != nil {
			return
		}
		entries[e.Key()] = info
	}
}

// fresh reports whether an entry would still exist in the TTL'd bucket.
func (d *ClusterDirectory) fresh(info ClusterInfo, now time.Time) bool {
	return now.Sub(info.LastHeartbeat) <= ClusterEntryTTL
}

// List returns all live cluster entries, sorted by name.
func (d *ClusterDirectory) List() ([]ClusterInfo, error) {
	d.mu.Lock()
	if !d.ensureWatchLocked() {
		d.mu.Unlock()
		return ListClusters(d.kv)
	}
	now := d.clock()
	result := make([]ClusterInfo, 0, len(d.entries))
	for key, info := range d.entries {
		if !d.fresh(info, now) {
			// MaxAge expiry emits no watch event; prune lazily.
			delete(d.entries, key)
			continue
		}
		result = append(result, info)
	}
	d.mu.Unlock()
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, nil
}

// Get returns the entry for one cluster, or nil when absent.
func (d *ClusterDirectory) Get(name string) (*ClusterInfo, error) {
	d.mu.Lock()
	if !d.ensureWatchLocked() {
		d.mu.Unlock()
		return GetCluster(d.kv, name)
	}
	info, ok := d.entries[name]
	if ok && !d.fresh(info, d.clock()) {
		delete(d.entries, name)
		ok = false
	}
	d.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return &info, nil
}

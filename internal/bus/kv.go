package bus

import (
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// ErrKVKeyExists is returned by Create when the key is already present.
var ErrKVKeyExists = errors.New("bus: kv key exists")

// KV is a thin wrapper around nats.KeyValue for storing mite snapshots
// and presence entries.
type KV struct {
	bucket nats.KeyValue
}

// keyValueConfig builds the nats.KeyValueConfig for a KV bucket. It is a
// standalone builder so the replica wiring can be unit-tested without a live
// NATS server. replicas < 1 is clamped to 1.
func keyValueConfig(bucket string, ttl time.Duration, replicas int) *nats.KeyValueConfig {
	return &nats.KeyValueConfig{
		Bucket:   bucket,
		TTL:      ttl,
		Storage:  nats.FileStorage,
		Replicas: clampReplicas(replicas),
	}
}

// EnsureKV creates or retrieves a KV bucket with the given name.
// TTL sets the default entry lifetime; 0 means no expiry. New buckets are
// created with the connection's JetStream replica count (Conn.Replicas(),
// sourced from VARROA_JETSTREAM_REPLICAS) so soft state survives a single
// NATS pod loss. NOTE: an already-existing bucket keeps its stored replica
// count — CreateKeyValue does not update replicas, and the lookup fallback
// below just adopts the existing bucket. To raise an existing bucket R1->R3,
// delete and recreate it (soft state repopulates within ~30-90s).
func (c *Conn) EnsureKV(bucket string, ttl time.Duration) (*KV, error) {
	kv, err := c.js.CreateKeyValue(keyValueConfig(bucket, ttl, c.Replicas()))
	if err != nil {
		// Bucket may already exist — try to look it up.
		kv, err = c.js.KeyValue(bucket)
		if err != nil {
			return nil, fmt.Errorf("ensure kv %s: %w", bucket, err)
		}
	}
	return &KV{bucket: kv}, nil
}

// KeyValue returns the raw nats.KeyValue for advanced operations.
func (kv *KV) KeyValue() nats.KeyValue { return kv.bucket }

// Get retrieves the value for a key. Returns nil if the key does not exist.
func (kv *KV) Get(key string) ([]byte, error) {
	entry, err := kv.bucket.Get(key)
	if err != nil {
		if err == nats.ErrKeyNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("kv get %s: %w", key, err)
	}
	return entry.Value(), nil
}

// Put stores a value under the given key.
func (kv *KV) Put(key string, value []byte) error {
	_, err := kv.bucket.Put(key, value)
	if err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// Create atomically stores a value under the given key only if the key does
// not already exist. Returns ErrKVKeyExists if it does.
func (kv *KV) Create(key string, value []byte) error {
	_, err := kv.bucket.Create(key, value)
	if errors.Is(err, nats.ErrKeyExists) {
		return ErrKVKeyExists
	}
	if err != nil {
		return fmt.Errorf("kv create %s: %w", key, err)
	}
	return nil
}

// PutString stores a string value under the given key.
func (kv *KV) PutString(key, value string) error {
	return kv.Put(key, []byte(value))
}

// Delete removes a key from the bucket.
func (kv *KV) Delete(key string) error {
	return kv.bucket.Delete(key)
}

// Keys returns all keys in the bucket. An empty bucket returns an empty
// slice, not an error.
func (kv *KV) Keys() ([]string, error) {
	keys, err := kv.bucket.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	return keys, nil
}

// Watch returns a watcher for the given key. When the key is updated or
// deleted, an entry (possibly nil) is sent on the returned channel.
func (kv *KV) Watch(key string) (nats.KeyWatcher, error) {
	return kv.bucket.Watch(key)
}

// WatchAll returns a watcher that receives all updates to the bucket.
func (kv *KV) WatchAll() (nats.KeyWatcher, error) {
	return kv.bucket.WatchAll()
}

// SnapshotKey returns the KV key for a mite snapshot: "<cluster>/<ns>/<name>".
func SnapshotKey(cluster, ns, name string) string {
	return cluster + "/" + ns + "/" + name
}

// ObservabilityKey returns the KV key for a mite observability report:
// "obs/<cluster>/<ns>/<name>". Uses the snapshot KV bucket with a prefix.
func ObservabilityKey(cluster, ns, name string) string {
	return "obs/" + cluster + "/" + ns + "/" + name
}

// PresenceKey returns the KV key for a mite's presence record: "<cluster>/<ns>/<name>".
func PresenceKey(cluster, ns, name string) string {
	return cluster + "/" + ns + "/" + name
}

// DesiredKey returns the KV key for a mite's desired state: "<cluster>/<ns>/<name>".
func DesiredKey(cluster, ns, name string) string { return cluster + "/" + ns + "/" + name }

// PluginInventoryKey returns the KV key for a mite's plugin inventory:
// "inv/<cluster>/<ns>/<name>". Uses the snapshot KV bucket with a prefix.
func PluginInventoryKey(cluster, ns, name string) string {
	return "inv/" + cluster + "/" + ns + "/" + name
}

// PluginClassificationKey returns the KV key for a controller's classified
// plugin inventory: "invc/<cluster>/<ns>/<name>". Uses the snapshot KV bucket.
func PluginClassificationKey(cluster, ns, name string) string {
	return "invc/" + cluster + "/" + ns + "/" + name
}

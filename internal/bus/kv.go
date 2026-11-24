package bus

import (
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// ErrKVKeyExists is returned by Create when the key is already present.
var ErrKVKeyExists = errors.New("bus: kv key exists")

// ErrKVRevisionMismatch is returned by Update and DeleteRevision when another
// write changed the key after the given revision. An expired or deleted key
// gives it too.
var ErrKVRevisionMismatch = errors.New("bus: kv revision mismatch")

// leaderReadWait bounds one GetFromLeader request to the stream leader.
const leaderReadWait = 2 * time.Second

// Header names and values that nats.go writes on KV delete and purge
// markers. nats.go does not export them.
const (
	kvOperationHeader = "KV-Operation"
	kvOperationDelete = "DEL"
	kvOperationPurge  = "PURGE"
)

// KV is a thin wrapper around nats.KeyValue for storing mite snapshots
// and presence entries.
type KV struct {
	bucket nats.KeyValue

	// js and stream serve GetFromLeader. subjectPrefix is the KV subject
	// prefix that maps a key to its stream subject ("$KV.<bucket>.").
	js            nats.JetStreamContext
	stream        string
	subjectPrefix string
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
	return &KV{
		bucket:        kv,
		js:            c.js,
		stream:        "KV_" + bucket,
		subjectPrefix: "$KV." + bucket + ".",
	}, nil
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

// GetFromLeader reads the latest value of a key from the stream leader.
// Use it where an old copy changes a decision. Every replica answers a
// direct get from its own copy, so a replica cut off from its leader can
// return a stale value. A missing or deleted key gives nil and a nil
// error. A stream with no leader gives an error. The read waits at most
// leaderReadWait. Do not fall back to Get on an error.
func (kv *KV) GetFromLeader(key string) ([]byte, error) {
	msg, err := kv.js.GetLastMsg(kv.stream, kv.subjectPrefix+key, nats.MaxWait(leaderReadWait))
	if errors.Is(err, nats.ErrMsgNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kv leader get %s: %w", key, err)
	}
	switch msg.Header.Get(kvOperationHeader) {
	case kvOperationDelete, kvOperationPurge:
		return nil, nil
	}
	return msg.Data, nil
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
	_, err := kv.CreateRevision(key, value)
	return err
}

// CreateRevision stores value under key only when the key is missing or
// deleted, and returns the revision of the write. It returns ErrKVKeyExists
// when the key holds a value.
func (kv *KV) CreateRevision(key string, value []byte) (uint64, error) {
	rev, err := kv.bucket.Create(key, value)
	if errors.Is(err, nats.ErrKeyExists) {
		return 0, ErrKVKeyExists
	}
	if err != nil {
		return 0, fmt.Errorf("kv create %s: %w", key, err)
	}
	return rev, nil
}

// PutRevision stores value under key and returns the revision of the write.
func (kv *KV) PutRevision(key string, value []byte) (uint64, error) {
	rev, err := kv.bucket.Put(key, value)
	if err != nil {
		return 0, fmt.Errorf("kv put %s: %w", key, err)
	}
	return rev, nil
}

// Entry returns the latest value of key and its revision. A missing or
// deleted key gives a nil value, revision zero, and a nil error.
func (kv *KV) Entry(key string) ([]byte, uint64, error) {
	e, err := kv.bucket.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("kv get %s: %w", key, err)
	}
	return e.Value(), e.Revision(), nil
}

// Update stores value under key only when last is the revision of the
// latest write of the key, and returns the revision of the write. It returns
// ErrKVRevisionMismatch when another write came first.
func (kv *KV) Update(key string, value []byte, last uint64) (uint64, error) {
	rev, err := kv.bucket.Update(key, value, last)
	if errors.Is(err, nats.ErrKeyExists) {
		return 0, ErrKVRevisionMismatch
	}
	if err != nil {
		return 0, fmt.Errorf("kv update %s: %w", key, err)
	}
	return rev, nil
}

// DeleteRevision deletes key only when last is the revision of the latest
// write of the key. It returns ErrKVRevisionMismatch when another write came
// first.
func (kv *KV) DeleteRevision(key string, last uint64) error {
	err := kv.bucket.Delete(key, nats.LastRevision(last))
	if errors.Is(err, nats.ErrKeyExists) {
		return ErrKVRevisionMismatch
	}
	if err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
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

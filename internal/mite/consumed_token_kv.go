package mite

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// kvCreator is the narrow slice of *bus.KV the layered store needs; it exists
// so unit tests can fake the bucket without an embedded NATS server.
type kvCreator interface {
	Create(key string, value []byte) error
}

// kvConsumedStore layers cross-replica single-use enforcement (a JetStream KV
// bucket written with atomic create-only semantics) over the per-replica
// in-memory store. The in-memory store is consulted first so same-replica
// replays are rejected without a bus round-trip; a KV error degrades to that
// per-replica guarantee (fail-open) rather than blocking registration.
type kvConsumedStore struct {
	local ConsumedTokenStore
	kv    kvCreator
	log   *slog.Logger
}

// NewKVConsumedStore returns a ConsumedTokenStore backed by the shared
// consumed-tokens KV bucket. A nil logger falls back to slog.Default().
func NewKVConsumedStore(kv *bus.KV, log *slog.Logger) ConsumedTokenStore {
	return newKVConsumedStore(kv, log)
}

func newKVConsumedStore(kv kvCreator, log *slog.Logger) *kvConsumedStore {
	if log == nil {
		log = slog.Default()
	}
	return &kvConsumedStore{local: newInMemoryConsumedStore(), kv: kv, log: log}
}

// Consume records jti and reports whether this is its first use. The expiry
// argument drives in-memory eviction only; the shared bucket evicts entries
// via its bucket-wide TTL (>= the bootstrap-token TTL).
func (s *kvConsumedStore) Consume(jti string, expiry time.Time) bool {
	if !s.local.Consume(jti, expiry) {
		return false
	}
	value := fmt.Sprintf("{\"ts\":%q}", time.Now().UTC().Format(time.RFC3339))
	switch err := s.kv.Create(jti, []byte(value)); {
	case err == nil:
		return true
	case errors.Is(err, bus.ErrKVKeyExists):
		// Consumed by another replica (or a prior life of this one).
		return false
	default:
		s.log.Warn("consumed-token KV unavailable; enforcing single-use per-replica only", "err", err)
		return true
	}
}

// stop forwards to the in-memory store's sweeper shutdown. It is called
// exactly once by Server.SetConsumedTokenStore when a store is replaced
// (a second call would double-close the sweeper's done channel).
func (s *kvConsumedStore) stop() {
	if st, ok := s.local.(interface{ stop() }); ok {
		st.stop()
	}
}

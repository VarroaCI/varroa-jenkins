package bus

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// NOTE: As of the last-value desired-state refactor, the command stream and
// these consumer helpers are NOT used by the operator→mite path (that moved to
// the mite_desired KV bucket, see internal/bus/subjects.go). They are retained
// for future IMPERATIVE one-shot commands (e.g. "restart now"), which are
// edge-triggered and must use a durable subject rather than the last-value KV.

// replicaArg resolves the effective replica count from an optional variadic
// argument. When omitted it defaults to 1; any value < 1 is clamped to 1.
// The variadic form (rather than a mandatory positional param) lets callers
// that don't care about replication — chiefly tests — keep their existing
// call sites, while production callers pass Conn.Replicas().
func replicaArg(replicas []int) int {
	if len(replicas) == 0 {
		return 1
	}
	return clampReplicas(replicas[0])
}

// StreamConfig returns a default JetStream stream configuration for the
// varroa command/result stream. The stream captures all mite.*.*.*.out subjects
// (operator→mite commands) for reliable, ordered delivery. The optional
// replicas argument sets the JetStream replica count (default 1); pass the NATS
// cluster size (Conn.Replicas()) so the stream survives a single NATS pod loss.
func StreamConfig(name string, replicas ...int) *nats.StreamConfig {
	return &nats.StreamConfig{
		Name:      name,
		Subjects:  []string{"mite.*.*.*.out"},
		Retention: nats.WorkQueuePolicy,
		MaxAge:    24 * time.Hour,
		Storage:   nats.FileStorage,
		Replicas:  replicaArg(replicas),
	}
}

// EnsureStream creates the stream if it does not already exist, or updates
// an existing stream's subjects if they differ.
func (c *Conn) EnsureStream(cfg *nats.StreamConfig) error {
	_, err := c.js.AddStream(cfg)
	if err == nil {
		return nil
	}
	// If the stream already exists, try updating it.
	_, err = c.js.UpdateStream(cfg)
	if err != nil {
		return fmt.Errorf("ensure stream %s: %w", cfg.Name, err)
	}
	return nil
}

// EnsureConsumer creates or updates a durable consumer on the given stream.
func (c *Conn) EnsureConsumer(stream, name string, cfg *nats.ConsumerConfig) error {
	// UpdateConsumer is idempotent — it creates or updates.
	_, err := c.js.UpdateConsumer(stream, cfg)
	if err != nil {
		// Fall back to AddConsumer if the stream was just created.
		_, err = c.js.AddConsumer(stream, cfg)
		if err != nil {
			return fmt.Errorf("ensure consumer %s/%s: %w", stream, name, err)
		}
	}
	return nil
}

// PullSubscribe creates a pull subscription on the given stream+consumer.
// subject should match the consumer's FilterSubject (or ">" for all).
func (c *Conn) PullSubscribe(subject, stream, consumer string) (*nats.Subscription, error) {
	sub, err := c.js.PullSubscribe(subject, consumer, nats.Bind(stream, consumer))
	if err != nil {
		return nil, fmt.Errorf("pull subscribe %s/%s: %w", stream, consumer, err)
	}
	return sub, nil
}

// PublishJetStream publishes data to a JetStream subject and waits for the
// server ack.
func (c *Conn) PublishJetStream(subject string, data []byte) (*nats.PubAck, error) {
	return c.js.Publish(subject, data)
}

// PublishJetStreamAsync publishes data to a JetStream subject without waiting
// for the server ack. Returns the PubAckFuture.
func (c *Conn) PublishJetStreamAsync(subject string, data []byte) (nats.PubAckFuture, error) {
	return c.js.PublishAsync(subject, data)
}

// DrainConsumer drains messages from a pull consumer, invoking handler for
// each message. It stops when the batch is empty or ctx is done.
func (c *Conn) DrainConsumer(sub *nats.Subscription, batchSize int) ([]*nats.Msg, error) {
	msgs, err := sub.Fetch(batchSize, nats.MaxWait(100*time.Millisecond))
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	return msgs, nil
}

// DeleteConsumer deletes a durable consumer from a stream.
func (c *Conn) DeleteConsumer(stream, name string) error {
	return c.js.DeleteConsumer(stream, name)
}

// ActivityStreamConfig returns a JetStream stream config for the bounded
// activity event stream. It captures activity.> with LimitsPolicy so that
// multiple independent readers can each read the full retained history.
// maxAge comes from the retention dial; maxMsgs and maxBytes provide hard
// caps (DiscardOld when any cap is hit). The optional replicas argument sets
// the JetStream replica count (default 1); pass the NATS cluster size
// (Conn.Replicas()) so retained activity history survives a single NATS pod loss.
func ActivityStreamConfig(name string, maxAge time.Duration, maxMsgs, maxBytes int64, replicas ...int) *nats.StreamConfig {
	return &nats.StreamConfig{
		Name:      name,
		Subjects:  []string{ActivityWildcard},
		Retention: nats.LimitsPolicy,
		MaxAge:    maxAge,
		MaxMsgs:   maxMsgs,
		MaxBytes:  maxBytes,
		Discard:   nats.DiscardOld,
		Storage:   nats.FileStorage,
		Replicas:  replicaArg(replicas),
	}
}

// WebhookStreamConfig returns a JetStream stream config for the bounded
// webhook replay stream. It captures webhook.> with LimitsPolicy so that
// multiple independent consumers can each read independently.
// MaxAge=1h, MaxMsgsPerSubject=1000, MaxBytes=64MiB, Discard=Old.
// replicas matches the caller-supplied value (same as activity stream).
func WebhookStreamConfig(name string, replicas int) *nats.StreamConfig {
	return &nats.StreamConfig{
		Name:              name,
		Subjects:          []string{WebhookWildcard},
		Retention:         nats.LimitsPolicy,
		MaxAge:            1 * time.Hour,
		MaxMsgsPerSubject: 1000,
		MaxBytes:          64 * 1024 * 1024, // 64 MiB
		Discard:           nats.DiscardOld,
		Storage:           nats.FileStorage,
		Replicas:          clampReplicas(replicas),
	}
}

// ReplayConsumerConfig returns a durable pull consumer config for the
// varroa-webhook-replay consumer. Filter subject webhook.<cluster>.>,
// AckWait=60s, MaxDeliver=-1 (unbounded; MaxAge is the real bound).
func ReplayConsumerConfig(cluster string) *nats.ConsumerConfig {
	filterSubject := fmt.Sprintf("webhook.%s.>", cluster)
	return &nats.ConsumerConfig{
		Durable:       "varroa-webhook-replay",
		FilterSubject: filterSubject,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       60 * time.Second,
		MaxDeliver:    -1,
	}
}

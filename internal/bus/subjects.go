// Package bus provides a thin wrapper around NATS for the Varroa control bus.
// It defines the subject scheme, connection management, JetStream helpers,
// and a simple KV store abstraction for mite state (snapshots, presence).
//
// Subject scheme — single source of truth.
//
// Mite subjects (5 tokens):   mite.<cluster>.<ns>.<name>.{in,out,content}
// Brood subjects (3 tokens):  events.brood.<cluster>
// Activity subjects:          activity.<cluster>.<ns>.<ctrl> / activity.<cluster>._global
// Webhook subjects:           webhook.<cluster>.<ns>.<ctrl> (JetStream)
// Wake subjects:              wake.<cluster>.<ns>.<ctrl> (core NATS, at-most-once)
// Operator subjects:          operator.<cluster>.<action>
// KV keys:                    <cluster>/<ns>/<name>
package bus

import (
	"fmt"
	"strings"
)

// Subject scheme — single source of truth.
const (
	// ActivityWildcard is the wildcard subject capturing all activity events
	// for stream subscription, backfill, and live fanout.
	ActivityWildcard = "activity.>"

	// WebhookWildcard is the wildcard subject capturing all webhook replay
	// payloads for the varroa_webhooks JetStream stream.
	WebhookWildcard = "webhook.>"

	// BroodWildcard is the wildcard subject capturing all brood events
	// for subscription.
	BroodWildcard = "events.brood.>"

	// ActivityStreamName is the JetStream stream name for the activity feed.
	ActivityStreamName = "varroa_activity"

	// WebhookStreamName is the JetStream stream name for the webhook replay
	// payloads. Subjects webhook.>, limits retention, MaxAge=1h,
	// MaxMsgsPerSubject=1000, MaxBytes=64MiB, Discard=Old.
	WebhookStreamName = "varroa_webhooks"

	// KVSnapshotBucket is the KV bucket for last-known mite snapshots.
	KVSnapshotBucket = "mite_snapshots"

	// KVPresenceBucket is the KV bucket for mite presence (gateway holding
	// the stream). Entries are keyed <cluster>/<ns>/<name> with a short TTL.
	KVPresenceBucket = "mite_presence"

	// KVDesiredBucket holds the latest desired-state command per mite, keyed
	// <cluster>/<ns>/<name>. Last-value semantics: Put overwrites; Watch delivers the
	// current value on attach plus updates. This is the operator→mite command
	// channel (level-triggered desired state). Imperative one-shot commands, if
	// ever added, must NOT use this bucket — see internal/bus/jetstream.go.
	KVDesiredBucket = "mite_desired"

	// KVConsumedTokensBucket stores consumed bootstrap-token jtis (keyed by
	// jti) for cross-replica single-use enforcement. Written create-only by
	// the gateway; entries evict via the bucket TTL.
	KVConsumedTokensBucket = "varroa_consumed_tokens"

	// KVClustersBucket holds one entry per member cluster, keyed <cluster>
	// (DNS-1123 label), written by that cluster's operator leader every 30s.
	// The bucket TTL (90s) makes entry presence equivalent to cluster health:
	// a dead operator's entry expires after three missed heartbeats.
	KVClustersBucket = "varroa_clusters"

	// DefaultStreamName is the JetStream stream name for ordered command/reply.
	DefaultStreamName = "varroa"
)

// MiteInSubject returns the subject for mite→operator messages.
// Format: mite.<cluster>.<ns>.<name>.in
func MiteInSubject(cluster, ns, name string) string {
	return fmt.Sprintf("mite.%s.%s.%s.in", cluster, ns, name)
}

// MiteOutSubject returns the subject for operator→mite messages.
// Format: mite.<cluster>.<ns>.<name>.out
func MiteOutSubject(cluster, ns, name string) string {
	return fmt.Sprintf("mite.%s.%s.%s.out", cluster, ns, name)
}

// MiteContentSubject returns the subject for on-demand content-fetch
// request/reply (core NATS, not JetStream).
// Format: mite.<cluster>.<ns>.<name>.content
func MiteContentSubject(cluster, ns, name string) string {
	return fmt.Sprintf("mite.%s.%s.%s.content", cluster, ns, name)
}

// BroodSubject returns the subject for brood-wide SSE events.
// Format: events.brood.<cluster>
func BroodSubject(cluster string) string {
	return fmt.Sprintf("events.brood.%s", cluster)
}

// ActivitySubject returns the hierarchical per-controller activity subject:
// activity.<cluster>.<namespace>.<controller>.
func ActivitySubject(cluster, ns, name string) string {
	return fmt.Sprintf("activity.%s.%s.%s", cluster, ns, name)
}

// ActivityGlobal returns the reserved global activity subject for Varroa/
// control-plane events with no controller association.
// Format: activity.<cluster>._global
func ActivityGlobal(cluster string) string {
	return fmt.Sprintf("activity.%s._global", cluster)
}

// WebhookSubject returns the subject for webhook replay payloads targeting
// a specific controller.
// Format: webhook.<cluster>.<ns>.<ctrl>
func WebhookSubject(cluster, ns, ctrl string) string {
	return fmt.Sprintf("webhook.%s.%s.%s", cluster, ns, ctrl)
}

// WebhookClusterWildcard returns the subject an operator leader filters its
// replay consumer on so it drains only its own cluster's queued webhooks — the
// varroa_webhooks stream is shared across clusters, so the bare webhook.>
// wildcard would let one cluster replay another's webhooks.
// Format: webhook.<cluster>.>
func WebhookClusterWildcard(cluster string) string {
	return fmt.Sprintf("webhook.%s.>", cluster)
}

// WakeSubject returns the core NATS subject for waking a hibernated
// controller. Published at-most-once by the BFF; subscribed by each
// cluster's operator leader.
// Format: wake.<cluster>.<ns>.<ctrl>
func WakeSubject(cluster, ns, ctrl string) string {
	return fmt.Sprintf("wake.%s.%s.%s", cluster, ns, ctrl)
}

// WebhookResultSubject returns the dedicated core-NATS subject the gateway
// publishes a REPLAY_WEBHOOK command result on. It is kept off the shared mite
// inbound subject so the operator's webhook-replay consumer can correlate
// replay results without contending for the reconciler's command-result buffer.
// The "whreply." prefix is deliberately disjoint from the varroa_webhooks
// stream filter ("webhook.>") so results are never captured as payloads.
// Format: whreply.<cluster>.<ns>.<ctrl>
func WebhookResultSubject(cluster, ns, ctrl string) string {
	return fmt.Sprintf("whreply.%s.%s.%s", cluster, ns, ctrl)
}

// WebhookResultWildcard returns the subscription subject the operator leader
// uses to receive every controller's replay result in its cluster.
// Format: whreply.<cluster>.>
func WebhookResultWildcard(cluster string) string {
	return fmt.Sprintf("whreply.%s.>", cluster)
}

// WakeSubjectWildcard returns the subscription subject an operator leader uses
// to receive wake commands for every controller in its cluster.
// Format: wake.<cluster>.>
func WakeSubjectWildcard(cluster string) string {
	return fmt.Sprintf("wake.%s.>", cluster)
}

// ParseWakeSubject extracts the namespace and controller name from a wake
// subject of the form wake.<cluster>.<ns>.<ctrl>. Kubernetes object names and
// namespaces are DNS-1123 labels (no dots), and cluster names are likewise
// dot-free, so the subject always has exactly four tokens. ok is false for any
// other shape.
func ParseWakeSubject(subject string) (ns, ctrl string, ok bool) {
	parts := strings.Split(subject, ".")
	if len(parts) != 4 || parts[0] != "wake" {
		return "", "", false
	}
	return parts[2], parts[3], true
}

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
// Ledger claim subject:       varroa.ledger.claim (core NATS, request-reply)
// Ledger owner subject:       varroa.ledger.owner (core NATS, request-reply)
// Ledger cross-tenant subject: varroa.ledger.crosstenant (core NATS, request-reply)
// Ledger action subjects:     varroa.ledger.actions.<cluster> (core NATS, at-most-once)
// Build subjects:             builds.<cluster>.<ns>.<cell> (JetStream)
// Run subjects:                runs.<cluster>.<tenant> (core NATS, at-most-once)
// Log subjects:                logs.<cluster>.<ns>.<cell>.<runid> (core NATS, gateway to BFF SSE)
// Tail subjects:               varroa.tail.<cluster>.<ns>.<cell> (core NATS, request-reply)
// Tenant purge subject:        varroa.recordstore.tenant.purge (core NATS, request-reply)
// Tenant sync subject:          varroa.recordstore.tenant.sync (core NATS, request-reply)
// Fleet lock release subject:  varroa.recordstore.fleetlock.release (core NATS, request-reply)
// Run note post subject:       varroa.recordstore.notes.post (core NATS, request-reply)
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

	// LedgerClaimSubject is the core-NATS request-reply subject the
	// varroa-recorder answers a build claim on. ClaimRequest carries the
	// cluster field, so the subject needs no cluster token.
	LedgerClaimSubject = "varroa.ledger.claim"

	// LedgerOwnerSubject is the core-NATS request-reply subject a gateway
	// asks for a run's current owner on, to authorize a console-log chunk
	// before it writes one byte. The gateway is the requester and the
	// recorder is the responder. The request body carries the run id, so
	// the subject needs no cluster token.
	LedgerOwnerSubject = "varroa.ledger.owner"

	// LedgerCrossTenantSubject is the core-NATS request-reply subject the
	// varroa-recorder answers a cross-tenant pipeline call on: a build
	// trigger, a status poll, an artifact read, or a fleet lock
	// operation (design.md D4). The gateway is the requester and the
	// recorder is the responder, queue group "recorder". The request
	// body carries the cluster field, so the subject needs no cluster
	// token.
	LedgerCrossTenantSubject = "varroa.ledger.crosstenant"

	// TenantPurgeSubject is the core-NATS request-reply subject the
	// operator asks the recorder to purge a tenant's job_schedules rows
	// on, whenever the tenant leaves a cell (a move, a teardown, or the
	// deletion of the owning cell). The operator is the requester and
	// the recorder is the responder. The request body carries the
	// cluster and the tenant, so the subject needs no cluster token.
	TenantPurgeSubject = "varroa.recordstore.tenant.purge"

	// TenantSyncSubject is the core-NATS request-reply subject the
	// operator announces a tenant's placement on, and asks the recorder
	// for a tenant's build records on. Two request kinds, "announce" and
	// "records", share the subject. The operator is the requester and
	// the recorder is the responder. The request body carries the
	// cluster and the tenant, so the subject needs no cluster token.
	TenantSyncSubject = "varroa.recordstore.tenant.sync"

	// FleetLockReleaseSubject is the core-NATS request-reply subject the
	// BFF asks the recorder to force-release a releasing fleet lock on.
	// The BFF is the requester and the recorder is the responder. The
	// request body names the lock and its run, so the subject needs no
	// cluster token.
	FleetLockReleaseSubject = "varroa.recordstore.fleetlock.release"

	// RunNotePostSubject is the core-NATS request-reply subject the BFF
	// asks the recorder to write a run note on. The BFF is the requester
	// and the recorder is the responder. The body names the run or the
	// job, so the subject needs no cluster token.
	RunNotePostSubject = "varroa.recordstore.notes.post"

	// WebhookEnqueueSubject is the core-NATS request-reply subject that the
	// BFF or the agent runner asks the recorder to enqueue one Tenant agent
	// message on a tenant webhook. The recorder is the responder, served by
	// queue group recorder. The body names the tenant, so the subject needs
	// no cluster token.
	WebhookEnqueueSubject = "varroa.recordstore.webhook.enqueue"

	// BuildsStreamName is the JetStream stream name for the build-event
	// work queue.
	BuildsStreamName = "varroa_builds"

	// BuildsWildcard is the wildcard subject capturing every build event
	// for the varroa_builds stream.
	BuildsWildcard = "builds.>"

	// RunNotifyWildcard is the wildcard subject capturing every live run
	// notification for the BFF's per-tenant run stream.
	RunNotifyWildcard = "runs.>"
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

// MiteDesiredSubject returns the subject for the desired-state request
// (core NATS, not JetStream). The operator sends the command as a request,
// and the gateway that holds the mite stream answers after it forwards the
// command. No stream may capture this subject: the command carries
// credential values, and NATS must keep no copy of them.
// Format: mite.<cluster>.<ns>.<name>.desired
func MiteDesiredSubject(cluster, ns, name string) string {
	return fmt.Sprintf("mite.%s.%s.%s.desired", cluster, ns, name)
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

// WebhookTenantSubject returns the subject for webhook replay payloads
// targeting a pipelines tenant (design.md D13). WebhookWildcard
// ("webhook.>") and WebhookClusterWildcard ("webhook.<cluster>.>") already
// capture this subject, so no stream or consumer change is needed to add
// it.
// Format: webhook.<cluster>.tenant.<tenant>
func WebhookTenantSubject(cluster, tenant string) string {
	return fmt.Sprintf("webhook.%s.tenant.%s", cluster, tenant)
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

// LedgerActionSubject returns the core-NATS subject the recorder publishes
// a ledger action on for one cluster. The operator of that cluster
// queue-subscribes to this subject only. The operators of all clusters
// share one queue-group name on the core NATS. A wildcard subject with
// that shared name would put them in one queue group, and NATS would
// give a share of one cluster's actions to another cluster.
// Format: varroa.ledger.actions.<cluster>
func LedgerActionSubject(cluster string) string {
	return fmt.Sprintf("varroa.ledger.actions.%s", cluster)
}

// BuildSubject returns the JetStream subject a gateway publishes one
// cell's build events on.
// Format: builds.<cluster>.<ns>.<cell>
func BuildSubject(cluster, ns, cell string) string {
	return fmt.Sprintf("builds.%s.%s.%s", cluster, ns, cell)
}

// LogSubject returns the core-NATS subject a gateway relays one run's
// console-log chunks on while a live tail lease is active.
// Format: logs.<cluster>.<ns>.<cell>.<runid>
func LogSubject(cluster, ns, cell, runID string) string {
	return fmt.Sprintf("logs.%s.%s.%s.%s", cluster, ns, cell, runID)
}

// TailSubject returns the core-NATS request-reply subject the BFF uses to
// start or stop a live-tail lease with the cell's gateway.
// Format: varroa.tail.<cluster>.<ns>.<cell>
func TailSubject(cluster, ns, cell string) string {
	return fmt.Sprintf("varroa.tail.%s.%s.%s", cluster, ns, cell)
}

// RunNotifySubject returns the core-NATS subject the recorder publishes one
// tenant's live run notifications on. The tenant of a controller is
// <namespace>/<controller>, which holds no dot, so the subject keeps three
// tokens.
// Format: runs.<cluster>.<tenant>
func RunNotifySubject(cluster, tenant string) string {
	return fmt.Sprintf("runs.%s.%s", cluster, tenant)
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

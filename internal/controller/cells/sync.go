// Package cells: this file defines the tenant.sync transport contract
// (design D6): the pure request and result shapes the placement pass sends
// over varroa.recordstore.tenant.sync, and the func-typed seams a caller
// wires to the real NATS call. Place calls Announcer the same way it calls
// PurgeFunc: a request-reply call that is not a Kubernetes write.
package cells

import (
	"context"
	"reflect"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// SyncPolicy carries the seven Team.spec.pipelines ledger and rehydration
// settings, plus the tenant's export grants, that an "announce" request's
// policy field holds (design D6, D10). It is the zero value for a
// controller with no cell role.
type SyncPolicy struct {
	RehydrateBuilds   int32
	RehydrateDeadline time.Duration
	InterruptedRetry  string
	Concurrency       string
	CronBackstop      bool
	CronCatchUp       string
	CronGrace         time.Duration

	// Exports carries the tenant's outward pipelines grants (design D10):
	// the jobs and locks this tenant exports, and the callers each entry
	// admits. Nil for a controller with no cell role, or for a tenant
	// that exports nothing.
	Exports []v1alpha1.TeamExport

	// Retention carries the tenant's record-store windows (the tenant
	// window and the per-job rules). Nil for a tenant that sets none.
	Retention *v1alpha1.TeamPipelinesRetention

	// ConcurrencyLimits carries the tenant's named job-set build caps. Nil
	// for a tenant that sets none.
	ConcurrencyLimits []v1alpha1.TeamConcurrencyLimit

	// CellMaxRuns is the per-tenant build cap of the pool that owns the
	// tenant's cell. Zero means no cap.
	CellMaxRuns int32

	// Webhook carries the tenant's completion webhook, resolved from the
	// Secret the Team names. Nil when the tenant declares no webhook or
	// its Secret is not usable. It never travels in the policy JSON; the
	// announce request carries it on its own field.
	Webhook *WebhookSync
}

// WebhookSync is the receiver URL and the HMAC signing key the record
// store posts a tenant's completion webhook with.
type WebhookSync struct {
	URL string
	Key string
}

// Equal reports whether p and other announce the same policy. SyncPolicy
// carries slice and pointer fields, so the == operator cannot compare two
// values; Equal compares the seven scalar settings directly, Exports,
// Retention, and Webhook by value.
func (p SyncPolicy) Equal(other SyncPolicy) bool {
	return p.RehydrateBuilds == other.RehydrateBuilds &&
		p.RehydrateDeadline == other.RehydrateDeadline &&
		p.InterruptedRetry == other.InterruptedRetry &&
		p.Concurrency == other.Concurrency &&
		p.CronBackstop == other.CronBackstop &&
		p.CronCatchUp == other.CronCatchUp &&
		p.CronGrace == other.CronGrace &&
		reflect.DeepEqual(p.Exports, other.Exports) &&
		reflect.DeepEqual(p.Retention, other.Retention) &&
		reflect.DeepEqual(p.ConcurrencyLimits, other.ConcurrencyLimits) &&
		p.CellMaxRuns == other.CellMaxRuns &&
		webhookSyncEqual(p.Webhook, other.Webhook)
}

// webhookSyncEqual compares two resolved webhooks. Two nil values are
// equal, and a nil value differs from any set value.
func webhookSyncEqual(a, b *WebhookSync) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SyncPolicyFromPipelines reads the seven settings and the export grants
// off a Team's spec.pipelines, applying the same defaults
// api/v1alpha1's Effective* accessors apply. A nil spec answers the zero
// SyncPolicy, matching a controller with no cell role, and contributes no
// export policy.
func SyncPolicyFromPipelines(p *v1alpha1.TeamPipelinesSpec) SyncPolicy {
	if p == nil {
		return SyncPolicy{}
	}
	return SyncPolicy{
		RehydrateBuilds:   p.EffectiveRehydrateBuilds(),
		RehydrateDeadline: p.EffectiveRehydrateDeadline(),
		InterruptedRetry:  p.EffectiveInterruptedRetry(),
		Concurrency:       p.EffectiveConcurrency(),
		CronBackstop:      p.EffectiveCronBackstop(),
		CronCatchUp:       p.EffectiveCronCatchUp(),
		CronGrace:         p.EffectiveCronGrace(),
		Exports:           p.Exports,
		Retention:         p.Retention,
		ConcurrencyLimits: p.ConcurrencyLimits,
	}
}

// DeadInstance is one confirmed-gone cell instance an AnnounceRequest
// carries (design D6). The caller names an instance here only when it
// confirms that instance is gone: a rolled pod's old instance, or a
// deleted cell's last-known instance.
type DeadInstance struct {
	Instance   string
	LastSeenAt time.Time
	// ConfirmedGoneAt is the time a pod read answered "no pod" for this
	// instance. A producer that made no pod read leaves it nil. A zero
	// time.Time is not a valid stand-in for nil: it marshals as
	// 0001-01-01T00:00:00Z, which reads as older than any grace and
	// confirms the loss at once.
	ConfirmedGoneAt *time.Time
	// Cell and Namespace name the cell that ran the instance. The
	// recorder hands an unconfirmed entry back with them, and the
	// operator reads that namespace's pods until the instance is gone.
	Cell      string
	Namespace string
}

// AnnounceMember is one member of a pipelines tenant an AnnounceRequest
// carries. The member set is announced as one list, so the recorder never
// observes a half-applied change. Slot 0's entry repeats the request's
// scalar fields.
type AnnounceMember struct {
	Cell      string
	Namespace string
	// Role is v1alpha1.TeamMemberRoleActive or TeamMemberRoleStandby.
	Role string
	// Slot is set only on an active member.
	Slot *int32
	// Instance is the cell pod's unique identifier, empty when no pod
	// runs.
	Instance string
	// RehydratedInstance and PendingJobs are this member's own seed state.
	RehydratedInstance string
	PendingJobs        []string
	// AppliedAt is when this member's cell first reported the tenant
	// applied. The promotion choice reads it on a later tick.
	AppliedAt *time.Time
	// LastSeenAt is this member's own newest mite-presence reading. A
	// later tick reports it as the dead instance's LastSeenAt, so each
	// member's window rests on its own clock and not on slot 0's.
	//
	// It never crosses the wire: dead_instances carries the value the
	// recorder needs, and the member entry carries none.
	LastSeenAt *time.Time
}

// Equal reports whether two member entries announce the same member.
//
// It ignores LastSeenAt on purpose. That value moves with every mite
// heartbeat, so comparing it would make every tick due for a fresh
// announcement.
func (m AnnounceMember) Equal(other AnnounceMember) bool {
	if m.Cell != other.Cell || m.Namespace != other.Namespace || m.Role != other.Role ||
		m.Instance != other.Instance || m.RehydratedInstance != other.RehydratedInstance {
		return false
	}
	if (m.Slot == nil) != (other.Slot == nil) {
		return false
	}
	if m.Slot != nil && *m.Slot != *other.Slot {
		return false
	}
	if (m.AppliedAt == nil) != (other.AppliedAt == nil) {
		return false
	}
	return stringSlicesEqual(m.PendingJobs, other.PendingJobs)
}

// membersEqual reports whether two announced member lists hold the same
// members in the same order.
func membersEqual(a, b []AnnounceMember) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// AnnounceRequest is one tenant's placement announcement (design D6, kind
// "announce"). The caller sends it immediately before a write that raises
// Team.status.placement.generation, at the same generation when the cell
// instance, the policy, or the rehydration state changes, and once every
// ten minutes as a resync.
type AnnounceRequest struct {
	Cluster string
	Tenant  string
	// Incarnation is the tenant object's metadata.uid. It tells one
	// tenant from the next under one name, and the recorder hands the
	// placement row to a tenant whose incarnation is new.
	Incarnation        string
	Namespace          string
	Cell               string
	Generation         int64
	CellInstance       string
	Policy             SyncPolicy
	RehydratedInstance string
	PendingJobs        []string
	CellAlive          bool
	CellAliveAt        *time.Time
	CellLastSeenAt     *time.Time
	DeadInstances      []DeadInstance
	// Members carries the whole member list of the tenant. Slot 0's entry
	// repeats the scalar fields above.
	Members []AnnounceMember
	// AgentNamespace is the tenant's spec.pipelines.agentNamespace. The
	// core reads it from the placement row, because a member cluster
	// holds the Team object.
	AgentNamespace string
}

// AnnounceResult is the "announce" reply (design D6).
type AnnounceResult struct {
	OK                bool
	InterruptedMarked int
	// Stale is true when the recorder refused the request because the
	// stored row is newer. OK is then false, so the pass stores no cache
	// entry and writes no placement status from its older reads.
	Stale bool
	// Unconfirmed lists every stored dead instance that names a cell and
	// carries no confirmation. The pass keeps it in SyncCache, and the
	// caller reads each pod again on every tick.
	Unconfirmed []DeadInstance
}

// Announcer sends one AnnounceRequest and reports the reply. The caller
// wires the real NATS request-reply call (bus.TenantSyncSubject, kind
// "announce"); a test injects a fake, the same way PurgeFunc does.
type Announcer func(ctx context.Context, req AnnounceRequest) (AnnounceResult, error)

// BuildRecord is one thin build record a RecordsResult carries for one job
// path (design D6, kind "records").
type BuildRecord struct {
	BuildNumber          int64
	QueuedAt             *time.Time
	StartedAt            *time.Time
	EndedAt              *time.Time
	DurationMs           *int64
	Result               string
	CauseDescription     string
	Params               []byte
	MaskedParameterNames []string
	Changes              []byte
}

// JobRecords is one job path's answer inside a RecordsResult: its next
// build number and the records selected for it.
type JobRecords struct {
	JobPath         string
	NextBuildNumber int64
	Records         []BuildRecord
}

// RecordsRequest is a tenant's build-record request (design D6, kind
// "records"). An empty JobPaths enumerates every job path the tenant holds
// a job_numbers row or a completed run for; a non-empty JobPaths pages at
// most 32 paths.
type RecordsRequest struct {
	Cluster  string
	Tenant   string
	JobPaths []string
	PerJob   int
}

// RecordsResult is the "records" reply (design D6).
type RecordsResult struct {
	Jobs []JobRecords
}

// RecordsFunc sends one RecordsRequest and reports the reply. The caller
// wires the real NATS request-reply call; a test injects a fake.
type RecordsFunc func(ctx context.Context, req RecordsRequest) (RecordsResult, error)

// SyncCache is the tenant's last successfully sent AnnounceRequest, kept by
// the caller across reconcile ticks in memory only. Place reads it to
// decide whether a same-generation announcement is due (task 4.4a) and to
// name a rolled or deleted cell's last-known instance in DeadInstances.
// The caller stores Outcome.SyncCacheUpdate back under the tenant's key
// after a successful send.
//
// An operator restart, a shard move, or a record store that has refused
// every announce since the start leaves the cache empty. An empty cache
// forces one announcement, and the cache producers then report no dead
// instance. cellPreviousInstances reads each cell's durable
// status.cell.previousInstance instead, so a pod replaced in that window
// is still reported. The recorder adds the rest from its own stored
// member list: an older replaced pod, and the last pod of a deleted cell.
type SyncCache struct {
	Cell               string
	CellNamespace      string
	Generation         int64
	CellInstance       string
	Policy             SyncPolicy
	RehydratedInstance string
	PendingJobs        []string
	// CellLastSeenAt is the bound cell's mite presence at the time
	// CellInstance was last confirmed current. Reported as a dead
	// instance's LastSeenAt when a later tick finds that instance gone.
	CellLastSeenAt *time.Time
	At             time.Time
	// Members is the member list the last announcement carried. The
	// member triggers compare against it, and the promotion choice reads
	// each member's applied time from it.
	Members []AnnounceMember
	// AgentNamespace is the agent namespace the last announcement
	// carried. A change is a trigger.
	AgentNamespace string
	// Unconfirmed is the recorder's list of unconfirmed dead instances
	// from the last announce reply. The stored placement row is the
	// durable copy: an empty cache announces at once, and the reply fills
	// the list again.
	Unconfirmed []DeadInstance
}

// HomesRequest asks for one tenant's member list and its job home map
// (design D9, kind "homes"). The recorder is the map's only owner.
type HomesRequest struct {
	Cluster string
	Tenant  string
}

// HomesMember is one member of the tenant a HomesResult carries.
type HomesMember struct {
	Cell      string
	Namespace string
	// Role is v1alpha1.TeamMemberRoleActive or TeamMemberRoleStandby.
	Role string
	// Slot is set only on an active member.
	Slot *int32
}

// HomesResult is the "homes" reply: the tenant's member list and its
// job-path to slot map.
type HomesResult struct {
	Members  []HomesMember
	JobHomes map[string]int32
}

// HomesFunc sends one HomesRequest and reports the reply. The caller
// wires the real NATS request-reply call; a test injects a fake.
//
// A failed call leaves the previous map in place for that tick. A stale
// map costs at most one refused claim, which the claim fence corrects.
type HomesFunc func(ctx context.Context, req HomesRequest) (HomesResult, error)

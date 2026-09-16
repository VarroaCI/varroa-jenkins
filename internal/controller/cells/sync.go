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
}

// Equal reports whether p and other announce the same policy. SyncPolicy
// carries a slice field, so the == operator cannot compare two values;
// Equal compares the seven scalar settings directly and Exports with
// reflect.DeepEqual.
func (p SyncPolicy) Equal(other SyncPolicy) bool {
	return p.RehydrateBuilds == other.RehydrateBuilds &&
		p.RehydrateDeadline == other.RehydrateDeadline &&
		p.InterruptedRetry == other.InterruptedRetry &&
		p.Concurrency == other.Concurrency &&
		p.CronBackstop == other.CronBackstop &&
		p.CronCatchUp == other.CronCatchUp &&
		p.CronGrace == other.CronGrace &&
		reflect.DeepEqual(p.Exports, other.Exports)
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
}

// AnnounceRequest is one tenant's placement announcement (design D6, kind
// "announce"). The caller sends it immediately before a write that raises
// Team.status.placement.generation, at the same generation when the cell
// instance, the policy, or the rehydration state changes, and once every
// ten minutes as a resync.
type AnnounceRequest struct {
	Cluster            string
	Tenant             string
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
}

// AnnounceResult is the "announce" reply (design D6).
type AnnounceResult struct {
	OK                bool
	InterruptedMarked int
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
// the caller across reconcile ticks (in memory; the tenant_placements row
// itself is the durable copy, so losing this cache costs one redundant,
// harmless announcement). Place reads it to decide whether a
// same-generation announcement is due (task 4.4a) and to name a rolled or
// deleted cell's last-known instance in DeadInstances; the caller stores
// Outcome.SyncCacheUpdate back under the tenant's key after a successful
// send.
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
}

// Package cells implements the pipelines-tenant placement pass (design D4 of
// add-cell-controllers): the decision logic that binds a Team to a cell
// Controller. Place performs no Kubernetes write. It reads only the Inputs
// and Team/TeamStatus values the caller hands it, and it names every write
// the caller must apply through its return value and through the mutated
// TeamStatus. The exceptions are PurgeFunc and Announcer, both NATS
// request-reply calls that are not a Kubernetes write (design D6, D7).
package cells

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

// teardownBoundedWait is the fixed wait design D4 gives a teardown whose
// owning cell exists but reports no apply, before completing anyway.
const teardownBoundedWait = 10 * time.Minute

// maxPurgeAttempts bounds the teardown schedule purge (design D4). A move's
// purge retry carries no such bound: a move never holds anything on it.
const maxPurgeAttempts = 10

// syncResyncInterval is the fixed period design D6/D7 gives the
// same-generation resync announcement: a placed tenant is re-announced at
// least this often, so a lost tenant_placements row repairs itself.
const syncResyncInterval = 10 * time.Minute

// teardownPendingMessage and tenantMovingMessage are held constant across
// ticks so the Placed condition's LastTransitionTime keeps recording when
// the wait began — setPlaced only stamps a fresh time when Status, Reason,
// or Message changes.
const (
	teardownPendingMessage = "the schedule purge must succeed, or ten attempts must pass, before teardown completes"
	tenantMovingMessage    = "the old cell must finish draining this tenant's folder before the move completes"
)

// CellCandidate is one cell Controller's read-only snapshot, gathered by the
// caller once per tick from a local List. Place never lists a Controller
// itself, so every field here is data, not a live read.
type CellCandidate struct {
	Name      string
	Namespace string

	// Enabled mirrors spec.cell.enabled.
	Enabled bool
	// CellReady mirrors the CellReady condition's Status == True.
	CellReady bool
	// Evacuate mirrors status.cell.evacuate.
	Evacuate bool
	// ResolvedProfile is the JenkinsVersionProfile name the caller resolved
	// for this cell's spec.version (controller.ResolveProfile), or "" when
	// only the embedded baseline matched. A cell can never satisfy a named
	// profile request through the baseline.
	ResolvedProfile string
	// MaxTenants mirrors spec.cell.maxTenants. 0 means the chart default,
	// supplied by Inputs.MaxTenantsDefault.
	MaxTenants int32

	// Tenants mirrors status.cell.tenants.
	Tenants []string
	// AppliedTenants mirrors status.cell.appliedTenants: the tenant set of
	// the desired state whose apply last succeeded.
	AppliedTenants []string
	// PendingDeletionPaths mirrors status.pendingItemDeletions[].Path.
	PendingDeletionPaths []string

	// Instance mirrors status.cell.instance: the running Jenkins pod's
	// metadata.uid, empty when no pod runs (design D6, D13).
	Instance string
	// MiteConnected mirrors status.miteStatus.connected: the operator's
	// newest mite-presence reading for Instance (design D6's cell_alive).
	MiteConnected bool
	// MiteLastSeen mirrors status.miteStatus.lastSeen: the newest time the
	// operator saw Instance's mite heartbeat (design D6's
	// cell_last_seen_at).
	MiteLastSeen *time.Time
}

// PurgeRequest is one tenant's schedule-purge request, sent over
// varroa.recordstore.tenant.purge whenever the tenant leaves a cell.
type PurgeRequest struct {
	Cluster       string
	Tenant        string
	OldGeneration int64
}

// PurgeFunc requests a tenant's job_schedules purge and reports whether the
// recorder answered success. The caller wires the real NATS request-reply
// call (bus.TenantPurgeSubject); a test injects a fake.
type PurgeFunc func(ctx context.Context, req PurgeRequest) (bool, error)

// Inputs carries every value Place needs beyond the Team itself: the cell
// listing, the profile presence test, the gate values, the managed-namespace
// set, the clock, and the purge requester. The caller gathers all of it once
// per tick; Place performs no Kubernetes read and no Kubernetes write.
type Inputs struct {
	// Now returns the current time. Defaults to time.Now when nil; a test
	// injects a fixed clock.
	Now func() time.Time

	// Cells lists every candidate cell Controller in the operator's own
	// cluster. Cross-cluster placement is out of scope, so Place never
	// needs to test cluster membership itself.
	Cells []CellCandidate

	// ProfileExists reports whether the named JenkinsVersionProfile exists.
	// A nil func is treated as "always exists" (used by tests that do not
	// exercise gate 3).
	ProfileExists func(name string) bool

	// ManagedNamespaces is the set of namespaces the control plane has RBAC
	// in. Contains always returns true in cluster-wide mode, so gate 5
	// needs no separate "scoped RBAC is on" flag.
	ManagedNamespaces tenancy.ManagedSet

	// RecordStoreEnabled and BlobsModeS3 are the operator's own boot-time
	// record-store gate values (refusal gates 1 and 2).
	RecordStoreEnabled bool
	BlobsModeS3        bool

	// MaxTenantsDefault is the chart default (cells.maxTenantsPerCell) used
	// when a candidate cell's MaxTenants is 0.
	MaxTenantsDefault int32

	// ClusterIdentity is the operator's own resolved cluster identity
	// (bus.ClusterFromEnv), written into status.placement.cluster.
	ClusterIdentity string

	// RequestPurge requests a tenant's schedule purge. Required whenever a
	// placed or torn-down tenant is reconciled; a nil func is treated as an
	// always-failing request so a missing wire-up is visible as a retry
	// rather than a panic.
	RequestPurge PurgeFunc

	// Announce sends a tenant.sync "announce" request (design D6, D7).
	// Required whenever a pipelines tenant is reconciled; a nil func is
	// treated as an always-failing request, the same way a nil
	// RequestPurge is: a missing wire-up shows up as a retry, not a panic.
	Announce Announcer

	// PendingJobs is this tenant's current pending-job set: the union of
	// the deferred job paths and the job paths no rehydration bundle has
	// covered yet. The rehydration rendering pass (design D4, task 5.7a)
	// computes it and hands it to Place through this field; a caller that
	// has not wired that pass yet leaves it nil, which Place treats as "no
	// pending job" rather than as a rehydrated tenant.
	PendingJobs []string

	// LastSync is this tenant's last successfully sent AnnounceRequest,
	// kept by the caller across reconcile ticks (design D6's five
	// triggers). Nil when none was sent yet, or when the caller's cache
	// was lost — Place then announces at once rather than waiting for the
	// resync window, which is harmless because the announce upsert is
	// idempotent.
	LastSync *SyncCache
}

// CellPatch names one cell's new complete tenant list. The caller applies
// each with PatchControllerCellStatus after the Team status patch succeeds
// (design D4's "one narrow writer" rule).
type CellPatch struct {
	Name      string
	Namespace string
	Tenants   []string
}

// ActivityEvent is one activity-stream event Place decided to publish. The
// caller publishes it through its own activity.EventSink.
type ActivityEvent struct {
	Type    string
	Message string
}

// Outcome names every write the caller must apply after Place returns, on
// top of the TeamStatus Place already mutated in place.
type Outcome struct {
	// CellPatches lists each affected cell's new tenant list.
	CellPatches []CellPatch
	// ActivityEvents lists the events to publish.
	ActivityEvents []ActivityEvent
	// FinalizerAdd is true when the caller must add
	// v1alpha1.TeamFinalizerTenantPlacement: a placement now exists and the
	// Team does not carry the finalizer yet.
	FinalizerAdd bool
	// FinalizerRelease is true when the caller must remove the finalizer:
	// teardown has converged.
	FinalizerRelease bool
	// SyncCacheUpdate is this tenant's new SyncCache value after Place sent
	// a successful announcement this tick. Nil when no announcement was
	// sent, or the one sent failed. The caller stores it under the
	// tenant's own key for the next tick's LastSync.
	SyncCacheUpdate *SyncCache
}

// Place mutates status in place and returns the writes the caller must
// still apply. It never patches the Team itself and it performs no
// Kubernetes write of its own.
func Place(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus) (Outcome, error) {
	if team == nil || status == nil {
		return Outcome{}, fmt.Errorf("cells: Place needs a non-nil team and status")
	}
	now := in.Now
	if now == nil {
		now = time.Now
	}
	nowT := now()
	hasFinalizer := containsString(team.Finalizers, v1alpha1.TeamFinalizerTenantPlacement)
	beingDeleted := team.DeletionTimestamp != nil && !team.DeletionTimestamp.IsZero()

	var out Outcome

	if team.Spec.Pipelines == nil || beingDeleted {
		if status.Placement == nil && !hasFinalizer {
			// Never a pipelines tenant, or a prior teardown already
			// finished; nothing to do.
			return out, nil
		}
		teardown(ctx, in, team, status, nowT, hasFinalizer, &out)
		return out, nil
	}

	// Credentials are unconditional: the door needs them even while a
	// refusal gate withholds placement.
	mintCredentials(status)
	status.Pipelines.AgentNamespace = team.Spec.Pipelines.AgentNamespace

	if status.Pipelines.PendingPurgeGeneration != nil {
		// A move still owes a purge for its old generation. This runs
		// every tick before the refusal gates and regardless of moveKind
		// or the current Placed reason, so neither a transient
		// CellNotReady tick nor a later refusal loses the retry (design
		// D4's move-purge retry).
		retryMovePurge(ctx, in, team, status)
	}

	if reason, refused := refuse(in, team); refused {
		setPlaced(status, metav1.ConditionFalse, reason, refusalMessage(reason), nowT)
		return out, nil
	}

	cell, kind := evaluateKeep(in, team, status)
	switch kind {
	case moveNone:
		reason := v1alpha1.TeamReasonTenantPlaced
		if placedReason(status) == v1alpha1.TeamReasonTenantMoved {
			reason = v1alpha1.TeamReasonTenantMoved
		}
		if cell.CellReady {
			setPlaced(status, metav1.ConditionTrue, reason, "", nowT)
		} else {
			setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonCellNotReady,
				"the bound cell reports CellReady=False; this tenant's builds do not start", nowT)
		}
		maybeReannounce(ctx, in, team, status, *cell, nowT, &out)
	case moveImmediate:
		performMove(ctx, in, team, status, cell, nowT, &out)
	case moveVoluntary:
		performVoluntaryMove(ctx, in, team, status, cell, nowT, &out)
	}

	if status.Placement != nil && !hasFinalizer {
		out.FinalizerAdd = true
	}
	return out, nil
}

// moveKind classifies why an existing (or absent) placement cannot be kept.
type moveKind int

const (
	// moveNone means the current placement (if any) stays as-is.
	moveNone moveKind = iota
	// moveImmediate means the tenant must move now, with no drain wait:
	// there was no placement yet, the bound cell is gone, or it is
	// evacuating.
	moveImmediate
	// moveVoluntary means the bound cell exists and is not evacuating, but
	// no longer satisfies the keep rule (profile, dedicated, or disabled).
	// The move waits for the drain signal first.
	moveVoluntary
)

// evaluateKeep implements the keep rule (task 6.3, tenant-placement spec).
// cell is nil for moveImmediate when there was no prior placement at all;
// otherwise it names the bound cell (found or not).
func evaluateKeep(in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus) (*CellCandidate, moveKind) {
	if status.Placement == nil {
		return nil, moveImmediate
	}
	c, found := lookupCell(in.Cells, status.Placement.Cell, status.Placement.CellNamespace)
	if !found {
		return nil, moveImmediate
	}
	if c.Evacuate {
		return &c, moveImmediate
	}
	if !c.Enabled || c.ResolvedProfile != team.Spec.Pipelines.Profile ||
		(team.Spec.Pipelines.Dedicated && len(c.Tenants) > 1) {
		return &c, moveVoluntary
	}
	return &c, moveNone
}

// refuse applies the five refusal gates in order (tenant-placement spec).
func refuse(in Inputs, team *v1alpha1.Team) (string, bool) {
	p := team.Spec.Pipelines
	if !in.RecordStoreEnabled {
		return v1alpha1.TeamReasonRecordStoreUnsupported, true
	}
	if !in.BlobsModeS3 {
		return v1alpha1.TeamReasonBlobTierUnsupported, true
	}
	if in.ProfileExists != nil && !in.ProfileExists(p.Profile) {
		return v1alpha1.TeamReasonProfileNotFound, true
	}
	if !containsString(team.Spec.Namespaces, p.AgentNamespace) {
		return v1alpha1.TeamReasonAgentNamespaceUnmanaged, true
	}
	if !in.ManagedNamespaces.Contains(p.AgentNamespace) {
		return v1alpha1.TeamReasonAgentNamespaceUnmanaged, true
	}
	return "", false
}

func refusalMessage(reason string) string {
	switch reason {
	case v1alpha1.TeamReasonRecordStoreUnsupported:
		return "pipelines need the record store enabled (recordStore.enabled)"
	case v1alpha1.TeamReasonBlobTierUnsupported:
		return "pipelines need the s3 blob tier (recordStore.blobs.mode)"
	case v1alpha1.TeamReasonProfileNotFound:
		return "the named JenkinsVersionProfile does not exist"
	case v1alpha1.TeamReasonAgentNamespaceUnmanaged:
		return "agentNamespace must be one of spec.namespaces and must be a managed namespace"
	default:
		return ""
	}
}

// mintCredentials mints the two door credentials, once, when spec.pipelines
// first appears. A stored value is never regenerated.
func mintCredentials(status *v1alpha1.TeamStatus) {
	if status.Pipelines == nil {
		status.Pipelines = &v1alpha1.TeamPipelinesStatus{}
	}
	if status.Pipelines.WebhookToken == "" {
		status.Pipelines.WebhookToken = randHex(16)
	}
	if status.Pipelines.WebhookSecret == "" {
		status.Pipelines.WebhookSecret = randHex(32)
	}
}

// randHex returns n random bytes, hex encoded. A crypto/rand failure is
// programmer error (an exhausted entropy source), not a condition Place can
// recover from meaningfully, so it panics rather than minting a weak or
// empty credential.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("cells: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// selectCandidate picks a cell for team, following the selection rule
// (tenant-placement spec): enabled, CellReady, not evacuating, the resolved
// profile matches; dedicated needs an empty cell (maxTenants treated as 1);
// otherwise the fewest tenants below maxTenants, breaking a tie by name.
func selectCandidate(in Inputs, team *v1alpha1.Team) *CellCandidate {
	var candidates []CellCandidate
	for _, c := range in.Cells {
		if !c.Enabled || !c.CellReady || c.Evacuate {
			continue
		}
		if c.ResolvedProfile == "" || c.ResolvedProfile != team.Spec.Pipelines.Profile {
			continue
		}
		maxTenants := c.MaxTenants
		if maxTenants <= 0 {
			maxTenants = in.MaxTenantsDefault
		}
		if team.Spec.Pipelines.Dedicated {
			if len(c.Tenants) != 0 {
				continue
			}
			maxTenants = 1
		}
		if int32(len(c.Tenants)) >= maxTenants {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].Tenants) != len(candidates[j].Tenants) {
			return len(candidates[i].Tenants) < len(candidates[j].Tenants)
		}
		return candidates[i].Name < candidates[j].Name
	})
	return &candidates[0]
}

// performMove implements the involuntary move order (tenant-placement
// spec): the fence (the new placement, with generation+1) is written before
// either cell's tenant list is patched.
func performMove(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus, oldCell *CellCandidate, nowT time.Time, out *Outcome) {
	newCell := selectCandidate(in, team)
	if newCell == nil {
		setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonAwaitingCell, "", nowT)
		return
	}

	oldGeneration := int64(0)
	oldCellName, oldCellNamespace := "", ""
	wasPlaced := status.Placement != nil
	if wasPlaced {
		oldGeneration = status.Placement.Generation
		oldCellName = status.Placement.Cell
		oldCellNamespace = status.Placement.CellNamespace
	}
	newGeneration := oldGeneration + 1

	reason := v1alpha1.TeamReasonTenantPlaced
	eventType := "tenant.placed"
	if wasPlaced {
		reason = v1alpha1.TeamReasonTenantMoved
		eventType = "tenant.moved"
	}

	// The announcement is the pre-step (design D6, D7): the record store
	// must hold the new generation before either Kubernetes write below.
	// A rolled pod is reported through the moveNone resync path; a deleted
	// old cell (oldCell nil while wasPlaced) is reported here, because
	// this is the one tick that still knows its identity.
	policy := SyncPolicyFromPipelines(team.Spec.Pipelines)
	var dead []DeadInstance
	if wasPlaced && oldCell == nil {
		dead = deadInstanceDelta(in.LastSync, oldCellName, oldCellNamespace, "")
	}
	result, announceErr := sendAnnounce(ctx, in, AnnounceRequest{
		Cluster:        in.ClusterIdentity,
		Tenant:         team.Name,
		Namespace:      newCell.Namespace,
		Cell:           newCell.Name,
		Generation:     newGeneration,
		CellInstance:   newCell.Instance,
		Policy:         policy,
		CellAlive:      newCell.MiteConnected,
		CellAliveAt:    &nowT,
		CellLastSeenAt: newCell.MiteLastSeen,
		DeadInstances:  dead,
	})
	if announceErr != nil || !result.OK {
		// A failed announcement stops the move for this tick (design
		// D6/D7's tenant-placement spec): the placement stays unchanged
		// and the Placed reason stays whatever it already was. Place
		// retries the announce, and the move, on the next tick.
		return
	}
	out.SyncCacheUpdate = &SyncCache{
		Cell: newCell.Name, CellNamespace: newCell.Namespace,
		Generation: newGeneration, CellInstance: newCell.Instance,
		Policy: policy, CellLastSeenAt: newCell.MiteLastSeen, At: nowT,
	}

	// The fence: write the placement before either cell's tenant list.
	status.Placement = &v1alpha1.TeamPlacementStatus{
		Cluster:       in.ClusterIdentity,
		Cell:          newCell.Name,
		CellNamespace: newCell.Namespace,
		Generation:    newGeneration,
	}
	setPlaced(status, metav1.ConditionTrue, reason, "", nowT)
	out.ActivityEvents = append(out.ActivityEvents, ActivityEvent{
		Type:    eventType,
		Message: fmt.Sprintf("team %s placed on cell %s/%s", team.Name, newCell.Namespace, newCell.Name),
	})

	out.CellPatches = append(out.CellPatches, CellPatch{
		Name:      newCell.Name,
		Namespace: newCell.Namespace,
		Tenants:   appendUnique(newCell.Tenants, team.Name),
	})
	if wasPlaced && oldCell != nil {
		out.CellPatches = append(out.CellPatches, CellPatch{
			Name:      oldCell.Name,
			Namespace: oldCell.Namespace,
			Tenants:   removeString(oldCell.Tenants, team.Name),
		})
	}

	if wasPlaced {
		gen := oldGeneration
		status.Pipelines.PendingPurgeGeneration = &gen
		if requestPurge(ctx, in, team.Name, oldGeneration) {
			status.Pipelines.PendingPurgeGeneration = nil
		}
	}
}

// performVoluntaryMove implements the three-step voluntary move
// (tenant-placement spec): set TenantMoving and wait for the drain signal,
// then perform the move exactly like an involuntary one.
func performVoluntaryMove(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus, oldCell *CellCandidate, nowT time.Time, out *Outcome) {
	if placedReason(status) != v1alpha1.TeamReasonTenantMoving {
		setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonTenantMoving, tenantMovingMessage, nowT)
		return
	}
	if containsString(oldCell.AppliedTenants, team.Name) || hasPendingDeletionForTenant(oldCell.PendingDeletionPaths, team.Name) {
		// Still draining: keep the message constant so LastTransitionTime
		// keeps recording when the wait began.
		setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonTenantMoving, tenantMovingMessage, nowT)
		return
	}
	performMove(ctx, in, team, status, oldCell, nowT, out)
}

// retryMovePurge re-requests the purge for status.Pipelines.
// PendingPurgeGeneration, every tick while that field is set. The delete is
// idempotent, so an indefinite retry is safe; there is no attempt bound
// because a move never blocks on it. The field, not the transient Placed
// reason, drives the retry, so it survives a tick that changes the reason
// for an unrelated cause (design D4's move-purge retry).
func retryMovePurge(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus) {
	gen := *status.Pipelines.PendingPurgeGeneration
	if requestPurge(ctx, in, team.Name, gen) {
		status.Pipelines.PendingPurgeGeneration = nil
	}
}

// requestPurge calls Inputs.RequestPurge and reports success. A failed
// purge on a move never blocks the move (tenant-placement spec); the caller
// decides what a failure means for its own retry state.
func requestPurge(ctx context.Context, in Inputs, tenant string, oldGeneration int64) bool {
	if in.RequestPurge == nil {
		return false
	}
	ok, err := in.RequestPurge(ctx, PurgeRequest{
		Cluster:       in.ClusterIdentity,
		Tenant:        tenant,
		OldGeneration: oldGeneration,
	})
	return ok && err == nil
}

// sendAnnounce calls Inputs.Announce and reports its reply. A nil Announce
// answers a failure, the same way requestPurge treats a nil RequestPurge:
// a missing wire-up is visible as a retry, not a panic.
func sendAnnounce(ctx context.Context, in Inputs, req AnnounceRequest) (AnnounceResult, error) {
	if in.Announce == nil {
		return AnnounceResult{}, fmt.Errorf("cells: no Announcer configured")
	}
	return in.Announce(ctx, req)
}

// maybeReannounce sends the same-generation announcement (tenant-placement
// spec, task 4.4a) when the owning cell's instance changed, the seven
// pipelines settings changed, the tenant's rehydration state changed, or
// the resync window elapsed. cell is the tenant's current, unchanged
// binding (moveNone): status.Placement already names it.
func maybeReannounce(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus, cell CellCandidate, nowT time.Time, out *Outcome) {
	policy := SyncPolicyFromPipelines(team.Spec.Pipelines)
	rehydratedInstance, pendingJobs := currentRehydrationState(status, in.PendingJobs)

	last := in.LastSync
	due := last == nil ||
		last.CellInstance != cell.Instance ||
		!last.Policy.Equal(policy) ||
		last.RehydratedInstance != rehydratedInstance ||
		!stringSlicesEqual(last.PendingJobs, pendingJobs) ||
		nowT.Sub(last.At) >= syncResyncInterval
	if !due {
		return
	}

	dead := deadInstanceDelta(last, status.Placement.Cell, status.Placement.CellNamespace, cell.Instance)

	result, err := sendAnnounce(ctx, in, AnnounceRequest{
		Cluster:            in.ClusterIdentity,
		Tenant:             team.Name,
		Namespace:          status.Placement.CellNamespace,
		Cell:               status.Placement.Cell,
		Generation:         status.Placement.Generation,
		CellInstance:       cell.Instance,
		Policy:             policy,
		RehydratedInstance: rehydratedInstance,
		PendingJobs:        pendingJobs,
		CellAlive:          cell.MiteConnected,
		CellAliveAt:        &nowT,
		CellLastSeenAt:     cell.MiteLastSeen,
		DeadInstances:      dead,
	})
	if err != nil || !result.OK {
		return
	}
	out.SyncCacheUpdate = &SyncCache{
		Cell: status.Placement.Cell, CellNamespace: status.Placement.CellNamespace,
		Generation: status.Placement.Generation, CellInstance: cell.Instance,
		Policy: policy, RehydratedInstance: rehydratedInstance, PendingJobs: pendingJobs,
		CellLastSeenAt: cell.MiteLastSeen, At: nowT,
	}
}

// currentRehydrationState answers the rehydrated_instance and pending_jobs
// an announcement carries (design D13): the rehydrated instance is
// status.rehydration.cellInstance while the Rehydrated condition reads
// True/TenantRehydrated, and empty otherwise; pending_jobs is the caller's
// own PendingJobs seam (task 5.7a fills it) while not applied, and empty
// once applied.
func currentRehydrationState(status *v1alpha1.TeamStatus, pendingJobsSeam []string) (rehydratedInstance string, pendingJobs []string) {
	if !rehydratedApplied(status) {
		return "", pendingJobsSeam
	}
	if status.Rehydration != nil {
		rehydratedInstance = status.Rehydration.CellInstance
	}
	return rehydratedInstance, nil
}

// rehydratedApplied reports whether status already carries
// Rehydrated=True/TenantRehydrated, as task 4.5's rehydration pass (and
// task 6.5, which completes it) last wrote it.
func rehydratedApplied(status *v1alpha1.TeamStatus) bool {
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TeamConditionRehydrated {
			return c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.TeamReasonTenantRehydrated
		}
	}
	return false
}

// deadInstanceDelta answers the one dead instance to report for a rolled
// pod or a deleted cell (design D6): last's own cell binding, when it names
// the same cell as cellName/cellNamespace and a different instance than
// currentInstance. It answers nil otherwise, including the first tick of a
// brand new placement, when last is nil.
func deadInstanceDelta(last *SyncCache, cellName, cellNamespace, currentInstance string) []DeadInstance {
	if last == nil || last.Cell != cellName || last.CellNamespace != cellNamespace ||
		last.CellInstance == "" || last.CellInstance == currentInstance {
		return nil
	}
	seenAt := last.At
	if last.CellLastSeenAt != nil {
		seenAt = *last.CellLastSeenAt
	}
	return []DeadInstance{{Instance: last.CellInstance, LastSeenAt: seenAt}}
}

// stringSlicesEqual reports whether a and b hold the same strings in the
// same order.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// teardown implements both teardown stages (tenant-placement spec). Stage
// one drops the tenant from the owning cell's tenant list and sets
// TenantTeardownPending. Stage two clears the placement and releases the
// finalizer once the cell confirms the drain (or is absent, or the bounded
// wait expires) and the schedule purge succeeds or exhausts its ten
// attempts.
func teardown(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus, nowT time.Time, hasFinalizer bool, out *Outcome) {
	if status.Placement == nil {
		finishTeardown(status, out, hasFinalizer)
		return
	}

	cell, found := lookupCell(in.Cells, status.Placement.Cell, status.Placement.CellNamespace)
	if found && containsString(cell.Tenants, team.Name) {
		out.CellPatches = append(out.CellPatches, CellPatch{
			Name:      cell.Name,
			Namespace: cell.Namespace,
			Tenants:   removeString(cell.Tenants, team.Name),
		})
	}

	if !found {
		// A deleted cell never reports an apply; teardown completes at
		// once. The purge is still attempted, best-effort, but never holds
		// the finalizer: no cell remains to drain.
		requestPurge(ctx, in, team.Name, status.Placement.Generation)
		finishTeardown(status, out, hasFinalizer)
		return
	}

	drained := !containsString(cell.AppliedTenants, team.Name) && !hasPendingDeletionForTenant(cell.PendingDeletionPaths, team.Name)
	boundedWaitExpired := nowT.Sub(placedTransitionTime(status)) >= teardownBoundedWait &&
		placedReason(status) == v1alpha1.TeamReasonTenantTeardownPending
	if !drained && !boundedWaitExpired {
		setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonTenantTeardownPending, teardownPendingMessage, nowT)
		return
	}

	ok, err := false, error(nil)
	if in.RequestPurge != nil {
		ok, err = in.RequestPurge(ctx, PurgeRequest{
			Cluster:       in.ClusterIdentity,
			Tenant:        team.Name,
			OldGeneration: status.Placement.Generation,
		})
	}
	if ok && err == nil {
		finishTeardown(status, out, hasFinalizer)
		return
	}

	attempts := int32(0)
	if status.Pipelines != nil {
		attempts = status.Pipelines.PurgeAttempts
	}
	attempts++
	if status.Pipelines == nil {
		status.Pipelines = &v1alpha1.TeamPipelinesStatus{}
	}
	status.Pipelines.PurgeAttempts = attempts
	if attempts >= maxPurgeAttempts {
		out.ActivityEvents = append(out.ActivityEvents, ActivityEvent{
			Type:    "tenant.purge_failed",
			Message: fmt.Sprintf("cluster %s tenant %s: schedule purge failed %d times; teardown completed anyway", in.ClusterIdentity, team.Name, attempts),
		})
		finishTeardown(status, out, hasFinalizer)
		return
	}
	setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonTenantTeardownPending, teardownPendingMessage, nowT)
}

// finishTeardown clears the placement and every pipelines status field, and
// removes the Placed condition, matching "the Team SHALL survive with no
// placement and no Placed condition once teardown completes".
func finishTeardown(status *v1alpha1.TeamStatus, out *Outcome, hasFinalizer bool) {
	status.Placement = nil
	status.Pipelines = nil
	status.Conditions = removeCondition(status.Conditions, v1alpha1.TeamConditionPlaced)
	if hasFinalizer {
		out.FinalizerRelease = true
	}
}

// lookupCell finds a cell candidate by name and namespace.
func lookupCell(cells []CellCandidate, name, namespace string) (CellCandidate, bool) {
	for _, c := range cells {
		if c.Name == name && c.Namespace == namespace {
			return c, true
		}
	}
	return CellCandidate{}, false
}

// hasPendingDeletionForTenant reports whether paths names the tenant's
// folder or a path beneath it. Controller.status.pendingItemDeletions is
// one flat, controller-wide list with no per-tenant rollup, so this is a
// prefix match.
func hasPendingDeletionForTenant(paths []string, tenant string) bool {
	for _, p := range paths {
		if p == tenant || strings.HasPrefix(p, tenant+"/") {
			return true
		}
	}
	return false
}

// setPlaced upserts the Placed condition, preserving LastTransitionTime
// when Status, Reason, and Message are unchanged — the same rule
// TeamReconciler.setCondition applies to every other Team condition, kept
// local here because Place is the only writer of this one's Message during
// a wait, and the wait's bounded-timeout arithmetic depends on that
// preserved timestamp.
func setPlaced(status *v1alpha1.TeamStatus, s metav1.ConditionStatus, reason, message string, nowT time.Time) {
	cond := v1alpha1.TeamCondition{
		Type:               v1alpha1.TeamConditionPlaced,
		Status:             s,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(nowT),
	}
	for i, existing := range status.Conditions {
		if existing.Type != cond.Type {
			continue
		}
		if existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
			cond.LastTransitionTime = existing.LastTransitionTime
		}
		status.Conditions[i] = cond
		return
	}
	status.Conditions = append(status.Conditions, cond)
}

// removeCondition drops every condition of the given type.
func removeCondition(conds []v1alpha1.TeamCondition, condType string) []v1alpha1.TeamCondition {
	out := make([]v1alpha1.TeamCondition, 0, len(conds))
	for _, c := range conds {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	return out
}

// placedReason returns the current Placed condition's Reason, or "" when
// none is set.
func placedReason(status *v1alpha1.TeamStatus) string {
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TeamConditionPlaced {
			return c.Reason
		}
	}
	return ""
}

// placedTransitionTime returns the current Placed condition's
// LastTransitionTime, or the zero time when none is set.
func placedTransitionTime(status *v1alpha1.TeamStatus) time.Time {
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TeamConditionPlaced {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// containsString reports whether slice holds s.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// removeString returns a copy of slice with every occurrence of s removed.
func removeString(slice []string, s string) []string {
	out := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// appendUnique returns a copy of slice with s appended, unless slice already
// holds it.
func appendUnique(slice []string, s string) []string {
	if containsString(slice, s) {
		out := make([]string, len(slice))
		copy(out, slice)
		return out
	}
	out := make([]string, 0, len(slice)+1)
	out = append(out, slice...)
	out = append(out, s)
	return out
}

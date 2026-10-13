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
	// MaxTenants mirrors spec.cell.maxTenants. 0 resolves through the cell's
	// effective owner, then through Inputs.MaxTenantsDefault.
	MaxTenants int32
	// Pool mirrors the varroa.dev/cell-pool label. It is the raw label, so
	// every test resolves it against Inputs.Pools first: a label naming a
	// deleted pool gives no effective owner.
	Pool string

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
	// PreviousInstance mirrors status.cell.previousInstance: the pod uid
	// Instance held before its last change. The Controller reconciler
	// writes it only after its own pod read proved that pod gone, so it
	// is a confirmed dead instance.
	PreviousInstance string
	// PreviousInstanceLastSeen mirrors
	// status.cell.previousInstanceLastSeen: the last mite heartbeat
	// recorded while PreviousInstance ran. It is nil when none was seen.
	PreviousInstanceLastSeen *time.Time
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
	// Cell narrows the purge to one cell's schedule rows. A slot move
	// raises the tenant generation, so a whole-tenant delete would drop
	// every other slot's rows. Teardown leaves it empty.
	Cell string
	// CellNamespace is that cell's namespace. A schedule row is keyed by
	// the namespace and the cell, so the name alone can name two cells.
	CellNamespace string
	// Incarnation is the tenant object's metadata.uid. A teardown purge
	// can arrive after the finalizer clears, so the recorder compares this
	// value with the one its placement row holds and keeps a row a newer
	// tenant of the same name already owns.
	Incarnation string
	// TeamDeleted reports that the Team object is under deletion. The
	// recorder then retires Incarnation, so a late announcement of the
	// deleted Team cannot restore the placement row. A teardown for a Team
	// that only drops spec.pipelines leaves it false, because that Team
	// keeps its uid. A move never sets it.
	TeamDeleted bool
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

	// Webhook is the tenant's resolved completion webhook. The caller
	// reads its Secret; the placement pass only carries it into the
	// announce policy, so a URL or key change re-announces.
	Webhook *WebhookSync

	// Cells lists every candidate cell Controller in the operator's own
	// cluster. Cross-cluster placement is out of scope, so Place never
	// needs to test cluster membership itself.
	Cells []CellCandidate

	// Pools lists every live CellPool. The caller gathers it once per tick,
	// beside the cell list. A pool absent from this slice does not exist, so
	// a cell labelled with its name has no effective owner.
	Pools []CellPoolSummary

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

	// MemberSeed answers one member cell's own seed state for this
	// tenant: the instance whose rehydration bundle applied there, and
	// the job paths that cell has not seeded yet. The caller reads both
	// from that cell's own status.cell.rehydration entry.
	//
	// The seam exists because one member's deferred job must never hold
	// another member's build. A nil func answers no seed for every
	// member, which closes the fence until the caller wires it.
	MemberSeed func(namespace, cell string) (rehydratedInstance string, pendingJobs []string)

	// PendingJobs is this tenant's current pending-job set: the union of
	// the deferred job paths and the job paths no rehydration bundle has
	// covered yet. The rehydration rendering pass (design D4, task 5.7a)
	// computes it and hands it to Place through this field; a caller that
	// has not wired that pass yet leaves it nil, which Place treats as "no
	// pending job" rather than as a rehydrated tenant.
	PendingJobs []string

	// DepartedPods answers, for one member cell that is absent from the
	// candidate set, whether that cell's Jenkins pod is gone. The key is
	// the cell's namespace and name, as memberKey builds it.
	//
	// A true value is a confirmed absence: the caller's read succeeded
	// and answered no pod, or a pod in phase Failed or Succeeded. Every
	// other outcome answers false — a pod that still exists, a read that
	// failed, a Controller that is gone, and a cell the caller did not
	// read. A gone Controller can leave a pod that still drains, so
	// ConfirmedDeparted confirms such an instance later, by its pod UID.
	//
	// The pass reports a member confirmed gone only on that value, and a
	// confirmed reading is what arms a takeover of the member's runs. The
	// caller builds the map because Place performs no Kubernetes read.
	DepartedPods map[string]bool

	// ConfirmedDeparted lists the unconfirmed dead instances of
	// LastSync.Unconfirmed whose pod the caller's own read proved gone,
	// each with ConfirmedGoneAt set. A non-empty list makes an
	// announcement due, and both announce paths carry it. The caller
	// builds it because Place performs no Kubernetes read.
	ConfirmedDeparted []DeadInstance

	// AgentPodsPending is true while a tearing-down tenant still has agent
	// pods in its agent namespace, or while the caller could not count
	// them. Teardown stage two holds on it, inside the same bounded wait
	// that holds on the cell drain. The caller deletes the pods and counts
	// them, because Place performs no Kubernetes read.
	AgentPodsPending bool

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

// CellClaim names the cell one tenant's new placement selected. The caller
// writes it with WriteCellClaim before it patches the Team status, and
// retires it with ReleaseCellClaim after the tenant patch. The claim makes
// the cell itself carry the intent the Team fence records, so a pool drain
// cannot delete the chosen cell in the window between the two writes.
type CellClaim struct {
	Name      string
	Namespace string
	Tenant    string
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
	// CellClaims names every cell this tenant needs held: the cell a new
	// placement selected, and each member cell whose tenant list does not
	// name the tenant yet. A cell leaves the list once its tenant patch
	// has landed. The caller writes every entry before the Team status
	// patch, and retires each one only after that cell's own tenant patch
	// succeeds.
	//
	// Every member cell needs a claim, not slot 0's alone. A pool drain
	// reads the fence set before it re-reads a free cell, and only the
	// claim closes the window in which the drain deletes a cell this pass
	// is binding.
	CellClaims []CellClaim
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

	if reason, message, refused := refuse(in, team); refused {
		setPlaced(status, metav1.ConditionFalse, reason, message, nowT)
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
		// The fence names this cell, and the cell's own tenant list does
		// not name the tenant yet, so the tenant patch has not landed. The
		// pass sends the tenant patch again, and the claim must live again
		// this tick: it expires after CellClaimTTL, and a drain would then
		// find a cell with no tenant and no claim. The claim retires only
		// when the patch lands, so the tenant list converges or the cell
		// stays held.
		if !containsString(cell.Tenants, team.Name) {
			out.CellClaims = append(out.CellClaims, CellClaim{
				Name:      cell.Name,
				Namespace: cell.Namespace,
				Tenant:    team.Name,
			})
			out.CellPatches = append(out.CellPatches, CellPatch{
				Name:      cell.Name,
				Namespace: cell.Namespace,
				Tenants:   appendUnique(cell.Tenants, team.Name),
			})
		}
		maybeReannounce(ctx, in, team, status, *cell, nowT, &out)
	case moveImmediate:
		performMove(ctx, in, team, status, cell, nowT, &out)
	case moveVoluntary:
		performVoluntaryMove(ctx, in, team, status, cell, nowT, &out)
	}

	applyMemberReason(team, status, nowT)

	if status.Placement != nil && !hasFinalizer {
		out.FinalizerAdd = true
	}
	return out, nil
}

// applyMemberReason overrides the Placed reason with the one the member
// set implies, when the member set has one to state. SlotPending outranks
// StandbyPending, and both report Placed=False. MembersApplied reports
// Placed=True.
//
// A tenant that asks for one member and no standby never reaches this
// path, so every earlier reason is unchanged for it.
func applyMemberReason(team *v1alpha1.Team, status *v1alpha1.TeamStatus, nowT time.Time) {
	if status.Placement == nil || team.Spec.Pipelines == nil {
		return
	}
	wantedSlots := team.Spec.Pipelines.EffectiveMembers()
	wantedStandby := team.Spec.Pipelines.EffectiveStandby()
	if wantedSlots+wantedStandby <= 1 {
		return
	}
	switch memberReasonFromStatus(status.Placement.Members, wantedSlots, wantedStandby) {
	case v1alpha1.TeamReasonSlotPending:
		setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonSlotPending,
			"at least one active slot holds no cell", nowT)
	case v1alpha1.TeamReasonStandbyPending:
		setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonStandbyPending,
			"every active slot holds a cell and at least one standby does not", nowT)
	case v1alpha1.TeamReasonMembersApplied:
		setPlaced(status, metav1.ConditionTrue, v1alpha1.TeamReasonMembersApplied, "", nowT)
	}
}

// memberReasonFromStatus answers the member reason for a stored member
// list, the shape applyMemberReason reads after the pass wrote it.
func memberReasonFromStatus(members []v1alpha1.TeamMemberStatus, wantedSlots, wantedStandby int32) string {
	bound := make([]MemberBinding, 0, len(members))
	for _, m := range members {
		bound = append(bound, MemberBinding{
			Cell: m.Cell, Namespace: m.CellNamespace, Role: m.Role, Slot: m.Slot, Applied: m.Applied,
		})
	}
	return memberReason(bound, wantedSlots, wantedStandby)
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
	// no longer satisfies the keep rule (profile, pool membership, or
	// disabled).
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
	if !c.Enabled || c.ResolvedProfile != team.Spec.Pipelines.Profile || !keepsMembership(in, team, c) {
		return &c, moveVoluntary
	}
	return &c, moveNone
}

// keepsMembership answers the membership test for a bound cell: it holds
// when the cell has no effective owner, or when its effective owner equals
// the tenant's resolved pool. Deleting a pool therefore moves no tenant.
func keepsMembership(in Inputs, team *v1alpha1.Team, c CellCandidate) bool {
	owner := effectiveOwner(in.Pools, c.Pool)
	return owner == nil || owner.Name == ResolvedPoolName(team.Spec.Pipelines)
}

// refuse applies the six refusal gates in order (tenant-placement spec). It
// returns the reason and the message together, because gate 4 names the pool
// and the profile that pool actually serves, values refusalMessage cannot
// reach from a reason alone.
func refuse(in Inputs, team *v1alpha1.Team) (string, string, bool) {
	p := team.Spec.Pipelines
	if !in.RecordStoreEnabled {
		return refused(v1alpha1.TeamReasonRecordStoreUnsupported)
	}
	if !in.BlobsModeS3 {
		return refused(v1alpha1.TeamReasonBlobTierUnsupported)
	}
	if in.ProfileExists != nil && !in.ProfileExists(p.Profile) {
		return refused(v1alpha1.TeamReasonProfileNotFound)
	}
	// A named pool must exist and must serve the named profile. A default
	// pool the operator has not created yet is not a refusal: the pool
	// reconciler creates it within one tick.
	if p.CellPool != "" {
		owner := effectiveOwner(in.Pools, p.CellPool)
		switch {
		case owner == nil:
			return v1alpha1.TeamReasonCellPoolNotFound,
				fmt.Sprintf("the CellPool %q does not exist", p.CellPool), true
		case owner.ProfileRef != p.Profile:
			return v1alpha1.TeamReasonCellPoolNotFound,
				fmt.Sprintf("the CellPool %q serves profile %q, not %q", p.CellPool, owner.ProfileRef, p.Profile), true
		}
	}
	if !containsString(team.Spec.Namespaces, p.AgentNamespace) {
		return refused(v1alpha1.TeamReasonAgentNamespaceUnmanaged)
	}
	if !in.ManagedNamespaces.Contains(p.AgentNamespace) {
		return refused(v1alpha1.TeamReasonAgentNamespaceUnmanaged)
	}
	return "", "", false
}

// refused pairs a fixed reason with its fixed message.
func refused(reason string) (string, string, bool) {
	return reason, refusalMessage(reason), true
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
// profile matches, the membership test holds, and the cell holds fewer
// tenants than its resolved capacity. It takes the fewest tenants first and
// breaks a tie by name.
func selectCandidate(in Inputs, team *v1alpha1.Team) *CellCandidate {
	resolvedPool := ResolvedPoolName(team.Spec.Pipelines)
	var candidates []CellCandidate
	for _, c := range in.Cells {
		if !c.Enabled || !c.CellReady || c.Evacuate {
			continue
		}
		if c.ResolvedProfile == "" || c.ResolvedProfile != team.Spec.Pipelines.Profile {
			continue
		}
		// The membership test for a candidate: the effective owner equals
		// the resolved pool, or the cell has no effective owner and the
		// tenant names no pool. A cell owned by another live pool is never
		// a candidate, so a single-tenant pool cannot be poached.
		owner := effectiveOwner(in.Pools, c.Pool)
		if owner != nil {
			if owner.Name != resolvedPool {
				continue
			}
		} else if team.Spec.Pipelines.CellPool != "" {
			continue
		}
		if int32(len(c.Tenants)) >= resolveCellCapacity(c.MaxTenants, owner, in.MaxTenantsDefault) {
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

// noCandidateReason answers the Placed reason for a tick that found no
// candidate: CellCapReached when the tenant's resolved pool sits at its cap,
// and AwaitingCell otherwise. The pass still creates no Controller.
func noCandidateReason(in Inputs, team *v1alpha1.Team) (string, string) {
	pool := effectiveOwner(in.Pools, ResolvedPoolName(team.Spec.Pipelines))
	if pool != nil && pool.CapReached {
		return v1alpha1.TeamReasonCellCapReached,
			fmt.Sprintf("the CellPool %q holds maxCells cells and none has a free tenant slot", pool.Name)
	}
	return v1alpha1.TeamReasonAwaitingCell, ""
}

// performMove implements the involuntary move order (tenant-placement
// spec): the fence (the new placement, with generation+1) is written before
// either cell's tenant list is patched.
func performMove(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus, oldCell *CellCandidate, nowT time.Time, out *Outcome) {
	newCell := promotionCellForSlotZero(in, team, status)
	if newCell == nil {
		newCell = selectCandidate(in, team)
	}
	if newCell == nil {
		reason, message := noCandidateReason(in, team)
		setPlaced(status, metav1.ConditionFalse, reason, message, nowT)
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
	// A deleted old cell (oldCell nil while wasPlaced) is reported here,
	// because this is the one tick that still knows its identity. A
	// rolled pod of a cell that stays a candidate is reported through
	// cellPreviousInstances, on this path and on the moveNone path.
	policy := syncPolicyFor(in, team)
	agentNamespace := teamAgentNamespace(team)
	var dead []DeadInstance
	if wasPlaced && oldCell == nil {
		// The old cell left the candidate set, so this call site reads
		// no pod of its own. It stamps the confirmation only when the
		// caller's own read of that cell answers no pod, because a
		// deleting cell pod can run its whole termination grace. An
		// unconfirmed entry keeps the five-minute moved window.
		dead = deadInstanceDelta(in.LastSync, oldCellName, oldCellNamespace, "",
			confirmedGoneAt(in, oldCellNamespace, oldCellName, nowT))
	}
	priorMembers := status.Placement.DeepCopy()
	members, _ := desiredMembers(in, team, status, newCell, nowT)
	announced := announceMembers(members)
	dead = append(dead, memberDeadInstances(in.LastSync, announced, dead, in.DepartedPods, nowT)...)
	dead = cellPreviousInstances(in, priorMembers, announced, dead, nowT)
	dead = appendConfirmedDeparted(dead, in.ConfirmedDeparted, announced)
	result, announceErr := sendAnnounce(ctx, in, AnnounceRequest{
		Cluster:        in.ClusterIdentity,
		Tenant:         team.Name,
		Incarnation:    string(team.UID),
		Namespace:      newCell.Namespace,
		Cell:           newCell.Name,
		Generation:     newGeneration,
		CellInstance:   newCell.Instance,
		Policy:         policy,
		CellAlive:      newCell.MiteConnected,
		CellAliveAt:    &nowT,
		CellLastSeenAt: newCell.MiteLastSeen,
		DeadInstances:  dead,
		Members:        announced,
		AgentNamespace: agentNamespace,
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
		Members: announced, AgentNamespace: agentNamespace,
		Unconfirmed: result.Unconfirmed,
	}

	// The claim: the caller writes it onto the new cell before the fence
	// below reaches the Team. A pool drain that re-read the cell earlier
	// then loses its delete precondition, and one that re-reads it later
	// finds the claim.
	out.CellClaims = append(out.CellClaims, CellClaim{
		Name:      newCell.Name,
		Namespace: newCell.Namespace,
		Tenant:    team.Name,
	})

	// The fence: write the placement before either cell's tenant list.
	status.Placement = &v1alpha1.TeamPlacementStatus{
		Cluster:       in.ClusterIdentity,
		Cell:          newCell.Name,
		CellNamespace: newCell.Namespace,
		Generation:    newGeneration,
		Members:       memberStatuses(members),
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
	appendMemberCellPatches(in, team, members, out)
	appendReleasedMemberPatches(ctx, in, team, priorMembers, members, out)
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
		status.Pipelines.PendingPurgeCell, status.Pipelines.PendingPurgeCellNamespace = purgeCellRef(oldCell)
		if requestPurge(ctx, in, team, oldGeneration,
			status.Pipelines.PendingPurgeCell, status.Pipelines.PendingPurgeCellNamespace) {
			status.Pipelines.PendingPurgeGeneration = nil
			status.Pipelines.PendingPurgeCell = ""
			status.Pipelines.PendingPurgeCellNamespace = ""
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
	if requestPurge(ctx, in, team, gen,
		status.Pipelines.PendingPurgeCell, status.Pipelines.PendingPurgeCellNamespace) {
		status.Pipelines.PendingPurgeGeneration = nil
		status.Pipelines.PendingPurgeCell = ""
		status.Pipelines.PendingPurgeCellNamespace = ""
	}
}

// purgeCellRef answers the cell a move's purge must narrow to. A move
// with no previous cell answers two empty strings, which keeps the
// whole-tenant delete.
func purgeCellRef(oldCell *CellCandidate) (name, namespace string) {
	if oldCell == nil {
		return "", ""
	}
	return oldCell.Name, oldCell.Namespace
}

// requestPurge calls Inputs.RequestPurge and reports success. A failed
// purge on a move never blocks the move (tenant-placement spec); the caller
// decides what a failure means for its own retry state.
func requestPurge(ctx context.Context, in Inputs, team *v1alpha1.Team, oldGeneration int64,
	cell, cellNamespace string) bool {
	if in.RequestPurge == nil {
		return false
	}
	ok, err := in.RequestPurge(ctx, PurgeRequest{
		Cluster:       in.ClusterIdentity,
		Tenant:        team.Name,
		OldGeneration: oldGeneration,
		Cell:          cell,
		CellNamespace: cellNamespace,
		Incarnation:   string(team.UID),
	})
	return ok && err == nil
}

// requestTeardownPurge requests the whole-tenant purge of a teardown. It
// reports a deleted Team, so the recorder retires the Team's incarnation.
// A nil RequestPurge answers a failure, the same way requestPurge does.
func requestTeardownPurge(ctx context.Context, in Inputs, team *v1alpha1.Team, generation int64) (bool, error) {
	if in.RequestPurge == nil {
		return false, nil
	}
	return in.RequestPurge(ctx, PurgeRequest{
		Cluster:       in.ClusterIdentity,
		Tenant:        team.Name,
		OldGeneration: generation,
		Incarnation:   string(team.UID),
		TeamDeleted:   team.DeletionTimestamp != nil && !team.DeletionTimestamp.IsZero(),
	})
}

// teamAgentNamespace answers the tenant's spec.pipelines.agentNamespace,
// or the empty string for a Team with no pipelines spec.
func teamAgentNamespace(team *v1alpha1.Team) string {
	if team.Spec.Pipelines == nil {
		return ""
	}
	return team.Spec.Pipelines.AgentNamespace
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

// syncPolicyFor builds the announce policy of one tenant: the seven
// pipelines settings and the export grants, plus the resolved webhook the
// caller read from the tenant's Secret.
func syncPolicyFor(in Inputs, team *v1alpha1.Team) SyncPolicy {
	policy := SyncPolicyFromPipelines(team.Spec.Pipelines)
	policy.Webhook = in.Webhook
	if pool := effectiveOwner(in.Pools, ResolvedPoolName(team.Spec.Pipelines)); pool != nil {
		policy.CellMaxRuns = pool.MaxRunsPerTenant
	}
	return policy
}

// maybeReannounce sends the same-generation announcement (tenant-placement
// spec, task 4.4a) when the owning cell's instance changed, the seven
// pipelines settings changed, the tenant's rehydration state changed, the
// agent namespace changed, the caller confirmed a departed instance gone,
// or the resync window elapsed. cell is the tenant's current, unchanged
// binding (moveNone): status.Placement already names it.
func maybeReannounce(ctx context.Context, in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus, cell CellCandidate, nowT time.Time, out *Outcome) {
	policy := syncPolicyFor(in, team)
	agentNamespace := teamAgentNamespace(team)
	rehydratedInstance, pendingJobs := currentRehydrationState(in, cell)

	members, _ := desiredMembers(in, team, status, &cell, nowT)
	announced := announceMembers(members)
	// A member joining, a member leaving, a member instance change, a
	// member seed change, and a member applied change are each a trigger.
	membersChanged := in.LastSync == nil || !membersEqual(in.LastSync.Members, announced)
	// The generation rises only when the tenant's bound cell set changes.
	cellSetChanged := memberCellSetChanged(status.Placement, members)

	last := in.LastSync
	due := last == nil ||
		last.CellInstance != cell.Instance ||
		!last.Policy.Equal(policy) ||
		last.RehydratedInstance != rehydratedInstance ||
		!stringSlicesEqual(last.PendingJobs, pendingJobs) ||
		last.AgentNamespace != agentNamespace ||
		membersChanged ||
		len(in.ConfirmedDeparted) > 0 ||
		nowT.Sub(last.At) >= syncResyncInterval
	if !due {
		return
	}

	generation := status.Placement.Generation
	if cellSetChanged {
		generation++
	}

	// The reannounce path stands behind updateCellInstance's own pod read,
	// so it stamps the confirmation with this tick's clock.
	dead := deadInstanceDelta(last, status.Placement.Cell, status.Placement.CellNamespace, cell.Instance, &nowT)
	dead = append(dead, memberDeadInstances(last, announced, dead, in.DepartedPods, nowT)...)
	dead = cellPreviousInstances(in, status.Placement, announced, dead, nowT)
	dead = appendConfirmedDeparted(dead, in.ConfirmedDeparted, announced)

	result, err := sendAnnounce(ctx, in, AnnounceRequest{
		Cluster:            in.ClusterIdentity,
		Tenant:             team.Name,
		Incarnation:        string(team.UID),
		Namespace:          status.Placement.CellNamespace,
		Cell:               status.Placement.Cell,
		Generation:         generation,
		CellInstance:       cell.Instance,
		Policy:             policy,
		RehydratedInstance: rehydratedInstance,
		PendingJobs:        pendingJobs,
		CellAlive:          cell.MiteConnected,
		CellAliveAt:        &nowT,
		CellLastSeenAt:     cell.MiteLastSeen,
		DeadInstances:      dead,
		Members:            announced,
		AgentNamespace:     agentNamespace,
	})
	if err != nil || !result.OK {
		// A failed announcement stops the member change for this tick.
		// The member list is unchanged and the pass retries next tick.
		return
	}
	out.SyncCacheUpdate = &SyncCache{
		Cell: status.Placement.Cell, CellNamespace: status.Placement.CellNamespace,
		Generation: generation, CellInstance: cell.Instance,
		Policy: policy, RehydratedInstance: rehydratedInstance, PendingJobs: pendingJobs,
		CellLastSeenAt: cell.MiteLastSeen, At: nowT, Members: announced,
		AgentNamespace: agentNamespace, Unconfirmed: result.Unconfirmed,
	}

	// The announce is the pre-step, so the status write follows it. The
	// prior member list is read before the write, because the release
	// path compares against it.
	prior := status.Placement.DeepCopy()
	status.Placement.Generation = generation
	status.Placement.Members = memberStatuses(members)
	appendMemberCellPatches(in, team, members, out)
	appendReleasedMemberPatches(ctx, in, team, prior, members, out)
}

// memberCellSetChanged reports whether the tenant's bound cell set
// differs between the stored member list and the decided one. The tenant
// generation rises only on such a change.
// A placement whose stored member list is empty describes one slot-0
// member on its scalar cell, so a tenant the pass has not yet written a
// member list for raises no generation on its first member-carrying tick.
func memberCellSetChanged(placement *v1alpha1.TeamPlacementStatus, decided []MemberBinding) bool {
	before := map[string]bool{}
	for _, m := range placement.Members {
		before[memberKey(m.CellNamespace, m.Cell)] = true
	}
	if len(before) == 0 && placement.Cell != "" {
		before[memberKey(placement.CellNamespace, placement.Cell)] = true
	}
	after := map[string]bool{}
	for _, m := range decided {
		after[memberKey(m.Namespace, m.Cell)] = true
	}
	if len(before) != len(after) {
		return true
	}
	for key := range after {
		if !before[key] {
			return true
		}
	}
	return false
}

// memberDeadInstances reports a dead instance for every member whose
// instance changed since the last announcement, and not for slot 0 alone.
//
// A member whose cell is still bound and reports a new instance carries a
// confirmation stamped with nowT, exactly as deadInstanceDelta stamps slot
// 0's. The evidence is the same: the Controller reconciler reads that
// cell's pod every tick, and CellCandidate.Instance is the pod UID it
// read. Without the stamp the confirmed reading never holds for a member
// above slot 0, so no run of that member is ever armed for a resume and no
// unstarted intent of it is ever re-fired.
//
// A member whose cell left the candidate set carries a confirmation only
// when departed reports that cell's pod gone. The cell object, its
// StatefulSet, and its pod can all go within seconds of a cell delete, and
// without the confirmation no run of that member is ever armed for the
// promoted standby. A cell whose pod still terminates carries no
// confirmation, because such a pod can serve builds for its whole
// termination grace.
//
// The LastSeenAt comes from the member's own presence reading. A cache
// entry written before that reading existed carries none, and the entry
// then falls back to the tenant's scalar reading and the cache time.
func memberDeadInstances(last *SyncCache, announced []AnnounceMember, reported []DeadInstance,
	departed map[string]bool, nowT time.Time) []DeadInstance {
	if last == nil {
		return nil
	}
	current := map[string]string{}
	for _, m := range announced {
		current[memberKey(m.Namespace, m.Cell)] = m.Instance
	}
	already := map[string]bool{}
	for _, d := range reported {
		already[d.Instance] = true
	}
	var out []DeadInstance
	for _, before := range last.Members {
		if before.Instance == "" || already[before.Instance] {
			continue
		}
		now, stillBound := current[memberKey(before.Namespace, before.Cell)]
		if stillBound && now == before.Instance {
			continue
		}
		already[before.Instance] = true
		entry := DeadInstance{
			Instance:   before.Instance,
			LastSeenAt: memberLastSeen(before, last),
			Cell:       before.Cell,
			Namespace:  before.Namespace,
		}
		if stillBound || departed[memberKey(before.Namespace, before.Cell)] {
			stamped := nowT
			entry.ConfirmedGoneAt = &stamped
		}
		out = append(out, entry)
	}
	return out
}

// cellPreviousInstances reports the status.cell.previousInstance of every
// cell the tenant touches as a dead instance, on every announcement.
//
// The cell set is the cells of the announced members and the cells the
// stored placement names, so a tenant that leaves a live cell still
// reports the pod that cell replaced.
//
// The Controller reconciler writes previousInstance only after its own pod
// read proved that pod gone, so each entry carries a confirmation stamped
// with nowT. The record is durable in the Controller status. It does not
// depend on SyncCache, which an operator restart or a shard move empties,
// and which a successful announce can fill without the replaced pod.
//
// The recorder merges entries by instance and keeps the earliest
// confirmation, so a repeat of an entry changes nothing. dead is the list
// the cache producers built. An entry of dead that names a recorded
// instance with no confirmation takes one, and no second entry is added.
// Such an entry also takes the recorded heartbeat when it is newer.
// An instance that a member runs now is never reported. The answer is
// dead with the new entries appended.
func cellPreviousInstances(in Inputs, placement *v1alpha1.TeamPlacementStatus,
	announced []AnnounceMember, dead []DeadInstance, nowT time.Time) []DeadInstance {
	type cellRef struct{ name, namespace string }
	var refs []cellRef
	for _, m := range announced {
		refs = append(refs, cellRef{m.Cell, m.Namespace})
	}
	if placement != nil {
		refs = append(refs, cellRef{placement.Cell, placement.CellNamespace})
		for _, m := range placement.Members {
			refs = append(refs, cellRef{m.Cell, m.CellNamespace})
		}
	}

	index := map[string]int{}
	for i, d := range dead {
		index[d.Instance] = i
	}
	skip := map[string]bool{}
	for _, m := range announced {
		if m.Instance != "" {
			skip[m.Instance] = true
		}
	}
	visited := map[string]bool{}
	for _, ref := range refs {
		key := memberKey(ref.namespace, ref.name)
		if ref.name == "" || visited[key] {
			continue
		}
		visited[key] = true
		cell, found := lookupCell(in.Cells, ref.name, ref.namespace)
		if !found {
			continue
		}
		prev := cell.PreviousInstance
		if prev == "" || prev == cell.Instance || skip[prev] {
			continue
		}
		confirmed := nowT
		if i, ok := index[prev]; ok {
			if dead[i].ConfirmedGoneAt == nil {
				dead[i].ConfirmedGoneAt = &confirmed
			}
			if seen := cell.PreviousInstanceLastSeen; seen != nil && seen.After(dead[i].LastSeenAt) {
				dead[i].LastSeenAt = *seen
			}
			continue
		}
		seenAt := nowT
		if cell.PreviousInstanceLastSeen != nil {
			seenAt = *cell.PreviousInstanceLastSeen
		}
		index[prev] = len(dead)
		dead = append(dead, DeadInstance{Instance: prev, LastSeenAt: seenAt, ConfirmedGoneAt: &confirmed,
			Cell: ref.name, Namespace: ref.namespace})
	}
	return dead
}

// appendConfirmedDeparted adds the caller's confirmed departed instances
// to dead. An entry of dead that names the same instance with no
// confirmation takes the confirmation, and no second entry is added. Each
// confirmation reaches the recorder once: the reply that follows no
// longer lists the instance as unconfirmed.
//
// An instance that an announced member runs is never added. A confirmation
// of a named instance would make the recorder refuse every later
// announcement as stale.
func appendConfirmedDeparted(dead, confirmed []DeadInstance, announced []AnnounceMember) []DeadInstance {
	index := map[string]int{}
	for i, d := range dead {
		index[d.Instance] = i
	}
	live := map[string]bool{}
	for _, m := range announced {
		if m.Instance != "" {
			live[m.Instance] = true
		}
	}
	for _, c := range confirmed {
		if c.Instance == "" || c.ConfirmedGoneAt == nil || live[c.Instance] {
			continue
		}
		if i, ok := index[c.Instance]; ok {
			if dead[i].ConfirmedGoneAt == nil {
				stamped := *c.ConfirmedGoneAt
				dead[i].ConfirmedGoneAt = &stamped
			}
			if dead[i].Cell == "" {
				dead[i].Cell, dead[i].Namespace = c.Cell, c.Namespace
			}
			continue
		}
		index[c.Instance] = len(dead)
		dead = append(dead, c)
	}
	return dead
}

// confirmedGoneAt answers the confirmation stamp for one cell that left the
// candidate set: nowT when the caller's own pod read answers no pod, and
// nil otherwise.
func confirmedGoneAt(in Inputs, namespace, cell string, nowT time.Time) *time.Time {
	if !in.DepartedPods[memberKey(namespace, cell)] {
		return nil
	}
	stamped := nowT
	return &stamped
}

// memberLastSeen answers the newest presence reading for one member: its
// own when the cache carries one, else the tenant's scalar reading, else
// the time the cache was written.
func memberLastSeen(before AnnounceMember, last *SyncCache) time.Time {
	if before.LastSeenAt != nil {
		return *before.LastSeenAt
	}
	if last.CellLastSeenAt != nil {
		return *last.CellLastSeenAt
	}
	return last.At
}

// appendMemberCellPatches names the tenant on every member cell that does
// not list it yet. The tenant's items and its placement rehydration seed
// then apply on every member, active or standby, exactly as they do for
// slot 0.
func appendMemberCellPatches(in Inputs, team *v1alpha1.Team, members []MemberBinding, out *Outcome) {
	for _, m := range members {
		if m.Slot != nil && *m.Slot == 0 {
			// Slot 0's patch is the caller's own, written beside its
			// claim.
			continue
		}
		cell, found := lookupCell(in.Cells, m.Cell, m.Namespace)
		if !found || containsString(cell.Tenants, team.Name) {
			continue
		}
		// The claim holds this cell for the same window slot 0's claim
		// holds its own: the caller writes it before the status patch,
		// so a drain that already read the fence set still finds a
		// claim when it re-reads the cell.
		out.CellClaims = append(out.CellClaims, CellClaim{
			Name:      cell.Name,
			Namespace: cell.Namespace,
			Tenant:    team.Name,
		})
		out.CellPatches = append(out.CellPatches, CellPatch{
			Name:      cell.Name,
			Namespace: cell.Namespace,
			Tenants:   appendUnique(cell.Tenants, team.Name),
		})
	}
}

// appendReleasedMemberPatches drops the tenant from every cell it held a
// member on before this tick and holds no member on now. A scale-down of
// the slots, a scale-down of the standbys, and a member replacement all
// reach this path, so no former member keeps the tenant's folder, its
// desired state, or a unit of that cell's capacity.
//
// Slot 0's cell is excluded: the keep and move path owns it, and it drops
// the tenant from the cell it leaves in the same tick it binds the new
// one.
//
// Each released cell also takes a cell-scoped schedule purge, so the
// backstop stops firing that cell's rows. The purge is best effort, the
// same way a move's is: the row's own staleness test already skips a row
// the released cell exported.
func appendReleasedMemberPatches(ctx context.Context, in Inputs, team *v1alpha1.Team,
	prior *v1alpha1.TeamPlacementStatus, members []MemberBinding, out *Outcome) {
	if prior == nil {
		return
	}
	kept := map[string]bool{}
	for _, m := range members {
		kept[memberKey(m.Namespace, m.Cell)] = true
	}
	for _, before := range prior.Members {
		if before.Cell == "" || kept[memberKey(before.CellNamespace, before.Cell)] {
			continue
		}
		if before.Slot != nil && *before.Slot == 0 {
			continue
		}
		requestPurge(ctx, in, team, prior.Generation, before.Cell, before.CellNamespace)
		cell, found := lookupCell(in.Cells, before.Cell, before.CellNamespace)
		if !found || !containsString(cell.Tenants, team.Name) {
			continue
		}
		out.CellPatches = append(out.CellPatches, CellPatch{
			Name:      cell.Name,
			Namespace: cell.Namespace,
			Tenants:   removeString(cell.Tenants, team.Name),
		})
	}
}

// currentRehydrationState answers the rehydrated_instance and pending_jobs
// an announcement carries for slot 0 (design D13). The rehydrated
// instance is the seed the bound cell itself reports through the
// MemberSeed seam, and empty while that cell applies none. pending_jobs
// is the caller's own PendingJobs seam (task 5.7a fills it) and does not
// depend on the instance: a job deferred on an applied seed stays listed
// while every other job builds.
//
// The seed comes from the cell and never from the Rehydrated condition.
// The rehydration pass writes that condition after this pass runs, so a
// read of it reports the state of the tick before. The tenant would be
// announced as unseeded on the tick its seed applies, and the fence
// would hold every build until the next announcement. The member entries
// read the same seam, so the scalar and the member seed always agree.
func currentRehydrationState(in Inputs, cell CellCandidate) (rehydratedInstance string, pendingJobs []string) {
	if in.MemberSeed != nil {
		rehydratedInstance, _ = in.MemberSeed(cell.Namespace, cell.Name)
	}
	return rehydratedInstance, in.PendingJobs
}

// deadInstanceDelta answers the one dead instance to report for a rolled
// pod or a deleted cell (design D6): last's own cell binding, when it names
// the same cell as cellName/cellNamespace and a different instance than
// currentInstance. It answers nil otherwise, including the first tick of a
// brand new placement, when last is nil.
//
// confirmedGoneAt stamps the entry only for a caller whose own pod read
// proved the instance gone. Every other caller passes nil, and the entry
// then keeps the five-minute moved window.
func deadInstanceDelta(last *SyncCache, cellName, cellNamespace, currentInstance string,
	confirmedGoneAt *time.Time) []DeadInstance {
	if last == nil || last.Cell != cellName || last.CellNamespace != cellNamespace ||
		last.CellInstance == "" || last.CellInstance == currentInstance {
		return nil
	}
	seenAt := last.At
	if last.CellLastSeenAt != nil {
		seenAt = *last.CellLastSeenAt
	}
	return []DeadInstance{{Instance: last.CellInstance, LastSeenAt: seenAt, ConfirmedGoneAt: confirmedGoneAt,
		Cell: cellName, Namespace: cellNamespace}}
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
	// A tenant holds one member on each of several cells, so teardown
	// drops it from every one of them. A cell the pass misses keeps the
	// tenant folder and a unit of its own capacity for good.
	appendTeardownMemberPatches(in, team, status.Placement, out)

	membersDrained := membersDrainedTenant(in, team, status.Placement)
	// The bounded wait is the escape from every hold below, the agent
	// pods included: a pod that never goes still completes the teardown.
	boundedWaitExpired := nowT.Sub(placedTransitionTime(status)) >= teardownBoundedWait &&
		placedReason(status) == v1alpha1.TeamReasonTenantTeardownPending
	podsGone := !in.AgentPodsPending || boundedWaitExpired
	if !found && membersDrained && podsGone {
		// A deleted cell never reports an apply, and every other member
		// has drained, so teardown completes at once. The purge is still
		// attempted, best effort, but never holds the finalizer: no cell
		// remains to drain.
		_, _ = requestTeardownPurge(ctx, in, team, status.Placement.Generation)
		finishTeardown(status, out, hasFinalizer)
		return
	}

	// A missing scalar cell never blocks on itself, and a surviving member
	// cell still does. Releasing the finalizer over such a member would
	// leave it the tenant folder and a unit of its capacity, and would
	// remove the retry path that drops them. The bounded wait below is the
	// escape: a member that never reports still completes the teardown.
	// The finishing tick clears status.pipelines, which holds the only
	// copy of the agent namespace once spec.pipelines is gone, so the
	// agent pods hold this stage and not the caller's finalizer release.
	drained := membersDrained && (!found || cellDrainedTenant(cell, team.Name)) && podsGone
	if !drained && !boundedWaitExpired {
		setPlaced(status, metav1.ConditionFalse, v1alpha1.TeamReasonTenantTeardownPending, teardownPendingMessage, nowT)
		return
	}

	ok, err := requestTeardownPurge(ctx, in, team, status.Placement.Generation)
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

// appendTeardownMemberPatches drops the tenant from every member cell but
// slot 0's, which the caller patches beside this call.
func appendTeardownMemberPatches(in Inputs, team *v1alpha1.Team, placement *v1alpha1.TeamPlacementStatus, out *Outcome) {
	for _, m := range placement.Members {
		if m.Cell == "" || (m.Cell == placement.Cell && m.CellNamespace == placement.CellNamespace) {
			continue
		}
		cell, found := lookupCell(in.Cells, m.Cell, m.CellNamespace)
		if !found || !containsString(cell.Tenants, team.Name) {
			continue
		}
		out.CellPatches = append(out.CellPatches, CellPatch{
			Name:      cell.Name,
			Namespace: cell.Namespace,
			Tenants:   removeString(cell.Tenants, team.Name),
		})
	}
}

// cellDrainedTenant reports whether one cell has finished dropping the
// tenant's folder: it no longer reports the tenant applied and owes no
// item deletion for it.
func cellDrainedTenant(cell CellCandidate, tenant string) bool {
	return !containsString(cell.AppliedTenants, tenant) &&
		!hasPendingDeletionForTenant(cell.PendingDeletionPaths, tenant)
}

// membersDrainedTenant reports whether every member cell the pass can
// still read has dropped the tenant. A cell the candidate set no longer
// holds counts as drained: the object is gone, so nothing remains to
// drain and nothing would ever report one.
func membersDrainedTenant(in Inputs, team *v1alpha1.Team, placement *v1alpha1.TeamPlacementStatus) bool {
	for _, m := range placement.Members {
		cell, found := lookupCell(in.Cells, m.Cell, m.CellNamespace)
		if !found {
			continue
		}
		if !cellDrainedTenant(cell, team.Name) {
			return false
		}
	}
	return true
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

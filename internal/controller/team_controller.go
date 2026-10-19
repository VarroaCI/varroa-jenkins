package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

// TeamReconciler reconciles Team CRDs by composing them into owned Group +
// VarroaRoleBinding objects, and — for a pipelines tenant (spec.pipelines
// set) — running the placement pass (design D4) that binds it to a cell.
type TeamReconciler struct {
	client            ResourceClient
	store             crdstore.Backend
	operatorNamespace string
	managedSet        tenancy.ManagedSet
	logger            *slog.Logger

	// clusterIdentity is the operator's own resolved cluster identity
	// (bus.ClusterFromEnv), written into status.placement.cluster.
	clusterIdentity string
	// claimGate and blobstoreURL mirror Reconciler's identically-named
	// fields (controller_controller.go): the boot-time VARROA_CLAIM_GATE
	// and VARROA_BLOBSTORE_URL environment the placement pass's refusal
	// gates 1 and 2 read.
	claimGate    string
	blobstoreURL string
	// cellsConfig supplies MaxTenantsPerCell, the chart default a
	// candidate cell falls back to when spec.cell.maxTenants is 0.
	cellsConfig CellsConfig
	// activityPublisher publishes tenant.placed / tenant.moved /
	// tenant.purge_failed. A nil sink drops the events (matches
	// Reconciler.activityPublisher's convention).
	activityPublisher activity.EventSink
	// requestPurge sends the C11 job_schedules purge request. A nil func
	// makes every purge attempt fail, which is visible (a held finalizer,
	// a retried move) rather than a panic.
	requestPurge cells.PurgeFunc
	// announce sends the C6 tenant.sync "announce" request (design D6,
	// D7). A nil func makes every announcement fail, which is visible (the
	// fence never opens) rather than a panic.
	announce cells.Announcer

	// syncCacheMu guards syncCache, the process-local memory of each
	// tenant's last successfully sent announcement (design D6's five
	// triggers). Reconcile calls run one at a time per Team but can run
	// concurrently across different Teams (controller-runtime's
	// MaxConcurrentReconciles), so the map itself needs its own lock even
	// though a single Team's entry is never touched by two goroutines at
	// once.
	syncCacheMu sync.Mutex
	// syncCache is keyed by Team name. It is a dedup and resync clock,
	// and the only record of each member's last announced instance. An
	// empty entry forces one announcement, and the cache producers then
	// report no replaced pod. The placement pass reads each cell's
	// status.cell.previousInstance to cover that case.
	syncCache map[string]cells.SyncCache

	// reapMu guards lastReap for the same reason syncCacheMu guards
	// syncCache: reconciles of different Teams run concurrently.
	reapMu sync.Mutex
	// lastReap is keyed by Team name. It holds the time of the last
	// steady-state agent pod list, which the reaper rate-limits.
	lastReap map[string]time.Time
}

// NewTeamReconciler creates a new TeamReconciler. The managed set defaults to
// cluster-wide; in scoped-RBAC mode the caller must inject the real set via
// SetManagedSet or Unmanaged namespaces are never detected (C1). The
// pipelines placement pass needs its own setters below — SetClusterIdentity,
// SetPipelinesGates, SetCellsConfig, SetActivityPublisher, and
// SetPurgeRequester — before it can place a tenant; a Team with no
// spec.pipelines never needs any of them.
func NewTeamReconciler(client ResourceClient, store crdstore.Backend, operatorNamespace string, logger *slog.Logger) *TeamReconciler {
	return &TeamReconciler{
		client:            client,
		store:             store,
		operatorNamespace: operatorNamespace,
		managedSet:        tenancy.NewManagedSet("", operatorNamespace),
		logger:            logger,
		syncCache:         make(map[string]cells.SyncCache),
	}
}

// SetManagedSet injects the managedNamespaces set (the same one the controller
// reconciler's tenancy gate uses) so Team namespace states honor scoped RBAC.
func (r *TeamReconciler) SetManagedSet(set tenancy.ManagedSet) {
	r.managedSet = set
}

// SetClusterIdentity injects the operator's own resolved cluster identity.
func (r *TeamReconciler) SetClusterIdentity(cluster string) {
	r.clusterIdentity = cluster
}

// SetPipelinesGates injects the record-store and blob-tier boot-time gate
// values the placement pass's refusal gates 1 and 2 read.
func (r *TeamReconciler) SetPipelinesGates(claimGate, blobstoreURL string) {
	r.claimGate = claimGate
	r.blobstoreURL = blobstoreURL
}

// SetCellsConfig injects the warm-pool/tenant-cap/grace-period tunables; the
// placement pass reads MaxTenantsPerCell.
func (r *TeamReconciler) SetCellsConfig(cfg CellsConfig) {
	r.cellsConfig = cfg
}

// SetActivityPublisher injects the activity sink for tenant.placed,
// tenant.moved, and tenant.purge_failed.
func (r *TeamReconciler) SetActivityPublisher(pub activity.EventSink) {
	r.activityPublisher = pub
}

// SetPurgeRequester injects the C11 job_schedules purge requester. Production
// wires NewNATSPurgeRequester(conn); a test injects a fake.
func (r *TeamReconciler) SetPurgeRequester(fn cells.PurgeFunc) {
	r.requestPurge = fn
}

// SetSyncAnnouncer injects the C6 tenant.sync "announce" requester.
// Production wires NewNATSSyncAnnouncer(conn); a test injects a fake.
func (r *TeamReconciler) SetSyncAnnouncer(fn cells.Announcer) {
	r.announce = fn
}

// loggedAnnounce wraps the injected announcer so a stale refusal leaves a
// log line. The placement pass treats the refusal as a failed announce,
// and without the line it reads the same as a transport failure. A nil
// announcer stays nil, because the pass reports a nil one as a failure.
func (r *TeamReconciler) loggedAnnounce() cells.Announcer {
	if r.announce == nil {
		return nil
	}
	announce := r.announce
	return func(ctx context.Context, req cells.AnnounceRequest) (cells.AnnounceResult, error) {
		result, err := announce(ctx, req)
		if err == nil && result.Stale {
			r.logger.Info("recorder refused a stale announce", "team", req.Tenant,
				"generation", req.Generation, "cellInstance", req.CellInstance)
		}
		return result, err
	}
}

// lastSync reads team's cached last-sent announcement, or nil when none is
// cached.
func (r *TeamReconciler) lastSync(team string) *cells.SyncCache {
	r.syncCacheMu.Lock()
	defer r.syncCacheMu.Unlock()
	if c, ok := r.syncCache[team]; ok {
		return &c
	}
	return nil
}

// storeSyncCache saves team's new cache value after Place reports a
// successful announcement (Outcome.SyncCacheUpdate).
func (r *TeamReconciler) storeSyncCache(team string, c cells.SyncCache) {
	r.syncCacheMu.Lock()
	defer r.syncCacheMu.Unlock()
	r.syncCache[team] = c
}

// Reconcile reconciles a single Team by name.
func (r *TeamReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	team, err := crdstore.Get[v1alpha1.Team](ctx, r.store, req.Name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.Info("Team not found, skipping", "team", req.Name)
			return reconcile.Result{}, nil
		}
		r.logger.Error("failed to get Team", "team", req.Name, "error", err)
		return reconcile.Result{}, err
	}

	status := &v1alpha1.TeamStatus{}
	team.Status.DeepCopyInto(status)
	status.ObservedGeneration = team.Generation

	role := defaultRoleRef(team.Spec.RoleRef)

	// --- roleRef guardrail ---
	if role == "admin" {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonInvalidRoleRef,
			Message: "roleRef 'admin' not permitted on a Team",
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	}

	if _, err := crdstore.Get[v1alpha1.VarroaRole](ctx, r.store, role, ""); apierrors.IsNotFound(err) {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonInvalidRoleRef,
			Message: fmt.Sprintf("roleRef %q does not exist", role),
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	} else if err != nil {
		r.logger.Error("failed to get VarroaRole", "role", role, "error", err)
		return reconcile.Result{}, err
	}

	labels := teamChildLabels(team)
	ownerRef := teamOwnerRef(team)

	// --- owned Group (only if members) ---
	groupRef := ""
	if len(team.Spec.Members) > 0 {
		wantName := "team-" + team.Name
		if !r.ensureOwned(ctx, "Group", wantName, team) {
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionRBACReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonGroupApplyFailed,
				Message: fmt.Sprintf("Group %q exists without Team ownership", wantName),
			})
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonChildApplyFailed,
				Message: fmt.Sprintf("failed to apply Group %q", wantName),
			})
			if err := r.patchStatus(ctx, team.Name, status); err != nil {
				r.logger.Error("failed to patch status", "team", team.Name, "error", err)
			}
			return reconcile.Result{}, nil
		}
		want := &v1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{
				Name:            wantName,
				Labels:          labels,
				OwnerReferences: []metav1.OwnerReference{ownerRef},
			},
			Spec: v1alpha1.GroupSpec{
				DisplayName: team.Spec.DisplayName,
				Members:     team.Spec.Members,
			},
		}
		if err := crdstore.Apply[v1alpha1.Group](ctx, r.store, want); err != nil {
			r.logger.Error("failed to apply Group", "group", wantName, "error", err)
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionRBACReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonGroupApplyFailed,
				Message: fmt.Sprintf("failed to apply Group %q: %v", wantName, err),
			})
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonChildApplyFailed,
				Message: fmt.Sprintf("failed to apply Group %q", wantName),
			})
			if err := r.patchStatus(ctx, team.Name, status); err != nil {
				r.logger.Error("failed to patch status", "team", team.Name, "error", err)
			}
			return reconcile.Result{}, nil
		}
		groupRef = wantName
	} else {
		// No members: delete a previously-owned Group if one exists.
		delName := "team-" + team.Name
		if r.ensureOwned(ctx, "Group", delName, team) {
			if err := crdstore.Delete[v1alpha1.Group](ctx, r.store, delName, ""); err != nil {
				r.logger.Warn("failed to delete previously-owned Group", "group", delName, "error", err)
			}
		}
	}

	// --- owned binding ---
	subjects := make([]v1alpha1.SubjectRef, 0)
	if groupRef != "" {
		subjects = append(subjects, v1alpha1.SubjectRef{Kind: "Group", Name: groupRef})
	}
	subjects = append(subjects, team.Spec.Subjects...)

	bindingName := "team-" + team.Name
	if !r.ensureOwned(ctx, "VarroaRoleBinding", bindingName, team) {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionRBACReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonBindingApplyFailed,
			Message: fmt.Sprintf("VarroaRoleBinding %q exists without Team ownership", bindingName),
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonChildApplyFailed,
			Message: fmt.Sprintf("failed to apply VarroaRoleBinding %q", bindingName),
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	}

	wantB := &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            bindingName,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: subjects,
			RoleRef:  role,
			Scope: &v1alpha1.VarroaRoleBindingScope{
				Namespaces: team.Spec.Namespaces,
			},
		},
	}

	if err := crdstore.Apply[v1alpha1.VarroaRoleBinding](ctx, r.store, wantB); err != nil {
		r.logger.Error("failed to apply VarroaRoleBinding", "binding", bindingName, "error", err)
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionRBACReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonBindingApplyFailed,
			Message: fmt.Sprintf("failed to apply VarroaRoleBinding %q: %v", bindingName, err),
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonChildApplyFailed,
			Message: fmt.Sprintf("failed to apply VarroaRoleBinding %q", bindingName),
		})
		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		}
		return reconcile.Result{}, nil
	}

	status.GroupRef = groupRef
	status.BindingRef = bindingName

	r.setCondition(status, v1alpha1.TeamCondition{
		Type:    v1alpha1.TeamConditionRBACReady,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.TeamReasonMaterialized,
		Message: "Group and VarroaRoleBinding applied",
	})

	// --- namespaces ---
	nsOK := true
	status.NamespaceStates = nil
	for _, ns := range team.Spec.Namespaces {
		st, err := r.reconcileNamespace(ctx, ns, team.Spec.ProvisionNamespaces, labels)
		nsState := v1alpha1.TeamNamespaceState{Name: ns, State: st}
		if err != nil || st == v1alpha1.TeamNamespaceStateMissing || st == v1alpha1.TeamNamespaceStateUnmanaged {
			nsOK = false
		}
		status.NamespaceStates = append(status.NamespaceStates, nsState)
	}

	if nsOK {
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionNamespacesReady,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.TeamReasonNamespacesSatisfied,
			Message: "All namespaces are satisfied",
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.TeamReasonReconciled,
			Message: "Team reconciled successfully",
		})
	} else {
		// Determine the specific reason for the NamespacesReady condition.
		nsReason := v1alpha1.TeamReasonNamespaceMissing
		for _, nsSt := range status.NamespaceStates {
			if nsSt.State == v1alpha1.TeamNamespaceStateUnmanaged {
				nsReason = v1alpha1.TeamReasonNamespaceUnmanaged
				break
			}
		}
		// If any had an error, use EnsureFailed
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionNamespacesReady,
			Status:  metav1.ConditionFalse,
			Reason:  nsReason,
			Message: "One or more namespaces are not satisfied",
		})
		r.setCondition(status, v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonNamespaceUnsatisfied,
			Message: "One or more namespaces are not satisfied",
		})
	}

	// --- pipelines placement pass (design D4) ---
	// A Team that never was and is not now a pipelines tenant skips this
	// whole block: no cell listing, no placement-pass call, no
	// RoleBinding/agent-identity sync. Every ordinary (non-pipelines) Team
	// on the fleet reconciles on the same periodic tick as a pipelines
	// tenant, so this guard is what keeps that tick cheap.
	// A tenant that owes an agent Service delete stays in this block even
	// after it stops being a pipelines tenant, because the drain below is
	// the only code that ends that debt.
	pipelinesTenant := team.Spec.Pipelines != nil || status.Placement != nil ||
		len(status.StaleAgentServiceNamespaces) > 0 ||
		containsString(team.Finalizers, v1alpha1.TeamFinalizerTenantPlacement)
	if pipelinesTenant {
		// Captured before Place mutates status: teardown's final tick
		// clears status.Pipelines in the same call that needs this value
		// to delete the tenant agent identity, so it must be read from the
		// pre-mutation status (or, while still an active pipelines tenant,
		// from spec).
		agentNamespace := ""
		if status.Pipelines != nil {
			agentNamespace = status.Pipelines.AgentNamespace
		}
		if team.Spec.Pipelines != nil {
			agentNamespace = team.Spec.Pipelines.AgentNamespace
		}
		// Captured for the same reason: teardown's final tick clears the
		// placement in the call that deletes the tenant's agent Service.
		priorCellNamespace := ""
		if status.Placement != nil {
			priorCellNamespace = status.Placement.CellNamespace
		}

		// Teardown deletes every tenant agent pod before the placement pass
		// runs, because the pass holds stage two on the count. The tenant
		// ServiceAccount goes in this tick too, and with it the cloud
		// credential, so no JVM can delete these pods after this point.
		agentPodsPending := false
		if teamTearingDown(team) && agentNamespace != "" {
			remaining, reapErr := r.reapTenantAgentPods(ctx, team.Name, agentNamespace, true)
			if reapErr != nil {
				r.logger.Error("tenant agent pod teardown reap failed", "team", team.Name,
					"namespace", agentNamespace, "error", reapErr)
			}
			agentPodsPending = remaining > 0 || reapErr != nil
		}

		cellCandidates, cellsByKey, err := r.listCellCandidates(ctx)
		if err != nil {
			r.logger.Error("failed to list cell candidates", "team", team.Name, "error", err)
		}
		// A failed pool list must fail closed. An empty Pools slice makes
		// every effective-owner lookup answer nil, so a cell owned by a live
		// single-tenant pool reads as unowned and admits a tenant that names
		// no pool. The tick therefore returns the error and requeues, and the
		// tenant keeps the placement it already holds.
		poolSummaries, err := r.listCellPools(ctx)
		if err != nil {
			r.logger.Error("failed to list cell pools", "team", team.Name, "error", err)
			return reconcile.Result{}, err
		}
		lastSync := r.lastSync(team.Name)
		webhook, webhookCond := r.resolveTeamWebhook(ctx, team)
		if webhookCond == nil {
			status.Conditions = removeTeamCondition(status.Conditions, v1alpha1.TeamConditionNotifications)
		} else {
			r.setCondition(status, *webhookCond)
		}
		placeInputs := cells.Inputs{
			Now:                time.Now,
			Webhook:            webhook,
			Cells:              cellCandidates,
			Pools:              poolSummaries,
			ProfileExists:      r.profileExists(ctx),
			ManagedNamespaces:  r.managedSet,
			RecordStoreEnabled: r.claimGate == "1",
			BlobsModeS3:        r.blobsModeS3(),
			MaxTenantsDefault:  int32(r.cellsConfig.MaxTenantsPerCell),
			ClusterIdentity:    r.clusterIdentity,
			RequestPurge:       r.requestPurge,
			Announce:           r.loggedAnnounce(),
			LastSync:           lastSync,
			// PendingJobs is the pending set of the tenant's slot-0 entry:
			// the deferred jobs and the jobs with no number seed. An
			// applied seed answers its instance, so only these jobs wait.
			// An entry that is not applied, or whose list overflowed,
			// answers no instance and holds every job.
			PendingJobs: cellRehydrationEntryPending(status, cellsByKey, team.Name),
			// MemberSeed reads each member cell's own rehydration entry,
			// so one member's deferred job never holds another member's
			// build.
			MemberSeed: memberSeedReader(cellsByKey, team.Name),
			// DepartedPods carries the pod reading of every member cell
			// the tenant still names that the candidate list no longer
			// holds. The pass reports such a member confirmed gone only
			// on a confirmed absence, and that reading is what arms the
			// takeover of the member's runs.
			DepartedPods:     r.departedMemberPods(ctx, status, cellsByKey),
			AgentPodsPending: agentPodsPending,
			// ConfirmedDeparted carries every unconfirmed dead instance of
			// the last announce reply whose pod is now gone. The read
			// repeats on every tick, so a cell the tenant left is read
			// until its old pod is gone.
			ConfirmedDeparted: r.confirmDepartedInstances(ctx, lastSync, time.Now()),
		}
		previousCell := ""
		if status.Placement != nil {
			previousCell = status.Placement.Cell
		}
		outcome, placeErr := cells.Place(ctx, placeInputs, team, status)
		if placeErr != nil {
			r.logger.Error("placement pass failed", "team", team.Name, "error", placeErr)
		}
		r.publishPlacementMoved(team.Name, previousCell, status.Placement)
		if outcome.SyncCacheUpdate != nil {
			r.storeSyncCache(team.Name, *outcome.SyncCacheUpdate)
		}
		r.setTenantWarningsCondition(status, team, cellsByKey)
		r.runRehydrationPass(status, team.Name, cellsByKey)

		// A move to a cell in another namespace leaves an agent Service
		// behind, under the same name, still selecting the old cell. The
		// move itself is not reversible here: the placement pass has already
		// announced the new generation to the recorder and requested the
		// purge of the old one. The pass therefore records the namespace to
		// clean as a marker, in the same patch that commits the placement,
		// and a later tick drains it.
		if status.Placement != nil && priorCellNamespace != "" &&
			priorCellNamespace != status.Placement.CellNamespace {
			setStaleAgentServiceNamespace(status, priorCellNamespace)
		}

		// The claim is the first write of a new placement, before the
		// fence below. The pool drain reads the fence set before it
		// re-reads a free cell, so a fence alone leaves this interleaving
		// open: the drain lists the cell, reads no fence, re-reads the
		// cell, this pass writes the fence, and the conditional delete
		// still succeeds over a tenant that is arriving. A failed claim
		// therefore ends the tick with no fence written.
		// Every member cell takes a claim, not slot 0's alone: a drain
		// that has already read the fence set finds a claim on each cell
		// this pass is binding.
		for _, claim := range outcome.CellClaims {
			if err := cells.WriteCellClaim(ctx, r.store, claim.Name, claim.Namespace, claim.Tenant, time.Now()); err != nil {
				r.logger.Error("failed to claim the cell", "team", team.Name,
					"cell", claim.Namespace+"/"+claim.Name, "error", err)
				return reconcile.Result{}, err
			}
		}

		if err := r.patchStatus(ctx, team.Name, status); err != nil {
			r.logger.Error("failed to patch status", "team", team.Name, "error", err)
			return reconcile.Result{}, err
		}

		// Every write the placement pass named is applied only after the
		// status patch succeeds (task 6.8): the status patch is the
		// durable record of intent, and every write below is either
		// idempotent (a cell patch, a finalizer patch, the pipelines
		// RoleBinding/agent-identity sync) or a best-effort side effect
		// (the activity events).
		settled := map[string]bool{}
		for _, cp := range outcome.CellPatches {
			cellStatus := &v1alpha1.CellStatus{Tenants: cp.Tenants}
			if err := r.client.PatchControllerCellStatus(ctx, cp.Name, cp.Namespace, cellStatus); err != nil {
				r.logger.Error("failed to patch cell tenant list", "cell", cp.Namespace+"/"+cp.Name, "error", err)
				continue
			}
			settled[cp.Namespace+"/"+cp.Name] = true
		}
		for _, claim := range outcome.CellClaims {
			if !settled[claim.Namespace+"/"+claim.Name] {
				continue
			}
			// Only a tenant patch that succeeded retires that cell's
			// claim: the tenant list is the durable record the claim
			// stands in for. A refused patch keeps the claim, and the
			// next tick writes it again. A failed release costs one claim
			// window of drain protection on a cell the drain must skip
			// anyway.
			if err := cells.ReleaseCellClaim(ctx, r.store, claim.Name, claim.Namespace, claim.Tenant, time.Now()); err != nil {
				r.logger.Error("failed to retire the cell claim", "team", team.Name,
					"cell", claim.Namespace+"/"+claim.Name, "error", err)
			}
		}
		// The children sync runs before the finalizer change, because the
		// finalizer is what keeps this whole block reachable. A tenant whose
		// agent Service delete is still owed, and whose finalizer went
		// first, would never reach the drain again: its spec.pipelines and
		// its placement are both gone by then.
		staleBefore := len(status.StaleAgentServiceNamespaces)
		childErr := r.syncPipelinesChildren(ctx, team, status, subjects, role, agentNamespace, priorCellNamespace, cellsByKey)
		for _, ev := range outcome.ActivityEvents {
			r.publishTenantEvent(ev)
		}
		releaseFinalizer := outcome.FinalizerRelease && childErr == nil &&
			len(status.StaleAgentServiceNamespaces) == 0
		if outcome.FinalizerAdd || releaseFinalizer {
			r.applyFinalizerChange(ctx, team, outcome.FinalizerAdd, releaseFinalizer)
		}
		if releaseFinalizer {
			// Teardown converged: drop the dedup/resync clock so a Team
			// name that is reused later starts with no stale cache entry.
			r.syncCacheMu.Lock()
			delete(r.syncCache, team.Name)
			r.syncCacheMu.Unlock()
			r.forgetTenantAgentReap(team.Name)
		}
		if childErr != nil {
			// The failure leaves status.placement alone: the tenant keeps
			// its cell and the next tick retries. The condition needs a
			// second patch, because the status write above already ran. That
			// patch also carries the namespaces the drain did clear. On a
			// teardown tick the condition reappears on a Team that already
			// dropped it, which is correct: teardown is not complete while a
			// Service of this tenant still exists.
			r.setCondition(status, v1alpha1.TeamCondition{
				Type:    v1alpha1.TeamConditionPlaced,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.TeamReasonAgentServiceFailed,
				Message: childErr.Error(),
			})
			if patchErr := r.patchStatus(ctx, team.Name, status); patchErr != nil {
				r.logger.Error("failed to patch status", "team", team.Name, "error", patchErr)
			}
			return reconcile.Result{}, childErr
		}
		if staleBefore != len(status.StaleAgentServiceNamespaces) {
			// The drain removed at least one namespace. Only this second
			// patch makes that durable, so a later tick does not delete
			// again.
			if patchErr := r.patchStatus(ctx, team.Name, status); patchErr != nil {
				r.logger.Error("failed to patch status", "team", team.Name, "error", patchErr)
			}
		}
		return reconcile.Result{}, nil
	}

	if err := r.patchStatus(ctx, team.Name, status); err != nil {
		r.logger.Error("failed to patch status", "team", team.Name, "error", err)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// setCondition upserts a condition on the team status.
func (r *TeamReconciler) setCondition(status *v1alpha1.TeamStatus, cond v1alpha1.TeamCondition) {
	for i, existing := range status.Conditions {
		if existing.Type == cond.Type {
			if existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
				cond.LastTransitionTime = existing.LastTransitionTime
			}
			status.Conditions[i] = cond
			return
		}
	}
	status.Conditions = append(status.Conditions, cond)
}

// runRehydrationPass calls cells.RehydrationPass (design D13, tasks 4.5
// and 6.5) with the tenant's owning cell instance and its own
// Controller.status.cell.rehydration entry, both read from the same
// cellsByKey snapshot listCellCandidates already built this tick.
func (r *TeamReconciler) runRehydrationPass(status *v1alpha1.TeamStatus, teamName string, cellsByKey map[string]*v1alpha1.Controller) cells.RehydrationOutcome {
	cellInstance := ""
	if status.Placement != nil {
		if cell := cellsByKey[teamCellKey(status.Placement.CellNamespace, status.Placement.Cell)]; cell != nil && cell.Status.Cell != nil {
			cellInstance = cell.Status.Cell.Instance
		}
	}
	entry := cellRehydrationEntryFor(status, cellsByKey, teamName)
	return cells.RehydrationPass(status, cells.RehydrationInputs{CellInstance: cellInstance, Entry: entry})
}

// cellRehydrationEntryFor finds teamName's own entry in the bound cell's
// Controller.status.cell.rehydration list, from the same cellsByKey
// snapshot listCellCandidates already built this tick. Nil when the
// tenant is unplaced, its cell is missing from the snapshot, or the cell
// carries no entry for it yet.
func cellRehydrationEntryFor(status *v1alpha1.TeamStatus, cellsByKey map[string]*v1alpha1.Controller, teamName string) *v1alpha1.CellRehydrationEntry {
	if status.Placement == nil {
		return nil
	}
	cell := cellsByKey[teamCellKey(status.Placement.CellNamespace, status.Placement.Cell)]
	if cell == nil || cell.Status.Cell == nil {
		return nil
	}
	for i := range cell.Status.Cell.Rehydration {
		if cell.Status.Cell.Rehydration[i].Tenant == teamName {
			return &cell.Status.Cell.Rehydration[i]
		}
	}
	return nil
}

// memberSeedReader answers one member cell's own seed state for teamName,
// read from that cell's status.cell.rehydration entry through seedAnswerOf.
// A cell with no entry, or one whose bundle is not applied, answers an
// empty instance, which holds every build on that member until its seed
// lands. An applied entry answers its pending jobs, which alone wait.
func memberSeedReader(cellsByKey map[string]*v1alpha1.Controller, teamName string) func(string, string) (string, []string) {
	return func(namespace, cellName string) (string, []string) {
		cell := cellsByKey[teamCellKey(namespace, cellName)]
		if cell == nil || cell.Status.Cell == nil {
			return "", nil
		}
		for i := range cell.Status.Cell.Rehydration {
			entry := &cell.Status.Cell.Rehydration[i]
			if entry.Tenant != teamName {
				continue
			}
			answer := seedAnswerOf(entry)
			return answer.instance, answer.pending
		}
		return "", nil
	}
}

// departedMemberPods reads the Jenkins pod of every member cell the tenant
// still names that the candidate list no longer holds, and answers whether
// each of those pods is gone.
//
// The placement pass performs no Kubernetes read, so the reading is taken
// here. It is the evidence the pass needs to report such a member confirmed
// gone. The pass alone can only see that the cell left the candidate list,
// which a scoped-RBAC change or a list error also produces.
//
// The read count is bounded by the tenant's member count, and only a
// departed member costs a read.
func (r *TeamReconciler) departedMemberPods(ctx context.Context, status *v1alpha1.TeamStatus,
	cellsByKey map[string]*v1alpha1.Controller) map[string]bool {
	if status.Placement == nil {
		return nil
	}
	out := map[string]bool{}
	for _, ref := range memberCellRefs(status.Placement) {
		key := teamCellKey(ref.namespace, ref.name)
		if _, live := cellsByKey[key]; live {
			continue
		}
		if _, read := out[key]; read {
			continue
		}
		out[key] = r.cellPodGone(ctx, ref.namespace, ref.name)
	}
	return out
}

// memberCellRefs lists the cells the tenant's members hold. A placement
// with an empty member list describes one slot-0 member on its scalar
// cell.
func memberCellRefs(placement *v1alpha1.TeamPlacementStatus) []struct{ namespace, name string } {
	var out []struct{ namespace, name string }
	add := func(namespace, name string) {
		if name == "" {
			return
		}
		out = append(out, struct{ namespace, name string }{namespace, name})
	}
	add(placement.CellNamespace, placement.Cell)
	for _, m := range placement.Members {
		add(m.CellNamespace, m.Cell)
	}
	return out
}

// cellPodGone reports whether one cell's Jenkins pod is confirmed absent.
// It applies updateCellInstance's own rule: no pod, or a pod in phase
// Failed or Succeeded, is an absence, and a running pod is not.
//
// Every read error answers false, so a transient API failure never
// confirms a member gone. A deleted Controller answers false too: the
// Controller can go while its pod still drains, and that pod can still
// serve builds. confirmDepartedInstances confirms such an instance once
// no pod with its UID runs.
func (r *TeamReconciler) cellPodGone(ctx context.Context, namespace, name string) bool {
	pod, err := r.client.GetControllerPod(ctx, namespace, name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			r.logger.Warn("read a departed member cell's pod",
				"cell", teamCellKey(namespace, name), "error", err)
		}
		return false
	}
	if pod == nil {
		return true
	}
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// confirmDepartedInstances reads the pods of every namespace that holds an
// unconfirmed dead instance of last, and answers the instances whose pod
// is gone, each confirmed at now. A pod is gone when no pod in the
// namespace carries the instance's UID, or when that pod is in phase
// Failed or Succeeded. A pod UID never comes back, so a pod of the same
// name with a new UID also proves the instance gone.
//
// A failed list confirms nothing for its namespace. The read costs one
// list for each namespace, and only while an unconfirmed entry exists.
func (r *TeamReconciler) confirmDepartedInstances(ctx context.Context, last *cells.SyncCache, now time.Time) []cells.DeadInstance {
	if last == nil || len(last.Unconfirmed) == 0 {
		return nil
	}
	phases := map[string]map[types.UID]corev1.PodPhase{}
	failed := map[string]bool{}
	var out []cells.DeadInstance
	for _, d := range last.Unconfirmed {
		if d.Instance == "" || d.Namespace == "" || d.ConfirmedGoneAt != nil || failed[d.Namespace] {
			continue
		}
		pods, read := phases[d.Namespace]
		if !read {
			var err error
			pods, err = r.client.ListPodPhases(ctx, d.Namespace)
			if err != nil {
				r.logger.Warn("read the pods of a departed instance's namespace",
					"namespace", d.Namespace, "instance", d.Instance, "error", err)
				failed[d.Namespace] = true
				continue
			}
			phases[d.Namespace] = pods
		}
		phase, present := pods[types.UID(d.Instance)]
		if present && phase != corev1.PodFailed && phase != corev1.PodSucceeded {
			continue
		}
		confirmed := now
		entry := d
		entry.ConfirmedGoneAt = &confirmed
		r.logger.Info("confirmed a departed cell instance gone",
			"cell", teamCellKey(d.Namespace, d.Cell), "instance", d.Instance)
		out = append(out, entry)
	}
	return out
}

// cellRehydrationEntryPending answers the pending job paths that teamName's
// own cell entry announces, or nil when no entry exists yet (task 5.7a).
func cellRehydrationEntryPending(status *v1alpha1.TeamStatus, cellsByKey map[string]*v1alpha1.Controller, teamName string) []string {
	entry := cellRehydrationEntryFor(status, cellsByKey, teamName)
	if entry == nil {
		return nil
	}
	return seedAnswerOf(entry).pending
}

// patchStatus patches the status subresource of a Team.
func (r *TeamReconciler) patchStatus(ctx context.Context, name string, status *v1alpha1.TeamStatus) error {
	return crdstore.PatchStatus[v1alpha1.Team](ctx, r.store, name, "", status)
}

// teamOwnerRef returns an OwnerReference for a Team.
func teamOwnerRef(t *v1alpha1.Team) metav1.OwnerReference {
	controller := true
	apiVersion := t.APIVersion
	kind := t.Kind
	if apiVersion == "" {
		apiVersion = v1alpha1.SchemeGroupVersion.String()
	}
	if kind == "" {
		kind = "Team"
	}
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               t.Name,
		UID:                t.UID,
		BlockOwnerDeletion: &controller,
		Controller:         &controller,
	}
}

// defaultRoleRef returns the default role ref when empty.
func defaultRoleRef(s string) string {
	if s == "" {
		return "developer"
	}
	return s
}

// teamChildLabels returns the labels to apply to owned child resources.
func teamChildLabels(t *v1alpha1.Team) map[string]string {
	return map[string]string{
		v1alpha1.LabelManagedBy: "team",
		v1alpha1.LabelTeamName:  t.Name,
	}
}

// ensureOwned checks whether a resource with the given name is safe to manage
// as a child of the Team. It returns true if the resource does not exist
// (free to create), or if it already has a Team ownership marker.
// Returns false if the resource exists without ownership (hand-authored collision).
func (r *TeamReconciler) ensureOwned(ctx context.Context, kind, name string, team *v1alpha1.Team) bool {
	var existing metav1.Object
	var err error

	switch kind {
	case "Group":
		var g *v1alpha1.Group
		g, err = crdstore.Get[v1alpha1.Group](ctx, r.store, name, "")
		if err == nil {
			existing = g
		}
	case "VarroaRoleBinding":
		var b *v1alpha1.VarroaRoleBinding
		b, err = crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, r.store, name, "")
		if err == nil {
			existing = b
		}
	default:
		return false
	}

	if apierrors.IsNotFound(err) {
		return true // free to create
	}
	if err != nil {
		return false // treat as not safe
	}

	// Check owner reference or label-based ownership.
	for _, or := range existing.GetOwnerReferences() {
		if or.UID == team.UID {
			return true
		}
	}

	lbls := existing.GetLabels()
	if lbls[v1alpha1.LabelManagedBy] == "team" && lbls[v1alpha1.LabelTeamName] == team.Name {
		return true
	}

	return false // hand-authored collision
}

// reconcileNamespace reconciles a single namespace for a Team using the
// tenancy helper.
func (r *TeamReconciler) reconcileNamespace(ctx context.Context, ns string, provision bool, labels map[string]string) (string, error) {
	managedSet := r.managedSet

	if !provision {
		// Precheck path: classify only, never creates.
		state, err := tenancy.Classify(ctx, r.client.(tenancy.NamespaceReader), managedSet, ns)
		if err != nil {
			return "", err
		}
		switch state {
		case tenancy.NamespaceReady:
			return v1alpha1.TeamNamespaceStateManaged, nil
		case tenancy.NamespaceMissing:
			return v1alpha1.TeamNamespaceStateMissing, nil
		case tenancy.NamespaceUnmanaged:
			return v1alpha1.TeamNamespaceStateUnmanaged, nil
		}
		return "", fmt.Errorf("unexpected namespace state: %s", state)
	}

	// Provisioning path: create or merge-patch labels.
	classifier := tenancy.NewClassifier(r.client.(tenancy.NamespaceClient), managedSet)
	result, err := classifier.Ensure(ctx, ns, labels)
	if err != nil {
		return "", err
	}
	// Ensure's postcondition is always Ready or Unmanaged, never Missing (tenancy.go
	// EnsureResult doc) — sync whenever the namespace is usable, covering both the
	// just-Created and the already-Managed-but-still-missing-the-secret cases. Must run
	// before the Created early-return below, or first-creation never syncs.
	if result.State == tenancy.NamespaceReady {
		r.syncImagePullSecrets(ctx, ns)
	}
	if result.Created {
		return v1alpha1.TeamNamespaceStateCreated, nil
	}
	switch result.State {
	case tenancy.NamespaceReady:
		return v1alpha1.TeamNamespaceStateManaged, nil
	case tenancy.NamespaceUnmanaged:
		return v1alpha1.TeamNamespaceStateUnmanaged, nil
	}
	return "", fmt.Errorf("unexpected ensure state: %s", result.State)
}

// Ensure TeamReconciler implements reconcile.Reconciler.
var _ reconcile.Reconciler = (*TeamReconciler)(nil)

// syncImagePullSecrets copies each imagePullSecret from
// ProvisioningDefaults.Spec.ImagePullSecrets into the given tenant namespace.
// Runs on every Team's every 30s tick (self-healing, idempotently converged).
// Failures are warn-logged only and never flip TeamConditionReady to False.
func (r *TeamReconciler) syncImagePullSecrets(ctx context.Context, ns string) {
	defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, r.store, provisioningDefaultsName, "")
	if err != nil || defaults == nil {
		return
	}
	for _, name := range defaults.Spec.ImagePullSecrets {
		if err := r.client.CopyImagePullSecret(ctx, r.operatorNamespace, ns, name); err != nil {
			r.logger.Warn("failed to sync image pull secret into tenant namespace",
				"secret", name, "namespace", ns, "error", err)
		}
	}
}

// --- pipelines placement pass wiring (design D4) ---

// blobsModeS3 reports whether VARROA_BLOBSTORE_URL resolves to the s3
// blob tier (refusal gate 2).
func (r *TeamReconciler) blobsModeS3() bool {
	params, parsed := parseBlobstoreURL(r.blobstoreURL)
	return parsed && params.IsS3
}

// profileExists returns a closure testing whether a named
// JenkinsVersionProfile exists (refusal gate 3).
func (r *TeamReconciler) profileExists(ctx context.Context) func(name string) bool {
	return func(name string) bool {
		if name == "" {
			return false
		}
		_, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, r.store, name, "")
		return err == nil
	}
}

// teamCellKey is the map key listCellCandidates and
// setTenantWarningsCondition share to find a Controller by namespace and
// name.
func teamCellKey(namespace, name string) string {
	return namespace + "/" + name
}

// timeFromMetaV1 converts a *metav1.Time to a *time.Time, answering nil for
// nil (design D6's cell_last_seen_at reads MiteStatus.LastSeen this way).
func timeFromMetaV1(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	tt := t.Time
	return &tt
}

// listCellCandidates lists every cell Controller (spec.cell.enabled) once
// per tick and converts each into a cells.CellCandidate. It also returns the
// raw Controllers keyed by teamCellKey, so setTenantWarningsCondition can
// read status.cell.tenantWarnings — a field cells.CellCandidate does not
// carry, because Place never needs it. It also lists JenkinsVersionProfile
// CRDs once per tick, so buildCellCandidate can resolve a profile for a
// cell that carries no warm-pool label.
func (r *TeamReconciler) listCellCandidates(ctx context.Context) ([]cells.CellCandidate, map[string]*v1alpha1.Controller, error) {
	all, err := crdstore.List[v1alpha1.Controller](ctx, r.store, "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("list controllers: %w", err)
	}
	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, r.store, "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("list jenkins version profiles: %w", err)
	}
	var out []cells.CellCandidate
	byKey := make(map[string]*v1alpha1.Controller)
	for _, cr := range all {
		if !v1alpha1.IsCell(cr) {
			continue
		}
		byKey[teamCellKey(cr.Namespace, cr.Name)] = cr
		out = append(out, buildCellCandidate(cr, profiles))
	}
	return out, byKey, nil
}

// listCellPools reads every live CellPool once per tick, beside the cell
// list. Place resolves each cell's varroa.dev/cell-pool label against this
// set, so a label naming a deleted pool gives no effective owner.
func (r *TeamReconciler) listCellPools(ctx context.Context) ([]cells.CellPoolSummary, error) {
	pools, err := crdstore.List[v1alpha1.CellPool](ctx, r.store, "", "")
	if err != nil {
		return nil, fmt.Errorf("list cell pools: %w", err)
	}
	out := make([]cells.CellPoolSummary, 0, len(pools))
	for _, pool := range pools {
		out = append(out, cells.SummarizeCellPool(pool))
	}
	return out, nil
}

// buildCellCandidate converts one cell Controller's live status into the
// read-only snapshot cells.Place consumes. profiles resolves the cell's
// spec.version to a profile name when the warm-pool label is absent.
func buildCellCandidate(cr *v1alpha1.Controller, profiles []*v1alpha1.JenkinsVersionProfile) cells.CellCandidate {
	cand := cells.CellCandidate{
		Name:      cr.Name,
		Namespace: cr.Namespace,
		Enabled:   true,
		Pool:      cr.Labels[cells.LabelCellPool],
	}
	if cr.Spec.Cell != nil {
		cand.MaxTenants = cr.Spec.Cell.MaxTenants
	}
	if cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady); cond != nil {
		cand.CellReady = cond.Status == metav1.ConditionTrue
	}
	if cr.Status.Cell != nil {
		cand.Evacuate = cr.Status.Cell.Evacuate
		cand.Tenants = append([]string(nil), cr.Status.Cell.Tenants...)
		cand.AppliedTenants = append([]string(nil), cr.Status.Cell.AppliedTenants...)
		cand.Instance = cr.Status.Cell.Instance
		cand.PreviousInstance = cr.Status.Cell.PreviousInstance
		cand.PreviousInstanceLastSeen = timeFromMetaV1(cr.Status.Cell.PreviousInstanceLastSeen)
	}
	if cr.Status.MiteStatus != nil {
		cand.MiteConnected = cr.Status.MiteStatus.Connected
		cand.MiteLastSeen = timeFromMetaV1(cr.Status.MiteStatus.LastSeen)
	}
	for _, pd := range cr.Status.PendingItemDeletions {
		cand.PendingDeletionPaths = append(cand.PendingDeletionPaths, pd.Path)
	}
	if label := cr.Labels[cells.LabelCellProfile]; label != "" {
		// cells.LabelCellProfile is the profile identity the warm pool
		// (D5) stamps at cell creation and never updates; the placement
		// pass reads the same label rather than re-deriving a profile
		// from spec.version, so both halves of the cells feature agree
		// on what a cell serves. The label, when present, wins.
		cand.ResolvedProfile = label
	} else if profile, _ := ResolveProfile(cr.Spec.Version, profiles); profile != nil {
		// A cell created without the warm-pool label (a hand-made cell,
		// not one the warm pool created) resolves its profile the same
		// way resolveBundleForController does, through the exact ->
		// LTS-line -> baseline ladder, so it still becomes a placement
		// candidate.
		cand.ResolvedProfile = profile.Name
	}
	return cand
}

// tenantWarningPriority orders the TenantWarnings condition's Reason
// (tenant-placement spec): the lowest rank wins.
var tenantWarningPriority = map[string]int{
	v1alpha1.TeamReasonItemsHeld:          0,
	v1alpha1.TeamReasonItemsApplyFailed:   1,
	v1alpha1.TeamReasonBundleKeysIgnored:  2,
	v1alpha1.TeamReasonPodTemplateSkipped: 3,
	v1alpha1.TeamReasonCredentialSkipped:  4,
	v1alpha1.TeamReasonHibernationIgnored: 5,
}

// foldedWarning is one distinct tenant warning and the namespace/name keys
// of the cells that report it.
type foldedWarning struct {
	w     v1alpha1.TenantWarning
	cells []string
}

// setTenantWarningsCondition folds the status.cell.tenantWarnings entries
// that name this Team, from every member cell (active and standby), into
// the Team's TenantWarnings condition (task 6.14, design D4). Each distinct
// warning appears once. When more than one cell contributes, each line
// names the cells that report it. TeamReconciler stays the only writer of
// every Team condition; the controller reconciler discovers warnings and
// hands them off through Controller.status.cell.tenantWarnings because it
// may never write to a Team.
func (r *TeamReconciler) setTenantWarningsCondition(status *v1alpha1.TeamStatus, team *v1alpha1.Team, cellsByKey map[string]*v1alpha1.Controller) {
	if status.Placement == nil {
		status.Conditions = removeTeamCondition(status.Conditions, v1alpha1.TeamConditionTenantWarnings)
		return
	}
	var folded []foldedWarning
	index := map[string]int{}
	contributing := map[string]bool{}
	for _, ref := range v1alpha1.PlacementCells(status.Placement) {
		cell := cellsByKey[teamCellKey(ref.Namespace, ref.Name)]
		if cell == nil || cell.Status.Cell == nil {
			continue
		}
		for _, w := range cell.Status.Cell.TenantWarnings {
			if w.Tenant != team.Name {
				continue
			}
			contributing[teamCellKey(ref.Namespace, ref.Name)] = true
			key := w.Reason + "\x00" + w.Message
			i, ok := index[key]
			if !ok {
				i = len(folded)
				index[key] = i
				folded = append(folded, foldedWarning{w: w})
			}
			cellKey := teamCellKey(ref.Namespace, ref.Name)
			if !slices.Contains(folded[i].cells, cellKey) {
				folded[i].cells = append(folded[i].cells, cellKey)
			}
		}
	}

	// The entries carry no timestamp, so a stable sort over the cell order
	// is the only deterministic order across ticks (tenant-placement spec).
	sort.SliceStable(folded, func(i, j int) bool { return folded[i].w.Reason < folded[j].w.Reason })

	cond := v1alpha1.TeamCondition{Type: v1alpha1.TeamConditionTenantWarnings, Status: metav1.ConditionFalse}
	if len(folded) > 0 {
		cond.Status = metav1.ConditionTrue
		bestRank := len(tenantWarningPriority)
		lines := make([]string, 0, len(folded))
		for _, f := range folded {
			if rank, ok := tenantWarningPriority[f.w.Reason]; ok && rank < bestRank {
				bestRank = rank
				cond.Reason = f.w.Reason
			}
			line := fmt.Sprintf("%s: %s", f.w.Reason, f.w.Message)
			if len(contributing) > 1 {
				names := make([]string, 0, len(f.cells))
				for _, k := range f.cells {
					names = append(names, k[strings.LastIndex(k, "/")+1:])
				}
				line = strings.Join(names, ", ") + ": " + line
			}
			lines = append(lines, line)
		}
		cond.Message = capBytes(strings.Join(lines, "; "), 2048)
	}
	r.setCondition(status, cond)
}

// resolveTeamWebhook reads the completion webhook a Team declares and
// answers the signing key the record store needs. It answers a nil
// webhook when the Team declares none, and a nil condition in that case,
// which tells the caller to drop the Notifications condition. A Secret
// that is missing, or that holds no value under the named key, answers a
// nil webhook with a False condition. A read error counts as a missing
// Secret, and the next tick reads it again.
func (r *TeamReconciler) resolveTeamWebhook(ctx context.Context, team *v1alpha1.Team) (*cells.WebhookSync, *v1alpha1.TeamCondition) {
	if team.Spec.Pipelines == nil || team.Spec.Pipelines.Notifications == nil ||
		team.Spec.Pipelines.Notifications.Webhook == nil {
		return nil, nil
	}
	hook := team.Spec.Pipelines.Notifications.Webhook
	key := hook.SecretRef.Key
	if key == "" {
		key = v1alpha1.TeamWebhookDefaultKey
	}
	missing := func(msg string) (*cells.WebhookSync, *v1alpha1.TeamCondition) {
		return nil, &v1alpha1.TeamCondition{
			Type:    v1alpha1.TeamConditionNotifications,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.TeamReasonWebhookSecretMissing,
			Message: msg,
		}
	}
	data, err := r.client.GetSecret(ctx, hook.SecretRef.Name, team.Spec.Pipelines.AgentNamespace)
	if err != nil {
		r.logger.Warn("tenant webhook secret read failed", "team", team.Name,
			"secret", hook.SecretRef.Name, "error", err)
		return missing(fmt.Sprintf("secret %s is not readable", hook.SecretRef.Name))
	}
	value := data[key]
	if len(value) == 0 {
		return missing(fmt.Sprintf("secret %s has no value under key %s", hook.SecretRef.Name, key))
	}
	return &cells.WebhookSync{URL: hook.URL, Key: string(value)}, &v1alpha1.TeamCondition{
		Type:    v1alpha1.TeamConditionNotifications,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.TeamReasonWebhookReady,
		Message: "completion webhook is configured",
	}
}

// removeTeamCondition drops every condition of the given type.
func removeTeamCondition(conds []v1alpha1.TeamCondition, condType string) []v1alpha1.TeamCondition {
	out := make([]v1alpha1.TeamCondition, 0, len(conds))
	for _, c := range conds {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	return out
}

// capBytes trims s to at most max bytes without splitting a UTF-8 rune.
func capBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	b := []byte(s)[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// applyFinalizerChange patches metadata.finalizers when the placement pass's
// Outcome asks for an add or a release.
func (r *TeamReconciler) applyFinalizerChange(ctx context.Context, team *v1alpha1.Team, add, release bool) {
	finalizers := append([]string(nil), team.Finalizers...)
	changed := false
	if add && !containsString(finalizers, v1alpha1.TeamFinalizerTenantPlacement) {
		finalizers = append(finalizers, v1alpha1.TeamFinalizerTenantPlacement)
		changed = true
	}
	if release {
		next := removeString(finalizers, v1alpha1.TeamFinalizerTenantPlacement)
		if len(next) != len(finalizers) {
			changed = true
		}
		finalizers = next
	}
	if !changed {
		return
	}
	if err := crdstore.PatchFinalizers[v1alpha1.Team](ctx, r.store, team.Name, "", finalizers); err != nil {
		r.logger.Error("failed to patch team finalizers", "team", team.Name, "error", err)
	}
}

// pipelinesRoleBindingName, tenantAgentServiceAccountName,
// tenantAgentTokenSecretName, and tenantCloudCredentialID are the placement
// pass's naming contract with the tenant-items batch that creates the
// ServiceAccount and its token Secret (design D7/D8/D11): task 6.9's
// teardown deletes exactly these names, and the controller reconciler reads
// them by the same names.
func pipelinesRoleBindingName(team string) string      { return "team-" + team + "-pipelines" }
func tenantAgentServiceAccountName(team string) string { return "varroa-agent-" + team }
func tenantAgentTokenSecretName(team string) string    { return "varroa-agent-" + team + "-token" }
func tenantCloudCredentialID(team string) string       { return "varroa-cloud-" + team }

// tenantAgentServiceName returns the agent Service of one slot of a tenant.
// A tenant holds one slot for each active member, and a cell holds at most
// one slot of a tenant. This release writes slot 0 only.
func tenantAgentServiceName(team string, slot int32) string {
	return fmt.Sprintf("varroa-agents-%s-%d", team, slot)
}

// teamTearingDown reports whether a pipelines tenant tears down: its
// spec.pipelines is gone, or the Team is deleted.
func teamTearingDown(team *v1alpha1.Team) bool {
	return team.Spec.Pipelines == nil || (team.DeletionTimestamp != nil && !team.DeletionTimestamp.IsZero())
}

// tenantServiceWriter is implemented by ResourceClient backends that can
// write a tenant's agent Service. It is checked with a type assertion rather
// than added to the ResourceClient interface: that interface has
// explicit-method fakes outside this package, and every one would need a new
// stub the moment this package widened it.
type tenantServiceWriter interface {
	CreateOrUpdateTenantService(ctx context.Context, name, namespace, selectorApp string) error
}

// syncPipelinesChildren applies or deletes the objects task 9.2 and task
// 6.9 name: the team-<team>-pipelines JenkinsRoleBinding and the slot 0
// agent Service while the tenant is placed, and (during teardown) the
// tenant agent ServiceAccount and its token Secret. Re-run
// every tick; every branch is idempotent. It also drains the list of agent
// Services the tenant still owns, on both branches, and a teardown adds its
// own cell namespace to that list first. priorCellNamespace is the
// placement's cell namespace read before the placement pass mutated the
// status, because teardown's final tick clears the placement in the same
// call that needs the value. This function answers an error for a failed
// drain and for a failed agent Service write, which the caller reports on
// the Placed condition.
func (r *TeamReconciler) syncPipelinesChildren(ctx context.Context, team *v1alpha1.Team, status *v1alpha1.TeamStatus, subjects []v1alpha1.SubjectRef, role, agentNamespace, priorCellNamespace string, cellsByKey map[string]*v1alpha1.Controller) error {
	tearingDown := teamTearingDown(team)
	bindingName := pipelinesRoleBindingName(team.Name)

	// The steady-state reaper covers every case where no live JVM holds
	// the slot's cloud, so the plugin's own garbage collection never runs
	// for that pod. A reap failure never fails the tick: the pods cost
	// capacity, not correctness, and the next list retries.
	if !tearingDown && agentNamespace != "" && r.reapTenantAgentPodsDue(team.Name, time.Now()) {
		if _, err := r.reapTenantAgentPods(ctx, team.Name, agentNamespace, false); err != nil {
			r.logger.Warn("tenant agent pod reap failed", "team", team.Name,
				"namespace", agentNamespace, "error", err)
		}
	}
	var serviceErr error

	// A teardown owes the Service of the namespace the tenant is leaving,
	// the same way a move owes the one it left. Both debts go on the one
	// list, so the finalizer release waits for every delete and a refused
	// one is retried rather than warned about.
	if tearingDown {
		namespace := priorCellNamespace
		if status.Placement != nil {
			namespace = status.Placement.CellNamespace
		}
		if namespace != "" {
			setStaleAgentServiceNamespace(status, namespace)
		}
	}

	// The drain runs on both branches, and before the slot 0 upsert: the
	// Service of the namespace a move left carries the same name as the one
	// this tick writes, so a failed drain must stop the upsert rather than
	// race it.
	if err := r.drainStaleAgentServices(ctx, team.Name, status); err != nil {
		r.logger.Error("failed to delete an agent Service the tenant still owns",
			"team", team.Name, "error", err)
		return err
	}

	if !tearingDown && status.Placement != nil {
		want := &v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:            bindingName,
				Labels:          teamChildLabels(team),
				OwnerReferences: []metav1.OwnerReference{teamOwnerRef(team)},
			},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				Subjects: subjects,
				RoleRef:  role,
				JenkinsScope: &v1alpha1.JenkinsScope{
					Type:      "Folder",
					Folder:    team.Name,
					Propagate: "Subtree",
				},
				ControllerScope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces:         []string{status.Placement.CellNamespace},
					ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelControllerName: status.Placement.Cell}},
				},
			},
		}
		if err := crdstore.Apply[v1alpha1.JenkinsRoleBinding](ctx, r.store, want); err != nil {
			r.logger.Error("failed to apply pipelines JenkinsRoleBinding", "team", team.Name, "error", err)
		}
		if agentNamespace != "" {
			r.ensureTenantAgentIdentity(ctx, team.Name, agentNamespace)
		}
		serviceErr = r.reconcileTenantAgentServices(ctx, team.Name, team.Status.Placement, status, cellsByKey)
	} else {
		if err := crdstore.Delete[v1alpha1.JenkinsRoleBinding](ctx, r.store, bindingName, ""); err != nil {
			r.logger.Warn("failed to delete pipelines JenkinsRoleBinding", "team", team.Name, "error", err)
		}
	}

	if tearingDown && agentNamespace != "" {
		saName := tenantAgentServiceAccountName(team.Name)
		secretName := tenantAgentTokenSecretName(team.Name)
		if err := r.client.DeleteResource(ctx, "ServiceAccount", saName, agentNamespace); err != nil {
			r.logger.Warn("failed to delete tenant agent ServiceAccount", "team", team.Name, "error", err)
		}
		if err := r.client.DeleteSecret(ctx, secretName, agentNamespace); err != nil {
			r.logger.Warn("failed to delete tenant agent token secret", "team", team.Name, "error", err)
		}
		if err := r.client.DeleteResource(ctx, "Role", saName, agentNamespace); err != nil {
			r.logger.Warn("failed to delete tenant agent Role", "team", team.Name, "error", err)
		}
		if err := r.client.DeleteResource(ctx, "RoleBinding", saName, agentNamespace); err != nil {
			r.logger.Warn("failed to delete tenant agent RoleBinding", "team", team.Name, "error", err)
		}
	}
	return serviceErr
}

// tenantSlotZero is the slot the scalar placement fields describe. A
// tenant with no member list holds this slot alone.
const tenantSlotZero = int32(0)

// maxTenantSlotSweep bounds the slot indexes a namespace drain deletes a
// Service for. It is the CRD maximum of spec.pipelines.members, so a
// namespace a move left loses every slot's Service whatever the tenant
// held there.
const maxTenantSlotSweep = int32(16)

// reconcileTenantAgentServices converges one Service for each active slot
// of the tenant, and deletes the Service of each slot a scale-down
// removed. A standby holds no Service of its own: it takes the Service of
// the slot it is promoted into, through the selector flip the promotion
// performs.
//
// The delete set is the slots the tenant held before this tick and holds
// no longer, so a steady tenant costs no delete call at all.
//
// It reports the first failure. The caller turns that into Placed=False
// with reason AgentServiceFailed and retries on a later tick.
func (r *TeamReconciler) reconcileTenantAgentServices(ctx context.Context, team string,
	prior *v1alpha1.TeamPlacementStatus, status *v1alpha1.TeamStatus,
	cellsByKey map[string]*v1alpha1.Controller) error {
	if status.Placement == nil {
		return nil
	}
	holders := activeSlotHolders(status.Placement)
	var first error
	for slot, holder := range holders {
		if err := r.upsertTenantAgentService(ctx, team, holder.namespace, holder.cell, cellsByKey, slot); err != nil && first == nil {
			first = err
		}
	}
	for slot, gone := range removedSlots(prior, holders) {
		name := tenantAgentServiceName(team, slot)
		if err := r.client.DeleteResource(ctx, "Service", name, gone.namespace); err != nil {
			r.logger.Warn("failed to delete a removed slot's agent Service",
				"team", team, "service", name, "error", err)
		}
	}
	return first
}

// removedSlots answers each slot the tenant held before this tick and
// holds no longer, with the namespace its Service sits in. A scale-down
// releases the highest slots, and each of them loses its Service.
func removedSlots(prior *v1alpha1.TeamPlacementStatus, holders map[int32]slotHolder) map[int32]slotHolder {
	out := map[int32]slotHolder{}
	if prior == nil {
		return out
	}
	for slot, before := range activeSlotHolders(prior) {
		if _, held := holders[slot]; held {
			continue
		}
		out[slot] = before
	}
	return out
}

// slotHolder names one active slot's cell.
type slotHolder struct {
	namespace string
	cell      string
}

// activeSlotHolders answers each active slot's holder. A placement with an
// empty member list answers slot 0 on its scalar cell, so a tenant the
// pass has not written a member list for is unchanged.
func activeSlotHolders(p *v1alpha1.TeamPlacementStatus) map[int32]slotHolder {
	out := map[int32]slotHolder{}
	for _, m := range p.Members {
		if m.Role == v1alpha1.TeamMemberRoleStandby || m.Slot == nil || m.Cell == "" {
			continue
		}
		out[*m.Slot] = slotHolder{namespace: m.CellNamespace, cell: m.Cell}
	}
	if len(out) == 0 && p.Cell != "" {
		out[tenantSlotZero] = slotHolder{namespace: p.CellNamespace, cell: p.Cell}
	}
	return out
}

// setStaleAgentServiceNamespace records one more namespace the next tick
// must clean. A namespace the list already names is not appended again, so a
// move back to a namespace still owing a delete adds no duplicate.
func setStaleAgentServiceNamespace(status *v1alpha1.TeamStatus, namespace string) {
	if slices.Contains(status.StaleAgentServiceNamespaces, namespace) {
		return
	}
	status.StaleAgentServiceNamespaces = append(status.StaleAgentServiceNamespaces, namespace)
}

// drainStaleAgentServices deletes the tenant's slot 0 agent Service from
// every namespace the list names, and keeps only the entries that failed.
// DeleteResource answers success for an absent Service, so a repeat costs
// one call. The pass walks the whole list, so one refused namespace never
// holds the others. It reports the first failure, and a later tick deletes
// again.
func (r *TeamReconciler) drainStaleAgentServices(ctx context.Context, team string, status *v1alpha1.TeamStatus) error {
	if len(status.StaleAgentServiceNamespaces) == 0 {
		return nil
	}
	var kept []string
	var first error
	for _, namespace := range status.StaleAgentServiceNamespaces {
		failed := false
		// Every slot's Service carries the same name shape, so the drain
		// deletes each one the tenant could have left behind.
		for slot := int32(0); slot < maxTenantSlotSweep; slot++ {
			name := tenantAgentServiceName(team, slot)
			if err := r.client.DeleteResource(ctx, "Service", name, namespace); err != nil {
				failed = true
				if first == nil {
					first = fmt.Errorf("delete the agent service %q in namespace %q: %w", name, namespace, err)
				}
			}
		}
		if failed {
			kept = append(kept, namespace)
		}
	}
	status.StaleAgentServiceNamespaces = kept
	return first
}

// upsertTenantAgentService converges one slot's agent Service. The Service
// lives in the slot holder's namespace, and its selector names that
// holder's pod label, so an agent pod dials an endpoint that follows the
// tenant's placement rather than a dead Jenkins JVM.
func (r *TeamReconciler) upsertTenantAgentService(ctx context.Context, team, namespace, cell string,
	cellsByKey map[string]*v1alpha1.Controller, slot int32) error {
	if cell == "" {
		return nil
	}
	writer, ok := r.client.(tenantServiceWriter)
	if !ok {
		return nil
	}
	holder := cellsByKey[teamCellKey(namespace, cell)]
	if holder == nil {
		// The pass holds no object for the cell, so it cannot derive the pod
		// label. The next tick reads the cell again.
		return nil
	}
	name := tenantAgentServiceName(team, slot)
	if err := writer.CreateOrUpdateTenantService(ctx, name, namespace, controllerPrefix(holder)); err != nil {
		r.logger.Error("failed to write the tenant agent Service", "team", team, "service", name, "error", err)
		return fmt.Errorf("write the agent service %q: %w", name, err)
	}
	return nil
}

// ensureTenantAgentIdentity creates the per-tenant agent identity in
// agentNamespace (design D7/D8/D11): the ServiceAccount, a
// service-account-token Secret bound to it, and the Role and RoleBinding
// CreateAgentRBAC builds. The placement pass is the one owner: the
// controller reconciler only reads these objects and never creates them, so
// a controller tick never restores what teardown removed. Every call is
// idempotent: an existing object logs nothing. The one exception is a
// Secret under the token name that is not a token for the ServiceAccount,
// which logs a warning.
func (r *TeamReconciler) ensureTenantAgentIdentity(ctx context.Context, team, agentNamespace string) {
	saName := tenantAgentServiceAccountName(team)
	if err := r.client.CreateServiceAccount(ctx, saName, agentNamespace); err != nil {
		r.logger.Error("failed to create tenant agent ServiceAccount", "team", team, "error", err)
		return
	}
	if err := r.client.CreateAgentRBAC(ctx, saName, agentNamespace); err != nil {
		r.logger.Error("failed to create tenant agent RBAC", "team", team, "error", err)
	}
	tokenName := tenantAgentTokenSecretName(team)
	if err := r.client.CreateServiceAccountTokenSecret(ctx, tokenName, agentNamespace, saName); err != nil {
		r.logger.Error("failed to create tenant agent token secret", "team", team, "error", err)
	}
}

// publishPlacementMoved publishes one tenant.placement_moved event when the
// bound cell of a Team changed between two passes. A nil publisher publishes
// nothing.
func (r *TeamReconciler) publishPlacementMoved(team, previousCell string, placement *v1alpha1.TeamPlacementStatus) {
	if r.activityPublisher == nil {
		return
	}
	if ev, ok := placementMovedEvent(team, previousCell, placement); ok {
		r.activityPublisher.Publish(ev)
	}
}

// placementMovedEvent builds the event of a bound cell that changed. A first
// bind has no previous cell, so it returns false, and so does an unchanged
// cell.
func placementMovedEvent(team, previousCell string, placement *v1alpha1.TeamPlacementStatus) (activity.Event, bool) {
	if previousCell == "" || placement == nil || placement.Cell == "" || placement.Cell == previousCell {
		return activity.Event{}, false
	}
	return activity.Event{
		Type:       "tenant.placement_moved",
		Source:     "operator",
		Severity:   "info",
		Cluster:    placement.Cluster,
		Namespace:  placement.CellNamespace,
		Controller: placement.Cell,
		ItemPath:   team,
		Reason:     previousCell,
		Message:    "team " + team + " moved from cell " + previousCell + " to cell " + placement.Cell,
	}, true
}

// publishTenantEvent publishes one placement-pass activity event.
func (r *TeamReconciler) publishTenantEvent(ev cells.ActivityEvent) {
	if r.activityPublisher == nil {
		return
	}
	r.activityPublisher.Publish(activity.Event{
		Type:    ev.Type,
		Source:  "operator",
		Message: ev.Message,
	})
}

// NewNATSPurgeRequester builds a cells.PurgeFunc that sends the C11
// job_schedules purge request over bus.TenantPurgeSubject and decodes the
// recorder's reply.
func NewNATSPurgeRequester(conn *bus.Conn) cells.PurgeFunc {
	return func(ctx context.Context, req cells.PurgeRequest) (bool, error) {
		if conn == nil {
			return false, fmt.Errorf("no nats connection")
		}
		payload, err := json.Marshal(recordstore.TenantPurgeRequest{
			Cluster:             req.Cluster,
			Tenant:              req.Tenant,
			PlacementGeneration: req.OldGeneration,
			Cell:                req.Cell,
			CellNamespace:       req.CellNamespace,
			Incarnation:         req.Incarnation,
			TeamDeleted:         req.TeamDeleted,
		})
		if err != nil {
			return false, fmt.Errorf("marshal tenant purge request: %w", err)
		}
		msg, err := conn.RequestWithContext(ctx, bus.TenantPurgeSubject, payload, tenantPurgeRequestTimeout)
		if err != nil {
			return false, err
		}
		var resp recordstore.TenantPurgeResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			return false, fmt.Errorf("decode tenant purge response: %w", err)
		}
		if resp.Error != "" {
			return false, fmt.Errorf("%s", resp.Error)
		}
		return true, nil
	}
}

// tenantPurgeRequestTimeout bounds one purge request from the operator
// side, mirroring the recorder's own tenantPurgeTimeout.
const tenantPurgeRequestTimeout = 10 * time.Second

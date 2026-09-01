package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ErrCandidateNotReady is returned by PromoteVersionCandidate when the
// candidate's Phase is not Ready. Both HTTP and MCP callers map it to a
// non-retriable-without-state-change 409/error.
var ErrCandidateNotReady = errors.New("candidate is not Ready")

// versionCandidateSeedLabel mirrors internal/controller.versionProfileSeedLabel
// (unexported there), which promotion strips from both the profile's
// PluginSetRef ConfigMap and the JenkinsVersionProfile CR itself.
const versionCandidateSeedLabel = "varroa.dev/version-profile-seed"

// HandleVersionCandidates handles GET /api/v1/version-candidates — a
// clusterless list resolved directly against s.deps.Store.
func (s *Server) HandleVersionCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateVersionProfile(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	items, err := crdstore.List[v1alpha1.ProfileCandidate](r.Context(), s.deps.Store, "", "")
	if err != nil {
		s.writeK8sError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(items))
}

// HandleVersionCandidateDispatch handles the clusterless dynamic-segment
// routes GET /api/v1/version-candidates/{name} and
// POST /api/v1/version-candidates/{name}/promote, mirroring
// handleVarroaRoleDispatch's hand-parsed dispatch style.
func (s *Server) HandleVersionCandidateDispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/version-candidates/") {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(path, "/version-candidates/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	if name, ok := strings.CutSuffix(rest, "/promote"); ok {
		if name == "" || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.promoteVersionCandidate(w, r, name)
		return
	}

	name := rest
	if strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		s.writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateVersionProfile(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	candidate, err := crdstore.Get[v1alpha1.ProfileCandidate](r.Context(), s.deps.Store, name, "")
	if err != nil {
		s.writeK8sError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, candidate)
}

// promoteVersionCandidate implements POST /version-candidates/{name}/promote,
// mapping PromoteVersionCandidate's result onto the HTTP response.
func (s *Server) promoteVersionCandidate(w http.ResponseWriter, r *http.Request, name string) {
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUpdateVersionProfile(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	candidate, err := PromoteVersionCandidate(r.Context(), s.deps, name, ActorFrom(claims))
	if err != nil {
		if errors.Is(err, ErrCandidateNotReady) {
			s.writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		s.writeK8sError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, candidate)
}

// PromoteVersionCandidate runs the seven-step promotion write sequence.
// Every step is resourceVersion-scoped; a k8s conflict error at any step
// aborts the whole promotion (the caller maps apierrors.IsConflict to 409).
// Exported so both the HTTP handler above and the MCP
// promote_version_candidate tool (internal/mcp/tools_versioncandidates.go)
// share one implementation instead of duplicating this sequence.
func PromoteVersionCandidate(ctx context.Context, deps *Dependencies, name, actorName string) (*v1alpha1.ProfileCandidate, error) {
	candidate, err := crdstore.Get[v1alpha1.ProfileCandidate](ctx, deps.Store, name, "")
	if err != nil {
		return nil, err
	}
	if candidate.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
		return nil, fmt.Errorf("%w: candidate %s (phase=%s)", ErrCandidateNotReady, candidate.Name, candidate.Status.Phase)
	}

	profile, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, deps.Store, candidate.Spec.ProfileRef, "")
	if err != nil {
		return nil, err
	}
	if profile.Spec.PluginSetRef == nil || profile.Spec.PluginSetRef.Name == "" {
		return nil, fmt.Errorf("profile %s has no pluginSetRef", profile.Name)
	}
	if candidate.Spec.ClosureContentRef == "" {
		return nil, fmt.Errorf("candidate %s has no closureContentRef", candidate.Name)
	}

	// Step 1: remove the seed label from the profile's PluginSetRef ConfigMap.
	if err := deps.Client.RemoveConfigMapLabel(ctx, profile.Spec.PluginSetRef.Name, deps.OperatorNamespace, versionCandidateSeedLabel); err != nil {
		return nil, fmt.Errorf("removing seed label from configmap: %w", err)
	}

	// Step 2: remove the seed label from the profile CR itself.
	if profile.Labels != nil {
		if _, ok := profile.Labels[versionCandidateSeedLabel]; ok {
			delete(profile.Labels, versionCandidateSeedLabel)
			if err := crdstore.Update[v1alpha1.JenkinsVersionProfile](ctx, deps.Store, profile); err != nil {
				return nil, err
			}
		}
	}

	// Step 3: advance Spec.ResolveVersion and persist it before the plugin
	// content that backs it lands. A plugin declares a minimum core version
	// and will not load below it, but an older plugin set loads fine on a
	// newer core — so a partial failure here must never leave the profile's
	// plugin set resolved for a newer core than the profile's own
	// resolveVersion.
	profile.Spec.ResolveVersion = candidate.Spec.ResolveVersion
	if err := crdstore.Update[v1alpha1.JenkinsVersionProfile](ctx, deps.Store, profile); err != nil {
		return nil, err
	}

	// Step 4: overwrite the pluginset ConfigMap content with the candidate's
	// resolved closure.
	closureData, err := deps.Client.GetConfigMap(ctx, candidate.Spec.ClosureContentRef, deps.OperatorNamespace)
	if err != nil {
		return nil, fmt.Errorf("reading closure configmap %s: %w", candidate.Spec.ClosureContentRef, err)
	}
	pluginsYAML, ok := closureData["plugins.yaml"]
	if !ok {
		return nil, fmt.Errorf("closure configmap %s has no plugins.yaml key", candidate.Spec.ClosureContentRef)
	}
	if err := deps.Client.UpdateConfigMapData(ctx, profile.Spec.PluginSetRef.Name, deps.OperatorNamespace, map[string]string{"plugins.yaml": pluginsYAML}); err != nil {
		return nil, fmt.Errorf("overwriting pluginset configmap: %w", err)
	}

	// Step 5: synchronously re-materialize, then verify. Spec.ResolveVersion
	// is NOT rolled back on failure — the candidate remains Ready so
	// promotion can be retried once the underlying issue is fixed.
	if deps.VersionProfileReconciler == nil {
		return nil, fmt.Errorf("version profile reconciler not configured")
	}
	if _, err := deps.VersionProfileReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		return nil, fmt.Errorf("materializing profile: %w", err)
	}
	reconciled, err := crdstore.Get[v1alpha1.JenkinsVersionProfile](ctx, deps.Store, profile.Name, "")
	if err != nil {
		return nil, err
	}
	if !controller.ProfilePluginSetReady(reconciled) || reconciled.Status.ContentRef == "" {
		return nil, fmt.Errorf("profile %s did not become PluginSetReady after promotion", profile.Name)
	}

	// Step 6: supersede every other open (Pending/Ready) sibling candidate.
	siblings, err := crdstore.List[v1alpha1.ProfileCandidate](ctx, deps.Store, "", "")
	if err != nil {
		return nil, err
	}
	for _, sib := range siblings {
		if sib.Name == candidate.Name || sib.Spec.ProfileRef != candidate.Spec.ProfileRef {
			continue
		}
		if sib.Status.Phase != v1alpha1.ProfileCandidatePhasePending && sib.Status.Phase != v1alpha1.ProfileCandidatePhaseReady {
			continue
		}
		sib.Status.Phase = v1alpha1.ProfileCandidatePhaseSuperseded
		if err := crdstore.UpdateStatus[v1alpha1.ProfileCandidate](ctx, deps.Store, sib); err != nil {
			return nil, err
		}
		if deps.ActivityPublisher != nil {
			deps.ActivityPublisher.Publish(activity.Event{
				Type:     "upgrade.candidate.superseded",
				Source:   "operator",
				Severity: "info",
				Actor:    actorName,
				Message:  fmt.Sprintf("candidate %s superseded by promotion of candidate %s for profile %s", sib.Name, candidate.Name, candidate.Spec.ProfileRef),
			})
		}
	}

	// Step 7 (last, non-idempotent): promote the candidate itself. The
	// candidate's own phase stays Pending/Ready until this write lands, so an
	// upgrade-tracker tick's supersedeOlder can land its own status patch on
	// this exact candidate between the Get above and here — that patch carries
	// no resourceVersion precondition, so it always succeeds and leaves this
	// write's captured resourceVersion stale. By this point the profile has
	// already been advanced and its plugin set already re-materialized (steps
	// 3-5), so Promoted must win over that stale patch rather than abort the
	// whole promotion and strand the candidate un-retriable in Superseded: on
	// a conflict, re-read the live candidate and reapply Promoted onto it.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, getErr := crdstore.Get[v1alpha1.ProfileCandidate](ctx, deps.Store, candidate.Name, "")
		if getErr != nil {
			return getErr
		}
		now := metav1.Now()
		fresh.Status.Phase = v1alpha1.ProfileCandidatePhasePromoted
		fresh.Status.PromotedAt = &now
		fresh.Status.Conditions = append(fresh.Status.Conditions, v1alpha1.ProfileCandidateCondition{
			Type:               v1alpha1.ConditionCandidatePromoted,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "Promoted",
			Message:            fmt.Sprintf("promoted by %s", actorName),
		})
		if updErr := crdstore.UpdateStatus[v1alpha1.ProfileCandidate](ctx, deps.Store, fresh); updErr != nil {
			return updErr
		}
		candidate = fresh
		return nil
	}); err != nil {
		return nil, err
	}
	if deps.ActivityPublisher != nil {
		deps.ActivityPublisher.Publish(activity.Event{
			Type:     "upgrade.candidate.promoted",
			Source:   "operator",
			Severity: "info",
			Actor:    actorName,
			Message:  fmt.Sprintf("candidate %s promoted profile %s to %s", candidate.Name, candidate.Spec.ProfileRef, candidate.Spec.ResolveVersion),
		})
	}

	return candidate, nil
}

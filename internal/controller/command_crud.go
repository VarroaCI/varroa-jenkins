package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// CommandCRUDClient is a narrow interface for the k8s client operations
// that CommandCRUD needs, allowing testability without *ClientsetClient.
type CommandCRUDClient interface {
	// ApplyControllerSpecSSA applies a sparse Controller spec via Kubernetes
	// server-side apply.
	ApplyControllerSpecSSA(ctx context.Context, namespace, name string, specPatch map[string]any, fieldManager string, force bool) (*v1alpha1.Controller, error)
	DeleteControllerPod(ctx context.Context, namespace, name string) error
}

// PreflightOptions mirrors preflight.Options to avoid an import cycle
// (preflight imports internal/controller, so controller cannot import preflight).
type PreflightOptions struct {
	OperatorNamespace string
	ManagedNamespaces string
	ForUpdate         bool
	PriorVersion      string
}

// PreflightFunc is a function that runs preflight checks and returns bus checks.
// Set to a wrapper around preflight.Run in main().
type PreflightFunc func(ctx context.Context, deps PreflightDepsInterface, draft *v1alpha1.Controller, inlineBundle *v1alpha1.ComposedBundleSpec, opts PreflightOptions) []bus.Check

// PreflightDepsInterface is the narrow interface PreflightFunc needs from the
// k8s client. It mirrors preflight.Deps to avoid an import cycle.
type PreflightDepsInterface interface {
	crdstore.Backend
	ListResourceQuotas(ctx context.Context, namespace string) ([]corev1.ResourceQuota, error)
	ListIngressHosts(ctx context.Context) (map[string][]string, error)
	GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error)
}

// LifecycleStateReader is the read interface for drain lifecycle state.
type LifecycleStateReader interface {
	State() LifecycleState
}

// CommandCRUD implements operator-side CRUD handlers for BFF-initiated
// controller operations received over the NATS bus. Each Handle* method
// unmarshals the request, validates against local k8s state, and returns a
// JSON-marshalled response (§3).
type CommandCRUD struct {
	Client            CommandCRUDClient
	Store             crdstore.Backend
	OperatorNamespace string
	ManagedNamespaces string // "" ⇒ cluster-wide mode
	Logger            *slog.Logger

	// Lifecycle is the drain lifecycle store. If nil, create-blocking is
	// skipped (safe in tests or pre-drain operators).
	Lifecycle LifecycleStateReader

	// PreflightCheck runs preflight validation. Must be set by the caller
	// (typically cmd/operator/main.go wraps preflight.Run to break the
	// import cycle). If nil, preflight is skipped.
	PreflightCheck PreflightFunc
}

// --- Shared helpers ---

// crudReply marshals v to JSON for the bus response.
// Returns a JSON error envelope on marshal failure.
func (c *CommandCRUD) crudReply(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		c.Logger.Error("crud reply marshal failed", "error", err)
		errResp, _ := json.Marshal(map[string]string{"error": "internal marshal error", "code": bus.CodeInternal})
		return errResp
	}
	return data
}

// stripManagedFields removes the managedFields entry from metadata to reduce
// response payload size (the largest metadata noise source).
func stripManagedFields(cr *v1alpha1.Controller) {
	cr.ManagedFields = nil
}

// k8sErrToCode maps a k8s API error to a bus error code.
func k8sErrToCode(err error) string {
	if k8serrors.IsNotFound(err) {
		return bus.CodeNotFound
	}
	if k8serrors.IsConflict(err) || k8serrors.IsAlreadyExists(err) {
		return bus.CodeConflict
	}
	return bus.CodeInternal
}

// preflightDeps returns a deps-like interface for preflight from the client.
func (c *CommandCRUD) preflightDeps() PreflightDepsInterface {
	if cc, ok := c.Client.(PreflightDepsInterface); ok {
		return cc
	}
	// In tests with a fake client, preflight is bypassed via PreflightCheck.
	// This should never be reached when PreflightCheck is nil.
	return nil
}

// --- Handle* methods ---

// HandleList handles operator.<cluster>.controllers.list.
func (c *CommandCRUD) HandleList(data []byte) []byte {
	var req bus.ControllersListRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.crudReply(bus.ControllersListResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, err := crdstore.List[v1alpha1.Controller](ctx, c.Store, req.Namespace, "")
	if err != nil {
		c.Logger.Error("list controllers failed", "namespace", req.Namespace, "error", err)
		return c.crudReply(bus.ControllersListResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	// Strip managedFields and marshal each CR to raw JSON.
	rawItems := make([]json.RawMessage, 0, len(items))
	for _, cr := range items {
		stripManagedFields(cr)
		raw, err := json.Marshal(cr)
		if err != nil {
			c.Logger.Error("marshal controller failed", "name", cr.Name, "namespace", cr.Namespace, "error", err)
			return c.crudReply(bus.ControllersListResponse{Error: "marshal error", Code: bus.CodeInternal})
		}
		rawItems = append(rawItems, raw)
	}

	// Marshal once: the same bytes serve both the size-budget check and the
	// reply (the list is the hottest fan-out path).
	reply, err := json.Marshal(bus.ControllersListResponse{Items: rawItems})
	if err != nil {
		c.Logger.Error("crud reply marshal failed", "error", err)
		return c.crudReply(bus.ControllersListResponse{Error: "marshal error", Code: bus.CodeInternal})
	}
	if len(reply) > 900*1024 {
		return c.crudReply(bus.ControllersListResponse{Code: bus.CodeInternal, Error: "list too large"})
	}
	return reply
}

// HandleGet handles operator.<cluster>.controllers.get.
func (c *CommandCRUD) HandleGet(data []byte) []byte {
	var req bus.ControllersGetRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.crudReply(bus.ControllersGetResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cr, err := crdstore.Get[v1alpha1.Controller](ctx, c.Store, req.Name, req.Namespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return c.crudReply(bus.ControllersGetResponse{Error: "controller not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get controller failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.crudReply(bus.ControllersGetResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	stripManagedFields(cr)
	raw, err := json.Marshal(cr)
	if err != nil {
		return c.crudReply(bus.ControllersGetResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.crudReply(bus.ControllersGetResponse{Item: raw})
}

// HandleCreate handles operator.<cluster>.controllers.create.
func (c *CommandCRUD) HandleCreate(data []byte) []byte {
	var req bus.ControllersCreateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.crudReply(bus.ControllersCreateResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	// Drain-block: reject creates (including dry-run) when the cluster is
	// draining or drained. This is the authoritative operator-side check.
	if c.Lifecycle != nil {
		if st := c.Lifecycle.State(); st.State != bus.ClusterStateActive {
			return c.crudReply(bus.ControllersCreateResponse{
				Code:  bus.CodeDraining,
				Error: fmt.Sprintf("cluster is %s: controller creation is blocked", st.State),
			})
		}
	}

	// Unmarshal the controller CR from the request.
	var cr v1alpha1.Controller
	if err := json.Unmarshal(req.Controller, &cr); err != nil {
		return c.crudReply(bus.ControllersCreateResponse{Error: "invalid controller JSON", Code: bus.CodeInvalid})
	}

	if cr.Name == "" {
		return c.crudReply(bus.ControllersCreateResponse{Error: "metadata.name is required", Code: bus.CodeInvalid})
	}

	// Set namespace/APIVersion/Kind.
	cr.Namespace = req.Namespace
	cr.APIVersion = "varroa.dev/v1alpha1"
	cr.Kind = "Controller"

	// Validate existing bundle reference with exact-namespace resolution.
	if cr.Spec.ComposedBundleRef != nil {
		lookupNS := cr.Spec.ComposedBundleRef.Namespace
		if lookupNS == "" {
			lookupNS = req.Namespace
		}
		if _, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), c.Store, cr.Spec.ComposedBundleRef.Name, lookupNS); err != nil {
			msg := fmt.Sprintf("composedBundle %q not found in namespace %q (set spec.composedBundleRef.namespace if it lives elsewhere)", cr.Spec.ComposedBundleRef.Name, lookupNS)
			return c.crudReply(bus.ControllersCreateResponse{Error: msg, Code: bus.CodeInvalid})
		}
	}

	// Unmarshal optional inline bundle.
	var bundle *v1alpha1.ComposedBundleSpec
	if req.Bundle != nil {
		bundle = &v1alpha1.ComposedBundleSpec{}
		if err := json.Unmarshal(req.Bundle, bundle); err != nil {
			return c.crudReply(bus.ControllersCreateResponse{Error: "invalid bundle spec", Code: bus.CodeInvalid})
		}
	}

	// Run preflight checks (if configured).
	if c.PreflightCheck != nil {
		checks := c.PreflightCheck(context.Background(), c.preflightDeps(), &cr, bundle, PreflightOptions{
			OperatorNamespace: c.OperatorNamespace,
			ManagedNamespaces: c.ManagedNamespaces,
		})

		// Check for failures.
		var failing []bus.Check
		for _, ch := range checks {
			if ch.Status == "fail" {
				failing = append(failing, ch)
			}
		}
		if len(failing) > 0 {
			code := bus.CodeInvalid
			failingIDs := make([]string, len(failing))
			for i, ch := range failing {
				failingIDs[i] = ch.ID
				if ch.ID == "name" {
					code = bus.CodeConflict
				}
			}
			// Log operator-side: the failing checks travel back over the bus,
			// but remote preflight failures must also be debuggable from this
			// cluster's logs.
			c.Logger.Warn("controller create preflight failed",
				"namespace", req.Namespace, "name", cr.Name,
				"failingChecks", failingIDs, "dryRun", req.DryRun)
			return c.crudReply(bus.ControllersCreateResponse{
				Checks: checks,
				Error:  "preflight failed",
				Code:   code,
			})
		}

		// DryRun: reply with checks only, nothing persisted.
		if req.DryRun {
			return c.crudReply(bus.ControllersCreateResponse{Checks: checks})
		}
	} else if req.DryRun {
		// No preflight function configured; dry-run with no checks.
		return c.crudReply(bus.ControllersCreateResponse{Checks: []bus.Check{}})
	}

	// Inline bundle: create the ComposedBundle first.
	if req.Bundle != nil {
		bundleName := cr.Name + "-bundle"
		existing, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), c.Store, bundleName, req.Namespace)
		if err == nil {
			if !reflect.DeepEqual(existing.Spec, *bundle) {
				return c.crudReply(bus.ControllersCreateResponse{
					Error: "bundle " + bundleName + " exists with different spec",
					Code:  bus.CodeConflict,
				})
			}
		} else if k8serrors.IsNotFound(err) {
			cb := &v1alpha1.ComposedBundle{
				ObjectMeta: metav1.ObjectMeta{Name: bundleName, Namespace: req.Namespace},
				Spec:       *bundle,
			}
			cb.APIVersion = "varroa.dev/v1alpha1"
			cb.Kind = "ComposedBundle"
			if err := crdstore.Apply[v1alpha1.ComposedBundle](context.Background(), c.Store, cb); err != nil {
				c.Logger.Error("create inline bundle failed", "bundle", bundleName, "error", err)
				return c.crudReply(bus.ControllersCreateResponse{Error: "failed to create bundle", Code: bus.CodeInternal})
			}
		} else {
			c.Logger.Error("get inline bundle failed", "bundle", bundleName, "error", err)
			return c.crudReply(bus.ControllersCreateResponse{Error: "failed to check existing bundle", Code: bus.CodeInternal})
		}
		cr.Spec.ComposedBundleRef = &v1alpha1.ComposedBundleRef{Name: bundleName}
	}

	// Apply the controller CRD.
	if err := crdstore.Apply[v1alpha1.Controller](context.Background(), c.Store, &cr); err != nil {
		c.Logger.Error("create controller failed", "namespace", req.Namespace, "name", cr.Name, "error", err)
		// Best-effort cleanup of the inline bundle.
		if req.Bundle != nil {
			bundleName := cr.Name + "-bundle"
			if delErr := crdstore.Delete[v1alpha1.ComposedBundle](context.Background(), c.Store, bundleName, req.Namespace); delErr != nil {
				c.Logger.Warn("failed to clean up orphaned bundle", "bundle", bundleName, "error", delErr)
			}
		}
		return c.crudReply(bus.ControllersCreateResponse{
			Error: fmt.Sprintf("failed to create controller: %s", err.Error()),
			Code:  k8sErrToCode(err),
		})
	}

	// Reply with the created CR.
	stripManagedFields(&cr)
	raw, err := json.Marshal(&cr)
	if err != nil {
		return c.crudReply(bus.ControllersCreateResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.crudReply(bus.ControllersCreateResponse{Item: raw})
}

// HandleUpdate handles operator.<cluster>.controllers.update.
func (c *CommandCRUD) HandleUpdate(data []byte) []byte {
	var req bus.ControllersUpdateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.crudReply(bus.ControllersUpdateResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx := context.Background()

	// Fetch existing CRD.
	existing, err := crdstore.Get[v1alpha1.Controller](ctx, c.Store, req.Name, req.Namespace)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return c.crudReply(bus.ControllersUpdateResponse{Error: "controller not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("get controller for update failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.crudReply(bus.ControllersUpdateResponse{Error: err.Error(), Code: bus.CodeInternal})
	}

	// Decode the patch.
	var patch map[string]interface{}
	if err := json.Unmarshal(req.Patch, &patch); err != nil {
		return c.crudReply(bus.ControllersUpdateResponse{Error: "invalid patch JSON", Code: bus.CodeInvalid})
	}

	// Defense-in-depth: only spec/apiVersion/kind top-level keys allowed.
	for k := range patch {
		if k != "spec" && k != "apiVersion" && k != "kind" {
			return c.crudReply(bus.ControllersUpdateResponse{
				Error: fmt.Sprintf("patch may only contain spec, apiVersion, or kind; got %q", k),
				Code:  bus.CodeInvalid,
			})
		}
	}

	// Merge-patch (RFC 7386) on the raw JSON: one marshal of the existing CR
	// and one unmarshal of the merged result, instead of the previous
	// marshal→map→merge→marshal→unmarshal round-trip of the whole CR.
	existingJSON, err := json.Marshal(existing)
	if err != nil {
		return c.crudReply(bus.ControllersUpdateResponse{Error: "json marshal failed", Code: bus.CodeInternal})
	}
	mergedJSON, err := jsonpatch.MergePatch(existingJSON, req.Patch)
	if err != nil {
		return c.crudReply(bus.ControllersUpdateResponse{Error: "invalid patch JSON", Code: bus.CodeInvalid})
	}

	var updated v1alpha1.Controller
	if err := json.Unmarshal(mergedJSON, &updated); err != nil {
		return c.crudReply(bus.ControllersUpdateResponse{Error: "json unmarshal failed", Code: bus.CodeInternal})
	}

	// Validate ingress mode immutability.
	if existing.Spec.IngressSpec.RoutingMode() != updated.Spec.IngressSpec.RoutingMode() {
		return c.crudReply(bus.ControllersUpdateResponse{Error: "ingressSpec.mode is immutable", Code: bus.CodeInvalid})
	}

	// Validate ingress mode enum.
	if updated.Spec.IngressSpec != nil {
		mode := updated.Spec.IngressSpec.Mode
		if mode != "" && mode != "subdomain" && mode != "path" {
			return c.crudReply(bus.ControllersUpdateResponse{
				Error: "ingressSpec.mode must be \"subdomain\" or \"path\"",
				Code:  bus.CodeInvalid,
			})
		}
		if err := ValidateIngressAnnotations(updated.Spec.IngressSpec.Annotations); err != nil {
			return c.crudReply(bus.ControllersUpdateResponse{Error: err.Error(), Code: bus.CodeInvalid})
		}
	}

	// Validate that composedBundleRef (if set) names an existing ComposedBundle.
	if updated.Spec.ComposedBundleRef != nil {
		lookupNS := updated.Spec.ComposedBundleRef.Namespace
		if lookupNS == "" {
			lookupNS = req.Namespace
		}
		if _, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, c.Store, updated.Spec.ComposedBundleRef.Name, lookupNS); err != nil {
			return c.crudReply(bus.ControllersUpdateResponse{
				Error: fmt.Sprintf("composedBundle %q not found in namespace %q (set spec.composedBundleRef.namespace if it lives elsewhere)", updated.Spec.ComposedBundleRef.Name, lookupNS),
				Code:  bus.CodeInvalid,
			})
		}
	}

	// Run preflight on the updated spec (if configured).
	if c.PreflightCheck != nil {
		checks := c.PreflightCheck(ctx, c.preflightDeps(), &updated, nil, PreflightOptions{
			OperatorNamespace: c.OperatorNamespace,
			ManagedNamespaces: c.ManagedNamespaces,
			ForUpdate:         true,
			PriorVersion:      existing.Spec.Version,
		})

		var failing []bus.Check
		for _, ch := range checks {
			if ch.Status == "fail" {
				failing = append(failing, ch)
			}
		}
		if len(failing) > 0 {
			return c.crudReply(bus.ControllersUpdateResponse{
				Checks: checks,
				Error:  "preflight failed",
				Code:   bus.CodeInvalid,
			})
		}
	}

	// Build the sparse spec map (fields-to-own) from req.Patch's "spec" key.
	// The map is passed directly to ApplyControllerSpecSSA — no typed round-trip,
	// so non-omitempty fields like spec.namespace are never serialized unless
	// the user explicitly included them in the patch.
	var specPatch map[string]any
	if specRaw, ok := patch["spec"]; ok {
		specJSON, err := json.Marshal(specRaw)
		if err != nil {
			return c.crudReply(bus.ControllersUpdateResponse{Error: "invalid spec JSON", Code: bus.CodeInvalid})
		}
		if err := json.Unmarshal(specJSON, &specPatch); err != nil {
			return c.crudReply(bus.ControllersUpdateResponse{Error: "invalid spec JSON", Code: bus.CodeInvalid})
		}
	}

	// Apply via server-side apply.
	fieldManager := req.FieldManager
	if fieldManager == "" {
		fieldManager = "varroa-ui" // defense-in-depth: never apply with empty manager
	}
	result, err := c.Client.ApplyControllerSpecSSA(ctx, req.Namespace, req.Name, specPatch, fieldManager, req.Force)
	if err != nil {
		if conflicts := SSAConflicts(err); conflicts != nil {
			busConflicts := make([]bus.FieldConflict, len(conflicts))
			for i, fc := range conflicts {
				busConflicts[i] = bus.FieldConflict{Field: fc.Field, Manager: fc.Manager, Message: fc.Message}
			}
			return c.crudReply(bus.ControllersUpdateResponse{
				Error: "field conflict", Code: bus.CodeConflict, Conflicts: busConflicts,
			})
		}
		c.Logger.Error("update controller failed", "namespace", req.Namespace, "name", req.Name, "error", err)
		return c.crudReply(bus.ControllersUpdateResponse{
			Error: fmt.Sprintf("failed to update controller: %s", err.Error()),
			Code:  k8sErrToCode(err),
		})
	}

	stripManagedFields(result)
	raw, err := json.Marshal(result)
	if err != nil {
		return c.crudReply(bus.ControllersUpdateResponse{Error: "marshal error", Code: bus.CodeInternal})
	}

	return c.crudReply(bus.ControllersUpdateResponse{Item: raw})
}

// HandleDelete handles operator.<cluster>.controllers.delete.
func (c *CommandCRUD) HandleDelete(data []byte) []byte {
	var req bus.ControllersDeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.crudReply(bus.ControllersDeleteResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := crdstore.Delete[v1alpha1.Controller](ctx, c.Store, req.Name, req.Namespace); err != nil {
		return c.crudReply(bus.ControllersDeleteResponse{
			Error: fmt.Sprintf("failed to delete controller: %s", err.Error()),
			Code:  k8sErrToCode(err),
		})
	}

	return c.crudReply(bus.ControllersDeleteResponse{})
}

// HandleDeletePod handles operator.<cluster>.controllers.deletepod.
func (c *CommandCRUD) HandleDeletePod(data []byte) []byte {
	var req bus.ControllersDeletePodRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.crudReply(bus.ControllersDeleteResponse{Error: "invalid request", Code: bus.CodeInvalid})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Client.DeleteControllerPod(ctx, req.Namespace, req.Name); err != nil {
		return c.crudReply(bus.ControllersDeleteResponse{
			Error: fmt.Sprintf("failed to delete pod: %s", err.Error()),
			Code:  k8sErrToCode(err),
		})
	}

	return c.crudReply(bus.ControllersDeleteResponse{})
}

// splitManagedNamespaces splits a space- and/or comma-separated string into a
// non-nil string slice, dropping empty entries. Returns nil if the input is
// empty or contains only separators (cluster-wide mode). Exact tokenization
// matches the BFF's parseManagedNamespaces (internal/api/handlers_namespaces.go).
func splitManagedNamespaces(raw string) []string {
	if raw == "" {
		return nil
	}
	f := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ','
	})
	if len(f) == 0 {
		return nil
	}
	return f
}

// HandleNamespacesList serves operator.<cluster>.namespaces.list: the target
// cluster's raw deployable-namespace discovery inputs (F4). No caller claims
// are involved — the ACL'd bus is the trust boundary; the core BFF applies
// caller-scope assembly.
func (c *CommandCRUD) HandleNamespacesList(data []byte) []byte {
	// Request body is an empty object; tolerate any bytes (no fields to decode).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, c.Store, provisioningDefaultsName, "")
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			c.Logger.Error("namespaces.list: provisioning defaults read failed", "error", err)
			return c.crudReply(bus.NamespacesListResponse{Error: "failed to read provisioning defaults", Code: bus.CodeInternal})
		}
		defaults = &v1alpha1.ProvisioningDefaults{}
	}

	curatedDefault := defaults.Spec.DefaultNamespace
	if curatedDefault == "" {
		curatedDefault = "varroa"
	}
	curated := []string{curatedDefault}
	for _, ns := range defaults.Spec.Namespaces {
		if ns != curatedDefault {
			curated = append(curated, ns)
		}
	}

	return c.crudReply(bus.NamespacesListResponse{
		ManagedNamespaces: splitManagedNamespaces(c.ManagedNamespaces),
		CuratedNamespaces: curated,
		CuratedDefault:    curatedDefault,
	})
}

// MergeMap recursively merges src into dst. Arrays and scalars from src overwrite dst.
// Exported so both internal/api/handlers.go and internal/controller/command_crud.go use one copy.
func MergeMap(dst, src map[string]interface{}) {
	for k, v := range src {
		if dstMap, ok := dst[k].(map[string]interface{}); ok {
			if srcMap, ok := v.(map[string]interface{}); ok {
				MergeMap(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

// ValidateIngressAnnotations rejects annotation keys that aren't valid Kubernetes
// qualified names.
func ValidateIngressAnnotations(annotations map[string]string) error {
	for k := range annotations {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return fmt.Errorf("ingressSpec.annotations key %q invalid: %s", k, strings.Join(errs, "; "))
		}
	}
	return nil
}

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/preflight"
)

// ControllerService owns controller mutation domain logic — validation,
// preflight, apply — behind one code path for the create and update
// handlers. HTTP concerns (decode, authz, response encoding) stay in the
// handlers; the service reports failures as *ServiceError so handlers map
// them without string matching.
type ControllerService struct {
	Store             crdstore.Backend
	Client            controller.ResourceClient
	LocalCluster      string // "" when multi-cluster routing is disabled
	DashboardHost     string
	OperatorNamespace string
	ManagedNamespaces string
	Logger            *slog.Logger
}

// ServiceError is a domain-rule violation with the HTTP status the handler
// must emit and the exact message body.
type ServiceError struct {
	Status  int
	Message string
	// Conflicts carries SSA field conflicts for 409 responses.
	Conflicts []bus.FieldConflict
	// Checks carries failing preflight checks for 400 responses.
	Checks []preflight.Check
}

func (e *ServiceError) Error() string { return e.Message }

// ValidateIngress enforces the ingress rules shared by create and update:
// mode enum, path-mode locality and dashboard-host equality (create only),
// and annotation safety. controllerName is used only for log context.
func (svc *ControllerService) ValidateIngress(spec *v1alpha1.IngressSpec, forCreate bool, cluster, controllerName string) *ServiceError {
	if spec == nil {
		return nil
	}
	if mode := spec.Mode; mode != "" && mode != "subdomain" && mode != "path" {
		return &ServiceError{Status: http.StatusBadRequest, Message: "ingressSpec.mode must be \"subdomain\" or \"path\""}
	}
	if forCreate && spec.RoutingMode() == v1alpha1.RoutingModePath {
		if svc.LocalCluster != "" && cluster != svc.LocalCluster {
			return &ServiceError{Status: http.StatusBadRequest, Message: "path mode is only supported on the local cluster"}
		}
		if svc.DashboardHost != "" && spec.Host != svc.DashboardHost {
			return &ServiceError{Status: http.StatusBadRequest, Message: "path mode requires ingressSpec.host to equal the dashboard host"}
		}
		if svc.DashboardHost == "" {
			svc.Logger.Warn("dashboard host unknown; skipping path-mode host check", "host", spec.Host, "controller", controllerName)
		}
	}
	if err := controller.ValidateIngressAnnotations(spec.Annotations); err != nil {
		return &ServiceError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	return nil
}

// ValidateBundleRef checks that a ComposedBundleRef (if set) names an
// existing local bundle. ComposedBundles are cluster-local (D6): callers
// skip this for remote targets — the target operator validates
// authoritatively.
func (svc *ControllerService) ValidateBundleRef(ctx context.Context, ref *v1alpha1.ComposedBundleRef, namespace string) *ServiceError {
	if ref == nil {
		return nil
	}
	lookupNS := ref.Namespace
	if lookupNS == "" {
		lookupNS = namespace
	}
	if _, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, svc.Store, ref.Name, lookupNS); err != nil {
		svc.Logger.Warn("controller references missing composed bundle",
			"bundle", ref.Name, "namespace", lookupNS, "error", err)
		return &ServiceError{Status: http.StatusBadRequest, Message: fmt.Sprintf("composedBundle %q not found in namespace %q (set spec.composedBundleRef.namespace if it lives elsewhere)", ref.Name, lookupNS)}
	}
	return nil
}

// ApplyUpdate runs the local update pipeline for an already-merged
// controller: routing-immutability and ingress validation, bundle-ref
// existence, preflight, and the server-side apply of the sparse spec patch.
// Returns the applied Controller or a ServiceError carrying the HTTP shape.
func (svc *ControllerService) ApplyUpdate(ctx context.Context, existing, updated *v1alpha1.Controller, specPatch map[string]any, namespace, name string, force bool) (*v1alpha1.Controller, *ServiceError) {
	if existing.Spec.IngressSpec.RoutingMode() != updated.Spec.IngressSpec.RoutingMode() {
		return nil, &ServiceError{Status: http.StatusBadRequest, Message: "ingressSpec.mode is immutable"}
	}
	if serr := svc.ValidateIngress(updated.Spec.IngressSpec, false, "", updated.Name); serr != nil {
		return nil, serr
	}
	if serr := svc.ValidateBundleRef(ctx, updated.Spec.ComposedBundleRef, namespace); serr != nil {
		return nil, serr
	}

	checks := preflight.Run(ctx, PreflightStore{Store: svc.Store, Client: svc.Client}, updated, nil, preflight.Options{
		OperatorNamespace: svc.OperatorNamespace,
		ManagedNamespaces: svc.ManagedNamespaces,
		ForUpdate:         true,
		PriorVersion:      existing.Spec.Version,
	})
	var failing []preflight.Check
	for _, c := range checks {
		if c.Status == "fail" {
			failing = append(failing, c)
		}
	}
	if len(failing) > 0 {
		return nil, &ServiceError{Status: http.StatusBadRequest, Message: "preflight failed", Checks: failing}
	}

	result, err := svc.Client.ApplyControllerSpecSSA(ctx, namespace, name, specPatch, "varroa-ui", force)
	if err != nil {
		if conflicts := controller.SSAConflicts(err); conflicts != nil {
			busConflicts := make([]bus.FieldConflict, len(conflicts))
			for i, fc := range conflicts {
				busConflicts[i] = bus.FieldConflict{Field: fc.Field, Manager: fc.Manager, Message: fc.Message}
			}
			return nil, &ServiceError{Status: http.StatusConflict, Message: "field conflict", Conflicts: busConflicts}
		}
		svc.Logger.Error("patch controller failed", "namespace", namespace, "name", name, "error", err)
		return nil, &ServiceError{Status: k8sErrorToHTTP(err), Message: "failed to update controller"}
	}
	return result, nil
}

// controllerSvc builds the service view over the server's dependencies.
func (s *Server) controllerSvc() *ControllerService {
	local := ""
	if s.deps.Brood != nil {
		local = s.deps.Brood.LocalCluster()
	}
	return &ControllerService{
		Store:             s.deps.Store,
		Client:            s.deps.Client,
		LocalCluster:      local,
		DashboardHost:     s.deps.DashboardHost,
		OperatorNamespace: s.deps.OperatorNamespace,
		ManagedNamespaces: s.deps.ManagedNamespaces,
		Logger:            s.deps.Logger,
	}
}

// controllerDetail is the single projection from a Controller CR to the
// ControllerDetail response shape, shared by the detail, create, and update
// handlers (both brood and local paths).
func (s *Server) controllerDetail(cr *v1alpha1.Controller, cluster string) controllerDetailResponse {
	resp := controllerDetailResponse{
		Name:              cr.Name,
		Namespace:         cr.Namespace,
		Cluster:           cluster,
		Phase:             string(cr.Status.Phase),
		Endpoint:          s.controllerEndpoint(cr),
		Version:           cr.Spec.Version,
		PowerState:        cr.Spec.PowerState,
		RoutingMode:       cr.Spec.IngressSpec.RoutingMode(),
		ComposedBundleRef: cr.Spec.ComposedBundleRef,
		EffectiveBundle:   s.effectiveBundleFor(cr),
		Probes:            cr.Spec.Probes,
		MiteImageStatus:   buildMiteImageStatus(cr.Status.MiteStatus, cr.Status.Conditions),
		ReconcileBlocked:  buildReconcileBlockedJSON(cr),
		PluginConflict:    buildPluginConflict(cr.Status.Conditions),
		PluginInventory:   buildPluginInventorySummary(cr.Status.PluginInventory),
	}
	if cr.Spec.ReconciliationPolicy != nil {
		interval := ""
		if cr.Spec.ReconciliationPolicy.Interval != nil {
			interval = cr.Spec.ReconciliationPolicy.Interval.Duration.String()
		}
		resp.ReconciliationPolicy = &ReconciliationPolicyJSON{
			Mode:                string(cr.Spec.ReconciliationPolicy.Mode),
			Interval:            interval,
			MaxDeferSeconds:     cr.Spec.ReconciliationPolicy.MaxDeferSeconds,
			DrainTimeoutSeconds: cr.Spec.ReconciliationPolicy.DrainTimeoutSeconds,
		}
	}
	return resp
}

// writeServiceError maps a ServiceError onto the exact HTTP shapes the
// handlers emitted before the service extraction.
func (s *Server) writeServiceError(w http.ResponseWriter, serr *ServiceError) {
	switch {
	case serr.Conflicts != nil:
		s.writeJSON(w, serr.Status, map[string]interface{}{
			"error":     serr.Message,
			"conflicts": serr.Conflicts,
		})
	case serr.Checks != nil:
		s.writeJSON(w, serr.Status, map[string]interface{}{
			"error":  serr.Message,
			"checks": serr.Checks,
		})
	default:
		s.writeJSONError(w, serr.Status, serr.Message)
	}
}

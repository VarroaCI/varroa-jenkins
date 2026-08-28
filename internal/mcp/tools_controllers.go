//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// =============================================================================
// Controller tools (8)
// =============================================================================

func registerControllerTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	clusterOrDefault := func(args map[string]any) string {
		if c := strArg(args, "cluster"); c != "" {
			return c
		}
		if deps.Brood != nil {
			return deps.Brood.LocalCluster()
		}
		return "core"
	}

	listC := mcp.NewTool("list_controllers",
		mcp.WithDescription("List Jenkins controllers managed by Varroa. Returns a compact "+
			"summary per controller (name, namespace, cluster, phase, version, "+
			"powerState, createdAt, lastReconcileError) — enough to survey a fleet and spot "+
			"what is broken. Use get_controller for the full resource."),
		mcp.WithString("namespace", mcp.Description("Optional namespace to filter by")),
		mcp.WithString("cluster", mcp.Description("Optional cluster to filter by (default: local cluster)")),
		mcp.WithBoolean("verbose", mcp.Description("Return full Controller resources instead of "+
			"summaries. Expensive: a full resource is roughly 100x the size of a summary, so a "+
			"fleet-sized listing can exhaust the context window. Prefer get_controller for detail "+
			"on specific controllers.")),
		withListOutput(controllerListOutputSchema),
	)
	addTool(mcpServer, kindRead, listC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		ns := strArg(args, "namespace")
		clusterFilter := strArg(args, "cluster")
		verbose, _ := args["verbose"].(bool)

		// Collect (cluster, CR) pairs first: cluster identity lives on the brood
		// wrapper, not the CR, so it has to be carried alongside rather than read
		// back out at projection time.
		type entry struct {
			cluster string
			cr      *v1alpha1.Controller
		}
		var entries []entry

		if deps.Brood != nil {
			cc, _, err := deps.Brood.ListAll(ctx, ns, clusterFilter)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to list controllers: %v", err)), nil
			}
			claims, _ := requireClaims(ctx)
			for _, c := range cc {
				if deps.Authorizer != nil && claims != nil &&
					!deps.Authorizer.CanReadController(claims, c.CR.Namespace, c.CR.Name) {
					continue
				}
				entries = append(entries, entry{cluster: c.Cluster, cr: c.CR})
			}
		} else {
			// Fallback: direct local list.
			controllers, err := crdstore.List[v1alpha1.Controller](ctx, deps.Store, ns, "")
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to list controllers: %v", err)), nil
			}
			claims, _ := requireClaims(ctx)
			for _, c := range controllers {
				if deps.Authorizer != nil && claims != nil &&
					!deps.Authorizer.CanReadController(claims, c.Namespace, c.Name) {
					continue
				}
				// Cluster is left empty rather than echoing the caller's filter.
				// Without a brood there is no authoritative cluster name to
				// report, and labelling local results with a caller-supplied
				// string would let `cluster: "prod"` come back stamped "prod".
				entries = append(entries, entry{cr: c})
			}
		}

		if verbose {
			full := make([]*v1alpha1.Controller, 0, len(entries))
			for _, e := range entries {
				full = append(full, e.cr)
			}
			return resultJSON(full)
		}
		summaries := make([]controllerSummary, 0, len(entries))
		for _, e := range entries {
			summaries = append(summaries, summarizeController(e.cluster, e.cr))
		}
		return resultJSON(summaries)
	})

	getC := mcp.NewTool("get_controller",
		mcp.WithDescription("Get a specific Jenkins controller by namespace and name"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindRead, getC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if deps.Authorizer != nil {
			claims, err := requireClaims(ctx)
			if err != nil {
				return mcp.NewToolResultError("authentication required"), nil
			}
			if !deps.Authorizer.CanReadController(claims, ns, name) {
				return mcp.NewToolResultError("access denied: missing controllers:read permission"), nil
			}
		}
		var cr *v1alpha1.Controller
		var err error
		if deps.Brood != nil {
			cr, err = deps.Brood.Get(ctx, cluster, ns, name)
		} else {
			cr, err = crdstore.Get[v1alpha1.Controller](ctx, deps.Store, name, ns)
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("controller not found: %v", err)), nil
		}
		return resultJSON(cr)
	})

	createC := mcp.NewTool("create_controller",
		mcp.WithDescription("Create a new Jenkins controller. Omitting version pins the "+
			"embedded plugin-lock baseline rather than the newest Jenkins — pass an explicit "+
			"version to select an LTS line. Omitting composedBundleRef uses the built-in "+
			"varroa-starter bundle; create a ComposedBundle first only if the controller needs "+
			"custom JCasC, plugins, or catalog items. hibernation and spec set the same fields "+
			"update_controller accepts, so a controller can be created already configured."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
		mcp.WithString("version", mcp.Description("Jenkins version")),
		mcp.WithString("composedBundleRef", mcp.Description("ComposedBundle name to attach")),
		mcp.WithString("powerState", mcp.Description("Power state: Running or Stopped")),
		mcp.WithObject("hibernation", mcp.Description("Auto-hibernation settings: enabled (bool), "+
			"gracePeriodMinutes (int, minimum 5), activityIgnoreRegex (string)")),
		mcp.WithObject("spec", mcp.Description("Any other Controller spec fields as a merge "+
			"patch, e.g. {\"resources\":{...},\"ingressSpec\":{...},\"probes\":{...}}. The "+
			"shorthand arguments above win over the same key here.")),
	)
	addTool(mcpServer, kindCreate, createC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanCreateController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:create permission"), nil
		}
		if msg := unknownControllerArgsError(args, nil); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
		cr := &v1alpha1.Controller{ObjectMeta: objMeta(args)}
		// Same patch shape as update_controller, so a field settable on an
		// existing controller is settable at creation too.
		if spec, err := controllerSpecPatch(args); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		} else if len(spec) > 0 {
			merged, err := mergeControllerSpec(cr, spec)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to apply spec patch: %v", err)), nil
			}
			cr = merged
		}
		// Same checks, in the same order, as handleCreateController runs before
		// its own brood branch: ingress rules first (path mode is local-cluster
		// only), then the bundle reference — and that one only for a local
		// target, since ComposedBundles are cluster-local and the target
		// cluster's operator validates remote refs authoritatively. spec can
		// carry both of those fields now, so skipping this would let MCP create
		// a controller REST rejects.
		svc := controllerSvc(deps)
		if serr := svc.ValidateIngress(cr.Spec.IngressSpec, true, cluster, name); serr != nil {
			return mcp.NewToolResultError(serr.Message), nil
		}
		if cluster == deps.Brood.LocalCluster() {
			if serr := svc.ValidateBundleRef(ctx, cr.Spec.ComposedBundleRef, ns); serr != nil {
				return mcp.NewToolResultError(serr.Message), nil
			}
		}
		crJSON, _ := json.Marshal(cr)
		// Namespace is not carried on the marshalled CR: the operator sets
		// cr.Namespace from req.Namespace, so omitting it here creates the
		// object in the empty namespace and the API server 404s.
		created, checks, err := deps.Brood.Create(ctx, cluster, api.ControllersCreateArgs{Namespace: ns, Controller: crJSON})
		if err != nil {
			return broodFailureToolError("create controller", err, checks), nil
		}
		emitActivityForCluster(deps, claims, cluster, activity.Event{
			Type:       "controller.created",
			Message:    "Controller " + name + " created in " + ns,
			Namespace:  ns,
			Controller: name,
		})
		return resultJSON(created)
	})

	updateC := mcp.NewTool("update_controller",
		mcp.WithDescription("Update an existing Jenkins controller. Only the fields you supply "+
			"are patched; omitted ones keep their current values. version, composedBundleRef, "+
			"powerState and hibernation are shorthands for the matching spec fields, and spec "+
			"carries any other mutable Controller spec field."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
		mcp.WithString("version", mcp.Description("Jenkins version")),
		mcp.WithString("composedBundleRef", mcp.Description("ComposedBundle name to attach")),
		mcp.WithString("powerState", mcp.Description("Power state: Running or Stopped")),
		mcp.WithObject("hibernation", mcp.Description("Auto-hibernation settings: enabled (bool), "+
			"gracePeriodMinutes (int, minimum 5), activityIgnoreRegex (string)")),
		mcp.WithObject("spec", mcp.Description("Any other mutable Controller spec fields as a "+
			"merge patch, e.g. {\"resources\":{...},\"ingressSpec\":{...},\"probes\":{...}}. The "+
			"shorthand arguments above win over the same key here.")),
		mcp.WithBoolean("force", mcp.Description("Take ownership of fields currently owned by "+
			"another field manager (e.g. one created by kubectl patch/apply), which otherwise "+
			"fail the update with a field conflict. Leave unset first: the conflict error names "+
			"the fields and their owner, so you can decide whether overriding them is safe.")),
	)
	addTool(mcpServer, kindUpdate, updateC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := getArgs(req)
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanUpdateController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:update permission"), nil
		}
		if msg := unknownControllerArgsError(args, controllerUpdateOnlyArgs); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
		spec, err := controllerSpecPatch(args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(spec) == 0 {
			return mcp.NewToolResultError(noSpecFields), nil
		}
		// force rides the brood path exactly as REST's ?force=true. It is an
		// SSA concept: the operator's ApplyControllerSpecSSA owns the fields it
		// submits under varroa-ui, and a conflicting foreign manager is the
		// only thing force overrides.
		force, _ := args["force"].(bool)
		patch, _ := json.Marshal(map[string]interface{}{"spec": spec})
		// Field manager stays "varroa-ui": ApplyControllerSpecSSA completes
		// the patch with the leaves THIS manager already owns (read from
		// metadata.managedFields). A different name would match no Apply
		// entry, own nothing, and the re-apply would release every field
		// the UI's manager previously owned. Every field reachable through
		// the spec patch therefore becomes varroa-ui-owned on first write,
		// exactly as it does through the REST endpoint.
		updated, checks, _, err := deps.Brood.Update(ctx, cluster, ns, name, patch, "varroa-ui", force)
		if err != nil {
			return broodFailureToolError("update controller", err, checks), nil
		}
		emitActivityForCluster(deps, claims, cluster, activity.Event{
			Type:       "controller.updated",
			Message:    "Controller " + name + " updated in " + ns,
			Namespace:  ns,
			Controller: name,
		})
		return resultJSON(updated)
	})

	deleteC := mcp.NewTool("delete_controller",
		mcp.WithDescription("Delete a Jenkins controller and all its resources"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindDelete, deleteC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanDeleteController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:delete permission"), nil
		}
		if deps.Brood != nil {
			if err := deps.Brood.Delete(ctx, cluster, ns, name); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to delete controller: %v", err)), nil
			}
			emitActivityForCluster(deps, claims, cluster, activity.Event{
				Type:       "controller.deleted",
				Message:    "Controller " + name + " deleted in " + ns,
				Namespace:  ns,
				Controller: name,
			})
			return mcp.NewToolResultText("controller " + ns + "/" + name + " deleted"), nil
		}
		if err := crdstore.Delete[v1alpha1.Controller](ctx, deps.Store, name, ns); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to delete controller: %v", err)), nil
		}
		emitActivity(deps, claims, activity.Event{
			Type:       "controller.deleted",
			Message:    "Controller " + name + " deleted in " + ns,
			Namespace:  ns,
			Controller: name,
		})
		return mcp.NewToolResultText("controller " + ns + "/" + name + " deleted"), nil
	})

	reconcileC := mcp.NewTool("reconcile_controller",
		mcp.WithDescription("Trigger an on-demand reconciliation: re-applies desired state "+
			"(JCasC, RBAC, plugins) to a running controller without restarting Jenkins. Use this "+
			"when configuration has changed but the controller has not picked it up. Use "+
			"restart_controller instead when the Jenkins process itself needs to come back."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindAction, reconcileC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanApproveRestart(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:approve-restart permission"), nil
		}
		if deps.Reconciler == nil {
			return mcp.NewToolResultError("reconciler not available"), nil
		}
		deps.Reconciler.TriggerReconcile(cluster, name, ns)
		emitActivityForCluster(deps, claims, cluster, activity.Event{
			Type:       "reconcile.triggered",
			Message:    "Controller " + name + " reconcile triggered in " + ns,
			Namespace:  ns,
			Controller: name,
		})
		return mcp.NewToolResultText("reconciliation triggered for " + ns + "/" + name), nil
	})

	restartC := mcp.NewTool("restart_controller",
		mcp.WithDescription("Safe-restart the Jenkins process on a controller: waits for running "+
			"builds to finish before restarting. Does not re-apply configuration — use "+
			"reconcile_controller for that."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindAction, restartC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanManageController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:manage permission"), nil
		}
		if deps.Reconciler == nil {
			return mcp.NewToolResultError("reconciler not available"), nil
		}
		if err := deps.Reconciler.ApproveRestart(ctx, cluster, ns, name, "restart"); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to restart controller: %v", err)), nil
		}
		emitActivityForCluster(deps, claims, cluster, activity.Event{
			Type:       "restart.approved",
			Message:    "Controller " + name + " restart approved in " + ns,
			Namespace:  ns,
			Controller: name,
		})
		return mcp.NewToolResultText("restart triggered for " + ns + "/" + name), nil
	})

	hibernateC := mcp.NewTool("hibernate_controller",
		mcp.WithDescription("Hibernate a Jenkins controller: scale it down and park it until "+
			"it is woken. Independent of the controller's automatic hibernation setting. Refused "+
			"when the controller's powerState is Stopped."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindAction, hibernateC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanManageController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:manage permission"), nil
		}
		if deps.Reconciler == nil {
			return mcp.NewToolResultError("reconciler not available"), nil
		}
		if err := deps.Reconciler.Hibernate(ctx, cluster, ns, name); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to hibernate controller: %v", err)), nil
		}
		emitActivityForCluster(deps, claims, cluster, activity.Event{
			Type:       "controller.hibernated",
			Message:    "Controller " + name + " hibernated in " + ns,
			Namespace:  ns,
			Controller: name,
		})
		return mcp.NewToolResultText("hibernation triggered for " + ns + "/" + name), nil
	})

	wakeC := mcp.NewTool("wake_controller",
		mcp.WithDescription("Wake a hibernated Jenkins controller: clear its hibernation and "+
			"re-provision it. Refused when the controller's powerState is Stopped."),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithString("cluster", mcp.Description("Cluster (default: local cluster)")),
	)
	addTool(mcpServer, kindAction, wakeC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorizer not configured"), nil
		}
		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		cluster := clusterOrDefault(args)
		if !deps.Authorizer.CanManageController(claims, ns, name) {
			return mcp.NewToolResultError("access denied: missing controllers:manage permission"), nil
		}
		if deps.Reconciler == nil {
			return mcp.NewToolResultError("reconciler not available"), nil
		}
		if err := deps.Reconciler.Wake(ctx, cluster, ns, name); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to wake controller: %v", err)), nil
		}
		emitActivityForCluster(deps, claims, cluster, activity.Event{
			Type:       "controller.woken",
			Message:    "Controller " + name + " woken in " + ns,
			Namespace:  ns,
			Controller: name,
		})
		return mcp.NewToolResultText("wake triggered for " + ns + "/" + name), nil
	})

	logsC := mcp.NewTool("get_controller_logs",
		mcp.WithDescription("Get recent logs from a Jenkins controller pod"),
		mcp.WithString("namespace", mcp.Required(), mcp.Description("Controller namespace")),
		mcp.WithString("name", mcp.Required(), mcp.Description("Controller name")),
		mcp.WithNumber("tailLines", mcp.Description("Number of log lines (default 100, max 500)")),
	)
	addTool(mcpServer, kindRead, logsC, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if deps.Client == nil {
			return mcp.NewToolResultError("kubernetes client not configured"), nil
		}
		args := getArgs(req)
		ns, name := strArg(args, "namespace"), strArg(args, "name")
		if deps.Authorizer != nil {
			claims, err := requireClaims(ctx)
			if err != nil {
				return mcp.NewToolResultError("authentication required"), nil
			}
			if !deps.Authorizer.CanReadController(claims, ns, name) {
				return mcp.NewToolResultError("access denied: missing controllers:read permission"), nil
			}
		}
		tailLines := int64(100)
		if n, ok := args["tailLines"].(float64); ok {
			tailLines = int64(n)
			if tailLines < 1 {
				tailLines = 1
			}
			if tailLines > 500 {
				tailLines = 500
			}
		}
		cr, err := crdstore.Get[v1alpha1.Controller](ctx, deps.Store, name, ns)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("controller not found: %v", err)), nil
		}
		uid := string(cr.UID)
		if len(uid) > 8 {
			uid = uid[:8]
		}
		podName := cr.Name + "-" + uid + "-0"
		rc, err := deps.Client.StreamPodLogs(ctx, ns, podName, "jenkins", tailLines, false)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to stream logs: %v", err)), nil
		}
		defer func() { _ = rc.Close() }()
		var buf strings.Builder
		if _, err := io.Copy(&buf, rc); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("error reading logs: %v", err)), nil
		}
		return mcp.NewToolResultText(buf.String()), nil
	})
}

// controllerSvc mirrors api.Server.controllerSvc so the MCP tools validate a
// controller through the same service the REST handlers use. Sharing it rather
// than re-deriving the rules is the point: spec now carries ingressSpec and
// composedBundleRef, and a second copy of those checks would drift.
func controllerSvc(deps *api.Dependencies) *api.ControllerService {
	local := ""
	if deps.Brood != nil {
		local = deps.Brood.LocalCluster()
	}
	// Invariant (5): the BFF wires optional Dependencies conditionally, and the
	// service logs on both validation failure paths — a nil Logger would panic
	// the request instead of returning the validation error.
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &api.ControllerService{
		Store:             deps.Store,
		Client:            deps.Client,
		LocalCluster:      local,
		DashboardHost:     deps.DashboardHost,
		OperatorNamespace: deps.OperatorNamespace,
		ManagedNamespaces: deps.ManagedNamespaces,
		Logger:            logger,
	}
}

// controllerSpecArgs are the arguments create_controller and update_controller
// understand: the addressing ones plus everything controllerSpecPatch reads.
// Any other argument is rejected by unknownControllerArgsError before a patch
// is built, so extending this allowlist means teaching controllerSpecPatch to
// read the new key too — otherwise it is accepted and then ignored.
var controllerSpecArgs = map[string]bool{
	"namespace": true, "name": true, "cluster": true,
	"version": true, "composedBundleRef": true, "powerState": true,
	"hibernation": true, "spec": true,
}

// controllerUpdateOnlyArgs are arguments update_controller understands and
// create_controller does not. They control HOW the patch is applied rather than
// what it contains, so controllerSpecPatch deliberately never reads them — the
// "accepted and then ignored" trap the allowlist comment warns about does not
// apply here. Keeping them out of controllerSpecArgs is what makes
// create_controller still reject `force` by name instead of swallowing it.
var controllerUpdateOnlyArgs = map[string]bool{"force": true}

// validControllerPowerStates are the spec.powerState values the contract
// accepts. Hibernated is deliberately absent: hibernation is reported in
// status, not requested in spec, and is driven by the hibernate/wake actions.
var validControllerPowerStates = map[string]bool{
	"":        true,
	"Running": true,
	"Stopped": true,
}

// validatePowerStateShorthand rejects a powerState argument the CRD enum would
// refuse, with a Hibernated-specific message pointing at the action tools.
func validatePowerStateShorthand(ps string) error {
	if validControllerPowerStates[ps] {
		return nil
	}
	if ps == "Hibernated" {
		return fmt.Errorf("powerState \"Hibernated\" is not a power state — hibernation is reported in status; use hibernate_controller to hibernate and wake_controller to wake")
	}
	return fmt.Errorf("invalid powerState %q: must be Running or Stopped", ps)
}

// controllerSpecPatch builds the spec merge patch from the arguments the caller
// actually supplied. Absent arguments are omitted rather than zeroed: a merge
// patch carrying "version": "" would blank a live controller's pinned Jenkins
// version, and every field reachable through spec carries the same trap.
func controllerSpecPatch(args map[string]any) (map[string]interface{}, error) {
	spec := map[string]interface{}{}
	// The generic passthrough goes first so a named shorthand wins when both
	// carry the same key.
	if raw, ok := mapArg(args, "spec"); ok {
		for k, v := range raw {
			spec[k] = v
		}
	}
	if v := strArg(args, "version"); v != "" {
		spec["version"] = v
	}
	if ref := strArg(args, "composedBundleRef"); ref != "" {
		spec["composedBundleRef"] = map[string]interface{}{"name": ref}
	}
	if ps := strArg(args, "powerState"); ps != "" {
		if err := validatePowerStateShorthand(ps); err != nil {
			return nil, err
		}
		spec["powerState"] = ps
	}
	if hib, ok := mapArg(args, "hibernation"); ok {
		spec["hibernation"] = hib
	}
	return spec, nil
}

// noSpecFields is the error for an update that would patch nothing.
const noSpecFields = "nothing to update: supply version, composedBundleRef, " +
	"powerState, hibernation, or a spec merge patch"

// unknownControllerArgsError rejects arguments the tool does not understand,
// naming them. Dropping them silently is the #512 failure mode itself: a
// `hibernation` argument vanished into a generic "nothing to update", which
// read as a malformed request rather than an unsupported field. Rejecting
// rather than warning also covers the harder half — a misplaced argument
// alongside a valid one, where the call otherwise succeeds and the caller has
// no way to learn that half of what they sent never landed.
// extra names arguments valid for this tool only; pass nil when there are none.
func unknownControllerArgsError(args map[string]any, extra map[string]bool) string {
	var unknown []string
	for k := range args {
		if !controllerSpecArgs[k] && !extra[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	sort.Strings(unknown)
	return "unrecognized argument(s) " + strings.Join(unknown, ", ") +
		": only version, composedBundleRef, powerState and hibernation are named " +
		"arguments — every other Controller spec field goes inside spec"
}

// mergeControllerSpec applies a spec merge patch to a Controller in-process,
// for the local-direct paths that have no brood to send a patch to. It
// round-trips through JSON so nested objects merge key by key, exactly as
// handleUpdateController's local branch does, rather than replacing whole
// structs the way a typed assignment would.
//
// An explicit null clears the key it names, whatever its shape. That does not
// follow from encoding/json — a null unmarshals into a non-pointer field as a
// no-op — but works here because the merge happens in map space and the
// Controller is rebuilt from the merged map, so a nulled key has nothing left
// to carry over.
func mergeControllerSpec(cr *v1alpha1.Controller, spec map[string]interface{}) (*v1alpha1.Controller, error) {
	crJSON, err := json.Marshal(cr)
	if err != nil {
		return nil, err
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(crJSON, &merged); err != nil {
		return nil, err
	}
	controller.MergeMap(merged, map[string]interface{}{"spec": spec})
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	var out v1alpha1.Controller
	if err := json.Unmarshal(mergedJSON, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

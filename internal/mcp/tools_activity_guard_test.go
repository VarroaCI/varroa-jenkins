package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// sourceOverride pins the expected activity Source for mutating tools whose
// underlying shared business logic stamps Source itself rather than
// delegating to emitActivity's "mcp" default. promote_version_candidate calls
// api.PromoteVersionCandidate — shared with the HTTP promote handler — which
// stamps Source: "operator" per
// specs/profile-candidate-promotion/spec.md: the event describes the
// operator-owned CRD state transition, with the actor recorded in Message
// instead, so an HTTP-triggered and an MCP-triggered promotion produce
// byte-identical events.
var sourceOverride = map[string]string{
	"promote_version_candidate": "operator",
}

func expectedSourceFor(name string) string {
	if s, ok := sourceOverride[name]; ok {
		return s
	}
	return "mcp"
}

// guardConfigBrood implements the five ConfigBrood methods the mutating tools
// reach. It embeds the interface so the other 28 need no body; an unexpected
// call nil-panics, which the per-tool subtest isolates to one row.
type guardConfigBrood struct{ api.ConfigBrood }

func (g *guardConfigBrood) SyncCatalogSource(_ context.Context, _, _, _ string) error { return nil }

func (g *guardConfigBrood) CreateVersionProfile(_ context.Context, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}

func (g *guardConfigBrood) UpdateVersionProfile(_ context.Context, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}

func (g *guardConfigBrood) DeleteVersionProfile(_ context.Context, _, _ string) error { return nil }

func (g *guardConfigBrood) UpdateProvisioningDefaults(_ context.Context, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}

// guardReconciler implements controller.ReconcilerAPI for the two controller
// action tools. Every method is a no-op that reports success so the tool
// reaches its emit call.
type guardReconciler struct{}

func (g *guardReconciler) TriggerReconcile(_, _, _ string) {}
func (g *guardReconciler) WakeController(_, _, _ string)   {}
func (g *guardReconciler) Reprovision(_, _, _ string)      {}

func (g *guardReconciler) ApproveRestart(_ context.Context, _, _, _, _ string) error { return nil }
func (g *guardReconciler) ApproveDeletion(_ context.Context, _, _, _, _ string) error {
	return nil
}
func (g *guardReconciler) Hibernate(_ context.Context, _, _, _ string) error { return nil }
func (g *guardReconciler) Wake(_ context.Context, _, _, _ string) error {
	return nil
}

// guardEmitDeps wires every dependency a mutating tool can reach, plus the
// recording sink. Tools are driven through recordingBrood rather than the
// store fallback, which means preflight.Run is skipped for the controller
// tools — the same shape a production server takes.
func guardEmitDeps(t *testing.T, sink activity.EventSink) *api.Dependencies {
	t.Helper()
	stub := newGuardStub()
	seedGuardObjects(t, stub)
	return &api.Dependencies{
		Client:      stub,
		Store:       stub,
		Authorizer:  guardAuthorizer(),
		Brood:       &recordingBrood{},
		ConfigBrood: &guardConfigBrood{},
		Reconciler:  &guardReconciler{},
		VersionProfileReconciler: controller.NewJenkinsVersionProfileReconciler(
			stub, stub, "varroa-system", slog.New(slog.DiscardHandler)),
		ActivityPublisher: sink,
		OperatorNamespace: "varroa-system",
	}
}

// seedGuardObjects seeds the target object for every store-based update tool,
// each of which does a crdstore.Get and fails "not found" before reaching its
// emit call. Deletes need no seeding — crdstore.Delete treats a missing object
// as success. update_provisioning_defaults also needs none: it substitutes a
// fresh object when the Get misses.
func seedGuardObjects(t *testing.T, stub *guardStub) {
	t.Helper()
	const ns, name = "team-a", "obj"
	crdstore.MustSeed(stub.Fake,
		&v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
	)

	// promote_version_candidate additionally needs a Ready ProfileCandidate,
	// its target JenkinsVersionProfile, and both ConfigMaps the promotion
	// sequence reads/overwrites — none of which any other guarded tool touches.
	const pluginSetName, closureName = "obj-pluginset", "obj-closure"
	stub.configMaps[pluginSetName] = map[string]string{
		"plugins.yaml": "core:\n  - \"2.500.1\"\nplugins:\n  - artifactId: git\n    version: \"5.0.0\"\n",
	}
	stub.configMaps[closureName] = map[string]string{
		"plugins.yaml": "core:\n  - \"2.500.2\"\nplugins:\n  - artifactId: git\n    version: \"5.1.0\"\n",
	}
	crdstore.MustSeed(stub.Fake,
		&v1alpha1.JenkinsVersionProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.JenkinsVersionProfileSpec{
				ResolveVersion: "2.500.1",
				PluginSetRef:   &v1alpha1.ConfigMapRef{Name: pluginSetName},
			},
		},
		&v1alpha1.ProfileCandidate{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.ProfileCandidateSpec{
				ProfileRef:        name,
				ObservedVersion:   "2.500.1",
				ResolveVersion:    "2.500.2",
				ClosureContentRef: closureName,
			},
			Status: v1alpha1.ProfileCandidateStatus{Phase: v1alpha1.ProfileCandidatePhaseReady},
		},
	)
}

// guardArgsFor returns the minimal valid argument map for each mutating tool,
// matching the tool's required parameters declared in its mcp.WithX(..., mcp.Required()).
// update_controller additionally needs a field, or the handler errors "nothing
// to update" before reaching the emit. A tool with no entry panics — a missing
// row must fail loudly rather than drift through schema validation silently.
func guardArgsFor(name string) map[string]interface{} {
	ns, obj := "team-a", "obj"
	subject := []interface{}{map[string]interface{}{"kind": "User", "name": "alice"}}
	byName := map[string]map[string]interface{}{
		// Controllers (brood path)
		"create_controller":    {"namespace": ns, "name": obj},
		"update_controller":    {"namespace": ns, "name": obj, "version": "2.500.1"},
		"delete_controller":    {"namespace": ns, "name": obj},
		"reconcile_controller": {"namespace": ns, "name": obj},
		"restart_controller":   {"namespace": ns, "name": obj},
		"hibernate_controller": {"namespace": ns, "name": obj},
		"wake_controller":      {"namespace": ns, "name": obj},
		// VarroaRoles (cluster-scoped)
		"create_varroa_role": {"name": obj},
		"update_varroa_role": {"name": obj},
		"delete_varroa_role": {"name": obj},
		// VarroaRoleBindings (cluster-scoped)
		"create_varroa_role_binding": {"name": obj, "roleRef": "some-role", "subjects": subject},
		"update_varroa_role_binding": {"name": obj},
		"delete_varroa_role_binding": {"name": obj},
		// JenkinsRoles (cluster-scoped)
		"create_jenkins_role": {"name": obj, "permissions": []interface{}{"Overall/Read"}},
		"update_jenkins_role": {"name": obj},
		"delete_jenkins_role": {"name": obj},
		// JenkinsRoleBindings (cluster-scoped)
		"create_jenkins_role_binding": {"name": obj, "roleRef": "some-role", "subjects": subject},
		"update_jenkins_role_binding": {"name": obj},
		"delete_jenkins_role_binding": {"name": obj},
		// CatalogSources (namespaced)
		"create_catalog_source": {"namespace": ns, "name": obj, "repoURL": "https://example.com/repo.git"},
		"update_catalog_source": {"namespace": ns, "name": obj},
		"delete_catalog_source": {"namespace": ns, "name": obj},
		"sync_catalog_source":   {"namespace": ns, "name": obj},
		// ComposedBundles (namespaced)
		"create_composed_bundle": {"namespace": ns, "name": obj, "inputs": []interface{}{map[string]interface{}{"itemRef": map[string]interface{}{"name": "cat-item"}}}},
		"update_composed_bundle": {"namespace": ns, "name": obj},
		"delete_composed_bundle": {"namespace": ns, "name": obj},
		// ProvisioningDefaults + JenkinsVersionProfiles (cluster-scoped, config brood)
		"update_provisioning_defaults":   {"name": obj},
		"create_jenkins_version_profile": {"name": obj},
		"update_jenkins_version_profile": {"name": obj},
		"delete_jenkins_version_profile": {"name": obj},
		// Users (namespaced, admin-only)
		"create_user": {"namespace": ns, "name": obj},
		"update_user": {"namespace": ns, "name": obj},
		"delete_user": {"namespace": ns, "name": obj},
		// Groups (cluster-scoped, admin-only)
		"create_group": {"name": obj},
		"delete_group": {"name": obj},
		// ProfileCandidates (cluster-scoped, direct store)
		"promote_version_candidate": {"name": obj},
	}
	args, ok := byName[name]
	if !ok {
		panic("guardArgsFor: no arguments defined for mutating tool " + name)
	}
	return args
}

// TestEveryMutatingToolEmitsActivity is the audit guard: a mutating tool that
// does not publish an activity event fails here. It is a runtime test rather
// than a static one because deletes and action tools return
// NewToolResultText and never pass through resultJSON, so there is no shared
// return expression to inspect.
func TestEveryMutatingToolEmitsActivity(t *testing.T) {
	for name, kind := range expectedToolKinds {
		if !kind.mutates() {
			continue
		}
		t.Run(name, func(t *testing.T) {
			sink := &recordingSink{}
			deps := guardEmitDeps(t, sink)
			handler := NewHandler(deps)

			resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
				"name":      name,
				"arguments": guardArgsFor(name),
			}, guardClaims)

			b, err := json.Marshal(resp.Result)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			if strings.Contains(string(b), `"isError":true`) {
				t.Fatalf("tool returned an error, cannot assert emission: %s", b)
			}
			if len(sink.events) != 1 {
				t.Fatalf("published %d events, want exactly 1", len(sink.events))
			}
			e := sink.events[0]
			if e.Type == "" {
				t.Error("event Type is empty")
			}
			if e.Message == "" {
				t.Error("event Message is empty")
			}
			if want := expectedSourceFor(name); e.Source != want {
				t.Errorf("Source = %q, want %s", e.Source, want)
			}
			if e.Actor == "" {
				t.Error("event Actor is empty")
			}
		})
	}
}

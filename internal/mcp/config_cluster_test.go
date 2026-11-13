package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// memberBrood knows the local cluster core and one member, member-b.
type memberBrood struct{ api.Brood }

func (memberBrood) LocalCluster() string { return "core" }

func (memberBrood) IsKnown(_ context.Context, c string) bool { return c == "core" || c == "member-b" }

func (memberBrood) Clusters(context.Context) ([]api.ClusterInfo, error) {
	return []api.ClusterInfo{
		{ClusterInfo: bus.ClusterInfo{Name: "core"}, Core: true, Healthy: true},
		{ClusterInfo: bus.ClusterInfo{Name: "member-b", OperatorNamespace: "varroa-member"}, Healthy: true},
	}, nil
}

// memberConfig serves the config kinds from fake stores, as the operators of
// the clusters would. A write on core changes local and appends to localWrites.
// A write on any other cluster changes store and appends to updates.
type memberConfig struct {
	api.ConfigBrood
	store       *crdstore.Fake
	local       *crdstore.Fake
	unreachable bool
	localDown   bool
	updates     []string
	localWrites []string
	composed    []string
}

var errLocalDown = &api.ErrClusterUnreachable{Cluster: "core", Err: errors.New("no responders")}

// on returns the store and the write log of the operator of cluster, or the
// error of an operator that does not answer.
func (m *memberConfig) on(cluster string) (*crdstore.Fake, *[]string, error) {
	if cluster == "core" {
		if m.localDown {
			return nil, nil, errLocalDown
		}
		return m.local, &m.localWrites, nil
	}
	if m.unreachable {
		return nil, nil, errMemberDown
	}
	return m.store, &m.updates, nil
}

// withLocalOperator wires a ConfigBrood into deps. Its local operator writes
// the store of deps, as the operator of the local cluster does over the bus.
func withLocalOperator(deps *api.Dependencies) {
	deps.ConfigBrood = &memberConfig{store: crdstore.NewFake(), local: deps.Store.(*crdstore.Fake)}
}

func memberGet[T any](ctx context.Context, store *crdstore.Fake, name, namespace string) (json.RawMessage, error) {
	obj, err := crdstore.Get[T](ctx, store, name, namespace)
	if apierrors.IsNotFound(err) {
		return nil, &api.BroodError{Code: bus.CodeNotFound, Msg: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

func memberUpdate[T any](ctx context.Context, store *crdstore.Fake, body json.RawMessage) (json.RawMessage, error) {
	var obj T
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	err := crdstore.ApplyAt[T](ctx, store, &obj)
	if apierrors.IsConflict(err) {
		return nil, &api.BroodError{Code: bus.CodeConflict, Msg: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

var errMemberDown = &api.ErrClusterUnreachable{Cluster: "member-b", Err: errors.New("no responders")}

func (m *memberConfig) GetComposedBundle(ctx context.Context, _, ns, name string) (json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberGet[v1alpha1.ComposedBundle](ctx, m.store, name, ns)
}

func (m *memberConfig) UpdateComposedBundle(ctx context.Context, cluster, _, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "composedbundle")
	return memberUpdate[v1alpha1.ComposedBundle](ctx, store, body)
}

func (m *memberConfig) GetCatalogSource(ctx context.Context, _, ns, name string) (json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberGet[v1alpha1.CatalogSource](ctx, m.store, name, ns)
}

func (m *memberConfig) UpdateCatalogSource(ctx context.Context, cluster, _, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "catalogsource")
	return memberUpdate[v1alpha1.CatalogSource](ctx, store, body)
}

func (m *memberConfig) GetJenkinsRole(ctx context.Context, _, name string) (json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberGet[v1alpha1.JenkinsRole](ctx, m.store, name, "")
}

func (m *memberConfig) UpdateJenkinsRole(ctx context.Context, cluster, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "jenkinsrole")
	return memberUpdate[v1alpha1.JenkinsRole](ctx, store, body)
}

func (m *memberConfig) GetJenkinsRoleBinding(ctx context.Context, _, name string) (json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberGet[v1alpha1.JenkinsRoleBinding](ctx, m.store, name, "")
}

func (m *memberConfig) UpdateJenkinsRoleBinding(ctx context.Context, cluster, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "jenkinsrolebinding")
	return memberUpdate[v1alpha1.JenkinsRoleBinding](ctx, store, body)
}

// GetProvisioningDefaults reads core from local and any other cluster from
// store. The real config brood reads the local cluster directly, so a down
// local operator does not fail this read.
func (m *memberConfig) GetProvisioningDefaults(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	if cluster == "core" {
		return memberGet[v1alpha1.ProvisioningDefaults](ctx, m.local, name, "")
	}
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberGet[v1alpha1.ProvisioningDefaults](ctx, m.store, name, "")
}

func (m *memberConfig) UpdateProvisioningDefaults(ctx context.Context, cluster, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "provisioningdefaults")
	return memberUpdate[v1alpha1.ProvisioningDefaults](ctx, store, body)
}

// memberDeps starts from the admin deps. Its local store is core; the member
// store is empty and reached only through memberConfig.
func memberDeps(t *testing.T) (*api.Dependencies, *crdstore.Fake, *memberConfig, *recordingSink) {
	t.Helper()
	deps := adminDeps()
	local := deps.Store.(*crdstore.Fake)
	member := &memberConfig{store: crdstore.NewFake(), local: local}
	sink := &recordingSink{}
	deps.Brood = memberBrood{}
	deps.ConfigBrood = member
	deps.ActivityPublisher = sink
	return deps, local, member, sink
}

// configKindCase describes one config kind with its update and get tools. desc
// is the field that each test changes, read back by read.
type configKindCase struct {
	name    string
	update  string
	get     string
	args    map[string]any
	seed    func(t *testing.T, store *crdstore.Fake, desc string)
	read    func(t *testing.T, store *crdstore.Fake) string
	setArgs map[string]any
}

func configKinds() []configKindCase {
	return []configKindCase{
		{
			name: "composed bundle", update: "update_composed_bundle", get: "get_composed_bundle",
			args: map[string]any{"namespace": "ns", "name": "cb1"},
			seed: func(t *testing.T, s *crdstore.Fake, desc string) {
				crdstore.MustSeed(s, &v1alpha1.ComposedBundle{
					ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"},
					Spec:       v1alpha1.ComposedBundleSpec{DisplayName: desc},
				})
			},
			read: func(t *testing.T, s *crdstore.Fake) string {
				o, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), s, "cb1", "ns")
				if err != nil {
					t.Fatal(err)
				}
				return o.Spec.DisplayName
			},
			setArgs: map[string]any{"displayName": "changed"},
		},
		{
			name: "catalog source", update: "update_catalog_source", get: "get_catalog_source",
			args: map[string]any{"namespace": "ns", "name": "cs1"},
			seed: func(t *testing.T, s *crdstore.Fake, desc string) {
				crdstore.MustSeed(s, &v1alpha1.CatalogSource{
					ObjectMeta: metav1.ObjectMeta{Name: "cs1", Namespace: "ns"},
					Spec:       v1alpha1.CatalogSourceSpec{RepoURL: "https://example.test/repo", Path: desc},
				})
			},
			read: func(t *testing.T, s *crdstore.Fake) string {
				o, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), s, "cs1", "ns")
				if err != nil {
					t.Fatal(err)
				}
				return o.Spec.Path
			},
			setArgs: map[string]any{"path": "changed"},
		},
		{
			name: "Jenkins role", update: "update_jenkins_role", get: "get_jenkins_role",
			args: map[string]any{"name": "r1"},
			seed: func(t *testing.T, s *crdstore.Fake, desc string) {
				crdstore.MustSeed(s, &v1alpha1.JenkinsRole{
					ObjectMeta: metav1.ObjectMeta{Name: "r1"},
					Spec:       v1alpha1.JenkinsRoleSpec{RoleType: "Global", Description: desc},
				})
			},
			read: func(t *testing.T, s *crdstore.Fake) string {
				o, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), s, "r1", "")
				if err != nil {
					t.Fatal(err)
				}
				return o.Spec.Description
			},
			setArgs: map[string]any{"description": "changed"},
		},
		{
			name: "Jenkins role binding", update: "update_jenkins_role_binding", get: "get_jenkins_role_binding",
			args: map[string]any{"name": "b1"},
			seed: func(t *testing.T, s *crdstore.Fake, desc string) {
				crdstore.MustSeed(s, &v1alpha1.JenkinsRoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: "b1"},
					Spec:       v1alpha1.JenkinsRoleBindingSpec{RoleRef: desc},
				})
			},
			read: func(t *testing.T, s *crdstore.Fake) string {
				o, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), s, "b1", "")
				if err != nil {
					t.Fatal(err)
				}
				return o.Spec.RoleRef
			},
			setArgs: map[string]any{"roleRef": "changed"},
		},
	}
}

// withArgs returns a copy of base with extra arguments added.
func withArgs(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestResolveConfigCluster(t *testing.T) {
	deps, _, _, _ := memberDeps(t)
	noBrood := &api.Dependencies{}
	noConfig := &api.Dependencies{Brood: memberBrood{}}
	tests := []struct {
		name  string
		deps  *api.Dependencies
		args  map[string]any
		local bool
		text  string
	}{
		{name: "empty is local", deps: deps, args: map[string]any{}, local: true},
		{name: "core is local", deps: deps, args: map[string]any{"cluster": "core"}, local: true},
		{name: "member", deps: deps, args: map[string]any{"cluster": "member-b"}},
		{name: "unknown member", deps: deps, args: map[string]any{"cluster": "member-z"}, text: "unknown cluster: member-z"},
		{name: "no brood, member", deps: noBrood, args: map[string]any{"cluster": "member-b"}, text: "unknown cluster: member-b"},
		{name: "no brood, empty", deps: noBrood, args: map[string]any{}, local: true},
		{name: "no config brood", deps: noConfig, args: map[string]any{"cluster": "member-b"}, text: "config brood not configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, res := resolveConfigCluster(context.Background(), tc.deps, tc.args)
			if tc.text != "" {
				if res == nil || resultTextOf(res) != tc.text {
					t.Fatalf("result = %v, want error %q", res, tc.text)
				}
				return
			}
			if res != nil {
				t.Fatalf("unexpected error: %s", resultTextOf(res))
			}
			if c.local != tc.local {
				t.Errorf("local = %v, want %v (cluster %q)", c.local, tc.local, c.name)
			}
		})
	}
}

func TestConfigUpdateTools_ClusterWritesOnlyThatCluster(t *testing.T) {
	for _, kind := range configKinds() {
		t.Run(kind.name, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(t, local, "local-desc")
			kind.seed(t, member.store, "member-desc")

			res := callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, withArgs(kind.setArgs, map[string]any{"cluster": "member-b"})))
			if res.IsError {
				t.Fatalf("update failed: %v", res.Content)
			}
			if got := kind.read(t, member.store); got != "changed" {
				t.Errorf("member value = %q, want changed", got)
			}
			if got := kind.read(t, local); got != "local-desc" {
				t.Errorf("local value = %q, want local-desc (the local object must not change)", got)
			}
			if len(sink.events) != 1 || sink.events[0].Cluster != "member-b" {
				t.Errorf("events = %+v, want one event on member-b", sink.events)
			}
		})
	}
}

func TestConfigUpdateTools_NoClusterWritesTheLocalCluster(t *testing.T) {
	for _, kind := range configKinds() {
		t.Run(kind.name, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(t, local, "local-desc")
			kind.seed(t, member.store, "member-desc")

			res := callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, kind.setArgs))
			if res.IsError {
				t.Fatalf("update failed: %v", res.Content)
			}
			if got := kind.read(t, local); got != "changed" {
				t.Errorf("local value = %q, want changed", got)
			}
			if got := kind.read(t, member.store); got != "member-desc" {
				t.Errorf("member value = %q, want member-desc", got)
			}
			if len(member.updates) != 0 {
				t.Errorf("member writes = %v, want none", member.updates)
			}
			if len(member.localWrites) != 1 {
				t.Errorf("local operator writes = %v, want one", member.localWrites)
			}
			if len(sink.events) != 1 || sink.events[0].Cluster != "" {
				t.Errorf("events = %+v, want one event with no cluster", sink.events)
			}
		})
	}
}

func TestConfigUpdateTools_UnknownClusterChangesNothing(t *testing.T) {
	for _, kind := range configKinds() {
		t.Run(kind.name, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(t, local, "local-desc")
			kind.seed(t, member.store, "member-desc")

			res := callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, withArgs(kind.setArgs, map[string]any{"cluster": "member-z"})))
			if !res.IsError || contentText(res) != "unknown cluster: member-z" {
				t.Fatalf("result = %+v, want unknown cluster refusal", res)
			}
			if kind.read(t, local) != "local-desc" || kind.read(t, member.store) != "member-desc" {
				t.Error("an unknown cluster changed an object")
			}
			if len(sink.events) != 0 {
				t.Errorf("events = %+v, want none", sink.events)
			}
		})
	}
}

func TestConfigUpdateTools_UnreachableMemberWritesNothing(t *testing.T) {
	for _, kind := range configKinds() {
		t.Run(kind.name, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(t, local, "local-desc")
			kind.seed(t, member.store, "member-desc")
			member.unreachable = true

			res := callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, withArgs(kind.setArgs, map[string]any{"cluster": "member-b"})))
			text := contentText(res)
			if !res.IsError || !strings.HasPrefix(text, "failed to read ") || !strings.Contains(text, " on cluster member-b: ") {
				t.Fatalf("result = %q, want a read failure that names member-b", text)
			}
			if kind.read(t, member.store) != "member-desc" || len(sink.events) != 0 {
				t.Error("an unreachable member changed state or published an event")
			}
		})
	}
}

func TestUpdateJenkinsRole_MemberConflictOnEveryAttempt(t *testing.T) {
	deps, _, member, _ := memberDeps(t)
	kind := configKinds()[2]
	kind.seed(t, member.store, "member-desc")
	gvr := jenkinsRoleGVR(t)
	member.store.FailAlways("update", gvr, apierrors.NewConflict(gvr.GroupResource(), "r1", errors.New("stale resourceVersion")))

	res := callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, withArgs(kind.setArgs, map[string]any{"cluster": "member-b"})))
	want := "Jenkins role r1 on cluster member-b changed during each of 3 attempts: call update_jenkins_role again"
	if !res.IsError || contentText(res) != want {
		t.Errorf("result = %q, want %q", contentText(res), want)
	}
}

func TestConfigGetTools_ReadTheNamedCluster(t *testing.T) {
	for _, kind := range configKinds() {
		t.Run(kind.name, func(t *testing.T) {
			deps, local, member, _ := memberDeps(t)
			kind.seed(t, local, "local-desc")
			kind.seed(t, member.store, "member-desc")

			if res := callToolAs(t, deps, mcpAdminClaims, kind.get, withArgs(kind.args, map[string]any{"cluster": "member-b"})); res.IsError || !strings.Contains(contentText(res), "member-desc") {
				t.Errorf("member get = %q, want the member object", contentText(res))
			}
			if res := callToolAs(t, deps, mcpAdminClaims, kind.get, kind.args); res.IsError || !strings.Contains(contentText(res), "local-desc") {
				t.Errorf("local get = %q, want the local object", contentText(res))
			}
			if res := callToolAs(t, deps, mcpAdminClaims, kind.get, withArgs(kind.args, map[string]any{"cluster": "member-z"})); !res.IsError || contentText(res) != "unknown cluster: member-z" {
				t.Errorf("unknown get = %q, want the refusal", contentText(res))
			}
		})
	}
}

func TestConfigClusterArgumentDeclared(t *testing.T) {
	declared := map[string]bool{
		"get_composed_bundle": true, "update_composed_bundle": true,
		"get_catalog_source": true, "update_catalog_source": true,
		"get_jenkins_role": true, "update_jenkins_role": true,
		"get_jenkins_role_binding": true, "update_jenkins_role_binding": true,
		"list_composed_bundles": true, "create_composed_bundle": true, "delete_composed_bundle": true,
		"validate_composed_bundle": true, "preview_composed_bundle": true,
		"list_catalog_items": true, "get_catalog_item": true,
		"list_catalog_sources": true, "create_catalog_source": true, "delete_catalog_source": true, "sync_catalog_source": true,
		"list_jenkins_roles": true, "create_jenkins_role": true, "delete_jenkins_role": true,
		"list_jenkins_role_bindings": true, "create_jenkins_role_binding": true, "delete_jenkins_role_binding": true,
		"update_varroa_role": false, "update_varroa_role_binding": false,
		"get_varroa_role": false, "get_varroa_role_binding": false,
		"list_varroa_roles": false, "create_varroa_role": false, "delete_varroa_role": false,
		"list_varroa_role_bindings": false, "create_varroa_role_binding": false, "delete_varroa_role_binding": false,
	}
	byName := map[string]listedTool{}
	for _, tool := range liveTools(t) {
		byName[tool.Name] = tool
	}
	for name, want := range declared {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("tool %s is not registered", name)
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if _, got := schema.Properties["cluster"]; got != want {
			t.Errorf("%s declares cluster = %v, want %v", name, got, want)
		}
	}
}

func TestBroodObject_SendsTheReadResourceVersion(t *testing.T) {
	var sent json.RawMessage
	target := broodObject[v1alpha1.JenkinsRole](
		func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"metadata":{"name":"r1","resourceVersion":"7"},"spec":{"description":"read"}}`), nil
		},
		func(_ context.Context, obj json.RawMessage) (json.RawMessage, error) {
			sent = obj
			return obj, nil
		},
	)
	obj, err := target.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	obj.Spec.Description = "merged"
	written, err := target.write(context.Background(), obj)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), `"resourceVersion":"7"`) {
		t.Errorf("write body = %s, want the read resourceVersion 7", sent)
	}
	if written.Spec.Description != "merged" {
		t.Errorf("written description = %q, want merged", written.Spec.Description)
	}
}

func TestUpdateObject_BroodConflictIsRetried(t *testing.T) {
	gets, writes := 0, 0
	target := configObject[v1alpha1.JenkinsRole]{
		get: func(context.Context) (*v1alpha1.JenkinsRole, error) {
			gets++
			return &v1alpha1.JenkinsRole{}, nil
		},
		write: func(_ context.Context, obj *v1alpha1.JenkinsRole) (*v1alpha1.JenkinsRole, error) {
			writes++
			if writes == 1 {
				return nil, &api.BroodError{Code: bus.CodeConflict, Msg: "stale"}
			}
			return obj, nil
		},
	}
	if _, res := updateObject(context.Background(), target, func(*v1alpha1.JenkinsRole) error { return nil }, updateTexts{
		tool: "update_jenkins_role", object: "Jenkins role r1 on cluster member-b", notFound: "Jenkins role not found", failed: "failed to update Jenkins role",
	}); res != nil {
		t.Fatalf("unexpected tool error: %s", resultTextOf(res))
	}
	if gets != 2 || writes != 2 {
		t.Errorf("gets = %d, writes = %d, want 2 and 2", gets, writes)
	}
}

// contentText returns the text of the first content item of a decoded result.
func contentText(res toolResult) string {
	for _, c := range res.Content {
		if c.Text != "" {
			return c.Text
		}
	}
	return ""
}

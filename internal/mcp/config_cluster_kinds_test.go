package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// The member fake serves every config kind through the same store as the
// update tools. Each write appends one entry to updates.

func memberList[T any](ctx context.Context, store *crdstore.Fake, namespace string) ([]json.RawMessage, error) {
	objs, err := crdstore.List[T](ctx, store, namespace, "")
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(objs))
	for _, o := range objs {
		raw, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func memberCreate[T any](ctx context.Context, store *crdstore.Fake, body json.RawMessage) (json.RawMessage, error) {
	var obj T
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if err := crdstore.Create[T](ctx, store, &obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, &api.BroodError{Code: bus.CodeConflict, Msg: err.Error()}
		}
		return nil, err
	}
	return json.Marshal(obj)
}

func (m *memberConfig) ListComposedBundles(ctx context.Context, _, ns string) ([]json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberList[v1alpha1.ComposedBundle](ctx, m.store, ns)
}

func (m *memberConfig) CreateComposedBundle(ctx context.Context, cluster, _, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "create composedbundle")
	return memberCreate[v1alpha1.ComposedBundle](ctx, store, body)
}

func (m *memberConfig) DeleteComposedBundle(ctx context.Context, cluster, ns, name string) error {
	store, writes, err := m.on(cluster)
	if err != nil {
		return err
	}
	*writes = append(*writes, "delete composedbundle")
	return crdstore.Delete[v1alpha1.ComposedBundle](ctx, store, name, ns)
}

func (m *memberConfig) ComposeBundle(_ context.Context, cluster, _ string, _ json.RawMessage) (*bus.BundleComposePreview, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	m.composed = append(m.composed, cluster)
	return &bus.BundleComposePreview{}, nil
}

func (m *memberConfig) ListCatalogItems(ctx context.Context, _, ns string, _ api.CatalogItemFilter) ([]json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	items, err := crdstore.List[v1alpha1.CatalogItem](ctx, m.store, ns, "")
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		raw, err := json.Marshal(controller.ProjectCatalogItemSummary(item))
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func (m *memberConfig) GetCatalogItem(ctx context.Context, _, ns, name string) (json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberGet[v1alpha1.CatalogItem](ctx, m.store, name, ns)
}

func (m *memberConfig) ListCatalogSources(ctx context.Context, _, ns string) ([]json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberList[v1alpha1.CatalogSource](ctx, m.store, ns)
}

func (m *memberConfig) CreateCatalogSource(ctx context.Context, cluster, _, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "create catalogsource")
	return memberCreate[v1alpha1.CatalogSource](ctx, store, body)
}

func (m *memberConfig) DeleteCatalogSource(ctx context.Context, cluster, ns, name string) error {
	store, writes, err := m.on(cluster)
	if err != nil {
		return err
	}
	*writes = append(*writes, "delete catalogsource")
	return crdstore.Delete[v1alpha1.CatalogSource](ctx, store, name, ns)
}

func (m *memberConfig) SyncCatalogSource(_ context.Context, cluster, ns, name string) error {
	if m.unreachable {
		return errMemberDown
	}
	m.updates = append(m.updates, "sync catalogsource "+cluster+"/"+ns+"/"+name)
	return nil
}

func (m *memberConfig) ListJenkinsRoles(ctx context.Context, _ string) ([]json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberList[v1alpha1.JenkinsRole](ctx, m.store, "")
}

func (m *memberConfig) CreateJenkinsRole(ctx context.Context, cluster, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "create jenkinsrole")
	return memberCreate[v1alpha1.JenkinsRole](ctx, store, body)
}

func (m *memberConfig) DeleteJenkinsRole(ctx context.Context, cluster, name string) error {
	store, writes, err := m.on(cluster)
	if err != nil {
		return err
	}
	*writes = append(*writes, "delete jenkinsrole")
	return crdstore.Delete[v1alpha1.JenkinsRole](ctx, store, name, "")
}

func (m *memberConfig) ListJenkinsRoleBindings(ctx context.Context, _ string) ([]json.RawMessage, error) {
	if m.unreachable {
		return nil, errMemberDown
	}
	return memberList[v1alpha1.JenkinsRoleBinding](ctx, m.store, "")
}

func (m *memberConfig) CreateJenkinsRoleBinding(ctx context.Context, cluster, _ string, body json.RawMessage) (json.RawMessage, error) {
	store, writes, err := m.on(cluster)
	if err != nil {
		return nil, err
	}
	*writes = append(*writes, "create jenkinsrolebinding")
	return memberCreate[v1alpha1.JenkinsRoleBinding](ctx, store, body)
}

func (m *memberConfig) DeleteJenkinsRoleBinding(ctx context.Context, cluster, name string) error {
	store, writes, err := m.on(cluster)
	if err != nil {
		return err
	}
	*writes = append(*writes, "delete jenkinsrolebinding")
	return crdstore.Delete[v1alpha1.JenkinsRoleBinding](ctx, store, name, "")
}

// listedNames returns the name of each row of a list result, for a list that
// answers either a bare array or an items envelope.
func listedNames(t *testing.T, tr toolResult) []string {
	t.Helper()
	if tr.IsError {
		t.Fatalf("list returned error: %v", tr.Content)
	}
	var payload any
	if err := json.Unmarshal(tr.StructuredContent, &payload); err != nil {
		t.Fatalf("decode structuredContent: %v", err)
	}
	items, _ := payload.([]any)
	if m, ok := payload.(map[string]any); ok {
		items, _ = m["items"].([]any)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			name, _ := m["name"].(string)
			out = append(out, name)
		}
	}
	return out
}

// writeKind describes one create and delete tool of a config kind, and how to
// seed and find one object of that kind in a store.
type writeKind struct {
	label      string
	create     string
	del        string
	namespace  string
	createArgs map[string]any
	seed       func(s *crdstore.Fake, name, ns string)
	exists     func(s *crdstore.Fake, name, ns string) bool
	conflict   string
}

func writeKinds() []writeKind {
	return []writeKind{
		{
			label: "composed bundle", create: "create_composed_bundle", del: "delete_composed_bundle",
			namespace:  "team-a",
			createArgs: map[string]any{"inputs": []any{map[string]any{"itemRef": map[string]any{"name": "item-a"}}}},
			seed: func(s *crdstore.Fake, name, ns string) {
				crdstore.MustSeed(s, &v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			},
			exists: func(s *crdstore.Fake, name, ns string) bool {
				_, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), s, name, ns)
				return err == nil
			},
			conflict: "composed bundle team-a/obj on cluster member-b already exists; use update_composed_bundle with cluster member-b to change it",
		},
		{
			label: "catalog source", create: "create_catalog_source", del: "delete_catalog_source",
			namespace:  "team-a",
			createArgs: map[string]any{"repoURL": "https://example.test/repo"},
			seed: func(s *crdstore.Fake, name, ns string) {
				crdstore.MustSeed(s, &v1alpha1.CatalogSource{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
					Spec:       v1alpha1.CatalogSourceSpec{RepoURL: "https://example.test/repo"},
				})
			},
			exists: func(s *crdstore.Fake, name, ns string) bool {
				_, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), s, name, ns)
				return err == nil
			},
			conflict: "catalog source team-a/obj on cluster member-b already exists; use update_catalog_source with cluster member-b to change it",
		},
		{
			label: "Jenkins role", create: "create_jenkins_role", del: "delete_jenkins_role",
			createArgs: map[string]any{"permissions": []any{"hudson.model.Hudson.Read"}},
			seed: func(s *crdstore.Fake, name, _ string) {
				crdstore.MustSeed(s, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}})
			},
			exists: func(s *crdstore.Fake, name, _ string) bool {
				_, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), s, name, "")
				return err == nil
			},
			conflict: "Jenkins role obj on cluster member-b already exists; use update_jenkins_role with cluster member-b to change it",
		},
		{
			label: "Jenkins role binding", create: "create_jenkins_role_binding", del: "delete_jenkins_role_binding",
			createArgs: map[string]any{"roleRef": "r1", "subjects": []any{map[string]any{"kind": "User", "name": "alice"}}},
			seed: func(s *crdstore.Fake, name, _ string) {
				crdstore.MustSeed(s, &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
			},
			exists: func(s *crdstore.Fake, name, _ string) bool {
				_, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), s, name, "")
				return err == nil
			},
			conflict: "Jenkins role binding obj on cluster member-b already exists; use update_jenkins_role_binding with cluster member-b to change it",
		},
	}
}

// args returns the arguments of a create call for the kind, named obj.
func (k writeKind) args(extra map[string]any) map[string]any {
	return withArgs(k.delArgs(nil), withArgs(k.createArgs, extra))
}

// delArgs returns the arguments of a delete call for the kind, named obj. A
// delete tool declares no create argument, so the undeclared-argument refusal
// would reject one.
func (k writeKind) delArgs(extra map[string]any) map[string]any {
	base := map[string]any{"name": "obj"}
	if k.namespace != "" {
		base["namespace"] = k.namespace
	}
	return withArgs(base, extra)
}

// TestConfigListTools_ReadOneCluster checks that each list tool reads one
// cluster, and that a list without cluster reads the local cluster.
func TestConfigListTools_ReadOneCluster(t *testing.T) {
	lists := []struct {
		tool string
		seed func(s *crdstore.Fake, name string)
	}{
		{"list_composed_bundles", func(s *crdstore.Fake, name string) {
			crdstore.MustSeed(s, &v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}})
		}},
		{"list_catalog_sources", func(s *crdstore.Fake, name string) {
			crdstore.MustSeed(s, &v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}, Spec: v1alpha1.CatalogSourceSpec{RepoURL: "https://example.test/repo"}})
		}},
		{"list_catalog_items", func(s *crdstore.Fake, name string) {
			crdstore.MustSeed(s, &v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}})
		}},
		{"list_jenkins_roles", func(s *crdstore.Fake, name string) {
			crdstore.MustSeed(s, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: name}})
		}},
		{"list_jenkins_role_bindings", func(s *crdstore.Fake, name string) {
			crdstore.MustSeed(s, &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}})
		}},
	}
	for _, l := range lists {
		t.Run(l.tool, func(t *testing.T) {
			deps, local, member, _ := memberDeps(t)
			l.seed(local, "local-obj")
			l.seed(member.store, "member-obj")

			if got := listedNames(t, callToolAs(t, deps, mcpAdminClaims, l.tool, map[string]any{"cluster": "member-b"})); strings.Join(got, ",") != "member-obj" {
				t.Errorf("member names = %v, want [member-obj]", got)
			}
			if got := listedNames(t, callToolAs(t, deps, mcpAdminClaims, l.tool, map[string]any{})); strings.Join(got, ",") != "local-obj" {
				t.Errorf("local names = %v, want [local-obj]", got)
			}
			if res := callToolAs(t, deps, mcpAdminClaims, l.tool, map[string]any{"cluster": "member-z"}); !res.IsError || contentText(res) != "unknown cluster: member-z" {
				t.Errorf("unknown cluster result = %q, want the refusal", contentText(res))
			}
			member.unreachable = true
			res := callToolAs(t, deps, mcpAdminClaims, l.tool, map[string]any{"cluster": "member-b"})
			text := contentText(res)
			if !res.IsError || !strings.HasPrefix(text, "failed to list ") || !strings.Contains(text, " on cluster member-b: ") {
				t.Errorf("unreachable result = %q, want a list failure that names member-b", text)
			}
		})
	}
}

// TestConfigCreateAndDeleteTools_ClusterWritesOnlyThatCluster checks that a
// create or delete with cluster changes only that cluster, and publishes one
// event on it.
func TestConfigCreateAndDeleteTools_ClusterWritesOnlyThatCluster(t *testing.T) {
	for _, kind := range writeKinds() {
		t.Run(kind.label+" create", func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			res := callToolAs(t, deps, mcpAdminClaims, kind.create, kind.args(map[string]any{"cluster": "member-b"}))
			if res.IsError {
				t.Fatalf("create failed: %s", contentText(res))
			}
			if !kind.exists(member.store, "obj", kind.namespace) || kind.exists(local, "obj", kind.namespace) {
				t.Error("create must write the member object and leave the local store alone")
			}
			if len(sink.events) != 1 || sink.events[0].Cluster != "member-b" {
				t.Errorf("events = %+v, want one event on member-b", sink.events)
			}
		})
		t.Run(kind.label+" delete", func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(local, "obj", kind.namespace)
			kind.seed(member.store, "obj", kind.namespace)
			res := callToolAs(t, deps, mcpAdminClaims, kind.del, kind.delArgs(map[string]any{"cluster": "member-b"}))
			if res.IsError {
				t.Fatalf("delete failed: %s", contentText(res))
			}
			if kind.exists(member.store, "obj", kind.namespace) || !kind.exists(local, "obj", kind.namespace) {
				t.Error("delete must remove the member object and keep the local object")
			}
			if len(sink.events) != 1 || sink.events[0].Cluster != "member-b" {
				t.Errorf("events = %+v, want one event on member-b", sink.events)
			}
		})
	}
}

// TestConfigCreateTools_MemberNameConflictNamesTheCluster checks the create
// refusal text on a member, which names the cluster and the update call.
func TestConfigCreateTools_MemberNameConflictNamesTheCluster(t *testing.T) {
	for _, kind := range writeKinds() {
		t.Run(kind.label, func(t *testing.T) {
			deps, _, member, sink := memberDeps(t)
			kind.seed(member.store, "obj", kind.namespace)
			res := callToolAs(t, deps, mcpAdminClaims, kind.create, kind.args(map[string]any{"cluster": "member-b"}))
			if !res.IsError || contentText(res) != kind.conflict {
				t.Errorf("result = %q, want %q", contentText(res), kind.conflict)
			}
			if len(sink.events) != 0 {
				t.Errorf("events = %+v, want none", sink.events)
			}
		})
	}
}

// TestConfigWriteTools_UnknownOrUnreachableClusterChangesNothing checks that a
// write to an unknown cluster, or to an unreachable member, changes no object
// and publishes no event.
func TestConfigWriteTools_UnknownOrUnreachableClusterChangesNothing(t *testing.T) {
	for _, kind := range writeKinds() {
		t.Run(kind.label+" unknown", func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(local, "obj", kind.namespace)
			res := callToolAs(t, deps, mcpAdminClaims, kind.del, kind.delArgs(map[string]any{"cluster": "member-z"}))
			if !res.IsError || contentText(res) != "unknown cluster: member-z" {
				t.Errorf("result = %q, want the unknown cluster refusal", contentText(res))
			}
			if !kind.exists(local, "obj", kind.namespace) || len(sink.events) != 0 || len(member.updates) != 0 {
				t.Error("an unknown cluster changed state or published an event")
			}
		})
		t.Run(kind.label+" unreachable", func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(local, "obj", kind.namespace)
			member.unreachable = true
			res := callToolAs(t, deps, mcpAdminClaims, kind.del, kind.delArgs(map[string]any{"cluster": "member-b"}))
			text := contentText(res)
			if !res.IsError || !strings.Contains(text, " on cluster member-b: ") {
				t.Errorf("result = %q, want a failure that names member-b", text)
			}
			if !kind.exists(local, "obj", kind.namespace) || len(sink.events) != 0 {
				t.Error("an unreachable member changed the local store or published an event")
			}
		})
	}
}

// TestSyncCatalogSource_SyncsTheNamedCluster checks that sync asks the operator
// of the named cluster, and that the local name is the default.
func TestSyncCatalogSource_SyncsTheNamedCluster(t *testing.T) {
	deps, _, member, sink := memberDeps(t)
	args := map[string]any{"namespace": "team-a", "name": "obj"}

	if res := callToolAs(t, deps, mcpAdminClaims, "sync_catalog_source", withArgs(args, map[string]any{"cluster": "member-b"})); res.IsError {
		t.Fatalf("sync failed: %s", contentText(res))
	}
	if len(member.updates) != 1 || member.updates[0] != "sync catalogsource member-b/team-a/obj" {
		t.Errorf("member calls = %v, want one sync of member-b/team-a/obj", member.updates)
	}
	if len(sink.events) != 1 || sink.events[0].Cluster != "member-b" {
		t.Errorf("events = %+v, want one event on member-b", sink.events)
	}

	if res := callToolAs(t, deps, mcpAdminClaims, "sync_catalog_source", args); res.IsError {
		t.Fatalf("local sync failed: %s", contentText(res))
	}
	if len(member.updates) != 2 || member.updates[1] != "sync catalogsource core/team-a/obj" {
		t.Errorf("calls = %v, want the local sync on core", member.updates)
	}
}

// TestComposeTools_ComposeOnTheNamedCluster checks that validate and preview
// compose on the named cluster, and that an unknown cluster composes nothing.
func TestComposeTools_ComposeOnTheNamedCluster(t *testing.T) {
	for _, tool := range []string{"validate_composed_bundle", "preview_composed_bundle"} {
		t.Run(tool, func(t *testing.T) {
			deps, _, member, _ := memberDeps(t)
			args := map[string]any{"namespace": "team-a", "inputs": []any{map[string]any{"itemRef": map[string]any{"name": "item-a"}}}}

			if res := callToolAs(t, deps, mcpAdminClaims, tool, withArgs(args, map[string]any{"cluster": "member-b"})); res.IsError {
				t.Fatalf("compose failed: %s", contentText(res))
			}
			if res := callToolAs(t, deps, mcpAdminClaims, tool, args); res.IsError {
				t.Fatalf("local compose failed: %s", contentText(res))
			}
			if strings.Join(member.composed, ",") != "member-b,core" {
				t.Errorf("composed on = %v, want [member-b core]", member.composed)
			}
			if res := callToolAs(t, deps, mcpAdminClaims, tool, withArgs(args, map[string]any{"cluster": "member-z"})); !res.IsError || contentText(res) != "unknown cluster: member-z" {
				t.Errorf("unknown cluster result = %q, want the refusal", contentText(res))
			}
			if len(member.composed) != 2 {
				t.Errorf("composed on = %v, want no compose for an unknown cluster", member.composed)
			}
		})
	}
}

// TestGetCatalogItem_ReadsTheNamedCluster checks the namespaced get of a catalog
// item on the named cluster.
func TestGetCatalogItem_ReadsTheNamedCluster(t *testing.T) {
	deps, local, member, _ := memberDeps(t)
	crdstore.MustSeed(local, &v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "local-obj", Namespace: "team-a"}})
	crdstore.MustSeed(member.store, &v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "member-obj", Namespace: "team-a"}})

	args := map[string]any{"namespace": "team-a", "name": "member-obj", "cluster": "member-b"}
	if res := callToolAs(t, deps, mcpAdminClaims, "get_catalog_item", args); res.IsError || !strings.Contains(contentText(res), "member-obj") {
		t.Errorf("member get = %q, want the member item", contentText(res))
	}
	if res := callToolAs(t, deps, mcpAdminClaims, "get_catalog_item", map[string]any{"namespace": "team-a", "name": "member-obj"}); !res.IsError {
		t.Errorf("local get of a member-only item = %q, want not found", contentText(res))
	}
}

// TestConfigListTools_MemberReadFilterUsesTheMemberOperatorNamespace checks that
// a scoped caller lists the namespaced kinds of member-b through the operator
// namespace of member-b. The caller reads team-a and the member operator
// namespace varroa-member, and nothing else.
func TestConfigListTools_MemberReadFilterUsesTheMemberOperatorNamespace(t *testing.T) {
	deps := scopedConfigDeps([]v1alpha1.APIRule{
		{Resources: []string{"composedbundles"}, Verbs: []string{"read"}},
		{Resources: []string{"catalogsources"}, Verbs: []string{"read"}},
		{Resources: []string{"catalogitems"}, Verbs: []string{"read"}},
	})
	member := &memberConfig{store: crdstore.NewFake()}
	deps.Brood = memberBrood{}
	deps.ConfigBrood = member
	for _, ns := range []string{"team-a", "team-b", "varroa-member", "varroa-system"} {
		crdstore.MustSeed(member.store,
			&v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: "cb-" + ns, Namespace: ns}},
			&v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: "cs-" + ns, Namespace: ns}, Spec: v1alpha1.CatalogSourceSpec{RepoURL: "https://example.test/repo"}},
			&v1alpha1.CatalogItem{ObjectMeta: metav1.ObjectMeta{Name: "ci-" + ns, Namespace: ns}},
		)
	}
	for _, tool := range []string{"list_composed_bundles", "list_catalog_sources", "list_catalog_items"} {
		t.Run(tool, func(t *testing.T) {
			got := listedNamespaces(t, callToolAs(t, deps, mcpScopedDevClaims, tool, map[string]any{"cluster": "member-b"}))
			if len(got) != 2 || !containsString(got, "team-a") || !containsString(got, "varroa-member") {
				t.Errorf("member namespaces = %v, want team-a and varroa-member", got)
			}
		})
	}
}

// TestConfigWriteTools_MemberEmitsOneEventOnTheMember checks the 13 writes with
// cluster member-b: four updates, four creates, four deletes, and one sync. Each
// makes one member write and emits one event that names member-b.
func TestConfigWriteTools_MemberEmitsOneEventOnTheMember(t *testing.T) {
	check := func(t *testing.T, res toolResult, member *memberConfig, sink *recordingSink) {
		t.Helper()
		if res.IsError {
			t.Fatalf("write failed: %s", contentText(res))
		}
		if len(member.updates) != 1 {
			t.Errorf("member writes = %v, want exactly one", member.updates)
		}
		if len(sink.events) != 1 || sink.events[0].Cluster != "member-b" {
			t.Errorf("events = %+v, want exactly one on member-b", sink.events)
		}
	}
	for _, kind := range configKinds() {
		t.Run("update "+kind.name, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(t, local, "local-desc")
			kind.seed(t, member.store, "member-desc")
			res := callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, withArgs(kind.setArgs, map[string]any{"cluster": "member-b"})))
			check(t, res, member, sink)
		})
	}
	for _, kind := range writeKinds() {
		t.Run("create "+kind.label, func(t *testing.T) {
			deps, _, member, sink := memberDeps(t)
			res := callToolAs(t, deps, mcpAdminClaims, kind.create, kind.args(map[string]any{"cluster": "member-b"}))
			check(t, res, member, sink)
		})
		t.Run("delete "+kind.label, func(t *testing.T) {
			deps, _, member, sink := memberDeps(t)
			kind.seed(member.store, "obj", kind.namespace)
			res := callToolAs(t, deps, mcpAdminClaims, kind.del, kind.delArgs(map[string]any{"cluster": "member-b"}))
			check(t, res, member, sink)
		})
	}
	t.Run("sync catalog source", func(t *testing.T) {
		deps, _, member, sink := memberDeps(t)
		res := callToolAs(t, deps, mcpAdminClaims, "sync_catalog_source", map[string]any{"namespace": "team-a", "name": "obj", "cluster": "member-b"})
		check(t, res, member, sink)
	})
}

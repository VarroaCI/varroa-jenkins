package rbac

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// --------------------------------------------------------------------------
// fakeInformer implements cache.SharedIndexInformer with just the indexer.
// --------------------------------------------------------------------------

type fakeInformer struct {
	indexer cache.Indexer
}

func (f *fakeInformer) GetIndexer() cache.Indexer                 { return f.indexer }
func (f *fakeInformer) AddIndexers(indexers cache.Indexers) error { return nil }
func (f *fakeInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (f *fakeInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	return nil
}
func (f *fakeInformer) GetStore() cache.Store                                      { return f.indexer }
func (f *fakeInformer) GetController() cache.Controller                            { return nil }
func (f *fakeInformer) Run(stopCh <-chan struct{})                                 {}
func (f *fakeInformer) RunWithContext(ctx context.Context)                         { <-ctx.Done() }
func (f *fakeInformer) HasSynced() bool                                            { return true }
func (f *fakeInformer) IsStopped() bool                                            { return false }
func (f *fakeInformer) SetTransform(handler cache.TransformFunc) error             { return nil }
func (f *fakeInformer) LastSyncResourceVersion() string                            { return "" }
func (f *fakeInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error { return nil }
func (f *fakeInformer) SetWatchErrorHandlerWithContext(handler cache.WatchErrorHandlerWithContext) error {
	return nil
}
func (f *fakeInformer) AddEventHandlerWithOptions(handler cache.ResourceEventHandler, options cache.HandlerOptions) (cache.ResourceEventHandlerRegistration, error) {
	return f.AddEventHandler(handler)
}
func (f *fakeInformer) HasSyncedChecker() cache.DoneChecker {
	ch := make(chan struct{})
	close(ch)
	return &doneChecker{ch: ch}
}

type doneChecker struct{ ch chan struct{} }

func (d *doneChecker) Name() string          { return "fake" }
func (d *doneChecker) Done() <-chan struct{} { return d.ch }

// --------------------------------------------------------------------------
// testResolver creates a Resolver pre-populated with roles, bindings, and
// controllers for unit testing without real informers.
// --------------------------------------------------------------------------

func testResolver(roles []*v1alpha1.VarroaRole, bindings []*v1alpha1.VarroaRoleBinding, controllers []*v1alpha1.Controller, defaultRead bool) *Resolver {
	return testResolverWithJenkins(roles, bindings, nil, nil, controllers, defaultRead)
}

// --------------------------------------------------------------------------
// Tests for EffectiveAPICapabilities
// --------------------------------------------------------------------------

func TestEffectiveAPICapabilities_ClusterWide(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read", "create", "delete"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "platform-eng"}},
				RoleRef:  "admin",
				// Scope: nil = cluster-wide (always matches)
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-ctrl", Namespace: "team-a"}},
	}

	r := testResolver(roles, bindings, controllers, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"platform-eng"}}

	caps := r.EffectiveAPICapabilities(claims, "team-a", "test-ctrl")

	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read to be true")
	}
	if !caps["controllers"]["create"] {
		t.Error("expected controllers:create to be true")
	}
	if !caps["controllers"]["delete"] {
		t.Error("expected controllers:delete to be true")
	}
	if len(caps) != 1 {
		t.Errorf("expected 1 resource key, got %d", len(caps))
	}
}

func TestEffectiveAPICapabilities_NamespaceScoped(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-team-a"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "team-a"}},
				RoleRef:  "developer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a"},
				},
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "team-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "team-b"}},
	}

	r := testResolver(roles, bindings, controllers, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"team-a"}}

	// Should match in team-a
	capsA := r.EffectiveAPICapabilities(claims, "team-a", "ctrl-a")
	if !capsA["controllers"]["read"] {
		t.Error("expected controllers:read to be true in team-a")
	}

	// Should NOT match in team-b
	capsB := r.EffectiveAPICapabilities(claims, "team-b", "ctrl-b")
	if len(capsB) > 0 {
		t.Errorf("expected no capabilities in team-b, got %v", capsB)
	}
}

func TestEffectiveAPICapabilities_LabelSelector(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-access"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "prod-team"}},
				RoleRef:  "prod-access",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"tier": "prod"},
					},
				},
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-ctrl", Namespace: "infra", Labels: map[string]string{"tier": "prod"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-ctrl", Namespace: "infra", Labels: map[string]string{"tier": "dev"}},
		},
	}

	r := testResolver(roles, bindings, controllers, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"prod-team"}}

	// Should match prod controller
	capsProd := r.EffectiveAPICapabilities(claims, "infra", "prod-ctrl")
	if !capsProd["controllers"]["read"] {
		t.Error("expected controllers:read to be true for prod controller")
	}

	// Should NOT match dev controller
	capsDev := r.EffectiveAPICapabilities(claims, "infra", "dev-ctrl")
	if len(capsDev) > 0 {
		t.Errorf("expected no capabilities for dev controller, got %v", capsDev)
	}
}

func TestEffectiveAPICapabilities_NamespaceAndLabelIntersection(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "restricted"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read", "update"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "restricted-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "sre"}},
				RoleRef:  "restricted",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"production"},
					ControllerSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"tier": "prod"},
					},
				},
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "match", Namespace: "production", Labels: map[string]string{"tier": "prod"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "wrong-ns", Namespace: "staging", Labels: map[string]string{"tier": "prod"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "wrong-label", Namespace: "production", Labels: map[string]string{"tier": "staging"}},
		},
	}

	r := testResolver(roles, bindings, controllers, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"sre"}}

	// Both namespace AND label selector must match
	caps := r.EffectiveAPICapabilities(claims, "production", "match")
	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read when both namespace and labels match")
	}

	// Wrong namespace
	capsWrongNS := r.EffectiveAPICapabilities(claims, "staging", "wrong-ns")
	if len(capsWrongNS) > 0 {
		t.Errorf("expected no capabilities for wrong namespace, got %v", capsWrongNS)
	}

	// Wrong label
	capsWrongLabel := r.EffectiveAPICapabilities(claims, "production", "wrong-label")
	if len(capsWrongLabel) > 0 {
		t.Errorf("expected no capabilities for wrong label, got %v", capsWrongLabel)
	}
}

func TestEffectiveAPICapabilities_MultiBindingUnion(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "read-roles"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"roles"}, Verbs: []string{"read"}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "write-roles"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"roles"}, Verbs: []string{"create", "delete"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "read-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "multi-user"}},
				RoleRef:  "read-roles",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "write-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "multi-user"}},
				RoleRef:  "write-roles",
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "multi-user"}

	caps := r.EffectiveAPICapabilities(claims, "any-ns", "any-ctrl")

	// Must have all verbs from both roles
	if !caps["roles"]["read"] {
		t.Error("expected roles:read from first binding")
	}
	if !caps["roles"]["create"] {
		t.Error("expected roles:create from second binding")
	}
	if !caps["roles"]["delete"] {
		t.Error("expected roles:delete from second binding")
	}
}

func TestEffectiveAPICapabilities_NoMatch_DefaultRead(t *testing.T) {
	r := testResolver(nil, nil, nil, true)
	claims := &auth.Claims{Subject: "nobody", Groups: []string{"unknown"}}

	caps := r.EffectiveAPICapabilities(claims, "any-ns", "any-ctrl")

	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read from defaultRead fallback")
	}
	if len(caps) != 1 {
		t.Errorf("expected exactly 1 resource key, got %d", len(caps))
	}
}

func TestEffectiveAPICapabilities_NoMatch_DefaultReadOff(t *testing.T) {
	r := testResolver(nil, nil, nil, false)
	claims := &auth.Claims{Subject: "nobody", Groups: []string{"unknown"}}

	caps := r.EffectiveAPICapabilities(claims, "any-ns", "any-ctrl")

	if len(caps) > 0 {
		t.Errorf("expected empty capabilities when no bindings match and defaultRead=false, got %v", caps)
	}
}

func TestEffectiveAPICapabilities_UserSubject(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "personal"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "user-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}},
				RoleRef:  "personal",
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)

	// alice should match
	capsAlice := r.EffectiveAPICapabilities(&auth.Claims{Subject: "alice"}, "ns", "ctrl")
	if !capsAlice["controllers"]["read"] {
		t.Error("expected controllers:read for alice")
	}

	// bob should NOT match
	capsBob := r.EffectiveAPICapabilities(&auth.Claims{Subject: "bob"}, "ns", "ctrl")
	if len(capsBob) > 0 {
		t.Errorf("expected no capabilities for bob, got %v", capsBob)
	}
}

func TestEffectiveAPICapabilities_NilClaims(t *testing.T) {
	// When claims is nil, defaultRead controls whether any capabilities are returned.
	r := testResolver(nil, nil, nil, true)
	caps := r.EffectiveAPICapabilities(nil, "ns", "ctrl")
	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read when claims is nil and defaultRead=true")
	}

	r2 := testResolver(nil, nil, nil, false)
	caps2 := r2.EffectiveAPICapabilities(nil, "ns", "ctrl")
	if len(caps2) > 0 {
		t.Errorf("expected empty capabilities when claims is nil and defaultRead=false, got %v", caps2)
	}
}

func TestEffectiveAPICapabilities_DeduplicatesRoles(t *testing.T) {
	// Multiple bindings referencing the same role should only be counted once.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "group-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "viewers"}},
				RoleRef:  "viewer",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "user-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "viewer-user"}},
				RoleRef:  "viewer",
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "viewer-user", Groups: []string{"viewers"}}

	caps := r.EffectiveAPICapabilities(claims, "ns", "ctrl")
	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read from deduplicated role")
	}
	// The role should contribute its rules only once
	if len(caps["controllers"]) != 1 {
		t.Errorf("expected exactly 1 verb (read), got %d: %v", len(caps["controllers"]), caps["controllers"])
	}
}

func TestEffectiveAPICapabilities_SameResourceDifferentVerbs(t *testing.T) {
	// Multiple rules for the same resource across different roles merge all verbs.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "can-read"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "can-write"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"create", "update", "delete"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "read-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "power-user"}},
				RoleRef:  "can-read",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "write-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "power-user"}},
				RoleRef:  "can-write",
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "power-user"}

	caps := r.EffectiveAPICapabilities(claims, "ns", "ctrl")

	expected := map[string]bool{"read": true, "create": true, "update": true, "delete": true}
	for verb := range expected {
		if !caps["controllers"][verb] {
			t.Errorf("expected controllers:%s to be true", verb)
		}
	}
}

// --------------------------------------------------------------------------
// Tests for EffectiveClusterAPICapabilities
// --------------------------------------------------------------------------

func TestEffectiveClusterAPICapabilities_ClusterWideBinding(t *testing.T) {
	// A nil-scope (cluster-wide) binding should grant the capabilities.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-viewer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "viewers"}},
				RoleRef:  "viewer",
				// Scope: nil = cluster-wide
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"viewers"}}

	caps := r.EffectiveClusterAPICapabilities(claims)

	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read from cluster-wide binding")
	}
}

func TestEffectiveClusterAPICapabilities_ControllerScopedBinding(t *testing.T) {
	// A controller-scoped binding (non-nil Scope) should NOT grant capabilities
	// via EffectiveClusterAPICapabilities.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ctrl-user"}},
				RoleRef:  "ctrl-viewer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a"},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "ctrl-user"}

	caps := r.EffectiveClusterAPICapabilities(claims)

	if caps["controllers"]["read"] {
		t.Error("expected no controllers:read from controller-scoped binding")
	}
	if len(caps) != 0 {
		t.Errorf("expected empty capability set, got %v", caps)
	}
}

func TestEffectiveClusterAPICapabilities_NamespaceScopedBinding(t *testing.T) {
	// A namespace-scoped binding should not grant cluster-wide capabilities.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-reader"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ns-user"}},
				RoleRef:  "ns-reader",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a"},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "ns-user"}

	caps := r.EffectiveClusterAPICapabilities(claims)

	if caps["controllers"]["read"] {
		t.Error("expected no controllers:read from namespace-scoped binding")
	}
	if len(caps) != 0 {
		t.Errorf("expected empty capability set, got %v", caps)
	}
}

func TestEffectiveClusterAPICapabilities_SelectorScopedBinding(t *testing.T) {
	// A controller-selector-scoped binding should not grant cluster-wide capabilities.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "selector-reader"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "selector-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "sel-user"}},
				RoleRef:  "selector-reader",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"env": "prod"},
					},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "sel-user"}

	caps := r.EffectiveClusterAPICapabilities(claims)

	if caps["controllers"]["read"] {
		t.Error("expected no controllers:read from selector-scoped binding")
	}
	if len(caps) != 0 {
		t.Errorf("expected empty capability set, got %v", caps)
	}
}

func TestEffectiveClusterAPICapabilities_AdminWildcard(t *testing.T) {
	// Cluster-wide admin role with wildcard verbs should grant wildcard capability.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "admins"}},
				RoleRef:  "admin",
				// Scope: nil = cluster-wide
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "admin-user", Groups: []string{"admins"}}

	caps := r.EffectiveClusterAPICapabilities(claims)

	if caps["*"] == nil || !caps["*"]["*"] {
		t.Error("expected wildcard capability for admin")
	}
}

func TestEffectiveClusterAPICapabilities_NilClaimsDefaultRead(t *testing.T) {
	// Nil claims + defaultRead should yield controllers:read.
	r := testResolver(nil, nil, nil, true)
	caps := r.EffectiveClusterAPICapabilities(nil)

	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read for nil claims with defaultRead")
	}
}

func TestEffectiveClusterAPICapabilities_NilClaimsNoDefaultRead(t *testing.T) {
	// Nil claims without defaultRead should yield empty.
	r := testResolver(nil, nil, nil, false)
	caps := r.EffectiveClusterAPICapabilities(nil)

	if len(caps) != 0 {
		t.Errorf("expected empty capability set, got %v", caps)
	}
}

func TestEffectiveClusterAPICapabilities_SkipsScopedWhenAlsoHasClusterWide(t *testing.T) {
	// A user with both a cluster-wide binding and a controller-scoped binding:
	// EffectiveClusterAPICapabilities should only include the cluster-wide one.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-reader"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-writer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"create", "update"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "mixed-user"}},
				RoleRef:  "cluster-reader",
				// Scope: nil = cluster-wide
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ctrl-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "mixed-user"}},
				RoleRef:  "ctrl-writer",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a"},
				},
			},
		},
	}

	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "mixed-user"}

	caps := r.EffectiveClusterAPICapabilities(claims)

	// Should have controllers:read from cluster-wide binding
	if !caps["controllers"]["read"] {
		t.Error("expected controllers:read from cluster-wide binding")
	}
	// Should NOT have controllers:create or controllers:update from scoped binding
	if caps["controllers"]["create"] {
		t.Error("expected no controllers:create from scoped binding")
	}
	if caps["controllers"]["update"] {
		t.Error("expected no controllers:update from scoped binding")
	}
}

// --------------------------------------------------------------------------
// Tests for JenkinsAssignments
// --------------------------------------------------------------------------

func TestJenkinsAssignments(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins-admin"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Administer"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins-viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read", "Job.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "admin-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{
					{Kind: "Group", Name: "jenkins-admins"},
				},
				RoleRef: "jenkins-admin",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a"},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{
					{Kind: "Group", Name: "jenkins-viewers"},
					{Kind: "User", Name: "specific-user"},
				},
				RoleRef: "jenkins-viewer",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "team-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "team-b"}},
	}

	r := testResolver(roles, bindings, controllers, false)

	// Controller in team-a should get both the admin and viewer roles
	assignments, err := r.JenkinsAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(assignments) != 2 {
		t.Fatalf("expected 2 role assignments for ctrl-a, got %d", len(assignments))
	}

	// Check admin role
	var adminFound, viewerFound bool
	for _, a := range assignments {
		switch a.RoleName {
		case "varroa:jenkins-admin":
			adminFound = true
			if len(a.Permissions) != 1 || a.Permissions[0] != "Overall.Administer" {
				t.Errorf("admin role: expected [Overall.Administer], got %v", a.Permissions)
			}
			if len(a.Subjects) != 1 || a.Subjects[0].Name != "jenkins-admins" {
				t.Errorf("admin role: expected [jenkins-admins], got %v", a.Subjects)
			}
		case "varroa:jenkins-viewer":
			viewerFound = true
			if len(a.Permissions) != 2 {
				t.Errorf("viewer role: expected 2 permissions, got %v", a.Permissions)
			}
			// Should have both subjects from the viewer binding
			if len(a.Subjects) != 2 {
				t.Errorf("viewer role: expected 2 subject names, got %v", a.Subjects)
			}
		}
	}
	if !adminFound {
		t.Error("expected jenkins-admin role assignment")
	}
	if !viewerFound {
		t.Error("expected jenkins-viewer role assignment")
	}
}

func TestJenkinsAssignments_NoMatch(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "some-role"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "team-a"}},
				RoleRef:  "some-role",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					Namespaces: []string{"team-a"},
				},
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "team-b"}},
	}

	r := testResolver(roles, bindings, controllers, false)
	assignments, err := r.JenkinsAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 0 {
		t.Errorf("expected 0 assignments for non-matching controller, got %d", len(assignments))
	}
}

func TestJenkinsAssignments_SkipsEmptyPermissions(t *testing.T) {
	// A role with no JenkinsPermissions should be skipped.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api-only"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
				// No JenkinsPermissions
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api-only-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "devs"}},
				RoleRef:  "api-only",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "ns1"}},
	}

	r := testResolver(roles, bindings, controllers, false)
	assignments, err := r.JenkinsAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 0 {
		t.Errorf("expected 0 assignments for API-only role, got %d", len(assignments))
	}
}

func TestJenkinsAssignments_DeduplicatesSubjects(t *testing.T) {
	// Multiple bindings referencing the same role for the same subject group
	// should produce a single RoleAssignment with deduplicated subject names.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "reader"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "reader-binding-1"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "readers"}},
				RoleRef:  "reader",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "reader-binding-2"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{
					{Kind: "Group", Name: "readers"},
					{Kind: "User", Name: "extra-user"},
				},
				RoleRef: "reader",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "ns1"}},
	}

	r := testResolver(roles, bindings, controllers, false)
	assignments, err := r.JenkinsAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 role assignment, got %d", len(assignments))
	}

	a := assignments[0]
	if len(a.Subjects) != 2 {
		t.Errorf("expected 2 deduplicated subject names, got %v", a.Subjects)
	}
	// Both "readers" and "extra-user" should be present
	seen := make(map[string]bool)
	for _, s := range a.Subjects {
		seen[s.Name] = true
	}
	if !seen["readers"] {
		t.Error("expected 'readers' in subject names")
	}
	if !seen["extra-user"] {
		t.Error("expected 'extra-user' in subject names")
	}
}

func TestJenkinsAssignments_ScopeMatchWithLabelSelector(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "label-scoped"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Job.Build"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "label-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "builders"}},
				RoleRef:  "label-scoped",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"environment": "staging"},
					},
				},
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "staging-ctrl", Namespace: "team-a", Labels: map[string]string{"environment": "staging"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-ctrl", Namespace: "team-a", Labels: map[string]string{"environment": "production"}},
		},
	}

	r := testResolver(roles, bindings, controllers, false)

	// Staging controller should match
	assignments, err := r.JenkinsAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment for staging controller, got %d", len(assignments))
	}
	if assignments[0].RoleName != "varroa:label-scoped" {
		t.Errorf("expected role name 'varroa:label-scoped', got %q", assignments[0].RoleName)
	}

	// Production controller should NOT match
	assignments, err = r.JenkinsAssignments(controllers[1])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 0 {
		t.Errorf("expected 0 assignments for prod controller, got %d", len(assignments))
	}
}

func TestJenkinsAssignments_MultipleSubjectsPerBinding(t *testing.T) {
	// A single binding with multiple subjects should include all of them.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "multi-subject-role"},
			Spec: v1alpha1.VarroaRoleSpec{
				JenkinsPermissions: []string{"Overall.Read", "Job.Read"},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "multi-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{
					{Kind: "Group", Name: "team-alpha"},
					{Kind: "Group", Name: "team-beta"},
					{Kind: "User", Name: "lead-user"},
				},
				RoleRef: "multi-subject-role",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-1", Namespace: "ns1"}},
	}

	r := testResolver(roles, bindings, controllers, false)
	assignments, err := r.JenkinsAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(assignments))
	}

	// Verify all three subjects are included
	if len(assignments[0].Subjects) != 3 {
		t.Errorf("expected 3 subject names, got %v", assignments[0].Subjects)
	}
}

func TestJenkinsAssignments_NoBindings(t *testing.T) {
	r := testResolver(nil, nil, nil, false)
	assignments, err := r.JenkinsAssignments(&v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl-1", Namespace: "ns1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(assignments) != 0 {
		t.Errorf("expected 0 assignments with no bindings, got %d", len(assignments))
	}
}

// testResolverWithJenkins creates a Resolver pre-populated with all role types.
func testResolverWithJenkins(
	roles []*v1alpha1.VarroaRole,
	bindings []*v1alpha1.VarroaRoleBinding,
	jenkinsRoles []*v1alpha1.JenkinsRole,
	jenkinsRoleBindings []*v1alpha1.JenkinsRoleBinding,
	controllers []*v1alpha1.Controller,
	defaultRead bool,
) *Resolver {
	roleIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range roles {
		_ = roleIdx.Add(r)
	}
	roleInf := &fakeInformer{indexer: roleIdx}

	bindingIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{BySubjectIndex: SubjectIndexFunc})
	for _, b := range bindings {
		_ = bindingIdx.Add(b)
	}
	bindingInf := &fakeInformer{indexer: bindingIdx}

	jrIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, jr := range jenkinsRoles {
		_ = jrIdx.Add(jr)
	}
	jrInf := &fakeInformer{indexer: jrIdx}

	jrbIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{JenkinsBySubjectIndex: JenkinsSubjectIndexFunc})
	for _, jrb := range jenkinsRoleBindings {
		_ = jrbIdx.Add(jrb)
	}
	jrbInf := &fakeInformer{indexer: jrbIdx}

	ctrlIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, c := range controllers {
		_ = ctrlIdx.Add(c)
	}
	ctrlInf := &fakeInformer{indexer: ctrlIdx}

	return NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, defaultRead, []string{"sub", "preferred_username"}, []string{"groups"})
}

// TestJenkinsRoleAssignments_MiteUsesGroupAuthority verifies the synthesized
// varroa:system-mite assignment targets the mite's GROUP authority
// (ROLE:varroa:system-mite), not a user SID. The mite authenticates as an
// impersonated principal whose only matchable SID under role-strategy is that
// group authority — a user:varroa-mite entry would silently fail to confer
// Administer and lock the mite out once role-based is the active strategy.
func TestJenkinsRoleAssignments_MiteUsesGroupAuthority(t *testing.T) {
	jenkinsRoles := []*v1alpha1.JenkinsRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "varroa:system-mite"},
			Spec: v1alpha1.JenkinsRoleSpec{
				RoleType:    "Global",
				Permissions: []string{"hudson.model.Hudson.Administer"},
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"}},
	}
	r := testResolverWithJenkins(nil, nil, jenkinsRoles, nil, controllers, false)

	assignments, err := r.JenkinsRoleAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var found *RoleAssignment
	for i := range assignments {
		if assignments[i].RoleName == "varroa:system-mite" {
			found = &assignments[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected synthesized varroa:system-mite assignment, got: %+v", assignments)
	}
	if len(found.Subjects) != 1 {
		t.Fatalf("expected exactly one subject, got: %+v", found.Subjects)
	}
	got := found.Subjects[0]
	if got.Kind != "Group" || got.Name != "ROLE:varroa:system-mite" {
		t.Errorf("expected mite assigned to Group ROLE:varroa:system-mite, got %s:%s", got.Kind, got.Name)
	}
}

// The varroa:system-operator assignment, like the mite, targets the GROUP
// authority ROLE:varroa:system-operator (the only SID matchable for the
// impersonated varroa-operator principal). It is synthesized unconditionally
// so an on-demand executeGroovy operator JWT is authorized for Jenkins.ADMINISTER,
// the only permission that gates /scriptText.
func TestJenkinsRoleAssignments_SystemOperatorUsesGroupAuthority(t *testing.T) {
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "ns1"}},
	}
	// No JenkinsRole/JenkinsRoleBinding for the operator — synthesis must still emit it.
	r := testResolverWithJenkins(nil, nil, nil, nil, controllers, false)

	assignments, err := r.JenkinsRoleAssignments(controllers[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var found *RoleAssignment
	for i := range assignments {
		if assignments[i].RoleName == "varroa:system-operator" {
			found = &assignments[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected synthesized varroa:system-operator assignment, got: %+v", assignments)
	}
	if len(found.Subjects) != 1 {
		t.Fatalf("expected exactly one subject, got: %+v", found.Subjects)
	}
	got := found.Subjects[0]
	if got.Kind != "Group" || got.Name != "ROLE:varroa:system-operator" {
		t.Errorf("expected operator assigned to Group ROLE:varroa:system-operator, got %s:%s", got.Kind, got.Name)
	}
	if len(found.Permissions) != 1 || found.Permissions[0] != "hudson.model.Hudson.Administer" {
		t.Errorf("expected exactly [hudson.model.Hudson.Administer], got %v", found.Permissions)
	}
}

// --------------------------------------------------------------------------
// Tests for EffectivePermissionScopes
// --------------------------------------------------------------------------

func TestEffectivePermissionScopes_NilClaims_DefaultReadOn(t *testing.T) {
	r := testResolver(nil, nil, nil, true)
	ps := r.EffectivePermissionScopes(nil)
	if ps.Global["controllers"]["read"] != true {
		t.Error("expected Global.controllers.read=true with nil claims and defaultRead=true")
	}
	if len(ps.Scopes) != 0 {
		t.Errorf("expected Scopes to be empty, got %d entries", len(ps.Scopes))
	}
}

func TestEffectivePermissionScopes_NilClaims_DefaultReadOff(t *testing.T) {
	r := testResolver(nil, nil, nil, false)
	ps := r.EffectivePermissionScopes(nil)
	if len(ps.Global) != 0 {
		t.Errorf("expected empty Global with nil claims and defaultRead=false, got %v", ps.Global)
	}
	if len(ps.Scopes) != 0 {
		t.Errorf("expected Scopes to be empty, got %d entries", len(ps.Scopes))
	}
}

func TestEffectivePermissionScopes_ClusterWideOnly(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "viewers"}},
				RoleRef:  "viewer",
				// Scope: nil = cluster-wide
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"viewers"}}
	ps := r.EffectivePermissionScopes(claims)

	if !ps.Global["controllers"]["read"] {
		t.Error("expected Global.controllers.read=true from cluster-wide binding")
	}
	if len(ps.Scopes) != 0 {
		t.Errorf("expected Scopes to be empty, got %d entries", len(ps.Scopes))
	}
}

func TestEffectivePermissionScopes_NamespaceScopedOnly(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "developer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read", "update"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "team-a"}},
				RoleRef:  "developer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"team-a"}}
	ps := r.EffectivePermissionScopes(claims)

	if len(ps.Global) != 0 {
		t.Errorf("expected empty Global for scoped-only caller, got %v", ps.Global)
	}
	if len(ps.Scopes) != 1 {
		t.Fatalf("expected 1 Scopes entry, got %d", len(ps.Scopes))
	}
	s := ps.Scopes[0]
	if len(s.Namespaces) != 1 || s.Namespaces[0] != "team-a" {
		t.Errorf("expected Namespaces=[team-a], got %v", s.Namespaces)
	}
	if s.HasControllerSelector {
		t.Error("expected HasControllerSelector=false")
	}
	if !s.Capabilities["controllers"]["read"] {
		t.Error("expected Capabilities.controllers.read=true")
	}
	if !s.Capabilities["controllers"]["update"] {
		t.Error("expected Capabilities.controllers.update=true")
	}
	// Verify scoped caps are NOT in Global.
	if ps.Global["controllers"]["update"] {
		t.Error("controllers:update should NOT be in Global for scoped-only caller")
	}
}

func TestEffectivePermissionScopes_SelectorOnlyScope(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "prod-access"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "sel-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "prod-team"}},
				RoleRef:  "prod-access",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"tier": "prod"},
					},
				},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"prod-team"}}
	ps := r.EffectivePermissionScopes(claims)

	if len(ps.Scopes) != 1 {
		t.Fatalf("expected 1 Scopes entry, got %d", len(ps.Scopes))
	}
	s := ps.Scopes[0]
	if !s.HasControllerSelector {
		t.Error("expected HasControllerSelector=true")
	}
	if len(s.Namespaces) != 0 {
		t.Errorf("expected Namespaces=[], got %v", s.Namespaces)
	}
	if !s.Capabilities["controllers"]["read"] {
		t.Error("expected Capabilities.controllers.read=true")
	}
}

func TestEffectivePermissionScopes_ClusterAndScopedBinding(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "reader"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "operator"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"create", "update", "delete"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-reader"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "devs"}},
				RoleRef:  "reader",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-operator"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "devs"}},
				RoleRef:  "operator",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"devs"}}
	ps := r.EffectivePermissionScopes(claims)

	// Global should have only controllers:read from cluster-wide reader.
	if !ps.Global["controllers"]["read"] {
		t.Error("expected Global.controllers.read=true from cluster-wide binding")
	}
	if ps.Global["controllers"]["create"] {
		t.Error("controllers:create must NOT be in Global (scoped only)")
	}
	if ps.Global["controllers"]["update"] {
		t.Error("controllers:update must NOT be in Global (scoped only)")
	}
	if ps.Global["controllers"]["delete"] {
		t.Error("controllers:delete must NOT be in Global (scoped only)")
	}

	// Scopes should have one entry with the scoped operator caps.
	if len(ps.Scopes) != 1 {
		t.Fatalf("expected 1 Scopes entry, got %d", len(ps.Scopes))
	}
	s := ps.Scopes[0]
	if !s.Capabilities["controllers"]["create"] {
		t.Error("expected scoped capabilities to include controllers:create")
	}
	if !s.Capabilities["controllers"]["update"] {
		t.Error("expected scoped capabilities to include controllers:update")
	}
	if !s.Capabilities["controllers"]["delete"] {
		t.Error("expected scoped capabilities to include controllers:delete")
	}
}

func TestEffectivePermissionScopes_SameScopeDifferentRolesMerge(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "can-read"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "can-write-bundles"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"composedbundles"}, Verbs: []string{"update"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "read-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user"}},
				RoleRef:  "can-read",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "write-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user"}},
				RoleRef:  "can-write-bundles",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user"}
	ps := r.EffectivePermissionScopes(claims)

	if len(ps.Scopes) != 1 {
		t.Fatalf("expected 1 merged Scopes entry, got %d", len(ps.Scopes))
	}
	s := ps.Scopes[0]
	if !s.Capabilities["controllers"]["read"] {
		t.Error("expected merged controllers:read")
	}
	if !s.Capabilities["composedbundles"]["update"] {
		t.Error("expected merged composedbundles:update")
	}
}

func TestEffectivePermissionScopes_DifferentScopesTwoEntries(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user"}},
				RoleRef:  "viewer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "team-b-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user"}},
				RoleRef:  "viewer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-b"}},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user"}
	ps := r.EffectivePermissionScopes(claims)

	if len(ps.Scopes) != 2 {
		t.Fatalf("expected 2 Scopes entries, got %d", len(ps.Scopes))
	}
	// Deterministic order: "team-a" sorts before "team-b".
	if len(ps.Scopes[0].Namespaces) != 1 || ps.Scopes[0].Namespaces[0] != "team-a" {
		t.Errorf("expected first entry Namespaces=[team-a], got %v", ps.Scopes[0].Namespaces)
	}
	if len(ps.Scopes[1].Namespaces) != 1 || ps.Scopes[1].Namespaces[0] != "team-b" {
		t.Errorf("expected second entry Namespaces=[team-b], got %v", ps.Scopes[1].Namespaces)
	}
}

func TestEffectivePermissionScopes_OperatorRoleScoped(t *testing.T) {
	// The key regression guard: operator-like verbs must appear ONLY under scopes.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "operator"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"provisioningdefaults"}, Verbs: []string{"update"}},
					{Resources: []string{"catalogsources"}, Verbs: []string{"create"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-operator"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "team-a"}},
				RoleRef:  "operator",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	r := testResolver(roles, bindings, nil, false)
	claims := &auth.Claims{Subject: "user1", Groups: []string{"team-a"}}
	ps := r.EffectivePermissionScopes(claims)

	// These caps must NOT appear in Global.
	if ps.Global["provisioningdefaults"]["update"] {
		t.Error("provisioningdefaults:update must NOT be in Global for scoped-only caller")
	}
	if ps.Global["catalogsources"]["create"] {
		t.Error("catalogsources:create must NOT be in Global for scoped-only caller")
	}
	if len(ps.Scopes) != 1 {
		t.Fatalf("expected 1 Scopes entry, got %d", len(ps.Scopes))
	}
	s := ps.Scopes[0]
	if !s.Capabilities["provisioningdefaults"]["update"] {
		t.Error("expected provisioningdefaults:update under Scopes")
	}
	if !s.Capabilities["catalogsources"]["create"] {
		t.Error("expected catalogsources:create under Scopes")
	}
}

func TestEffectivePermissionScopes_DefaultReadNotFiredWhenScoped(t *testing.T) {
	// When a scoped binding exists, defaultRead must NOT add controllers:read to Global.
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
			Spec: v1alpha1.VarroaRoleSpec{
				APIRules: []v1alpha1.APIRule{
					{Resources: []string{"controllers"}, Verbs: []string{"read"}},
				},
			},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "team-a"}},
				RoleRef:  "viewer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			},
		},
	}
	r := testResolver(roles, bindings, nil, true) // defaultRead=true
	claims := &auth.Claims{Subject: "user1", Groups: []string{"team-a"}}
	ps := r.EffectivePermissionScopes(claims)

	// Global should be empty — defaultRead fallback must NOT fire when buckets is non-empty.
	if len(ps.Global) != 0 {
		t.Errorf("expected empty Global when scoped binding exists (defaultRead should not fire), got %v", ps.Global)
	}
}

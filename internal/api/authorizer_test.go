package api

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
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
// testResolver creates a Resolver pre-populated with roles and bindings.
// --------------------------------------------------------------------------

func testResolver(roles []*v1alpha1.VarroaRole, bindings []*v1alpha1.VarroaRoleBinding) *rbac.Resolver {
	roleIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, r := range roles {
		_ = roleIdx.Add(r)
	}
	roleInf := &fakeInformer{indexer: roleIdx}

	bindingIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{rbac.BySubjectIndex: rbac.SubjectIndexFunc})
	for _, b := range bindings {
		_ = bindingIdx.Add(b)
	}
	bindingInf := &fakeInformer{indexer: bindingIdx}

	jrIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	jrInf := &fakeInformer{indexer: jrIdx}

	jrbIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{rbac.JenkinsBySubjectIndex: rbac.JenkinsSubjectIndexFunc})
	jrbInf := &fakeInformer{indexer: jrbIdx}

	ctrlIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	ctrlInf := &fakeInformer{indexer: ctrlIdx}

	return rbac.NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, false, []string{"sub", "preferred_username"}, []string{"groups"})
}

// --------------------------------------------------------------------------
// Tests for CanReadActivityEvent
// --------------------------------------------------------------------------

func TestCanReadActivityEvent_ControllerAUserSeesAEvent(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "a-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "user-a"}

	e := activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}
	if !a.CanReadActivityEvent(claims, e) {
		t.Error("expected caller to see controller A event (namespace-scoped on ns-a)")
	}
}

func TestCanReadActivityEvent_ControllerAUserDeniedBEvent(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "a-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "user-a"}

	e := activity.Event{Namespace: "ns-b", Controller: "ctrl-b"}
	if a.CanReadActivityEvent(claims, e) {
		t.Error("expected caller to be denied controller B event")
	}
}

func TestCanReadActivityEvent_ControllerAUserDeniedGlobalEvent(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "a-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "user-a"}

	e := activity.Event{Namespace: "", Controller: ""} // global event
	if a.CanReadActivityEvent(claims, e) {
		t.Error("expected controller-scoped-only caller to be denied global event")
	}
}

func TestCanReadActivityEvent_ClusterViewerSeesAllControllersAndGlobal(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cluster-viewer"}},
			RoleRef:  "viewer",
			// Scope: nil = cluster-wide
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "cluster-viewer"}

	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}) {
		t.Error("expected cluster viewer to see controller A event")
	}
	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "ns-b", Controller: "ctrl-b"}) {
		t.Error("expected cluster viewer to see controller B event")
	}
	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "", Controller: ""}) {
		t.Error("expected cluster viewer to see global event")
	}
}

func TestCanReadActivityEvent_AdminSeesEverything(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "admin"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin-user"}},
			RoleRef:  "admin",
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "admin-user"}

	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}) {
		t.Error("expected admin to see controller A event")
	}
	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "", Controller: ""}) {
		t.Error("expected admin to see global event")
	}
}

func TestCanReadActivityEvent_ControllerAndClusterCaps(t *testing.T) {
	// Caller with both namespace-scoped (controller-A) and cluster-wide bindings.
	// EffectiveClusterAPICapabilities should only see the cluster-wide one,
	// and EffectiveAPICapabilities should see both (since the cluster-wide
	// binding covers all controllers).
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "mixed-user"}},
			RoleRef:  "viewer",
			// Scope: nil = cluster-wide
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "mixed-user"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "mixed-user"}

	// Mixed user can read ns-a/ctrl-a via the namespace-scoped binding.
	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}) {
		t.Error("expected mixed user to see controller A event (ns-scoped)")
	}
	// Global event visible via the cluster-wide binding (EffectiveClusterAPICapabilities).
	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "", Controller: ""}) {
		t.Error("expected mixed user to see global event (cluster-wide)")
	}
	// Controller B also visible via the cluster-wide binding (EffectiveAPICapabilities
	// with nil-scope binding matches any controller).
	if !a.CanReadActivityEvent(claims, activity.Event{Namespace: "ns-b", Controller: "ctrl-b"}) {
		t.Error("expected mixed user to see controller B via cluster-wide binding")
	}
}

func TestCanReadActivityEvent_NilAuthorizerResolver(t *testing.T) {
	// When the Authorizer has a nil resolver and non-nil claims,
	// CanReadController panics. This is by design — callers (handlers)
	// must check authz == nil before calling. This test verifies that
	// a nil resolver with nil claims (anonymous) falls through to
	// defaultRead correctly.
	a := NewAuthorizer(nil, false)
	if a.CanReadActivityEvent(nil, activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}) {
		t.Error("expected nil-resolver with nil claims and no defaultRead to deny")
	}

	a2 := NewAuthorizer(nil, true)
	if !a2.CanReadActivityEvent(nil, activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}) {
		t.Error("expected nil-resolver with nil claims and defaultRead to allow")
	}
}

// --------------------------------------------------------------------------
// Tests for CanReadGlobalActivity
// --------------------------------------------------------------------------

func TestCanReadGlobalActivity_ClusterViewer(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "cluster-viewer"}},
			RoleRef:  "viewer",
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "cluster-viewer"}

	if !a.CanReadGlobalActivity(claims) {
		t.Error("expected cluster viewer to read global activity")
	}
}

func TestCanReadGlobalActivity_ControllerScopedOnly(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "ctrl-user"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "ctrl-user"}

	if a.CanReadGlobalActivity(claims) {
		t.Error("expected controller-scoped-only caller to be denied global activity")
	}
}

func TestCanReadGlobalActivity_Admin(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "admin"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin"}},
			RoleRef:  "admin",
		}},
	}
	r := testResolver(roles, bindings)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "admin"}

	if !a.CanReadGlobalActivity(claims) {
		t.Error("expected admin to read global activity")
	}
}

func TestCanReadGlobalActivity_DefaultRead(t *testing.T) {
	a := NewAuthorizer(nil, true)

	if !a.CanReadGlobalActivity(nil) {
		t.Error("expected nil claims with defaultRead to read global activity")
	}
}

func TestCanReadGlobalActivity_NoDefaultRead(t *testing.T) {
	a := NewAuthorizer(nil, false)

	if a.CanReadGlobalActivity(nil) {
		t.Error("expected nil claims without defaultRead to be denied global activity")
	}
}

func TestCanReadGlobalActivity_EmptyClaimsNoDefaultRead(t *testing.T) {
	r := testResolver(nil, nil)
	a := NewAuthorizer(r, false)
	claims := &auth.Claims{Subject: "some-user"}

	if a.CanReadGlobalActivity(claims) {
		t.Error("expected user without cluster-wide caps to be denied global activity")
	}
}

// --------------------------------------------------------------------------
// Tests for EffectivePermissions
// --------------------------------------------------------------------------

func TestEffectivePermissions_ClusterWideCaller(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "admin"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"*"}, Verbs: []string{"*"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "admin"}},
			RoleRef:  "admin",
		}},
	}
	a := NewAuthorizer(testResolver(roles, bindings), false)
	claims := &auth.Claims{Subject: "admin"}

	ps := a.EffectivePermissions(claims)
	if !ps.Global["*"]["*"] {
		t.Error("expected Global.*.*=true for admin")
	}
	if len(ps.Scopes) != 0 {
		t.Errorf("expected empty Scopes for cluster-wide admin, got %d entries", len(ps.Scopes))
	}
}

func TestEffectivePermissions_ScopedOnlyCaller(t *testing.T) {
	roles := []*v1alpha1.VarroaRole{
		{ObjectMeta: metav1.ObjectMeta{Name: "viewer"}, Spec: v1alpha1.VarroaRoleSpec{
			APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-binding"}, Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "scoped-user"}},
			RoleRef:  "viewer",
			Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
		}},
	}
	a := NewAuthorizer(testResolver(roles, bindings), false)
	claims := &auth.Claims{Subject: "scoped-user"}

	ps := a.EffectivePermissions(claims)
	if len(ps.Global) != 0 {
		t.Errorf("expected empty Global for scoped-only caller, got %v", ps.Global)
	}
	if len(ps.Scopes) != 1 {
		t.Fatalf("expected 1 Scopes entry, got %d", len(ps.Scopes))
	}
	if !ps.Scopes[0].Capabilities["controllers"]["read"] {
		t.Error("expected Scopes[0].Capabilities.controllers.read=true")
	}
}

func TestEffectivePermissions_NilClaimsDefaultRead(t *testing.T) {
	// Create a resolver with defaultRead=true for this test.
	roleIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	roleInf := &fakeInformer{indexer: roleIdx}
	bindingIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	bindingInf := &fakeInformer{indexer: bindingIdx}
	jrIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	jrInf := &fakeInformer{indexer: jrIdx}
	jrbIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	jrbInf := &fakeInformer{indexer: jrbIdx}
	ctrlIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	ctrlInf := &fakeInformer{indexer: ctrlIdx}

	r := rbac.NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, true, []string{"sub"}, []string{"groups"})
	a := NewAuthorizer(r, true)
	ps := a.EffectivePermissions(nil)
	if !ps.Global["controllers"]["read"] {
		t.Error("expected Global.controllers.read=true with nil claims and defaultRead=true")
	}
	if len(ps.Scopes) != 0 {
		t.Errorf("expected empty Scopes, got %d entries", len(ps.Scopes))
	}

	r2 := rbac.NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, false, []string{"sub"}, []string{"groups"})
	a2 := NewAuthorizer(r2, false)
	ps2 := a2.EffectivePermissions(nil)
	if len(ps2.Global) != 0 {
		t.Errorf("expected empty Global with nil claims and defaultRead=false, got %v", ps2.Global)
	}
}

// --------------------------------------------------------------------------
// Tests for group-A cluster-scoped-resource gates (scope-cluster-resource-gates)
// --------------------------------------------------------------------------

func TestGlobalOnlyGates_NamespaceScopedOperatorDenied(t *testing.T) {
	// A caller with ONLY a namespace-scoped operator binding must NOT pass
	// cluster-scoped-resource gates (the leak fix).
	opRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"provisioningdefaults"}, Verbs: []string{"read", "update"}},
			{Resources: []string{"roles", "rolebindings", "jenkinsroles", "jenkinsrolebindings"}, Verbs: []string{"read"}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-operator"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "operator",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{opRole}, bindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if a.CanUpdateProvisioningDefaults(claims) {
		t.Error("namespace-scoped operator must be denied CanUpdateProvisioningDefaults")
	}
	if a.CanReadRoles(claims) {
		t.Error("namespace-scoped operator must be denied CanReadRoles")
	}
	if a.CanReadRoleBindings(claims) {
		t.Error("namespace-scoped operator must be denied CanReadRoleBindings")
	}
	if a.CanReadJenkinsRoles(claims) {
		t.Error("namespace-scoped operator must be denied CanReadJenkinsRoles")
	}
	if a.CanReadJenkinsRoleBindings(claims) {
		t.Error("namespace-scoped operator must be denied CanReadJenkinsRoleBindings")
	}
}

func TestGlobalOnlyGates_ClusterWideOperatorAllowed(t *testing.T) {
	// A caller with a cluster-wide (nil-scope) operator binding passes.
	opRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"provisioningdefaults"}, Verbs: []string{"read", "update"}},
			{Resources: []string{"roles", "rolebindings", "jenkinsroles", "jenkinsrolebindings"}, Verbs: []string{"read"}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-operator"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "operator",
				// Scope: nil = cluster-wide
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{opRole}, bindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if !a.CanUpdateProvisioningDefaults(claims) {
		t.Error("cluster-wide operator must be allowed CanUpdateProvisioningDefaults")
	}
	if !a.CanReadRoles(claims) {
		t.Error("cluster-wide operator must be allowed CanReadRoles")
	}
	if !a.CanReadRoleBindings(claims) {
		t.Error("cluster-wide operator must be allowed CanReadRoleBindings")
	}
	if !a.CanReadJenkinsRoles(claims) {
		t.Error("cluster-wide operator must be allowed CanReadJenkinsRoles")
	}
	if !a.CanReadJenkinsRoleBindings(claims) {
		t.Error("cluster-wide operator must be allowed CanReadJenkinsRoleBindings")
	}
}

func TestIsAdmin_NamespaceScopedAdminDenied(t *testing.T) {
	// A caller with ONLY a namespace-scoped admin binding must NOT pass IsAdmin
	// or any admin-level CUD gate.
	adminRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"*"}, Verbs: []string{"*"}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-admin"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "admin",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{adminRole}, bindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if a.IsAdmin(claims) {
		t.Error("namespace-scoped admin must be denied IsAdmin")
	}
	if a.CanCreateRole(claims) {
		t.Error("namespace-scoped admin must be denied CanCreateRole")
	}
	if a.CanDeleteRole(claims) {
		t.Error("namespace-scoped admin must be denied CanDeleteRole")
	}
	if a.CanCreateRoleBinding(claims) {
		t.Error("namespace-scoped admin must be denied CanCreateRoleBinding")
	}
	if a.CanCreateJenkinsRole(claims) {
		t.Error("namespace-scoped admin must be denied CanCreateJenkinsRole")
	}
	if a.CanCreateJenkinsRoleBinding(claims) {
		t.Error("namespace-scoped admin must be denied CanCreateJenkinsRoleBinding")
	}
}

func TestIsAdmin_ClusterWideAdminAllowed(t *testing.T) {
	// A caller with a cluster-wide (nil-scope) admin binding passes IsAdmin
	// and all admin-level CUD gates.
	adminRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"*"}, Verbs: []string{"*"}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-admin"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "admin",
				// Scope: nil = cluster-wide
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{adminRole}, bindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if !a.IsAdmin(claims) {
		t.Error("cluster-wide admin must be allowed IsAdmin")
	}
	if !a.CanCreateRole(claims) {
		t.Error("cluster-wide admin must be allowed CanCreateRole")
	}
	if !a.CanDeleteRole(claims) {
		t.Error("cluster-wide admin must be allowed CanDeleteRole")
	}
	if !a.CanCreateRoleBinding(claims) {
		t.Error("cluster-wide admin must be allowed CanCreateRoleBinding")
	}
	if !a.CanCreateJenkinsRole(claims) {
		t.Error("cluster-wide admin must be allowed CanCreateJenkinsRole")
	}
	if !a.CanCreateJenkinsRoleBinding(claims) {
		t.Error("cluster-wide admin must be allowed CanCreateJenkinsRoleBinding")
	}
}

// --------------------------------------------------------------------------
// Tests for group-B namespace-allowed gates (must-not-regress)
// --------------------------------------------------------------------------

func TestNamespaceAllowedGates_ScopedDeveloperKeepsAccess(t *testing.T) {
	// A namespace-scoped developer must still pass group-B gates (catalogsources,
	// catalogitems, composedbundles) — this MUST NOT regress.
	devRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"catalogsources"}, Verbs: []string{"read"}},
			{Resources: []string{"catalogitems"}, Verbs: []string{"read"}},
			{Resources: []string{"composedbundles"}, Verbs: []string{"read", "create", "update"}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-developer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "developer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{devRole}, bindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if !a.CanReadCatalogSources(claims) {
		t.Error("namespace-scoped developer must be allowed CanReadCatalogSources")
	}
	if !a.CanReadCatalogItems(claims) {
		t.Error("namespace-scoped developer must be allowed CanReadCatalogItems")
	}
	if !a.CanReadComposedBundles(claims) {
		t.Error("namespace-scoped developer must be allowed CanReadComposedBundles")
	}
	if !a.CanWriteComposedBundles(claims, "create") {
		t.Error("namespace-scoped developer must be allowed CanWriteComposedBundles(create)")
	}
	if !a.CanWriteComposedBundles(claims, "update") {
		t.Error("namespace-scoped developer must be allowed CanWriteComposedBundles(update)")
	}
}

func TestNamespaceAllowedGates_ScopedViewerKeepsReads(t *testing.T) {
	// A namespace-scoped viewer with read-only access to catalog resources
	// must still pass the read gates — MUST NOT regress.
	viewerRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"catalogsources", "catalogitems", "composedbundles"}, Verbs: []string{"read"}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-viewer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "viewer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{viewerRole}, bindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if !a.CanReadCatalogSources(claims) {
		t.Error("namespace-scoped viewer must be allowed CanReadCatalogSources")
	}
	if !a.CanReadCatalogItems(claims) {
		t.Error("namespace-scoped viewer must be allowed CanReadCatalogItems")
	}
	if !a.CanReadComposedBundles(claims) {
		t.Error("namespace-scoped viewer must be allowed CanReadComposedBundles")
	}
}

// --------------------------------------------------------------------------
// Tests for group-C per-controller gates (unaffected by this change)
// --------------------------------------------------------------------------

func TestPerControllerGates_Unaffected(t *testing.T) {
	// A namespace-scoped caller with controllers:read must pass
	// CanReadController for the bound namespace but be denied for others.
	ctrlRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"controllers"}, Verbs: []string{"read"}},
		}},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-viewer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "viewer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{ctrlRole}, bindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if !a.CanReadController(claims, "team-a", "c1") {
		t.Error("namespace-scoped viewer must be allowed CanReadController in bound namespace")
	}
	if a.CanReadController(claims, "team-b", "c1") {
		t.Error("namespace-scoped viewer must be denied CanReadController outside bound namespace")
	}
}

// --------------------------------------------------------------------------
// Tests for CanManageCatalogSourcesInNamespace (add-namespace-scoped-catalog-rbac)
// --------------------------------------------------------------------------

func TestCanManageCatalogSourcesInNamespace(t *testing.T) {
	devRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "developer"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"catalogsources"}, Verbs: []string{"create", "update", "delete"}},
		}},
	}
	scopedBindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped-developer"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-a"}},
				RoleRef:  "developer",
				Scope:    &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"team-a"}},
			}},
	}
	a := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{devRole}, scopedBindings), false)
	claims := &auth.Claims{Subject: "user-a"}

	if !a.CanManageCatalogSourcesInNamespace(claims, "create", "team-a") {
		t.Error("scoped developer must be allowed to create CatalogSources in its own namespace")
	}
	if a.CanManageCatalogSourcesInNamespace(claims, "create", "team-b") {
		t.Error("scoped developer must be denied creating CatalogSources in another namespace")
	}

	opRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "operator"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"catalogsources"}, Verbs: []string{"create", "update", "delete"}},
		}},
	}
	clusterWideBindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide-operator"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-b"}},
				RoleRef:  "operator",
				// Scope: nil = cluster-wide
			}},
	}
	aOp := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{opRole}, clusterWideBindings), false)
	claimsOp := &auth.Claims{Subject: "user-b"}
	if !aOp.CanManageCatalogSourcesInNamespace(claimsOp, "create", "team-a") {
		t.Error("cluster-wide operator must be allowed to create CatalogSources in any namespace")
	}
	if !aOp.CanManageCatalogSourcesInNamespace(claimsOp, "create", "team-z") {
		t.Error("cluster-wide operator must be allowed to create CatalogSources in any namespace")
	}

	if a.CanManageCatalogSourcesInNamespace(nil, "create", "team-a") {
		t.Error("nil claims must be denied CanManageCatalogSourcesInNamespace")
	}

	// Empty namespace must deny even for callers that hold the verb somewhere:
	// EffectiveAPICapabilities skips scope filtering when namespace and
	// controllerName are both empty, so without the guard a namespace-scoped
	// binding would pass the write gate for an unspecified target namespace.
	if a.CanManageCatalogSourcesInNamespace(claims, "create", "") {
		t.Error("scoped developer must be denied CatalogSource create for an empty namespace")
	}
	if aOp.CanManageCatalogSourcesInNamespace(claimsOp, "create", "") {
		t.Error("even a cluster-wide operator must be denied for an empty namespace — callers must name a target")
	}

	selectorRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "selector-scoped"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"catalogsources"}, Verbs: []string{"create"}},
		}},
	}
	selectorBindings := []*v1alpha1.VarroaRoleBinding{
		{ObjectMeta: metav1.ObjectMeta{Name: "selector-scoped-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user-c"}},
				RoleRef:  "selector-scoped",
				Scope: &v1alpha1.VarroaRoleBindingScope{
					ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
				},
			}},
	}
	aSel := NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{selectorRole}, selectorBindings), false)
	claimsSel := &auth.Claims{Subject: "user-c"}
	for _, ns := range []string{"team-a", "team-b", "varroa-system"} {
		if aSel.CanManageCatalogSourcesInNamespace(claimsSel, "create", ns) {
			t.Errorf("controller-selector-scoped binding must be denied CatalogSource create in %q — "+
				"CatalogSource is not a per-controller resource, so a selector scope can never match", ns)
		}
	}
}

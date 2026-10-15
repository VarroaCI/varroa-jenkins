package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// testResolverWithControllers mirrors authorizer_test.go's testResolver but
// also seeds the controller indexer, so a ControllerSelector-scoped binding
// can actually match a live controller — the case testResolver's callers
// never exercise, because none of them register a controller.
func testResolverWithControllers(roles []*v1alpha1.VarroaRole, bindings []*v1alpha1.VarroaRoleBinding, controllers []*v1alpha1.Controller) *rbac.Resolver {
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
	for _, c := range controllers {
		_ = ctrlIdx.Add(c)
	}
	ctrlInf := &fakeInformer{indexer: ctrlIdx}

	return rbac.NewResolver(roleInf, bindingInf, jrInf, jrbInf, ctrlInf, nil, false, []string{"sub", "preferred_username"})
}

// activityScopeShape names the four real VarroaRoleBindingScope shapes.
type activityScopeShape struct {
	name        string
	authorizer  func() *Authorizer
	claims      *auth.Claims
	sampleEvent activity.Event // an event the shape's binding actually admits
}

func activityScopeShapes() []activityScopeShape {
	readRole := &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}},
	}
	claims := &auth.Claims{Subject: "user"}

	return []activityScopeShape{
		{
			name: "nil scope (cluster-wide)",
			authorizer: func() *Authorizer {
				bindings := []*v1alpha1.VarroaRoleBinding{{
					ObjectMeta: metav1.ObjectMeta{Name: "cluster-wide"},
					Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user"}}, RoleRef: "viewer"},
				}}
				return NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{readRole}, bindings), false)
			},
			claims:      claims,
			sampleEvent: activity.Event{Namespace: "", Controller: ""}, // global event
		},
		{
			name: "Namespaces list",
			authorizer: func() *Authorizer {
				bindings := []*v1alpha1.VarroaRoleBinding{{
					ObjectMeta: metav1.ObjectMeta{Name: "ns-scoped"},
					Spec: v1alpha1.VarroaRoleBindingSpec{
						Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user"}}, RoleRef: "viewer",
						Scope: &v1alpha1.VarroaRoleBindingScope{Namespaces: []string{"ns-a"}},
					},
				}}
				return NewAuthorizer(testResolver([]*v1alpha1.VarroaRole{readRole}, bindings), false)
			},
			claims:      claims,
			sampleEvent: activity.Event{Namespace: "ns-a", Controller: "ctrl-a"},
		},
		{
			name: "ControllerSelector",
			authorizer: func() *Authorizer {
				bindings := []*v1alpha1.VarroaRoleBinding{{
					ObjectMeta: metav1.ObjectMeta{Name: "selector-scoped"},
					Spec: v1alpha1.VarroaRoleBindingSpec{
						Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "user"}}, RoleRef: "viewer",
						Scope: &v1alpha1.VarroaRoleBindingScope{ControllerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}},
					},
				}}
				controllers := []*v1alpha1.Controller{{
					ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "ns-a", Labels: map[string]string{"team": "a"}},
				}}
				return NewAuthorizer(testResolverWithControllers([]*v1alpha1.VarroaRole{readRole}, bindings, controllers), false)
			},
			claims:      claims,
			sampleEvent: activity.Event{Namespace: "ns-a", Controller: "ctrl-a"},
		},
		{
			name: "no binding at all",
			authorizer: func() *Authorizer {
				return NewAuthorizer(testResolver(nil, nil), false)
			},
			claims:      claims,
			sampleEvent: activity.Event{Namespace: "ns-a", Controller: "ctrl-a"}, // admitted by nobody
		},
	}
}

// TestActivityScope_SupersetOfPredicate proves, for every one of the four
// real binding shapes, that every event CanReadActivityEvent admits also
// passes Scope.Admits — the property that makes the SQL pushdown sound.
// It fails if the ControllerSelector fallback (Scope.All = true) is
// dropped: without it, the selector-scoped shape's admitted event would
// fail Scope.Admits, since the scope has no namespace list of its own.
func TestActivityScope_SupersetOfPredicate(t *testing.T) {
	for _, shape := range activityScopeShapes() {
		t.Run(shape.name, func(t *testing.T) {
			a := shape.authorizer()
			scope := activity.ScopeFromPermissions(a.EffectivePermissions(shape.claims), a.CanReadGlobalActivity(shape.claims))

			admitted := a.CanReadActivityEvent(shape.claims, shape.sampleEvent)
			if shape.name == "no binding at all" && admitted {
				t.Fatalf("test setup: expected no binding to deny the sample event")
			}
			if admitted && !scope.Admits(shape.sampleEvent) {
				t.Fatalf("predicate admitted %+v but Scope %+v did not — the SQL pushdown would silently drop an authorized row", shape.sampleEvent, scope)
			}

			// The superset property must hold for any event, not only the
			// shape's own sample: an unrelated event the predicate happens
			// to admit must also pass Scope.Admits.
			unrelated := activity.Event{Namespace: "unrelated-ns", Controller: "unrelated-ctrl"}
			if a.CanReadActivityEvent(shape.claims, unrelated) && !scope.Admits(unrelated) {
				t.Fatalf("predicate admitted an unrelated event %+v but Scope %+v did not", unrelated, scope)
			}
		})
	}
}

package rbac

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// NewTestResolver creates a Resolver with empty in-memory stores, suitable for
// unit tests that do not need to exercise RBAC resolution logic.
func NewTestResolver() *Resolver {
	roleIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	bindingIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		BySubjectIndex: SubjectIndexFunc,
	})
	jenkinsRoleIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	jenkinsRoleBindingIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		JenkinsBySubjectIndex: JenkinsSubjectIndexFunc,
	})
	controllerIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	return &Resolver{
		roleLister:              cache.NewGenericLister(roleIndexer, schema.GroupResource{Group: "varroa.dev", Resource: "varroaroles"}),
		roleBindingIndex:        bindingIndexer,
		jenkinsRoleLister:       cache.NewGenericLister(jenkinsRoleIndexer, schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsroles"}),
		jenkinsRoleBindingIndex: jenkinsRoleBindingIndexer,
		controllerLister:        cache.NewGenericLister(controllerIndexer, schema.GroupResource{Group: "varroa.dev", Resource: "controllers"}),
		defaultRead:             true,
		userClaimNames:          []string{"sub", "preferred_username"},
		groupClaimNames:         []string{"groups"},
	}
}

// NewTestResolverWithRoles creates a Resolver seeded with the given VarroaRoles
// and VarroaRoleBindings, for tests that need to exercise capability resolution
// (e.g. distinguishing an admin with wildcard */* from a non-admin).
func NewTestResolverWithRoles(roles []*v1alpha1.VarroaRole, bindings []*v1alpha1.VarroaRoleBinding) *Resolver {
	r := NewTestResolver()
	r.defaultRead = false
	roleIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, role := range roles {
		_ = roleIndexer.Add(role)
	}
	r.roleLister = cache.NewGenericLister(roleIndexer, schema.GroupResource{Group: "varroa.dev", Resource: "varroaroles"})

	bindingIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{BySubjectIndex: SubjectIndexFunc})
	for _, b := range bindings {
		_ = bindingIndexer.Add(b)
	}
	r.roleBindingIndex = bindingIndexer
	return r
}

// NewTestResolverWithJenkins creates a Resolver seeded with the given JenkinsRoles
// and JenkinsRoleBindings, for tests that need human-admin presence or other
// JenkinsRoleAssignment behavior.
func NewTestResolverWithJenkins(jr []*v1alpha1.JenkinsRole, jrb []*v1alpha1.JenkinsRoleBinding) *Resolver {
	r := NewTestResolver()
	r.defaultRead = false
	jrIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, role := range jr {
		_ = jrIdx.Add(role)
	}
	r.jenkinsRoleLister = cache.NewGenericLister(jrIdx, schema.GroupResource{Group: "varroa.dev", Resource: "jenkinsroles"})
	jrbIdx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{JenkinsBySubjectIndex: JenkinsSubjectIndexFunc})
	for _, b := range jrb {
		_ = jrbIdx.Add(b)
	}
	r.jenkinsRoleBindingIndex = jrbIdx
	return r
}

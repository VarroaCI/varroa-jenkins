package rbac

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestResolver_GroupBindingFromResolvedGroups checks that a kind:Group binding
// applies to Claims.Groups. The OIDC validator fills that field from the
// configured group claim, so the resolver needs no claim-name list.
func TestResolver_GroupBindingFromResolvedGroups(t *testing.T) {
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
			ObjectMeta: metav1.ObjectMeta{Name: "dev-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "a"}},
				RoleRef:  "developer",
			},
		},
	}
	controllers := []*v1alpha1.Controller{
		{ObjectMeta: metav1.ObjectMeta{Name: "test-ctrl", Namespace: "tenant-a"}},
	}

	r := testResolver(roles, bindings, controllers, false)

	granted := r.EffectiveAPICapabilities(&auth.Claims{Subject: "u", Groups: []string{"a"}}, "tenant-a", "test-ctrl")
	if !granted["controllers"]["read"] {
		t.Error("group a in Claims.Groups: expected controllers:read")
	}

	denied := r.EffectiveAPICapabilities(&auth.Claims{Subject: "u", Groups: []string{"b"}}, "tenant-a", "test-ctrl")
	if denied["controllers"]["read"] {
		t.Error("group b in Claims.Groups: expected no controllers:read")
	}
}

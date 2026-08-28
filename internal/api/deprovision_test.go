package api

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func subj(kind, name string) v1alpha1.SubjectRef { return v1alpha1.SubjectRef{Kind: kind, Name: name} }

func TestDeprovisionUser_CrossPlaneCascade(t *testing.T) {
	store := crdstore.NewFake()
	ctx := context.Background()

	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alice",
			Annotations: map[string]string{
				v1alpha1.AnnotationOIDCSubject:           "subj-1",
				v1alpha1.AnnotationOIDCPreferredUsername: "alice-preferred",
			},
		},
		Spec: v1alpha1.UserSpec{Email: "alice@example.com", DisplayName: "Alice Cooper"},
	}

	crdstore.MustSeed(store,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-shared"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice@example.com"), subj("Group", "devs")}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-solo"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice-preferred")}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-displayname"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "Alice Cooper")}},
		},
		&v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "jrb-solo"},
			Spec:       v1alpha1.JenkinsRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice")}},
		},
		&v1alpha1.Group{
			ObjectMeta: metav1.ObjectMeta{Name: "g1"},
			Spec:       v1alpha1.GroupSpec{Members: []string{"alice", "bob"}},
		},
	)

	if err := deprovisionUser(ctx, store, user, string(auth.AuthModeLocal)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}

	shared, _ := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-shared", "")
	if shared == nil || len(shared.Spec.Subjects) != 1 || shared.Spec.Subjects[0].Kind != "Group" {
		t.Errorf("expected vrb-shared to retain only the Group subject, got %+v", shared)
	}
	if _, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-solo", ""); err == nil {
		t.Error("expected vrb-solo (emptied) to be deleted")
	}
	if _, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, store, "jrb-solo", ""); err == nil {
		t.Error("expected jrb-solo (emptied) to be deleted")
	}
	if dn, _ := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-displayname", ""); dn == nil || len(dn.Spec.Subjects) != 1 {
		t.Error("expected vrb-displayname to be untouched")
	}
	if g, _ := crdstore.Get[v1alpha1.Group](ctx, store, "g1", ""); g == nil || len(g.Spec.Members) != 1 || g.Spec.Members[0] != "bob" {
		t.Errorf("expected group g1 to retain only 'bob', got %+v", g)
	}
}

func TestDeprovisionUser_OIDCSkipsGroupCleanup(t *testing.T) {
	store := crdstore.NewFake()
	ctx := context.Background()

	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "oidc-abc"},
		Spec:       v1alpha1.UserSpec{Email: "x@example.com"},
	}
	crdstore.MustSeed(store, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: "g1"},
		Spec:       v1alpha1.GroupSpec{Members: []string{"oidc-abc"}},
	})
	if err := deprovisionUser(ctx, store, user, string(auth.AuthModeOIDC)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}
	g, _ := crdstore.Get[v1alpha1.Group](ctx, store, "g1", "")
	if g == nil || len(g.Spec.Members) != 1 {
		t.Error("expected group membership untouched in OIDC mode")
	}
}

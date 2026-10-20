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

func TestDeprovisionUser_StripsFormerIdentifiers(t *testing.T) {
	store := crdstore.NewFake()
	ctx := context.Background()

	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: "carol",
			Annotations: map[string]string{
				v1alpha1.AnnotationOIDCSubject:           "subj-carol",
				v1alpha1.AnnotationOIDCFormerIdentifiers: `["old-name","old@example.com"]`,
			},
		},
		Spec: v1alpha1.UserSpec{Email: "carol@example.com"},
	}

	crdstore.MustSeed(store,
		&v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-old-name"},
			Spec:       v1alpha1.JenkinsRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "old-name")}},
		},
		&v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "jrb-old-email"},
			Spec: v1alpha1.JenkinsRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{
				subj("User", "old@example.com"), subj("Group", "devs"),
			}},
		},
	)

	if err := deprovisionUser(ctx, store, user, string(auth.AuthModeOIDC)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}

	if b, _ := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, store, "vrb-old-name", ""); b != nil {
		t.Errorf("expected vrb-old-name deleted, got %+v", b)
	}
	kept, _ := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, store, "jrb-old-email", "")
	if kept == nil || len(kept.Spec.Subjects) != 1 || kept.Spec.Subjects[0].Kind != "Group" {
		t.Errorf("expected jrb-old-email to retain only the Group subject, got %+v", kept)
	}
}

func TestDeprovisionUser_KeepsFormerIdentifierHeldByLiveUser(t *testing.T) {
	store := crdstore.NewFake()
	ctx := context.Background()

	departing := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "alice",
			Labels:      map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations: map[string]string{v1alpha1.AnnotationOIDCFormerIdentifiers: `["dana"]`},
		},
	}
	live := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "bob",
			Labels:      map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations: map[string]string{v1alpha1.AnnotationOIDCPreferredUsername: "dana"},
		},
	}

	crdstore.MustSeed(store,
		live,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-dana"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "dana")}},
		},
	)

	if err := deprovisionUser(ctx, store, departing, string(auth.AuthModeOIDC)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}

	kept, _ := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-dana", "")
	if kept == nil || len(kept.Spec.Subjects) != 1 || kept.Spec.Subjects[0].Name != "dana" {
		t.Errorf("expected vrb-dana to keep subject dana held by a live user, got %+v", kept)
	}
}

func TestDeprovisionUser_StripsCustomUserClaimValue(t *testing.T) {
	store := crdstore.NewFake()
	ctx := context.Background()

	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dave",
			Annotations: map[string]string{
				v1alpha1.AnnotationOIDCSubject:    "subj-dave",
				v1alpha1.AnnotationOIDCUserClaims: `{"employee_id":"E123"}`,
			},
		},
		Spec: v1alpha1.UserSpec{Email: "dave@example.com"},
	}

	crdstore.MustSeed(store,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-employee"},
			Spec: v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{
				subj("User", "E123"), subj("Group", "devs"),
			}},
		},
	)

	if err := deprovisionUser(ctx, store, user, string(auth.AuthModeOIDC)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}

	kept, _ := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-employee", "")
	if kept == nil || len(kept.Spec.Subjects) != 1 || kept.Spec.Subjects[0].Kind != "Group" {
		t.Errorf("expected vrb-employee to retain only the Group subject, got %+v", kept)
	}
}

func TestDeprovisionUser_KeepsFormerCustomClaimHeldByLiveUser(t *testing.T) {
	store := crdstore.NewFake()
	ctx := context.Background()

	departing := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "alice",
			Labels:      map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations: map[string]string{v1alpha1.AnnotationOIDCFormerIdentifiers: `["E123"]`},
		},
	}
	live := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "bob",
			Labels:      map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations: map[string]string{v1alpha1.AnnotationOIDCUserClaims: `{"employee_id":"E123"}`},
		},
	}

	crdstore.MustSeed(store,
		live,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-e123"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "E123")}},
		},
	)

	if err := deprovisionUser(ctx, store, departing, string(auth.AuthModeOIDC)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}

	kept, _ := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-e123", "")
	if kept == nil || len(kept.Spec.Subjects) != 1 || kept.Spec.Subjects[0].Name != "E123" {
		t.Errorf("expected vrb-e123 to keep subject E123 held by a live user, got %+v", kept)
	}
}

func TestDeprovisionUser_KeepsCurrentCustomClaimHeldByLiveUser(t *testing.T) {
	store := crdstore.NewFake()
	ctx := context.Background()

	departing := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "alice",
			Labels:      map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations: map[string]string{v1alpha1.AnnotationOIDCUserClaims: `{"department":"engineering"}`},
		},
	}
	live := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "bob",
			Labels:      map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations: map[string]string{v1alpha1.AnnotationOIDCUserClaims: `{"department":"engineering"}`},
		},
	}

	crdstore.MustSeed(store,
		live,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-dept"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "engineering")}},
		},
	)

	if err := deprovisionUser(ctx, store, departing, string(auth.AuthModeOIDC)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}

	kept, _ := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, store, "vrb-dept", "")
	if kept == nil || len(kept.Spec.Subjects) != 1 || kept.Spec.Subjects[0].Name != "engineering" {
		t.Errorf("expected vrb-dept to keep subject engineering held by a live user, got %+v", kept)
	}
}

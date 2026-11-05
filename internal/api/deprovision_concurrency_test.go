package api

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// onceAfterGet runs write once, after the first read of the named object.
// Every later read of that object passes through.
func onceAfterGet(t *testing.T, st *crdstore.Fake, gvr schema.GroupVersionResource, target string, write func()) {
	t.Helper()
	fired := false
	st.SetAfterGet(gvr, func(_, name string) {
		if fired || name != target {
			return
		}
		fired = true
		write()
	})
}

// TestDeprovisionUser_ConcurrentSubjectAddSurvives pins that a subject added
// after the deprovision list survives the strip of a shared binding.
func TestDeprovisionUser_ConcurrentSubjectAddSurvives(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()
	crdstore.MustSeed(st, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb-shared"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice"), subj("Group", "devs")}},
	})
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	onceAfterGet(t, st, gvr, "vrb-shared", func() {
		if _, err := crdstore.Mutate[v1alpha1.VarroaRoleBinding](ctx, st, "vrb-shared", "", func(b *v1alpha1.VarroaRoleBinding) (bool, error) {
			b.Spec.Subjects = append(b.Spec.Subjects, subj("User", "carol"))
			return true, nil
		}); err != nil {
			t.Errorf("concurrent add: %v", err)
		}
	})

	if err := deprovisionUser(ctx, st, &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}}, string(auth.AuthModeLocal)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}
	got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, st, "vrb-shared", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []v1alpha1.SubjectRef{subj("Group", "devs"), subj("User", "carol")}
	if len(got.Spec.Subjects) != len(want) || got.Spec.Subjects[0] != want[0] || got.Spec.Subjects[1] != want[1] {
		t.Errorf("subjects = %+v, want %+v", got.Spec.Subjects, want)
	}
}

// TestDeprovisionUser_ConcurrentAddKeepsEmptiedBinding pins that a subject
// added after the list keeps a binding that the strip would otherwise delete.
func TestDeprovisionUser_ConcurrentAddKeepsEmptiedBinding(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()
	crdstore.MustSeed(st, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb-solo"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice")}},
	})
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	onceAfterGet(t, st, gvr, "vrb-solo", func() {
		if _, err := crdstore.Mutate[v1alpha1.VarroaRoleBinding](ctx, st, "vrb-solo", "", func(b *v1alpha1.VarroaRoleBinding) (bool, error) {
			b.Spec.Subjects = append(b.Spec.Subjects, subj("User", "carol"))
			return true, nil
		}); err != nil {
			t.Errorf("concurrent add: %v", err)
		}
	})

	if err := deprovisionUser(ctx, st, &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}}, string(auth.AuthModeLocal)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}
	got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, st, "vrb-solo", "")
	if err != nil {
		t.Fatalf("binding deleted, want it kept: %v", err)
	}
	if len(got.Spec.Subjects) != 1 || got.Spec.Subjects[0] != subj("User", "carol") {
		t.Errorf("subjects = %+v, want [carol]", got.Spec.Subjects)
	}
}

// TestDeprovisionUser_ConcurrentAddKeepsEmptiedJenkinsBinding is the data-plane
// counterpart of TestDeprovisionUser_ConcurrentAddKeepsEmptiedBinding.
func TestDeprovisionUser_ConcurrentAddKeepsEmptiedJenkinsBinding(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()
	crdstore.MustSeed(st, &v1alpha1.JenkinsRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "jrb-solo"},
		Spec:       v1alpha1.JenkinsRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice")}},
	})
	gvr, _ := crdstore.GVRFor[v1alpha1.JenkinsRoleBinding]()
	onceAfterGet(t, st, gvr, "jrb-solo", func() {
		if _, err := crdstore.Mutate[v1alpha1.JenkinsRoleBinding](ctx, st, "jrb-solo", "", func(b *v1alpha1.JenkinsRoleBinding) (bool, error) {
			b.Spec.Subjects = append(b.Spec.Subjects, subj("User", "carol"))
			return true, nil
		}); err != nil {
			t.Errorf("concurrent add: %v", err)
		}
	})

	if err := deprovisionUser(ctx, st, &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}}, string(auth.AuthModeLocal)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}
	got, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](ctx, st, "jrb-solo", "")
	if err != nil {
		t.Fatalf("binding deleted, want it kept: %v", err)
	}
	if len(got.Spec.Subjects) != 1 || got.Spec.Subjects[0] != subj("User", "carol") {
		t.Errorf("subjects = %+v, want [carol]", got.Spec.Subjects)
	}
}

// TestDeprovisionUser_ConcurrentGroupMemberAddSurvives pins that a member added
// after the group list survives the removal of a local account.
func TestDeprovisionUser_ConcurrentGroupMemberAddSurvives(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()
	crdstore.MustSeed(st, &v1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{Name: "g1"},
		Spec:       v1alpha1.GroupSpec{Members: []string{"alice", "bob"}},
	})
	gvr, _ := crdstore.GVRFor[v1alpha1.Group]()
	onceAfterGet(t, st, gvr, "g1", func() {
		if _, err := crdstore.Mutate[v1alpha1.Group](ctx, st, "g1", "", func(g *v1alpha1.Group) (bool, error) {
			g.Spec.Members = append(g.Spec.Members, "dave")
			return true, nil
		}); err != nil {
			t.Errorf("concurrent add: %v", err)
		}
	})

	if err := deprovisionUser(ctx, st, &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}}, string(auth.AuthModeLocal)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}
	got, err := crdstore.Get[v1alpha1.Group](ctx, st, "g1", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Members) != 2 || got.Spec.Members[0] != "bob" || got.Spec.Members[1] != "dave" {
		t.Errorf("members = %v, want [bob dave]", got.Spec.Members)
	}
}

// TestDeprovisionUser_GrantAfterListIsStripped pins that a grant of the
// departing user, added to a binding after the list, is removed. The binding
// held no grant at list time.
func TestDeprovisionUser_GrantAfterListIsStripped(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()
	crdstore.MustSeed(st,
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-first"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("Group", "ops")}},
		},
		&v1alpha1.VarroaRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "vrb-second"},
			Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "bob")}},
		},
	)
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	// On the first read, the grant lands on the other binding. The fake
	// lists in map order, so either binding may be read first. The binding
	// read first gets no grant. The other one gets it after the list and
	// before its own read.
	fired := false
	st.SetAfterGet(gvr, func(_, name string) {
		if fired {
			return
		}
		fired = true
		other := "vrb-second"
		if name == "vrb-second" {
			other = "vrb-first"
		}
		if _, err := crdstore.Mutate[v1alpha1.VarroaRoleBinding](ctx, st, other, "", func(b *v1alpha1.VarroaRoleBinding) (bool, error) {
			b.Spec.Subjects = append(b.Spec.Subjects, subj("User", "alice"))
			return true, nil
		}); err != nil {
			t.Errorf("concurrent grant: %v", err)
		}
	})

	if err := deprovisionUser(ctx, st, &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}}, string(auth.AuthModeLocal)); err != nil {
		t.Fatalf("deprovisionUser: %v", err)
	}
	want := map[string]v1alpha1.SubjectRef{
		"vrb-first":  subj("Group", "ops"),
		"vrb-second": subj("User", "bob"),
	}
	for name, keep := range want {
		got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, st, name, "")
		if err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		if len(got.Spec.Subjects) != 1 || got.Spec.Subjects[0] != keep {
			t.Errorf("%s subjects = %+v, want [%+v]", name, got.Spec.Subjects, keep)
		}
	}
}

// TestDeprovisionUser_BindingGoneAfterListIsSkipped pins that a binding deleted
// after the list does not fail the deprovision.
func TestDeprovisionUser_BindingGoneAfterListIsSkipped(t *testing.T) {
	ctx := context.Background()
	st := crdstore.NewFake()
	crdstore.MustSeed(st, &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vrb-gone"},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: []v1alpha1.SubjectRef{subj("User", "alice"), subj("Group", "devs")}},
	})
	gvr, _ := crdstore.GVRFor[v1alpha1.VarroaRoleBinding]()
	st.FailNext("get", gvr, apierrors.NewNotFound(gvr.GroupResource(), "vrb-gone"))

	if err := deprovisionUser(ctx, st, &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{Name: "alice"}}, string(auth.AuthModeLocal)); err != nil {
		t.Fatalf("deprovisionUser: %v, want nil", err)
	}
	got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](ctx, st, "vrb-gone", "")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Subjects) != 2 {
		t.Errorf("subjects = %+v, want the unchanged two", got.Spec.Subjects)
	}
}

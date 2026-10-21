package api

import (
	"context"
	"errors"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth/userdirectory"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// seedRecordLoginUser seeds an idp-managed User that RecordLogin refreshes.
// It returns the fake, the User GVR, and the User's namespace and name.
func seedRecordLoginUser(t *testing.T) (*crdstore.Fake, schema.GroupVersionResource) {
	t.Helper()
	f := crdstore.NewFake()
	gvr, err := crdstore.GVRFor[v1alpha1.User]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	crdstore.MustSeed(f, &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "oidc-test",
			Namespace:       "ns",
			ResourceVersion: "1",
			Labels:          map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByIDP},
			Annotations:     map[string]string{v1alpha1.AnnotationOIDCPreferredUsername: "alice"},
		},
		Spec: v1alpha1.UserSpec{Email: "old@example.com"},
	})
	return f, gvr
}

func getRecordLoginUser(t *testing.T, f *crdstore.Fake) *v1alpha1.User {
	t.Helper()
	u, err := crdstore.Get[v1alpha1.User](context.Background(), f, "oidc-test", "ns")
	if err != nil {
		t.Fatalf("Get user: %v", err)
	}
	return u
}

var testUserGR = schema.GroupResource{Group: "varroa.dev", Resource: "users"}

func TestRecordLogin_ConcurrentWriteKeepsFormerIdentifiers(t *testing.T) {
	ctx := context.Background()
	f, gvr := seedRecordLoginUser(t)
	fired := false
	f.SetAfterGet(gvr, func(ns, name string) {
		if fired {
			return
		}
		fired = true
		if _, err := f.PatchObjectMetaAt(ctx, gvr, ns, name, "1", map[string]any{
			"annotations": map[string]any{v1alpha1.AnnotationOIDCFormerIdentifiers: `["bob"]`},
		}); err != nil {
			t.Errorf("concurrent patch: %v", err)
		}
	})

	err := userdirectory.RecordLogin(ctx, userStoreAdapter{store: f}, "oidc-test",
		"new@example.com", "", nil, "sub", "alice", nil, "ns")
	if err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}

	u := getRecordLoginUser(t, f)
	if got := userdirectory.FormerIdentifiers(u); !slices.Equal(got, []string{"bob", "old@example.com"}) {
		t.Errorf("former identifiers = %v, want [bob old@example.com]", got)
	}
	if u.Spec.Email != "new@example.com" {
		t.Errorf("email = %q, want new@example.com", u.Spec.Email)
	}
	if n := len(f.StatusPatches(gvr)); n != 1 {
		t.Errorf("status patches = %d, want 1", n)
	}
}

func TestRecordLogin_ConflictBudgetSpentFailsWithoutStatusPatch(t *testing.T) {
	ctx := context.Background()
	f, gvr := seedRecordLoginUser(t)
	f.FailAlways("update", gvr, apierrors.NewConflict(testUserGR, "oidc-test", errors.New("busy")))

	err := userdirectory.RecordLogin(ctx, userStoreAdapter{store: f}, "oidc-test",
		"new@example.com", "", nil, "sub", "alice", nil, "ns")
	if !apierrors.IsConflict(err) {
		t.Fatalf("RecordLogin error = %v, want Conflict", err)
	}
	if n := len(f.StatusPatches(gvr)); n != 0 {
		t.Errorf("status patches = %d, want 0", n)
	}
	if e := getRecordLoginUser(t, f).Spec.Email; e != "old@example.com" {
		t.Errorf("email = %q, want old@example.com", e)
	}
}

func TestRecordLogin_ReadErrorKeepsRecord(t *testing.T) {
	ctx := context.Background()
	f, gvr := seedRecordLoginUser(t)
	if _, err := f.PatchObjectMetaAt(ctx, gvr, "ns", "oidc-test", "1", map[string]any{
		"annotations": map[string]any{v1alpha1.AnnotationOIDCFormerIdentifiers: `["bob"]`},
	}); err != nil {
		t.Fatalf("seed former identifiers: %v", err)
	}
	f.FailNext("get", gvr, apierrors.NewInternalError(errors.New("boom")))

	if err := userdirectory.RecordLogin(ctx, userStoreAdapter{store: f}, "oidc-test",
		"new@example.com", "", nil, "sub", "alice", nil, "ns"); err == nil {
		t.Fatal("RecordLogin error = nil, want a read error")
	}
	got := getRecordLoginUser(t, f)
	if fi := userdirectory.FormerIdentifiers(got); !slices.Equal(fi, []string{"bob"}) {
		t.Errorf("former identifiers = %v, want [bob]", fi)
	}
	if got.Spec.Email != "old@example.com" {
		t.Errorf("email = %q, want old@example.com", got.Spec.Email)
	}
}

func TestRecordLogin_FirstLoginRaceMergesIntoCreatedUser(t *testing.T) {
	ctx := context.Background()
	f, gvr := seedRecordLoginUser(t)
	if _, err := f.PatchObjectMetaAt(ctx, gvr, "ns", "oidc-test", "1", map[string]any{
		"annotations": map[string]any{v1alpha1.AnnotationOIDCFormerIdentifiers: `["bob"]`},
	}); err != nil {
		t.Fatalf("seed former identifiers: %v", err)
	}
	// The first read misses. The create then answers AlreadyExists, and the retry reads the User.
	f.FailNext("get", gvr, apierrors.NewNotFound(testUserGR, "oidc-test"))

	if err := userdirectory.RecordLogin(ctx, userStoreAdapter{store: f}, "oidc-test",
		"new@example.com", "", nil, "sub", "alice", nil, "ns"); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}
	u := getRecordLoginUser(t, f)
	if fi := userdirectory.FormerIdentifiers(u); !slices.Equal(fi, []string{"bob", "old@example.com"}) {
		t.Errorf("former identifiers = %v, want [bob old@example.com]", fi)
	}
	if u.Spec.Email != "new@example.com" {
		t.Errorf("email = %q, want new@example.com", u.Spec.Email)
	}
}

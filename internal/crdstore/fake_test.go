package crdstore

import (
	"context"
	"strconv"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// fakeRV parses the resourceVersion of obj, failing the test when it is not a
// number. The Fake assigns numeric values.
func fakeRV(t *testing.T, obj *v1alpha1.User) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(obj.ResourceVersion, 10, 64)
	if err != nil {
		t.Fatalf("resourceVersion %q is not numeric: %v", obj.ResourceVersion, err)
	}
	return n
}

func fakeUser(name, email string) *v1alpha1.User {
	return &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       v1alpha1.UserSpec{Email: email},
	}
}

func TestFake_SeedAssignsResourceVersionAndWritesItBack(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	alice := fakeUser("alice", "a@example.com")
	bob := fakeUser("bob", "b@example.com")
	MustSeed(f, alice, bob)

	if alice.ResourceVersion == "" || bob.ResourceVersion == "" {
		t.Fatalf("Seed left resourceVersion empty: alice %q, bob %q", alice.ResourceVersion, bob.ResourceVersion)
	}
	if alice.ResourceVersion == bob.ResourceVersion {
		t.Errorf("two seeded objects share resourceVersion %q", alice.ResourceVersion)
	}
	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion != alice.ResourceVersion {
		t.Errorf("stored resourceVersion = %q, want the value Seed wrote back %q", got.ResourceVersion, alice.ResourceVersion)
	}
}

func TestFake_SeedKeepsAnExplicitResourceVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	alice := fakeUser("alice", "a@example.com")
	alice.ResourceVersion = "7"
	MustSeed(f, alice)

	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion != "7" {
		t.Fatalf("stored resourceVersion = %q, want 7", got.ResourceVersion)
	}
	got.Spec.Email = "next@example.com"
	if err := Update(ctx, f, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if n := fakeRV(t, updated); n <= 7 {
		t.Errorf("resourceVersion after Update = %d, want a value above 7", n)
	}
}

func TestFake_CreateObjectAssignsResourceVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	alice := fakeUser("alice", "a@example.com")
	alice.ResourceVersion = "99"
	if err := Create(ctx, f, alice); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ResourceVersion == "" || got.ResourceVersion == "99" {
		t.Errorf("created resourceVersion = %q, want a fresh value", got.ResourceVersion)
	}
	if alice.ResourceVersion != got.ResourceVersion {
		t.Errorf("Create wrote %q back, stored value is %q", alice.ResourceVersion, got.ResourceVersion)
	}
}

func TestFake_UpdateAdvancesResourceVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, fakeUser("alice", "a@example.com"))

	before, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	startRV := before.ResourceVersion
	before.Spec.Email = "next@example.com"
	if err := Update(ctx, f, before); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if after.ResourceVersion == startRV {
		t.Errorf("Update left resourceVersion at %q", after.ResourceVersion)
	}
	if after.Spec.Email != "next@example.com" {
		t.Errorf("email = %q, want next@example.com", after.Spec.Email)
	}
}

func TestFake_UpdateWithStaleResourceVersionConflicts(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, fakeUser("alice", "a@example.com"))

	first, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	second, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}

	first.Spec.Email = "first@example.com"
	if err := Update(ctx, f, first); err != nil {
		t.Fatalf("first Update: %v", err)
	}

	second.Spec.Email = "second@example.com"
	if err := Update(ctx, f, second); !apierrors.IsConflict(err) {
		t.Fatalf("second Update = %v, want Conflict", err)
	}
	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after conflict: %v", err)
	}
	if got.Spec.Email != "first@example.com" {
		t.Errorf("stored email = %q, want the first write", got.Spec.Email)
	}
}

func TestFake_UpdateWithEmptyResourceVersionIsInvalid(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, fakeUser("alice", "a@example.com"))
	gvr, err := GVRFor[v1alpha1.User]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}

	// The typed Update stamps the live value, so the raw Backend call is the
	// only path that can send an empty resourceVersion.
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "User",
		"metadata":   map[string]any{"name": "alice", "namespace": "ns"},
		"spec":       map[string]any{"email": "raw@example.com"},
	}}
	if _, err := f.UpdateObject(ctx, gvr, "ns", u); !apierrors.IsInvalid(err) {
		t.Fatalf("UpdateObject without resourceVersion = %v, want Invalid", err)
	}
	if _, err := f.UpdateObjectStatus(ctx, gvr, "ns", u); !apierrors.IsInvalid(err) {
		t.Fatalf("UpdateObjectStatus without resourceVersion = %v, want Invalid", err)
	}
	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Email != "a@example.com" {
		t.Errorf("stored email = %q, want the seeded value", got.Spec.Email)
	}
}

func TestFake_RecreatedObjectGetsANewResourceVersion(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	alice := fakeUser("alice", "a@example.com")
	if err := Create(ctx, f, alice); err != nil {
		t.Fatalf("Create: %v", err)
	}
	old, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := Delete[v1alpha1.User](ctx, f, "alice", "ns"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := Create(ctx, f, fakeUser("alice", "b@example.com")); err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	fresh, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get re-created: %v", err)
	}
	if fresh.ResourceVersion == old.ResourceVersion {
		t.Fatalf("re-created object reuses resourceVersion %q", old.ResourceVersion)
	}
	err = DeleteIfUnchanged[v1alpha1.User](ctx, f, "alice", "ns", string(old.UID), old.ResourceVersion)
	if !apierrors.IsConflict(err) {
		t.Fatalf("DeleteIfUnchanged with the old resourceVersion = %v, want Conflict", err)
	}
}

func TestFake_PatchObjectMetaBumpsOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, fakeUser("alice", "a@example.com"))
	gvr, err := GVRFor[v1alpha1.User]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	start, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// The first patch adds a label, which changes the object and moves the value.
	if err := f.PatchObjectMeta(ctx, gvr, "ns", "alice", map[string]any{
		"labels": map[string]any{"team": "blue"},
	}); err != nil {
		t.Fatalf("first PatchObjectMeta: %v", err)
	}
	afterSet, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after set: %v", err)
	}
	if afterSet.ResourceVersion == start.ResourceVersion {
		t.Fatalf("a changing patch left resourceVersion at %q", start.ResourceVersion)
	}
	if err := f.PatchObjectMeta(ctx, gvr, "ns", "alice", map[string]any{
		"labels": map[string]any{"team": "blue"},
	}); err != nil {
		t.Fatalf("second PatchObjectMeta: %v", err)
	}
	afterNoop, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after no-op: %v", err)
	}
	// The second patch repeats the label, which changes nothing.
	if afterNoop.ResourceVersion != afterSet.ResourceVersion {
		t.Errorf("a no-op patch moved resourceVersion from %q to %q", afterSet.ResourceVersion, afterNoop.ResourceVersion)
	}
}

func TestFake_PatchObjectStatusBumpsOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, fakeUser("alice", "a@example.com"))
	gvr, err := GVRFor[v1alpha1.User]()
	if err != nil {
		t.Fatalf("GVRFor: %v", err)
	}
	status := map[string]any{"phase": "Ready"}
	if err := f.PatchObjectStatus(ctx, gvr, "ns", "alice", status); err != nil {
		t.Fatalf("first PatchObjectStatus: %v", err)
	}
	afterSet, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after set: %v", err)
	}
	if err := f.PatchObjectStatus(ctx, gvr, "ns", "alice", status); err != nil {
		t.Fatalf("second PatchObjectStatus: %v", err)
	}
	afterNoop, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("Get after no-op: %v", err)
	}
	if afterNoop.ResourceVersion != afterSet.ResourceVersion {
		t.Errorf("a no-op status patch moved resourceVersion from %q to %q", afterSet.ResourceVersion, afterNoop.ResourceVersion)
	}
}

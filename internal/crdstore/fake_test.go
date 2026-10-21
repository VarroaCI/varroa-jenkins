package crdstore

import (
	"context"
	"strconv"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

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

// userWithCredentials returns a User named alice with the stored password hash h1.
func userWithCredentials() *v1alpha1.User {
	u := fakeUser("alice", "a@example.test")
	u.Status.Credentials = &v1alpha1.UserCredentials{PasswordHash: "h1"}
	return u
}

// TestFake_UpdateObjectKeepsStoredStatus proves a main-resource update does
// not change .status on a kind with a status subresource, as the apiserver
// behaves.
func TestFake_UpdateObjectKeepsStoredStatus(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, userWithCredentials())

	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.Email = "b@example.test"
	got.Status.Credentials = &v1alpha1.UserCredentials{PasswordHash: "h2"}
	if err := Update(ctx, f, got); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.Spec.Email != "b@example.test" {
		t.Errorf("email = %q, want the updated value", after.Spec.Email)
	}
	if after.Status.Credentials == nil || after.Status.Credentials.PasswordHash != "h1" {
		t.Errorf("credentials = %+v, want the stored hash h1", after.Status.Credentials)
	}
}

// TestFake_UpdateObjectStatusOnlyChangeIsANoop proves a main-resource write
// that changes only .status changes nothing, so the resourceVersion holds.
func TestFake_UpdateObjectStatusOnlyChangeIsANoop(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, userWithCredentials())

	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	before := got.ResourceVersion
	got.Status.Credentials = &v1alpha1.UserCredentials{PasswordHash: "h2"}
	if err := Update(ctx, f, got); err != nil {
		t.Fatalf("update: %v", err)
	}

	after, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if after.ResourceVersion != before {
		t.Errorf("resourceVersion = %q, want unchanged %q", after.ResourceVersion, before)
	}
	if after.Status.Credentials == nil || after.Status.Credentials.PasswordHash != "h1" {
		t.Errorf("credentials = %+v, want the stored hash h1", after.Status.Credentials)
	}
}

// TestFake_CreateObjectDropsStatus proves a create ignores .status on a kind
// with a status subresource, as the apiserver does.
func TestFake_CreateObjectDropsStatus(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	if err := Create(ctx, f, userWithCredentials()); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Credentials != nil {
		t.Errorf("credentials = %+v, want none after a create", got.Status.Credentials)
	}
}

// TestFake_StatusWritesChangeOnlyStatus proves a status write leaves spec
// alone, and that an unstructured status write without a status key removes
// the stored status.
func TestFake_StatusWritesChangeOnlyStatus(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	MustSeed(f, userWithCredentials())

	got, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Spec.Email = "b@example.test"
	got.Status.Credentials = &v1alpha1.UserCredentials{PasswordHash: "h2"}
	if err := UpdateStatus(ctx, f, got); err != nil {
		t.Fatalf("update status: %v", err)
	}

	after, err := Get[v1alpha1.User](ctx, f, "alice", "ns")
	if err != nil {
		t.Fatalf("get after status write: %v", err)
	}
	if after.Spec.Email != "a@example.test" {
		t.Errorf("email = %q, want the spec unchanged", after.Spec.Email)
	}
	if after.Status.Credentials == nil || after.Status.Credentials.PasswordHash != "h2" {
		t.Errorf("credentials = %+v, want the written hash h2", after.Status.Credentials)
	}

	raw, err := f.GetObject(ctx, userGVR, "ns", "alice")
	if err != nil {
		t.Fatalf("raw get: %v", err)
	}
	delete(raw.Object, "status")
	if _, err := f.UpdateObjectStatus(ctx, userGVR, "ns", raw); err != nil {
		t.Fatalf("status write without a status key: %v", err)
	}
	cleared, err := f.GetObject(ctx, userGVR, "ns", "alice")
	if err != nil {
		t.Fatalf("raw get after clear: %v", err)
	}
	if _, ok := cleared.Object["status"]; ok {
		t.Errorf("status = %v, want the key removed", cleared.Object["status"])
	}
}

// TestFake_KindWithoutStatusSubresourceKeepsWholeObject proves a kind whose
// CRD has no status subresource keeps .status on a create, as the apiserver
// does for such a kind.
func TestFake_KindWithoutStatusSubresourceKeepsWholeObject(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"kind":       "JenkinsRole",
		"metadata":   map[string]any{"name": "r1"},
		"status":     map[string]any{"x": "y"},
	}}
	if _, err := f.CreateObject(ctx, jenkinsRoleGVR, "", obj); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := f.GetObject(ctx, jenkinsRoleGVR, "", "r1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Object["status"] == nil {
		t.Error("status was dropped on a kind without a status subresource")
	}
}

// TestFake_SeedObjectKeepsStatus proves SeedObject stores status as given and
// assigns a resourceVersion.
func TestFake_SeedObjectKeepsStatus(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: "cell-1"},
		},
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(team)
	if err != nil {
		t.Fatalf("to unstructured: %v", err)
	}
	if err := f.SeedObject(teamGVR, &unstructured.Unstructured{Object: u}); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	got, err := Get[v1alpha1.Team](ctx, f, "payments", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Placement == nil || got.Status.Placement.Cell != "cell-1" {
		t.Errorf("placement = %+v, want cell-1 kept", got.Status.Placement)
	}
	if got.ResourceVersion == "" {
		t.Error("resourceVersion is empty, want one assigned by SeedObject")
	}
}

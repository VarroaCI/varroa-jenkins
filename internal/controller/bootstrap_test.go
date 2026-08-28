package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func TestBootstrapLocalAdmin_Idempotent(t *testing.T) {
	client := newTestClient()
	logger := slog.Default()
	ctx := context.Background()

	// First bootstrap should succeed and generate credentials.
	err := BootstrapLocalAdmin(ctx, client, client.store, "default", logger)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// Verify the admin user was created in the store.
	_, err = crdstore.Get[v1alpha1.User](ctx, client.store, bootstrapAdminUser, "default")
	if err != nil {
		t.Fatalf("expected admin user in store: %v", err)
	}

	// Verify the group was created.
	groups, _ := crdstore.List[v1alpha1.Group](ctx, client.store, "", "")
	var foundGroup *v1alpha1.Group
	for _, g := range groups {
		if g.Name == bootstrapGroupName {
			foundGroup = g
			break
		}
	}
	if foundGroup == nil {
		t.Fatal("expected varroa-admins group to be created")
	}
	if len(foundGroup.Spec.Members) != 1 || foundGroup.Spec.Members[0] != bootstrapAdminUser {
		t.Errorf("expected group members=[%s], got %v", bootstrapAdminUser, foundGroup.Spec.Members)
	}

	// Verify the role binding was created.
	bindings, _ := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, client.store, "", "")
	var foundBinding *v1alpha1.VarroaRoleBinding
	for _, b := range bindings {
		if b.Name == bootstrapRoleBindingName {
			foundBinding = b
			break
		}
	}
	if foundBinding == nil {
		t.Fatal("expected role binding to be created")
	}
	if foundBinding.Spec.RoleRef != bootstrapAdminRole {
		t.Errorf("expected RoleRef=%s, got %s", bootstrapAdminRole, foundBinding.Spec.RoleRef)
	}
	if len(foundBinding.Spec.Subjects) != 2 ||
		foundBinding.Spec.Subjects[0].Kind != "User" || foundBinding.Spec.Subjects[0].Name != bootstrapAdminUser ||
		foundBinding.Spec.Subjects[1].Kind != "Group" || foundBinding.Spec.Subjects[1].Name != bootstrapGroupName {
		t.Errorf("expected subjects [User:%s Group:%s], got %v", bootstrapAdminUser, bootstrapGroupName, foundBinding.Spec.Subjects)
	}

	// Verify the secret was annotated as consumed.
	sec, _ := client.GetSecret(ctx, bootstrapSecretName, "default")
	if string(sec[bootstrapAnnotation]) != "true" {
		t.Error("expected bootstrap-consumed annotation to be true")
	}

	// Verify the password was logged (generated case).
	pw := string(sec["password"])
	if len(pw) != 24 {
		t.Errorf("expected 24-char password, got %d chars", len(pw))
	}

	// Second bootstrap should be a no-op.
	prevUsers, _ := crdstore.List[v1alpha1.User](ctx, client.store, "", "")
	prevGroups, _ := crdstore.List[v1alpha1.Group](ctx, client.store, "", "")
	prevBindings, _ := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, client.store, "", "")

	err = BootstrapLocalAdmin(ctx, client, client.store, "default", logger)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}

	usersAfter, _ := crdstore.List[v1alpha1.User](ctx, client.store, "", "")
	groupsAfter, _ := crdstore.List[v1alpha1.Group](ctx, client.store, "", "")
	bindingsAfter, _ := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, client.store, "", "")
	if len(usersAfter) != len(prevUsers) {
		t.Error("second bootstrap should not create additional users")
	}
	if len(groupsAfter) != len(prevGroups) {
		t.Error("second bootstrap should not create additional groups")
	}
	if len(bindingsAfter) != len(prevBindings) {
		t.Error("second bootstrap should not create additional role bindings")
	}
}

func TestBootstrapLocalAdmin_UsesExistingSecret(t *testing.T) {
	client := newTestClient()
	logger := slog.Default()
	ctx := context.Background()

	// Pre-populate a secret with explicit credentials.
	client.existingSecrets[bootstrapSecretName] = map[string][]byte{
		"username": []byte("myadmin"),
		"password": []byte("supersecretpassword!!"),
	}

	err := BootstrapLocalAdmin(ctx, client, client.store, "default", logger)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Should use the pre-set username, not "admin".
	_, err = crdstore.Get[v1alpha1.User](ctx, client.store, "myadmin", "default")
	if err != nil {
		t.Fatalf("expected user 'myadmin' to be created: %v", err)
	}

	// Group should reference myadmin.
	groups, _ := crdstore.List[v1alpha1.Group](ctx, client.store, "", "")
	for _, g := range groups {
		if g.Name == bootstrapGroupName {
			if g.Spec.Members[0] != "myadmin" {
				t.Errorf("expected group member 'myadmin', got %s", g.Spec.Members[0])
			}
			break
		}
	}

	// RoleBinding should reference myadmin.
	bindings, _ := crdstore.List[v1alpha1.VarroaRoleBinding](ctx, client.store, "", "")
	for _, b := range bindings {
		if b.Name == bootstrapRoleBindingName {
			if b.Spec.Subjects[0].Name != "myadmin" {
				t.Errorf("expected binding subject 'myadmin', got %s", b.Spec.Subjects[0].Name)
			}
			break
		}
	}
}

func TestBootstrapLocalAdmin_DoesNotLogPassword(t *testing.T) {
	client := newTestClient()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := context.Background()

	if err := BootstrapLocalAdmin(ctx, client, client.store, "default", logger); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sec, _ := client.GetSecret(ctx, bootstrapSecretName, "default")
	pw := string(sec["password"])
	if pw == "" {
		t.Fatal("expected a generated password in the secret")
	}
	if strings.Contains(buf.String(), pw) {
		t.Error("bootstrap log output must not contain the generated password")
	}
	if !strings.Contains(buf.String(), bootstrapSecretName) {
		t.Error("expected the log to reference the secret name")
	}
}

func TestUserReconciler_HashesPassword(t *testing.T) {
	client := newTestClient()
	ns := "default"
	ctx := context.Background()

	// Create a user with a raw password in spec and seed the store.
	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: ns},
		Spec: v1alpha1.UserSpec{
			Password: "rawpassword123",
			Email:    "alice@example.com",
		},
	}
	client.users["alice"] = user
	crdstore.MustSeed(client.store, user)

	ur := NewUserReconciler(client, client.store, ns)
	ur.Logger = slog.Default()

	err := ur.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Password should have been cleared from spec (via ClearUserPassword on client).
	if client.users["alice"].Spec.Password != "" {
		t.Error("expected spec.password to be cleared after reconciliation")
	}

	// A status patch should have been made with a password hash.
	userGVR, _ := crdstore.GVRFor[v1alpha1.User]()
	patches := client.store.StatusPatches(userGVR)
	if len(patches) != 1 {
		t.Fatalf("expected 1 status patch, got %d", len(patches))
	}
	p, ok := patches[0].Status.(*v1alpha1.UserStatus)
	if !ok {
		t.Fatalf("expected *v1alpha1.UserStatus, got %T", patches[0].Status)
	}
	if p.Credentials == nil || p.Credentials.PasswordHash == "" {
		t.Error("expected password hash in status patch")
	}
	if p.Credentials.LastChanged == nil {
		t.Error("expected LastChanged timestamp in status patch")
	}

	// Verify the hash is a valid bcrypt hash.
	if !strings.HasPrefix(p.Credentials.PasswordHash, "$2a$") {
		t.Error("expected bcrypt hash prefix $2a$")
	}
}

func TestUserReconciler_SkipsEmptyPassword(t *testing.T) {
	client := newTestClient()
	ns := "default"
	ctx := context.Background()

	// Create a user WITHOUT a password in spec.
	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: ns},
		Spec: v1alpha1.UserSpec{
			Email: "bob@example.com",
		},
	}
	client.users["bob"] = user
	crdstore.MustSeed(client.store, user)

	ur := NewUserReconciler(client, client.store, ns)
	ur.Logger = slog.Default()

	err := ur.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// No status patch should have been made.
	userGVR, _ := crdstore.GVRFor[v1alpha1.User]()
	patches := client.store.StatusPatches(userGVR)
	if len(patches) != 0 {
		t.Errorf("expected 0 status patches, got %d", len(patches))
	}
	if len(client.passwordClears) != 0 {
		t.Errorf("expected 0 password clears, got %d", len(client.passwordClears))
	}
}

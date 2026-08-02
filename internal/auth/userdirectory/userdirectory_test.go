package userdirectory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// stubRecordLoginClient mimics the real ResourceClient's status-subresource
// semantics: ApplyUserCRD (a write to the main resource) NEVER persists the
// status stanza, and PatchUserStatus is the only path that writes status. This
// is essential — a stub that let ApplyUserCRD persist status would mask the
// real bug where status fields silently vanish.
type stubRecordLoginClient struct {
	users         map[string]*v1alpha1.User
	statusPatches int
}

func newStubRecordLoginClient() *stubRecordLoginClient {
	return &stubRecordLoginClient{users: make(map[string]*v1alpha1.User)}
}

func (c *stubRecordLoginClient) GetUserCRD(_ context.Context, name, _ string) (*v1alpha1.User, error) {
	u, ok := c.users[name]
	if !ok {
		return nil, nil
	}
	return u.DeepCopy(), nil
}

func (c *stubRecordLoginClient) ApplyUserCRD(_ context.Context, user *v1alpha1.User) error {
	cp := user.DeepCopy()
	// Main-resource writes do not touch status (status subresource). Preserve
	// any previously persisted status; discard status carried in the request.
	if existing, ok := c.users[user.Name]; ok {
		cp.Status = existing.Status
	} else {
		cp.Status = v1alpha1.UserStatus{}
	}
	c.users[user.Name] = cp
	return nil
}

func (c *stubRecordLoginClient) PatchUserStatus(_ context.Context, name, _ string, status *v1alpha1.UserStatus) error {
	u, ok := c.users[name]
	if !ok {
		return fmt.Errorf("user %s not found", name)
	}
	c.statusPatches++
	if status.LastLogin != nil {
		u.Status.LastLogin = status.LastLogin
	}
	if status.ObservedGroups != nil {
		u.Status.ObservedGroups = status.ObservedGroups
	}
	return nil
}

func oidcUserName(subject string) string {
	h := sha256.Sum256([]byte(subject))
	return "oidc-" + hex.EncodeToString(h[:])[:32]
}

func TestRecordLogin_FirstLogin(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "oidc:abc123"
	userName := oidcUserName(subject)

	err := RecordLogin(context.Background(), client, userName,
		"alice@example.com", "Alice Cooper",
		[]string{"developers", "admins"}, subject, "alice", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(client.users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(client.users))
	}
	user := client.users[userName]
	if user == nil {
		t.Fatal("expected user to be created under the hashed name")
	}

	if user.Namespace != "test-ns" {
		t.Errorf("expected namespace 'test-ns', got %q", user.Namespace)
	}
	if user.Spec.Email != "alice@example.com" {
		t.Errorf("expected email 'alice@example.com', got %q", user.Spec.Email)
	}
	if user.Spec.DisplayName != "Alice Cooper" {
		t.Errorf("expected displayName 'Alice Cooper', got %q", user.Spec.DisplayName)
	}
	// Status must be persisted via PatchUserStatus, NOT via ApplyUserCRD.
	if client.statusPatches != 1 {
		t.Errorf("expected exactly 1 status patch, got %d", client.statusPatches)
	}
	if user.Status.LastLogin == nil {
		t.Error("expected lastLogin to be persisted via status subresource")
	}
	if len(user.Status.ObservedGroups) != 2 {
		t.Errorf("expected 2 observed groups persisted, got %d", len(user.Status.ObservedGroups))
	}

	if lb := user.Labels[v1alpha1.LabelManagedBy]; lb != v1alpha1.ManagedByIDP {
		t.Errorf("expected managed-by 'idp', got %q", lb)
	}
	if sub := user.Annotations[v1alpha1.AnnotationOIDCSubject]; sub != "oidc:abc123" {
		t.Errorf("expected oidc-subject annotation 'oidc:abc123', got %q", sub)
	}
	if pu := user.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; pu != "alice" {
		t.Errorf("expected preferred-username annotation 'alice', got %q", pu)
	}
}

func TestRecordLogin_SubsequentRefresh(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "fixed-subject-for-test-determinism"
	userName := oidcUserName(subject)

	if err := RecordLogin(context.Background(), client, userName,
		"initial@example.com", "Initial Name",
		[]string{"alpha"}, subject, "user1", "test-ns"); err != nil {
		t.Fatalf("unexpected error on first login: %v", err)
	}

	if err := RecordLogin(context.Background(), client, userName,
		"updated@example.com", "Updated Name",
		[]string{"beta", "gamma"}, subject, "user1", "test-ns"); err != nil {
		t.Fatalf("unexpected error on second login: %v", err)
	}
	if len(client.users) != 1 {
		t.Fatalf("expected still 1 user, got %d", len(client.users))
	}

	u := client.users[userName]
	if u.Spec.Email != "updated@example.com" {
		t.Errorf("expected email refresh, got %q", u.Spec.Email)
	}
	if u.Spec.DisplayName != "Updated Name" {
		t.Errorf("expected displayName refresh, got %q", u.Spec.DisplayName)
	}
	// Observed groups must be refreshed on subsequent login (via status patch).
	if len(u.Status.ObservedGroups) != 2 {
		t.Errorf("expected 2 observed groups after refresh, got %d", len(u.Status.ObservedGroups))
	}
	if client.statusPatches != 2 {
		t.Errorf("expected a status patch on each login (2), got %d", client.statusPatches)
	}
}

func TestRecordLogin_NeverClobberLocal(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "some-oidc-subject"
	userName := oidcUserName(subject)

	client.users[userName] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{
			Name:      userName,
			Namespace: "test-ns",
			Labels:    map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByLocal},
		},
		Spec: v1alpha1.UserSpec{Email: "local@example.com", DisplayName: "Local User"},
	}

	if err := RecordLogin(context.Background(), client, userName,
		"oidc@example.com", "OIDC Overshadow",
		[]string{"hackers"}, subject, "overshadow", "test-ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	u := client.users[userName]
	if u.Spec.Email != "local@example.com" {
		t.Errorf("local email should not change, got %q", u.Spec.Email)
	}
	if u.Spec.DisplayName != "Local User" {
		t.Errorf("local displayName should not change, got %q", u.Spec.DisplayName)
	}
	if u.Status.LastLogin != nil {
		t.Error("local lastLogin should not be touched by OIDC record")
	}
	if client.statusPatches != 0 {
		t.Errorf("expected no status patch for a local user, got %d", client.statusPatches)
	}
}

func TestRecordLogin_EmptyUserName(t *testing.T) {
	client := newStubRecordLoginClient()
	if err := RecordLogin(context.Background(), client, "",
		"test@example.com", "Test", nil, "subj", "pref", "test-ns"); err == nil {
		t.Error("expected error for empty userName")
	}
}

func TestRecordLogin_EmptyNamespace(t *testing.T) {
	client := newStubRecordLoginClient()
	if err := RecordLogin(context.Background(), client, "oidc-testname",
		"test@example.com", "Test", nil, "subj", "pref", ""); err == nil {
		t.Error("expected error for empty namespace")
	}
}

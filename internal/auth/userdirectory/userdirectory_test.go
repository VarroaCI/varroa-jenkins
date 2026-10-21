package userdirectory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// stubRecordLoginClient mimics the real ResourceClient's status-subresource
// semantics: CreateUserCRD and UpdateUserCRD (writes to the main resource) NEVER
// persist the status stanza, and PatchUserStatus is the only path that writes
// status. This is essential — a stub that let the main-resource writes persist
// status would mask the real bug where status fields silently vanish.
//
// conflictsLeft makes the next UpdateUserCRD calls answer Conflict. onConflict
// runs at each conflict, so a test can model a concurrent write.
type stubRecordLoginClient struct {
	users         map[string]*v1alpha1.User
	statusPatches int
	conflictsLeft int
	onConflict    func()
	// beforeUpdate runs once at the start of the next UpdateUserCRD call.
	beforeUpdate func()
}

var stubUserGR = schema.GroupResource{Group: "varroa.dev", Resource: "users"}

func newStubRecordLoginClient() *stubRecordLoginClient {
	return &stubRecordLoginClient{users: make(map[string]*v1alpha1.User)}
}

func (c *stubRecordLoginClient) GetUserCRD(_ context.Context, name, _ string) (*v1alpha1.User, error) {
	u, ok := c.users[name]
	if !ok {
		return nil, apierrors.NewNotFound(stubUserGR, name)
	}
	return u.DeepCopy(), nil
}

func (c *stubRecordLoginClient) CreateUserCRD(_ context.Context, user *v1alpha1.User) error {
	if _, ok := c.users[user.Name]; ok {
		return apierrors.NewAlreadyExists(stubUserGR, user.Name)
	}
	cp := user.DeepCopy()
	cp.Status = v1alpha1.UserStatus{}
	c.users[user.Name] = cp
	return nil
}

func (c *stubRecordLoginClient) UpdateUserCRD(_ context.Context, user *v1alpha1.User) error {
	if hook := c.beforeUpdate; hook != nil {
		c.beforeUpdate = nil
		hook()
	}
	if c.conflictsLeft > 0 {
		c.conflictsLeft--
		if c.onConflict != nil {
			c.onConflict()
		}
		return apierrors.NewConflict(stubUserGR, user.Name, errors.New("concurrent write"))
	}
	existing, ok := c.users[user.Name]
	if !ok {
		return apierrors.NewNotFound(stubUserGR, user.Name)
	}
	cp := user.DeepCopy()
	// Main-resource writes do not touch status (status subresource). Preserve
	// any previously persisted status; discard status carried in the request.
	cp.Status = existing.Status
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

// TestRecordLogin_RetriesOnConflict pins that a write made between the read
// and the update is kept: the retry reads the User again and merges into it.
func TestRecordLogin_RetriesOnConflict(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "fixed-subject-for-conflict"
	userName := oidcUserName(subject)

	if err := RecordLogin(context.Background(), client, userName,
		"old@example.com", "Old Name",
		nil, subject, "alice", nil, "test-ns"); err != nil {
		t.Fatalf("unexpected error on first login: %v", err)
	}

	client.conflictsLeft = 1
	client.onConflict = func() {
		client.users[userName].Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers] = `["bob"]`
	}
	if err := RecordLogin(context.Background(), client, userName,
		"new@example.com", "New Name",
		nil, subject, "alice", nil, "test-ns"); err != nil {
		t.Fatalf("unexpected error on second login: %v", err)
	}

	u := client.users[userName]
	if u.Spec.Email != "new@example.com" {
		t.Errorf("expected email refresh, got %q", u.Spec.Email)
	}
	if got := FormerIdentifiers(u); !slices.Contains(got, "bob") {
		t.Errorf("expected former identifiers to keep the concurrent write, got %v", got)
	}
	if client.statusPatches != 2 {
		t.Errorf("expected one status patch per login, got %d", client.statusPatches)
	}
}

// TestRecordLogin_UserDeletedBeforeWriteIsCreatedAgain pins that a User
// deleted between the read and the write is created again by the retry.
func TestRecordLogin_UserDeletedBeforeWriteIsCreatedAgain(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "fixed-subject-for-deletion"
	userName := oidcUserName(subject)

	if err := RecordLogin(context.Background(), client, userName,
		"old@example.com", "Old Name",
		nil, subject, "alice", nil, "test-ns"); err != nil {
		t.Fatalf("unexpected error on first login: %v", err)
	}

	// The User disappears after the read, so the update answers NotFound.
	client.beforeUpdate = func() {
		delete(client.users, userName)
	}
	if err := RecordLogin(context.Background(), client, userName,
		"new@example.com", "New Name",
		nil, subject, "alice", nil, "test-ns"); err != nil {
		t.Fatalf("unexpected error on second login: %v", err)
	}
	u := client.users[userName]
	if u == nil {
		t.Fatal("expected the User to be created again")
	}
	if u.Spec.Email != "new@example.com" {
		t.Errorf("expected email refresh, got %q", u.Spec.Email)
	}
}

func TestRecordLogin_FirstLogin(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "oidc:abc123"
	userName := oidcUserName(subject)

	err := RecordLogin(context.Background(), client, userName,
		"alice@example.com", "Alice Cooper",
		[]string{"developers", "admins"}, subject, "alice", nil, "test-ns")
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
		[]string{"alpha"}, subject, "user1", nil, "test-ns"); err != nil {
		t.Fatalf("unexpected error on first login: %v", err)
	}

	if err := RecordLogin(context.Background(), client, userName,
		"updated@example.com", "Updated Name",
		[]string{"beta", "gamma"}, subject, "user1", nil, "test-ns"); err != nil {
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
		[]string{"hackers"}, subject, "overshadow", nil, "test-ns"); err != nil {
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
		"test@example.com", "Test", nil, "subj", "pref", nil, "test-ns"); err == nil {
		t.Error("expected error for empty userName")
	}
}

func TestRecordLogin_EmptyNamespace(t *testing.T) {
	client := newStubRecordLoginClient()
	if err := RecordLogin(context.Background(), client, "oidc-testname",
		"test@example.com", "Test", nil, "subj", "pref", nil, ""); err == nil {
		t.Error("expected error for empty namespace")
	}
}

func recordLoginTest(t *testing.T, client *stubRecordLoginClient, subject, email, preferred string) {
	t.Helper()
	if err := RecordLogin(context.Background(), client, oidcUserName(subject),
		email, "Dana", nil, subject, preferred, nil, "test-ns"); err != nil {
		t.Fatalf("RecordLogin(%q, %q): %v", email, preferred, err)
	}
}

func TestRecordLogin_DropsPreferredUsernameWhenClaimAbsent(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "subject-drop"
	recordLoginTest(t, client, subject, "dana@example.com", "dana")
	recordLoginTest(t, client, subject, "dana@example.com", "")

	u := client.users[oidcUserName(subject)]
	if got := u.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; got != "" {
		t.Errorf("current preferred username = %q, want none", got)
	}
	if got := FormerIdentifiers(u); len(got) != 1 || got[0] != "dana" {
		t.Errorf("FormerIdentifiers = %v, want [dana]", got)
	}
}

func TestRecordLogin_RenamedPreferredUsernameRecordsFormer(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "subject-rename"
	recordLoginTest(t, client, subject, "dana@example.com", "dana")
	recordLoginTest(t, client, subject, "dana@example.com", "dana2")

	u := client.users[oidcUserName(subject)]
	if got := u.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; got != "dana2" {
		t.Errorf("current preferred username = %q, want dana2", got)
	}
	if got := FormerIdentifiers(u); len(got) != 1 || got[0] != "dana" {
		t.Errorf("FormerIdentifiers = %v, want [dana]", got)
	}
}

func TestRecordLogin_ChangedEmailRecordsFormer(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "subject-email"
	recordLoginTest(t, client, subject, "a@x", "")
	recordLoginTest(t, client, subject, "b@x", "")

	u := client.users[oidcUserName(subject)]
	if u.Spec.Email != "b@x" {
		t.Errorf("Spec.Email = %q, want b@x", u.Spec.Email)
	}
	if got := FormerIdentifiers(u); len(got) != 1 || got[0] != "a@x" {
		t.Errorf("FormerIdentifiers = %v, want [a@x]", got)
	}
}

func TestRecordLogin_UnchangedClaimsWriteNoFormer(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "subject-unchanged"
	recordLoginTest(t, client, subject, "dana@example.com", "dana")
	recordLoginTest(t, client, subject, "dana@example.com", "dana")

	u := client.users[oidcUserName(subject)]
	if _, ok := u.Annotations[v1alpha1.AnnotationOIDCFormerIdentifiers]; ok {
		t.Errorf("former annotation written for unchanged claims: %v", u.Annotations)
	}
}

func TestRecordLogin_FormerIdentifiersAccumulate(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "subject-accumulate"
	recordLoginTest(t, client, subject, "dana@example.com", "dana")
	recordLoginTest(t, client, subject, "dana@example.com", "")
	if got := FormerIdentifiers(client.users[oidcUserName(subject)]); len(got) != 1 || got[0] != "dana" {
		t.Fatalf("after drop FormerIdentifiers = %v, want [dana]", got)
	}
	recordLoginTest(t, client, subject, "dana@example.com", "dana")
	recordLoginTest(t, client, subject, "dana@example.com", "dana3")

	u := client.users[oidcUserName(subject)]
	if got := u.Annotations[v1alpha1.AnnotationOIDCPreferredUsername]; got != "dana3" {
		t.Errorf("current preferred username = %q, want dana3", got)
	}
	if got := FormerIdentifiers(u); len(got) != 1 || got[0] != "dana" {
		t.Errorf("FormerIdentifiers = %v, want [dana] with no duplicate", got)
	}
}

func TestFormerIdentifiers_MalformedReturnsNil(t *testing.T) {
	u := &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{v1alpha1.AnnotationOIDCFormerIdentifiers: "not-json"},
	}}
	if got := FormerIdentifiers(u); got != nil {
		t.Errorf("FormerIdentifiers = %v, want nil", got)
	}
	if got := FormerIdentifiers(nil); got != nil {
		t.Errorf("FormerIdentifiers(nil) = %v, want nil", got)
	}
}

func TestRecordLogin_StoresExtraUserClaims(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "oidc:claims1"
	userName := oidcUserName(subject)

	if err := RecordLogin(context.Background(), client, userName,
		"alice@example.com", "Alice", nil, subject, "alice",
		map[string]string{"employee_id": "E123"}, "test-ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	u := client.users[userName]
	if got := u.Annotations[v1alpha1.AnnotationOIDCUserClaims]; got != `{"employee_id":"E123"}` {
		t.Errorf("user claims annotation = %q, want employee_id E123", got)
	}
}

func TestRecordLogin_DropsUserClaimsWhenAbsent(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "oidc:claims2"
	userName := oidcUserName(subject)

	if err := RecordLogin(context.Background(), client, userName,
		"alice@example.com", "Alice", nil, subject, "alice",
		map[string]string{"employee_id": "E123"}, "test-ns"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if err := RecordLogin(context.Background(), client, userName,
		"alice@example.com", "Alice", nil, subject, "alice", nil, "test-ns"); err != nil {
		t.Fatalf("second login: %v", err)
	}

	u := client.users[userName]
	if _, ok := u.Annotations[v1alpha1.AnnotationOIDCUserClaims]; ok {
		t.Errorf("expected user claims annotation removed, got %q", u.Annotations[v1alpha1.AnnotationOIDCUserClaims])
	}
	if former := FormerIdentifiers(u); !slices.Contains(former, "E123") {
		t.Errorf("former identifiers = %v, want E123", former)
	}
}

func TestRecordLogin_ChangedUserClaimRecordsFormer(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "oidc:claims3"
	userName := oidcUserName(subject)

	if err := RecordLogin(context.Background(), client, userName,
		"alice@example.com", "Alice", nil, subject, "alice",
		map[string]string{"employee_id": "E123"}, "test-ns"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if err := RecordLogin(context.Background(), client, userName,
		"alice@example.com", "Alice", nil, subject, "alice",
		map[string]string{"employee_id": "E456"}, "test-ns"); err != nil {
		t.Fatalf("second login: %v", err)
	}

	u := client.users[userName]
	if got := UserClaims(u)["employee_id"]; got != "E456" {
		t.Errorf("stored employee_id = %q, want E456", got)
	}
	if former := FormerIdentifiers(u); !slices.Contains(former, "E123") {
		t.Errorf("former identifiers = %v, want E123", former)
	}
}

func TestRecordLogin_UnchangedUserClaimsWriteNoFormer(t *testing.T) {
	client := newStubRecordLoginClient()
	subject := "oidc:claims4"
	userName := oidcUserName(subject)
	claims := map[string]string{"employee_id": "E123"}

	for i := 0; i < 2; i++ {
		if err := RecordLogin(context.Background(), client, userName,
			"alice@example.com", "Alice", nil, subject, "alice", claims, "test-ns"); err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
	}

	u := client.users[userName]
	if former := FormerIdentifiers(u); former != nil {
		t.Errorf("expected no former identifiers, got %v", former)
	}
}

func TestUserClaims_MalformedReturnsNil(t *testing.T) {
	if got := UserClaims(nil); got != nil {
		t.Errorf("nil user: got %v", got)
	}
	u := &v1alpha1.User{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{v1alpha1.AnnotationOIDCUserClaims: `["not","an","object"]`},
	}}
	if got := UserClaims(u); got != nil {
		t.Errorf("malformed annotation: got %v", got)
	}
}

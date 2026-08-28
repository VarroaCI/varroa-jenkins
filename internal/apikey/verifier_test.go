package apikey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

var secretsGR = schema.GroupResource{Resource: "secrets"}

// mockStore implements keyStore for testing. It mirrors the real client:
// CreateSecretExclusive surfaces AlreadyExists, PatchSecretData merges only the
// given keys (preserving labels), and ListSecrets honors the label selector.
type mockStore struct {
	secrets map[string]map[string][]byte
	labels  map[string]map[string]string
	groups  []*v1alpha1.Group
	users   map[string]*v1alpha1.User
}

func newMockStore() *mockStore {
	return &mockStore{
		secrets: make(map[string]map[string][]byte),
		labels:  make(map[string]map[string]string),
		users:   make(map[string]*v1alpha1.User),
	}
}

func (m *mockStore) GetSecret(_ context.Context, name, _ string) (map[string][]byte, error) {
	data, ok := m.secrets[name]
	if !ok {
		return nil, apierrors.NewNotFound(secretsGR, name)
	}
	return data, nil
}

func (m *mockStore) CreateSecretExclusive(_ context.Context, name, _ string, labels map[string]string, data map[string][]byte) error {
	if _, exists := m.secrets[name]; exists {
		return apierrors.NewAlreadyExists(secretsGR, name)
	}
	m.secrets[name] = data
	m.labels[name] = labels
	return nil
}

func (m *mockStore) PatchSecretData(_ context.Context, name, _ string, data map[string][]byte) error {
	existing, ok := m.secrets[name]
	if !ok {
		return apierrors.NewNotFound(secretsGR, name)
	}
	for k, v := range data {
		existing[k] = v
	}
	// Labels intentionally untouched — that is the behavior under test.
	return nil
}

func (m *mockStore) DeleteSecret(_ context.Context, name, _ string) error {
	delete(m.secrets, name)
	delete(m.labels, name)
	return nil
}

func (m *mockStore) ListSecrets(_ context.Context, _, labelSelector string) ([]map[string][]byte, error) {
	wantKey, wantVal, hasFilter := strings.Cut(labelSelector, "=")
	result := make([]map[string][]byte, 0, len(m.secrets))
	for name, data := range m.secrets {
		if hasFilter && m.labels[name][wantKey] != wantVal {
			continue
		}
		out := make(map[string][]byte)
		for k, v := range data {
			out[k] = v
		}
		out["_name"] = []byte(name)
		result = append(result, out)
	}
	return result, nil
}

func (m *mockStore) ListGroupCRDs(_ context.Context) ([]*v1alpha1.Group, error) {
	return m.groups, nil
}

func (m *mockStore) GetUserCRD(_ context.Context, name, _ string) (*v1alpha1.User, error) {
	u, ok := m.users[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "varroa.dev", Resource: "users"}, name)
	}
	return u, nil
}

func TestVerifierGenerateAndVerify(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "testuser",
		Email:             "test@example.com",
		Name:              "Test User",
		PreferredUsername: "testuser",
	}

	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	got, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if got.Subject != "testuser" {
		t.Errorf("Subject = %q, want %q", got.Subject, "testuser")
	}
	if got.Email != "test@example.com" {
		t.Errorf("Email = %q, want %q", got.Email, "test@example.com")
	}
}

func TestVerifyExpiredKey(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "testuser",
		Email:             "t@t.com",
		Name:              "T",
		PreferredUsername: "testuser",
	}

	_, _, token, err := v.Generate(context.Background(), claims, -1*time.Hour, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// Modify the expiry to be in the past
	prefix, _, _ := Parse(token)
	data, _ := store.GetSecret(context.Background(), "apikey-"+prefix, "default")
	data["expires"] = []byte(time.Now().Add(-1 * time.Hour).Format(time.RFC3339))

	_, err = v.Verify(context.Background(), token)
	if err == nil {
		t.Error("expected error for expired key")
	}
}

func TestVerifyRevokedKey(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "testuser",
		Email:             "t@t.com",
		Name:              "T",
		PreferredUsername: "testuser",
	}

	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	prefix, _, _ := Parse(token)

	// Cache it first
	v.Verify(context.Background(), token)

	// Revoke
	v.Revoke(context.Background(), prefix)

	// Should fail after revoke (cache evicted + secret deleted)
	_, err = v.Verify(context.Background(), token)
	if err == nil {
		t.Error("expected error for revoked key")
	}
}

func TestCacheHit(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "testuser",
		Email:             "t@t.com",
		Name:              "T",
		PreferredUsername: "testuser",
	}

	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// First call populates cache
	got1, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("first Verify() error: %v", err)
	}

	// Delete the underlying secret to prove cache is used
	prefix, _, _ := Parse(token)
	store.DeleteSecret(context.Background(), "apikey-"+prefix, "default")

	// Second call should still succeed from cache
	got2, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("cached Verify() error: %v", err)
	}
	if got2.Subject != got1.Subject {
		t.Error("cached claims differ")
	}
}

func TestCacheHitRequiresCorrectSecret(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "testuser",
		Email:             "t@t.com",
		Name:              "T",
		PreferredUsername: "testuser",
	}

	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// Warm the cache with the legitimate token.
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("first Verify() error: %v", err)
	}

	// A well-formed token with the same prefix but a wrong secret must be
	// rejected — the cache entry for the prefix must not satisfy it.
	prefix, secret, _ := Parse(token)
	wrongByte := byte('A')
	if secret[0] == wrongByte {
		wrongByte = 'B'
	}
	forged := "vk_" + prefix + "." + string(wrongByte) + secret[1:]
	if _, err := v.Verify(context.Background(), forged); err == nil {
		t.Error("expected error for wrong secret with cached prefix")
	}
}

func TestCacheEviction(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "testuser",
		Email:             "t@t.com",
		Name:              "T",
		PreferredUsername: "testuser",
	}

	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	prefix, secret, _ := Parse(token)
	v.Verify(context.Background(), token)
	v.cache.evict(prefix)

	// After eviction, should fail (secret is still there but cache miss with
	// verify should succeed — unless we also delete the secret).
	// Just verify the cache entry is gone.
	if cached := v.cache.get(prefix, secret); cached != nil {
		t.Error("cache entry still present after evict")
	}
}

func TestPrefixCollisionRetry(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "user",
		Email:             "u@u.com",
		Name:              "U",
		PreferredUsername: "user",
	}

	_, _, token1, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("first Generate() error: %v", err)
	}

	// Second generate should succeed with a different prefix
	_, _, token2, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("second Generate() error: %v", err)
	}

	prefix1, _, _ := Parse(token1)
	prefix2, _, _ := Parse(token2)
	if prefix1 == prefix2 {
		t.Error("second generate returned same prefix as first (extremely unlikely)")
	}
}

func TestRotate(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "user",
		Email:             "u@u.com",
		Name:              "U",
		PreferredUsername: "user",
	}

	_, _, oldToken, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	oldPrefix, _, _ := Parse(oldToken)
	v.Verify(context.Background(), oldToken) // populate cache

	_, _, newToken, err := v.Rotate(context.Background(), claims, oldPrefix, 0, "", "")
	if err != nil {
		t.Fatalf("Rotate() error: %v", err)
	}

	// Old key should be revoked
	_, err = v.Verify(context.Background(), oldToken)
	if err == nil {
		t.Error("expected error for rotated old key")
	}

	// New key should verify
	got, err := v.Verify(context.Background(), newToken)
	if err != nil {
		t.Fatalf("new token Verify() error: %v", err)
	}
	if got.Subject != "user" {
		t.Errorf("Subject = %q, want %q", got.Subject, "user")
	}
}

// failDeleteStore wraps mockStore but always fails DeleteSecret, to exercise
// the rotate partial-failure branch (new key created, old key undeletable).
type failDeleteStore struct {
	*mockStore
}

func (f *failDeleteStore) DeleteSecret(context.Context, string, string) error {
	return errors.New("simulated delete failure")
}

func TestRotatePartialFailure(t *testing.T) {
	store := &failDeleteStore{mockStore: newMockStore()}
	v := NewVerifier(store, "default")

	claims := &auth.Claims{Subject: "user", PreferredUsername: "user"}
	_, _, oldToken, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	oldPrefix, _, _ := Parse(oldToken)

	_, _, newToken, err := v.Rotate(context.Background(), claims, oldPrefix, 0, "", "")
	if err == nil {
		t.Fatal("expected RotateError when old-key delete fails, got nil")
	}
	var rotErr *RotateError
	if !errors.As(err, &rotErr) {
		t.Fatalf("expected *RotateError, got %T: %v", err, err)
	}
	if rotErr.NewToken != newToken || newToken == "" {
		t.Errorf("RotateError.NewToken = %q, want returned newToken %q", rotErr.NewToken, newToken)
	}
	// The new key must still be usable despite the old-key delete failure.
	if _, verr := v.Verify(context.Background(), newToken); verr != nil {
		t.Errorf("new token should verify after partial-failure rotate: %v", verr)
	}
}

func TestListByUser(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{
		Subject:           "user1",
		Email:             "u1@t.com",
		Name:              "U1",
		PreferredUsername: "user1",
	}

	_, _, _, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	keys, err := v.ListByUser(context.Background(), "user1")
	if err != nil {
		t.Fatalf("ListByUser() error: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("got %d keys, want 1", len(keys))
	}
}

// TestFlushPreservesLabels guards against the lastUsed flush wiping the key's
// labels, which would make the key invisible to list/ownership checks.
func TestFlushPreservesLabels(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{Subject: "user1", PreferredUsername: "user1"}
	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	prefix, _, _ := Parse(token)

	// Simulate a throttled lastUsed flush.
	when := time.Now().Add(time.Minute)
	v.flushLastUsed(prefix, when)

	// The key must still be discoverable by its owner (labels intact).
	keys, err := v.ListByUser(context.Background(), "user1")
	if err != nil {
		t.Fatalf("ListByUser() error: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("key vanished after flush: got %d keys, want 1", len(keys))
	}
	if keys[0].LastUsed == "" {
		t.Error("lastUsed not updated by flush")
	}
}

// TestVerifyInvalidExpiry verifies the fail-closed behavior: an unparseable
// expires field rejects the key rather than treating it as never-expiring.
func TestVerifyInvalidExpiry(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{Subject: "user1", PreferredUsername: "user1"}
	_, _, token, err := v.Generate(context.Background(), claims, time.Hour, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	prefix, _, _ := Parse(token)
	data, _ := store.GetSecret(context.Background(), "apikey-"+prefix, "default")
	data["expires"] = []byte("not-a-timestamp")

	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Error("expected error for key with unparseable expiry")
	}
}

// TestVerifyFreshSkipsCache verifies that VerifyFresh ignores a live cache
// entry: if the secret is deleted, VerifyFresh fails while Verify may still
// serve from cache.
func TestVerifyFreshSkipsCache(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{Subject: "user1", PreferredUsername: "user1"}
	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// Verify populates the cache.
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("initial Verify() error: %v", err)
	}

	// Delete the underlying secret.
	prefix, _, _ := Parse(token)
	store.DeleteSecret(context.Background(), "apikey-"+prefix, "default")

	// VerifyFresh should fail (secret is gone).
	if _, err := v.VerifyFresh(context.Background(), token); err == nil {
		t.Error("VerifyFresh succeeded after secret deletion, expected error")
	}
}

// TestVerifyFreshDoesNotPopulateCache verifies that a VerifyFresh success
// does not pre-populate the cache for a subsequent Verify call.
func TestVerifyFreshDoesNotPopulateCache(t *testing.T) {
	store := newMockStore()
	v := NewVerifier(store, "default")

	claims := &auth.Claims{Subject: "user1", PreferredUsername: "user1"}
	_, _, token, err := v.Generate(context.Background(), claims, 0, "", "")
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// VerifyFresh succeeds but should NOT pre-populate the cache.
	if _, err := v.VerifyFresh(context.Background(), token); err != nil {
		t.Fatalf("VerifyFresh() error: %v", err)
	}

	// Delete the secret.
	prefix, _, _ := Parse(token)
	store.DeleteSecret(context.Background(), "apikey-"+prefix, "default")

	// Verify should now fail (no cache entry, secret gone).
	if _, err := v.Verify(context.Background(), token); err == nil {
		t.Error("Verify succeeded after secret deletion and VerifyFresh, expected cache miss")
	}
}

// TestResolveGroups_LocalUserViaUserRef tests the ladder step 1 + 4a: local
// user via userRef → Group.Spec.Members scan.
func TestResolveGroups_LocalUserViaUserRef(t *testing.T) {
	store := newMockStore()
	store.users["jdoe"] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "jdoe", Labels: map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByLocal}},
	}
	store.groups = []*v1alpha1.Group{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.GroupSpec{Members: []string{"jdoe"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}, Spec: v1alpha1.GroupSpec{Members: []string{"other"}}},
	}

	data := map[string][]byte{
		"preferredUsername": []byte("jdoe"),
		"subject":           []byte("jdoe"),
		"userRef":           []byte("jdoe"),
	}
	groups, err := resolveGroups(context.Background(), store, "ns", data)
	if err != nil {
		t.Fatalf("resolveGroups() error: %v", err)
	}
	if len(groups) != 1 || groups[0] != "team-a" {
		t.Errorf("groups = %v, want [team-a]", groups)
	}
}

// TestResolveGroups_IdPUserViaUserRef tests ladder step 1 + 4b: idp user via
// userRef → ObservedGroups verbatim, Group CRDs NOT consulted.
func TestResolveGroups_IdPUserViaUserRef(t *testing.T) {
	store := newMockStore()
	store.users["oidc-abc123"] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "oidc-abc123", Labels: map[string]string{v1alpha1.LabelManagedBy: "idp"}},
		Status:     v1alpha1.UserStatus{ObservedGroups: []string{"eng", "sre"}},
	}
	// If ListGroupCRDs is called, the test should fail.
	store.groups = []*v1alpha1.Group{
		{ObjectMeta: metav1.ObjectMeta{Name: "should-not-be-scanned"}},
	}

	data := map[string][]byte{
		"preferredUsername": []byte("oidc-user"),
		"subject":           []byte("subject-123"),
		"userRef":           []byte("oidc-abc123"),
	}
	groups, err := resolveGroups(context.Background(), store, "ns", data)
	if err != nil {
		t.Fatalf("resolveGroups() error: %v", err)
	}
	if len(groups) != 2 || groups[0] != "eng" || groups[1] != "sre" {
		t.Errorf("groups = %v, want [eng sre]", groups)
	}
}

// TestResolveGroups_StaleUserRefFallsThrough tests that a userRef pointing at
// a deleted/missing User CRD falls through to ladder steps 2-3 rather than
// short-circuiting to empty groups (spec: "A NotFound error from any
// GetUserCRD call SHALL fall through the ladder").
func TestResolveGroups_StaleUserRefFallsThrough(t *testing.T) {
	store := newMockStore()
	// No CRD named "stale-ref"; the preferredUsername-named CRD exists.
	store.users["jdoe"] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "jdoe", Labels: map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByLocal}},
	}
	store.groups = []*v1alpha1.Group{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}, Spec: v1alpha1.GroupSpec{Members: []string{"jdoe"}}},
	}

	data := map[string][]byte{
		"preferredUsername": []byte("jdoe"),
		"subject":           []byte("jdoe"),
		"userRef":           []byte("stale-ref"),
	}
	groups, err := resolveGroups(context.Background(), store, "ns", data)
	if err != nil {
		t.Fatalf("resolveGroups() error: %v", err)
	}
	if len(groups) != 1 || groups[0] != "team-a" {
		t.Errorf("groups = %v, want [team-a]", groups)
	}
}

// TestResolveGroups_NoUserRefFallback tests ladder steps 2-3: no userRef,
// falls through preferredUsername then oidc- derivation.
func TestResolveGroups_NoUserRefFallback(t *testing.T) {
	store := newMockStore()
	h := sha256.Sum256([]byte("subject-456"))
	store.users["oidc-"+hex.EncodeToString(h[:])[:32]] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "oidc-derived", Labels: map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByLocal}},
	}
	store.groups = []*v1alpha1.Group{
		{ObjectMeta: metav1.ObjectMeta{Name: "team-c"}, Spec: v1alpha1.GroupSpec{Members: []string{"oidc-user"}}},
	}

	data := map[string][]byte{
		"preferredUsername": []byte("oidc-user"),
		"subject":           []byte("subject-456"),
	}
	groups, err := resolveGroups(context.Background(), store, "ns", data)
	if err != nil {
		t.Fatalf("resolveGroups() error: %v", err)
	}
	if len(groups) != 1 || groups[0] != "team-c" {
		t.Errorf("groups = %v, want [team-c]", groups)
	}
}

// TestResolveGroups_NoUserCRD tests that when no User CRD is found at any
// ladder step, empty groups are returned with no error.
func TestResolveGroups_NoUserCRD(t *testing.T) {
	store := newMockStore()
	store.groups = []*v1alpha1.Group{
		{ObjectMeta: metav1.ObjectMeta{Name: "any-group"}, Spec: v1alpha1.GroupSpec{Members: []string{"nobody"}}},
	}

	data := map[string][]byte{
		"preferredUsername": []byte("unknown"),
		"subject":           []byte("unknown"),
	}
	groups, err := resolveGroups(context.Background(), store, "ns", data)
	if err != nil {
		t.Fatalf("resolveGroups() error: %v", err)
	}
	if groups != nil {
		t.Errorf("groups = %v, want nil", groups)
	}
}

// TestResolveGroups_StoreError verifies that a non-NotFound store error is
// wrapped with ErrUnavailable.
func TestResolveGroups_StoreError(t *testing.T) {
	errStore := &errorStore{mockStore: newMockStore()}
	errStore.users["jdoe"] = &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "jdoe"},
	}

	data := map[string][]byte{
		"preferredUsername": []byte("jdoe"),
		"subject":           []byte("jdoe"),
	}
	_, err := resolveGroups(context.Background(), errStore, "ns", data)
	if err == nil {
		t.Fatal("expected error from errStore, got nil")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("error %v does not wrap ErrUnavailable", err)
	}
}

// errorStore wraps mockStore but fails GetUserCRD with a non-NotFound error.
type errorStore struct {
	*mockStore
}

func (e *errorStore) GetUserCRD(_ context.Context, _, _ string) (*v1alpha1.User, error) {
	return nil, errors.New("simulated k8s API error")
}

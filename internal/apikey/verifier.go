package apikey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

const (
	defaultTTL    = 60 * time.Second
	maxRetries    = 3
	flushInterval = time.Minute
)

// keyStore defines the storage operations needed by the Verifier.
type keyStore interface {
	GetSecret(ctx context.Context, name, namespace string) (map[string][]byte, error)
	// CreateSecretExclusive must surface an AlreadyExists error (not swallow it)
	// so Generate can detect a prefix collision and retry with a fresh prefix.
	CreateSecretExclusive(ctx context.Context, name, namespace string, labels map[string]string, data map[string][]byte) error
	// PatchSecretData updates only the given data keys, preserving labels.
	PatchSecretData(ctx context.Context, name, namespace string, data map[string][]byte) error
	DeleteSecret(ctx context.Context, name, namespace string) error
	ListSecrets(ctx context.Context, namespace, labelSelector string) ([]map[string][]byte, error)
	ListGroupCRDs(ctx context.Context) ([]*v1alpha1.Group, error)
	GetUserCRD(ctx context.Context, name, namespace string) (*v1alpha1.User, error)
}

// Verifier validates API key tokens and produces *auth.Claims.
type Verifier struct {
	store    keyStore
	ns       string
	cache    *verifiedCache
	lastSeen *lastUsedTracker
}

// NewVerifier creates a new API key Verifier.
func NewVerifier(store keyStore, ns string) *Verifier {
	v := &Verifier{
		store:    store,
		ns:       ns,
		cache:    newVerifiedCache(defaultTTL),
		lastSeen: newLastUsedTracker(),
	}
	go v.lastSeen.runFlushLoop(flushInterval, v.flushLastUsed)
	return v
}

// Generate creates a new API key for the specified identity and returns the raw token.
// When name is non-empty, it is stored as the key's display name in the secret data.
func (v *Verifier) Generate(ctx context.Context, claims *auth.Claims, expiresIn time.Duration, userRef string, name string) (prefix, secret, token string, err error) {
	for range maxRetries {
		prefix, secret, token, err = Generate()
		if err != nil {
			return "", "", "", fmt.Errorf("generate token: %w", err)
		}

		labels := map[string]string{
			"varroa.dev/apikey":      "true",
			"varroa.dev/apikey-user": claims.PreferredUsername,
		}
		if labels["varroa.dev/apikey-user"] == "" {
			labels["varroa.dev/apikey-user"] = claims.Subject
		}

		data := map[string][]byte{
			"hash":              []byte(Hash(secret)),
			"subject":           []byte(claims.Subject),
			"email":             []byte(claims.Email),
			"name":              []byte(claims.Name),
			"preferredUsername": []byte(claims.PreferredUsername),
			"created":           []byte(time.Now().Format(time.RFC3339)),
		}
		if userRef != "" {
			data["userRef"] = []byte(userRef)
		}
		if name != "" {
			data["keyName"] = []byte(name)
		}
		if expiresIn > 0 {
			data["expires"] = []byte(time.Now().Add(expiresIn).Format(time.RFC3339))
		}
		// lastUsed is intentionally not set at creation — a never-used key reports
		// an empty lastUsed (the throttled flush populates it on first use).

		err = v.store.CreateSecretExclusive(ctx, "apikey-"+prefix, v.ns, labels, data)
		if err == nil {
			return prefix, secret, token, nil
		}
		// Only a prefix collision is retryable; any other error is fatal.
		if !apierrors.IsAlreadyExists(err) {
			return "", "", "", fmt.Errorf("create key: %w", err)
		}
		// Collision — regenerate prefix and retry.
	}
	return "", "", "", fmt.Errorf("create key: max retries exceeded after prefix collisions")
}

// Verify validates a vk_ token and returns the owning user's claims.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*auth.Claims, error) {
	prefix, secret, err := Parse(rawToken)
	if err != nil {
		return nil, err
	}

	// Check cache.
	if cached := v.cache.get(prefix, secret); cached != nil {
		return cached, nil
	}

	claims, storedHash, err := v.verifyUncached(ctx, prefix, secret)
	if err != nil {
		return nil, err
	}

	v.cache.set(prefix, storedHash, claims)
	v.lastSeen.record(prefix)
	return claims, nil
}

// VerifyFresh performs full verification without reading or writing the
// verified cache. It always fetches the Secret and checks the hash/expiry.
// Unlike Verify, it does not consult v.cache at all.
func (v *Verifier) VerifyFresh(ctx context.Context, rawToken string) (*auth.Claims, error) {
	prefix, secret, err := Parse(rawToken)
	if err != nil {
		return nil, err
	}

	claims, _, err := v.verifyUncached(ctx, prefix, secret)
	if err != nil {
		return nil, err
	}

	v.lastSeen.record(prefix)
	return claims, nil
}

// verifyUncached performs the full verification logic without reading or
// writing the verified cache. It returns (claims, storedHash, error).
func (v *Verifier) verifyUncached(ctx context.Context, prefix, secret string) (*auth.Claims, string, error) {
	data, err := v.store.GetSecret(ctx, "apikey-"+prefix, v.ns)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", fmt.Errorf("%w: get key: %v", ErrUnavailable, err)
		}
		return nil, "", fmt.Errorf("get key: %w", err)
	}

	// Verify hash.
	storedHash := string(data["hash"])
	if !Verify(secret, storedHash) {
		return nil, "", fmt.Errorf("invalid secret")
	}

	// Check expiry. Fail closed: an unparseable expires field rejects the key
	// rather than treating it as never-expiring.
	if expRaw := string(data["expires"]); expRaw != "" {
		exp, err := time.Parse(time.RFC3339, expRaw)
		if err != nil {
			return nil, "", fmt.Errorf("invalid expiry on key")
		}
		if time.Now().After(exp) {
			return nil, "", fmt.Errorf("key expired")
		}
	}

	// Resolve group memberships.
	groups, err := resolveGroups(ctx, v.store, v.ns, data)
	if err != nil {
		return nil, "", fmt.Errorf("resolve groups: %w", err)
	}

	claims := &auth.Claims{
		Subject:           string(data["subject"]),
		Email:             string(data["email"]),
		Name:              string(data["name"]),
		PreferredUsername: string(data["preferredUsername"]),
		Groups:            groups,
	}

	return claims, storedHash, nil
}

// ListByUser returns metadata for all API keys owned by the given user.
func (v *Verifier) ListByUser(ctx context.Context, user string) ([]KeyMeta, error) {
	all, err := v.store.ListSecrets(ctx, v.ns, "varroa.dev/apikey-user="+user)
	if err != nil {
		return nil, err
	}
	var out []KeyMeta
	for _, data := range all {
		out = append(out, keyMetaFromData(data))
	}
	return out, nil
}

// ListAll returns metadata for all API keys (admin view).
func (v *Verifier) ListAll(ctx context.Context) ([]KeyMeta, error) {
	all, err := v.store.ListSecrets(ctx, v.ns, "varroa.dev/apikey=true")
	if err != nil {
		return nil, err
	}
	var out []KeyMeta
	for _, data := range all {
		out = append(out, keyMetaFromData(data))
	}
	return out, nil
}

// Revoke deletes an API key and evicts it from the cache.
func (v *Verifier) Revoke(ctx context.Context, prefix string) error {
	v.cache.evict(prefix)
	v.lastSeen.forget(prefix)
	return v.store.DeleteSecret(ctx, "apikey-"+prefix, v.ns)
}

// Rotate issues a new key and deletes the old one. On partial failure (new
// created but old delete fails), returns a RotateError carrying the new token.
// When name is empty, the old key's keyName is carried forward.
func (v *Verifier) Rotate(ctx context.Context, claims *auth.Claims, oldPrefix string, expiresIn time.Duration, userRef string, name string) (newPrefix, newSecret, newToken string, err error) {
	// Carry forward old keyName when caller doesn't supply a new name.
	if name == "" {
		if data, getErr := v.store.GetSecret(ctx, "apikey-"+oldPrefix, v.ns); getErr == nil {
			if oldName := string(data["keyName"]); oldName != "" {
				name = oldName
			}
		}
	}
	newPrefix, newSecret, newToken, err = v.Generate(ctx, claims, expiresIn, userRef, name)
	if err != nil {
		return "", "", "", fmt.Errorf("rotate: generate new key: %w", err)
	}

	// Evict old prefix from cache regardless of delete outcome.
	v.cache.evict(oldPrefix)
	v.lastSeen.forget(oldPrefix)

	if delErr := v.store.DeleteSecret(ctx, "apikey-"+oldPrefix, v.ns); delErr != nil {
		return newPrefix, newSecret, newToken, &RotateError{
			NewToken: newToken,
			Err:      fmt.Errorf("delete old key: %w", delErr),
		}
	}
	return newPrefix, newSecret, newToken, nil
}

func (v *Verifier) flushLastUsed(prefix string, lastUsed time.Time) {
	ctx := context.Background()
	name := "apikey-" + prefix
	// Patch only the lastUsed field. A full create-or-update would replace the
	// object and drop the key's labels, making it invisible to list endpoints.
	_ = v.store.PatchSecretData(ctx, name, v.ns, map[string][]byte{
		"lastUsed": []byte(lastUsed.Format(time.RFC3339)),
	})
}

// resolveGroups resolves group memberships using the managed-by-aware ladder:
//  1. userRef present → GetUserCRD(userRef)
//  2. no userRef → GetUserCRD(preferredUsername or subject)
//  3. still not found → derive oidc- name → GetUserCRD(derived)
//     4a. found + managed-by=local → scan Group.Spec.Members
//     4b. found + any other managed-by → return User.Status.ObservedGroups
func resolveGroups(ctx context.Context, store keyStore, ns string, data map[string][]byte) ([]string, error) {
	preferredUsername := string(data["preferredUsername"])
	subject := string(data["subject"])
	resolveFor := preferredUsername
	if resolveFor == "" {
		resolveFor = subject
	}

	// Ladder: candidate CRD names in order. A NotFound at any step falls
	// through to the next candidate (specs/apikey-identity-resolution);
	// any other error fails closed as ErrUnavailable.
	var candidates []string
	if userRef := string(data["userRef"]); userRef != "" {
		candidates = append(candidates, userRef)
	}
	if resolveFor != "" {
		candidates = append(candidates, resolveFor)
	}
	if subject != "" {
		h := sha256.Sum256([]byte(subject))
		candidates = append(candidates, "oidc-"+hex.EncodeToString(h[:])[:32])
	}

	var user *v1alpha1.User
	for _, name := range candidates {
		u, err := store.GetUserCRD(ctx, name, ns)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("%w: get user CRD: %v", ErrUnavailable, err)
		}
		if u != nil {
			user = u
			break
		}
	}

	// No User CRD found at any step → empty groups, log one warning.
	if user == nil {
		slog.Warn("no User CRD found for key", "resolveFor", resolveFor)
		return nil, nil
	}

	// Step 4: managed-by-aware group resolution.
	managedBy := user.Labels[v1alpha1.LabelManagedBy]
	if managedBy == v1alpha1.ManagedByLocal {
		// Scan Group CRD members for preferredUsername-or-subject (the same
		// identity the login path matches on — see auth/local resolveGroups).
		memberName := resolveFor
		all, err := store.ListGroupCRDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: list groups: %v", ErrUnavailable, err)
		}
		var groups []string
		for _, g := range all {
			for _, m := range g.Spec.Members {
				if m == memberName {
					groups = append(groups, g.Name)
					break
				}
			}
		}
		if groups == nil {
			groups = []string{}
		}
		return groups, nil
	}

	// IdP-managed (or any other non-local) → return ObservedGroups verbatim.
	groups := user.Status.ObservedGroups
	if groups == nil {
		groups = []string{}
	}
	return groups, nil
}

// KeyMeta is the non-secret metadata returned by list endpoints.
type KeyMeta struct {
	Prefix   string `json:"prefix"`
	Created  string `json:"created"`
	LastUsed string `json:"lastUsed,omitempty"`
	Expires  string `json:"expires,omitempty"`
	Name     string `json:"name,omitempty"`
}

func keyMetaFromData(data map[string][]byte) KeyMeta {
	prefix := ""
	if name := string(data["_name"]); len(name) > 7 {
		prefix = name[7:] // strip "apikey-" prefix
	}
	return KeyMeta{
		Prefix:   prefix,
		Created:  string(data["created"]),
		LastUsed: string(data["lastUsed"]),
		Expires:  string(data["expires"]),
		Name:     string(data["keyName"]),
	}
}

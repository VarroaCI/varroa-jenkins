package api

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// mustJSON renders a sanitized value back to a string for substring assertions.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal sanitized value: %v", err)
	}
	return string(b)
}

func TestSanitizeObject_StripsControllerWakeToken(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "hibernated",
			Namespace:       "ns",
			ResourceVersion: "12345",
			UID:             "abcd-uid",
			Generation:      7,
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "varroa-ui"}},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:     "Hibernated",
			WakeToken: "super-secret-wake-token-12345",
		},
	}

	out, err := SanitizeObject(cr)
	if err != nil {
		t.Fatalf("SanitizeObject: %v", err)
	}
	got := mustJSON(t, out)

	for _, forbidden := range []string{
		"wakeToken", "super-secret-wake-token-12345",
		"managedFields", "resourceVersion", "12345", "abcd-uid",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sanitized output must not contain %q, got:\n%s", forbidden, got)
		}
	}
	// Non-sensitive content survives.
	if !strings.Contains(got, "hibernated") || !strings.Contains(got, "Hibernated") {
		t.Errorf("sanitizer stripped legitimate fields, got:\n%s", got)
	}
}

func TestSanitizeObject_StripsUserCredentials(t *testing.T) {
	user := &v1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "ns"},
		Spec:       v1alpha1.UserSpec{Password: "plaintext-hunter2"},
		Status: v1alpha1.UserStatus{
			Credentials: &v1alpha1.UserCredentials{PasswordHash: "$2a$10$deadbeefhash"},
		},
	}

	out, err := SanitizeObject(user)
	if err != nil {
		t.Fatalf("SanitizeObject: %v", err)
	}
	got := mustJSON(t, out)

	for _, forbidden := range []string{
		"passwordHash", "$2a$10$deadbeefhash", "plaintext-hunter2",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sanitized output must not contain %q, got:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "alice") {
		t.Errorf("sanitizer stripped the user identity, got:\n%s", got)
	}
}

// Collections are the case the MCP list_* tools hit: stripping only the
// top-level object would leave every member's secrets intact.
func TestSanitizeObject_StripsCollectionMembers(t *testing.T) {
	crs := []*v1alpha1.Controller{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
			Status:     v1alpha1.ControllerStatus{WakeToken: "token-a"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"},
			Status:     v1alpha1.ControllerStatus{WakeToken: "token-b"},
		},
	}

	out, err := SanitizeObject(crs)
	if err != nil {
		t.Fatalf("SanitizeObject: %v", err)
	}
	got := mustJSON(t, out)

	for _, forbidden := range []string{"wakeToken", "token-a", "token-b"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sanitized collection must not contain %q, got:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, `"a"`) || !strings.Contains(got, `"b"`) {
		t.Errorf("sanitizer dropped collection members, got:\n%s", got)
	}
}

// A blanket "delete any key containing secret/password" sweep would strip these;
// they are references by name, not secret values, and callers need them.
func TestSanitizeObject_PreservesSecretReferences(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"tlsSecretName":    "my-tls",
			"existingSecret":   "my-existing",
			"imagePullSecrets": []any{"regcred"},
			"secretRef":        map[string]any{"name": "creds"},
		},
	}

	out, err := SanitizeObject(obj)
	if err != nil {
		t.Fatalf("SanitizeObject: %v", err)
	}
	got := mustJSON(t, out)

	for _, required := range []string{"tlsSecretName", "existingSecret", "imagePullSecrets", "secretRef"} {
		if !strings.Contains(got, required) {
			t.Errorf("sanitizer must preserve reference field %q, got:\n%s", required, got)
		}
	}
}

func TestSanitizeObject_IsIdempotentAndPassesScalars(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Status:     v1alpha1.ControllerStatus{WakeToken: "tok"},
	}
	once, err := SanitizeObject(cr)
	if err != nil {
		t.Fatalf("SanitizeObject: %v", err)
	}
	twice, err := SanitizeObject(once)
	if err != nil {
		t.Fatalf("SanitizeObject (second pass): %v", err)
	}
	if mustJSON(t, once) != mustJSON(t, twice) {
		t.Errorf("sanitization is not idempotent:\nonce:  %s\ntwice: %s", mustJSON(t, once), mustJSON(t, twice))
	}

	for _, scalar := range []any{"a string", 42.0, true, nil} {
		out, err := SanitizeObject(scalar)
		if err != nil {
			t.Fatalf("SanitizeObject(%v): %v", scalar, err)
		}
		if mustJSON(t, out) != mustJSON(t, scalar) {
			t.Errorf("scalar %v was altered: got %s", scalar, mustJSON(t, out))
		}
	}
}

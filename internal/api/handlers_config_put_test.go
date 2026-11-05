package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// putRecordingBrood records the five config updates that the BFF forwards to
// a cluster. It answers each update with its input, or with err when err is set.
type putRecordingBrood struct {
	ConfigBrood
	calls int
	obj   json.RawMessage
	err   error
}

func (b *putRecordingBrood) record(obj json.RawMessage) (json.RawMessage, error) {
	b.calls++
	b.obj = obj
	if b.err != nil {
		return nil, b.err
	}
	return obj, nil
}

func (b *putRecordingBrood) UpdateComposedBundle(_ context.Context, _, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return b.record(obj)
}

func (b *putRecordingBrood) UpdateCatalogSource(_ context.Context, _, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return b.record(obj)
}

func (b *putRecordingBrood) UpdateJenkinsRole(_ context.Context, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return b.record(obj)
}

func (b *putRecordingBrood) UpdateJenkinsRoleBinding(_ context.Context, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return b.record(obj)
}

func (b *putRecordingBrood) UpdateVersionProfile(_ context.Context, _, _ string, obj json.RawMessage) (json.RawMessage, error) {
	return b.record(obj)
}

// configPutAuthorizer grants the config kinds to two users. editor holds read
// and update. author holds read, create and update. The bindings are unscoped.
func configPutAuthorizer() *Authorizer {
	resources := []string{"composedbundles", "catalogsources", "jenkinsroles", "jenkinsrolebindings", "version-profiles"}
	roles := []*v1alpha1.VarroaRole{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "config-editor"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: resources, Verbs: []string{"read", "update"}},
			}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "config-author"},
			Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
				{Resources: resources, Verbs: []string{"read", "create", "update"}},
			}},
		},
	}
	bindings := []*v1alpha1.VarroaRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "editor-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "editor"}},
				RoleRef:  "config-editor",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "author-binding"},
			Spec: v1alpha1.VarroaRoleBindingSpec{
				Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "author"}},
				RoleRef:  "config-author",
			},
		},
	}
	return NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

type configPutDispatch func(*Server, http.ResponseWriter, *http.Request, string, []string)

type configPutCase struct {
	name     string
	dispatch configPutDispatch
	segments []string
}

func configPutCases() []configPutCase {
	return []configPutCase{
		{"composedbundles", (*Server).dispatchComposedBundles, []string{"team-a", "base"}},
		{"catalogsources", (*Server).dispatchCatalogSources, []string{"team-a", "src"}},
		{"jenkinsroles", (*Server).dispatchJenkinsRoles, []string{"viewer"}},
		{"jenkinsrolebindings", (*Server).dispatchJenkinsRoleBindings, []string{"viewer-binding"}},
		{"version-profiles", (*Server).dispatchVersionProfiles, []string{"2.479"}},
	}
}

// configPutServer builds a server whose brood records the updates. Its cluster
// is member-b, a member that is not the BFF's own cluster.
func configPutServer(brood ConfigBrood) *Server {
	cluster := &configOpnsBrood{clusters: []ClusterInfo{
		{ClusterInfo: bus.ClusterInfo{Name: "core"}, Core: true, Healthy: true},
		{ClusterInfo: bus.ClusterInfo{Name: "member-b", OperatorNamespace: "varroa-member-b"}, Healthy: true},
	}}
	return NewServer(&Dependencies{
		Authorizer:        configPutAuthorizer(),
		ConfigBrood:       brood,
		Brood:             cluster,
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})
}

// configPutRequest sends a PUT whose metadata carries resourceVersion when rv
// is not empty.
func configPutRequest(t *testing.T, user string, rv string) *http.Request {
	t.Helper()
	md := map[string]any{"name": "x"}
	if rv != "" {
		md["resourceVersion"] = rv
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"metadata":   md,
		"spec":       map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	return req.WithContext(contextWithClaims(req.Context(), &auth.Claims{Subject: user}))
}

// TestConfigPut_WithoutVersionNeedsCreatePermission pins that a body without a
// resourceVersion answers 403 and publishes nothing for a caller that holds only
// the update verb.
func TestConfigPut_WithoutVersionNeedsCreatePermission(t *testing.T) {
	for _, tc := range configPutCases() {
		t.Run(tc.name, func(t *testing.T) {
			brood := &putRecordingBrood{}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			tc.dispatch(srv, w, configPutRequest(t, "editor", ""), "member-b", tc.segments)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403 (body %s)", w.Code, w.Body.String())
			}
			if brood.calls != 0 {
				t.Fatalf("brood calls %d, want 0", brood.calls)
			}
		})
	}
}

// TestConfigPut_WithoutVersionAndCreatePermissionReachesBrood pins that a caller
// with the create verb forwards a body without a resourceVersion.
func TestConfigPut_WithoutVersionAndCreatePermissionReachesBrood(t *testing.T) {
	for _, tc := range configPutCases() {
		t.Run(tc.name, func(t *testing.T) {
			brood := &putRecordingBrood{}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			tc.dispatch(srv, w, configPutRequest(t, "author", ""), "member-b", tc.segments)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if brood.calls != 1 {
				t.Fatalf("brood calls %d, want 1", brood.calls)
			}
		})
	}
}

// TestConfigPut_WithVersionNeedsOnlyUpdatePermission pins that a body with a
// resourceVersion needs the update verb alone, and forwards the version.
func TestConfigPut_WithVersionNeedsOnlyUpdatePermission(t *testing.T) {
	for _, tc := range configPutCases() {
		t.Run(tc.name, func(t *testing.T) {
			brood := &putRecordingBrood{}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			tc.dispatch(srv, w, configPutRequest(t, "editor", "7"), "member-b", tc.segments)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if brood.calls != 1 {
				t.Fatalf("brood calls %d, want 1", brood.calls)
			}
			var sent struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(brood.obj, &sent); err != nil {
				t.Fatalf("decode forwarded object: %v", err)
			}
			if sent.Metadata.ResourceVersion != "7" {
				t.Fatalf("forwarded resourceVersion %q, want %q", sent.Metadata.ResourceVersion, "7")
			}
		})
	}
}

// TestConfigPut_BroodConflictAnswers409 pins that a conflict from the member's
// operator reaches the caller as 409 with the operator's text.
func TestConfigPut_BroodConflictAnswers409(t *testing.T) {
	const msg = "role \"viewer\" changed or was deleted after it was read: read it again and retry"
	brood := &putRecordingBrood{err: &BroodError{Code: bus.CodeConflict, Msg: msg}}
	srv := configPutServer(brood)
	w := httptest.NewRecorder()
	srv.dispatchJenkinsRoles(w, configPutRequest(t, "author", ""), "member-b", []string{"viewer"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "changed or was deleted after it was read") {
		t.Fatalf("body %q, want the operator conflict text", w.Body.String())
	}
}

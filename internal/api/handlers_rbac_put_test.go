package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// rbacPutBody builds a PUT body that carries the given resourceVersion. An
// empty version yields a body without one.
func rbacPutBody(t *testing.T, name, rv string, spec any) *strings.Reader {
	t.Helper()
	meta := map[string]any{"name": name}
	if rv != "" {
		meta["resourceVersion"] = rv
	}
	raw, err := json.Marshal(map[string]any{"metadata": meta, "spec": spec})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return strings.NewReader(string(raw))
}

// getRoleForPut reads a role through the REST GET and returns the decoded
// object, including its resourceVersion.
func getRoleForPut(t *testing.T, srv *Server, name string) v1alpha1.VarroaRole {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleVarroaRoleDispatch(w, withClaims(httptest.NewRequest(http.MethodGet, "/roles/"+name, nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("GET role %s: status %d, body %s", name, w.Code, w.Body.String())
	}
	var role v1alpha1.VarroaRole
	if err := json.Unmarshal(w.Body.Bytes(), &role); err != nil {
		t.Fatalf("decode role: %v", err)
	}
	return role
}

// getBindingForPut reads a binding through the REST GET and returns the
// decoded object, including its resourceVersion.
func getBindingForPut(t *testing.T, srv *Server) v1alpha1.VarroaRoleBinding {
	t.Helper()
	w := httptest.NewRecorder()
	srv.handleVarroaRoleBindingDispatch(w, withClaims(httptest.NewRequest(http.MethodGet, "/rolebindings/binding-a", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("GET binding-a: status %d, body %s", w.Code, w.Body.String())
	}
	var b v1alpha1.VarroaRoleBinding
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	return b
}

func seedRolePut(t *testing.T, srv *Server, name, ref string) {
	t.Helper()
	crdstore.MustSeed(srv.deps.Store.(*crdstore.Fake), &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.VarroaRoleSpec{JenkinsRoleRef: ref},
	})
}

func seedBindingPut(t *testing.T, srv *Server, name string, subjects ...v1alpha1.SubjectRef) {
	t.Helper()
	crdstore.MustSeed(srv.deps.Store.(*crdstore.Fake), &v1alpha1.VarroaRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.VarroaRoleBindingSpec{Subjects: subjects, RoleRef: "admin"},
	})
}

// TestHandleVarroaRolePut_ConcurrentWriteAnswers409 pins that a write made
// between the read and the PUT answers 409 and keeps the concurrent value.
func TestHandleVarroaRolePut_ConcurrentWriteAnswers409(t *testing.T) {
	srv, sink := publishTestServer(t)
	ctx := context.Background()
	seedRolePut(t, srv, "role-a", "first")
	read := getRoleForPut(t, srv, "role-a")
	sink.events = nil

	if _, err := crdstore.Mutate[v1alpha1.VarroaRole](ctx, srv.deps.Store, "role-a", "", func(r *v1alpha1.VarroaRole) (bool, error) {
		r.Spec.JenkinsRoleRef = "concurrent"
		return true, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/roles/role-a",
		rbacPutBody(t, "role-a", read.ResourceVersion, v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "mine"})))
	srv.handleVarroaRoleDispatch(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	got := getRoleForPut(t, srv, "role-a")
	if got.Spec.JenkinsRoleRef != "concurrent" {
		t.Errorf("jenkinsRoleRef = %q, want the concurrent value", got.Spec.JenkinsRoleRef)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none", sink.events)
	}
}

// TestHandleVarroaRolePut_CurrentVersionWrites pins that a PUT with the
// version of its read writes and publishes the update.
func TestHandleVarroaRolePut_CurrentVersionWrites(t *testing.T) {
	srv, sink := publishTestServer(t)
	seedRolePut(t, srv, "role-a", "first")
	read := getRoleForPut(t, srv, "role-a")
	sink.events = nil

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/roles/role-a",
		rbacPutBody(t, "role-a", read.ResourceVersion, v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "mine"})))
	srv.handleVarroaRoleDispatch(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body.String())
	}
	if got := getRoleForPut(t, srv, "role-a"); got.Spec.JenkinsRoleRef != "mine" {
		t.Errorf("jenkinsRoleRef = %q, want mine", got.Spec.JenkinsRoleRef)
	}
	if len(sink.events) != 1 {
		t.Errorf("events = %d, want 1", len(sink.events))
	}
}

// TestHandleVarroaRolePut_WithoutVersionOnExistingAnswers409 pins that a body
// without a version does not overwrite an existing role.
func TestHandleVarroaRolePut_WithoutVersionOnExistingAnswers409(t *testing.T) {
	srv, _ := publishTestServer(t)
	seedRolePut(t, srv, "role-a", "first")

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/roles/role-a",
		rbacPutBody(t, "role-a", "", v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "mine"})))
	srv.handleVarroaRoleDispatch(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "metadata.resourceVersion") {
		t.Errorf("body %s does not name metadata.resourceVersion", w.Body.String())
	}
	if got := getRoleForPut(t, srv, "role-a"); got.Spec.JenkinsRoleRef != "first" {
		t.Errorf("jenkinsRoleRef = %q, want first", got.Spec.JenkinsRoleRef)
	}
}

// TestHandleVarroaRolePut_WithoutVersionCreates pins that a body without a
// version creates a missing role.
func TestHandleVarroaRolePut_WithoutVersionCreates(t *testing.T) {
	srv, _ := publishTestServer(t)

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/roles/role-new",
		rbacPutBody(t, "role-new", "", v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "mine"})))
	srv.handleVarroaRoleDispatch(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body.String())
	}
	if got := getRoleForPut(t, srv, "role-new"); got.Spec.JenkinsRoleRef != "mine" {
		t.Errorf("jenkinsRoleRef = %q, want mine", got.Spec.JenkinsRoleRef)
	}
}

// TestHandleVarroaRolePut_DeletedAfterReadAnswers409 pins that a version for a
// role that no longer exists answers 409 and creates nothing.
func TestHandleVarroaRolePut_DeletedAfterReadAnswers409(t *testing.T) {
	srv, _ := publishTestServer(t)

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/roles/role-gone",
		rbacPutBody(t, "role-gone", "5", v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "mine"})))
	srv.handleVarroaRoleDispatch(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	if _, err := crdstore.Get[v1alpha1.VarroaRole](context.Background(), srv.deps.Store, "role-gone", ""); err == nil {
		t.Errorf("role-gone exists after a 409, want none")
	}
}

// TestHandleVarroaRoleBindingPut_ConcurrentWriteAnswers409 pins that a subject
// added between the read and the PUT answers 409 and survives.
func TestHandleVarroaRoleBindingPut_ConcurrentWriteAnswers409(t *testing.T) {
	srv, sink := publishTestServer(t)
	ctx := context.Background()
	seedBindingPut(t, srv, "binding-a", v1alpha1.SubjectRef{Kind: "User", Name: "alice"})
	read := getBindingForPut(t, srv)
	sink.events = nil

	if _, err := crdstore.Mutate[v1alpha1.VarroaRoleBinding](ctx, srv.deps.Store, "binding-a", "", func(b *v1alpha1.VarroaRoleBinding) (bool, error) {
		b.Spec.Subjects = append(b.Spec.Subjects, v1alpha1.SubjectRef{Kind: "User", Name: "carol"})
		return true, nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/rolebindings/binding-a",
		rbacPutBody(t, "binding-a", read.ResourceVersion, v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}, RoleRef: "viewer",
		})))
	srv.handleVarroaRoleBindingDispatch(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
	got := getBindingForPut(t, srv)
	if len(got.Spec.Subjects) != 2 || got.Spec.Subjects[1].Name != "carol" {
		t.Errorf("subjects = %+v, want the concurrent carol kept", got.Spec.Subjects)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %+v, want none", sink.events)
	}
}

// TestHandleVarroaRoleBindingPut_WithoutVersionOnExistingAnswers409 pins that a
// body without a version does not overwrite an existing binding.
func TestHandleVarroaRoleBindingPut_WithoutVersionOnExistingAnswers409(t *testing.T) {
	srv, _ := publishTestServer(t)
	seedBindingPut(t, srv, "binding-a", v1alpha1.SubjectRef{Kind: "User", Name: "alice"})

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/rolebindings/binding-a",
		rbacPutBody(t, "binding-a", "", v1alpha1.VarroaRoleBindingSpec{RoleRef: "viewer"})))
	srv.handleVarroaRoleBindingDispatch(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s; want 409", w.Code, w.Body.String())
	}
}

// TestHandleVarroaRoleBindingPut_CurrentVersionWrites pins that a PUT with the
// version of its read writes.
func TestHandleVarroaRoleBindingPut_CurrentVersionWrites(t *testing.T) {
	srv, _ := publishTestServer(t)
	seedBindingPut(t, srv, "binding-a", v1alpha1.SubjectRef{Kind: "User", Name: "alice"})
	read := getBindingForPut(t, srv)

	w := httptest.NewRecorder()
	r := withClaims(httptest.NewRequest(http.MethodPut, "/rolebindings/binding-a",
		rbacPutBody(t, "binding-a", read.ResourceVersion, v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "alice"}}, RoleRef: "viewer",
		})))
	srv.handleVarroaRoleBindingDispatch(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", w.Code, w.Body.String())
	}
	if got := getBindingForPut(t, srv); got.Spec.RoleRef != "viewer" {
		t.Errorf("roleRef = %q, want viewer", got.Spec.RoleRef)
	}
}

// Seeded versions of the contract fixtures. The PUT cases send them back as
// the precondition of their read.
const (
	contractAdminRoleVersion    = "100"
	contractAdminBindingVersion = "200"
)

// versionedBody returns a body that names the object and carries its version.
func versionedBody(name, rv string) string {
	return `{"metadata":{"name":"` + name + `","resourceVersion":"` + rv + `"}}`
}

// createdResourceVersion decodes the metadata.resourceVersion of a create
// response.
func createdResourceVersion(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var obj struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if obj.Metadata.ResourceVersion == "" {
		t.Fatalf("create response has no resourceVersion: %s", w.Body.String())
	}
	return obj.Metadata.ResourceVersion
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// federatedStored is a stored JenkinsRole or JenkinsRoleBinding copy. Its labels
// and annotations name the source objects.
func federatedStored(t *testing.T) json.RawMessage {
	t.Helper()
	obj := map[string]any{
		"apiVersion": "varroa.dev/v1alpha1",
		"metadata": map[string]any{
			"name":            "x",
			"resourceVersion": "3",
			"labels":          map[string]string{rbac.LabelFederatedFrom: "alpha"},
			"annotations": map[string]string{
				rbac.AnnotationFederatedRoles:    "alpha,zeta",
				rbac.AnnotationFederatedBindings: "bind-a,bind-b",
			},
		},
		"spec": map[string]any{},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal stored object: %v", err)
	}
	return raw
}

// TestConfigPut_FederatedCopyAnswers409 pins that a PUT to a stored federated
// copy answers 409 with code federated_copy and writes nothing, even when the
// body drops the label.
func TestConfigPut_FederatedCopyAnswers409(t *testing.T) {
	cases := []struct {
		name     string
		dispatch configPutDispatch
		want     string
	}{
		{"jenkinsroles", (*Server).dispatchJenkinsRoles, "JenkinsRole x is a federated copy of VarroaRole alpha, zeta."},
		{"jenkinsrolebindings", (*Server).dispatchJenkinsRoleBindings, "JenkinsRoleBinding x is a federated copy of VarroaRoleBinding bind-a, bind-b."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brood := &putRecordingBrood{stored: federatedStored(t)}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			tc.dispatch(srv, w, configPutRequest(t, "editor", "7"), "member-b", []string{"x"})
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409 (body %s)", w.Code, w.Body.String())
			}
			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Code != federatedCopyCode {
				t.Fatalf("code %q, want %q", body.Code, federatedCopyCode)
			}
			if !strings.Contains(body.Error, tc.want) {
				t.Fatalf("error %q, want it to name the source %q", body.Error, tc.want)
			}
			if brood.calls != 0 {
				t.Fatalf("brood calls %d, want 0", brood.calls)
			}
		})
	}
}

// TestConfigPut_UnlabeledStoredObjectReachesBrood pins that a stored object
// without the federation label takes the write.
func TestConfigPut_UnlabeledStoredObjectReachesBrood(t *testing.T) {
	for _, tc := range configPutCases() {
		if tc.name != "jenkinsroles" && tc.name != "jenkinsrolebindings" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			stored := json.RawMessage(`{"metadata":{"name":"x","resourceVersion":"3"},"spec":{}}`)
			brood := &putRecordingBrood{stored: stored}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			tc.dispatch(srv, w, configPutRequest(t, "editor", "3"), "member-b", tc.segments)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if brood.calls != 1 {
				t.Fatalf("brood calls %d, want 1", brood.calls)
			}
		})
	}
}

// TestConfigPut_StoredReadErrorWritesNothing pins that a failed read of the
// stored object stops the write, so the guard never guesses.
func TestConfigPut_StoredReadErrorWritesNothing(t *testing.T) {
	for _, tc := range configPutCases() {
		if tc.name != "jenkinsroles" && tc.name != "jenkinsrolebindings" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			brood := &putRecordingBrood{getErr: &BroodError{Code: bus.CodeInternal, Msg: "bus down"}}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			tc.dispatch(srv, w, configPutRequest(t, "editor", "3"), "member-b", tc.segments)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500 (body %s)", w.Code, w.Body.String())
			}
			if brood.calls != 0 {
				t.Fatalf("brood calls %d, want 0", brood.calls)
			}
		})
	}
}

// labelKinds names the two Jenkins RBAC kinds that take a caller body.
var labelKinds = []struct {
	name     string
	dispatch configPutDispatch
	kind     string
}{
	{"jenkinsroles", (*Server).dispatchJenkinsRoles, "JenkinsRole"},
	{"jenkinsrolebindings", (*Server).dispatchJenkinsRoleBindings, "JenkinsRoleBinding"},
}

// requireBodyError checks a 400 or 409 answer that names its machine code.
func requireBodyError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d (body %s)", w.Code, status, w.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != code {
		t.Fatalf("code %q, want %q", body.Code, code)
	}
}

// TestConfigPost_FederatedLabelAnswers400 pins that a create body which sets the
// federation label answers 400 with code federated_label and writes nothing.
func TestConfigPost_FederatedLabelAnswers400(t *testing.T) {
	for _, tc := range labelKinds {
		t.Run(tc.name, func(t *testing.T) {
			brood := &putRecordingBrood{}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			req := configWriteRequest(t, http.MethodPost, "author", "", map[string]string{rbac.LabelFederatedFrom: "alpha"})
			tc.dispatch(srv, w, req, "member-b", nil)
			requireBodyError(t, w, http.StatusBadRequest, federatedLabelCode)
			want := "The " + tc.kind + " request sets the label varroa.dev/federated-from"
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if !strings.Contains(body.Error, want) {
				t.Fatalf("error %q, want it to contain %q", body.Error, want)
			}
			if brood.calls != 0 {
				t.Fatalf("brood calls %d, want 0", brood.calls)
			}
		})
	}
}

// TestConfigPost_OtherLabelsReachBrood pins that a create body without the
// federation label, or with it set to an empty value, takes the write.
func TestConfigPost_OtherLabelsReachBrood(t *testing.T) {
	subcases := []struct {
		name   string
		labels map[string]string
	}{
		{"other label", map[string]string{"owner": "a"}},
		{"empty federation label", map[string]string{rbac.LabelFederatedFrom: ""}},
	}
	for _, tc := range labelKinds {
		for _, sc := range subcases {
			t.Run(tc.name+"/"+sc.name, func(t *testing.T) {
				brood := &putRecordingBrood{}
				srv := configPutServer(brood)
				w := httptest.NewRecorder()
				tc.dispatch(srv, w, configWriteRequest(t, http.MethodPost, "author", "", sc.labels), "member-b", nil)
				if w.Code != http.StatusCreated {
					t.Fatalf("status %d, want 201 (body %s)", w.Code, w.Body.String())
				}
				if brood.calls != 1 {
					t.Fatalf("brood calls %d, want 1", brood.calls)
				}
			})
		}
	}
}

// TestConfigPut_FederatedLabelOnOtherObjectAnswers400 pins that an update body
// which sets the federation label answers 400 when the stored object is not a
// copy, or does not exist, and writes nothing.
func TestConfigPut_FederatedLabelOnOtherObjectAnswers400(t *testing.T) {
	subcases := []struct {
		name   string
		stored json.RawMessage
		user   string
		rv     string
	}{
		{"stored unlabeled", json.RawMessage(`{"metadata":{"name":"x","resourceVersion":"3"},"spec":{}}`), "editor", "3"},
		{"not found", nil, "author", ""},
	}
	for _, tc := range labelKinds {
		for _, sc := range subcases {
			t.Run(tc.name+"/"+sc.name, func(t *testing.T) {
				brood := &putRecordingBrood{stored: sc.stored}
				srv := configPutServer(brood)
				w := httptest.NewRecorder()
				req := configWriteRequest(t, http.MethodPut, sc.user, sc.rv, map[string]string{rbac.LabelFederatedFrom: "alpha"})
				tc.dispatch(srv, w, req, "member-b", []string{"x"})
				requireBodyError(t, w, http.StatusBadRequest, federatedLabelCode)
				if brood.calls != 0 {
					t.Fatalf("brood calls %d, want 0", brood.calls)
				}
			})
		}
	}
}

// TestConfigPut_CopyWithLabelInBodyAnswers409 pins the order of the two guards:
// a write to a stored copy keeps the 409 federated_copy answer, even when the
// body repeats the label.
func TestConfigPut_CopyWithLabelInBodyAnswers409(t *testing.T) {
	for _, tc := range labelKinds {
		t.Run(tc.name, func(t *testing.T) {
			brood := &putRecordingBrood{stored: federatedStored(t)}
			srv := configPutServer(brood)
			w := httptest.NewRecorder()
			req := configWriteRequest(t, http.MethodPut, "editor", "3", map[string]string{rbac.LabelFederatedFrom: "alpha"})
			tc.dispatch(srv, w, req, "member-b", []string{"x"})
			requireBodyError(t, w, http.StatusConflict, federatedCopyCode)
			if brood.calls != 0 {
				t.Fatalf("brood calls %d, want 0", brood.calls)
			}
		})
	}
}

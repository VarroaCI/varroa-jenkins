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

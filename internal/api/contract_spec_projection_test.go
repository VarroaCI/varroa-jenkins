package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestControllerDetailCarriesSpec pins the API-contract requirement that the
// controller detail response carries the full ControllerSpec under `spec`,
// consistently across the GET, create and update responses. The response
// schema validation in TestContract also enforces `spec` as a required
// property of ControllerDetail; this test asserts the actual value matches
// the controller's stored spec on each of the three operations.
func TestControllerDetailCarriesSpec(t *testing.T) {
	srv, _ := newContractServer(t)
	defer srv.Close()

	// GET detail — the seeded controller in newContractServer.
	seeded := &v1alpha1.ControllerSpec{Endpoint: "https://test.example.com"}
	status, body := doAuthorizedContractRequest(t, srv, http.MethodGet, "/api/v1/clusters/core/controllers/test-ns/test-ctrl", nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body: %s", status, body)
	}
	requireSpecInResponse(t, body, seeded)

	// POST create — the response spec echoes the request spec.
	createBody := map[string]any{
		"metadata": map[string]any{"name": "created-ctrl"},
		"spec":     map[string]any{"version": "2.555", "className": "small"},
	}
	status, body = doAuthorizedContractRequest(t, srv, http.MethodPost, "/api/v1/clusters/core/controllers/test-ns", createBody)
	if status != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201; body: %s", status, body)
	}
	requireSpecInResponse(t, body, &v1alpha1.ControllerSpec{Version: "2.555", ClassName: "small"})

	// PATCH update — the response spec is the merged (existing + patch) spec.
	patchBody := map[string]any{"spec": map[string]any{"version": "2.0"}}
	status, body = doAuthorizedContractRequest(t, srv, http.MethodPatch, "/api/v1/clusters/core/controllers/test-ns/test-ctrl", patchBody)
	if status != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body: %s", status, body)
	}
	requireSpecInResponse(t, body, &v1alpha1.ControllerSpec{Endpoint: "https://test.example.com", Version: "2.0"})
}

// doAuthorizedContractRequest performs a contract-style request with the
// admin token/claims, mirroring the request construction in TestContract.
func doAuthorizedContractRequest(t *testing.T, srv *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()

	var rd io.Reader
	if body != nil {
		js, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(js)
	}

	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(contextWithClaims(req.Context(), adminClaims))
	req.Header.Set("Authorization", "Bearer valid-admin-token")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// requireSpecInResponse asserts that the JSON body carries a `spec` object
// equal to the marshalled v1alpha1.ControllerSpec.
func requireSpecInResponse(t *testing.T, body []byte, want *v1alpha1.ControllerSpec) {
	t.Helper()

	var resp struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Spec) == 0 || string(resp.Spec) == "null" {
		t.Fatalf("response is missing the spec field; body: %s", body)
	}

	var got, wantMap map[string]any
	if err := json.Unmarshal(resp.Spec, &got); err != nil {
		t.Fatalf("decode response spec: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want spec: %v", err)
	}
	if err := json.Unmarshal(wantJSON, &wantMap); err != nil {
		t.Fatalf("decode want spec: %v", err)
	}

	if !reflect.DeepEqual(got, wantMap) {
		t.Errorf("spec mismatch:\n  got:  %s\n  want: %s", resp.Spec, wantJSON)
	}
}

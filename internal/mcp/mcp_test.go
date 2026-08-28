package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func newTestDeps() *api.Dependencies {
	return &api.Dependencies{
		Client: &stubClient{},
		Store:  crdstore.NewFake(),
	}
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func mcpRequest(t *testing.T, handler http.Handler, method string, params interface{}, claims *auth.Claims) rpcResponse {
	t.Helper()

	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		body["params"] = params
	}
	b, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	if claims != nil {
		ctx := auth.ContextWithClaims(req.Context(), claims)
		req = req.WithContext(ctx)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp rpcResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	return resp
}

type toolResult struct {
	Content           []contentItem   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseToolResult(t *testing.T, raw json.RawMessage) toolResult {
	t.Helper()
	var tr toolResult
	json.Unmarshal(raw, &tr)
	return tr
}

func TestToolsListReturnsAllTools(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/list", nil, nil)
	if resp.Error != nil {
		t.Fatalf("tools/list returned error: %s", resp.Error)
	}

	var wrapper struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(resp.Result, &wrapper)

	if len(wrapper.Tools) < 50 {
		t.Fatalf("expected at least 50 tools, got %d", len(wrapper.Tools))
	}
	names := map[string]bool{}
	for _, tool := range wrapper.Tools {
		names[tool.Name] = true
	}
	expected := []string{
		"list_controllers", "get_controller", "create_controller",
		"update_controller", "delete_controller",
		"reconcile_controller", "restart_controller", "get_controller_logs",
		"list_varroa_roles", "get_varroa_role", "create_varroa_role",
		"update_varroa_role", "delete_varroa_role",
		"list_varroa_role_bindings", "create_varroa_role_binding",
		"list_jenkins_roles", "list_jenkins_role_bindings",
		"list_catalog_sources", "list_catalog_items",
		"list_composed_bundles", "validate_composed_bundle",
		"get_provisioning_defaults",
		"list_users", "list_groups",
		"list_activity", "search", "get_me", "get_my_permissions",
		"list_jenkins_controllers", "call_jenkins_tool",
	}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("expected tool %q not found", name)
		}
	}
}

func TestGetMeWithValidAuth(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)
	claims := &auth.Claims{
		Subject:           "test-subject",
		PreferredUsername: "testuser",
		Name:              "Test User",
		Email:             "test@example.com",
		Groups:            []string{"group1", "group2"},
	}

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_me",
		"arguments": map[string]interface{}{},
	}, claims)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("get_me returned tool error: %v", tr.Content)
	}

	var me struct {
		Subject           string   `json:"subject"`
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name"`
		Email             string   `json:"email"`
		Groups            []string `json:"groups"`
	}
	json.Unmarshal(tr.StructuredContent, &me)

	if me.Subject != "test-subject" {
		t.Errorf("expected subject 'test-subject', got %q", me.Subject)
	}
	if me.PreferredUsername != "testuser" {
		t.Errorf("expected preferred_username 'testuser', got %q", me.PreferredUsername)
	}
	if me.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", me.Email)
	}
	if len(me.Groups) != 2 {
		t.Errorf("expected 2 groups, got %d", len(me.Groups))
	}
}

func TestGetMeWithoutAuthReturnsError(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_me",
		"arguments": map[string]interface{}{},
	}, nil)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("expected isError for unauthenticated get_me")
	}
	if len(tr.Content) == 0 || tr.Content[0].Text != "authentication required" {
		t.Errorf("expected 'authentication required' error text, got %+v", tr.Content)
	}
}

func TestUnknownToolReturnsError(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "nonexistent_tool",
		"arguments": map[string]interface{}{},
	}, nil)

	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for unknown tool")
	}
	var errObj struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	json.Unmarshal(resp.Error, &errObj)
	if errObj.Code == 0 {
		t.Error("expected non-zero error code for unknown tool")
	}
}

func TestInvalidParamsError(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_controller",
		"arguments": map[string]interface{}{},
	}, nil)

	// With input schema validation enabled, missing required params should
	// produce a tool-level error with isError: true
	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatal("expected isError for missing required params")
	}
}

// Without an Authorizer wired there is no RBAC filter to apply, so search
// succeeds unauthenticated and an empty store yields no hits. (With an
// Authorizer, claims are mandatory — see TestSearchRequiresClaimsWithAuthorizer.)
func TestSearchWithoutClaimsSucceeds(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name": "search",
		"arguments": map[string]interface{}{
			"query": "test",
		},
	}, nil)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("search returned error: %v", tr.Content)
	}
}

// Without a Backfill source (activity subsystem not wired), list_activity
// returns an empty feed rather than an error — even unauthenticated.
func TestListActivityNoBackfillReturnsEmpty(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_activity",
		"arguments": map[string]interface{}{},
	}, nil)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_activity returned error: %v", tr.Content)
	}
}

func TestJenkinsProxyURLConstruction(t *testing.T) {
	tests := []struct {
		name      string
		uid       string
		crName    string
		namespace string
		expected  string
	}{
		{
			name:      "short UID",
			uid:       "abc12345",
			crName:    "mycontroller",
			namespace: "jenkins",
			expected:  "http://mycontroller-abc12345-svc.jenkins.svc.cluster.local:8080",
		},
		{
			name:      "long UID truncated to 8 chars",
			uid:       "abc123456789",
			crName:    "mycontroller",
			namespace: "jenkins",
			expected:  "http://mycontroller-abc12345-svc.jenkins.svc.cluster.local:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildJenkinsServiceURL(&v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{
					UID:       types.UID(tt.uid),
					Name:      tt.crName,
					Namespace: tt.namespace,
				},
			})
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestJSONRPCEnvelopeConstruction(t *testing.T) {
	body, err := buildJSONRPCEnvelope("tools/list", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var env map[string]interface{}
	json.Unmarshal(body, &env)

	if env["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc 2.0, got %v", env["jsonrpc"])
	}
	if env["method"] != "tools/list" {
		t.Errorf("expected method tools/list, got %v", env["method"])
	}
	if _, ok := env["params"]; ok {
		t.Error("expected params to be absent when nil")
	}

	body, _ = buildJSONRPCEnvelope("tools/call", map[string]string{"name": "test"})
	json.Unmarshal(body, &env)
	if env["params"] == nil {
		t.Error("expected params to be present")
	}
}

func TestStreamableHTTPServerInit(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	if handler == nil {
		t.Fatal("expected non-nil handler")
	}

	_, ok := handler.(*server.StreamableHTTPServer)
	if !ok {
		t.Fatalf("expected *server.StreamableHTTPServer, got %T", handler)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestListControllersReturnsSuccess(t *testing.T) {
	deps := newTestDeps()
	handler := NewHandler(deps)

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "list_controllers",
		"arguments": map[string]interface{}{},
	}, nil)

	tr := parseToolResult(t, resp.Result)
	if tr.IsError {
		t.Fatalf("list_controllers returned error: %v", tr.Content)
	}
}

// get_controller_logs reads pod logs through deps.Client, which is optional and
// nil on an incompletely-wired server. Its authorizer check is deliberately
// conditional, so nothing upstream stops a nil Client reaching the log read
// (#472). Store is present here to isolate Client as the only missing piece.
func TestGetControllerLogs_ClientNilReturnsError(t *testing.T) {
	handler := NewHandler(&api.Dependencies{Store: crdstore.NewFake()})

	resp := mcpRequest(t, handler, "tools/call", map[string]interface{}{
		"name":      "get_controller_logs",
		"arguments": map[string]interface{}{"namespace": "ns", "name": "c1"},
	}, nil)

	tr := parseToolResult(t, resp.Result)
	if !tr.IsError {
		t.Fatalf("get_controller_logs with nil Client must return a tool error, got success: %v", tr.Content)
	}
	if got := toolText(tr); got != "kubernetes client not configured" {
		t.Errorf("error text = %q, want %q", got, "kubernetes client not configured")
	}
}

func (c *stubClient) CreateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (c *stubClient) UpdateComposedBundleCRD(_ context.Context, _ *v1alpha1.ComposedBundle) error {
	return nil
}
func (c *stubClient) CreateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (c *stubClient) UpdateCatalogSourceCRD(_ context.Context, _ *v1alpha1.CatalogSource) error {
	return nil
}
func (c *stubClient) CreateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (c *stubClient) UpdateJenkinsRoleCRD(_ context.Context, _ *v1alpha1.JenkinsRole) error {
	return nil
}
func (c *stubClient) CreateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}
func (c *stubClient) UpdateJenkinsRoleBindingCRD(_ context.Context, _ *v1alpha1.JenkinsRoleBinding) error {
	return nil
}

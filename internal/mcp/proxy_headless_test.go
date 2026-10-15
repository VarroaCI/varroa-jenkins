package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// readOnlyAuthorizer grants controllers:read but not break-glass, unlike
// guardAuthorizer's wildcard grant. It proves the read verb alone does not
// unlock a headless controller's admin door.
func readOnlyAuthorizer() *api.Authorizer {
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "read-only"},
		Spec: v1alpha1.VarroaRoleSpec{APIRules: []v1alpha1.APIRule{
			{Resources: []string{"controllers"}, Verbs: []string{"read"}},
		}},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "read-only-binding"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "Group", Name: "readers"}},
			RoleRef:  "read-only",
		},
	}}
	return api.NewAuthorizer(rbac.NewTestResolverWithRoles(roles, bindings), false)
}

var readOnlyClaims = &auth.Claims{Subject: "reader", Groups: []string{"readers"}}

func headlessController() *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "jenkins"},
		Spec:       v1alpha1.ControllerSpec{Headless: true},
		Status:     v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{Connected: true}},
	}
}

// TestCallJenkinsTool_DeclaresOptionalReasonArg is the schema test for task
// 9.1: the reason argument exists, is a string, and is never required, so a
// non-headless caller who never supplies one still validates.
func TestCallJenkinsTool_DeclaresOptionalReasonArg(t *testing.T) {
	for _, tool := range liveTools(t) {
		if tool.Name != "call_jenkins_tool" {
			continue
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("unmarshal inputSchema: %v", err)
		}
		prop, ok := schema.Properties["reason"].(map[string]any)
		if !ok {
			t.Fatal("call_jenkins_tool does not declare reason")
		}
		if prop["type"] != "string" {
			t.Errorf("reason type = %v, want string", prop["type"])
		}
		for _, r := range schema.Required {
			if r == "reason" {
				t.Error("reason must not be required: it stays optional for a non-headless controller")
			}
		}
		return
	}
	t.Fatal("call_jenkins_tool not found")
}

// TestCallJenkinsTool_HeadlessRefusals covers task 9.2's four refusal
// conditions. Each must fail before the mint: no upstream HTTP request and no
// audit event.
func TestCallJenkinsTool_HeadlessRefusals(t *testing.T) {
	tests := []struct {
		name       string
		authorizer *api.Authorizer
		claims     *auth.Claims
		reason     string
		noSink     bool
		want       string
	}{
		{
			name:       "caller lacks break-glass",
			authorizer: readOnlyAuthorizer(),
			claims:     readOnlyClaims,
			reason:     "investigating a stuck build",
			want:       "access denied: controller is headless",
		},
		{
			name:       "reason is empty",
			authorizer: guardAuthorizer(),
			claims:     guardClaims,
			reason:     "",
			want:       "reason is required for a headless controller",
		},
		{
			name:       "reason exceeds 256 characters",
			authorizer: guardAuthorizer(),
			claims:     guardClaims,
			reason:     strings.Repeat("a", 257),
			want:       "reason must be at most 256 characters",
		},
		{
			name:       "activity publisher not configured",
			authorizer: guardAuthorizer(),
			claims:     guardClaims,
			reason:     "investigating a stuck build",
			noSink:     true,
			want:       "break-glass auditing is unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCount := 0
			setProxyTransport(t, func(*http.Request) (*http.Response, error) {
				requestCount++
				return testHTTPResponse(http.StatusInternalServerError, "text/plain", "unexpected", nil), nil
			})

			ctrl := headlessController()
			store := crdstore.NewFake()
			crdstore.MustSeed(store, ctrl)
			sink := &recordingSink{}
			deps := &api.Dependencies{
				Store:              store,
				Authorizer:         tt.authorizer,
				JenkinsTokenSigner: mustMiteSigner(t),
			}
			if !tt.noSink {
				deps.ActivityPublisher = sink
			}

			resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
				"name": "call_jenkins_tool",
				"arguments": map[string]interface{}{
					"namespace": "jenkins", "name": "controller", "method": "tools/list",
					"reason": tt.reason,
				},
			}, tt.claims)

			result := parseToolResult(t, resp.Result)
			if !result.IsError || len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, tt.want) {
				t.Fatalf("got tool result %+v, want error containing %q", result, tt.want)
			}
			if requestCount != 0 {
				t.Fatalf("expected zero upstream requests, got %d", requestCount)
			}
			if len(sink.events) != 0 {
				t.Fatalf("expected no audit event on refusal, got %d", len(sink.events))
			}
		})
	}
}

// TestCallJenkinsTool_HeadlessAllowedPublishesGrant covers task 9.3: an
// allowed call against a headless controller publishes
// controller.breakglass.granted, sourced "mcp", carrying the caller's reason.
func TestCallJenkinsTool_HeadlessAllowedPublishesGrant(t *testing.T) {
	requestCount := 0
	setProxyTransport(t, func(req *http.Request) (*http.Response, error) {
		requestCount++
		if req.Method == http.MethodDelete {
			return testHTTPResponse(http.StatusNoContent, "", "", nil), nil
		}
		if requestCount == 1 {
			return testHTTPResponse(http.StatusOK, "application/json", `{}`, map[string]string{"Mcp-Session-Id": "sess-1"}), nil
		}
		return testHTTPResponse(http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":1,"result":{}}`, nil), nil
	})

	ctrl := headlessController()
	store := crdstore.NewFake()
	crdstore.MustSeed(store, ctrl)
	sink := &recordingSink{}
	deps := &api.Dependencies{
		Store:              store,
		Authorizer:         guardAuthorizer(),
		JenkinsTokenSigner: mustMiteSigner(t),
		ActivityPublisher:  sink,
	}

	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name": "call_jenkins_tool",
		"arguments": map[string]interface{}{
			"namespace": "jenkins", "name": "controller", "method": "tools/list",
			"reason": "investigating a stuck build",
		},
	}, guardClaims)

	result := parseToolResult(t, resp.Result)
	if result.IsError {
		t.Fatalf("allowed headless call was refused: %+v", result.Content)
	}
	if len(sink.events) != 1 {
		t.Fatalf("got %d audit events, want 1", len(sink.events))
	}
	e := sink.events[0]
	if e.Type != "controller.breakglass.granted" {
		t.Errorf("Type = %q, want controller.breakglass.granted", e.Type)
	}
	if e.Source != "mcp" {
		t.Errorf("Source = %q, want mcp", e.Source)
	}
	if e.Reason != "investigating a stuck build" {
		t.Errorf("Reason = %q, want the caller's reason", e.Reason)
	}
	if e.Namespace != "jenkins" || e.Controller != "controller" {
		t.Errorf("event target = %s/%s, want jenkins/controller", e.Namespace, e.Controller)
	}
}

// TestCallJenkinsTool_NonHeadlessUnaffected is the D10 scenario "A controller
// that is not headless is unaffected": no break-glass verb and no reason are
// required, and nothing is audited.
func TestCallJenkinsTool_NonHeadlessUnaffected(t *testing.T) {
	requestCount := 0
	setProxyTransport(t, func(req *http.Request) (*http.Response, error) {
		requestCount++
		if req.Method == http.MethodDelete {
			return testHTTPResponse(http.StatusNoContent, "", "", nil), nil
		}
		if requestCount == 1 {
			return testHTTPResponse(http.StatusOK, "application/json", `{}`, map[string]string{"Mcp-Session-Id": "sess-1"}), nil
		}
		return testHTTPResponse(http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":1,"result":{}}`, nil), nil
	})

	ctrl := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "jenkins"},
		Status:     v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{Connected: true}},
	}
	store := crdstore.NewFake()
	crdstore.MustSeed(store, ctrl)
	sink := &recordingSink{}
	deps := &api.Dependencies{
		Store:              store,
		Authorizer:         readOnlyAuthorizer(),
		JenkinsTokenSigner: mustMiteSigner(t),
		ActivityPublisher:  sink,
	}

	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name": "call_jenkins_tool",
		"arguments": map[string]interface{}{
			"namespace": "jenkins", "name": "controller", "method": "tools/list",
		},
	}, readOnlyClaims)

	result := parseToolResult(t, resp.Result)
	if result.IsError {
		t.Fatalf("non-headless call with no reason and no break-glass verb was refused: %+v", result.Content)
	}
	if len(sink.events) != 0 {
		t.Fatalf("non-headless call must not be audited, got %d events", len(sink.events))
	}
}

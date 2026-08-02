package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/mite"
)

type proxyTestClient struct {
	stubClient
	controller *v1alpha1.Controller
	lookups    int
}

func (c *proxyTestClient) GetControllerCRD(_ context.Context, _, _ string) (*v1alpha1.Controller, error) {
	c.lookups++
	return c.controller, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCallJenkinsToolSessionHandshake(t *testing.T) {
	wantResponse := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	var requests []*http.Request
	setProxyTransport(t, func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		if req.URL.Path != "/mcp-server/mcp" {
			t.Errorf("expected plugin endpoint path, got %q", req.URL.Path)
		}
		if got := req.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("expected streamable HTTP Accept header, got %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected JSON Content-Type, got %q", got)
		}
		switch len(requests) {
		case 1:
			if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("initialize request missing Bearer authorization: %q", got)
			}
			if req.Method != http.MethodPost || req.Header.Get("Mcp-Session-Id") != "" {
				t.Errorf("initialize request had method %q and session %q", req.Method, req.Header.Get("Mcp-Session-Id"))
			}
			assertInitializeHandshake(t, req)
			return testHTTPResponse(http.StatusOK, "application/json", `{}`, map[string]string{"Mcp-Session-Id": "sess-1"}), nil
		case 2:
			if got := req.Header.Get("Authorization"); got != requests[0].Header.Get("Authorization") {
				t.Errorf("forwarded request authorization %q does not match initialize %q", got, requests[0].Header.Get("Authorization"))
			}
			if req.Method != http.MethodPost || req.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Errorf("forwarded request had method %q and session %q", req.Method, req.Header.Get("Mcp-Session-Id"))
			}
			assertRequestMethod(t, req, "tools/list")
			return testHTTPResponse(http.StatusOK, "application/json", wantResponse, nil), nil
		case 3:
			if got := req.Header.Get("Authorization"); got != requests[0].Header.Get("Authorization") {
				t.Errorf("cleanup request authorization %q does not match initialize %q", got, requests[0].Header.Get("Authorization"))
			}
			if req.Method != http.MethodDelete || req.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Errorf("cleanup request had method %q and session %q", req.Method, req.Header.Get("Mcp-Session-Id"))
			}
			return testHTTPResponse(http.StatusNoContent, "", "", nil), nil
		default:
			t.Fatalf("unexpected request %d", len(requests))
			return nil, nil
		}
	})

	signer, err := mite.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	store := crdstore.NewFake()
	ctrl := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "jenkins"},
		Status:     v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{Connected: true}},
	}
	crdstore.MustSeed(store, ctrl)
	deps := &api.Dependencies{Client: &proxyTestClient{controller: ctrl}, Store: store, Authorizer: guardAuthorizer(), JenkinsTokenSigner: signer}
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name": "call_jenkins_tool",
		"arguments": map[string]interface{}{
			"namespace": "jenkins",
			"name":      "controller",
			"method":    "tools/list",
		},
	}, guardClaims)

	result := parseToolResult(t, resp.Result)
	if result.IsError {
		t.Fatalf("call_jenkins_tool returned error: %+v", result.Content)
	}
	assertJSONEqual(t, result.StructuredContent, []byte(wantResponse))
	if len(requests) != 3 {
		t.Fatalf("expected initialize, forwarded call, and cleanup; got %d requests", len(requests))
	}
}

func TestCallJenkinsToolReadOnlyCallerCanForwardToolsCall(t *testing.T) {
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
	signer, err := mite.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	store := crdstore.NewFake()
	ctrl := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "jenkins"},
		Status:     v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{Connected: true}},
	}
	crdstore.MustSeed(store, ctrl)
	deps := &api.Dependencies{
		Client:             &proxyTestClient{controller: ctrl},
		Store:              store,
		Authorizer:         guardAuthorizer(),
		JenkinsTokenSigner: signer,
	}
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name": "call_jenkins_tool",
		"arguments": map[string]interface{}{
			"namespace": "jenkins", "name": "controller", "method": "tools/call",
		},
	}, guardClaims)
	result := parseToolResult(t, resp.Result)
	if result.IsError {
		t.Fatalf("read-only caller was denied: %+v", result.Content)
	}
	if requestCount != 3 {
		t.Fatalf("expected session requests, got %d", requestCount)
	}
}

func TestCallJenkinsToolVisibilityAndFailClosedGuards(t *testing.T) {
	tests := []struct {
		name       string
		deps       func(*proxyTestClient) *api.Dependencies
		claims     *auth.Claims
		want       string
		wantLookup bool
	}{
		{
			name:   "missing claims",
			deps:   func(c *proxyTestClient) *api.Dependencies { return &api.Dependencies{Client: c} },
			want:   "authentication required",
			claims: nil,
		},
		{
			name: "nil authorizer",
			deps: func(c *proxyTestClient) *api.Dependencies {
				return &api.Dependencies{Client: c, JenkinsTokenSigner: mustMiteSigner(t)}
			},
			claims: guardClaims,
			want:   "authorization unavailable",
		},
		{
			name: "not visible",
			deps: func(c *proxyTestClient) *api.Dependencies {
				return &api.Dependencies{Client: c, Authorizer: guardAuthorizer(), JenkinsTokenSigner: mustMiteSigner(t)}
			},
			claims: &auth.Claims{Subject: "not-admin"},
			want:   "access denied: missing controllers:read permission",
		},
		{
			name: "nil signer",
			deps: func(c *proxyTestClient) *api.Dependencies {
				return &api.Dependencies{Client: c, Authorizer: guardAuthorizer()}
			},
			claims:     guardClaims,
			want:       "per-caller identity unavailable: mite signing key not loaded",
			wantLookup: false, // lookup now goes through crdstore, not client
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCount := 0
			setProxyTransport(t, func(*http.Request) (*http.Response, error) {
				requestCount++
				return testHTTPResponse(http.StatusInternalServerError, "text/plain", "unexpected", nil), nil
			})
			ctrl := &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "jenkins"},
				Status:     v1alpha1.ControllerStatus{MiteStatus: &v1alpha1.MiteStatus{Connected: true}},
			}
			client := &proxyTestClient{controller: ctrl}
			deps := tt.deps(client)
			if deps.Store == nil {
				store := crdstore.NewFake()
				crdstore.MustSeed(store, ctrl)
				deps.Store = store
			}
			resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
				"name": "call_jenkins_tool",
				"arguments": map[string]interface{}{
					"namespace": "jenkins", "name": "controller", "method": "tools/list",
				},
			}, tt.claims)
			result := parseToolResult(t, resp.Result)
			if !result.IsError || len(result.Content) == 0 || result.Content[0].Text != tt.want {
				t.Fatalf("got tool result %+v, want %q", result, tt.want)
			}
			if requestCount != 0 {
				t.Fatalf("expected zero upstream requests, got %d", requestCount)
			}
			if (client.lookups > 0) != tt.wantLookup {
				t.Fatalf("controller lookup presence=%t, want %t", client.lookups > 0, tt.wantLookup)
			}
		})
	}
}

func mustMiteSigner(t *testing.T) *mite.MiteTokenSigner {
	t.Helper()
	signer, err := mite.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	return signer
}

func TestCallJenkinsMCPInitializeHandshakeErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		wantErr string
	}{
		{
			name:    "missing session ID",
			status:  http.StatusOK,
			wantErr: "jenkins mcp endpoint returned no session id",
		},
		{
			name:    "forbidden",
			status:  http.StatusForbidden,
			body:    "<html>forbidden</html>",
			wantErr: "initialize handshake failed: jenkins returned HTTP 403",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setProxyTransport(t, func(_ *http.Request) (*http.Response, error) {
				return testHTTPResponse(tt.status, "text/html", tt.body, tt.headers), nil
			})
			_, err := callJenkinsMCP(context.Background(), "http://jenkins", "tools/list", nil, "")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestCallJenkinsMCPDeleteFailureDoesNotAffectResult(t *testing.T) {
	want := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	requestCount := 0
	setProxyTransport(t, func(req *http.Request) (*http.Response, error) {
		requestCount++
		switch req.Method {
		case http.MethodDelete:
			return nil, errors.New("cleanup failed")
		default:
			if requestCount == 1 {
				return testHTTPResponse(http.StatusOK, "application/json", `{}`, map[string]string{"Mcp-Session-Id": "sess-1"}), nil
			}
			return testHTTPResponse(http.StatusOK, "application/json", string(want), nil), nil
		}
	})

	got, err := callJenkinsMCP(context.Background(), "http://jenkins", "tools/list", nil, "")
	if err != nil {
		t.Fatalf("cleanup failure affected result: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	assertJSONEqual(t, gotJSON, want)
	if requestCount != 3 {
		t.Fatalf("expected three requests, got %d", requestCount)
	}
}

func TestCallJenkinsMCPInitializePassthrough(t *testing.T) {
	want := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
	requestCount := 0
	setProxyTransport(t, func(req *http.Request) (*http.Response, error) {
		requestCount++
		if req.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", req.Method)
		}
		if got := req.Header.Get("Mcp-Session-Id"); got != "" {
			t.Errorf("expected no session header, got %q", got)
		}
		assertRequestMethod(t, req, "initialize")
		return testHTTPResponse(http.StatusOK, "application/json", string(want), nil), nil
	})

	got, err := callJenkinsMCP(context.Background(), "http://jenkins", "initialize", map[string]interface{}{"test": true}, "")
	if err != nil {
		t.Fatalf("initialize passthrough failed: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	assertJSONEqual(t, gotJSON, want)
	if requestCount != 1 {
		t.Fatalf("expected exactly one request, got %d", requestCount)
	}
}

func TestDecodeMCPResponse(t *testing.T) {
	want := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantErr     string
	}{
		{
			name:        "application JSON",
			status:      http.StatusOK,
			contentType: "application/json; charset=utf-8",
			body:        string(want),
		},
		{
			name:        "event stream",
			status:      http.StatusOK,
			contentType: "text/event-stream",
			body:        "event: message\ndata: " + string(want) + "\n\n",
		},
		{
			name:        "multiline event data",
			status:      http.StatusOK,
			contentType: "text/event-stream",
			body:        "data: {\"jsonrpc\":\"2.0\",\"id\":1,\ndata: \"result\":{\"ok\":true}}\n\n",
		},
		{
			name:        "mixed-case content type",
			status:      http.StatusOK,
			contentType: "Application/JSON; charset=UTF-8",
			body:        string(want),
		},
		{
			name:        "event stream with CRLF line endings",
			status:      http.StatusOK,
			contentType: "Text/Event-Stream",
			body:        "event: message\r\ndata: " + string(want) + "\r\n\r\n",
		},
		{
			name:        "non-2xx response",
			status:      http.StatusForbidden,
			contentType: "text/html",
			body:        "<html>forbidden</html>",
			wantErr:     "403",
		},
		{
			name:        "event stream without JSON-RPC response",
			status:      http.StatusOK,
			contentType: "text/event-stream",
			body:        "event: message\ndata: {\"result\":{\"ok\":true}}\n\n",
			wantErr:     "no JSON-RPC response in event stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			recorder.Header().Set("Content-Type", tt.contentType)
			recorder.WriteHeader(tt.status)
			_, _ = recorder.Write([]byte(tt.body))
			resp := recorder.Result()
			defer resp.Body.Close()

			got, err := decodeMCPResponse(resp)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode response: %v", err)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal decoded response: %v", err)
			}
			assertJSONEqual(t, gotJSON, want)
		})
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue interface{}
	var wantValue interface{}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode actual JSON: %v", err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	gotCanonical, _ := json.Marshal(gotValue)
	wantCanonical, _ := json.Marshal(wantValue)
	if string(gotCanonical) != string(wantCanonical) {
		t.Errorf("JSON mismatch: got %s, want %s", gotCanonical, wantCanonical)
	}
}

func setProxyTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	originalClient := proxyHTTPClient
	proxyHTTPClient = &http.Client{Transport: fn}
	t.Cleanup(func() { proxyHTTPClient = originalClient })
}

func testHTTPResponse(status int, contentType, body string, headers map[string]string) *http.Response {
	recorder := httptest.NewRecorder()
	if contentType != "" {
		recorder.Header().Set("Content-Type", contentType)
	}
	for name, value := range headers {
		recorder.Header().Set(name, value)
	}
	recorder.WriteHeader(status)
	_, _ = recorder.WriteString(body)
	return recorder.Result()
}

func assertRequestMethod(t *testing.T, req *http.Request, want string) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if envelope.Method != want {
		t.Errorf("expected JSON-RPC method %q, got %q", want, envelope.Method)
	}
}

func assertInitializeHandshake(t *testing.T, req *http.Request) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read initialize body: %v", err)
	}
	want := []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"varroa-bff","version":"dev"}}}`)
	assertJSONEqual(t, body, want)
}

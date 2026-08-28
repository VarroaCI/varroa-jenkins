//nolint:nilerr // NewToolResultError encodes errors in the result, not the Go error return
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func registerProxyTools(mcpServer *server.MCPServer, deps *api.Dependencies) {
	listJenkinsCtlrsTool := mcp.NewTool("list_jenkins_controllers",
		mcp.WithDescription("List all Jenkins controllers managed by Varroa with their reachable status"),
		mcp.WithString("namespace",
			mcp.Description("Optional namespace to filter by"),
		),
	)
	addTool(mcpServer, kindRead, listJenkinsCtlrsTool, handleListJenkinsControllers(deps))

	callJenkinsToolTool := mcp.NewTool("call_jenkins_tool",
		mcp.WithDescription("Forward an MCP JSON-RPC call to a Jenkins controller managed by Varroa"),
		mcp.WithString("namespace",
			mcp.Required(),
			mcp.Description("Controller namespace"),
		),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Controller name"),
		),
		mcp.WithString("method",
			mcp.Required(),
			mcp.Description("JSON-RPC method to call on Jenkins (e.g. 'tools/list', 'tools/call')"),
		),
		mcp.WithObject("params",
			mcp.Description("JSON-RPC params payload"),
		),
	)
	addTool(mcpServer, kindProxy, callJenkinsToolTool, handleCallJenkinsTool(deps))
}

func handleListJenkinsControllers(deps *api.Dependencies) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		namespace, _ := getArgs(req)["namespace"].(string)

		controllers, err := crdstore.List[v1alpha1.Controller](ctx, deps.Store, namespace, "")
		if err != nil {
			return mcp.NewToolResultError("failed to list controllers: " + err.Error()), nil
		}

		if deps.Authorizer != nil {
			if claims, _ := requireClaims(ctx); claims != nil {
				filtered := make([]*v1alpha1.Controller, 0)
				for _, c := range controllers {
					if deps.Authorizer.CanReadController(claims, c.Namespace, c.Name) {
						filtered = append(filtered, c)
					}
				}
				controllers = filtered
			}
		}

		type controllerEntry struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Reachable bool   `json:"reachable"`
		}

		var results []controllerEntry
		for _, cr := range controllers {
			results = append(results, controllerEntry{
				Namespace: cr.Namespace,
				Name:      cr.Name,
				Reachable: cr.Status.MiteStatus.Connected,
			})
		}

		return resultJSON(results)
	}
}

var proxyHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
}

func handleCallJenkinsTool(deps *api.Dependencies) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		namespace, _ := getArgs(req)["namespace"].(string)
		name, _ := getArgs(req)["name"].(string)
		method, _ := getArgs(req)["method"].(string)
		var params interface{}
		if p, ok := getArgs(req)["params"]; ok {
			params = p
		}

		claims, err := requireClaims(ctx)
		if err != nil {
			return mcp.NewToolResultError("authentication required"), nil
		}

		if namespace == "" || name == "" || method == "" {
			return mcp.NewToolResultError("namespace, name, and method are required"), nil
		}

		if deps.Authorizer == nil {
			return mcp.NewToolResultError("authorization unavailable"), nil
		}
		if !deps.Authorizer.CanReadController(claims, namespace, name) {
			return mcp.NewToolResultError("access denied: missing controllers:read permission"), nil
		}

		cr, err := crdstore.Get[v1alpha1.Controller](ctx, deps.Store, name, namespace)
		if err != nil {
			return mcp.NewToolResultError("controller not found: " + err.Error()), nil
		}

		if !cr.Status.MiteStatus.Connected {
			return mcp.NewToolResultError("controller is not reachable: mite not connected"), nil
		}
		if deps.JenkinsTokenSigner == nil {
			return mcp.NewToolResultError("per-caller identity unavailable: mite signing key not loaded"), nil
		}
		token, err := deps.JenkinsTokenSigner.GenerateUserJenkinsToken(name, namespace, claims, 5*time.Minute)
		if err != nil {
			return mcp.NewToolResultError("failed to mint per-caller identity: " + err.Error()), nil
		}

		targetURL := buildJenkinsServiceURL(cr)
		result, err := callJenkinsMCP(ctx, targetURL, method, params, token)
		if err != nil {
			return mcp.NewToolResultError("Jenkins MCP call failed: " + err.Error()), nil
		}

		return resultJSON(result)
	}
}

func callJenkinsMCP(ctx context.Context, targetURL, method string, params interface{}, token string) (interface{}, error) {
	endpoint := targetURL + "/mcp-server/mcp"
	if method == "initialize" {
		return postJenkinsMCP(ctx, endpoint, method, params, "", token)
	}

	initParams := map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "varroa-bff",
			"version": "dev",
		},
	}
	initBody, err := buildJSONRPCEnvelopeWithID(0, "initialize", initParams)
	if err != nil {
		return nil, fmt.Errorf("initialize handshake failed: build request: %w", err)
	}
	initReq, err := newJenkinsMCPRequest(ctx, http.MethodPost, endpoint, initBody, "", token)
	if err != nil {
		return nil, fmt.Errorf("initialize handshake failed: create request: %w", err)
	}
	initResp, err := proxyHTTPClient.Do(initReq)
	if err != nil {
		return nil, fmt.Errorf("initialize handshake failed: %w", err)
	}
	statusErr := validateMCPResponseStatus(initResp)
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	_, _ = io.Copy(io.Discard, initResp.Body)
	_ = initResp.Body.Close()
	if statusErr != nil {
		return nil, fmt.Errorf("initialize handshake failed: %w", statusErr)
	}
	if sessionID == "" {
		return nil, fmt.Errorf("initialize handshake failed: jenkins mcp endpoint returned no session id")
	}

	defer terminateJenkinsMCPSession(ctx, endpoint, sessionID, token)
	return postJenkinsMCP(ctx, endpoint, method, params, sessionID, token)
}

func postJenkinsMCP(ctx context.Context, endpoint, method string, params interface{}, sessionID, token string) (interface{}, error) {
	body, err := buildJSONRPCEnvelope(method, params)
	if err != nil {
		return nil, fmt.Errorf("forwarded call failed: build request: %w", err)
	}
	req, err := newJenkinsMCPRequest(ctx, http.MethodPost, endpoint, body, sessionID, token)
	if err != nil {
		return nil, fmt.Errorf("forwarded call failed: create request: %w", err)
	}
	resp, err := proxyHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forwarded call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	result, err := decodeMCPResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("forwarded call failed: %w", err)
	}
	return result, nil
}

func newJenkinsMCPRequest(ctx context.Context, method, endpoint string, body []byte, sessionID, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func terminateJenkinsMCPSession(ctx context.Context, endpoint, sessionID, token string) {
	req, err := newJenkinsMCPRequest(ctx, http.MethodDelete, endpoint, nil, sessionID, token)
	if err != nil {
		return
	}
	resp, err := proxyHTTPClient.Do(req)
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}

func validateMCPResponseStatus(resp *http.Response) error {
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return fmt.Errorf("jenkins returned HTTP %d (failed to read response body: %w)", resp.StatusCode, err)
	}
	return fmt.Errorf("jenkins returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func decodeMCPResponse(resp *http.Response) (interface{}, error) {
	if err := validateMCPResponseStatus(resp); err != nil {
		return nil, err
	}

	contentType := resp.Header.Get("Content-Type")
	normalizedType := strings.ToLower(contentType)
	switch {
	case strings.Contains(normalizedType, "application/json"):
		var result interface{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, err
		}
		return result, nil
	case strings.Contains(normalizedType, "text/event-stream"):
		return decodeMCPEventStream(resp.Body)
	default:
		return nil, fmt.Errorf("unsupported Jenkins response Content-Type %q", contentType)
	}
}

func decodeMCPEventStream(body io.Reader) (interface{}, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var dataLines []string

	decodeEvent := func() (interface{}, bool) {
		if len(dataLines) == 0 {
			return nil, false
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		var result interface{}
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, false
		}
		object, ok := result.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if _, ok := object["jsonrpc"]; !ok {
			return nil, false
		}
		return result, true
	}

	for scanner.Scan() {
		// SSE permits CRLF line endings; Scanner only strips the LF.
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if result, ok := decodeEvent(); ok {
				return result, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			dataLines = append(dataLines, strings.TrimPrefix(data, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read Jenkins event stream: %w", err)
	}
	if result, ok := decodeEvent(); ok {
		return result, nil
	}
	return nil, fmt.Errorf("no JSON-RPC response in event stream")
}

func buildJenkinsServiceURL(cr *v1alpha1.Controller) string {
	uid := string(cr.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	prefix := cr.Name + "-" + uid
	return fmt.Sprintf("http://%s-svc.%s.svc.cluster.local:8080", prefix, cr.Namespace)
}

func buildJSONRPCEnvelope(method string, params interface{}) ([]byte, error) {
	return buildJSONRPCEnvelopeWithID(1, method, params)
}

func buildJSONRPCEnvelopeWithID(id int, method string, params interface{}) ([]byte, error) {
	envelope := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		envelope["params"] = params
	}
	return json.Marshal(envelope)
}

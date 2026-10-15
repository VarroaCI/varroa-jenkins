package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// runnerDoorPrefix is the host-root path the door serves, outside /api/v1
// and outside auth (AuthMiddleware bypasses every non-/api/ path, the same
// property the hibernation webhook door already relies on).
const runnerDoorPrefix = "/runners/"

// runnerDoorOperatorTokenTTL is the operator Jenkins token lifetime for
// one agent-secret fetch, one agent.jar stream, or one websocket upgrade
// (design D8, mirrors D10's probeTokenTTL).
const runnerDoorOperatorTokenTTL = 60 * time.Second

// runnerDoorHandshakeRequestHeaders are relayed unchanged from the client
// to the cell: the WebSocket handshake itself (design D8).
var runnerDoorHandshakeRequestHeaders = []string{
	"Upgrade",
	"Connection",
	"Sec-WebSocket-Key",
	"Sec-WebSocket-Version",
	"Sec-WebSocket-Protocol",
	"Sec-WebSocket-Extensions",
}

// runnerDoorHandshakeResponseHeaders are relayed unchanged from the cell
// to the client: the WebSocket handshake response (design D8).
var runnerDoorHandshakeResponseHeaders = []string{
	"Upgrade",
	"Connection",
	"Sec-WebSocket-Accept",
	"Sec-WebSocket-Protocol",
	"Sec-WebSocket-Extensions",
}

// runnerDoorRequestCopyHeaders are copied from the client to the cell
// unchanged, alongside the door's own Node-Name and Secret-Key (design D8).
var runnerDoorRequestCopyHeaders = []string{"X-Remoting-Capability", "Connection-Cookie"}

// runnerDoorResponseCopyHeaders are copied from the cell to the client
// unchanged (design D8).
var runnerDoorResponseCopyHeaders = []string{"X-Remoting-Capability", "X-Remoting-Minimum-Version", "Connection-Cookie"}

// runnerDoorHTTPClient serves the agent-secret fetch and the agent.jar
// stream. The websocket upgrade goes through httputil.ReverseProxy instead,
// which manages its own transport.
var runnerDoorHTTPClient = &http.Client{Timeout: 30 * time.Second}

// HandleRunnerDoor serves the agent WebSocket door (design D7, D8;
// specs/agent-websocket-door). It is registered at the host root
// (/runners/{token}/{path...}), beside HandleHibernationDispatch, outside
// the /api/v1 prefix and outside auth: the URL's token is the only
// credential.
//
//	GET /runners/{token}/login                 → 200 {"varroaRunnerDoor":true}
//	GET /runners/{token}/jnlpJars/agent.jar    → streamed proxy
//	GET /runners/{token}/wsagents/              → WebSocket upgrade proxy
//	any other                                   → 404
//
// The two odd paths are the Jenkins remoting engine's own contract, not a
// Varroa invention: it reads "login" relative to its base URL and refuses
// to continue unless the status is 200, then opens "wsagents/" relative to
// the same base URL.
func (s *Server) HandleRunnerDoor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}

	remain := strings.TrimPrefix(r.URL.Path, runnerDoorPrefix)
	token, action, cut := strings.Cut(remain, "/")
	if !cut || token == "" || action == "" {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	if action != "login" && action != "jnlpJars/agent.jar" && action != "wsagents/" {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}

	runner, ok := s.resolveRunnerDoorToken(w, r, token)
	if !ok {
		return
	}
	placement, ok := s.resolveRunnerDoorPlacement(w, r, runner)
	if !ok {
		return
	}

	switch action {
	case "login":
		s.writeJSON(w, http.StatusOK, map[string]bool{"varroaRunnerDoor": true})
	case "jnlpJars/agent.jar":
		cell, ok := s.resolveRunnerDoorCell(w, r, placement)
		if !ok {
			return
		}
		s.handleRunnerDoorAgentJar(w, r, cell)
	case "wsagents/":
		cell, ok := s.resolveRunnerDoorCell(w, r, placement)
		if !ok {
			return
		}
		s.handleRunnerDoorUpgrade(w, r, runner, cell)
	}
}

// resolveRunnerDoorToken finds the Runner the presented token names. It
// answers 401 with the standard body (writeWakeError, same bytes as the
// hibernation door) and returns ok=false for an unknown token, an empty
// stored token, or a runner whose launcher is ssh — an ssh runner has no
// door identity, so the door refuses it rather than proxy a connection
// Jenkins would reject. None of these causes has a side effect.
//
// The token index (RunnerTokenIndex) only narrows the search to a
// candidate name; the live Runner CRD's own status.token is the actual
// check, compared in constant time.
func (s *Server) resolveRunnerDoorToken(w http.ResponseWriter, r *http.Request, token string) (*v1alpha1.Runner, bool) {
	if s.deps.RunnerTokenIndex == nil {
		writeWakeError(w, http.StatusUnauthorized)
		return nil, false
	}
	name, ok := s.deps.RunnerTokenIndex.Resolve(token)
	if !ok {
		writeWakeError(w, http.StatusUnauthorized)
		return nil, false
	}
	runner, err := crdstore.Get[v1alpha1.Runner](r.Context(), s.deps.Store, name, "")
	if err != nil {
		writeWakeError(w, http.StatusUnauthorized)
		return nil, false
	}
	if runner.Status.Token == "" || !constantTimeEqual(token, runner.Status.Token) {
		writeWakeError(w, http.StatusUnauthorized)
		return nil, false
	}
	if runner.Spec.Launcher == "ssh" {
		writeWakeError(w, http.StatusUnauthorized)
		return nil, false
	}
	return runner, true
}

// resolveRunnerDoorPlacement reads the owning Team's status.placement and
// answers 503 when it is empty, so the agent's own retry loop backs off
// rather than the door failing the connection outright. The door reads no
// placement.generation: that field fences a ledger claim in C6, where a
// stale cell can still submit one; the door holds no long-lived placement
// copy and simply re-reads on every attempt.
func (s *Server) resolveRunnerDoorPlacement(w http.ResponseWriter, r *http.Request, runner *v1alpha1.Runner) (*v1alpha1.TeamPlacementStatus, bool) {
	ctx := r.Context()
	team, err := crdstore.Get[v1alpha1.Team](ctx, s.deps.Store, runner.Spec.Team, "")
	if err != nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	placement := team.Status.Placement
	if placement == nil || placement.Cluster == "" || placement.Cell == "" || placement.CellNamespace == "" {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	return placement, true
}

// resolveRunnerDoorCell resolves the cell Controller named by placement,
// through the BFF's cross-cluster reader. A cell name is unique only
// within one cluster, so the door reads placement.cluster and never reads
// the local store by name and namespace alone (task 8.3a).
func (s *Server) resolveRunnerDoorCell(w http.ResponseWriter, r *http.Request, placement *v1alpha1.TeamPlacementStatus) (*v1alpha1.Controller, bool) {
	if s.deps.Brood == nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	cell, err := s.deps.Brood.Get(r.Context(), placement.Cluster, placement.CellNamespace, placement.Cell)
	if err != nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	return cell, true
}

// cellServiceURL builds cell's in-cluster Jenkins Service URL, honoring
// the test seam runnerDoorServiceURL.
func (s *Server) cellServiceURL(cell *v1alpha1.Controller) string {
	if s.runnerDoorServiceURL != nil {
		return s.runnerDoorServiceURL(cell)
	}
	return BuildJenkinsServiceURL(cell)
}

// handleRunnerDoorAgentJar streams cell's own agent.jar to the client. It
// is a plain streamed GET with the operator token, not a proxy: the door
// must not relay the cell's response headers for a binary download
// (design D8). Proxying the cell's own jar keeps the remoting version of
// the jar and the cell in step.
func (s *Server) handleRunnerDoorAgentJar(w http.ResponseWriter, r *http.Request, cell *v1alpha1.Controller) {
	if s.deps.JenkinsTokenSigner == nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}
	token, err := s.deps.JenkinsTokenSigner.GenerateOperatorJenkinsToken(cell.Name, cell.Namespace, runnerDoorOperatorTokenTTL)
	if err != nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.cellServiceURL(cell)+"/jnlpJars/agent.jar", nil)
	if err != nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := runnerDoorHTTPClient.Do(req)
	if err != nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}

	_, _ = io.Copy(w, resp.Body)
}

// fetchRunnerAgentSecret reads cell's agent JNLP page for nodeName,
// authenticated with a 60-second operator Jenkins token, and returns the
// first <argument> element's text — the value core renders as
// ${it.jnlpMac}, JnlpAgentReceiver.SLAVE_SECRET.mac(nodeName) (design D8).
// It caches nothing: a cell roll changes the confidential store the
// secret comes from, and the door cannot observe that change.
func (s *Server) fetchRunnerAgentSecret(ctx context.Context, cell *v1alpha1.Controller, nodeName string) (string, error) {
	if s.deps.JenkinsTokenSigner == nil {
		return "", fmt.Errorf("jenkins token signer unavailable")
	}
	token, err := s.deps.JenkinsTokenSigner.GenerateOperatorJenkinsToken(cell.Name, cell.Namespace, runnerDoorOperatorTokenTTL)
	if err != nil {
		return "", fmt.Errorf("mint operator jenkins token: %w", err)
	}

	jnlpURL := s.cellServiceURL(cell) + "/computer/" + nodeName + "/jenkins-agent.jnlp"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jnlpURL, nil)
	if err != nil {
		return "", fmt.Errorf("build agent jnlp request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := runnerDoorHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch agent jnlp: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch agent jnlp for %s: status %d", nodeName, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read agent jnlp body: %w", err)
	}
	secret, ok := firstJNLPArgument(body)
	if !ok {
		return "", fmt.Errorf("agent jnlp for %s carries no non-empty argument element", nodeName)
	}
	return secret, nil
}

// firstJNLPArgument returns the text of the first <argument> element in
// body, the shape core's agent JNLP page renders. An empty or
// whitespace-only element is not a secret and reports ok false, so the
// caller treats it as a fetch failure rather than forwarding an empty
// Secret-Key to the cell.
func firstJNLPArgument(body []byte) (string, bool) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "argument" {
			continue
		}
		var value string
		if err := dec.DecodeElement(&value, &start); err != nil {
			return "", false
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return "", false
		}
		return value, true
	}
}

// handleRunnerDoorUpgrade proxies the WebSocket upgrade to cell's
// /wsagents/ endpoint (design D7, D8). It fetches the current agent
// secret first, then builds an httputil.ReverseProxy whose Rewrite hook
// applies the request-side header table and whose ModifyResponse hook
// applies the response-side table — Rewrite mutates only the outbound
// request, so the response allowlist needs its own hook, and ReverseProxy
// calls ModifyResponse before it handles a 101 switch, so the hook runs on
// an upgrade. FlushInterval -1 disables response buffering.
func (s *Server) handleRunnerDoorUpgrade(w http.ResponseWriter, r *http.Request, runner *v1alpha1.Runner, cell *v1alpha1.Controller) {
	secret, err := s.fetchRunnerAgentSecret(r.Context(), cell, runner.Status.NodeName)
	if err != nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}

	target, err := url.Parse(s.cellServiceURL(cell))
	if err != nil {
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}

	nodeName := runner.Status.NodeName
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = "/wsagents/"
			pr.Out.URL.RawQuery = ""
			pr.Out.Host = target.Host

			out := make(http.Header)
			for _, h := range runnerDoorHandshakeRequestHeaders {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					out[textproto.CanonicalMIMEHeaderKey(h)] = v
				}
			}
			for _, h := range runnerDoorRequestCopyHeaders {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					out[textproto.CanonicalMIMEHeaderKey(h)] = v
				}
			}
			// Node-Name and Secret-Key are never taken from the client: a
			// runner cannot name a node it does not own (design D8's
			// security property), and it never holds the door's secret.
			out.Set("Node-Name", nodeName)
			out.Set("Secret-Key", secret)
			pr.Out.Header = out
		},
		ModifyResponse: func(resp *http.Response) error {
			filterHeaders(resp.Header, runnerDoorHandshakeResponseHeaders, runnerDoorResponseCopyHeaders)
			return nil
		},
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

// filterHeaders drops every header from h not named in one of the given
// allowlists.
func filterHeaders(h http.Header, allowlists ...[]string) {
	allow := make(map[string]bool)
	for _, list := range allowlists {
		for _, name := range list {
			allow[textproto.CanonicalMIMEHeaderKey(name)] = true
		}
	}
	for name := range h {
		if !allow[textproto.CanonicalMIMEHeaderKey(name)] {
			h.Del(name)
		}
	}
}

package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/hibernation"
)

// HeaderAllowlist is the set of headers forwarded from the original webhook
// into the envelope. Matches D7.
var HeaderAllowlist = []string{
	"Content-Type",
	"User-Agent",
	"X-GitHub-Event",
	"X-GitHub-Delivery",
	"X-GitHub-Hook-ID",
	"X-Hub-Signature",
	"X-Hub-Signature-256",
	"X-Gitlab-Event",
	"X-Gitlab-Token",
	"X-Gitea-Event",
	"X-Gitea-Event-Type",
	"X-Gitea-Delivery",
	"X-Gitea-Signature",
	"X-Forgejo-Event",
	"X-Forgejo-Event-Type",
	"X-Forgejo-Delivery",
	"X-Forgejo-Signature",
	"X-Event-Key",
	"X-Hook-UUID",
	"X-Attempt-Number",
}

// headerAllowlistSet is a fast lookup of allowed header keys.
var headerAllowlistSet = func() map[string]bool {
	m := make(map[string]bool, len(HeaderAllowlist))
	for _, h := range HeaderAllowlist {
		m[strings.ToLower(h)] = true
	}
	return m
}()

const maxWebhookBodyBytes = 1 << 20 // 1 MiB

// HandleHibernationDispatch is the entry point for the hibernation wake surface.
// It is registered at the host root (/hibernation/...), OUTSIDE the /api/v1
// prefix and outside auth (AuthMiddleware bypasses non-/api/ paths), because SCM
// webhooks hit it unauthenticated and it carries its own per-tenant or
// per-controller token. Patterns:
//
//	/hibernation/{token}/clusters/{cluster}/ns/{ns}/queue/{ctrl}/{path...}
//	/hibernation/{token}/clusters/{cluster}/ns/{ns}/redirect/{ctrl}/{path...}
//	/hibernation/{token}/clusters/{cluster}/ns/{ns}/status/{ctrl}
//	/hibernation/{token}/teams/{team}/queue/{path...}
//
// The controller shape needs at least six segments naming the literal
// "clusters" and "ns" tokens; the tenant shape needs at least five naming
// the literal "teams" and "queue" tokens. Any other shape is 404 (design.md
// D13). The literal-segment checks stop a malformed URL from routing with
// arbitrary segments read as a cluster, a namespace, or a team.
func (s *Server) HandleHibernationDispatch(w http.ResponseWriter, r *http.Request) {
	remain := strings.TrimPrefix(r.URL.Path, "/hibernation/")
	parts := strings.SplitN(remain, "/", 7)

	switch {
	case len(parts) >= 6 && parts[1] == "clusters" && parts[3] == "ns":
		s.handleControllerHibernationDispatch(w, r, parts)
	case len(parts) >= 5 && parts[1] == "teams" && parts[3] == "queue":
		s.handleTenantHibernationDispatch(w, r, parts)
	default:
		http.Error(w, "404 not found", http.StatusNotFound)
	}
}

// handleControllerHibernationDispatch implements the controller shape:
// {token}/clusters/{cluster}/ns/{ns}/{action}/{ctrl}[/{path...}].
func (s *Server) handleControllerHibernationDispatch(w http.ResponseWriter, r *http.Request, parts []string) {
	token := parts[0]
	cluster := parts[2]
	ns := parts[4]
	action := parts[5]
	ctrl := ""
	path := ""
	if len(parts) >= 7 {
		// parts[6] is "{ctrl}" or "{ctrl}/{sub/path...}"; split off the
		// controller name from the optional trailing sub-path.
		ctrl, path, _ = strings.Cut(parts[6], "/")
	}
	// Validate cluster/ns/ctrl presence.
	if cluster == "" || ns == "" || ctrl == "" {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}

	switch action {
	case "queue":
		s.handleHibernationQueue(w, r, token, cluster, ns, ctrl, path)
	case "redirect":
		s.handleHibernationRedirect(w, r, token, cluster, ns, ctrl, path)
	case "status":
		s.handleHibernationStatus(w, r, token, cluster, ns, ctrl)
	default:
		http.Error(w, "404 not found", http.StatusNotFound)
	}
}

// handleTenantHibernationDispatch implements the tenant shape:
// {token}/teams/{team}/queue/{path...}. Only the queue action exists for a
// tenant; a cell serves no browsable URL, so redirect and status stay
// controller-only (design.md D13).
func (s *Server) handleTenantHibernationDispatch(w http.ResponseWriter, r *http.Request, parts []string) {
	token := parts[0]
	team := parts[2]
	// parts[4:] holds the replay path, possibly still split across more
	// than one element when the SplitN(..., 7) cap above landed inside it;
	// re-joining with "/" reconstructs the original path regardless.
	path := strings.Join(parts[4:], "/")
	if team == "" {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
	s.handleTenantHibernationQueue(w, r, token, team, path)
}

// checkWakeToken validates the token against the controller's status.wakeToken.
// Returns the controller CR on success, or writes an error response and returns nil.
func (s *Server) checkWakeToken(w http.ResponseWriter, r *http.Request, token, cluster, ns, ctrl string) *v1alpha1.Controller {
	if s.deps.Brood == nil {
		s.deps.Logger.Error("brood not configured, cannot verify wake token")
		writeWakeError(w, http.StatusInternalServerError)
		return nil
	}

	cr, err := s.deps.Brood.Get(r.Context(), cluster, ns, ctrl)
	if err != nil {
		// 401 with same body regardless of existence (D6).
		writeWakeError(w, http.StatusUnauthorized)
		return nil
	}

	// A controller with no minted token (hibernation never enabled) can never be
	// woken via this surface — reject rather than let an empty guess match "".
	if cr.Status.WakeToken == "" || !constantTimeEqual(token, cr.Status.WakeToken) {
		writeWakeError(w, http.StatusUnauthorized)
		return nil
	}
	return cr
}

// constantTimeEqual reports whether a and b are equal, in constant time
// regardless of length. subtle.ConstantTimeCompare alone leaks a length
// mismatch by refusing before it reaches the byte compare, so both sides
// are hashed to a fixed size first (D6, D13).
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// writeWakeError writes a uniform 401/500 JSON body (D6: identical body).
func writeWakeError(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

// handleHibernationQueue implements the queue endpoint per D6.
func (s *Server) handleHibernationQueue(w http.ResponseWriter, r *http.Request, token, cluster, ns, ctrl, path string) {
	cr := s.checkWakeToken(w, r, token, cluster, ns, ctrl)
	if cr == nil {
		return
	}

	// Step 2: path allowlist check. An empty or non-allowlisted path is not a
	// valid replay target (the mite would reject it), so reject it here with 404
	// and no side effects — never persist an envelope or publish a wake for it.
	if !isPathAllowlisted(path) {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}

	// Step 3: body cap.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		s.deps.Logger.Error("read webhook body", "error", err)
		http.Error(w, "413 request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) > maxWebhookBodyBytes {
		http.Error(w, "413 request too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Step 4: build envelope with allowlisted headers.
	envHeaders := make(map[string]string)
	for k := range r.Header {
		if headerAllowlistSet[strings.ToLower(k)] {
			envHeaders[k] = r.Header.Get(k)
		}
	}

	envelope := map[string]interface{}{
		"method":     r.Method,
		"path":       path,
		"query":      r.URL.RawQuery,
		"headers":    envHeaders,
		"bodyB64":    base64.StdEncoding.EncodeToString(body),
		"receivedAt": time.Now().UTC().Format(time.RFC3339),
		"cluster":    cluster,
		"namespace":  ns,
		"controller": ctrl,
	}

	envData, err := json.Marshal(envelope)
	if err != nil {
		s.deps.Logger.Error("marshal envelope", "error", err)
		http.Error(w, "500 internal server error", http.StatusInternalServerError)
		return
	}

	// Step 5: publish to JetStream with ack.
	webhookSubject := bus.WebhookSubject(cluster, ns, ctrl)
	busConn := s.deps.BusConn
	if busConn == nil {
		s.deps.Logger.Error("bus connection not configured")
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}
	pubAck, err := busConn.PublishJetStream(webhookSubject, envData)
	if err != nil || pubAck == nil {
		s.deps.Logger.Error("jetstream publish failed", "error", err)
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Step 6: publish wake command (skipped if already Running/Connected).
	if cr.Status.Phase != v1alpha1.ControllerPhaseRunning &&
		cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
		wakeSubject := bus.WakeSubject(cluster, ns, ctrl)
		if pubErr := busConn.Publish(wakeSubject, nil); pubErr != nil {
			// Wake is at-most-once; log and continue (D8).
			s.deps.Logger.Warn("wake publish failed", "error", pubErr, "subject", wakeSubject)
		}
	}

	// Step 7: 202 Accepted.
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

// handleTenantHibernationQueue implements the tenant queue endpoint per
// design.md D13 and specs/hibernation-wake/spec.md's ten-step order.
func (s *Server) handleTenantHibernationQueue(w http.ResponseWriter, r *http.Request, token, team, path string) {
	if s.deps.Brood == nil {
		s.deps.Logger.Error("brood not configured, cannot resolve local cluster for a tenant webhook")
		writeWakeError(w, http.StatusInternalServerError)
		return
	}

	// Step 1: read the Team. An unknown Team answers the same 401 body as
	// a bad token, so the response never confirms a Team's existence.
	tm, err := crdstore.Get[v1alpha1.Team](r.Context(), s.deps.Store, team, "")
	if err != nil {
		writeWakeError(w, http.StatusUnauthorized)
		return
	}
	var pipelines v1alpha1.TeamPipelinesStatus
	if tm.Status.Pipelines != nil {
		pipelines = *tm.Status.Pipelines
	}

	// Step 2: constant-time token compare against the tenant's own door
	// token. An empty stored token — no pipelines tenancy, or one whose
	// credentials the placement pass has not minted yet — refuses too.
	if pipelines.WebhookToken == "" || !constantTimeEqual(token, pipelines.WebhookToken) {
		writeWakeError(w, http.StatusUnauthorized)
		return
	}

	// Step 3: replay-target path allowlist check.
	if !isPathAllowlisted(path) {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}

	// Step 4: body cap.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		s.deps.Logger.Error("read tenant webhook body", "error", err)
		http.Error(w, "413 request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) > maxWebhookBodyBytes {
		http.Error(w, "413 request too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Step 5: identify the forge and verify the delivery against the
	// tenant's door secret. The verification header is always read from
	// the request itself, never from the allowlisted header subset the
	// envelope carries.
	forge := identifyForge(r)
	if pipelines.WebhookSecret == "" {
		// The placement pass has not finished allocating this tenant's door
		// credentials.
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !verifyForgeSignature(r, forge, body, pipelines.WebhookSecret) {
		http.Error(w, "400 bad request", http.StatusBadRequest)
		return
	}

	// Step 6: derive the delivery id.
	deliveryID := deriveDeliveryID(r, forge)

	// Step 7: build the tenant envelope. It carries no namespace and no
	// controller. It drops X-Gitlab-Token: both it and X-Varroa-Token hold
	// the tenant secret, and the envelope reaches JetStream and then
	// Jenkins. X-Varroa-Token is not in the header allowlist, so only
	// X-Gitlab-Token needs an explicit drop here.
	envHeaders := make(map[string]string)
	for k := range r.Header {
		if headerAllowlistSet[strings.ToLower(k)] {
			envHeaders[k] = r.Header.Get(k)
		}
	}
	delete(envHeaders, "X-Gitlab-Token")

	cluster := s.deps.Brood.LocalCluster()
	envelope := map[string]interface{}{
		"method":     r.Method,
		"path":       path,
		"query":      r.URL.RawQuery,
		"headers":    envHeaders,
		"bodyB64":    base64.StdEncoding.EncodeToString(body),
		"receivedAt": time.Now().UTC().Format(time.RFC3339),
		"cluster":    cluster,
		"team":       team,
		"forge":      forge,
		"deliveryId": deliveryID,
	}

	envData, err := json.Marshal(envelope)
	if err != nil {
		s.deps.Logger.Error("marshal tenant envelope", "error", err)
		http.Error(w, "500 internal server error", http.StatusInternalServerError)
		return
	}

	// Step 8: publish to JetStream with ack. webhook.<cluster>.tenant.<team>
	// is already captured by the existing WebhookWildcard/replay-consumer
	// filter, so no stream or consumer configuration changes.
	busConn := s.deps.BusConn
	if busConn == nil {
		s.deps.Logger.Error("bus connection not configured")
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}
	pubAck, err := busConn.PublishJetStream(bus.WebhookTenantSubject(cluster, team), envData)
	if err != nil || pubAck == nil {
		s.deps.Logger.Error("tenant jetstream publish failed", "error", err)
		http.Error(w, "503 service unavailable", http.StatusServiceUnavailable)
		return
	}

	// Step 9: no wake publish — a cell never hibernates.

	// Step 10: 202 Accepted.
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

// identifyForge reads the request headers to name the forge a tenant
// delivery came from (design.md D13).
func identifyForge(r *http.Request) string {
	// Gitea and Forgejo also send X-GitHub-Event, so they are checked first.
	if r.Header.Get("X-Gitea-Event") != "" || r.Header.Get("X-Forgejo-Event") != "" {
		return "gitea"
	}
	if r.Header.Get("X-GitHub-Event") != "" {
		return "github"
	}
	if r.Header.Get("X-Gitlab-Event") != "" {
		return "gitlab"
	}
	return "generic"
}

// verifyForgeSignature verifies a tenant delivery against secret, per
// forge. It always reads the verification header from the request, never
// from the allowlisted header subset the envelope carries.
func verifyForgeSignature(r *http.Request, forge string, body []byte, secret string) bool {
	switch forge {
	case "github":
		sig := r.Header.Get("X-Hub-Signature-256")
		if sig == "" {
			return false
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(sig), []byte(want))
	case "gitea":
		// Gitea and Forgejo sign the bare hex digest, without a prefix.
		sig := r.Header.Get("X-Gitea-Signature")
		if sig == "" {
			sig = r.Header.Get("X-Forgejo-Signature")
		}
		if sig == "" {
			return false
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(sig), []byte(want))
	case "gitlab":
		got := r.Header.Get("X-Gitlab-Token")
		if got == "" {
			return false
		}
		return constantTimeEqual(got, secret)
	default: // generic
		got := r.Header.Get("X-Varroa-Token")
		if got == "" {
			return false
		}
		return constantTimeEqual(got, secret)
	}
}

// deriveDeliveryID reads the forge's own delivery-id header, minting a
// UUID when the header is absent (design.md D13).
func deriveDeliveryID(r *http.Request, forge string) string {
	switch forge {
	case "github":
		if v := r.Header.Get("X-GitHub-Delivery"); v != "" {
			return v
		}
	case "gitea":
		if v := r.Header.Get("X-Gitea-Delivery"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forgejo-Delivery"); v != "" {
			return v
		}
	case "gitlab":
		if v := r.Header.Get("X-Gitlab-Event-UUID"); v != "" {
			return v
		}
	}
	return uuid.NewString()
}

// handleHibernationRedirect implements the redirect endpoint per D6.
func (s *Server) handleHibernationRedirect(w http.ResponseWriter, r *http.Request, token, cluster, ns, ctrl, path string) {
	cr := s.checkWakeToken(w, r, token, cluster, ns, ctrl)
	if cr == nil {
		return
	}

	// A headless controller has no external URL, so the interstitial has no
	// page to redirect the browser to (D5). The 404 comes after the token
	// guard above, so the response leaks nothing about which controllers
	// exist, and no wake command is published.
	if v1alpha1.IsHeadless(cr) {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}

	// Publish wake command.
	if s.deps.BusConn != nil {
		wakeSubject := bus.WakeSubject(cluster, ns, ctrl)
		if pubErr := s.deps.BusConn.Publish(wakeSubject, nil); pubErr != nil {
			s.deps.Logger.Warn("redirect wake publish failed", "error", pubErr)
		}
	}

	// Determine the controller URL from the CR, preserving any deep sub-path so a
	// human wake link lands on the intended Jenkins page (e.g. /job/foo/), not the
	// controller root.
	ctrlURL := cr.Status.Endpoint
	if ctrlURL == "" {
		ctrlURL = fmt.Sprintf("/api/v1/clusters/%s/ns/%s/controllers/%s", cluster, ns, ctrl)
	}
	if path != "" {
		ctrlURL = strings.TrimRight(ctrlURL, "/") + "/" + path
	}

	// Serve interstitial HTML.
	statusPath := fmt.Sprintf("/hibernation/%s/clusters/%s/ns/%s/status/%s", token, cluster, ns, ctrl)
	hibernation.WriteInterstitial(w, hibernation.InterstitialParams{
		StatusPath:                statusPath,
		TargetURL:                 ctrlURL,
		HTTPStatus:                http.StatusOK,
		RedirectOnNonWakeResponse: false,
	})
}

// handleHibernationStatus returns the controller phase as JSON.
func (s *Server) handleHibernationStatus(w http.ResponseWriter, r *http.Request, token, cluster, ns, ctrl string) {
	cr := s.checkWakeToken(w, r, token, cluster, ns, ctrl)
	if cr == nil {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"varroaWake":true,"phase":"%s"}`, cr.Status.Phase)
}

// isPathAllowlisted checks if the given path matches any allowlisted prefix.
func isPathAllowlisted(path string) bool {
	for _, prefix := range hibernation.ReplayPathAllowlist {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

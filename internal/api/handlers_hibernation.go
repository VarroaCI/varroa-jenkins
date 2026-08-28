package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
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
// webhooks hit it unauthenticated and it carries its own per-controller token.
// Patterns:
//
//	/hibernation/{token}/clusters/{cluster}/ns/{ns}/queue/{ctrl}/{path...}
//	/hibernation/{token}/clusters/{cluster}/ns/{ns}/redirect/{ctrl}/{path...}
//	/hibernation/{token}/clusters/{cluster}/ns/{ns}/status/{ctrl}
func (s *Server) HandleHibernationDispatch(w http.ResponseWriter, r *http.Request) {
	// Strip /hibernation/ prefix.
	remain := strings.TrimPrefix(r.URL.Path, "/hibernation/")
	// Expected: {token}/clusters/{cluster}/ns/{ns}/{action}/{ctrl}[/{path...}]
	parts := strings.SplitN(remain, "/", 7)
	// Require the exact literal segments so a malformed URL returns a clean 404
	// rather than routing with arbitrary segments read as cluster/ns.
	if len(parts) < 6 || parts[1] != "clusters" || parts[3] != "ns" {
		http.Error(w, "404 not found", http.StatusNotFound)
		return
	}
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
	if cr.Status.WakeToken == "" {
		writeWakeError(w, http.StatusUnauthorized)
		return nil
	}

	// Constant-time compare (D6). Hash both first so the comparison stays
	// constant-time even when the supplied token's length differs from the
	// stored one (subtle.ConstantTimeCompare short-circuits on unequal lengths).
	got := sha256.Sum256([]byte(token))
	want := sha256.Sum256([]byte(cr.Status.WakeToken))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		writeWakeError(w, http.StatusUnauthorized)
		return nil
	}
	return cr
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

// handleHibernationRedirect implements the redirect endpoint per D6.
func (s *Server) handleHibernationRedirect(w http.ResponseWriter, r *http.Request, token, cluster, ns, ctrl, path string) {
	cr := s.checkWakeToken(w, r, token, cluster, ns, ctrl)
	if cr == nil {
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

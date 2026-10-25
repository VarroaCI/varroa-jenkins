// Package api: the Jenkinsfile lint surface. This file implements
// POST /pipelines/{tenant}/lint. It sends the text to the tenant's slot-0
// cell, where Jenkins answers its declarative linter. The route writes no
// state and never wakes a cell.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items/lint"
)

// jenkinsfileLintMaxBody bounds the whole request body.
const jenkinsfileLintMaxBody = 1 << 20

// jenkinsfileLintMaxText bounds the jenkinsfile field.
const jenkinsfileLintMaxText = 256 << 10

// jenkinsfileLintTokenTTL bounds the operator token of one lint call.
const jenkinsfileLintTokenTTL = 60 * time.Second

// jenkinsfileLintReplyMax bounds the linter reply that the route reads.
const jenkinsfileLintReplyMax = 1 << 20

// jenkinsfileLintHTTPClient calls the cell linter.
var jenkinsfileLintHTTPClient = &http.Client{Timeout: 20 * time.Second}

// linterLocation extracts the line and column that the declarative linter
// writes into its message text.
var linterLocation = regexp.MustCompile(`@ line (\d+), column (\d+)\.`)

// jenkinsfileLintRequest is the POST /pipelines/{tenant}/lint body.
type jenkinsfileLintRequest struct {
	Jenkinsfile string `json:"jenkinsfile"`
}

// jenkinsfileLintErrorJSON is one finding of a Jenkinsfile lint.
type jenkinsfileLintErrorJSON struct {
	Source  string `json:"source"`
	Rule    string `json:"rule,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
}

// JenkinsfileLintResult is the POST /pipelines/{tenant}/lint answer.
type JenkinsfileLintResult struct {
	Valid     bool                       `json:"valid"`
	Cell      string                     `json:"cell"`
	Namespace string                     `json:"namespace"`
	Errors    []jenkinsfileLintErrorJSON `json:"errors"`
}

// handleLintJenkinsfile answers one lint call from the HTTP surface.
func (s *Server) handleLintJenkinsfile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, jenkinsfileLintMaxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", jenkinsfileLintMaxBody))
			return
		}
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var req jenkinsfileLintRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	body, serr := s.LintJenkinsfile(r.Context(), auth.ClaimsFromContext(r.Context()), r.PathValue("tenant"), req.Jenkinsfile)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

// LintJenkinsfile lints text on the slot-0 cell of tenant. The HTTP route
// and the lint_jenkinsfile MCP tool both call it. It returns a ServiceError
// for every refusal and for a linter that cannot answer. A lint failure is
// data in the returned body.
func (s *Server) LintJenkinsfile(ctx context.Context, claims *auth.Claims, tenant, text string) (JenkinsfileLintResult, *ServiceError) {
	if strings.TrimSpace(text) == "" {
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusBadRequest, Message: "jenkinsfile is required"}
	}
	if len(text) > jenkinsfileLintMaxText {
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusRequestEntityTooLarge,
			Message: fmt.Sprintf("jenkinsfile exceeds %d bytes", jenkinsfileLintMaxText)}
	}

	refs, serr := s.resolveTenantTenants(ctx, claims, tenant)
	if serr != nil {
		return JenkinsfileLintResult{}, serr
	}
	ref, ok := tenantPipelinesTenant(refs, tenant)
	if !ok {
		for _, candidate := range refs {
			if candidate.Controller != "" {
				ref, ok = candidate, true
				break
			}
		}
	}
	if !ok || ref.Controller == "" {
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusConflict,
			Message: fmt.Sprintf("tenant %q has no cell yet", tenant)}
	}
	if !s.canReadPipeline(claims, ref, "") {
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusNotFound, Message: "tenant not found"}
	}
	if ref.Cluster != "" && s.deps.Brood != nil && ref.Cluster != s.deps.Brood.LocalCluster() {
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusConflict,
			Message: fmt.Sprintf("lint reaches only a cell on the core cluster; tenant %q runs on cluster %q", tenant, ref.Cluster)}
	}
	if s.deps.JenkinsTokenSigner == nil {
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "jenkins token signer unavailable"}
	}

	cr, err := s.cellController(ctx, ref.Cluster, ref.Namespace, ref.Controller)
	if err == nil && cr == nil && s.deps.Store != nil {
		cr, err = crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, ref.Controller, ref.Namespace)
	}
	if err != nil || cr == nil {
		if err != nil {
			s.deps.Logger.Error("read lint cell", "tenant", tenant, "error", err)
		}
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "read tenant cell failed"}
	}

	switch cr.Status.Phase {
	case v1alpha1.ControllerPhaseHibernated:
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusConflict,
			Message: fmt.Sprintf("cell %s is hibernated; start a run to wake it, then lint again", cr.Name)}
	case v1alpha1.ControllerPhaseStopped:
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusConflict, Message: fmt.Sprintf("cell %s is stopped", cr.Name)}
	case v1alpha1.ControllerPhaseConnected, v1alpha1.ControllerPhaseRunning:
	default:
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusServiceUnavailable,
			Message: fmt.Sprintf("cell %s is not ready (phase %s)", cr.Name, cr.Status.Phase)}
	}

	errs, err := s.callCellLinter(ctx, cr, text)
	if err != nil {
		s.deps.Logger.Error("cell linter", "cell", cr.Name, "error", err)
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusBadGateway, Message: err.Error()}
	}

	if v1alpha1.IsCell(cr) {
		for _, f := range lint.RunScript(text, "Jenkinsfile", lint.CellRules()) {
			errs = append(errs, jenkinsfileLintErrorJSON{Source: "varroa", Rule: f.Rule, Message: f.Message})
		}
	}
	return JenkinsfileLintResult{Valid: len(errs) == 0, Cell: cr.Name, Namespace: cr.Namespace, Errors: errs}, nil
}

// callCellLinter posts text to the cell's declarative linter with an
// operator token and parses the reply.
func (s *Server) callCellLinter(ctx context.Context, cr *v1alpha1.Controller, text string) ([]jenkinsfileLintErrorJSON, error) {
	token, err := s.deps.JenkinsTokenSigner.GenerateOperatorJenkinsToken(cr.Name, cr.Namespace, jenkinsfileLintTokenTTL)
	if err != nil {
		return nil, fmt.Errorf("mint operator jenkins token: %w", err)
	}

	form := url.Values{"jenkinsfile": {text}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cellServiceURL(cr)+"/pipeline-model-converter/validateJenkinsfile", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build linter request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := jenkinsfileLintHTTPClient.Do(req)
	if err != nil {
		// The client message stays generic. The transport detail goes to the log only.
		s.deps.Logger.Error("cell linter transport", "cell", cr.Name, "error", err)
		return nil, fmt.Errorf("the cell linter is unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the cell linter answered status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, jenkinsfileLintReplyMax))
	if err != nil {
		return nil, fmt.Errorf("read the cell linter reply: %w", err)
	}
	return parseLinterReply(raw)
}

// linterReply is the declarative linter's JSON envelope.
type linterReply struct {
	Status string `json:"status"`
	Data   struct {
		Result string `json:"result"`
		Errors []struct {
			Error json.RawMessage `json:"error"`
		} `json:"errors"`
	} `json:"data"`
}

// parseLinterReply turns the linter's JSON envelope into findings. An
// unknown envelope is an error, so a changed linter cannot pass as valid.
func parseLinterReply(body []byte) ([]jenkinsfileLintErrorJSON, error) {
	var reply linterReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("the cell linter reply is not JSON")
	}
	if reply.Status != "ok" {
		return nil, fmt.Errorf("the cell linter reported status %q", reply.Status)
	}
	out := []jenkinsfileLintErrorJSON{}
	switch reply.Data.Result {
	case "success":
		return out, nil
	case "failure":
	default:
		return nil, fmt.Errorf("the cell linter reported result %q", reply.Data.Result)
	}
	for _, e := range reply.Data.Errors {
		for _, msg := range linterMessages(e.Error) {
			f := jenkinsfileLintErrorJSON{Source: "jenkins", Message: strings.TrimSpace(msg)}
			if m := linterLocation.FindStringSubmatch(msg); m != nil {
				f.Line, _ = strconv.Atoi(m[1])
				f.Column, _ = strconv.Atoi(m[2])
			}
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the cell linter reported failure without an error message")
	}
	return out, nil
}

// linterMessages decodes one error field, which is a string or a list of
// strings.
func linterMessages(raw json.RawMessage) []string {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

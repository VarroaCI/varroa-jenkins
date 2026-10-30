// Package api: the Jenkinsfile lint surface. This file implements
// POST /pipelines/{tenant}/lint. A tenant on the local cluster is linted by
// the BFF through its cell's declarative linter. A tenant on a member
// cluster is linted by that cluster's operator, which calls its own cell's
// linter (cells.lint). The route writes no state and never wakes a cell.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/declarativelint"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items/lint"
)

// jenkinsfileLintMaxBody bounds the whole request body.
const jenkinsfileLintMaxBody = 1 << 20

// jenkinsfileLintMaxText bounds the jenkinsfile field.
const jenkinsfileLintMaxText = 256 << 10

// jenkinsfileLintTokenTTL bounds the operator token of one lint call.
const jenkinsfileLintTokenTTL = 60 * time.Second

// jenkinsfileLintRequest is the POST /pipelines/{tenant}/lint body.
type jenkinsfileLintRequest struct {
	Jenkinsfile string `json:"jenkinsfile"`
}

// jenkinsfileLintErrorJSON is one finding of a Jenkinsfile lint.
type jenkinsfileLintErrorJSON = declarativelint.Finding

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

	local := s.isLocalCluster(ref.Cluster)
	if local && s.deps.JenkinsTokenSigner == nil {
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "jenkins token signer unavailable"}
	}

	cr, err := s.cellController(ctx, ref.Cluster, ref.Namespace, ref.Controller)
	if err == nil && cr == nil && local && s.deps.Store != nil {
		cr, err = crdstore.Get[v1alpha1.Controller](ctx, s.deps.Store, ref.Controller, ref.Namespace)
	}
	if err != nil || cr == nil {
		if err != nil {
			s.deps.Logger.Error("read lint cell", "tenant", tenant, "error", err)
		}
		return JenkinsfileLintResult{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "read tenant cell failed"}
	}

	if refusal := lintPhaseRefusal(cr.Name, cr.Status.Phase); refusal != nil {
		return JenkinsfileLintResult{}, refusal
	}

	var errs []jenkinsfileLintErrorJSON
	if local {
		errs, serr = s.lintOnLocalCell(ctx, cr, text)
	} else {
		errs, serr = s.lintOnMember(ctx, ref.Cluster, cr, text)
	}
	if serr != nil {
		return JenkinsfileLintResult{}, serr
	}

	if v1alpha1.IsCell(cr) {
		for _, f := range lint.RunScript(text, "Jenkinsfile", lint.CellRules()) {
			errs = append(errs, jenkinsfileLintErrorJSON{Source: "varroa", Rule: f.Rule, Message: f.Message})
		}
	}
	return JenkinsfileLintResult{Valid: len(errs) == 0, Cell: cr.Name, Namespace: cr.Namespace, Errors: errs}, nil
}

// lintPhaseRefusal returns the refusal for a cell that cannot lint, or nil
// for a Connected or Running cell. A lint never wakes a cell.
func lintPhaseRefusal(name string, phase v1alpha1.ControllerPhase) *ServiceError {
	switch phase {
	case v1alpha1.ControllerPhaseHibernated:
		return &ServiceError{Status: http.StatusConflict,
			Message: fmt.Sprintf("cell %s is hibernated; start a run to wake it, then lint again", name)}
	case v1alpha1.ControllerPhaseStopped:
		return &ServiceError{Status: http.StatusConflict, Message: fmt.Sprintf("cell %s is stopped", name)}
	case v1alpha1.ControllerPhaseConnected, v1alpha1.ControllerPhaseRunning:
		return nil
	default:
		return &ServiceError{Status: http.StatusServiceUnavailable,
			Message: fmt.Sprintf("cell %s is not ready (phase %s)", name, phase)}
	}
}

// lintOnLocalCell lints text on a cell of the local cluster. The BFF signs
// the operator token and calls the cell's linter.
func (s *Server) lintOnLocalCell(ctx context.Context, cr *v1alpha1.Controller, text string) ([]jenkinsfileLintErrorJSON, *ServiceError) {
	token, err := s.deps.JenkinsTokenSigner.GenerateOperatorJenkinsToken(cr.Name, cr.Namespace, jenkinsfileLintTokenTTL)
	if err != nil {
		s.deps.Logger.Error("mint lint token", "cell", cr.Name, "error", err)
		return nil, &ServiceError{Status: http.StatusBadGateway, Message: "mint operator jenkins token failed"}
	}
	errs, err := declarativelint.Validate(ctx, declarativelint.HTTPClient, s.cellServiceURL(cr), token, text)
	if err != nil {
		cause := err
		var unreachable *declarativelint.UnreachableError
		if errors.As(err, &unreachable) {
			cause = unreachable.Err
		}
		s.deps.Logger.Error("cell linter", "cell", cr.Name, "error", cause)
		return nil, &ServiceError{Status: http.StatusBadGateway, Message: err.Error()}
	}
	return errs, nil
}

// lintOnMember sends text to the operator of a member cluster, which lints
// on its own cell. The member answers a phase refusal and a linter failure
// as data, so each maps to its status here.
func (s *Server) lintOnMember(ctx context.Context, cluster string, cr *v1alpha1.Controller, text string) ([]jenkinsfileLintErrorJSON, *ServiceError) {
	if s.deps.ConfigBrood == nil {
		return nil, &ServiceError{Status: http.StatusBadGateway, Message: "cluster " + cluster + " unreachable"}
	}
	reply, err := s.deps.ConfigBrood.CellLint(ctx, cluster, bus.CellLintRequest{
		Namespace: cr.Namespace, Cell: cr.Name, Jenkinsfile: text,
	})
	if err != nil {
		var unreachable *ErrClusterUnreachable
		var brood *BroodError
		switch {
		case errors.As(err, &unreachable):
			return nil, &ServiceError{Status: http.StatusBadGateway, Message: "cluster " + cluster + " unreachable"}
		case errors.As(err, &brood) && brood.Code == bus.CodeNotFound:
			return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "read tenant cell failed"}
		}
		s.deps.Logger.Error("member cell lint", "cluster", cluster, "cell", cr.Name, "error", err)
		return nil, &ServiceError{Status: http.StatusInternalServerError, Message: "lint on cluster " + cluster + " failed"}
	}
	if reply.Phase != "" {
		if refusal := lintPhaseRefusal(cr.Name, v1alpha1.ControllerPhase(reply.Phase)); refusal != nil {
			return nil, refusal
		}
		return nil, &ServiceError{Status: http.StatusBadGateway,
			Message: fmt.Sprintf("cluster %s answered phase %q for a lint", cluster, reply.Phase)}
	}
	if reply.LinterError != "" {
		return nil, &ServiceError{Status: http.StatusBadGateway, Message: reply.LinterError}
	}
	errs := make([]jenkinsfileLintErrorJSON, 0, len(reply.Errors))
	for _, f := range reply.Errors {
		errs = append(errs, jenkinsfileLintErrorJSON{
			Source: f.Source, Rule: f.Rule, Line: f.Line, Column: f.Column, Message: f.Message,
		})
	}
	return errs, nil
}

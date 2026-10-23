// Package api: the marker lint surface, design D13. This file implements
// POST /pipelines/{tenant}/markers/validate, which varroactl lint marker
// (internal/varroactl) also calls.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
)

// markerValidateFindingJSON is one line of design D13's lint table:
// SEVERITY, PATH, and MESSAGE. Every finding this endpoint reports is a
// rejection, so Severity is always "error": the marker's schema subset
// carries no warning class.
type markerValidateFindingJSON struct {
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// markerValidateResponseJSON is POST /pipelines/{tenant}/markers/validate's
// response body. TemplateName and TemplateVersion are present only when
// the marker resolved against the tenant's delivered set and its values
// validated: an empty Findings list.
type markerValidateResponseJSON struct {
	Findings        []markerValidateFindingJSON `json:"findings"`
	TemplateName    string                      `json:"templateName,omitempty"`
	TemplateVersion string                      `json:"templateVersion,omitempty"`
}

// markerFinding builds one error-severity finding.
func markerFinding(path, format string, args ...any) markerValidateFindingJSON {
	return markerValidateFindingJSON{Severity: "error", Path: path, Message: fmt.Sprintf(format, args...)}
}

// oneFindingResponse answers 200 with exactly one finding: every failure
// this handler reports is data, per design D11's L4 layer, never a
// transport error, so a lint client always gets a body to print.
func (s *Server) oneFindingResponse(w http.ResponseWriter, f markerValidateFindingJSON) {
	s.writeJSON(w, http.StatusOK, markerValidateResponseJSON{Findings: []markerValidateFindingJSON{f}})
}

// handleValidatePipelineMarker implements design D13's pre-commit lint
// surface: it resolves the tenant's delivered pipeline-template set through
// the same TenantResolver seam and authorization the tenant's other
// pipeline reads use, then runs the Go marker and schema validator
// against the posted marker text. It never contacts a cell, so it works
// while a cell is down, and it reads no record-store data, so a disabled
// record store does not affect it.
func (s *Server) handleValidatePipelineMarker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	tenant := r.PathValue("tenant")

	refs, serr := s.resolveTenantTenants(ctx, claims, tenant)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}

	var req struct {
		Marker string `json:"marker"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	marker, err := pipelinetemplate.ParseMarker([]byte(req.Marker))
	if err != nil {
		s.oneFindingResponse(w, markerFinding("", "%s", err))
		return
	}

	delivered := s.deliveredPipelineTemplatesForTenant(ctx, refs)
	if len(delivered) == 0 {
		s.oneFindingResponse(w, markerFinding("", "tenant %q delivers no pipeline template", tenant))
		return
	}

	doc, err := marker.Resolve(delivered)
	if err != nil {
		s.oneFindingResponse(w, markerFinding("template", "%s", err))
		return
	}

	_, valueFindings, err := pipelinetemplate.ValidateValues(doc.Spec.Parameters, marker.Values)
	if err != nil {
		s.oneFindingResponse(w, markerFinding("values", "%s", err))
		return
	}

	resp := markerValidateResponseJSON{Findings: []markerValidateFindingJSON{}}
	for _, f := range valueFindings {
		resp.Findings = append(resp.Findings, markerFinding("values"+f.Path, "%s", f.Message))
	}
	if len(resp.Findings) == 0 {
		resp.TemplateName = doc.Metadata.Name
		resp.TemplateVersion = doc.Spec.Version
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// deliveredPipelineTemplatesForTenant unions every admitted tenant's
// delivered pipeline-template set, de-duplicated by name and version. A
// tenant ordinarily resolves to exactly one tenant (design D1 steps 1, 3,
// and 4); the union matters only for an unplaced Tenant's multi-controller
// fan-out (step 2).
func (s *Server) deliveredPipelineTemplatesForTenant(ctx context.Context, refs []TenantRef) []pipelinetemplate.Document {
	seen := map[string]bool{}
	var out []pipelinetemplate.Document
	for _, ref := range refs {
		docs, err := s.controllerPipelineTemplates(ctx, ref)
		if err != nil {
			continue
		}
		for _, doc := range docs {
			key := doc.Metadata.Name + "@" + doc.Spec.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, doc)
		}
	}
	return out
}

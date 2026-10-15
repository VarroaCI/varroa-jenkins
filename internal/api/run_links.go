package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// runDetailDownstreamLimit is the number of downstream runs GET /runs/{run}
// carries inline. A longer list pages through GET /runs/{run}/downstream.
const runDetailDownstreamLimit = 20

// RunLink names one run on the far side of a run's upstream or downstream
// link, or a dependency edge's last run. JobPath, RunID, BuildNumber,
// Status, and Result are absent when the reader may not read the run. Only
// Tenant remains, so the reader still sees which tenant holds the run.
type RunLink struct {
	Tenant      string `json:"tenant"`
	JobPath     string `json:"jobPath,omitempty"`
	RunID       string `json:"runId,omitempty"`
	BuildNumber int64  `json:"buildNumber,omitempty"`
	Status      string `json:"status,omitempty"`
	Result      string `json:"result,omitempty"`
}

// RunLinkPage is one page of downstream run links. NextCursor is empty on
// the last page. Items is never null.
type RunLinkPage struct {
	Items      []RunLink `json:"items"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// redactRunLink returns l as the reader may see it. A link to a run the
// reader may not read keeps its tenant alone. A link with no job path has
// nothing to check against, so it keeps its tenant alone too.
func (s *Server) redactRunLink(claims *auth.Claims, l RunLink) RunLink {
	if l.JobPath == "" || !s.canReadRun(claims, l.Tenant, l.JobPath) {
		return RunLink{Tenant: l.Tenant}
	}
	return l
}

// upstreamRunLink builds the upstream link of run. It answers nil when the
// run was not started by another run.
func (s *Server) upstreamRunLink(claims *auth.Claims, run recordstore.Run) *RunLink {
	if run.UpstreamTenant == nil {
		return nil
	}
	link := RunLink{Tenant: *run.UpstreamTenant}
	if run.UpstreamJob != nil {
		link.JobPath = *run.UpstreamJob
	}
	if run.UpstreamNumber != nil {
		link.BuildNumber = *run.UpstreamNumber
	}
	if run.UpstreamRunID != nil {
		link.RunID = run.UpstreamRunID.String()
	}
	out := s.redactRunLink(claims, link)
	return &out
}

// runLinkFromRun maps a stored run to its link form, before redaction.
func runLinkFromRun(run recordstore.Run) RunLink {
	link := RunLink{Tenant: run.Tenant, JobPath: run.JobPath, RunID: run.RunID, Status: run.Status}
	if run.BuildNumber != nil {
		link.BuildNumber = *run.BuildNumber
	}
	if run.Result != nil {
		link.Result = *run.Result
	}
	return link
}

// downstreamRunPage reads one page of the runs that runID started and
// redacts each item for the reader. Items is never nil.
func (s *Server) downstreamRunPage(ctx context.Context, claims *auth.Claims, runID string, limit int, cursor string) (RunLinkPage, error) {
	page, err := s.deps.RecordStore.ListDownstreamRuns(ctx, recordstore.DownstreamRunQuery{
		UpstreamRunID: runID,
		Limit:         limit,
		Cursor:        cursor,
	})
	if err != nil {
		return RunLinkPage{Items: []RunLink{}}, err
	}
	out := RunLinkPage{Items: make([]RunLink, 0, len(page.Runs)), NextCursor: page.NextCursor}
	for _, run := range page.Runs {
		out.Items = append(out.Items, s.redactRunLink(claims, runLinkFromRun(run)))
	}
	return out, nil
}

// handleListRunDownstream answers GET /runs/{run}/downstream: one keyset
// page of the runs the run started. A run the caller may not read answers
// 404, the same as GET /runs/{run}.
func (s *Server) handleListRunDownstream(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	run, _, ok := s.resolveRunOrNotFound(w, r, claims, r.PathValue("run"))
	if !ok {
		return
	}
	limit, err := parseRunsLimit(r)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.downstreamRunPage(r.Context(), claims, run.RunID, limit, r.URL.Query().Get("cursor"))
	if errors.Is(err, recordstore.ErrInvalidRunCursor) {
		s.writeJSONError(w, http.StatusBadRequest, "invalid cursor")
		return
	}
	if s.handleReaderError(w, err, "list downstream runs") {
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

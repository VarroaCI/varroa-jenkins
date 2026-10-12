package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// JobBranchList is the body of GET /pipelines/{team}/jobs/{job}/branches.
// It carries the branch and change-request inventory of one multibranch
// job. The inventory is up to ten minutes stale, and a down cell keeps
// the last inventory it sent. observedAt is absent while the cell has
// not exported the job yet.
type JobBranchList struct {
	JobPath        string              `json:"jobPath"`
	ObservedAt     *string             `json:"observedAt,omitempty"`
	IndexedAt      *string             `json:"indexedAt,omitempty"`
	IndexResult    string              `json:"indexResult,omitempty"`
	Truncated      bool                `json:"truncated"`
	Branches       []jobBranchHeadJSON `json:"branches"`
	ChangeRequests []jobBranchHeadJSON `json:"changeRequests"`
	Tags           []jobBranchHeadJSON `json:"tags"`
	LastScan       *branchScanJSON     `json:"lastScan,omitempty"`
}

// jobBranchHeadJSON is one head of a multibranch job. LastRun is the
// newest run of the head, and it is absent when the head never built.
type jobBranchHeadJSON struct {
	Name          string             `json:"name"`
	JobPath       string             `json:"jobPath"`
	Orphaned      bool               `json:"orphaned"`
	Revision      string             `json:"revision,omitempty"`
	ChangeRequest *changeRequestJSON `json:"changeRequest,omitempty"`
	LastRun       *runSummaryJSON    `json:"lastRun,omitempty"`
}

// branchScanJSON is the last requested scan of a multibranch job.
type branchScanJSON struct {
	ScanID        string   `json:"scanId"`
	State         string   `json:"state"`
	Actor         string   `json:"actor,omitempty"`
	RequestedAt   string   `json:"requestedAt,omitempty"`
	EndedAt       string   `json:"endedAt,omitempty"`
	Result        string   `json:"result,omitempty"`
	NewHeads      []string `json:"newHeads"`
	ChangedHeads  []string `json:"changedHeads"`
	OrphanedHeads []string `json:"orphanedHeads"`
	Message       string   `json:"message,omitempty"`
}

// handleListJobBranches serves GET /pipelines/{team}/jobs/{job}/branches.
func (s *Server) handleListJobBranches(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	list, serr := s.ListJobBranches(r.Context(), auth.ClaimsFromContext(r.Context()), r.PathValue("team"), r.PathValue("job"))
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

// ListJobBranches answers the branch and change-request inventory of one
// multibranch job of a team. It is the shared read for the HTTP route and
// the list_job_branches MCP tool. A job with no inventory row answers an
// empty list, not a 404, so the job page renders while the cell has not
// exported the job yet.
func (s *Server) ListJobBranches(ctx context.Context, claims *auth.Claims, team, jobPath string) (JobBranchList, *ServiceError) {
	if s.deps.RecordStore == nil {
		return JobBranchList{}, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		return JobBranchList{}, serr
	}
	ref, ok := s.tenantForJob(claims, refs, jobPath)
	if !ok {
		return JobBranchList{}, &ServiceError{Status: http.StatusNotFound, Message: "job not found"}
	}

	out := JobBranchList{
		JobPath:        jobPath,
		Branches:       []jobBranchHeadJSON{},
		ChangeRequests: []jobBranchHeadJSON{},
		Tags:           []jobBranchHeadJSON{},
	}
	project, found, err := s.deps.RecordStore.GetJobBranches(ctx, ref.Cluster, ref.Tenant, jobPath)
	if err != nil {
		return JobBranchList{}, s.readerServiceError(err, "get job branches")
	}
	if !found {
		return out, nil
	}

	observed := project.ObservedAt.UTC().Format(time.RFC3339)
	out.ObservedAt = &observed
	if project.IndexedAt != nil {
		indexed := project.IndexedAt.UTC().Format(time.RFC3339)
		out.IndexedAt = &indexed
	}
	out.IndexResult = project.IndexResult
	out.Truncated = project.HeadsTruncated
	if project.LastScan != nil {
		out.LastScan = branchScanOf(*project.LastScan)
	}

	// Each head meets the read grant on its own job path, as a run row
	// does. A head the caller may not read never reaches the response.
	grants, granted := s.deps.Authorizer.tenantGrants(claims, ref.Tenant, ref.Folder)
	var admitted []recordstore.JobBranchHead
	for _, head := range project.Heads {
		if !granted || !grantsAllow(grants, ref.Folder, head.JobPath, "read") {
			continue
		}
		admitted = append(admitted, head)
	}

	paths := make([]string, 0, len(admitted))
	for _, head := range admitted {
		paths = append(paths, head.JobPath)
	}
	runs := map[string]recordstore.Run{}
	if len(paths) > 0 {
		runs, err = s.deps.RecordStore.LatestRunsForJobs(ctx, ref.Cluster, ref.Tenant, paths)
		if err != nil {
			return JobBranchList{}, s.readerServiceError(err, "latest runs for job branches")
		}
	}

	for _, head := range admitted {
		row := jobBranchHeadJSON{
			Name:     head.Name,
			JobPath:  head.JobPath,
			Orphaned: head.Orphaned,
			Revision: head.Revision,
		}
		if head.ChangeRequest != nil {
			row.ChangeRequest = &changeRequestJSON{
				ID:           head.ChangeRequest.ID,
				Title:        head.ChangeRequest.Title,
				SourceBranch: head.ChangeRequest.SourceBranch,
				TargetBranch: head.ChangeRequest.TargetBranch,
				URL:          head.ChangeRequest.URL,
			}
		}
		if run, ok := runs[head.JobPath]; ok {
			summary := buildRunSummary(run)
			row.LastRun = &summary
		}
		switch head.Kind {
		case "changeRequest":
			out.ChangeRequests = append(out.ChangeRequests, row)
		case "tag":
			out.Tags = append(out.Tags, row)
		default:
			out.Branches = append(out.Branches, row)
		}
	}

	sort.SliceStable(out.Branches, func(i, j int) bool { return out.Branches[i].Name < out.Branches[j].Name })
	sort.SliceStable(out.Tags, func(i, j int) bool { return out.Tags[i].Name < out.Tags[j].Name })
	sort.SliceStable(out.ChangeRequests, func(i, j int) bool {
		return changeRequestIDLess(out.ChangeRequests[j], out.ChangeRequests[i])
	})
	return out, nil
}

// changeRequestIDLess orders two change requests by their numeric id. A
// non-numeric id falls back to a string order, so the sort stays total.
// Call it with the operands swapped to order newest first.
func changeRequestIDLess(a, b jobBranchHeadJSON) bool {
	idA, idB := "", ""
	if a.ChangeRequest != nil {
		idA = a.ChangeRequest.ID
	}
	if b.ChangeRequest != nil {
		idB = b.ChangeRequest.ID
	}
	numA, errA := strconv.Atoi(idA)
	numB, errB := strconv.Atoi(idB)
	if errA == nil && errB == nil {
		return numA < numB
	}
	return idA < idB
}

// branchScanOf converts a stored scan into its response shape.
func branchScanOf(scan recordstore.BranchScan) *branchScanJSON {
	out := branchScanJSON{
		ScanID:        scan.ScanID,
		State:         scan.State,
		Actor:         scan.Actor,
		RequestedAt:   scan.RequestedAt,
		EndedAt:       scan.EndedAt,
		Result:        scan.Result,
		NewHeads:      scan.NewHeads,
		ChangedHeads:  scan.ChangedHeads,
		OrphanedHeads: scan.OrphanedHeads,
		Message:       scan.Message,
	}
	if out.NewHeads == nil {
		out.NewHeads = []string{}
	}
	if out.ChangedHeads == nil {
		out.ChangedHeads = []string{}
	}
	if out.OrphanedHeads == nil {
		out.OrphanedHeads = []string{}
	}
	return &out
}

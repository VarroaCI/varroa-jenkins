// Package api: the pipelines read paths (design D1, D2, D7). Router
// registration lives in router.go, which lists the twelve pipelines
// routes together. This file implements the team-scoped paths:
// GET /pipelines/{team}, GET /pipelines/{team}/jobs/{job}, and
// GET /pipelines/{team}/jobs/{job}/runs. POST .../runs lives in
// handlers_pipelines_write.go, alongside the run-scoped write paths.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/rbac"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// runningNowPageLimit is design D7's page size: one page per tenant, per
// status value, capped so a webhook herd cannot make the team home read
// unbounded rows.
const runningNowPageLimit = 50

// crossTenantExhaustedCursor marks a tenant with no more rows in a
// cross-tenant runs cursor. It is never valid base64 the same field
// would otherwise decode to a real recordstore cursor.
const crossTenantExhaustedCursor = "exhausted"

// --- Response shapes (api/openapi/components/schemas.yaml) ---

type runSummaryJSON struct {
	RunID         string             `json:"runId"`
	BuildNumber   int64              `json:"buildNumber,omitempty"`
	JobPath       string             `json:"jobPath,omitempty"`
	Status        string             `json:"status"`
	Result        string             `json:"result,omitempty"`
	Branch        string             `json:"branch,omitempty"`
	SCMRevision   string             `json:"scmRevision,omitempty"`
	Actor         string             `json:"actor,omitempty"`
	Cause         string             `json:"cause,omitempty"`
	SCMURL        string             `json:"scmUrl,omitempty"`
	SCMForge      string             `json:"scmForge,omitempty"`
	TriggerSource string             `json:"triggerSource,omitempty"`
	ChangeRequest *changeRequestJSON `json:"changeRequest,omitempty"`
	QueuedAt      *time.Time         `json:"queuedAt,omitempty"`
	StartedAt     *time.Time         `json:"startedAt,omitempty"`
	EndedAt       *time.Time         `json:"endedAt,omitempty"`
	DurationMs    int64              `json:"durationMs,omitempty"`
}

type runningRunJSON struct {
	RunID             string             `json:"runId"`
	JobPath           string             `json:"jobPath"`
	BuildNumber       int64              `json:"buildNumber,omitempty"`
	Status            string             `json:"status"`
	CurrentStage      string             `json:"currentStage,omitempty"`
	Branch            string             `json:"branch,omitempty"`
	SCMRevision       string             `json:"scmRevision,omitempty"`
	ChangeRequest     *changeRequestJSON `json:"changeRequest,omitempty"`
	ChangeSummary     string             `json:"changeSummary,omitempty"`
	QueuedAt          *time.Time         `json:"queuedAt,omitempty"`
	StartedAt         *time.Time         `json:"startedAt,omitempty"`
	TypicalDurationMs int64              `json:"typicalDurationMs,omitempty"`
}

type pipelineSummaryJSON struct {
	JobPath string `json:"jobPath"`
	Health  string `json:"health"`
	// Kind is the item kind the tenant's bundle declares: "pipeline",
	// "multibranch", or "unknown". It is set at assembly, never on the row,
	// because a record summary replaces the whole row.
	Kind string `json:"kind"`
	// Pending is true when the tenant's complete inventory lacks this
	// declared job. It is set at assembly, like Kind.
	Pending         bool            `json:"pending"`
	LastBuild       *runSummaryJSON `json:"lastBuild,omitempty"`
	DurationTrendMs []int64         `json:"durationTrendMs,omitempty"`
	// MedianQueueWaitMs is the job's median queue wait in milliseconds
	// over the trailing window. It is absent when the job started no
	// build in that window.
	MedianQueueWaitMs *int64 `json:"medianQueueWaitMs,omitempty"`
}

type pipelineListResponse struct {
	Items      []pipelineSummaryJSON `json:"items"`
	RunningNow []runningRunJSON      `json:"runningNow"`
	// TenantFolder is the Jenkins folder that prefixes every job path in
	// the response. It is always present, and empty for a controller
	// tenant. The response carries no count of the rows the caller's job
	// globs removed: such a count is itself a disclosure.
	TenantFolder string `json:"tenantFolder"`
	Truncated    bool   `json:"truncated"`
	// Retention reports the record-store windows that apply to this team.
	Retention pipelineRetentionJSON `json:"retention"`
}

// pipelineDefinitionJSON is the Defined by card's data. Source names
// where the pipeline script comes from: "jenkinsfile" for a script read
// from source control, "inline" for a script the bundle item carries,
// "template" for an item whose definition names a catalog template. A
// marker-defined multibranch job reads its marker from a repository, so
// it stays "jenkinsfile"; the template row names its template.
type pipelineDefinitionJSON struct {
	Source            string          `json:"source"`
	JenkinsfileRepo   string          `json:"jenkinsfileRepo,omitempty"`
	JenkinsfileCommit string          `json:"jenkinsfileCommit,omitempty"`
	Profile           string          `json:"profile,omitempty"`
	DeclaredBy        *declaredByJSON `json:"declaredBy,omitempty"`
	DefinitionPath    string          `json:"definitionPath,omitempty"`
}

// pipelineTemplateInfoJSON is design D12's Template row: the resolved
// catalog template for a job. Source "marker" reads the record store for
// a marker-defined job; source "items" reads the items engine and the
// catalog for an items-declared job. Absent from pipelineJobDetailJSON
// for a job with no catalog template.
type pipelineTemplateInfoJSON struct {
	Name           string `json:"name,omitempty"`
	Version        string `json:"version,omitempty"`
	MarkerPath     string `json:"markerPath,omitempty"`
	MarkerRevision string `json:"markerRevision,omitempty"`
	Source         string `json:"source"`
	AsOfBuild      int64  `json:"asOfBuild,omitempty"`
}

type pipelineTriggersJSON struct {
	Push        bool   `json:"push,omitempty"`
	PullRequest bool   `json:"pullRequest,omitempty"`
	Webhook     bool   `json:"webhook,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

type pipelineParameterJSON struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	Kind         string         `json:"kind"`
	Schema       map[string]any `json:"schema"`
	UIWidget     string         `json:"uiWidget,omitempty"`
	DefaultValue string         `json:"defaultValue,omitempty"`
}

type pipelineJobDetailJSON struct {
	JobPath string `json:"jobPath"`
	// Kind is pipeline, multibranch, or unknown. The job page reads it to
	// choose its card.
	Kind string `json:"kind"`
	// Pending is true when the tenant's complete inventory lacks this
	// declared job. It is set at assembly, like Kind.
	Pending         bool                    `json:"pending"`
	Health          string                  `json:"health"`
	TenantFolder    string                  `json:"tenantFolder"`
	Tenant          string                  `json:"tenant"`
	LastBuild       *runSummaryJSON         `json:"lastBuild,omitempty"`
	DurationTrendMs []int64                 `json:"durationTrendMs,omitempty"`
	Definition      *pipelineDefinitionJSON `json:"definition,omitempty"`
	Triggers        *pipelineTriggersJSON   `json:"triggers,omitempty"`
	// Waiting lists the queued runs of this job that a concurrency cap
	// holds. It is absent when none waits, or when the read failed.
	Waiting      []runWaitEntry            `json:"waiting,omitempty"`
	Parameters   []pipelineParameterJSON   `json:"parameters,omitempty"`
	Template     *pipelineTemplateInfoJSON `json:"template,omitempty"`
	Last30Days   *pipelineLast30DaysJSON   `json:"last30Days"`
	Dependencies pipelineDependenciesJSON  `json:"dependencies"`
}

// pipelineDependencyEdgeJSON is one cross-tenant dependency edge (design
// D13): a producer tenant and job, the result threshold the edge fires
// on, and whether the producer's exports currently grant it. Job is
// relative to the other tenant's folder. JobPath is the path the job
// page route takes. Granted false marks a declared edge whose grant is
// missing; it is never dropped from the list.
type pipelineDependencyEdgeJSON struct {
	Tenant  string `json:"tenant"`
	Job     string `json:"job"`
	JobPath string `json:"jobPath"`
	Result  string `json:"result"`
	Granted bool   `json:"granted"`
	// LastRun is the edge's most recent run within 30 days, redacted for the
	// reader. It is absent when no such run exists.
	LastRun *RunLink `json:"lastRun,omitempty"`
}

// pipelineDependenciesJSON is the job detail's dependency edge object.
// Both arrays are empty, never omitted, for a job with no edge.
type pipelineDependenciesJSON struct {
	Upstream   []pipelineDependencyEdgeJSON `json:"upstream"`
	Downstream []pipelineDependencyEdgeJSON `json:"downstream"`
}

// buildPipelineDependencies converts the record store's dependency edges
// into the API shape. Each edge's last run passes the same run read check
// as GET /runs/{run}, so the reader sees a tenant alone for a run it may
// not read.
func (s *Server) buildPipelineDependencies(claims *auth.Claims, deps recordstore.JobDependencies) pipelineDependenciesJSON {
	out := pipelineDependenciesJSON{
		Upstream:   make([]pipelineDependencyEdgeJSON, 0, len(deps.Upstream)),
		Downstream: make([]pipelineDependencyEdgeJSON, 0, len(deps.Downstream)),
	}
	for _, edge := range deps.Upstream {
		out.Upstream = append(out.Upstream, s.dependencyEdgeJSON(claims, edge))
	}
	for _, edge := range deps.Downstream {
		out.Downstream = append(out.Downstream, s.dependencyEdgeJSON(claims, edge))
	}
	return out
}

// dependencyEdgeJSON maps one edge to its API shape, redacting its last run.
func (s *Server) dependencyEdgeJSON(claims *auth.Claims, edge recordstore.DependencyEdge) pipelineDependencyEdgeJSON {
	out := pipelineDependencyEdgeJSON{
		Tenant: edge.Tenant, Job: edge.Job, JobPath: edge.JobPath, Result: edge.Result, Granted: edge.Granted,
	}
	if edge.LastRun != nil {
		last := s.redactRunLink(claims, RunLink{
			Tenant:      edge.LastRun.Tenant,
			JobPath:     edge.LastRun.JobPath,
			RunID:       edge.LastRun.RunID,
			BuildNumber: derefInt64(edge.LastRun.BuildNumber),
			Status:      edge.LastRun.Status,
			Result:      edge.LastRun.Result,
		})
		out.LastRun = &last
	}
	return out
}

// derefInt64 returns *v, or 0 when v is nil.
func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// pipelineLast30DaysJSON is design D8's "Last 30 days" card. PassRate,
// MedianDurationMs, MedianQueueWaitMs, and SlowestStage stay nil when
// the window holds no finished run, no started build, or no finished
// stage. handleGetPipelineJob sets the whole field to nil, not this
// struct's zero value, when the record-store call itself fails.
type pipelineLast30DaysJSON struct {
	Builds            int64                     `json:"builds"`
	PassRate          *float64                  `json:"passRate,omitempty"`
	MedianDurationMs  *int64                    `json:"medianDurationMs,omitempty"`
	MedianQueueWaitMs *int64                    `json:"medianQueueWaitMs,omitempty"`
	SlowestStage      *pipelineSlowestStageJSON `json:"slowestStage,omitempty"`
}

type pipelineSlowestStageJSON struct {
	Name       string `json:"name"`
	DurationMs int64  `json:"durationMs"`
}

type runListResponse struct {
	Items      []runSummaryJSON `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// --- Shared helpers ---

// pipelinesUnavailable answers 503 with the uniform error envelope,
// design D12's contract for a disabled or too-old record store.
func (s *Server) pipelinesUnavailable(w http.ResponseWriter) {
	s.writeJSONError(w, http.StatusServiceUnavailable, "record store not configured")
}

// handleReaderError maps a recordstore.Reader failure onto its HTTP
// shape: ErrSchemaTooOld is 503, everything else is 500. It returns
// true when it wrote a response, so the caller returns immediately.
func (s *Server) handleReaderError(w http.ResponseWriter, err error, action string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, recordstore.ErrSchemaTooOld) {
		s.pipelinesUnavailable(w)
		return true
	}
	s.deps.Logger.Error(action, "error", err)
	s.writeJSONError(w, http.StatusInternalServerError, action+" failed")
	return true
}

// resolveTeamTenants resolves team through TenantResolver, translating a
// nil resolver and a *ServiceError into the response the handler writes.
func (s *Server) resolveTeamTenants(ctx context.Context, claims *auth.Claims, team string) ([]TenantRef, *ServiceError) {
	if s.deps.TenantResolver == nil {
		return nil, &ServiceError{Status: http.StatusServiceUnavailable, Message: "record store not configured"}
	}
	refs, err := s.deps.TenantResolver.Resolve(ctx, claims, team)
	if err != nil {
		var serr *ServiceError
		if errors.As(err, &serr) {
			return nil, serr
		}
		return nil, &ServiceError{Status: http.StatusInternalServerError, Message: "resolve team failed"}
	}
	return refs, nil
}

// tenantForJob returns the first of refs the caller may read at jobPath.
// Resolve already checked the whole-tenant grant; a concrete job path
// meets the grant's globs on its own, per design D1a.
func (s *Server) tenantForJob(claims *auth.Claims, refs []TenantRef, jobPath string) (TenantRef, bool) {
	for _, ref := range refs {
		if s.canReadPipeline(claims, ref, jobPath) {
			return ref, true
		}
	}
	return TenantRef{}, false
}

// buildRunSummary converts a record-store run into the API's condensed
// run shape, used for lastBuild and every run-list row.
func buildRunSummary(run recordstore.Run) runSummaryJSON {
	s := runSummaryJSON{RunID: run.RunID, JobPath: run.JobPath, Status: run.Status}
	if run.BuildNumber != nil {
		s.BuildNumber = *run.BuildNumber
	}
	if run.Result != nil {
		s.Result = *run.Result
	}
	if run.Branch != nil {
		s.Branch = *run.Branch
	}
	if run.SCMRevision != nil {
		s.SCMRevision = *run.SCMRevision
	}
	if run.Actor != nil {
		s.Actor = *run.Actor
	}
	s.Cause = causeSummary(run.Cause)
	o := runOriginOf(run.Payload)
	s.SCMURL, s.SCMForge, s.TriggerSource = o.SCMURL, o.SCMForge, o.TriggerSource
	s.ChangeRequest = changeRequestOf(run.Payload)
	s.QueuedAt = run.QueuedAt
	s.StartedAt = run.StartedAt
	s.EndedAt = run.EndedAt
	if run.DurationMs != nil {
		s.DurationMs = *run.DurationMs
	}
	return s
}

// causeSummary reads runs.cause. The gateway writes the cause as a
// plain JSON string. It also reads an object or an array with a
// shortDescription field, for an older payload shape. An unrecognized
// shape yields the empty string.
func causeSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	type causeShape struct {
		ShortDescription string `json:"shortDescription"`
	}
	var single causeShape
	if json.Unmarshal(raw, &single) == nil && single.ShortDescription != "" {
		return single.ShortDescription
	}
	var list []causeShape
	if json.Unmarshal(raw, &list) == nil {
		for _, c := range list {
			if c.ShortDescription != "" {
				return c.ShortDescription
			}
		}
	}
	return ""
}

// latestChangeMessage reads runs.changes, the ordered array of commit
// entries the gateway writes oldest first. It returns the newest
// entry's commit message, or the empty string when there is no entry
// or the newest entry carries no message.
func latestChangeMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var entries []struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &entries) != nil || len(entries) == 0 {
		return ""
	}
	return entries[len(entries)-1].Message
}

// computeHealth implements design D8's weather glyph: clear when every
// one of the job's last five finished runs passed, cloudy when one or
// two failed, and rain when three or more failed. unknown when the job
// has no finished run yet, whether it has never run or every run so
// far is still queued or running.
func computeHealth(js recordstore.JobSummary) string {
	if len(js.RecentResults) == 0 {
		return "unknown"
	}
	var failed int
	for _, result := range js.RecentResults {
		if result != "SUCCESS" {
			failed++
		}
	}
	switch {
	case failed == 0:
		return "clear"
	case failed <= 2:
		return "cloudy"
	default:
		return "rain"
	}
}

// buildPipelineSummary converts one job's record-store summary into the
// Pipelines table row.
func buildPipelineSummary(js recordstore.JobSummary) pipelineSummaryJSON {
	out := pipelineSummaryJSON{JobPath: js.JobPath, Health: computeHealth(js), DurationTrendMs: js.DurationTrendMs}
	if js.LastRun != nil {
		rs := buildRunSummary(*js.LastRun)
		out.LastBuild = &rs
	}
	out.MedianQueueWaitMs = js.MedianQueueWaitMs
	return out
}

// averageInt64 returns the mean of vs, or 0 for an empty slice.
func averageInt64(vs []int64) int64 {
	if len(vs) == 0 {
		return 0
	}
	var sum int64
	for _, v := range vs {
		sum += v
	}
	return sum / int64(len(vs))
}

// buildRunningRun converts one unfinished run into the Running now tile.
// trendMs is the job's duration trend, when known, for typicalDurationMs.
// currentStage is the run's stage in progress. It is empty when the run
// holds no such stage, and the tile then names the run's own state.
// ChangeSummary reads runs.changes, which the gateway fills only at the
// completed phase, so a QUEUED or RUNNING row has no entry yet and the
// tile shows no change summary until the build finishes.
func buildRunningRun(run recordstore.Run, trendMs []int64, currentStage string) runningRunJSON {
	out := runningRunJSON{RunID: run.RunID, JobPath: run.JobPath, Status: run.Status}
	if run.BuildNumber != nil {
		out.BuildNumber = *run.BuildNumber
	}
	if run.Branch != nil {
		out.Branch = *run.Branch
	}
	if run.SCMRevision != nil {
		out.SCMRevision = *run.SCMRevision
	}
	out.CurrentStage = currentStage
	out.ChangeRequest = changeRequestOf(run.Payload)
	out.ChangeSummary = latestChangeMessage(run.Changes)
	out.QueuedAt = run.QueuedAt
	out.StartedAt = run.StartedAt
	out.TypicalDurationMs = averageInt64(trendMs)
	return out
}

// runningNowForTenant reads ref's unfinished runs: design D7 restricts
// the query to QUEUED and RUNNING rather than reading recent runs and
// discarding the finished ones. RunQuery.Status takes one value, so this
// issues one call per status and merges them. truncated is true when
// either page was full.
func (s *Server) runningNowForTenant(ctx context.Context, ref TenantRef) ([]recordstore.Run, bool, error) {
	var all []recordstore.Run
	truncated := false
	for _, status := range []string{"QUEUED", "RUNNING"} {
		page, err := s.deps.RecordStore.ListRuns(ctx, recordstore.RunQuery{
			Cluster: ref.Cluster,
			Tenant:  ref.Tenant,
			Status:  status,
			Limit:   runningNowPageLimit,
		})
		if err != nil {
			return nil, false, err
		}
		all = append(all, page.Runs...)
		if page.NextCursor != "" {
			truncated = true
		}
	}
	return all, truncated, nil
}

// --- GET /pipelines/{team} ---

func (s *Server) handleListPipelines(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	team := r.PathValue("team")

	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}

	jobs := map[string]pipelineSummaryJSON{}
	trendByJob := map[string][]int64{}
	kindByJob := map[string]string{}
	pendingByJob := map[string]bool{}
	hasRun := map[string]bool{}
	var runningRuns []recordstore.Run
	truncated := false

	// Every tenant one team resolves to shares one folder, so one envelope
	// field describes the whole response. A ref that Resolve admitted still
	// supplies the folder when the row filter below refuses every one of
	// its rows.
	tenantFolder := ""
	if len(refs) > 0 {
		tenantFolder = refs[0].Folder
	}

	// One window serves every tenant, so a later tenant's read never
	// measures a different 30 days than the first.
	since := time.Now().UTC().AddDate(0, 0, -pipelineWindowDays)

	for _, ref := range refs {
		// The grants resolve once per tenant, never once per row. The row
		// filter then runs before the rows of this ref reach the shared
		// maps, so a refused row of one tenant cannot replace an admitted
		// row of another tenant that carries the same job path.
		grants, granted := s.deps.Authorizer.tenantGrants(claims, ref.Tenant, ref.Folder)

		summaries, err := s.deps.RecordStore.ListJobSummaries(ctx, ref.Cluster, ref.Tenant, since)
		if s.handleReaderError(w, err, "list job summaries") {
			return
		}
		for _, js := range summaries {
			if !granted || !grantsAllow(grants, ref.Folder, js.JobPath, "read") {
				continue
			}
			jobs[js.JobPath] = buildPipelineSummary(js)
			if len(js.DurationTrendMs) > 0 {
				trendByJob[js.JobPath] = js.DurationTrendMs
			}
			if js.LastRun != nil {
				hasRun[js.JobPath] = true
			}
		}
		declared := s.catalogDeclaredJobPaths(ctx, ref)
		var inv recordstore.JobInventory
		if len(declared) > 0 {
			inv = s.readJobInventory(ctx, ref)
		}
		for _, dj := range declared {
			if !granted || !grantsAllow(grants, ref.Folder, dj.Path, "read") {
				continue
			}
			if _, ok := jobs[dj.Path]; !ok {
				jobs[dj.Path] = pipelineSummaryJSON{JobPath: dj.Path, Health: "unknown"}
			}
			// A record summary writes no kind. A declared item sets the
			// kind when none is set, and "multibranch" replaces a stored
			// "pipeline", so two tenants that declare one job path with
			// different kinds agree whichever one the handler reads first.
			if stored, ok := kindByJob[dj.Path]; !ok || (stored == "pipeline" && dj.Kind == "multibranch") {
				kindByJob[dj.Path] = dj.Kind
			}
			// Any tenant that does not report the job pending wins.
			p := declaredJobPending(inv, dj.Path, dj.Kind)
			if prev, seen := pendingByJob[dj.Path]; seen {
				pendingByJob[dj.Path] = prev && p
			} else {
				pendingByJob[dj.Path] = p
			}
		}

		runs, tenantTruncated, err := s.runningNowForTenant(ctx, ref)
		if s.handleReaderError(w, err, "list running runs") {
			return
		}
		for _, run := range runs {
			if !granted || !grantsAllow(grants, ref.Folder, run.JobPath, "read") {
				continue
			}
			runningRuns = append(runningRuns, run)
		}
		// truncated reports the page the record store answered, not the
		// filter above. A query matches one exact job path, so the filter
		// cannot narrow the query, and a narrowed caller reads fewer tiles
		// than the page held.
		truncated = truncated || tenantTruncated
	}

	items := make([]pipelineSummaryJSON, 0, len(jobs))
	for _, js := range jobs {
		js.Kind = kindByJob[js.JobPath]
		if js.Kind == "" {
			js.Kind = "unknown"
		}
		js.Pending = pendingByJob[js.JobPath] && !hasRun[js.JobPath]
		items = append(items, js)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].JobPath < items[j].JobPath })

	// One stage read answers every tile of this page, so the strip never
	// issues one read per run (design D7's bound). The list holds the run
	// ids the row filters admitted, so the read names no run this response
	// drops.
	runIDs := make([]string, 0, len(runningRuns))
	for _, run := range runningRuns {
		runIDs = append(runIDs, run.RunID)
	}
	currentStages, err := s.deps.RecordStore.CurrentStages(ctx, runIDs)
	if s.handleReaderError(w, err, "list current stages") {
		return
	}

	runningOut := make([]runningRunJSON, 0, len(runningRuns))
	for _, run := range runningRuns {
		runningOut = append(runningOut, buildRunningRun(run, trendByJob[run.JobPath], currentStages[run.RunID]))
	}
	sort.Slice(runningOut, func(i, j int) bool {
		if runningOut[i].JobPath != runningOut[j].JobPath {
			return runningOut[i].JobPath < runningOut[j].JobPath
		}
		return runningOut[i].RunID < runningOut[j].RunID
	})

	// The retention block reads the first tenant's policy. Every tenant
	// one team resolves to shares one placement policy in practice.
	var policy recordstore.TenantRetentionPolicy
	if len(refs) > 0 {
		p, err := s.deps.RecordStore.TenantRetention(ctx, refs[0].Cluster, refs[0].Tenant)
		if s.handleReaderError(w, err, "read tenant retention") {
			return
		}
		policy = p
	}

	s.writeJSON(w, http.StatusOK, pipelineListResponse{
		Items:        items,
		RunningNow:   runningOut,
		TenantFolder: tenantFolder,
		Truncated:    truncated,
		Retention:    buildPipelineRetention(s.deps.RecordStoreRunRetention, policy),
	})
}

// --- GET /pipelines/{team}/jobs/{job} ---

func (s *Server) handleGetPipelineJob(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	team := r.PathValue("team")
	jobPath := r.PathValue("job")

	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	ref, ok := s.tenantForJob(claims, refs, jobPath)
	if !ok {
		s.writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}

	summary, err := s.deps.RecordStore.GetJobSummary(ctx, ref.Cluster, ref.Tenant, jobPath)
	if s.handleReaderError(w, err, "get job summary") {
		return
	}

	detail := pipelineJobDetailJSON{JobPath: jobPath, Health: computeHealth(summary), TenantFolder: ref.Folder, Tenant: ref.Tenant, DurationTrendMs: summary.DurationTrendMs}
	if summary.LastRun != nil {
		rs := buildRunSummary(*summary.LastRun)
		detail.LastBuild = &rs
	}

	cr, item, declaredBy, err := s.findDeclaredItem(ctx, ref, jobPath)
	if err != nil {
		s.deps.Logger.Warn("pipeline job definition lookup failed", "jobPath", jobPath, "error", err)
	}
	if item != nil {
		detail.Definition = buildPipelineDefinition(item)
		if detail.Definition != nil {
			detail.Definition.DeclaredBy = declaredBy
			if cr != nil {
				detail.Definition.Profile = cr.Spec.Version
			}
			// A checked-out commit belongs to a repository script only.
			if detail.Definition.Source == "jenkinsfile" && summary.LastRun != nil && summary.LastRun.SCMRevision != nil {
				detail.Definition.JenkinsfileCommit = *summary.LastRun.SCMRevision
			}
			// The script path names a file inside that repository, so
			// only a repository script carries one.
			if detail.Definition.Source == "jenkinsfile" && summary.LastRun != nil {
				detail.Definition.DefinitionPath = lastRunDefinitionPath(summary.LastRun)
			}
		}
		detail.Triggers = buildPipelineTriggers(item)
		detail.Parameters = buildPipelineParameters(item)
	}
	detail.Template = s.buildPipelineTemplateInfo(ctx, ref, jobPath, item, summary.LastRun)

	detail.Last30Days = s.buildLast30Days(ctx, ref, jobPath)

	deps, err := s.deps.RecordStore.JobDependencies(ctx, ref.Cluster, ref.Tenant, jobPath)
	if s.handleReaderError(w, err, "get job dependencies") {
		return
	}
	detail.Dependencies = s.buildPipelineDependencies(claims, deps)

	waits, werr := s.deps.RecordStore.ListJobRunWaits(ctx, ref.Cluster, ref.Tenant, jobPath)
	if werr != nil {
		s.deps.Logger.Warn("list job run waits failed", "jobPath", jobPath, "error", werr)
	}
	for _, w := range waits {
		detail.Waiting = append(detail.Waiting, runWaitEntry{RunID: w.RunID, RunWaitJSON: RunWaitJSON{
			Kind: w.Kind, Limit: w.Limit, Max: w.Max, Running: w.Running, Cell: w.Cell, Since: w.Since}})
	}

	_, hasBranches, err := s.deps.RecordStore.GetJobBranches(ctx, ref.Cluster, ref.Tenant, jobPath)
	if s.handleReaderError(w, err, "get job branches") {
		return
	}
	declaredKind := ""
	if item != nil {
		declaredKind = item.Kind
	}
	detail.Kind = pipelineJobKind(declaredKind, hasBranches, summary.LastRun != nil)
	if summary.LastRun == nil {
		detail.Pending = s.jobPendingAcross(ctx, claims, refs, jobPath)
	}

	s.writeJSON(w, http.StatusOK, detail)
}

// declaredJobPending answers whether inv proves that the declared job at
// path is absent from its cell. An unknown inventory proves nothing.
func declaredJobPending(inv recordstore.JobInventory, path, kind string) bool {
	switch kind {
	case "pipeline":
		if !inv.JobsKnown {
			return false
		}
		_, ok := inv.Jobs[path]
		return !ok
	case "multibranch":
		if !inv.ProjectsKnown {
			return false
		}
		_, ok := inv.Projects[path]
		return !ok
	}
	return false
}

// jobPendingAcross answers whether a job of the team is pending. It runs in
// two passes over the refs the caller reads. First, a run of jobPath on any
// readable tenant means not pending, the same rule the list applies through
// hasRun. A summary read that fails also means not pending. Second, every
// tenant that declares jobPath must report it absent from a complete
// inventory. No declaring tenant answers false.
func (s *Server) jobPendingAcross(ctx context.Context, claims *auth.Claims, refs []TenantRef, jobPath string) bool {
	var readable []TenantRef
	for _, ref := range refs {
		grants, granted := s.deps.Authorizer.tenantGrants(claims, ref.Tenant, ref.Folder)
		if !granted || !grantsAllow(grants, ref.Folder, jobPath, "read") {
			continue
		}
		readable = append(readable, ref)
		summary, err := s.deps.RecordStore.GetJobSummary(ctx, ref.Cluster, ref.Tenant, jobPath)
		if err != nil || summary.LastRun != nil {
			return false
		}
	}

	declaredAny := false
	for _, ref := range readable {
		kind := ""
		for _, dj := range s.catalogDeclaredJobPaths(ctx, ref) {
			if dj.Path == jobPath {
				kind = dj.Kind
				break
			}
		}
		if kind == "" {
			continue
		}
		if !declaredJobPending(s.readJobInventory(ctx, ref), jobPath, kind) {
			return false
		}
		declaredAny = true
	}
	return declaredAny
}

// readJobInventory reads ref's job inventory. A failed read answers the
// zero value, which marks no job pending.
func (s *Server) readJobInventory(ctx context.Context, ref TenantRef) recordstore.JobInventory {
	inv, err := s.deps.RecordStore.JobInventory(ctx, ref.Cluster, ref.Tenant)
	if err != nil {
		s.deps.Logger.Warn("read job inventory failed", "cluster", ref.Cluster, "tenant", ref.Tenant, "error", err)
		return recordstore.JobInventory{}
	}
	return inv
}

// pipelineJobKind answers the kind the job page renders. A declared
// pipeline or multibranch item wins. Without one, a branch inventory row
// marks a multibranch job, and a run marks a pipeline job. Any other job
// is unknown.
func pipelineJobKind(declaredKind string, hasBranches, hasRun bool) string {
	switch declaredKind {
	case "pipeline", "multibranch":
		return declaredKind
	}
	if hasBranches {
		return "multibranch"
	}
	if hasRun {
		return "pipeline"
	}
	return "unknown"
}

// pipelineWindowDays is design D8's "Last 30 days" card window.
const pipelineWindowDays = 30

// buildLast30Days reads JobWindowStats for the trailing pipelineWindowDays
// days. A record-store failure on this call degrades the card to nil
// rather than failing the whole job page, because every other field on
// the response already resolved.
func (s *Server) buildLast30Days(ctx context.Context, ref TenantRef, jobPath string) *pipelineLast30DaysJSON {
	since := time.Now().UTC().AddDate(0, 0, -pipelineWindowDays)
	stats, err := s.deps.RecordStore.JobWindowStats(ctx, ref.Cluster, ref.Tenant, jobPath, since)
	if err != nil {
		s.deps.Logger.Warn("pipeline job window stats failed", "jobPath", jobPath, "error", err)
		return nil
	}
	out := &pipelineLast30DaysJSON{
		Builds:            stats.Builds,
		PassRate:          stats.PassRate,
		MedianDurationMs:  stats.MedianDurationMs,
		MedianQueueWaitMs: stats.MedianQueueWaitMs,
	}
	if stats.SlowestStage != nil {
		out.SlowestStage = &pipelineSlowestStageJSON{Name: stats.SlowestStage.Name, DurationMs: stats.SlowestStage.DurationMs}
	}
	return out
}

// --- GET/POST /pipelines/{team}/jobs/{job}/runs ---

// listRunsFilters carries the four run-list filters, decoded from the
// request's query parameters.
type listRunsFilters struct {
	branch, result, actor, cause string
}

func parseListRunsFilters(r *http.Request) listRunsFilters {
	v := r.URL.Query()
	return listRunsFilters{
		branch: v.Get("branch"),
		result: v.Get("result"),
		actor:  v.Get("actor"),
		cause:  v.Get("cause"),
	}
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	team := r.PathValue("team")
	jobPath := r.PathValue("job")

	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	var admitted []TenantRef
	for _, ref := range refs {
		if s.canReadPipeline(claims, ref, jobPath) {
			admitted = append(admitted, ref)
		}
	}
	if len(admitted) == 0 {
		s.writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}

	limit, err := parseRunsLimit(r)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, serr2, err := s.listRunsMerged(ctx, admitted, jobPath, parseListRunsFilters(r), limit, r.URL.Query().Get("cursor"))
	if serr2 != nil {
		s.writeServiceError(w, serr2)
		return
	}
	if s.handleReaderError(w, err, "list runs") {
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func parseRunsLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 200 {
		return 0, errors.New("limit must be between 1 and 200")
	}
	return n, nil
}

// --- GET /pipelines/{team}/jobs/{job}/tests/{case} ---

// testCaseHistoryDefaultLimit and testCaseHistoryMaxLimit bound the case
// history read. A limit above the maximum is clamped, never refused.
const (
	testCaseHistoryDefaultLimit = 50
	testCaseHistoryMaxLimit     = 200
)

// testCaseOutcomeJSON is one row of the case history read: one run's
// outcome for one class and test name. RunLink is the run's own page
// path, which the caller's own tenant read check has already admitted.
type testCaseOutcomeJSON struct {
	RunID        string    `json:"runId"`
	BuildNumber  int64     `json:"buildNumber,omitempty"`
	Suite        string    `json:"suite"`
	Status       string    `json:"status"`
	DurationMs   int64     `json:"durationMs,omitempty"`
	ErrorDetails string    `json:"errorDetails,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	Result       string    `json:"result,omitempty"`
	RunLink      string    `json:"runLink"`
}

// testCaseHistoryResponse is the case history envelope. Items is never
// null: a job with no stored case answers an empty array.
type testCaseHistoryResponse struct {
	Items []testCaseOutcomeJSON `json:"items"`
}

// parseTestCaseHistoryLimit reads the optional limit query parameter. It
// answers the default for an absent value and clamps a larger value to
// the maximum. A value that is not a positive integer answers an error.
func parseTestCaseHistoryLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return testCaseHistoryDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	if n > testCaseHistoryMaxLimit {
		n = testCaseHistoryMaxLimit
	}
	return n, nil
}

// handleListJobTestCaseHistory serves one test case's recent outcomes for
// one job. It resolves the team the way handleListRuns does, applies the
// tenant read check to each tenant with the decoded job path, and reads
// only the admitted tenants. A job the store holds no case row for
// answers an empty list, never 404: the case rows hold no job inventory,
// and GET /pipelines/{team}/jobs/{job} answers whether a job exists.
func (s *Server) handleListJobTestCaseHistory(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	team := r.PathValue("team")
	jobPath := r.PathValue("job")

	refs, serr := s.resolveTeamTenants(ctx, claims, team)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	var admitted []TenantRef
	for _, ref := range refs {
		if s.canReadPipeline(claims, ref, jobPath) {
			admitted = append(admitted, ref)
		}
	}
	if len(admitted) == 0 {
		s.writeJSONError(w, http.StatusNotFound, "job not found")
		return
	}

	className, name, err := ParseTestCaseKey(r.PathValue("case"))
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "malformed test case key")
		return
	}
	limit, err := parseTestCaseHistoryLimit(r)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	items := []testCaseOutcomeJSON{}
	for _, ref := range admitted {
		outcomes, err := s.deps.RecordStore.ListTestCaseHistory(ctx, recordstore.TestCaseHistoryQuery{
			Cluster:   ref.Cluster,
			Tenant:    ref.Tenant,
			JobPath:   jobPath,
			ClassName: className,
			Name:      name,
			Limit:     limit,
		})
		if s.handleReaderError(w, err, "list test case history") {
			return
		}
		for _, o := range outcomes {
			items = append(items, buildTestCaseOutcome(ref, jobPath, o))
		}
	}
	// Each tenant answers its own rows newest first. The merge keeps that
	// order across tenants, and the run id orders two rows of one time.
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].RunID > items[j].RunID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	s.writeJSON(w, http.StatusOK, testCaseHistoryResponse{Items: items})
}

// buildTestCaseOutcome maps one stored outcome onto the response shape.
func buildTestCaseOutcome(ref TenantRef, jobPath string, o recordstore.TestCaseOutcome) testCaseOutcomeJSON {
	out := testCaseOutcomeJSON{
		RunID:        o.RunID,
		Suite:        o.Suite,
		Status:       o.Status,
		ErrorDetails: o.ErrorDetails,
		CreatedAt:    o.CreatedAt,
		RunLink:      searchRunLink(ref.Team, jobPath, o.RunID),
	}
	if o.BuildNumber != nil {
		out.BuildNumber = *o.BuildNumber
	}
	if o.DurationMs != nil {
		out.DurationMs = *o.DurationMs
	}
	if o.Result != nil {
		out.Result = *o.Result
	}
	return out
}

// crossTenantCursor is the decoded form of a cross-tenant runs cursor:
// one recordstore cursor (or crossTenantExhaustedCursor) per tenant key
// "<cluster>/<tenant>".
type crossTenantCursor struct {
	Tenants map[string]string `json:"tenants"`
}

func tenantKey(ref TenantRef) string { return ref.Cluster + "/" + ref.Tenant }

func encodeCrossTenantCursor(c crossTenantCursor) (string, error) {
	if len(c.Tenants) == 0 {
		return "", nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

func decodeCrossTenantCursor(s string) (crossTenantCursor, error) {
	if s == "" {
		return crossTenantCursor{}, nil
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return crossTenantCursor{}, err
	}
	var c crossTenantCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return crossTenantCursor{}, err
	}
	return c, nil
}

// runResumeCursor builds a cursor value recordstore's Reader decodes as
// "resume strictly after this row". Its format must match recordstore's
// own (unexported) encodeRunCursor: base64 of the row's RFC3339Nano
// createdAt, "|", then its run id. The merge below needs it only for a
// tenant whose already-fetched batch it did not fully consume; every
// other tenant reuses the cursor its own ListRuns page already returned.
func runResumeCursor(createdAt time.Time, runID string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + runID
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// runAfter reports whether a sorts before b in ListRuns' order:
// created_at descending, then run_id descending.
func runAfter(a, b recordstore.Run) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.RunID > b.RunID
}

// tenantFetch holds one tenant's fetched page and how much of it the
// merge below has consumed.
type tenantFetch struct {
	ref  TenantRef
	page recordstore.RunPage
	used int
}

// listRunsMerged implements the pipelines-api delta's cross-tenant
// cursor merge: it queries every admitted tenant for up to limit rows,
// merges them by (created_at, run_id) descending, and returns the first
// limit. The returned cursor carries one resume position per tenant: a
// live resume point, or the exhausted marker once a tenant's rows are
// all returned. An exhausted tenant's marker stays in the cursor on
// every later page while it remains admitted, so it is never requeried
// and never repeats rows. A tenant dropped from admitted between two
// pages is silently absent from both the query and the next cursor,
// and paging continues.
func (s *Server) listRunsMerged(ctx context.Context, admitted []TenantRef, jobPath string, filters listRunsFilters, limit int, cursor string) (runListResponse, *ServiceError, error) {
	incoming, err := decodeCrossTenantCursor(cursor)
	if err != nil {
		return runListResponse{}, &ServiceError{Status: http.StatusBadRequest, Message: "invalid cursor"}, nil
	}

	// nextTenants starts with every already-exhausted tenant carried
	// forward. Skipping their query below must not drop them: a tenant
	// absent from this map decodes as a fresh, unbounded cursor next
	// round, so an exhausted tenant would requery its whole history
	// and repeat every row it already returned.
	nextTenants := map[string]string{}
	fetches := make([]*tenantFetch, 0, len(admitted))
	for _, ref := range admitted {
		key := tenantKey(ref)
		tenantCursor := incoming.Tenants[key]
		if tenantCursor == crossTenantExhaustedCursor {
			nextTenants[key] = crossTenantExhaustedCursor
			continue
		}
		page, err := s.deps.RecordStore.ListRuns(ctx, recordstore.RunQuery{
			Cluster: ref.Cluster,
			Tenant:  ref.Tenant,
			JobPath: jobPath,
			Branch:  filters.branch,
			Result:  filters.result,
			Actor:   filters.actor,
			Cause:   filters.cause,
			Limit:   limit,
			Cursor:  tenantCursor,
		})
		if err != nil {
			return runListResponse{}, nil, err
		}
		fetches = append(fetches, &tenantFetch{ref: ref, page: page})
	}

	var merged []recordstore.Run
	for len(merged) < limit {
		best := -1
		for i, f := range fetches {
			if f.used >= len(f.page.Runs) {
				continue
			}
			if best == -1 || runAfter(f.page.Runs[f.used], fetches[best].page.Runs[fetches[best].used]) {
				best = i
			}
		}
		if best == -1 {
			break
		}
		merged = append(merged, fetches[best].page.Runs[fetches[best].used])
		fetches[best].used++
	}

	for _, f := range fetches {
		key := tenantKey(f.ref)
		switch {
		case f.used > 0 && f.used < len(f.page.Runs):
			last := f.page.Runs[f.used-1]
			nextTenants[key] = runResumeCursor(last.CreatedAt, last.RunID)
		case f.used == 0 && len(f.page.Runs) > 0:
			// Untouched this page: its fetched rows are all older than
			// everything already selected, so reconsider the same set
			// next page by keeping its incoming resume position.
			nextTenants[key] = incoming.Tenants[key]
		case f.page.NextCursor != "":
			nextTenants[key] = f.page.NextCursor
		default:
			nextTenants[key] = crossTenantExhaustedCursor
		}
	}
	// Drop a tenant no longer admitted, and an exhausted tenant whose
	// incoming entry was also exhausted, so the cursor never grows
	// without bound.
	anyLive := false
	for _, v := range nextTenants {
		if v != crossTenantExhaustedCursor {
			anyLive = true
			break
		}
	}
	if !anyLive {
		nextTenants = nil
	}

	resp := runListResponse{Items: make([]runSummaryJSON, len(merged))}
	for i, run := range merged {
		resp.Items[i] = buildRunSummary(run)
	}
	if len(nextTenants) > 0 {
		cur, err := encodeCrossTenantCursor(crossTenantCursor{Tenants: nextTenants})
		if err != nil {
			return runListResponse{}, nil, err
		}
		resp.NextCursor = cur
	}
	return resp, nil, nil
}

// --- GET /pipelines/{team}/cell ---

// Team cell states. The handler tests them in this order and stops at
// the first match.
const (
	teamCellNotCellHosted = "notCellHosted"
	teamCellUnplaced      = "unplaced"
	teamCellMoving        = "moving"
	teamCellRehydrating   = "rehydrating"
	teamCellPlaced        = "placed"
)

// teamCellJSON is the tenant's own view of the cell that hosts it. An
// absent field is absent from the object: it is never a null and never a
// zero. The body carries no other tenant's name, job path, run, or
// count.
type teamCellJSON struct {
	State            string `json:"state"`
	Reason           string `json:"reason,omitempty"`
	Cell             string `json:"cell,omitempty"`
	Namespace        string `json:"namespace,omitempty"`
	Cluster          string `json:"cluster,omitempty"`
	CellPool         string `json:"cellPool,omitempty"`
	Profile          string `json:"profile,omitempty"`
	JenkinsVersion   string `json:"jenkinsVersion,omitempty"`
	FreeSlots        *int32 `json:"freeSlots,omitempty"`
	QueueLength      *int64 `json:"queueLength,omitempty"`
	RunningNow       *int64 `json:"runningNow,omitempty"`
	RunsLast24h      *int64 `json:"runsLast24h,omitempty"`
	FailedLast24h    *int64 `json:"failedLast24h,omitempty"`
	Jobs             *int64 `json:"jobs,omitempty"`
	OtherTenantCount *int32 `json:"otherTenantCount,omitempty"`
	CanOpenCell      bool   `json:"canOpenCell"`
	// Members carries one entry for each member of the tenant, ordered
	// by role: active members by ascending slot first, then standbys by
	// cell name. It is absent for unplaced and notCellHosted. Every
	// top-level field above keeps describing slot 0.
	Members []teamCellMemberJSON `json:"members,omitempty"`
}

// teamCellMemberJSON is one member of the tenant on the team cell read.
// Each entry carries that member's own four load counts, narrowed by the
// caller's own grant exactly as the top-level counts are.
type teamCellMemberJSON struct {
	Cell          string `json:"cell"`
	Namespace     string `json:"namespace"`
	Cluster       string `json:"cluster"`
	Role          string `json:"role"`
	RunningNow    int64  `json:"runningNow"`
	RunsLast24h   int64  `json:"runsLast24h"`
	FailedLast24h int64  `json:"failedLast24h"`
	Jobs          int64  `json:"jobs"`
}

// placedConditionReason returns the Team's Placed condition reason, or
// the empty string when the Team carries no such condition.
func placedConditionReason(team *v1alpha1.Team) string {
	if team == nil {
		return ""
	}
	for _, c := range team.Status.Conditions {
		if c.Type == v1alpha1.TeamConditionPlaced {
			return c.Reason
		}
	}
	return ""
}

// handleGetTeamCell serves GET /pipelines/{team}/cell. It is the one
// pipelines path whose subject is the placement, not the jobs, so it
// resolves the Team object first and falls back to the tenant resolver.
func (s *Server) handleGetTeamCell(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	teamName := r.PathValue("team")

	team, err := crdstore.Get[v1alpha1.Team](ctx, s.deps.Store, teamName, "")
	if err != nil || team == nil || team.Spec.Pipelines == nil {
		refs, serr := s.resolveTeamTenants(ctx, claims, teamName)
		if serr != nil {
			s.writeServiceError(w, serr)
			return
		}
		// A Team that only a member cluster holds resolves to one placed
		// tenant of its own name. The member's API server holds the Team
		// status, so the body carries no Placed reason and one slot-0
		// member.
		if len(refs) == 1 && refs[0].Tenant == teamName && refs[0].Controller != "" {
			body, serr := s.buildTeamCell(ctx, claims, refs[0], "", nil)
			if serr != nil {
				s.writeServiceError(w, serr)
				return
			}
			s.writeJSON(w, http.StatusOK, body)
			return
		}
		// Step 2: every surviving candidate is a single-controller
		// tenant, so the answer is notCellHosted.
		s.writeJSON(w, http.StatusOK, teamCellJSON{State: teamCellNotCellHosted})
		return
	}

	// Step 1: a pipelines Team authorizes through the tenant read check
	// with an empty job path. A refusal answers 404, so the surface does
	// not confirm that the team exists.
	ref := TenantRef{Tenant: team.Name, Team: team.Name, Folder: team.Name}
	if p := team.Status.Placement; p != nil {
		ref.Cluster = p.Cluster
		ref.Namespace = p.CellNamespace
		ref.Controller = p.Cell
	}
	if !s.canReadPipeline(claims, ref, "") {
		s.writeJSONError(w, http.StatusNotFound, "team not found")
		return
	}

	reason := placedConditionReason(team)
	if ref.Controller == "" {
		s.writeJSON(w, http.StatusOK, teamCellJSON{State: teamCellUnplaced, Reason: reason})
		return
	}

	body, serr := s.buildTeamCell(ctx, claims, ref, reason, team)
	if serr != nil {
		s.writeServiceError(w, serr)
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

// buildTeamCell fills the placed, moving, and rehydrating body. Rule 4
// reads status.cell.appliedTenants. It reads no status.cell.rehydration:
// the cell-controllers capability keeps that key internal to the
// operator.
func (s *Server) buildTeamCell(ctx context.Context, claims *auth.Claims, ref TenantRef, reason string,
	team *v1alpha1.Team) (teamCellJSON, *ServiceError) {
	cr, err := s.cellController(ctx, ref.Cluster, ref.Namespace, ref.Controller)
	if err != nil {
		s.deps.Logger.Error("read team cell", "team", ref.Team, "error", err)
		return teamCellJSON{}, &ServiceError{Status: http.StatusInternalServerError, Message: "read team cell failed"}
	}

	jobRows, err := s.deps.RecordStore.CellTenantUsage(ctx, ref.Cluster, ref.Namespace, ref.Controller, ref.Tenant)
	if serr := s.readerServiceError(err, "read team cell usage"); serr != nil {
		return teamCellJSON{}, serr
	}
	counts := s.admittedJobCounts(claims, ref, jobRows)

	presentation := s.cellPresentationFor(cr, ref.Cluster)
	placed := cellPlacedTenants(cr)
	freeSlots := cellFreeSlots(s.resolveCellCap(ctx, ref.Cluster, cr), placed)
	others := placed - 1
	if others < 0 {
		others = 0
	}

	body := teamCellJSON{
		State:            s.teamCellState(cr, ref, reason),
		Reason:           reason,
		Cell:             ref.Controller,
		Namespace:        ref.Namespace,
		Cluster:          ref.Cluster,
		CellPool:         presentation.CellPool,
		Profile:          presentation.Profile,
		JenkinsVersion:   presentation.JenkinsVersion,
		FreeSlots:        &freeSlots,
		QueueLength:      presentation.QueueLength,
		RunningNow:       &counts.RunningNow,
		RunsLast24h:      &counts.RunsLast24h,
		FailedLast24h:    &counts.FailedLast24h,
		Jobs:             &counts.Jobs,
		OtherTenantCount: &others,
	}

	// The hint grants nothing. It tells the card whether to render a
	// link, and it decides no other field of this body.
	if s.deps.Authorizer != nil {
		body.CanOpenCell = s.deps.Authorizer.CanReadController(claims, ref.Namespace, ref.Controller)
	}
	body.Members = s.teamCellMembers(ctx, claims, ref, team)
	return body, nil
}

// teamCellMembers answers one entry for each member of the tenant,
// ordered by role: active members by ascending slot first, then standbys
// by cell name. Each entry carries that member's own four load counts,
// narrowed by the caller's own grant.
//
// The body names no other tenant. A tenant with no stored member list
// answers one entry for slot 0, so a single-member tenant reads one
// member and the top-level fields describe that same cell.
func (s *Server) teamCellMembers(ctx context.Context, claims *auth.Claims, ref TenantRef,
	team *v1alpha1.Team) []teamCellMemberJSON {
	members := orderedTeamMembers(team, ref)
	out := make([]teamCellMemberJSON, 0, len(members))
	for _, m := range members {
		entry := teamCellMemberJSON{
			Cell: m.Cell, Namespace: m.CellNamespace, Cluster: ref.Cluster,
			Role: renderTeamMemberRole(m),
		}
		memberRef := ref
		memberRef.Namespace = m.CellNamespace
		memberRef.Controller = m.Cell
		rows, err := s.deps.RecordStore.CellTenantUsage(ctx, ref.Cluster, m.CellNamespace, m.Cell, ref.Tenant)
		if err != nil {
			s.deps.Logger.Warn("read a team cell member's usage",
				"team", ref.Team, "cell", m.Cell, "error", err)
		} else {
			counts := s.admittedJobCounts(claims, memberRef, rows)
			entry.RunningNow = counts.RunningNow
			entry.RunsLast24h = counts.RunsLast24h
			entry.FailedLast24h = counts.FailedLast24h
			entry.Jobs = counts.Jobs
		}
		out = append(out, entry)
	}
	return out
}

// orderedTeamMembers answers the tenant's members in the response order:
// active members by ascending slot, then standbys by cell name. A tenant
// with no stored member list answers one synthetic slot-0 entry built
// from the scalar placement.
func orderedTeamMembers(team *v1alpha1.Team, ref TenantRef) []v1alpha1.TeamMemberStatus {
	var members []v1alpha1.TeamMemberStatus
	if team != nil && team.Status.Placement != nil {
		members = append(members, team.Status.Placement.Members...)
	}
	if len(members) == 0 {
		slot := int32(0)
		members = []v1alpha1.TeamMemberStatus{{
			Cell: ref.Controller, CellNamespace: ref.Namespace,
			Role: v1alpha1.TeamMemberRoleActive, Slot: &slot,
		}}
	}
	sort.SliceStable(members, func(i, j int) bool {
		si, sj := teamMemberSortKey(members[i]), teamMemberSortKey(members[j])
		if si != sj {
			return si < sj
		}
		return members[i].Cell < members[j].Cell
	})
	return members
}

// teamMemberSortKey answers an active member's slot index, and a value
// above every slot for a standby, so a standby sorts last.
func teamMemberSortKey(m v1alpha1.TeamMemberStatus) int32 {
	if m.Role == v1alpha1.TeamMemberRoleStandby || m.Slot == nil {
		return 1 << 20
	}
	return *m.Slot
}

// renderTeamMemberRole answers the rendered role: "slot <n>" for an
// active member and "standby" for a standby.
func renderTeamMemberRole(m v1alpha1.TeamMemberStatus) string {
	if m.Role == v1alpha1.TeamMemberRoleStandby || m.Slot == nil {
		if m.Role == v1alpha1.TeamMemberRoleStandby {
			return v1alpha1.TeamMemberRoleStandby
		}
		return "slot 0"
	}
	return fmt.Sprintf("slot %d", *m.Slot)
}

// admittedJobCounts sums the counts of the jobs this caller may read.
// The tenant read check admits the empty job path for a grant narrowed by
// jobs globs, so the gate alone cannot scope the numbers; each job path
// must meet the globs on its own, exactly as the team listing tests each
// row. An unnarrowed grant admits every row and reports the tenant's
// whole load.
func (s *Server) admittedJobCounts(claims *auth.Claims, ref TenantRef, rows []recordstore.CellJobUsage) recordstore.CellUsageCounts {
	var total recordstore.CellUsageCounts
	grants, granted := s.deps.Authorizer.tenantGrants(claims, ref.Tenant, ref.Folder)
	if !granted {
		return total
	}
	for _, row := range rows {
		if !grantsAllow(grants, ref.Folder, row.JobPath, "read") {
			continue
		}
		total.RunningNow += row.RunningNow
		total.RunsLast24h += row.RunsLast24h
		total.FailedLast24h += row.FailedLast24h
		total.Jobs += row.Jobs
	}
	return total
}

// teamCellState applies rules 3 to 5 of the state ladder.
func (s *Server) teamCellState(cr *v1alpha1.Controller, ref TenantRef, reason string) string {
	if reason == v1alpha1.TeamReasonTenantMoving {
		return teamCellMoving
	}
	if cr == nil || cr.Status.Cell == nil {
		return teamCellRehydrating
	}
	for _, applied := range cr.Status.Cell.AppliedTenants {
		if applied == ref.Tenant {
			return teamCellPlaced
		}
	}
	return teamCellRehydrating
}

// --- Fleet locks (components/schemas.yaml#FleetLock) ---

// fleetLockJSON is one row of either lock response. tenant and jobPath
// are always present, so a caller learns which tenant holds a lock even
// when it may not read that tenant's runs. runLink is present only when
// the caller's own grant reads the row's own run. waitingSince is
// present only on a waiting row: a held or a releasing row reports
// acquiredAt instead. No other field exists: a consumer that wants a
// lock's holder groups the rows of one name itself.
type fleetLockJSON struct {
	Name         string     `json:"name"`
	Cluster      string     `json:"cluster"`
	Tenant       string     `json:"tenant"`
	JobPath      string     `json:"jobPath"`
	State        string     `json:"state"`
	RunID        string     `json:"runId"`
	RunLink      *string    `json:"runLink,omitempty"`
	WaitingSince *time.Time `json:"waitingSince,omitempty"`
	AcquiredAt   *time.Time `json:"acquiredAt,omitempty"`
}

// fleetLockGrant is one tenant's own read decision. It is held for the
// length of one request, so a page of rows sharing few tenants resolves
// few grants. team is the resolved ref's own team, the first segment of
// the run link the grant allows.
type fleetLockGrant struct {
	folder  string
	team    string
	grants  []rbac.TenantGrant
	granted bool
}

// fleetLockMergeKey is one row's own place in a merged list: the
// ordering triple both reads share. The read of one tenant can answer a
// row twice for a team of two tenants, once by the row's owner tenant
// and once by the row's tenant column, so the merge de-duplicates by
// this triple.
type fleetLockMergeKey struct {
	name         string
	waitingSince time.Time
	runID        string
}

// handleListTeamFleetLocks serves GET /pipelines/{team}/locks. It
// answers what the list_fleet_locks MCP tool answers for the same team,
// because both call ListFleetLocks.
func (s *Server) handleListTeamFleetLocks(w http.ResponseWriter, r *http.Request) {
	claims := auth.ClaimsFromContext(r.Context())
	rows, serr := s.ListFleetLocks(r.Context(), claims, r.PathValue("team"))
	if serr != nil {
		s.writeJSONError(w, serr.Status, serr.Message)
		return
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(rows))
}

// handleListAllFleetLocks serves GET /locks, the fleet-wide read. The
// gate admits only a cluster-wide fleetlocks:read binding, and the
// answer carries every tenant's rows of every cluster.
func (s *Server) handleListAllFleetLocks(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecordStore == nil {
		s.pipelinesUnavailable(w)
		return
	}
	claims := auth.ClaimsFromContext(r.Context())
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanReadFleetLocks(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	locks, err := s.deps.RecordStore.ListAllFleetLocks(r.Context())
	if s.handleReaderError(w, err, "list fleet locks") {
		return
	}
	s.writeJSON(w, http.StatusOK, itemsEnvelope(s.fleetLockRows(claims, nil, locks)))
}

// listTeamFleetLocks reads every admitted tenant's locks and merges them
// into one list. The order is (name, waiting_since, run_id), the order
// one read answers with, so the merge keeps a holder ahead of its
// waiters.
func (s *Server) listTeamFleetLocks(ctx context.Context, claims *auth.Claims, refs []TenantRef) ([]fleetLockJSON, error) {
	seen := make(map[fleetLockMergeKey]bool)
	var merged []recordstore.FleetLock
	for _, ref := range refs {
		locks, err := s.deps.RecordStore.ListFleetLocks(ctx, ref.Cluster, ref.Tenant)
		if err != nil {
			return nil, err
		}
		for _, lock := range locks {
			key := fleetLockMergeKey{name: lock.Name, waitingSince: lock.WaitingSince, runID: lock.RunID}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, lock)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Name != merged[j].Name {
			return merged[i].Name < merged[j].Name
		}
		if !merged[i].WaitingSince.Equal(merged[j].WaitingSince) {
			return merged[i].WaitingSince.Before(merged[j].WaitingSince)
		}
		return merged[i].RunID < merged[j].RunID
	})
	return s.fleetLockRows(claims, refs, merged), nil
}

// fleetLockRows maps the record store's rows onto the response shape and
// decides each row's own run link. A row whose job path is empty carries
// no run link, because a run page path needs a job path.
func (s *Server) fleetLockRows(claims *auth.Claims, refs []TenantRef, locks []recordstore.FleetLock) []fleetLockJSON {
	cache := make(map[string]fleetLockGrant)
	rows := make([]fleetLockJSON, 0, len(locks))
	for _, lock := range locks {
		row := fleetLockJSON{
			Name:       lock.Name,
			Cluster:    lock.Cluster,
			Tenant:     lock.Tenant,
			JobPath:    lock.JobPath,
			State:      lock.State,
			RunID:      lock.RunID,
			AcquiredAt: lock.AcquiredAt,
		}
		if lock.State == recordstore.FleetLockStateWaiting {
			since := lock.WaitingSince
			row.WaitingSince = &since
		}
		grant := s.fleetLockGrantFor(claims, refs, cache, lock.Tenant)
		if lock.JobPath != "" && grant.granted && grantsAllow(grant.grants, grant.folder, lock.JobPath, "read") {
			link := searchRunLink(grant.team, lock.JobPath, lock.RunID)
			row.RunLink = &link
		}
		rows = append(rows, row)
	}
	return rows
}

// fleetLockGrantFor answers the read decision of one row's own tenant. A
// tenant one of refs already names reuses that ref's folder and grants,
// so the team route resolves no grant twice. Every other tenant resolves
// through TenantRefFromTenant alone: a pure parse, and no resolver round
// trip. The result is cached per tenant string, because a page holds
// many rows and few tenants. A tenant value that does not parse answers
// no grant, so its row carries tenant and jobPath, carries no run link,
// and raises no error.
func (s *Server) fleetLockGrantFor(claims *auth.Claims, refs []TenantRef, cache map[string]fleetLockGrant, tenant string) fleetLockGrant {
	if grant, ok := cache[tenant]; ok {
		return grant
	}
	var grant fleetLockGrant
	matched := false
	for _, ref := range refs {
		if ref.Tenant != tenant {
			continue
		}
		grant.folder = ref.Folder
		grant.team = ref.Team
		grant.grants, grant.granted = s.deps.Authorizer.tenantGrants(claims, ref.Tenant, ref.Folder)
		matched = true
		break
	}
	if !matched {
		if ref, err := TenantRefFromTenant(tenant); err == nil {
			grant.folder = ref.Folder
			grant.team = ref.Team
			grant.grants, grant.granted = s.deps.Authorizer.tenantGrants(claims, tenant, ref.Folder)
		}
	}
	cache[tenant] = grant
	return grant
}

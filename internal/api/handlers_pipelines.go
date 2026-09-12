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
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/auth"
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
	RunID       string     `json:"runId"`
	BuildNumber int64      `json:"buildNumber,omitempty"`
	JobPath     string     `json:"jobPath,omitempty"`
	Status      string     `json:"status"`
	Result      string     `json:"result,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	SCMRevision string     `json:"scmRevision,omitempty"`
	Actor       string     `json:"actor,omitempty"`
	Cause       string     `json:"cause,omitempty"`
	QueuedAt    *time.Time `json:"queuedAt,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	DurationMs  int64      `json:"durationMs,omitempty"`
}

type runningRunJSON struct {
	RunID             string     `json:"runId"`
	JobPath           string     `json:"jobPath"`
	BuildNumber       int64      `json:"buildNumber,omitempty"`
	Status            string     `json:"status"`
	CurrentStage      string     `json:"currentStage,omitempty"`
	Branch            string     `json:"branch,omitempty"`
	SCMRevision       string     `json:"scmRevision,omitempty"`
	ChangeSummary     string     `json:"changeSummary,omitempty"`
	QueuedAt          *time.Time `json:"queuedAt,omitempty"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	TypicalDurationMs int64      `json:"typicalDurationMs,omitempty"`
}

type pipelineSummaryJSON struct {
	JobPath         string          `json:"jobPath"`
	Health          string          `json:"health"`
	LastBuild       *runSummaryJSON `json:"lastBuild,omitempty"`
	DurationTrendMs []int64         `json:"durationTrendMs,omitempty"`
}

type pipelineListResponse struct {
	Items      []pipelineSummaryJSON `json:"items"`
	RunningNow []runningRunJSON      `json:"runningNow"`
	Truncated  bool                  `json:"truncated"`
}

type pipelineDefinitionJSON struct {
	JenkinsfileRepo   string `json:"jenkinsfileRepo,omitempty"`
	JenkinsfileCommit string `json:"jenkinsfileCommit,omitempty"`
	Profile           string `json:"profile,omitempty"`
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
	JobPath         string                    `json:"jobPath"`
	Health          string                    `json:"health"`
	TenantFolder    string                    `json:"tenantFolder"`
	Tenant          string                    `json:"tenant"`
	LastBuild       *runSummaryJSON           `json:"lastBuild,omitempty"`
	DurationTrendMs []int64                   `json:"durationTrendMs,omitempty"`
	Definition      *pipelineDefinitionJSON   `json:"definition,omitempty"`
	Triggers        *pipelineTriggersJSON     `json:"triggers,omitempty"`
	Parameters      []pipelineParameterJSON   `json:"parameters,omitempty"`
	Template        *pipelineTemplateInfoJSON `json:"template,omitempty"`
	Last30Days      *pipelineLast30DaysJSON   `json:"last30Days"`
	Dependencies    pipelineDependenciesJSON  `json:"dependencies"`
}

// pipelineDependencyEdgeJSON is one cross-tenant dependency edge (design
// D13): a producer tenant and job, the result threshold the edge fires
// on, and whether the producer's exports currently grant it. Granted
// false marks a declared edge whose grant is missing; it is never
// dropped from the list.
type pipelineDependencyEdgeJSON struct {
	Tenant  string `json:"tenant"`
	Job     string `json:"job"`
	Result  string `json:"result"`
	Granted bool   `json:"granted"`
}

// pipelineDependenciesJSON is the job detail's dependency edge object.
// Both arrays are empty, never omitted, for a job with no edge.
type pipelineDependenciesJSON struct {
	Upstream   []pipelineDependencyEdgeJSON `json:"upstream"`
	Downstream []pipelineDependencyEdgeJSON `json:"downstream"`
}

// buildPipelineDependencies converts the record store's dependency edges
// into the API shape.
func buildPipelineDependencies(deps recordstore.JobDependencies) pipelineDependenciesJSON {
	out := pipelineDependenciesJSON{
		Upstream:   make([]pipelineDependencyEdgeJSON, 0, len(deps.Upstream)),
		Downstream: make([]pipelineDependencyEdgeJSON, 0, len(deps.Downstream)),
	}
	for _, edge := range deps.Upstream {
		out.Upstream = append(out.Upstream, pipelineDependencyEdgeJSON{
			Tenant: edge.Tenant, Job: edge.Job, Result: edge.Result, Granted: edge.Granted,
		})
	}
	for _, edge := range deps.Downstream {
		out.Downstream = append(out.Downstream, pipelineDependencyEdgeJSON{
			Tenant: edge.Tenant, Job: edge.Job, Result: edge.Result, Granted: edge.Granted,
		})
	}
	return out
}

// pipelineLast30DaysJSON is design D8's "Last 30 days" card. PassRate,
// MedianDurationMs, and SlowestStage stay nil when the window holds no
// finished run or no finished stage. handleGetPipelineJob sets the
// whole field to nil, not this struct's zero value, when the
// record-store call itself fails.
type pipelineLast30DaysJSON struct {
	Builds           int64                     `json:"builds"`
	PassRate         *float64                  `json:"passRate,omitempty"`
	MedianDurationMs *int64                    `json:"medianDurationMs,omitempty"`
	SlowestStage     *pipelineSlowestStageJSON `json:"slowestStage,omitempty"`
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
	s.QueuedAt = run.QueuedAt
	s.StartedAt = run.StartedAt
	s.EndedAt = run.EndedAt
	if run.DurationMs != nil {
		s.DurationMs = *run.DurationMs
	}
	return s
}

// causeSummary reads runs.cause, the free-form Jenkins Cause object, for
// its shortDescription. Jenkins reports a build's causes as either one
// object or an array; an unrecognized shape yields the empty string.
func causeSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
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
// currentStage is left empty: a live stage read is one more call per
// running run, which the team home's first load does not need — the
// build page's own log-stream poll (design D9) is where stage state
// updates live.
func buildRunningRun(run recordstore.Run, trendMs []int64) runningRunJSON {
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
	out.ChangeSummary = causeSummary(run.Cause)
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
	var runningRuns []recordstore.Run
	truncated := false

	for _, ref := range refs {
		summaries, err := s.deps.RecordStore.ListJobSummaries(ctx, ref.Cluster, ref.Tenant)
		if s.handleReaderError(w, err, "list job summaries") {
			return
		}
		for _, js := range summaries {
			jobs[js.JobPath] = buildPipelineSummary(js)
			if len(js.DurationTrendMs) > 0 {
				trendByJob[js.JobPath] = js.DurationTrendMs
			}
		}
		for _, path := range s.catalogDeclaredJobPaths(ctx, ref) {
			if _, ok := jobs[path]; !ok {
				jobs[path] = pipelineSummaryJSON{JobPath: path, Health: "unknown"}
			}
		}

		runs, tenantTruncated, err := s.runningNowForTenant(ctx, ref)
		if s.handleReaderError(w, err, "list running runs") {
			return
		}
		runningRuns = append(runningRuns, runs...)
		truncated = truncated || tenantTruncated
	}

	items := make([]pipelineSummaryJSON, 0, len(jobs))
	for _, js := range jobs {
		items = append(items, js)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].JobPath < items[j].JobPath })

	runningOut := make([]runningRunJSON, 0, len(runningRuns))
	for _, run := range runningRuns {
		runningOut = append(runningOut, buildRunningRun(run, trendByJob[run.JobPath]))
	}
	sort.Slice(runningOut, func(i, j int) bool {
		if runningOut[i].JobPath != runningOut[j].JobPath {
			return runningOut[i].JobPath < runningOut[j].JobPath
		}
		return runningOut[i].RunID < runningOut[j].RunID
	})

	s.writeJSON(w, http.StatusOK, pipelineListResponse{Items: items, RunningNow: runningOut, Truncated: truncated})
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

	cr, item, err := s.findDeclaredItem(ctx, ref, jobPath)
	if err != nil {
		s.deps.Logger.Warn("pipeline job definition lookup failed", "jobPath", jobPath, "error", err)
	}
	if item != nil {
		detail.Definition = buildPipelineDefinition(item)
		if detail.Definition != nil {
			if cr != nil {
				detail.Definition.Profile = cr.Spec.Version
			}
			if summary.LastRun != nil && summary.LastRun.SCMRevision != nil {
				detail.Definition.JenkinsfileCommit = *summary.LastRun.SCMRevision
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
	detail.Dependencies = buildPipelineDependencies(deps)

	s.writeJSON(w, http.StatusOK, detail)
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
	out := &pipelineLast30DaysJSON{Builds: stats.Builds, PassRate: stats.PassRate, MedianDurationMs: stats.MedianDurationMs}
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

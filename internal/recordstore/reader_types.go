package recordstore

import (
	"encoding/json"
	"time"
)

// Run is one row of varroa.runs, in the shape a reader returns it.
type Run struct {
	RunID                   string
	CreatedAt               time.Time
	Cluster                 string
	Namespace               string
	Cell                    string
	Tenant                  string
	JobPath                 string
	BuildNumber             *int64
	Branch                  *string
	SCMRevision             *string
	Status                  string
	Result                  *string
	QueuedAt                *time.Time
	StartedAt               *time.Time
	EndedAt                 *time.Time
	LastHeartbeatAt         *time.Time
	InterruptedSupersededAt *time.Time
	DurationMs              *int64
	Actor                   *string
	Cause                   json.RawMessage
	Params                  json.RawMessage
	Changes                 json.RawMessage
	Source                  json.RawMessage
	Summary                 *string
	TestsTotal              *int32
	TestsFailed             *int32
	TestsSkipped            *int32
	Payload                 json.RawMessage
}

// Stage is one row of varroa.run_stages.
type Stage struct {
	RunID      string
	StageID    string
	CreatedAt  time.Time
	ParentID   *string
	Name       string
	NodeType   string
	Status     string
	StartedAt  *time.Time
	EndedAt    *time.Time
	DurationMs *int64
	Error      *string
	LogOffset  *int64
	LogLength  *int64
	Payload    json.RawMessage
}

// Artifact is one row of varroa.run_artifacts.
type Artifact struct {
	RunID       string
	Path        string
	CreatedAt   time.Time
	SizeBytes   int64
	Digest      *string
	ContentType *string
	BlobKey     string
}

// TestSuite is one row of varroa.run_tests: one suite's counts for one
// run.
type TestSuite struct {
	RunID      string
	Suite      string
	CreatedAt  time.Time
	Total      int32
	Failed     int32
	Skipped    int32
	DurationMs *int64
}

// FleetLock is one row of varroa.fleet_locks, in the shape both lock
// reads return it. It carries no queue position: a position is a count
// over other tenants' rows of one lock name, and neither read path
// computes one.
type FleetLock struct {
	Name    string `json:"name"`
	RunID   string `json:"runId"`
	Cluster string `json:"cluster"`
	Tenant  string `json:"tenant"`
	JobPath string `json:"jobPath"`
	State   string `json:"state"`
	// FenceToken stays zero on a waiting row, because the promotion
	// stamps it.
	FenceToken int64 `json:"fenceToken,omitempty"`
	// WaitingSince carries no omitempty. The first insert sets the
	// column on every row, and nothing clears it.
	WaitingSince time.Time  `json:"waitingSince"`
	AcquiredAt   *time.Time `json:"acquiredAt,omitempty"`
	ExpiresAt    time.Time  `json:"expiresAt"`
}

// JobSchedule is one row of varroa.job_schedules, in the shape a reader
// returns it: the job path, the trigger type, the trigger specification,
// the time zone, the disabled state, the exported occurrences in order,
// and the observation time. It describes the complete job inventory of
// a reporting cell, including a job that has never run and carries no
// trigger.
type JobSchedule struct {
	JobPath       string
	TriggerType   string
	Spec          string
	Timezone      string
	Disabled      bool
	NextFireTimes []time.Time
	ObservedAt    time.Time
	// Namespace and Cell name the reporting cell: the backstop cron
	// scheduler (design D11) fills a wake_cell/retry_run action's
	// namespace and controller fields from them.
	Namespace string
	Cell      string
	// PlacementGeneration is the row's own placement_generation. The
	// backstop scheduler skips a row below the tenant's stored
	// generation: that row came from a cell the tenant has left.
	PlacementGeneration int64
	// UpstreamTenant, UpstreamJob, and UpstreamResult carry a
	// varroa-upstream row's subscription, design.md D9. Every other
	// row carries the three fields empty.
	UpstreamTenant string
	UpstreamJob    string
	UpstreamResult string
}

// Intent is the subset of a varroa.run_intents row GetIntentByKey
// returns.
type Intent struct {
	IntentKey string
	RunID     string
	State     string
	CreatedAt time.Time
}

// jobSummaryTrendSize bounds how many of a job's most recent runs
// JobSummary.LastRun and DurationTrendMs draw from.
const jobSummaryTrendSize = 10

// jobHealthRecentResultsSize is how many of a job's most recent
// finished runs JobSummary.RecentResults draws from. Design D8 computes
// the pipelines table's health glyph from exactly this many runs.
const jobHealthRecentResultsSize = 5

// JobSummary is one job's run-history summary, derived only from record
// data: no job definition, no trigger state, and no job that has never
// run. The pipelines API joins the catalog for those. LastRun is the
// newest run regardless of status. DurationTrendMs holds the most
// recent runs' duration_ms, newest first, for at most
// jobSummaryTrendSize runs, skipping a run with no duration yet.
// RecentResults holds the job's most recent jobHealthRecentResultsSize
// finished runs' results, newest first; a run with no result yet
// (QUEUED or RUNNING) is not finished and never appears here.
// MedianQueueWaitMs is nil when the tenant's job has no run in the
// window with both queued_at and started_at set. ListJobSummaries
// alone sets it; GetJobSummary leaves it nil, because the job detail
// page reads its own queue wait from JobWindowStats.
type JobSummary struct {
	JobPath           string
	LastRun           *Run
	DurationTrendMs   []int64
	RecentResults     []string
	MedianQueueWaitMs *int64
}

// RunQuery is ListRuns' filter and paging input. Cluster and Tenant are
// required; every other field is optional. JobPath empty lists across
// every job in Tenant. Cause filters by run_intents.cause, the plain
// text classification a claim request carries, because runs.cause holds
// the free-form Jenkins Cause object instead.
type RunQuery struct {
	Cluster string
	Tenant  string
	JobPath string
	Branch  string
	Result  string
	Actor   string
	Cause   string
	Status  string
	Limit   int
	Cursor  string
}

// RunPage is one page of ListRuns. NextCursor is empty on the last page.
type RunPage struct {
	Runs       []Run
	NextCursor string
}

// CellUsageCounts holds the four load counts of one tenant on one cell,
// or the same four summed over the cell. RunningNow carries no time
// bound: a build can run longer than the window, and a long build must
// stay visible. RunsLast24h and FailedLast24h count inside the fixed
// 24-hour window. Jobs counts distinct job paths, never job_schedules
// rows. A run whose result is TRIGGER_FAILED counts in no value.
type CellUsageCounts struct {
	RunningNow    int64
	RunsLast24h   int64
	FailedLast24h int64
	Jobs          int64
}

// CellJobUsage is one job's share of a tenant's load on a cell. Jobs is
// one when job_schedules holds the path, and zero otherwise, so the sum
// of a row set reports the same four counts the tenant-wide aggregate
// reports for the same jobs. A caller that may read only part of a
// tenant's jobs therefore sums the rows it may read.
type CellJobUsage struct {
	JobPath string
	CellUsageCounts
}

// CellTenantUsage names one tenant on a cell and holds that tenant's
// four counts.
type CellTenantUsage struct {
	Tenant string
	// Role is that tenant's role on this cell: "slot <n>" for an active
	// member and "standby" for a standby. A standby runs no build, so
	// its running count is zero.
	Role string
	CellUsageCounts
}

// CellUsage is one cell's load, tenant by tenant. Tenants comes from
// varroa.tenant_placements, ordered by tenant name, so a response is
// stable. A tenant that left the cell does not appear. WindowStart is
// the start of the fixed 24-hour window the counts used.
type CellUsage struct {
	Cluster     string
	Namespace   string
	Cell        string
	WindowStart time.Time
	Tenants     []CellTenantUsage
	Totals      CellUsageCounts
}

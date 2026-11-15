package api

import "time"

// FleetRunTrigger is the trigger class of a fleet run. Kind is one of the
// nine trigger kinds. Label is the text the UI shows.
type FleetRunTrigger struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// FleetRunUpstream names the run that started a fleet run, when another run
// did. Job is the tenant-qualified job path of that run.
type FleetRunUpstream struct {
	Tenant      string `json:"tenant"`
	Job         string `json:"job"`
	BuildNumber int64  `json:"buildNumber,omitempty"`
	RunID       string `json:"runId,omitempty"`
}

// FleetRunRow is one row of the fleet run feed. It extends the run summary
// with the cluster, the rail name, and the trigger. CanWrite is
// true when the caller passes the tenant write check for the run's job.
type FleetRunRow struct {
	runSummaryJSON
	Cluster           string            `json:"cluster"`
	Tenant            string            `json:"tenant"` // rail and URL name
	TenantFolder      string            `json:"tenantFolder"`
	Trigger           FleetRunTrigger   `json:"trigger"`
	PendingInput      bool              `json:"pendingInput"`
	QueueWaitMs       *int64            `json:"queueWaitMs,omitempty"`
	InterruptedReason string            `json:"interruptedReason,omitempty"`
	Upstream          *FleetRunUpstream `json:"upstream,omitempty"`
	CanWrite          bool              `json:"canWrite"`
	CreatedAt         time.Time         `json:"createdAt"`
}

// FleetRunPage is one page of the fleet run feed. Items is never null.
// NextCursor is empty on the last page.
type FleetRunPage struct {
	Items      []FleetRunRow `json:"items"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// The nine trigger kinds of a fleet run.
const (
	triggerKindPullRequest = "pullRequest"
	triggerKindPush        = "push"
	triggerKindCron        = "cron"
	triggerKindManual      = "manual"
	triggerKindUpstream    = "upstream"
	triggerKindWebhook     = "webhook"
	triggerKindReplay      = "replay"
	triggerKindRestart     = "restart"
	triggerKindOther       = "other"
)

// RecentRunResult is one recent run of a rail tenant. Result is empty while
// the run has no result.
type RecentRunResult struct {
	RunID  string `json:"runId"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
}

// PipelinesOverviewTotals holds the counts of the overview window. PassRate
// is the share of counted finished runs that passed. It is nil when the
// window holds no counted run.
type PipelinesOverviewTotals struct {
	Runs24h           int64    `json:"runs24h"`
	RunsPrior24h      int64    `json:"runsPrior24h"`
	Passed            int64    `json:"passed"`
	Failed            int64    `json:"failed"`
	Interrupted       int64    `json:"interrupted"`
	Finished          int64    `json:"finished"`
	Running           int64    `json:"running"`
	Queued            int64    `json:"queued"`
	PassRate          *float64 `json:"passRate,omitempty"`
	MedianQueueWaitMs *int64   `json:"medianQueueWaitMs,omitempty"`
	MedianDurationMs  *int64   `json:"medianDurationMs,omitempty"`
	P90DurationMs     *int64   `json:"p90DurationMs,omitempty"`
}

// PipelinesOverviewAttention counts what needs a look now.
type PipelinesOverviewAttention struct {
	Failed24h    int64 `json:"failed24h"`
	WaitingInput int64 `json:"waitingInput"`
	AwaitingCell int64 `json:"awaitingCell"`
	LocksHeld    int64 `json:"locksHeld"`
}

// PipelinesOverviewTenant is one rail row. Placement is placed when any row
// of the name is placed, and awaitingCell or controllers otherwise.
type PipelinesOverviewTenant struct {
	Name         string            `json:"name"`
	Clusters     []string          `json:"clusters"`
	Placement    string            `json:"placement"`
	Runs24h      int64             `json:"runs24h"`
	Failed24h    int64             `json:"failed24h"`
	Running      int64             `json:"running"`
	WaitingInput int64             `json:"waitingInput"`
	LocksHeld    int64             `json:"locksHeld"`
	Recent       []RecentRunResult `json:"recent"`
}

// PipelinesOverview is the body of GET /pipelines/overview. Tenants is in
// attention order.
type PipelinesOverview struct {
	WindowStart       time.Time                  `json:"windowStart"`
	LocalCluster      string                     `json:"localCluster"`
	Clusters          int                        `json:"clusters"`
	CanReadFleetLocks bool                       `json:"canReadFleetLocks"`
	Totals            PipelinesOverviewTotals    `json:"totals"`
	Attention         PipelinesOverviewAttention `json:"attention"`
	Tenants           []PipelinesOverviewTenant  `json:"tenants"`
	// Hourly holds the 24 hourly buckets of the window, oldest first.
	Hourly []PipelinesOverviewHour `json:"hourly"`
	// WaitingInputs lists the runs that wait for an answer, oldest first.
	WaitingInputs     []PipelinesOverviewWaitingInput `json:"waitingInputs"`
	MoreWaitingInputs bool                            `json:"moreWaitingInputs"`
	// FailingJobs lists the jobs whose newest counted run failed, newest first.
	FailingJobs     []PipelinesOverviewFailingJob `json:"failingJobs"`
	MoreFailingJobs bool                          `json:"moreFailingJobs"`
	// Running lists up to three runs in progress that wait for no answer.
	Running []PipelinesOverviewRunningRun `json:"running"`
	// NextScheduled is the earliest future cron run in scope, when one exists.
	NextScheduled *PipelinesOverviewScheduledRun `json:"nextScheduled,omitempty"`
}

// PipelinesOverviewHour is one hourly bucket of the overview window. Start is
// the bucket start. Runs and Failed count the runs created in the hour.
type PipelinesOverviewHour struct {
	Start  time.Time `json:"start"`
	Runs   int64     `json:"runs"`
	Failed int64     `json:"failed"`
}

// PipelinesOverviewInput is the first pending input of a run that waits for
// an answer. ParameterCount counts its parameters.
type PipelinesOverviewInput struct {
	ID             string `json:"id"`
	Message        string `json:"message,omitempty"`
	OK             string `json:"ok,omitempty"`
	Submitter      string `json:"submitter,omitempty"`
	ParameterCount int    `json:"parameterCount"`
}

// PipelinesOverviewWaitingInput is one run that waits for an answer. Input is
// its first pending input, and InputCount counts all of them.
type PipelinesOverviewWaitingInput struct {
	Run        FleetRunRow            `json:"run"`
	Input      PipelinesOverviewInput `json:"input"`
	InputCount int                    `json:"inputCount"`
}

// PipelinesOverviewPassedRun is the last passed run of a failing job.
type PipelinesOverviewPassedRun struct {
	RunID       string     `json:"runId"`
	BuildNumber int64      `json:"buildNumber,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
}

// PipelinesOverviewFailingJob is one job whose newest counted run failed.
// Stage and StageError name the failing stage. TestsFailed counts the failed
// tests of the run. LastPassed is the last passed run, and FailuresSincePass
// counts the failed runs after it.
type PipelinesOverviewFailingJob struct {
	Run               FleetRunRow                 `json:"run"`
	Stage             string                      `json:"stage,omitempty"`
	StageError        string                      `json:"stageError,omitempty"`
	TestsFailed       int32                       `json:"testsFailed,omitempty"`
	LastPassed        *PipelinesOverviewPassedRun `json:"lastPassed,omitempty"`
	FailuresSincePass int64                       `json:"failuresSincePass"`
}

// PipelinesOverviewRunningRun is one run in progress. Stage names its top-level
// stage in progress, when one is known.
type PipelinesOverviewRunningRun struct {
	Run   FleetRunRow `json:"run"`
	Stage string      `json:"stage,omitempty"`
}

// PipelinesOverviewScheduledRun is the next cron run in scope. Tenant is the
// rail name, and At is the scheduled time.
type PipelinesOverviewScheduledRun struct {
	Tenant       string    `json:"tenant"`
	TenantFolder string    `json:"tenantFolder"`
	Cluster      string    `json:"cluster"`
	JobPath      string    `json:"jobPath"`
	At           time.Time `json:"at"`
}

// FleetRunGroup holds the finished runs of one job in the day window, except
// the held run. Newest is the newest of them.
type FleetRunGroup struct {
	Tenant       string      `json:"tenant"`
	TenantFolder string      `json:"tenantFolder"`
	Cluster      string      `json:"cluster"`
	JobPath      string      `json:"jobPath"`
	Runs         int64       `json:"runs"`
	Passed       int64       `json:"passed"`
	Failed       int64       `json:"failed"`
	FirstAt      time.Time   `json:"firstAt"`
	Newest       FleetRunRow `json:"newest"`
}

// FleetRunGroupPage is the body of GET /pipelines/runs/groups. Held counts the
// held runs that the groups leave out. More is true when the page is cut.
type FleetRunGroupPage struct {
	Since  time.Time       `json:"since"`
	Groups []FleetRunGroup `json:"groups"`
	Held   int64           `json:"held"`
	More   bool            `json:"more"`
}

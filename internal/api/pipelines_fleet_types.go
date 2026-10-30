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
// with the cluster, the rail name, the title, and the trigger. CanWrite is
// true when the caller passes the tenant write check for the run's job.
type FleetRunRow struct {
	runSummaryJSON
	Cluster           string            `json:"cluster"`
	Tenant            string            `json:"tenant"` // rail and URL name
	Title             string            `json:"title"`
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
// is nil when no run of the window has finished.
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
}

package recordstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/internal/pgdial"
)

// DefaultReadTimeout bounds each Reader call the BFF makes. A read that
// cannot reach the record store answers ErrUnavailable within this time.
const DefaultReadTimeout = 15 * time.Second

// WithTimeout wraps r so that every method runs under a deadline of d. A
// deadline or an unreachable database answers ErrUnavailable. Every other
// error passes through unchanged, and so does a cancel by the caller.
func WithTimeout(r Reader, d time.Duration) Reader {
	return &timeoutReader{r: r, d: d}
}

// timeoutReader implements Reader with a per-call deadline.
type timeoutReader struct {
	r Reader
	d time.Duration
}

var _ Reader = (*timeoutReader)(nil)

// errReadDeadline is the cause of the per-call deadline. A deadline that a
// caller set earlier has another cause, so it does not read as unreachable.
var errReadDeadline = errors.New("record store read deadline")

// classify maps an inner error to the Reader contract. It keeps nil and
// ErrUnavailable as they are. The per-call deadline, or an unreachable
// database, becomes ErrUnavailable. Any other error passes through.
func (t *timeoutReader) classify(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, ErrUnavailable) {
		return err
	}
	if errors.Is(context.Cause(ctx), errReadDeadline) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	// A caller's own deadline or cancel ends the call. It is not a store
	// that did not answer, so the error passes through.
	if ctx.Err() != nil {
		return err
	}
	if pgdial.IsUnreachable(err) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

// ChangeRequestJobs bounds the inner call with the per-read deadline.
func (t *timeoutReader) ChangeRequestJobs(ctx context.Context, cluster, tenant, changeURL string) ([]string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ChangeRequestJobs(ctx, cluster, tenant, changeURL)
	return v0, t.classify(ctx, err)
}

// RevisionJobs bounds the inner call with the per-read deadline.
func (t *timeoutReader) RevisionJobs(ctx context.Context, cluster, tenant, revision string) ([]string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.RevisionJobs(ctx, cluster, tenant, revision)
	return v0, t.classify(ctx, err)
}

// ListJobSummaries bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListJobSummaries(ctx context.Context, cluster, tenant string, since time.Time) ([]JobSummary, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListJobSummaries(ctx, cluster, tenant, since)
	return v0, t.classify(ctx, err)
}

// GetJobSummary bounds the inner call with the per-read deadline.
func (t *timeoutReader) GetJobSummary(ctx context.Context, cluster, tenant, jobPath string) (JobSummary, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.GetJobSummary(ctx, cluster, tenant, jobPath)
	return v0, t.classify(ctx, err)
}

// ListRuns bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListRuns(ctx context.Context, q RunQuery) (RunPage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListRuns(ctx, q)
	return v0, t.classify(ctx, err)
}

// ListRunsAcross bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListRunsAcross(ctx context.Context, q RunsAcrossQuery) (RunPage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListRunsAcross(ctx, q)
	return v0, t.classify(ctx, err)
}

// RunsOverview bounds the inner call with the per-read deadline.
func (t *timeoutReader) RunsOverview(ctx context.Context, q RunsOverviewQuery) (RunsOverview, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.RunsOverview(ctx, q)
	return v0, t.classify(ctx, err)
}

// ListDownstreamRuns bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListDownstreamRuns(ctx context.Context, q DownstreamRunQuery) (RunPage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListDownstreamRuns(ctx, q)
	return v0, t.classify(ctx, err)
}

// GetRun bounds the inner call with the per-read deadline.
func (t *timeoutReader) GetRun(ctx context.Context, runID string) (Run, []Stage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, v1, err := t.r.GetRun(ctx, runID)
	return v0, v1, t.classify(ctx, err)
}

// GetRunWait bounds the inner call with the per-read deadline.
func (t *timeoutReader) GetRunWait(ctx context.Context, runID string) (RunWait, bool, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, v1, err := t.r.GetRunWait(ctx, runID)
	return v0, v1, t.classify(ctx, err)
}

// ListJobRunWaits bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListJobRunWaits(ctx context.Context, cluster, tenant, jobPath string) ([]RunWait, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListJobRunWaits(ctx, cluster, tenant, jobPath)
	return v0, t.classify(ctx, err)
}

// TenantAgentSpend bounds the inner call with the per-read deadline.
func (t *timeoutReader) TenantAgentSpend(ctx context.Context, tenant string, day time.Time) (TenantAgentSpendRow, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.TenantAgentSpend(ctx, tenant, day)
	return v0, t.classify(ctx, err)
}

// TenantRetention bounds the inner call with the per-read deadline.
func (t *timeoutReader) TenantRetention(ctx context.Context, cluster, tenant string) (TenantRetentionPolicy, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.TenantRetention(ctx, cluster, tenant)
	return v0, t.classify(ctx, err)
}

// CurrentStages bounds the inner call with the per-read deadline.
func (t *timeoutReader) CurrentStages(ctx context.Context, runIDs []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.CurrentStages(ctx, runIDs)
	return v0, t.classify(ctx, err)
}

// ListArtifacts bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListArtifacts(ctx context.Context, runID string) ([]Artifact, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListArtifacts(ctx, runID)
	return v0, t.classify(ctx, err)
}

// GetArtifact bounds the inner call with the per-read deadline.
func (t *timeoutReader) GetArtifact(ctx context.Context, runID, path string) (artifact Artifact, found bool, err error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, v1, err := t.r.GetArtifact(ctx, runID, path)
	return v0, v1, t.classify(ctx, err)
}

// ListTests bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListTests(ctx context.Context, runID string) ([]TestSuite, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListTests(ctx, runID)
	return v0, t.classify(ctx, err)
}

// ListTestCases bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListTestCases(ctx context.Context, runID string) ([]TestCase, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListTestCases(ctx, runID)
	return v0, t.classify(ctx, err)
}

// ListTestCaseHistory bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListTestCaseHistory(ctx context.Context, q TestCaseHistoryQuery) ([]TestCaseOutcome, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListTestCaseHistory(ctx, q)
	return v0, t.classify(ctx, err)
}

// RunQuality bounds the inner call with the per-read deadline.
func (t *timeoutReader) RunQuality(ctx context.Context, runID string) (RunQuality, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.RunQuality(ctx, runID)
	return v0, t.classify(ctx, err)
}

// JobQualityTrend bounds the inner call with the per-read deadline.
func (t *timeoutReader) JobQualityTrend(ctx context.Context, cluster, tenant, jobPath string, runIDs []string) (RunQuality, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.JobQualityTrend(ctx, cluster, tenant, jobPath, runIDs)
	return v0, t.classify(ctx, err)
}

// GetIntentByKey bounds the inner call with the per-read deadline.
func (t *timeoutReader) GetIntentByKey(ctx context.Context, intentKey string) (Intent, bool, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, v1, err := t.r.GetIntentByKey(ctx, intentKey)
	return v0, v1, t.classify(ctx, err)
}

// NextBuildNumber bounds the inner call with the per-read deadline.
func (t *timeoutReader) NextBuildNumber(ctx context.Context, cluster, tenant, jobPath string) (int64, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.NextBuildNumber(ctx, cluster, tenant, jobPath)
	return v0, t.classify(ctx, err)
}

// SchemaVersion bounds the inner call with the per-read deadline.
func (t *timeoutReader) SchemaVersion(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.SchemaVersion(ctx)
	return v0, t.classify(ctx, err)
}

// ListActivity bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListActivity(ctx context.Context, q ActivityQuery) (ActivityPage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListActivity(ctx, q)
	return v0, t.classify(ctx, err)
}

// ActivityRetentionDays bounds the inner call with the per-read deadline.
func (t *timeoutReader) ActivityRetentionDays(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ActivityRetentionDays(ctx)
	return v0, t.classify(ctx, err)
}

// ListJobSchedules bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListJobSchedules(ctx context.Context, cluster, tenant string) ([]JobSchedule, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListJobSchedules(ctx, cluster, tenant)
	return v0, t.classify(ctx, err)
}

// JobInventory bounds the inner call with the per-read deadline.
func (t *timeoutReader) JobInventory(ctx context.Context, cluster, tenant string) (JobInventory, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.JobInventory(ctx, cluster, tenant)
	return v0, t.classify(ctx, err)
}

// GetJobBranches bounds the inner call with the per-read deadline.
func (t *timeoutReader) GetJobBranches(ctx context.Context, cluster, tenant, jobPath string) (JobBranchProject, bool, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, v1, err := t.r.GetJobBranches(ctx, cluster, tenant, jobPath)
	return v0, v1, t.classify(ctx, err)
}

// LatestRunsForJobs bounds the inner call with the per-read deadline.
func (t *timeoutReader) LatestRunsForJobs(ctx context.Context, cluster, tenant string, jobPaths []string) (map[string]Run, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.LatestRunsForJobs(ctx, cluster, tenant, jobPaths)
	return v0, t.classify(ctx, err)
}

// SearchRecords bounds the inner call with the per-read deadline.
func (t *timeoutReader) SearchRecords(ctx context.Context, q SearchQuery) (SearchResults, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.SearchRecords(ctx, q)
	return v0, t.classify(ctx, err)
}

// JobWindowStats bounds the inner call with the per-read deadline.
func (t *timeoutReader) JobWindowStats(ctx context.Context, cluster, tenant, jobPath string, since time.Time) (JobWindowStats, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.JobWindowStats(ctx, cluster, tenant, jobPath, since)
	return v0, t.classify(ctx, err)
}

// JobDependencies bounds the inner call with the per-read deadline.
func (t *timeoutReader) JobDependencies(ctx context.Context, cluster, tenant, jobPath string) (JobDependencies, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.JobDependencies(ctx, cluster, tenant, jobPath)
	return v0, t.classify(ctx, err)
}

// CellUsage bounds the inner call with the per-read deadline.
func (t *timeoutReader) CellUsage(ctx context.Context, cluster, namespace, cell string) (CellUsage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.CellUsage(ctx, cluster, namespace, cell)
	return v0, t.classify(ctx, err)
}

// CellUsageMany bounds the inner call with the per-read deadline.
func (t *timeoutReader) CellUsageMany(ctx context.Context, cluster, namespace string, cells []string) (map[string]CellUsage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.CellUsageMany(ctx, cluster, namespace, cells)
	return v0, t.classify(ctx, err)
}

// CellTenantUsage bounds the inner call with the per-read deadline.
func (t *timeoutReader) CellTenantUsage(ctx context.Context, cluster, namespace, cell, tenant string) ([]CellJobUsage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.CellTenantUsage(ctx, cluster, namespace, cell, tenant)
	return v0, t.classify(ctx, err)
}

// JobHome bounds the inner call with the per-read deadline.
func (t *timeoutReader) JobHome(ctx context.Context, cluster, tenant, jobPath string) (JobHome, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.JobHome(ctx, cluster, tenant, jobPath)
	return v0, t.classify(ctx, err)
}

// ListFleetLocks bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListFleetLocks(ctx context.Context, cluster, tenant string) ([]FleetLock, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListFleetLocks(ctx, cluster, tenant)
	return v0, t.classify(ctx, err)
}

// ListRunNotes bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListRunNotes(ctx context.Context, runID uuid.UUID, runCreatedAt time.Time) ([]RunNote, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListRunNotes(ctx, runID, runCreatedAt)
	return v0, t.classify(ctx, err)
}

// ListJobNotes bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListJobNotes(ctx context.Context, cluster, tenant, jobPath string, limit int) ([]RunNote, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListJobNotes(ctx, cluster, tenant, jobPath, limit)
	return v0, t.classify(ctx, err)
}

// ListAllFleetLocks bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListAllFleetLocks(ctx context.Context) ([]FleetLock, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListAllFleetLocks(ctx)
	return v0, t.classify(ctx, err)
}

// ListTenantPlacements bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListTenantPlacements(ctx context.Context, tenant string) ([]TenantPlacement, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListTenantPlacements(ctx, tenant)
	return v0, t.classify(ctx, err)
}

// TenantBadgeToken bounds the inner call with the per-read deadline.
func (t *timeoutReader) TenantBadgeToken(ctx context.Context, cluster, tenant string) (string, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.TenantBadgeToken(ctx, cluster, tenant)
	return v0, t.classify(ctx, err)
}

// ListAllTenantPlacements bounds the inner call with the per-read deadline.
func (t *timeoutReader) ListAllTenantPlacements(ctx context.Context) ([]TenantPlacement, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, t.d, errReadDeadline)
	defer cancel()
	v0, err := t.r.ListAllTenantPlacements(ctx)
	return v0, t.classify(ctx, err)
}

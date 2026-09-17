package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

const (
	// jobSchedulesPath is the plugin route the mite polls for the live
	// job-schedule inventory.
	jobSchedulesPath = "/varroa-activity/schedules"

	// jobSchedulePollInterval is the default poll cadence. The mite also
	// polls once immediately on every stream (re)connect.
	jobSchedulePollInterval = 10 * time.Minute

	// jobScheduleResendInterval bounds how stale a sent set becomes: the
	// mite resends the set at least this often even when its hash is
	// unchanged, so a stored occurrence stays no older than one poll
	// interval.
	jobScheduleResendInterval = 30 * time.Minute
)

// jobScheduleRow is one row of the plugin's GET /varroa-activity/schedules
// response.
type jobScheduleRow struct {
	JobPath       string   `json:"jobPath"`
	TriggerType   string   `json:"triggerType"`
	Spec          string   `json:"spec"`
	Timezone      string   `json:"timezone"`
	Disabled      bool     `json:"disabled"`
	NextFireTimes []string `json:"nextFireTimes"`
	// A varroa-upstream row sets all three fields. A native upstream row
	// sets the result alone, and carries its job list in Spec.
	UpstreamTenant string `json:"upstreamTenant,omitempty"`
	UpstreamJob    string `json:"upstreamJob,omitempty"`
	UpstreamResult string `json:"upstreamResult,omitempty"`
}

// jobScheduleResponse is the JSON envelope the schedules endpoint returns.
type jobScheduleResponse struct {
	Schedules []jobScheduleRow `json:"schedules"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
}

// startJobSchedulePoller polls the Jenkins plugin's job-schedule endpoint
// on a ticker and once immediately, so a fresh stream connect always
// starts with a current poll. Each successful poll updates the cached set
// that maybeAttachJobSchedules reads. A failed poll keeps the last known
// set and hash in place, logs the failure once, and the poller retries on
// the next tick.
func (a *Agent) startJobSchedulePoller(ctx context.Context) {
	logger := a.Logger.With("component", "job-schedule-poller")

	interval := a.jobScheduleInterval
	if interval <= 0 {
		interval = jobSchedulePollInterval
	}

	a.pollJobSchedulesOnce(ctx, logger)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.pollJobSchedulesOnce(ctx, logger)
		}
	}
}

// pollJobSchedulesOnce performs one poll and, on success, replaces the
// cached job-schedule set. On failure it changes nothing and logs once —
// the caller retries on the next tick.
func (a *Agent) pollJobSchedulesOnce(ctx context.Context, logger *slog.Logger) {
	set, err := a.pollJobSchedules(ctx)
	if err != nil {
		logger.Warn("job-schedule poll failed, keeping last known set", "error", err)
		return
	}

	a.jobScheduleMu.Lock()
	a.jobScheduleSet = set
	a.jobScheduleMu.Unlock()
}

// pollJobSchedules performs a single GET /varroa-activity/schedules
// request against the Jenkins plugin and converts the response into a
// JobScheduleSet.
func (a *Agent) pollJobSchedules(ctx context.Context) (*mitev1.JobScheduleSet, error) {
	client := a.getJenkinsClient()

	httpResp, err := client.DoGet(ctx, jobSchedulesPath)
	if err != nil {
		return nil, fmt.Errorf("do get: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status %d", httpResp.StatusCode)
	}

	var resp jobScheduleResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	items := make([]*mitev1.JobSchedule, 0, len(resp.Schedules))
	for _, row := range resp.Schedules {
		items = append(items, &mitev1.JobSchedule{
			JobPath:        row.JobPath,
			TriggerType:    row.TriggerType,
			Spec:           row.Spec,
			Timezone:       row.Timezone,
			Disabled:       row.Disabled,
			NextFireTimes:  row.NextFireTimes,
			UpstreamTenant: row.UpstreamTenant,
			UpstreamJob:    row.UpstreamJob,
			UpstreamResult: row.UpstreamResult,
		})
	}

	return &mitev1.JobScheduleSet{
		Hash:      computeJobScheduleHash(resp.Schedules),
		Items:     items,
		Truncated: resp.Truncated,
		Total:     resp.Total,
	}, nil
}

// computeJobScheduleHash hashes the schedule rows sorted by job path then
// trigger type, covering the job path, the trigger type, the
// specification, the time zone, the disabled state, and the upstream
// tenant, job, and result. The occurrences never enter the hash: an
// occurrence moves as time passes, and a hash over it would keep the
// send gate open on every poll.
func computeJobScheduleHash(rows []jobScheduleRow) string {
	sorted := make([]jobScheduleRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].JobPath != sorted[j].JobPath {
			return sorted[i].JobPath < sorted[j].JobPath
		}
		return sorted[i].TriggerType < sorted[j].TriggerType
	})

	h := sha256.New()
	for _, r := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%t\x00%s\x00%s\x00%s\n",
			r.JobPath, r.TriggerType, r.Spec, r.Timezone, r.Disabled,
			r.UpstreamTenant, r.UpstreamJob, r.UpstreamResult)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// maybeAttachJobSchedules fills snap.JobSchedules when one of three
// conditions holds: the cached set's hash differs from the hash of the
// last set the mite sent, the stream reconnected since the last send, or
// the resend interval has passed since the last send. Every other call
// leaves the field unset, so the field is absent from most snapshots. The
// set never contributes to any state hash — the caller computes those
// before this call runs.
func (a *Agent) maybeAttachJobSchedules(snap *mitev1.StateSnapshot) {
	a.jobScheduleMu.Lock()
	defer a.jobScheduleMu.Unlock()

	set := a.jobScheduleSet
	if set == nil {
		return
	}

	resendAfter := a.jobScheduleResendAfter
	if resendAfter <= 0 {
		resendAfter = jobScheduleResendInterval
	}

	changed := set.Hash != a.jobScheduleLastSentHash
	stale := time.Since(a.jobScheduleLastSendTime) >= resendAfter
	if !changed && !a.jobScheduleForceSend && !stale {
		return
	}

	snap.JobSchedules = set
	a.jobScheduleLastSentHash = set.Hash
	a.jobScheduleLastSendTime = time.Now()
	a.jobScheduleForceSend = false
}

// markStreamReconnected opens the job-schedule send gate so the next
// snapshot carries the last known set unconditionally, even when its hash
// is unchanged. A gateway restart or a mite reconnect drops the
// CommandStream, and the gateway/recorder need a fresh copy on the new
// stream.
func (a *Agent) markStreamReconnected() {
	a.jobScheduleMu.Lock()
	a.jobScheduleForceSend = true
	a.jobScheduleMu.Unlock()
}

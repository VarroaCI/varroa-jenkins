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

	// jobSchedulePollInterval is the longest wait between two polls. An
	// earlier cron occurrence brings the next poll forward.
	jobSchedulePollInterval = 10 * time.Minute

	// jobScheduleMinPollInterval is the shortest wait between two
	// successful polls. It bounds the send rate for a dense cron.
	jobScheduleMinPollInterval = time.Minute

	// jobScheduleOccurrenceSlack is the wait after an occurrence before
	// the poll that replaces it. Jenkins then exports the next occurrence.
	jobScheduleOccurrenceSlack = 5 * time.Second

	// jobScheduleRetryBackoff is the first wait after a failed poll. The
	// wait doubles up to jobScheduleMinPollInterval.
	jobScheduleRetryBackoff = 5 * time.Second

	// jobScheduleResendInterval bounds how stale a sent set becomes: the
	// mite resends the set at least this often even when its hash is
	// unchanged.
	jobScheduleResendInterval = 30 * time.Minute

	// jobScheduleTriggerCron is the trigger type whose occurrences drive
	// the poll horizon. The recorder's backstop reads only this type.
	jobScheduleTriggerCron = "cron"
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

// startJobSchedulePoller polls the Jenkins plugin's job-schedule
// endpoint until ctx ends. It does not poll while the export gate is
// closed: an empty JENKINS_HOME answers an empty or partial set, and the
// recorder then deletes every row of the cell. openScheduleExport wakes
// the poller at once. A failed poll retries with a doubling backoff. A
// successful poll schedules the next poll by nextJobSchedulePollDelay.
func (a *Agent) startJobSchedulePoller(ctx context.Context) {
	logger := a.Logger.With("component", "job-schedule-poller")

	interval := a.jobScheduleInterval
	if interval <= 0 {
		interval = jobSchedulePollInterval
	}
	minInterval := a.jobScheduleMinInterval
	if minInterval <= 0 {
		minInterval = jobScheduleMinPollInterval
	}
	retryBase := a.jobScheduleRetryBase
	if retryBase <= 0 {
		retryBase = jobScheduleRetryBackoff
	}

	retry := retryBase
	for {
		delay := interval
		if a.scheduleExportOpen() {
			// This poll serves a pending wake too.
			select {
			case <-a.jobSchedulePollNow:
			default:
			}
			if earliest, ok := a.pollJobSchedulesOnce(ctx, logger); ok {
				retry = retryBase
				delay = nextJobSchedulePollDelay(earliest, time.Now(), interval, minInterval)
				if a.branchScanFast(time.Now()) {
					delay = minInterval
				}
			} else {
				delay = min(retry, interval)
				retry = min(2*retry, minInterval)
			}
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.jobSchedulePollNow:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// nextJobSchedulePollDelay answers the wait before the next poll: the
// interval, or the time to earliest plus a short slack when that is
// sooner. The answer is never below minInterval, so a dense cron sends
// at most one set each minInterval. A zero earliest answers the interval.
func nextJobSchedulePollDelay(earliest, now time.Time, interval, minInterval time.Duration) time.Duration {
	if earliest.IsZero() {
		return interval
	}
	d := earliest.Add(jobScheduleOccurrenceSlack).Sub(now)
	if d >= interval {
		return interval
	}
	return max(d, minInterval)
}

// earliestCronOccurrence answers the earliest occurrence after now of an
// enabled cron row, or the zero time when no row has one. A value that
// does not parse as RFC 3339 is ignored.
func earliestCronOccurrence(rows []jobScheduleRow, now time.Time) time.Time {
	var earliest time.Time
	for _, r := range rows {
		if r.TriggerType != jobScheduleTriggerCron || r.Disabled {
			continue
		}
		for _, v := range r.NextFireTimes {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil || !t.After(now) {
				continue
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
	}
	return earliest
}

// scheduleExportOpen reports whether this JENKINS_HOME holds an applied
// desired state. A convergence marker on disk opens the gate through
// openScheduleExport, so the wake is never lost when a heartbeat sees
// the marker before the apply result opens the gate.
func (a *Agent) scheduleExportOpen() bool {
	if a.jobScheduleExportOpen.Load() {
		return true
	}
	if !a.hasLastAppliedHash() {
		return false
	}
	a.openScheduleExport()
	return true
}

// openScheduleExport opens the export gate and wakes the poller, so the
// first set after an apply describes the applied jobs. It wakes the
// poller only when the gate was closed: a wake on every apply would
// bypass the minimum poll interval. An items apply that writes the
// section also wakes the poller (handleDesiredState), so a new job reaches
// the inventory without waiting for the interval.
func (a *Agent) openScheduleExport() {
	if !a.jobScheduleExportOpen.CompareAndSwap(false, true) {
		return
	}
	select {
	case a.jobSchedulePollNow <- struct{}{}:
	default:
	}
}

// pollJobSchedulesOnce performs one poll and, on success, replaces the
// cached job-schedule set and answers its earliest cron occurrence. On
// failure it changes nothing, logs once, and answers false.
func (a *Agent) pollJobSchedulesOnce(ctx context.Context, logger *slog.Logger) (time.Time, bool) {
	// The branch inventory has its own endpoint. Polling it first keeps a
	// failed schedule read from freezing the branch export.
	a.pollJobBranchesOnce(ctx, logger)

	now := time.Now()
	set, rows, err := a.pollJobSchedules(ctx)
	if err != nil {
		logger.Warn("job-schedule poll failed, keeping last known set", "error", err)
		return time.Time{}, false
	}

	earliest := earliestCronOccurrence(rows, now)
	a.storeJobScheduleSet(set, earliest)
	return earliest, true
}

// storeJobScheduleSet replaces the cached set and counts the poll.
func (a *Agent) storeJobScheduleSet(set *mitev1.JobScheduleSet, earliest time.Time) {
	a.jobScheduleMu.Lock()
	a.jobScheduleSet = set
	a.jobScheduleSetEarliest = earliest
	a.jobScheduleSetSeq++
	a.jobScheduleMu.Unlock()
}

// pollJobSchedules performs a single GET /varroa-activity/schedules
// request against the Jenkins plugin and converts the response into a
// JobScheduleSet. It also answers the decoded rows.
func (a *Agent) pollJobSchedules(ctx context.Context) (*mitev1.JobScheduleSet, []jobScheduleRow, error) {
	client := a.getJenkinsClient()

	// The time is read before the request. A run created before it names a
	// job that the plugin walk saw, so the time is a safe lower bound.
	takenAt := time.Now()
	httpResp, err := client.DoGet(ctx, jobSchedulesPath)
	if err != nil {
		return nil, nil, fmt.Errorf("do get: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("unexpected status %d", httpResp.StatusCode)
	}

	var resp jobScheduleResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, nil, fmt.Errorf("decode response: %w", err)
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
		Hash:          computeJobScheduleHash(resp.Schedules),
		Items:         items,
		Truncated:     resp.Truncated,
		Total:         resp.Total,
		TakenAtMillis: takenAt.UnixMilli(),
	}, resp.Schedules, nil
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

// maybeAttachJobSchedules fills snap.JobSchedules when the export gate
// is open and one of four conditions holds: the cached set's hash
// differs from the hash of the last set the mite sent, the stream
// reconnected since the last send, the resend interval has passed since
// the last send, or the earliest cron occurrence of the last sent set
// has passed and a newer poll exists. Every other call leaves the field
// unset, so the field is absent from most snapshots. The set never
// contributes to any state hash — the caller computes those before this
// call runs.
func (a *Agent) maybeAttachJobSchedules(snap *mitev1.StateSnapshot) {
	if !a.scheduleExportOpen() {
		return
	}

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

	now := time.Now()
	changed := set.Hash != a.jobScheduleLastSentHash
	stale := now.Sub(a.jobScheduleLastSendTime) >= resendAfter
	passed := !a.jobScheduleLastSentEarliest.IsZero() &&
		!now.Before(a.jobScheduleLastSentEarliest) &&
		a.jobScheduleSetSeq != a.jobScheduleLastSentSeq
	if !changed && !a.jobScheduleForceSend && !stale && !passed {
		return
	}

	snap.JobSchedules = set
	a.jobScheduleLastSentHash = set.Hash
	a.jobScheduleLastSendTime = now
	a.jobScheduleLastSentSeq = a.jobScheduleSetSeq
	a.jobScheduleLastSentEarliest = a.jobScheduleSetEarliest
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
	a.jobBranchForceSend = true
	a.jobScheduleMu.Unlock()
}

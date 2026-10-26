package mite

import (
	"context"
	"encoding/json"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// kindJobSchedules is the ingest kind a job.schedules envelope carries.
// The literal value must match internal/recordstore.KindJobSchedules.
const kindJobSchedules = "job.schedules"

// defaultJobScheduleStreamMissingTTL bounds how long the gateway waits
// before it retries a job.schedules publish after one failed, so a
// record-store-disabled install produces no error flood and no round
// trip on every snapshot (design.md D8 "The gateway").
const defaultJobScheduleStreamMissingTTL = 5 * time.Minute

// jobScheduleEnvelopePayload is the payload of a job.schedules envelope,
// per design.md D8 "The envelope". Field names mirror
// internal/recordstore's JobSchedulesPayload; the two packages do not
// share a Go type because the gateway and the recorder communicate over
// the wire only.
type jobScheduleEnvelopePayload struct {
	Hash      string           `json:"hash"`
	Truncated bool             `json:"truncated"`
	Total     int              `json:"total"`
	Rows      []jobScheduleRow `json:"rows"`
}

// jobScheduleRow is one row of a job.schedules envelope's payload. Every
// row carries its own tenant and placement generation: one envelope
// describes a whole cell, and a cell hosts many tenants with independent
// generations.
type jobScheduleRow struct {
	Tenant              string   `json:"tenant"`
	PlacementGeneration int64    `json:"placement_generation"`
	JobPath             string   `json:"job_path"`
	TriggerType         string   `json:"trigger_type"`
	Spec                string   `json:"spec"`
	Timezone            string   `json:"timezone"`
	Disabled            bool     `json:"disabled"`
	NextFireTimes       []string `json:"next_fire_times"`
	UpstreamTenant      string   `json:"upstream_tenant,omitempty"`
	UpstreamJob         string   `json:"upstream_job,omitempty"`
	UpstreamResult      string   `json:"upstream_result,omitempty"`
}

// jobScheduleStreamMissingTTL returns the configured negative-cache
// duration, or the default when unset.
func (h *BusHandler) jobScheduleStreamMissingTTL() time.Duration {
	if h.scheduleStreamMissingTTL > 0 {
		return h.scheduleStreamMissingTTL
	}
	return defaultJobScheduleStreamMissingTTL
}

// scheduleClock returns the negative-cache clock, or time.Now when unset.
func (h *BusHandler) scheduleClock() time.Time {
	if h.scheduleNow != nil {
		return h.scheduleNow()
	}
	return time.Now()
}

// handleJobSchedules is OnSnapshot's post-strip hook for the job-schedule
// set. When the snapshot carried a set, it replaces any retained set for
// this connection — a later snapshot's set always wins over an earlier
// retained one. Whichever set is now current, it attempts one publish;
// a resolved and durably published set clears the retained state, and
// an unresolved row or a publish failure keeps it for the next tick.
func (h *BusHandler) handleJobSchedules(ns, name string, set *mitev1.JobScheduleSet) {
	key := ns + "/" + name

	if set != nil {
		h.scheduleMu.Lock()
		if h.scheduleRetained == nil {
			h.scheduleRetained = make(map[string]*mitev1.JobScheduleSet)
		}
		h.scheduleRetained[key] = set
		h.scheduleMu.Unlock()
	}

	h.scheduleMu.Lock()
	current := h.scheduleRetained[key]
	h.scheduleMu.Unlock()
	if current == nil {
		return
	}

	if h.jobScheduleStreamMissing(key) {
		return
	}

	if h.publishJobSchedules(ns, name, current) {
		h.scheduleMu.Lock()
		delete(h.scheduleRetained, key)
		h.scheduleMu.Unlock()
	}
}

// jobScheduleStreamMissing reports whether key's negative cache is still
// armed, so the caller skips the publish attempt entirely.
func (h *BusHandler) jobScheduleStreamMissing(key string) bool {
	h.scheduleMu.Lock()
	defer h.scheduleMu.Unlock()
	until, ok := h.scheduleStreamMissingUntil[key]
	return ok && h.scheduleClock().Before(until)
}

// armJobScheduleStreamMissing arms key's negative cache after a failed
// publish attempt.
func (h *BusHandler) armJobScheduleStreamMissing(key string) {
	h.scheduleMu.Lock()
	if h.scheduleStreamMissingUntil == nil {
		h.scheduleStreamMissingUntil = make(map[string]time.Time)
	}
	h.scheduleStreamMissingUntil[key] = h.scheduleClock().Add(h.jobScheduleStreamMissingTTL())
	h.scheduleMu.Unlock()
}

// clearJobScheduleStreamMissing disarms key's negative cache after a
// successful publish.
func (h *BusHandler) clearJobScheduleStreamMissing(key string) {
	h.scheduleMu.Lock()
	delete(h.scheduleStreamMissingUntil, key)
	h.scheduleMu.Unlock()
}

// publishJobSchedules resolves every row of set and, only when every row
// resolves, publishes one job.schedules envelope on the cell's build
// subject with a JetStream acknowledgement. The envelope time is the time
// the cell took the set, so a kept or resent set keeps its time. A set
// with no taken time publishes nothing. One unresolved row publishes
// nothing and counts a rejection; the caller retains set for the next
// tick. It reports whether the publish succeeded (the retained set can
// then be cleared).
func (h *BusHandler) publishJobSchedules(ns, name string, set *mitev1.JobScheduleSet) bool {
	key := ns + "/" + name

	if set.TakenAtMillis <= 0 {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("job schedule set has no taken time, publishing no envelope", "controller", key)
		return false
	}

	rows := make([]jobScheduleRow, 0, len(set.Items))
	for _, item := range set.Items {
		if item == nil {
			// design.md D8 "The envelope": all or nothing. A nil row is
			// as malformed as an unresolved tenant; it rejects the whole
			// envelope the same way, not a skipped row.
			h.metrics.buildEventsDropped.Add(context.Background(), 1)
			h.log().Warn("job schedule row nil, publishing no envelope", "controller", key)
			return false
		}
		tenant, generation := h.resolver().ResolveJobTenant(h.cluster, ns, name, item.JobPath)
		if tenant == "" {
			// design.md D8 "The envelope": all or nothing. One unresolved
			// row rejects the whole envelope; the caller keeps the
			// retained set for the next tick's retry.
			h.metrics.buildEventsDropped.Add(context.Background(), 1)
			h.log().Warn("job schedule row unresolved, publishing no envelope",
				"controller", key, "job_path", item.JobPath)
			return false
		}
		rows = append(rows, jobScheduleRow{
			Tenant:              tenant,
			PlacementGeneration: generation,
			JobPath:             item.JobPath,
			TriggerType:         item.TriggerType,
			Spec:                item.Spec,
			Timezone:            item.Timezone,
			Disabled:            item.Disabled,
			NextFireTimes:       item.NextFireTimes,
			UpstreamTenant:      item.UpstreamTenant,
			UpstreamJob:         item.UpstreamJob,
			UpstreamResult:      item.UpstreamResult,
		})
	}

	env := ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kindJobSchedules,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		OccurredAt:    millisToTime(set.TakenAtMillis),
		Payload: map[string]interface{}{
			"hash":      set.Hash,
			"truncated": set.Truncated,
			"total":     set.Total,
			"rows":      rows,
		},
	}

	data, err := json.Marshal(env)
	if err != nil {
		h.log().Error("marshal job schedule envelope failed", "controller", key, "error", err)
		return false
	}

	if _, err := h.publisher.PublishJetStream(bus.BuildSubject(h.cluster, ns, name), data); err != nil {
		h.metrics.buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("job schedule publish failed, retaining for retry",
			"controller", key, "error", err)
		h.armJobScheduleStreamMissing(key)
		return false
	}

	h.clearJobScheduleStreamMissing(key)
	return true
}

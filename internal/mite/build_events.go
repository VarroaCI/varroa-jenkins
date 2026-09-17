package mite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// Build event phases a mite reports. These match the literal strings the
// Jenkins plugin sends on BuildEvent.phase.
const (
	phaseStarted      = "STARTED"
	phaseStageStarted = "STAGE_STARTED"
	phaseStage        = "STAGE"
	phaseHeartbeat    = "HEARTBEAT"
	phaseCompleted    = "COMPLETED"
)

// Ingest kinds the recorder accepts on builds.<cluster>.<ns>.<cell>. The
// literal values must match internal/recordstore's ingest kind constants.
const (
	kindRunStarted   = "run.started"
	kindRunStage     = "run.stage"
	kindRunCompleted = "run.completed"
	kindRunTests     = "run.tests"
	kindRunArtifacts = "run.artifacts"
	kindRunChanges   = "run.changes"
	kindRunLog       = "run.log"
	kindRunHeartbeat = "run.heartbeat"
	// kindRunTriggerFailed records a build trigger Jenkins refused. The
	// gateway publishes it from a failed TRIGGER_BUILD result, not from a
	// BuildEvent.
	kindRunTriggerFailed = "run.trigger_failed"
	// kindRunTriggerAccepted records a build trigger Jenkins accepted onto
	// its queue. The gateway publishes it from a successful TRIGGER_BUILD
	// result, not from a BuildEvent.
	kindRunTriggerAccepted = "run.trigger_accepted"
	// kindRunState records one run-state push outcome. The gateway
	// publishes it after it stores a file, and after it refuses a push
	// because the run passed its byte cap.
	kindRunState = "run.state"
)

// Reason codes a run.trigger_failed envelope carries. The set is complete:
// every status maps onto exactly one of them.
const (
	reasonJobNotFound     = "JOB_NOT_FOUND"
	reasonJobNotBuildable = "JOB_NOT_BUILDABLE"
	reasonTriggerRejected = "TRIGGER_REJECTED"
)

// reasonCodeForStatus maps the Jenkins HTTP status of a failed trigger onto
// one reason code. The status 0 names a call that reached no Jenkins.
func reasonCodeForStatus(status int) string {
	switch status {
	case 404:
		return reasonJobNotFound
	case 409:
		return reasonJobNotBuildable
	default:
		return reasonTriggerRejected
	}
}

// EventAck status values.
const (
	eventAckStored   = "STORED"
	eventAckRejected = "REJECTED"
)

// maskedParamValue replaces the value of a sensitive parameter.
const maskedParamValue = "<masked>"

// gatewayEnvelopeBudget is the byte budget for one marshaled envelope,
// matching the plugin-inventory degradation path's headroom below NATS'
// 1 MiB message limit.
const gatewayEnvelopeBudget = 900 * 1024

// errorTruncateChars is the character cap the size ladder applies to a
// run.stage envelope's error field.
const errorTruncateChars = 512

var (
	buildEventsDropped   metric.Int64Counter
	triggerFailedTotal   metric.Int64Counter
	triggerAcceptedTotal metric.Int64Counter
)

func init() {
	meter := otel.Meter("varroa-gateway")
	buildEventsDropped, _ = meter.Int64Counter("varroa.gateway.build_events.dropped",
		metric.WithDescription("Build events and build event publishes the gateway dropped or rejected"),
	)
	triggerFailedTotal, _ = meter.Int64Counter("varroa.gateway.trigger_failed",
		metric.WithDescription("Failed build triggers the gateway published as a record"),
	)
	triggerAcceptedTotal, _ = meter.Int64Counter("varroa.gateway.trigger_accepted",
		metric.WithDescription("Accepted build triggers the gateway published as a record"),
	)
}

// ingestEnvelope is the wire shape of one message on
// builds.<cluster>.<ns>.<cell>. Field names and JSON tags mirror
// internal/recordstore's IngestEnvelope exactly; the two packages do not
// share a Go type because the gateway and the recorder are separate
// binaries and communicate over the wire only.
type ingestEnvelope struct {
	SchemaVersion int                    `json:"schema_version"`
	Kind          string                 `json:"kind"`
	RunID         string                 `json:"run_id"`
	IntentKey     string                 `json:"intent_key"`
	Cluster       string                 `json:"cluster"`
	Namespace     string                 `json:"namespace"`
	Cell          string                 `json:"cell"`
	CellInstance  string                 `json:"cell_instance"`
	Tenant        string                 `json:"tenant"`
	JobPath       string                 `json:"job_path"`
	OccurredAt    time.Time              `json:"occurred_at"`
	Payload       map[string]interface{} `json:"payload"`
}

// changeEntry is one entry of a run.changes envelope's payload["changes"]
// array.
type changeEntry struct {
	CommitID          string    `json:"commit_id"`
	AuthorID          string    `json:"author_id"`
	AuthorDisplayName string    `json:"author_display_name"`
	Message           string    `json:"message"`
	Timestamp         time.Time `json:"timestamp"`
	AffectedPathCount int       `json:"affected_path_count"`
}

// kindPayload is one mapped envelope's kind and payload, before the
// envelope-level identity fields are stamped on.
type kindPayload struct {
	kind    string
	payload map[string]interface{}
}

// millisToTime converts a millisecond epoch value to a UTC time.Time. A
// zero input has no meaningful conversion; callers check for zero before
// calling this so an absent field stays absent rather than becoming the
// Unix epoch.
func millisToTime(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

// truncateRunes cuts s to at most n runes, so a truncation never splits a
// multi-byte UTF-8 character.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// maskParameters copies params, replacing the value of every name in
// masked with the literal <masked>. It returns nil for an empty map, so
// callers can omit the payload key entirely.
func maskParameters(params map[string]string, masked []string) map[string]string {
	if len(params) == 0 {
		return nil
	}
	maskedSet := make(map[string]bool, len(masked))
	for _, name := range masked {
		maskedSet[name] = true
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		if maskedSet[k] {
			out[k] = maskedParamValue
		} else {
			out[k] = v
		}
	}
	return out
}

// addTemplateFields copies evt's four template-resolution fields onto p,
// omitting any field the event does not carry. A run with no template
// carries none, so p gains no key. This runs only for kinds C1 folds into
// runs.payload (run.started, run.completed): design.md D14 reads the
// resolved template from the job's most recent run, so a stage-level
// envelope needs no copy.
func addTemplateFields(p map[string]interface{}, evt *mitev1.BuildEvent) {
	if evt.TemplateName != "" {
		p["template_name"] = evt.TemplateName
	}
	if evt.TemplateVersion != "" {
		p["template_version"] = evt.TemplateVersion
	}
	if evt.MarkerPath != "" {
		p["marker_path"] = evt.MarkerPath
	}
	if evt.MarkerRevision != "" {
		p["marker_revision"] = evt.MarkerRevision
	}
}

// startedPayload maps a STARTED BuildEvent onto a run.started payload, per
// design.md D9's payload-key table. A field the event does not carry is
// omitted, so the recorder's coalescing merge leaves the stored column
// untouched rather than clearing it.
func startedPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{}
	if evt.BuildNumber != 0 {
		p["build_number"] = evt.BuildNumber
	}
	if evt.Branch != "" {
		p["branch"] = evt.Branch
	}
	if evt.QueuedAtMillis != 0 {
		p["queued_at"] = millisToTime(evt.QueuedAtMillis)
	}
	if evt.StartedAtMillis != 0 {
		p["started_at"] = millisToTime(evt.StartedAtMillis)
	}
	if evt.CauseDescription != "" {
		p["cause"] = evt.CauseDescription
	}
	if evt.Actor != "" {
		p["actor"] = evt.Actor
	}
	if params := maskParameters(evt.Parameters, evt.MaskedParameterNames); params != nil {
		p["params"] = params
	}
	if evt.Seq != 0 {
		p["seq"] = evt.Seq
	}
	if evt.URL != "" {
		p["url"] = evt.URL
	}
	if evt.JobDisplayName != "" {
		p["job_display_name"] = evt.JobDisplayName
	}
	if evt.BuildDisplayName != "" {
		p["build_display_name"] = evt.BuildDisplayName
	}
	if len(evt.CauseTypes) > 0 {
		p["cause_types"] = evt.CauseTypes
	}
	if evt.UpstreamJobPath != "" {
		p["upstream_job_path"] = evt.UpstreamJobPath
	}
	if evt.UpstreamBuildNumber != 0 {
		p["upstream_build_number"] = evt.UpstreamBuildNumber
	}
	if evt.QueueWaitMillis != 0 {
		p["queue_wait_ms"] = evt.QueueWaitMillis
	}
	if evt.EstimatedDurationMillis != 0 {
		p["estimated_duration_ms"] = evt.EstimatedDurationMillis
	}
	if len(evt.MaskedParameterNames) > 0 {
		p["masked_params"] = evt.MaskedParameterNames
	}
	if evt.ScmUrl != "" {
		p["scm_url"] = evt.ScmUrl
	}
	if evt.ScmRevision != "" {
		p["scm_revision"] = evt.ScmRevision
	}
	if evt.DeliveryId != "" {
		p["delivery_id"] = evt.DeliveryId
	}
	addTemplateFields(p, evt)
	return p
}

// stagePayload maps one BuildStage onto a run.stage payload. It carries
// the typed subset C1 reads plus every other BuildStage field, because
// C1 keeps the whole envelope in run_stages.payload even where it has no
// typed column.
func stagePayload(st *mitev1.BuildStage) map[string]interface{} {
	p := map[string]interface{}{
		"stage_id":  st.StageId,
		"name":      st.Name,
		"node_type": st.NodeType,
		"status":    st.Status,
	}
	if st.ParentId != "" {
		p["parent_id"] = st.ParentId
	}
	if st.StartedAtMillis != 0 {
		p["started_at"] = millisToTime(st.StartedAtMillis)
	}
	if st.EndedAtMillis != 0 {
		p["ended_at"] = millisToTime(st.EndedAtMillis)
	}
	if st.DurationMillis != 0 {
		p["duration_ms"] = st.DurationMillis
	}
	if st.Error != "" {
		p["error"] = st.Error
	}
	if len(st.ParentIds) > 0 {
		p["parent_ids"] = st.ParentIds
	}
	if st.EnclosingId != "" {
		p["enclosing_id"] = st.EnclosingId
	}
	if st.ParallelBranchName != "" {
		p["parallel_branch_name"] = st.ParallelBranchName
	}
	if st.PauseDurationMillis != 0 {
		p["pause_duration_millis"] = st.PauseDurationMillis
	}
	if st.NodeName != "" {
		p["node_name"] = st.NodeName
	}
	if st.AgentLabel != "" {
		p["agent_label"] = st.AgentLabel
	}
	return p
}

// heartbeatPayload maps a HEARTBEAT BuildEvent onto a run.heartbeat
// payload.
func heartbeatPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{}
	if evt.Seq != 0 {
		p["seq"] = evt.Seq
	}
	return p
}

// changesPayload maps a COMPLETED BuildEvent's changes onto a run.changes
// payload.
func changesPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	entries := make([]changeEntry, 0, len(evt.Changes))
	for _, c := range evt.Changes {
		if c == nil {
			continue
		}
		entries = append(entries, changeEntry{
			CommitID:          c.CommitId,
			AuthorID:          c.AuthorId,
			AuthorDisplayName: c.AuthorDisplayName,
			Message:           c.Message,
			Timestamp:         millisToTime(c.TimestampMillis),
			AffectedPathCount: c.AffectedPathCount,
		})
	}
	p := map[string]interface{}{"changes": entries}
	if evt.ChangeCount != 0 {
		p["change_count"] = evt.ChangeCount
	}
	return p
}

// testsPayload maps a COMPLETED BuildEvent's test counters onto the
// aggregate run.tests payload. suite is always the sentinel "_total": C2
// captures aggregate counters only.
func testsPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	return map[string]interface{}{
		"suite":   "_total",
		"total":   evt.TestTotal,
		"failed":  evt.TestFailed,
		"skipped": evt.TestSkipped,
		"passed":  evt.TestPassed,
	}
}

// completedPayload maps a COMPLETED BuildEvent onto a run.completed
// payload.
func completedPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{}
	if evt.Result != "" {
		p["result"] = evt.Result
	}
	if evt.CompletedAtMillis != 0 {
		p["ended_at"] = millisToTime(evt.CompletedAtMillis)
	}
	if evt.DurationMillis != 0 {
		p["duration_ms"] = evt.DurationMillis
	}
	if evt.Seq != 0 {
		p["seq"] = evt.Seq
	}
	addTemplateFields(p, evt)
	return p
}

// artifactPayload maps one BuildArtifact onto a run.artifacts payload.
// Field names mirror internal/recordstore's RunArtifactsPayload; that
// struct carries no field for BuildArtifact.storage or
// BuildArtifact.display_path, so this mapping carries neither.
func artifactPayload(art *mitev1.BuildArtifact) map[string]interface{} {
	p := map[string]interface{}{
		"path":     art.Path,
		"blob_key": art.BlobKey,
	}
	if art.SizeBytes != 0 {
		p["size_bytes"] = art.SizeBytes
	}
	return p
}

// logSummaryPayload maps a COMPLETED BuildEvent's LogSummary onto a
// run.log payload, merged with the gateway's own per-run incompleteness
// flag by logical OR. finalized always comes from the gateway's own
// knowledge: the plugin's LogSummary never sets it, because only the
// gateway's durable final index entry proves the log is finished.
func (h *BusHandler) logSummaryPayload(evt *mitev1.BuildEvent) map[string]interface{} {
	p := map[string]interface{}{}
	var incomplete bool
	var reason string
	if ls := evt.LogSummary; ls != nil {
		p["chunk_count"] = ls.ChunkCount
		p["dropped_bytes"] = ls.DroppedBytes
		incomplete = ls.Incomplete
		reason = ls.IncompleteReason
	}
	if gwIncomplete, gwReason := h.runIncompleteness(evt.RunId); gwIncomplete {
		incomplete = true
		if reason == "" {
			reason = gwReason
		}
	}
	p["incomplete"] = incomplete
	if reason != "" {
		p["incomplete_reason"] = reason
	}
	p["finalized"] = h.runFinalized(evt.RunId)
	return p
}

// buildPayloadGroup maps one non-heartbeat BuildEvent onto the ordered
// list of kinds and payloads design.md D9 states. The caller handles
// HEARTBEAT separately, because a heartbeat is droppable and every other
// phase is not. run.artifacts (one envelope per BuildArtifact) and
// run.log publish immediately before run.completed, per design.md A0.
func (h *BusHandler) buildPayloadGroup(evt *mitev1.BuildEvent) ([]kindPayload, error) {
	switch evt.Phase {
	case phaseStarted:
		return []kindPayload{{kindRunStarted, startedPayload(evt)}}, nil

	case phaseStageStarted, phaseStage:
		if len(evt.Stages) != 1 {
			return nil, errors.New("malformed stage event: want exactly one stage")
		}
		payload := stagePayload(evt.Stages[0])
		if evt.Phase == phaseStageStarted {
			// A defensive restamp: the plugin already sets IN_PROGRESS for
			// a start event, but the gateway does not trust the payload
			// for this invariant.
			payload["status"] = "IN_PROGRESS"
		}
		return []kindPayload{{kindRunStage, payload}}, nil

	case phaseCompleted:
		out := make([]kindPayload, 0, len(evt.Stages)+len(evt.Artifacts)+4)
		for _, st := range evt.Stages {
			if st == nil {
				continue
			}
			out = append(out, kindPayload{kindRunStage, stagePayload(st)})
		}
		if len(evt.Changes) > 0 {
			out = append(out, kindPayload{kindRunChanges, changesPayload(evt)})
		}
		if evt.TestTotal > 0 {
			out = append(out, kindPayload{kindRunTests, testsPayload(evt)})
		}
		for _, art := range evt.Artifacts {
			if art == nil {
				continue
			}
			out = append(out, kindPayload{kindRunArtifacts, artifactPayload(art)})
		}
		out = append(out, kindPayload{kindRunLog, h.logSummaryPayload(evt)})
		out = append(out, kindPayload{kindRunCompleted, completedPayload(evt)})
		return out, nil

	default:
		return nil, fmt.Errorf("unknown phase %q", evt.Phase)
	}
}

// shrinkToBudget applies design.md D9's size ladder to env until its
// marshaled size fits gatewayEnvelopeBudget, or returns false when it
// still does not fit. It truncates payload["error"], then drops
// payload["changes"] entries from the oldest, then drops
// payload["stages"] entries from the oldest (defensive: no kind this
// package emits carries a plural "stages" key today).
func shrinkToBudget(env *ingestEnvelope) bool {
	fits := func() bool {
		data, err := json.Marshal(env)
		return err == nil && len(data) <= gatewayEnvelopeBudget
	}
	if fits() {
		return true
	}

	if errVal, ok := env.Payload["error"].(string); ok && errVal != "" {
		env.Payload["error"] = truncateRunes(errVal, errorTruncateChars)
		if fits() {
			return true
		}
	}

	if changes, ok := env.Payload["changes"].([]changeEntry); ok {
		for len(changes) > 0 {
			changes = changes[1:]
			env.Payload["changes"] = changes
			if fits() {
				return true
			}
		}
	}

	if stages, ok := env.Payload["stages"].([]interface{}); ok {
		for len(stages) > 0 {
			stages = stages[1:]
			env.Payload["stages"] = stages
			if fits() {
				return true
			}
		}
	}

	return fits()
}

// buildEventItem is one unit the publish worker drains: either the single
// droppable envelope of a HEARTBEAT event, or every ordered envelope of
// one other BuildEvent, acknowledged together once all are durably
// published.
type buildEventItem struct {
	heartbeat bool
	runID     string
	eventSeq  int64
	envelopes []ingestEnvelope
}

// maxQueueEnvelopes bounds the publish worker's queue. It is sized for
// roughly one large COMPLETED event (at most 1000 stages plus a changes,
// a tests, and a completed envelope, per the plugin's own D11 caps).
// enqueue admits one larger item into an empty queue.
const maxQueueEnvelopes = 1024

// buildEventWorker is the per-connection publish worker. One instance
// runs for the life of one CommandStream, so envelopes publish in the
// per-cell order the recorder's monotonic status merge needs, and a slow
// stream never blocks another controller.
type buildEventWorker struct {
	ctx  context.Context
	send Sender

	mu        sync.Mutex
	queue     []buildEventItem
	queueLen  int // sum of len(item.envelopes) across queue
	notify    chan struct{}
	roomFreed chan struct{}
}

func newBuildEventWorker(ctx context.Context, send Sender) *buildEventWorker {
	return &buildEventWorker{
		ctx:       ctx,
		send:      send,
		notify:    make(chan struct{}, 1),
		roomFreed: make(chan struct{}, 1),
	}
}

// tryEnqueueHeartbeat enqueues item without waiting for room. It reports
// false when the queue has no room, so the caller can drop and count the
// heartbeat instead.
func (w *buildEventWorker) tryEnqueueHeartbeat(item buildEventItem) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queueLen+len(item.envelopes) > maxQueueEnvelopes {
		return false
	}
	w.queue = append(w.queue, item)
	w.queueLen += len(item.envelopes)
	w.wakeLocked()
	return true
}

// enqueue waits up to wait for room for the whole item, so a group's
// envelopes are never split across two waits. It reports false when no
// room freed within wait, or the connection ended while waiting.
//
// An item whose own envelope count exceeds maxQueueEnvelopes is admitted
// when the queue is empty. The plugin caps stages but not artifacts, so
// one COMPLETED event can outgrow the bound. Without this exception each
// retry of that event is rejected again forever. The empty-queue rule
// still stops a pile of smaller items.
func (w *buildEventWorker) enqueue(item buildEventItem, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		w.mu.Lock()
		if w.queueLen == 0 || w.queueLen+len(item.envelopes) <= maxQueueEnvelopes {
			w.queue = append(w.queue, item)
			w.queueLen += len(item.envelopes)
			w.wakeLocked()
			w.mu.Unlock()
			return true
		}
		w.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-w.roomFreed:
			timer.Stop()
		case <-timer.C:
			return false
		case <-w.ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// wakeLocked signals the drain goroutine. Caller must hold w.mu.
func (w *buildEventWorker) wakeLocked() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// dequeue removes and returns the oldest queued item, and signals any
// producer waiting for room.
func (w *buildEventWorker) dequeue() (buildEventItem, bool) {
	w.mu.Lock()
	if len(w.queue) == 0 {
		w.mu.Unlock()
		return buildEventItem{}, false
	}
	item := w.queue[0]
	w.queue = w.queue[1:]
	w.queueLen -= len(item.envelopes)
	w.mu.Unlock()
	select {
	case w.roomFreed <- struct{}{}:
	default:
	}
	return item, true
}

// registeredBuildWorker pairs a worker with the connection epoch it was
// created for, so a lookup from a superseded stream's connToken misses.
type registeredBuildWorker struct {
	token  int64
	worker *buildEventWorker
}

// getBuildWorker returns the worker for name/ns, or nil when none is
// registered or connToken names a superseded connection.
func (h *BusHandler) getBuildWorker(ns, name string, connToken int64) *buildEventWorker {
	key := ns + "/" + name
	h.buildMu.Lock()
	defer h.buildMu.Unlock()
	rw, ok := h.buildWorkers[key]
	if !ok || (connToken != 0 && rw.token != connToken) {
		return nil
	}
	return rw.worker
}

// startBuildEventWorker registers and starts the publish worker for one
// new connection. Called from OnConnect with the same ctx and send the
// other per-connection watchers use.
func (h *BusHandler) startBuildEventWorker(ctx context.Context, ns, name string, token int64, send Sender) {
	key := ns + "/" + name
	w := newBuildEventWorker(ctx, send)
	h.buildMu.Lock()
	if h.buildWorkers == nil {
		h.buildWorkers = make(map[string]*registeredBuildWorker)
	}
	h.buildWorkers[key] = &registeredBuildWorker{token: token, worker: w}
	h.buildMu.Unlock()
	go h.runBuildEventWorker(ctx, ns, name, w)
}

// clearBuildWorker removes the worker registered for token, if any. A
// call for a token a newer connection already replaced is a no-op, so a
// superseded stream's late teardown cannot drop the live connection's
// worker.
func (h *BusHandler) clearBuildWorker(ns, name string, token int64) {
	key := ns + "/" + name
	h.buildMu.Lock()
	defer h.buildMu.Unlock()
	if rw, ok := h.buildWorkers[key]; ok && (token == 0 || rw.token == token) {
		delete(h.buildWorkers, key)
	}
}

// runBuildEventWorker drains w until ctx is done. It never publishes on
// the CommandStream read loop: OnBuildEvent only enqueues, and this
// goroutine performs every JetStream publish and every EventAck send.
func (h *BusHandler) runBuildEventWorker(ctx context.Context, ns, name string, w *buildEventWorker) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.notify:
		}
		for {
			if ctx.Err() != nil {
				return
			}
			item, ok := w.dequeue()
			if !ok {
				break
			}
			h.deliverBuildEventItem(ctx, ns, name, w.send, item)
		}
	}
}

// deliverBuildEventItem publishes every envelope of item in order, then
// sends STORED once all publishes are durably acknowledged. A heartbeat
// item publishes asynchronously and best-effort, and never gets an
// EventAck.
func (h *BusHandler) deliverBuildEventItem(ctx context.Context, ns, name string, send Sender, item buildEventItem) {
	subject := bus.BuildSubject(h.cluster, ns, name)

	if item.heartbeat {
		if len(item.envelopes) != 1 {
			return
		}
		h.publishHeartbeatEnvelope(ctx, subject, item.envelopes[0], ns, name)
		return
	}

	for _, env := range item.envelopes {
		data, err := json.Marshal(env)
		if err != nil {
			h.log().Error("marshal build envelope failed",
				"controller", ns+"/"+name, "kind", env.Kind, "error", err)
			return
		}
		if !h.publishWithRetry(ctx, subject, data, env.Kind, ns, name) {
			// ctx ended (disconnect). The mite still retains the event and
			// replays it after reconnect, so abandoning here is safe.
			return
		}
	}
	h.sendEventAck(send, item.runID, item.eventSeq, eventAckStored, "")
}

const (
	defaultPublishRetryDelay = 1 * time.Second
	publishRetryMaxDelay     = 30 * time.Second
)

// publishRetryDelay returns the starting retry delay for a rejected
// build-event publish. buildPublishRetryDelay lets a test shrink it; zero
// means defaultPublishRetryDelay.
func (h *BusHandler) publishRetryDelay() time.Duration {
	if h.buildPublishRetryDelay > 0 {
		return h.buildPublishRetryDelay
	}
	return defaultPublishRetryDelay
}

// publishWithRetry publishes data to subject and waits for the JetStream
// ack, retrying with a growing delay until it succeeds. It returns false
// only when ctx ends first.
func (h *BusHandler) publishWithRetry(ctx context.Context, subject string, data []byte, kind, ns, name string) bool {
	delay := h.publishRetryDelay()
	for {
		_, err := h.publisher.PublishJetStream(subject, data)
		if err == nil {
			return true
		}
		buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("build event publish rejected, retrying",
			"controller", ns+"/"+name, "kind", kind, "retryIn", delay, "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		if delay *= 2; delay > publishRetryMaxDelay {
			delay = publishRetryMaxDelay
		}
	}
}

// publishHeartbeatEnvelope publishes one run.heartbeat envelope
// asynchronously and reads its PubAckFuture. A publish failure is
// counted and never retried: the next heartbeat follows within the
// interval.
func (h *BusHandler) publishHeartbeatEnvelope(ctx context.Context, subject string, env ingestEnvelope, ns, name string) {
	data, err := json.Marshal(env)
	if err != nil {
		buildEventsDropped.Add(context.Background(), 1)
		h.log().Error("marshal heartbeat envelope failed", "controller", ns+"/"+name, "error", err)
		return
	}
	future, err := h.publisher.PublishJetStreamAsync(subject, data)
	if err != nil {
		buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("heartbeat publish failed", "controller", ns+"/"+name, "error", err)
		return
	}
	select {
	case <-future.Ok():
	case pubErr := <-future.Err():
		buildEventsDropped.Add(context.Background(), 1)
		h.log().Warn("heartbeat publish rejected", "controller", ns+"/"+name, "error", pubErr)
	case <-ctx.Done():
	}
}

// sendEventAck sends one OperatorMessage_EventAck to the mite.
func (h *BusHandler) sendEventAck(send Sender, runID string, eventSeq int64, status, reason string) {
	msg := &mitev1.OperatorMessage{
		Message: &mitev1.OperatorMessage_EventAck{
			EventAck: &mitev1.EventAck{
				RunId:    runID,
				EventSeq: eventSeq,
				Status:   status,
				Reason:   reason,
			},
		},
	}
	if err := send(msg); err != nil {
		h.log().Error("send event ack failed",
			"run_id", runID, "event_seq", eventSeq, "status", status, "error", err)
	}
}

// buildEnvelope stamps the identity fields of one ingest envelope. cell
// and cell_instance never come from the event payload: cell/namespace
// come from the mTLS stream identity (the name/ns parameters), and
// cell_instance from the mite's registered pod unique identifier. tenant
// is resolved once per event by the caller, through CellTenantResolver.
func (h *BusHandler) buildEnvelope(ns, name string, evt *mitev1.BuildEvent, kind string, payload map[string]interface{}, tenant string) ingestEnvelope {
	return ingestEnvelope{
		SchemaVersion: 1,
		Kind:          kind,
		RunID:         evt.RunId,
		IntentKey:     evt.IntentKey,
		Cluster:       h.cluster,
		Namespace:     ns,
		Cell:          name,
		CellInstance:  h.lookupCellInstance(ns, name),
		Tenant:        tenant,
		JobPath:       evt.JobPath,
		OccurredAt:    millisToTime(evt.EmittedAtMillis),
		Payload:       payload,
	}
}

// RecordCellInstance stores the pod unique identifier a mite presented at
// registration, keyed by controller. Server.Register calls this so a
// later build event on this connection — possibly a different
// CommandStream than the one Register arrived on — can stamp the same
// value the mite also sends on a ClaimRequest.
func (h *BusHandler) RecordCellInstance(name, ns, cellInstance string) {
	h.mu.Lock()
	if h.cellInstances == nil {
		h.cellInstances = make(map[string]string)
	}
	h.cellInstances[ns+"/"+name] = cellInstance
	h.mu.Unlock()
}

func (h *BusHandler) lookupCellInstance(ns, name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cellInstances[ns+"/"+name]
}

const defaultEventPublishWait = 10 * time.Second

// eventPublishWait reads VARROA_EVENT_PUBLISH_WAIT, the back-pressure
// budget OnBuildEvent waits for room in the publish worker's queue
// before it ends the stream.
func eventPublishWait() time.Duration {
	v := os.Getenv("VARROA_EVENT_PUBLISH_WAIT")
	if v == "" {
		return defaultEventPublishWait
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultEventPublishWait
	}
	return d
}

// OnBuildEvent maps evt onto one or more ingest envelopes and enqueues
// them on this connection's publish worker. It never publishes to
// JetStream itself: a synchronous publish of a large COMPLETED event
// would stall this controller's heartbeats on the CommandStream read
// loop, which is where the caller runs this method from.
func (h *BusHandler) OnBuildEvent(name, ns string, connToken int64, evt *mitev1.BuildEvent) error {
	w := h.getBuildWorker(ns, name, connToken)
	if w == nil {
		// The connection already ended. The read loop's IsCurrentConnection
		// gate should already have stopped this call from reaching here.
		return nil
	}

	// Resolved once per event, from this event's own job path: one cell
	// hosts many tenants, so a build event's tenant is not fixed per
	// connection the way cluster/namespace/cell are (design.md D14).
	tenant, _ := h.resolver().ResolveJobTenant(h.cluster, ns, name, evt.JobPath)
	if tenant == "" {
		// A cell must not write a record under a tenant it does not
		// host. This is an admission refusal, not the back-pressure case
		// below: the mite must release the event, not replay it forever.
		buildEventsDropped.Add(context.Background(), 1)
		if evt.Phase != phaseHeartbeat {
			h.sendEventAck(w.send, evt.RunId, evt.Seq, eventAckRejected, "unhosted tenant")
		}
		return nil
	}

	if evt.Phase == phaseHeartbeat {
		env := h.buildEnvelope(ns, name, evt, kindRunHeartbeat, heartbeatPayload(evt), tenant)
		item := buildEventItem{heartbeat: true, envelopes: []ingestEnvelope{env}}
		if !w.tryEnqueueHeartbeat(item) {
			buildEventsDropped.Add(context.Background(), 1)
		}
		return nil
	}

	if evt.Phase == phaseCompleted && evt.RunId != "" {
		// The log writer's finalize deadline anchors on this moment, so
		// it must be recorded before the ordered envelope group below
		// reads the gateway's own incompleteness and finalized knowledge.
		h.markRunTerminal(ns, name, evt.RunId, evt.JobPath, millisToTime(evt.CompletedAtMillis))
	}

	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		buildEventsDropped.Add(context.Background(), 1)
		h.sendEventAck(w.send, evt.RunId, evt.Seq, eventAckRejected, err.Error())
		return nil
	}

	envs := make([]ingestEnvelope, 0, len(kps))
	for _, kp := range kps {
		env := h.buildEnvelope(ns, name, evt, kp.kind, kp.payload, tenant)
		if !shrinkToBudget(&env) {
			buildEventsDropped.Add(context.Background(), 1)
			h.sendEventAck(w.send, evt.RunId, evt.Seq, eventAckRejected, "OVERSIZED")
			return nil
		}
		envs = append(envs, env)
	}

	item := buildEventItem{runID: evt.RunId, eventSeq: evt.Seq, envelopes: envs}
	if !w.enqueue(item, eventPublishWait()) {
		return fmt.Errorf("build event queue full for %s/%s: no room within the publish wait", ns, name)
	}
	return nil
}

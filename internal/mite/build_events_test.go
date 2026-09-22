package mite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// fakePubAck is a nats.PubAckFuture a test can resolve either way.
type fakePubAck struct {
	okCh  chan *nats.PubAck
	errCh chan error
}

func newOKPubAck() *fakePubAck {
	f := &fakePubAck{okCh: make(chan *nats.PubAck, 1), errCh: make(chan error, 1)}
	f.okCh <- &nats.PubAck{}
	return f
}

func newErrPubAck(err error) *fakePubAck {
	f := &fakePubAck{okCh: make(chan *nats.PubAck, 1), errCh: make(chan error, 1)}
	f.errCh <- err
	return f
}

func (f *fakePubAck) Ok() <-chan *nats.PubAck { return f.okCh }
func (f *fakePubAck) Err() <-chan error       { return f.errCh }
func (f *fakePubAck) Msg() *nats.Msg          { return nil }

type publishedMsg struct {
	subject string
	data    []byte
}

// fakePublisher implements buildEventPublisher. Sync publishes fail
// syncFailCount times (counting down) before succeeding; async publishes
// resolve with asyncErr when set.
type fakePublisher struct {
	mu           sync.Mutex
	syncFailLeft int
	syncErr      error
	asyncErr     error
	published    []publishedMsg
	syncCalls    int
}

func (f *fakePublisher) PublishJetStream(subject string, data []byte) (*nats.PubAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls++
	if f.syncFailLeft > 0 {
		f.syncFailLeft--
		return nil, f.syncErr
	}
	cp := append([]byte(nil), data...)
	f.published = append(f.published, publishedMsg{subject, cp})
	return &nats.PubAck{}, nil
}

func (f *fakePublisher) PublishJetStreamAsync(subject string, data []byte) (nats.PubAckFuture, error) {
	if f.asyncErr != nil {
		// Mirrors nats.go's real PublishAsync: the call itself succeeds
		// (nil error); the failure surfaces later on the future's Err()
		// channel, which publishHeartbeatEnvelope reads.
		//nolint:nilerr
		return newErrPubAck(f.asyncErr), nil
	}
	f.mu.Lock()
	cp := append([]byte(nil), data...)
	f.published = append(f.published, publishedMsg{subject, cp})
	f.mu.Unlock()
	return newOKPubAck(), nil
}

func (f *fakePublisher) publishedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

func (f *fakePublisher) envelopeAt(i int) ingestEnvelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	var env ingestEnvelope
	_ = json.Unmarshal(f.published[i].data, &env)
	return env
}

// fakeSender records every OperatorMessage sent to the mite.
type fakeSender struct {
	mu   sync.Mutex
	sent []*mitev1.OperatorMessage
}

func (f *fakeSender) send(msg *mitev1.OperatorMessage) error {
	f.mu.Lock()
	f.sent = append(f.sent, msg)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// first returns the earliest message sent, for a test that expects
// exactly one.
func (f *fakeSender) first() *mitev1.OperatorMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[0]
}

func (f *fakeSender) acks() []*mitev1.EventAck {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*mitev1.EventAck
	for _, m := range f.sent {
		if a := m.GetEventAck(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// newBuildEventTestHandler builds a BusHandler with fake publish/ledger
// seams, no NATS connection required for these narrow unit tests.
func newBuildEventTestHandler(pub *fakePublisher) *BusHandler {
	return &BusHandler{
		cluster:        "cluster1",
		publisher:      pub,
		cellInstances:  make(map[string]string),
		buildWorkers:   make(map[string]*registeredBuildWorker),
		claimCounts:    make(map[string]int),
		tenantResolver: defaultCellTenantResolver{},
		logRuns:        make(map[string]*runLogState),
		ownerCache:     newOwnerCache(0),
	}
}

// testConnToken is the connection epoch every startTestWorker call uses.
const testConnToken = 1

// startTestWorker registers and starts a build-event worker directly,
// bypassing OnConnect's NATS-backed watchers, which these tests do not
// exercise. Every caller uses the fixed identity "ns1"/"ctrl1" and
// testConnToken.
func startTestWorker(t *testing.T, h *BusHandler, send Sender) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.startBuildEventWorker(ctx, "ns1", "ctrl1", testConnToken, send)
	t.Cleanup(cancel)
	return cancel
}

// --- 3.3: envelope mapping ---

func TestBuildPayloadGroup_CompletedOrder(t *testing.T) {
	evt := &mitev1.BuildEvent{
		Phase: phaseCompleted,
		RunId: "run-1",
		Stages: []*mitev1.BuildStage{
			{StageId: "s1", Name: "build", NodeType: "stage", Status: "SUCCESS"},
			{StageId: "s2", Name: "test", NodeType: "stage", Status: "SUCCESS"},
		},
		Changes:   []*mitev1.BuildChange{{CommitId: "abc"}},
		TestTotal: 5,
		Artifacts: []*mitev1.BuildArtifact{{Path: "a.txt", BlobKey: "artifacts/c/n/cell/a.txt"}},
		Result:    "SUCCESS",
	}
	h := newBuildEventTestHandler(nil)
	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		t.Fatalf("buildPayloadGroup: %v", err)
	}
	kinds := make([]string, 0, len(kps))
	for _, kp := range kps {
		kinds = append(kinds, kp.kind)
	}
	want := []string{kindRunStage, kindRunStage, kindRunChanges, kindRunTests, kindRunArtifacts, kindRunLog, kindRunCompleted}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("kind order = %v, want %v", kinds, want)
	}
	artEnv := kps[4]
	if artEnv.payload["path"] != "a.txt" || artEnv.payload["blob_key"] != "artifacts/c/n/cell/a.txt" {
		t.Fatalf("artifact entry reached run.artifacts changed: %+v", artEnv.payload)
	}
}

func TestBuildPayloadGroup_CompletedOmitsEmptyOptionalKinds(t *testing.T) {
	evt := &mitev1.BuildEvent{Phase: phaseCompleted, RunId: "run-1", Result: "SUCCESS"}
	h := newBuildEventTestHandler(nil)
	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		t.Fatalf("buildPayloadGroup: %v", err)
	}
	// run.log always publishes (it carries the finalized/incomplete
	// summary even with an empty LogSummary); run.artifacts is omitted
	// when the event carries no artifacts.
	if len(kps) != 2 || kps[0].kind != kindRunLog || kps[1].kind != kindRunCompleted {
		t.Fatalf("kps = %+v, want exactly run.log then run.completed", kps)
	}
}

func TestBuildPayloadGroup_PerPhase(t *testing.T) {
	cases := []struct {
		name string
		evt  *mitev1.BuildEvent
		want string
	}{
		{"started", &mitev1.BuildEvent{Phase: phaseStarted, RunId: "r"}, kindRunStarted},
		{
			"stage_started",
			&mitev1.BuildEvent{Phase: phaseStageStarted, RunId: "r", Stages: []*mitev1.BuildStage{{StageId: "s1", Status: "IN_PROGRESS"}}},
			kindRunStage,
		},
		{
			"stage",
			&mitev1.BuildEvent{Phase: phaseStage, RunId: "r", Stages: []*mitev1.BuildStage{{StageId: "s1", Status: "SUCCESS"}}},
			kindRunStage,
		},
	}
	h := newBuildEventTestHandler(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kps, err := h.buildPayloadGroup(tc.evt)
			if err != nil {
				t.Fatalf("buildPayloadGroup: %v", err)
			}
			if len(kps) != 1 || kps[0].kind != tc.want {
				t.Fatalf("kps = %+v, want exactly one %s", kps, tc.want)
			}
		})
	}
}

func TestBuildPayloadGroup_StageStartedForcesInProgress(t *testing.T) {
	evt := &mitev1.BuildEvent{
		Phase:  phaseStageStarted,
		RunId:  "r",
		Stages: []*mitev1.BuildStage{{StageId: "s1", Status: "SUCCESS"}}, // wrong on purpose
	}
	h := newBuildEventTestHandler(nil)
	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		t.Fatalf("buildPayloadGroup: %v", err)
	}
	if got := kps[0].payload["status"]; got != "IN_PROGRESS" {
		t.Fatalf("status = %v, want IN_PROGRESS", got)
	}
}

func TestBuildPayloadGroup_UnknownPhase(t *testing.T) {
	h := newBuildEventTestHandler(nil)
	_, err := h.buildPayloadGroup(&mitev1.BuildEvent{Phase: "BOGUS", RunId: "r"})
	if err == nil {
		t.Fatal("want an error for an unknown phase")
	}
}

func TestBuildPayloadGroup_MalformedStageEvent(t *testing.T) {
	h := newBuildEventTestHandler(nil)
	_, err := h.buildPayloadGroup(&mitev1.BuildEvent{Phase: phaseStage, RunId: "r"}) // no stages
	if err == nil {
		t.Fatal("want an error for a stage event with no stage")
	}
}

// TestBuildPayloadGroup_LogSummaryMergesGatewayIncompleteness asserts the
// run.log envelope's incomplete flag is a logical OR of the plugin's
// LogSummary and the gateway's own per-run incompleteness, and that
// finalized always comes from the gateway's own knowledge.
func TestBuildPayloadGroup_LogSummaryMergesGatewayIncompleteness(t *testing.T) {
	h := newBuildEventTestHandler(nil)
	h.logRuns = map[string]*runLogState{
		"run-1": {incomplete: true, incompleteReason: logReasonGatewayGap, finalDurable: true},
	}
	evt := &mitev1.BuildEvent{
		Phase: phaseCompleted, RunId: "run-1", Result: "SUCCESS",
		LogSummary: &mitev1.LogSummary{ChunkCount: 3, Incomplete: false},
	}
	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		t.Fatalf("buildPayloadGroup: %v", err)
	}
	var logEnv kindPayload
	for _, kp := range kps {
		if kp.kind == kindRunLog {
			logEnv = kp
		}
	}
	if logEnv.payload["incomplete"] != true {
		t.Errorf("incomplete = %v, want true (merged from the gateway)", logEnv.payload["incomplete"])
	}
	if logEnv.payload["incomplete_reason"] != logReasonGatewayGap {
		t.Errorf("incomplete_reason = %v, want %q", logEnv.payload["incomplete_reason"], logReasonGatewayGap)
	}
	if logEnv.payload["finalized"] != true {
		t.Errorf("finalized = %v, want true (from the gateway, never the plugin)", logEnv.payload["finalized"])
	}
}

// --- masking ---

func TestStartedPayload_MasksParameters(t *testing.T) {
	evt := &mitev1.BuildEvent{
		Phase:                phaseStarted,
		RunId:                "r",
		Parameters:           map[string]string{"TOKEN": "secret", "BRANCH": "main"},
		MaskedParameterNames: []string{"TOKEN"},
	}
	p := startedPayload(evt)
	params, ok := p["params"].(map[string]string)
	if !ok {
		t.Fatalf("params missing or wrong type: %+v", p)
	}
	if params["TOKEN"] != maskedParamValue {
		t.Fatalf("TOKEN = %q, want masked", params["TOKEN"])
	}
	if params["BRANCH"] != "main" {
		t.Fatalf("BRANCH = %q, want main", params["BRANCH"])
	}
}

// --- 6.3/6.4: template fields reach the ingest envelope ---

// TestTemplateFields_ReachStartedAndCompletedPayloads proves the gateway
// copies all four template fields into both the run.started and the
// run.completed payload, per design.md D14: both feed runs.payload, so
// either kind's key answers the job's most recent run.
func TestTemplateFields_ReachStartedAndCompletedPayloads(t *testing.T) {
	base := mitev1.BuildEvent{
		RunId:           "run-1",
		TemplateName:    "go-service",
		TemplateVersion: "2.1.4",
		MarkerPath:      ".varroa/pipeline.yaml",
		MarkerRevision:  "abc123",
	}

	started := base
	started.Phase = phaseStarted
	sp := startedPayload(&started)
	assertTemplateFields(t, sp)

	completed := base
	completed.Phase = phaseCompleted
	completed.Result = "SUCCESS"
	cp := completedPayload(&completed)
	assertTemplateFields(t, cp)
}

// assertTemplateFields checks p carries the four template keys the test
// fixtures above set.
func assertTemplateFields(t *testing.T, p map[string]interface{}) {
	t.Helper()
	want := map[string]string{
		"template_name":    "go-service",
		"template_version": "2.1.4",
		"marker_path":      ".varroa/pipeline.yaml",
		"marker_revision":  "abc123",
	}
	for k, v := range want {
		if got := p[k]; got != v {
			t.Errorf("payload[%q] = %v, want %q", k, got, v)
		}
	}
}

// TestTemplateFields_OmittedForNoTemplateAndHeartbeat proves a run with no
// template adds no key to run.started/run.completed, and that a heartbeat
// carries none of the four fields even when the event sets them.
func TestTemplateFields_OmittedForNoTemplateAndHeartbeat(t *testing.T) {
	noTemplate := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1"}
	sp := startedPayload(noTemplate)
	for _, k := range []string{"template_name", "template_version", "marker_path", "marker_revision"} {
		if _, ok := sp[k]; ok {
			t.Errorf("run.started payload carries %q for a run with no template", k)
		}
	}

	withTemplate := &mitev1.BuildEvent{
		Phase:           phaseHeartbeat,
		RunId:           "run-1",
		TemplateName:    "go-service",
		TemplateVersion: "2.1.4",
		MarkerPath:      ".varroa/pipeline.yaml",
		MarkerRevision:  "abc123",
	}
	hp := heartbeatPayload(withTemplate)
	for _, k := range []string{"template_name", "template_version", "marker_path", "marker_revision"} {
		if _, ok := hp[k]; ok {
			t.Errorf("run.heartbeat payload carries %q, want none of the four template fields", k)
		}
	}
}

// --- summary: the two carrier payloads ---

// TestSummaryPayload_CarriersWriteSummaryOnlyWhenSet proves both carrier
// payloads write the summary key only for a non-empty value, and write
// seq unconditionally, including the value zero. The recorder's summary
// guard reads seq on every heartbeat, so a zero seq stays on the wire.
func TestSummaryPayload_CarriersWriteSummaryOnlyWhenSet(t *testing.T) {
	const text = "deployed canary to 10%"
	for _, tc := range []struct {
		name    string
		phase   string
		payload func(*mitev1.BuildEvent) map[string]interface{}
	}{
		{"run.heartbeat", phaseHeartbeat, heartbeatPayload},
		{"run.completed", phaseCompleted, completedPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSummary := tc.payload(&mitev1.BuildEvent{
				Phase:   tc.phase,
				RunId:   "run-1",
				Seq:     4,
				Summary: text,
			})
			if got := withSummary["summary"]; got != text {
				t.Errorf("summary = %v, want %q", got, text)
			}

			withoutSummary := tc.payload(&mitev1.BuildEvent{Phase: tc.phase, RunId: "run-1"})
			if _, ok := withoutSummary["summary"]; ok {
				t.Errorf("payload carries a summary key for an empty value: %+v", withoutSummary)
			}
			if got := withoutSummary["seq"]; got != int64(0) {
				t.Errorf("seq = %v, want the value zero written unconditionally", got)
			}
		})
	}
}

// TestOnBuildEvent_TemplateFieldsReachPublishedEnvelope is the end-to-end
// gateway mapping test: a STARTED BuildEvent's template fields must reach
// the published run.started envelope's payload.
func TestOnBuildEvent_TemplateFieldsReachPublishedEnvelope(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	send := &fakeSender{}
	startTestWorker(t, h, send.send)

	evt := &mitev1.BuildEvent{
		Phase:           phaseStarted,
		RunId:           "run-1",
		JobPath:         "job1",
		TemplateName:    "go-service",
		TemplateVersion: "2.1.4",
		MarkerPath:      ".varroa/pipeline.yaml",
		MarkerRevision:  "abc123",
	}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	waitFor(t, func() bool { return pub.publishedCount() == 1 }, time.Second, "envelope not published")
	env := pub.envelopeAt(0)
	if env.Kind != kindRunStarted {
		t.Fatalf("kind = %q, want %q", env.Kind, kindRunStarted)
	}
	if env.Payload["template_name"] != "go-service" {
		t.Errorf("template_name = %v, want go-service", env.Payload["template_name"])
	}
	if env.Payload["template_version"] != "2.1.4" {
		t.Errorf("template_version = %v, want 2.1.4", env.Payload["template_version"])
	}
	if env.Payload["marker_path"] != ".varroa/pipeline.yaml" {
		t.Errorf("marker_path = %v, want .varroa/pipeline.yaml", env.Payload["marker_path"])
	}
	if env.Payload["marker_revision"] != "abc123" {
		t.Errorf("marker_revision = %v, want abc123", env.Payload["marker_revision"])
	}
}

// --- 3.3a: publish worker queue ---

func TestBuildEventWorker_FullQueueRejectsNonHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newBuildEventWorker(ctx, func(*mitev1.OperatorMessage) error { return nil })
	w.queueLen = maxQueueEnvelopes // simulate a full queue with no drain

	item := buildEventItem{runID: "r", envelopes: []ingestEnvelope{{}}}
	ok := w.enqueue(item, 30*time.Millisecond)
	if ok {
		t.Fatal("enqueue on a full queue should report false")
	}
	if len(w.queue) != 0 {
		t.Fatalf("queue should be untouched, got %d items", len(w.queue))
	}
}

// TestBuildEventWorker_OversizedItemAdmittedOnEmptyQueue proves a single
// item whose own envelope count exceeds maxQueueEnvelopes still enqueues
// once the queue is empty. The plugin caps stages at 1000 but places no
// cap on artifacts, so one COMPLETED event's own ordered group can
// legally exceed the bound; without this admission the item is rejected
// forever and the run is never recorded.
func TestBuildEventWorker_OversizedItemAdmittedOnEmptyQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newBuildEventWorker(ctx, func(*mitev1.OperatorMessage) error { return nil })

	envelopes := make([]ingestEnvelope, maxQueueEnvelopes+1)
	item := buildEventItem{runID: "r", envelopes: envelopes}
	ok := w.enqueue(item, 30*time.Millisecond)
	if !ok {
		t.Fatal("an oversized item must still enqueue onto an empty queue")
	}
	if len(w.queue) != 1 || w.queueLen != len(envelopes) {
		t.Fatalf("queue = %d items, queueLen = %d, want 1 item of length %d", len(w.queue), w.queueLen, len(envelopes))
	}

	// A second item, however small, must wait for the first to drain:
	// the empty-queue exception does not let an oversized backlog build
	// up unbounded.
	second := buildEventItem{runID: "r2", envelopes: []ingestEnvelope{{}}}
	if w.enqueue(second, 30*time.Millisecond) {
		t.Fatal("a second item must not enqueue while the oversized item still occupies the queue")
	}
}

func TestBuildEventWorker_HeartbeatDroppedWhenFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newBuildEventWorker(ctx, func(*mitev1.OperatorMessage) error { return nil })
	w.queueLen = maxQueueEnvelopes

	item := buildEventItem{heartbeat: true, envelopes: []ingestEnvelope{{}}}
	if w.tryEnqueueHeartbeat(item) {
		t.Fatal("tryEnqueueHeartbeat on a full queue should report false")
	}
}

func TestOnBuildEvent_QueueFullClosesStreamAndDropsNothing(t *testing.T) {
	t.Setenv("VARROA_EVENT_PUBLISH_WAIT", "30ms")
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	h.buildMu.Lock()
	h.buildWorkers["ns1/ctrl1"].worker.mu.Lock()
	h.buildWorkers["ns1/ctrl1"].worker.queueLen = maxQueueEnvelopes
	h.buildWorkers["ns1/ctrl1"].worker.mu.Unlock()
	h.buildMu.Unlock()

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "r1", Seq: 1}
	err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt)
	if err == nil {
		t.Fatal("want an error when the worker queue has no room")
	}
	if pub.publishedCount() != 0 {
		t.Fatalf("published %d envelopes, want 0 (no drop, no silent publish)", pub.publishedCount())
	}
}

func TestOnBuildEvent_HeartbeatDroppedWhenFullReturnsNilError(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	h.buildMu.Lock()
	h.buildWorkers["ns1/ctrl1"].worker.mu.Lock()
	h.buildWorkers["ns1/ctrl1"].worker.queueLen = maxQueueEnvelopes
	h.buildWorkers["ns1/ctrl1"].worker.mu.Unlock()
	h.buildMu.Unlock()

	evt := &mitev1.BuildEvent{Phase: phaseHeartbeat, RunId: "r1", Seq: 1}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("heartbeat drop should return nil, got %v", err)
	}
}

// --- 3.3b / 3.5: ack + sync publish + retry ---

func TestOnBuildEvent_StoredAckAfterPublish(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 7, Branch: "main"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	waitFor(t, func() bool { return len(sender.acks()) == 1 }, time.Second, "condition not met")
	ack := sender.acks()[0]
	if ack.RunId != "run-1" || ack.EventSeq != 7 || ack.Status != eventAckStored {
		t.Fatalf("ack = %+v, want STORED for run-1/7", ack)
	}
	if pub.publishedCount() != 1 {
		t.Fatalf("published %d envelopes, want 1", pub.publishedCount())
	}
}

func TestOnBuildEvent_NoAckForHeartbeat(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseHeartbeat, RunId: "run-1", Seq: 1}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return pub.publishedCount() == 1 }, time.Second, "condition not met")
	time.Sleep(50 * time.Millisecond) // give a stray ack a chance to arrive
	if len(sender.acks()) != 0 {
		t.Fatalf("got %d acks for a heartbeat, want 0", len(sender.acks()))
	}
}

// --- 11.4: the build-event path resolves per job path, and fails closed
// on an unhosted tenant ---

func TestOnBuildEvent_ResolvesTenantFromJobPath(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"payments/build": {tenant: "payments", generation: 4},
	}}
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 1, JobPath: "payments/build"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	waitFor(t, func() bool { return pub.publishedCount() == 1 }, time.Second, "condition not met")
	if got := pub.envelopeAt(0).Tenant; got != "payments" {
		t.Fatalf("envelope tenant = %q, want payments", got)
	}
}

func TestOnBuildEvent_UnhostedTenantRejectedAndDropped(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"payments/build": {tenant: "payments", generation: 4},
	}}
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 1, JobPath: "search/build"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	waitFor(t, func() bool { return len(sender.acks()) == 1 }, time.Second, "condition not met")
	ack := sender.acks()[0]
	if ack.Status != eventAckRejected || ack.Reason == "" {
		t.Fatalf("ack = %+v, want REJECTED with a reason", ack)
	}
	if pub.publishedCount() != 0 {
		t.Fatalf("published %d envelopes for an unhosted tenant, want 0", pub.publishedCount())
	}
}

func TestOnBuildEvent_UnhostedTenantHeartbeatDroppedWithNoAck(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"payments/build": {tenant: "payments", generation: 4},
	}}
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseHeartbeat, RunId: "run-1", Seq: 1, JobPath: "search/build"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	// A heartbeat never gets an ack (design.md D8), and an unhosted-tenant
	// heartbeat publishes nothing either.
	time.Sleep(50 * time.Millisecond)
	if len(sender.acks()) != 0 {
		t.Fatalf("got %d acks for an unhosted-tenant heartbeat, want 0", len(sender.acks()))
	}
	if pub.publishedCount() != 0 {
		t.Fatalf("published %d envelopes for an unhosted-tenant heartbeat, want 0", pub.publishedCount())
	}
}

func TestOnBuildEvent_NonCellControllerKeepsCompoundTenant(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 1, JobPath: "some/job"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return pub.publishedCount() == 1 }, time.Second, "condition not met")
	if got := pub.envelopeAt(0).Tenant; got != "ns1/ctrl1" {
		t.Fatalf("envelope tenant = %q, want ns1/ctrl1", got)
	}
}

func TestOnBuildEvent_RejectedAckForUnknownPhase(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: "BOGUS", RunId: "run-1", Seq: 3}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return len(sender.acks()) == 1 }, time.Second, "condition not met")
	ack := sender.acks()[0]
	if ack.Status != eventAckRejected || ack.Reason == "" {
		t.Fatalf("ack = %+v, want REJECTED with a reason", ack)
	}
	if pub.publishedCount() != 0 {
		t.Fatalf("published %d envelopes for a rejected event, want 0", pub.publishedCount())
	}
}

func TestPublishWithRetry_RetriesThenAcks(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 2, syncErr: fmt.Errorf("stream full")}
	h := newBuildEventTestHandler(pub)
	h.buildPublishRetryDelay = 5 * time.Millisecond
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 1}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}

	// No ack while the publish keeps failing.
	time.Sleep(4 * time.Millisecond)
	if len(sender.acks()) != 0 {
		t.Fatalf("got an ack before the publish succeeded")
	}

	waitFor(t, func() bool { return len(sender.acks()) == 1 }, time.Second, "condition not met")
	if pub.syncCalls < 3 {
		t.Fatalf("syncCalls = %d, want at least 3 (2 failures + 1 success)", pub.syncCalls)
	}
	if pub.publishedCount() != 1 {
		t.Fatalf("published %d envelopes, want 1", pub.publishedCount())
	}
}

// --- 3.4: size ladder ---

func TestShrinkToBudget_DropsOldestChanges(t *testing.T) {
	const entryCount = 10
	entries := make([]changeEntry, 0, entryCount)
	for i := 0; i < entryCount; i++ {
		entries = append(entries, changeEntry{
			CommitID: fmt.Sprintf("commit-%d", i),
			Message:  strings.Repeat("x", 200*1024), // 200 KiB each, ~2 MiB total
		})
	}
	env := ingestEnvelope{
		Kind:    kindRunChanges,
		Payload: map[string]interface{}{"changes": entries},
	}
	data, _ := json.Marshal(env)
	if len(data) <= gatewayEnvelopeBudget {
		t.Fatalf("fixture is not oversized: %d bytes", len(data))
	}

	ok := shrinkToBudget(&env)
	if !ok {
		t.Fatal("shrinkToBudget should fit by dropping changes")
	}
	shrunk, _ := json.Marshal(env)
	if len(shrunk) > gatewayEnvelopeBudget {
		t.Fatalf("still oversized after shrink: %d bytes", len(shrunk))
	}
	remaining := env.Payload["changes"].([]changeEntry)
	if len(remaining) == 0 || len(remaining) >= entryCount {
		t.Fatalf("want some but not all changes dropped, got %d remaining", len(remaining))
	}
	// Oldest-first: the earliest commit ids are the ones dropped.
	if remaining[0].CommitID == "commit-0" {
		t.Fatal("want the oldest entries dropped first")
	}
}

// --- 5.6: the run.source mapping ---

func TestBuildPayloadGroup_RunSourceMapsSourceThenChanges(t *testing.T) {
	evt := &mitev1.BuildEvent{
		Phase:          phaseRunSource,
		RunId:          "run-1",
		Branch:         "main",
		ScmRevision:    "abc123",
		ScmHead:        "main",
		ScmUrl:         "https://example.invalid/repo.git",
		DefinitionPath: "Jenkinsfile",
		Libraries:      []*mitev1.BuildLibrary{{Name: "shared", Version: "1.0", Revision: "def456"}},
		PodTemplates:   []*mitev1.BuildPodTemplate{{Name: "pod-1", Label: "linux", Image: "img:1"}},
		Changes:        []*mitev1.BuildChange{{CommitId: "abc123"}},
		ChangeCount:    1,
	}
	h := newBuildEventTestHandler(nil)
	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		t.Fatalf("buildPayloadGroup: %v", err)
	}
	if len(kps) != 2 || kps[0].kind != kindRunSource || kps[1].kind != kindRunChanges {
		t.Fatalf("kps = %+v, want run.source then run.changes", kps)
	}
	src := kps[0].payload
	if src["seq"] != evt.Seq {
		t.Fatalf("seq = %v, want %v written unconditionally", src["seq"], evt.Seq)
	}
	if src["branch"] != "main" || src["scm_revision"] != "abc123" || src["scm_head"] != "main" {
		t.Fatalf("source payload = %+v", src)
	}
	if src["definition_path"] != "Jenkinsfile" {
		t.Fatalf("definition_path = %v, want the Jenkinsfile path", src["definition_path"])
	}
	libs, ok := src["libraries"].([]libraryEntry)
	if !ok || len(libs) != 1 || libs[0].Name != "shared" || libs[0].Revision != "def456" {
		t.Fatalf("libraries = %+v", src["libraries"])
	}
	pods, ok := src["pod_templates"].([]podTemplateEntry)
	if !ok || len(pods) != 1 || pods[0].Image != "img:1" {
		t.Fatalf("pod_templates = %+v", src["pod_templates"])
	}
}

func TestBuildPayloadGroup_RunSourceWithoutChanges(t *testing.T) {
	evt := &mitev1.BuildEvent{Phase: phaseRunSource, RunId: "run-1", Seq: 3}
	h := newBuildEventTestHandler(nil)
	kps, err := h.buildPayloadGroup(evt)
	if err != nil {
		t.Fatalf("buildPayloadGroup: %v", err)
	}
	if len(kps) != 1 || kps[0].kind != kindRunSource {
		t.Fatalf("kps = %+v, want exactly one run.source envelope", kps)
	}
	p := kps[0].payload
	if p["seq"] != int64(3) {
		t.Fatalf("seq = %v, want 3", p["seq"])
	}
	for _, key := range []string{"branch", "scm_revision", "scm_head", "scm_url", "definition_path", "libraries", "pod_templates"} {
		if _, ok := p[key]; ok {
			t.Fatalf("payload carries %q for an empty value: %+v", key, p)
		}
	}
}

func TestShrinkToBudget_DropsOldestPodTemplatesThenLibraries(t *testing.T) {
	const entryCount = 8
	libs := make([]libraryEntry, 0, entryCount)
	pods := make([]podTemplateEntry, 0, entryCount)
	for i := 0; i < entryCount; i++ {
		libs = append(libs, libraryEntry{
			Name:    fmt.Sprintf("lib-%d", i),
			Version: strings.Repeat("v", 150*1024), // 150 KiB each, ~1.2 MiB total
		})
		pods = append(pods, podTemplateEntry{
			Name:  fmt.Sprintf("pod-%d", i),
			Image: strings.Repeat("p", 150*1024),
		})
	}
	env := ingestEnvelope{
		Kind: kindRunSource,
		Payload: map[string]interface{}{
			"seq":           int64(1),
			"libraries":     libs,
			"pod_templates": pods,
		},
	}
	data, _ := json.Marshal(env)
	if len(data) <= gatewayEnvelopeBudget {
		t.Fatalf("fixture is not oversized: %d bytes", len(data))
	}

	if !shrinkToBudget(&env) {
		t.Fatal("shrinkToBudget should fit by dropping the two lists")
	}
	shrunk, _ := json.Marshal(env)
	if len(shrunk) > gatewayEnvelopeBudget {
		t.Fatalf("still oversized after shrink: %d bytes", len(shrunk))
	}
	// pod_templates drops before libraries, so pod-0 goes before lib-0.
	remainingPods, ok := env.Payload["pod_templates"].([]podTemplateEntry)
	if !ok || len(remainingPods) != 0 {
		t.Fatalf("pod_templates remaining = %+v, want none", env.Payload["pod_templates"])
	}
	remainingLibs, ok := env.Payload["libraries"].([]libraryEntry)
	if !ok || len(remainingLibs) == 0 || len(remainingLibs) >= entryCount {
		t.Fatalf("libraries remaining = %+v, want some but not all dropped", env.Payload["libraries"])
	}
	if remainingLibs[0].Name == "lib-0" {
		t.Fatal("want the oldest libraries dropped first")
	}
	if env.Payload["seq"] != int64(1) {
		t.Fatalf("seq = %v, want the ladder to keep it", env.Payload["seq"])
	}
}

func TestOnBuildEvent_OversizeRunSourceStillPublishes(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	libs := make([]*mitev1.BuildLibrary, 0, 8)
	for i := 0; i < 8; i++ {
		libs = append(libs, &mitev1.BuildLibrary{
			Name:    fmt.Sprintf("lib-%d", i),
			Version: strings.Repeat("v", 150*1024),
		})
	}
	evt := &mitev1.BuildEvent{Phase: phaseRunSource, RunId: "run-1", Seq: 1, Libraries: libs}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return len(sender.acks()) == 1 }, time.Second, "condition not met")
	if ack := sender.acks()[0]; ack.Status == eventAckRejected {
		t.Fatalf("ack = %+v, want an accepted run.source publish", ack)
	}
	if pub.publishedCount() != 1 {
		t.Fatalf("published %d envelopes, want 1", pub.publishedCount())
	}
}

func TestOnBuildEvent_OversizedRejected(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	// The stage name has no size cap and the ladder never touches it, so an
	// oversized name stays oversized after every ladder step: the error
	// truncation and the changes/stages drops apply to other fields.
	evt := &mitev1.BuildEvent{
		Phase: phaseStage,
		RunId: "run-1",
		Seq:   9,
		Stages: []*mitev1.BuildStage{{
			StageId: "s1",
			Name:    strings.Repeat("z", gatewayEnvelopeBudget+1024),
			Status:  "SUCCESS",
		}},
	}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return len(sender.acks()) == 1 }, time.Second, "condition not met")
	ack := sender.acks()[0]
	if ack.Status != eventAckRejected || ack.Reason != "OVERSIZED" {
		t.Fatalf("ack = %+v, want REJECTED/OVERSIZED", ack)
	}
}

// --- ruling k: cell_instance stamping ---

func TestOnBuildEvent_StampsRegisteredCellInstance(t *testing.T) {
	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	h.RecordCellInstance("ctrl1", "ns1", "uid-1")
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 1}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return pub.publishedCount() == 1 }, time.Second, "condition not met")
	env := pub.envelopeAt(0)
	if env.CellInstance != "uid-1" {
		t.Fatalf("cell_instance = %q, want uid-1", env.CellInstance)
	}
}

// TestOnBuildEvent_MoveIsServedStaleWithinTTLThenRefreshedAfterExpiry
// wires the real, cache-backed CellTenantResolver (not a stub) into the
// build-event path and proves D14's staleness bound for a move: within
// the resolver's cache TTL, the build-event mapping still routes an
// event to the tenant's old cell, and once the cache entry expires it
// refuses an event the old cell no longer hosts.
func TestOnBuildEvent_MoveIsServedStaleWithinTTLThenRefreshedAfterExpiry(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl1", Namespace: "ns1"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
	})
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: "ctrl1", CellNamespace: "ns1", Generation: 1},
		},
	})
	resolver := NewCellTenantResolver(store)

	pub := &fakePublisher{}
	h := newBuildEventTestHandler(pub)
	h.tenantResolver = resolver
	sender := &fakeSender{}
	startTestWorker(t, h, sender.send)

	evt1 := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-1", Seq: 1, JobPath: "payments/build"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt1); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return pub.publishedCount() == 1 }, time.Second, "first event never published")
	if got := pub.envelopeAt(0).Tenant; got != "payments" {
		t.Fatalf("first envelope tenant = %q, want payments", got)
	}

	// The tenant moves off ctrl1/ns1 before the cache entry expires.
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: "ctrl2", CellNamespace: "ns2", Generation: 2},
		},
	})

	evt2 := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-2", Seq: 1, JobPath: "payments/build"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt2); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return pub.publishedCount() == 2 }, time.Second, "second event never published")
	if got := pub.envelopeAt(1).Tenant; got != "payments" {
		t.Fatalf("event within the TTL, tenant = %q, want the still-cached payments", got)
	}

	// Force the resolver's cache entry for "payments" past its TTL,
	// standing in for the passage of cellTenantResolveTTL.
	cr, ok := resolver.(*crdCellTenantResolver)
	if !ok {
		t.Fatalf("NewCellTenantResolver returned %T, want *crdCellTenantResolver", resolver)
	}
	cr.mu.Lock()
	entry := cr.teams["payments"]
	entry.expires = time.Now().Add(-time.Second)
	cr.teams["payments"] = entry
	cr.mu.Unlock()

	acksBefore := len(sender.acks())
	evt3 := &mitev1.BuildEvent{Phase: phaseStarted, RunId: "run-3", Seq: 1, JobPath: "payments/build"}
	if err := h.OnBuildEvent("ctrl1", "ns1", testConnToken, evt3); err != nil {
		t.Fatalf("OnBuildEvent: %v", err)
	}
	waitFor(t, func() bool { return len(sender.acks()) == acksBefore+1 }, time.Second, "unhosted-tenant rejection never sent")
	if pub.publishedCount() != 2 {
		t.Fatalf("published %d envelopes after TTL expiry, want still 2: ctrl1/ns1 no longer hosts payments", pub.publishedCount())
	}
	lastAck := sender.acks()[len(sender.acks())-1]
	if lastAck.Status != eventAckRejected {
		t.Fatalf("ack status = %v, want REJECTED once the resolver refreshes and finds ctrl1/ns1 no longer hosts payments", lastAck.Status)
	}
}

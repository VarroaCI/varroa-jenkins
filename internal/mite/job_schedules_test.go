package mite

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// stubJobTenantResolver implements CellTenantResolver for job.schedules
// tests, with a per-job-path lookup table. ResolveTenant (the claim
// path) is not exercised by these tests, so it always returns the
// single-tenant placeholder.
type stubJobTenantResolver struct {
	byJobPath map[string]stubTenantEntry
}

type stubTenantEntry struct {
	tenant     string
	generation int64
}

func (s stubJobTenantResolver) ResolveTenant(_, ns, cell string) (string, int64) {
	return ns + "/" + cell, 0
}

func (s stubJobTenantResolver) ResolveJobTenant(_, _, _, jobPath string) (string, int64) {
	v, ok := s.byJobPath[jobPath]
	if !ok {
		return "", 0
	}
	return v.tenant, v.generation
}

// newJobScheduleTestHandler builds a BusHandler backed by a live
// in-process NATS/JetStream server (for the core mite-inbound publish
// and the snapshot KV put) and pub as the JetStream publish seam for the
// job.schedules envelope.
func newJobScheduleTestHandler(t *testing.T, pub buildEventPublisher, resolver CellTenantResolver) (*BusHandler, *bus.Conn) {
	t.Helper()
	ns := startNATS(t)
	busConn, err := bus.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(busConn.Close)

	snapshotKV, err := busConn.EnsureKV(bus.KVSnapshotBucket, 0)
	if err != nil {
		t.Fatalf("ensure snapshot kv: %v", err)
	}

	if resolver == nil {
		resolver = defaultCellTenantResolver{}
	}

	h := &BusHandler{
		cluster:        bus.DefaultCluster,
		conn:           busConn,
		kv:             snapshotKV,
		publisher:      pub,
		cellInstances:  make(map[string]string),
		buildWorkers:   make(map[string]*registeredBuildWorker),
		claimCounts:    make(map[string]int),
		tenantResolver: resolver,
		logRuns:        make(map[string]*runLogState),
		ownerCache:     newOwnerCache(0),
	}
	return h, busConn
}

// decodeJobSchedulePayload re-marshals an ingestEnvelope's untyped
// Payload map into jobScheduleEnvelopePayload for row-level assertions.
func decodeJobSchedulePayload(t *testing.T, env ingestEnvelope) jobScheduleEnvelopePayload {
	t.Helper()
	raw, err := json.Marshal(env.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var p jobScheduleEnvelopePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return p
}

func oneRowSet() *mitev1.JobScheduleSet {
	return &mitev1.JobScheduleSet{
		Hash: "sh1",
		Items: []*mitev1.JobSchedule{
			{
				JobPath:       "team-a/build",
				TriggerType:   "cron",
				Spec:          "H/15 * * * *",
				Timezone:      "America/New_York",
				Disabled:      false,
				NextFireTimes: []string{"2026-09-05T13:07:00Z"},
			},
		},
		Total: 1,
	}
}

// --- 10.1: the strip ---

func TestOnSnapshot_StripsJobSchedules(t *testing.T) {
	pub := &fakePublisher{}
	h, busConn := newJobScheduleTestHandler(t, pub, nil)

	var (
		mu       sync.Mutex
		received []byte
	)
	sub, err := busConn.NATSConn().Subscribe(bus.MiteInSubject(h.cluster, "ns1", "ctrl1"), func(msg *nats.Msg) {
		mu.Lock()
		received = msg.Data
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	snap := &mitev1.StateSnapshot{
		ActualStateHash: "hash-1",
		JobSchedules:    oneRowSet(),
	}
	h.OnSnapshot("ctrl1", "ns1", snap)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return received != nil
	}, 5*time.Second, "no mite.in message received")

	mu.Lock()
	data := received
	mu.Unlock()

	var mm mitev1.MiteMessage
	if err := json.Unmarshal(data, &mm); err != nil {
		t.Fatalf("unmarshal mite.in message: %v", err)
	}
	got := mm.GetStateSnapshot()
	if got == nil {
		t.Fatal("no state snapshot in mite.in message")
	}
	if got.JobSchedules != nil {
		t.Errorf("mite.in snapshot carries job schedules, want stripped: %+v", got.JobSchedules)
	}
	if got.ActualStateHash != "hash-1" {
		t.Errorf("ActualStateHash = %q, want unchanged", got.ActualStateHash)
	}

	waitFor(t, func() bool {
		data, gerr := h.kv.Get(bus.SnapshotKey(h.cluster, "ns1", "ctrl1"))
		return gerr == nil && len(data) > 0
	}, 5*time.Second, "snapshot KV value never written")

	kvData, err := h.kv.Get(bus.SnapshotKey(h.cluster, "ns1", "ctrl1"))
	if err != nil {
		t.Fatalf("kv get: %v", err)
	}
	var kvSnap mitev1.StateSnapshot
	if err := json.Unmarshal(kvData, &kvSnap); err != nil {
		t.Fatalf("unmarshal kv snapshot: %v", err)
	}
	if kvSnap.JobSchedules != nil {
		t.Errorf("KV snapshot carries job schedules, want stripped: %+v", kvSnap.JobSchedules)
	}

	// The caller's own snapshot object is never mutated: OnSnapshot strips
	// a clone, not the original.
	if snap.JobSchedules == nil {
		t.Error("OnSnapshot mutated the caller's snapshot; want the original set left intact")
	}
}

// --- 10.2: the envelope ---

func TestOnSnapshot_JobScheduleEnvelopeFields(t *testing.T) {
	pub := &fakePublisher{}
	h, _ := newJobScheduleTestHandler(t, pub, nil)
	h.RecordCellInstance("ctrl1", "ns1", "uid-1")

	snap := &mitev1.StateSnapshot{JobSchedules: oneRowSet()}
	h.OnSnapshot("ctrl1", "ns1", snap)

	waitFor(t, func() bool { return pub.publishedCount() == 1 }, 5*time.Second, "envelope never published")

	env := pub.envelopeAt(0)
	if env.Kind != kindJobSchedules {
		t.Errorf("Kind = %q, want %q", env.Kind, kindJobSchedules)
	}
	if env.RunID != "" || env.IntentKey != "" || env.JobPath != "" || env.Tenant != "" {
		t.Errorf("envelope carries non-empty run_id/intent_key/job_path/tenant: %+v", env)
	}
	if env.Cluster != h.cluster || env.Namespace != "ns1" || env.Cell != "ctrl1" {
		t.Errorf("envelope identity = %+v, want cluster=%s ns=ns1 cell=ctrl1", env, h.cluster)
	}
	if env.CellInstance != "uid-1" {
		t.Errorf("CellInstance = %q, want uid-1", env.CellInstance)
	}
	if env.OccurredAt.IsZero() {
		t.Error("OccurredAt is zero, want the publish time")
	}

	payload := decodeJobSchedulePayload(t, env)
	if payload.Hash != "sh1" || payload.Total != 1 || payload.Truncated {
		t.Errorf("payload = %+v, want hash=sh1 total=1 truncated=false", payload)
	}
	if len(payload.Rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(payload.Rows))
	}
	row := payload.Rows[0]
	if row.Tenant != "ns1/ctrl1" || row.PlacementGeneration != 0 {
		t.Errorf("row tenant/generation = %q/%d, want ns1/ctrl1 / 0 (single-tenant placeholder)", row.Tenant, row.PlacementGeneration)
	}
	if row.JobPath != "team-a/build" || row.TriggerType != "cron" {
		t.Errorf("row job_path/trigger_type = %q/%q, want team-a/build/cron", row.JobPath, row.TriggerType)
	}
}

func TestOnSnapshot_JobScheduleEnvelope_TwoTenantsOneCell(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/build": {tenant: "alpha", generation: 4},
		"team-b/build": {tenant: "beta", generation: 9},
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	snap := &mitev1.StateSnapshot{JobSchedules: &mitev1.JobScheduleSet{
		Hash: "sh1",
		Items: []*mitev1.JobSchedule{
			{JobPath: "team-a/build", TriggerType: "cron", Spec: "H * * * *"},
			{JobPath: "team-b/build", TriggerType: "varroa-upstream",
				UpstreamTenant: "alpha", UpstreamJob: "build", UpstreamResult: "SUCCESS"},
		},
		Total: 2,
	}}
	h.OnSnapshot("ctrl1", "ns1", snap)

	waitFor(t, func() bool { return pub.publishedCount() == 1 }, 5*time.Second, "envelope never published")

	env := pub.envelopeAt(0)
	if env.Tenant != "" {
		t.Errorf("envelope top-level tenant = %q, want empty", env.Tenant)
	}
	payload := decodeJobSchedulePayload(t, env)
	if len(payload.Rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(payload.Rows))
	}
	byJobPath := map[string]jobScheduleRow{}
	for _, r := range payload.Rows {
		byJobPath[r.JobPath] = r
	}
	a := byJobPath["team-a/build"]
	b := byJobPath["team-b/build"]
	if a.Tenant != "alpha" || a.PlacementGeneration != 4 {
		t.Errorf("team-a row = %+v, want tenant=alpha generation=4", a)
	}
	if b.Tenant != "beta" || b.PlacementGeneration != 9 {
		t.Errorf("team-b row = %+v, want tenant=beta generation=9", b)
	}
	if b.UpstreamTenant != "alpha" || b.UpstreamJob != "build" || b.UpstreamResult != "SUCCESS" {
		t.Errorf("team-b upstream fields = %+v, want alpha/build/SUCCESS", b)
	}
}

// --- 10.2: all or nothing, and the retry ---

func TestOnSnapshot_JobSchedule_OneUnresolvedRowRejectsWholeEnvelope(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/build": {tenant: "alpha", generation: 1},
		// team-b/build is intentionally absent: unresolved.
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	snap := &mitev1.StateSnapshot{JobSchedules: &mitev1.JobScheduleSet{
		Hash: "sh1",
		Items: []*mitev1.JobSchedule{
			{JobPath: "team-a/build", TriggerType: "cron"},
			{JobPath: "team-b/build", TriggerType: "cron"},
		},
		Total: 2,
	}}
	h.OnSnapshot("ctrl1", "ns1", snap)

	// Give the synchronous OnSnapshot call a moment; it runs inline, but
	// this guards against a future async change silently making the
	// assertion racy.
	time.Sleep(20 * time.Millisecond)
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("publishedCount() = %d, want 0 (one unresolved row must reject the whole envelope)", got)
	}

	// A later tick with a resolver that now resolves everything must
	// still see BOTH original rows: the rejected attempt changed nothing
	// about the retained set.
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/build": {tenant: "alpha", generation: 1},
		"team-b/build": {tenant: "beta", generation: 2},
	}}
	h.OnSnapshot("ctrl1", "ns1", &mitev1.StateSnapshot{})

	waitFor(t, func() bool { return pub.publishedCount() == 1 }, 5*time.Second, "retained set never published")
	payload := decodeJobSchedulePayload(t, pub.envelopeAt(0))
	if len(payload.Rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (retained set must be the complete original set)", len(payload.Rows))
	}
}

func TestOnSnapshot_JobSchedule_NilRowRejectsWholeEnvelope(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/build": {tenant: "alpha", generation: 1},
	}}
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	snap := &mitev1.StateSnapshot{JobSchedules: &mitev1.JobScheduleSet{
		Hash: "sh1",
		Items: []*mitev1.JobSchedule{
			{JobPath: "team-a/build", TriggerType: "cron"},
			nil,
		},
		Total: 2,
	}}
	h.OnSnapshot("ctrl1", "ns1", snap)

	// Give the synchronous OnSnapshot call a moment; it runs inline, but
	// this guards against a future async change silently making the
	// assertion racy.
	time.Sleep(20 * time.Millisecond)
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("publishedCount() = %d, want 0 (a nil row must reject the whole envelope, not be skipped)", got)
	}
}

func TestOnSnapshot_JobSchedule_RetainedSetPublishesOnLaterTick(t *testing.T) {
	pub := &fakePublisher{}
	resolver := stubJobTenantResolver{} // resolves nothing at first
	h, _ := newJobScheduleTestHandler(t, pub, resolver)

	snap := &mitev1.StateSnapshot{JobSchedules: oneRowSet()}
	h.OnSnapshot("ctrl1", "ns1", snap)
	if got := pub.publishedCount(); got != 0 {
		t.Fatalf("publishedCount() = %d, want 0 on the first, unresolved tick", got)
	}

	// The resolver becomes able to resolve the job path; the next
	// snapshot carries no set at all.
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/build": {tenant: "ns1/ctrl1", generation: 0},
	}}
	h.OnSnapshot("ctrl1", "ns1", &mitev1.StateSnapshot{})

	waitFor(t, func() bool { return pub.publishedCount() == 1 }, 5*time.Second, "retained set never published on a later tick")

	// Once published, the gateway retains nothing: a further no-set tick
	// publishes nothing more.
	h.OnSnapshot("ctrl1", "ns1", &mitev1.StateSnapshot{})
	time.Sleep(20 * time.Millisecond)
	if got := pub.publishedCount(); got != 1 {
		t.Fatalf("publishedCount() = %d, want 1 (nothing retained after a successful publish)", got)
	}
}

// --- 10.3: the five-minute negative cache ---

func TestOnSnapshot_JobSchedule_StreamMissingNegativeCache(t *testing.T) {
	pub := &fakePublisher{syncFailLeft: 1000, syncErr: errStreamMissingForTest}
	h, _ := newJobScheduleTestHandler(t, pub, nil)
	h.scheduleStreamMissingTTL = 30 * time.Millisecond

	snap := &mitev1.StateSnapshot{JobSchedules: oneRowSet()}
	h.OnSnapshot("ctrl1", "ns1", snap)
	waitFor(t, func() bool { return pub.syncCalls == 1 }, 5*time.Second, "no publish attempt was made")

	// Several ticks inside the negative-cache window make no further
	// attempt.
	for i := 0; i < 5; i++ {
		h.OnSnapshot("ctrl1", "ns1", &mitev1.StateSnapshot{})
	}
	if got := pub.syncCalls; got != 1 {
		t.Fatalf("syncCalls = %d, want 1 while the negative cache is armed", got)
	}

	time.Sleep(60 * time.Millisecond)
	h.OnSnapshot("ctrl1", "ns1", &mitev1.StateSnapshot{})
	waitFor(t, func() bool { return pub.syncCalls == 2 }, 5*time.Second, "no retry after the negative cache expired")
}

var errStreamMissingForTest = errStreamMissing{}

type errStreamMissing struct{}

func (errStreamMissing) Error() string { return "stream not found" }

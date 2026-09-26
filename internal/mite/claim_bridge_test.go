package mite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

// fakeLedger implements ledgerRequester. reply, if set, is returned as the
// request-reply body; err, if set, is returned instead of a reply. Every
// call is recorded so a test can assert the request body's fields.
type fakeLedger struct {
	mu       sync.Mutex
	reply    *ledgerClaimResponse
	replyRaw []byte // overrides reply when set, to test an unparsable body
	err      error
	requests []ledgerClaimRequest
}

func (f *fakeLedger) RequestWithContext(_ context.Context, _ string, data []byte, _ time.Duration) (*nats.Msg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req ledgerClaimRequest
	_ = json.Unmarshal(data, &req)
	f.requests = append(f.requests, req)

	if f.err != nil {
		return nil, f.err
	}
	if f.replyRaw != nil {
		return &nats.Msg{Data: f.replyRaw}, nil
	}
	body, _ := json.Marshal(f.reply)
	return &nats.Msg{Data: body}, nil
}

func (f *fakeLedger) lastRequest() ledgerClaimRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// requestCount reports how many requests have been recorded so far,
// under the same lock RequestWithContext writes behind — safe to poll
// from a waitFor condition, unlike reading len(f.requests) directly.
func (f *fakeLedger) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newClaimTestHandler(ledger ledgerRequester) *BusHandler {
	return &BusHandler{
		cluster:        "cluster1",
		ledger:         ledger,
		cellInstances:  make(map[string]string),
		buildWorkers:   make(map[string]*registeredBuildWorker),
		claimCounts:    make(map[string]int),
		tenantResolver: defaultCellTenantResolver{},
	}
}

// --- 3.6: granted path, copied fields, timeout ---

func TestOnClaimRequest_GrantedPathCopiesReply(t *testing.T) {
	ledger := &fakeLedger{reply: &ledgerClaimResponse{
		Decision:    "granted",
		RunID:       "run-1",
		BuildNumber: 42,
	}}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	req := &mitev1.ClaimRequest{
		RequestId:              "req-1",
		IntentKey:              "scm:team-a/build:abc",
		RunId:                  "run-1",
		JobPath:                "team-a/build",
		CellInstance:           "pod-uid-1",
		JenkinsNextBuildNumber: 41,
	}
	h.OnClaimRequest("ctrl1", "ns1", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no claim response sent")
	resp := sender.first().GetClaimResponse()
	if resp == nil {
		t.Fatal("sent message is not a ClaimResponse")
	}
	if resp.RequestId != "req-1" || resp.Decision != "granted" || resp.BuildNumber != 42 {
		t.Fatalf("resp = %+v, want request_id echoed and the reply copied", resp)
	}

	lastReq := ledger.lastRequest()
	if lastReq.CellInstance != "pod-uid-1" {
		t.Fatalf("ledger request cell_instance = %q, want pod-uid-1", lastReq.CellInstance)
	}
	if lastReq.JenkinsNextBuildNumber != 41 {
		t.Fatalf("ledger request jenkins_next_build_number = %d, want 41", lastReq.JenkinsNextBuildNumber)
	}
	if lastReq.Cluster != "cluster1" || lastReq.Namespace != "ns1" || lastReq.Cell != "ctrl1" {
		t.Fatalf("ledger request identity = %+v, want stream identity, not the payload", lastReq)
	}
}

func TestOnClaimRequest_TransportErrorAnswersUnavailable(t *testing.T) {
	ledger := &fakeLedger{err: errors.New("no responders")}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	req := &mitev1.ClaimRequest{RequestId: "req-2", RunId: "run-2"}
	h.OnClaimRequest("ctrl1", "ns1", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no claim response sent")
	resp := sender.first().GetClaimResponse()
	if resp == nil || resp.Code != codeUnavailable || resp.Decision != "" {
		t.Fatalf("resp = %+v, want empty decision and code UNAVAILABLE", resp)
	}
	if resp.RequestId != "req-2" {
		t.Fatalf("request_id = %q, want echoed req-2", resp.RequestId)
	}
}

// lockedBuffer serializes writes from the claim goroutine and the test's read.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestOnClaimRequest_UnavailableAnswerLogsAWarning(t *testing.T) {
	ledger := &fakeLedger{err: errors.New("nats: no responders available for request")}
	h := newClaimTestHandler(ledger)
	var out lockedBuffer
	h.Logger = slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sender := &fakeSender{}

	req := &mitev1.ClaimRequest{RequestId: "req-warn", RunId: "run-warn", JobPath: "team-a/build"}
	h.OnClaimRequest("ctrl1", "ns1", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no claim response sent")
	got := out.String()
	for _, want := range []string{"level=WARN", "request_id=req-warn", "controller=ns1/ctrl1", "no responders"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log output = %q, want it to contain %q", got, want)
		}
	}
}

func TestOnClaimRequest_UnparsableReplyAnswersUnavailable(t *testing.T) {
	ledger := &fakeLedger{replyRaw: []byte("not json")}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	req := &mitev1.ClaimRequest{RequestId: "req-3", RunId: "run-3"}
	h.OnClaimRequest("ctrl1", "ns1", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no claim response sent")
	resp := sender.first().GetClaimResponse()
	if resp == nil || resp.Code != codeUnavailable {
		t.Fatalf("resp = %+v, want code UNAVAILABLE for an unparsable reply", resp)
	}
}

// --- 3.6: the 32 in-flight cap ---

func TestOnClaimRequest_CapRefusesWithNoLedgerCall(t *testing.T) {
	block := make(chan struct{})
	ledger := &blockingLedger{block: block}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	for i := 0; i < maxInFlightClaims; i++ {
		h.OnClaimRequest("ctrl1", "ns1", 1, &mitev1.ClaimRequest{RequestId: "in-flight", RunId: "r"}, sender.send)
	}
	waitFor(t, func() bool { return ledger.callCount() == maxInFlightClaims }, time.Second, "in-flight claims never reached the ledger")

	over := &fakeSender{}
	h.OnClaimRequest("ctrl1", "ns1", 1, &mitev1.ClaimRequest{RequestId: "over-cap", RunId: "r"}, over.send)

	waitFor(t, func() bool { return over.count() == 1 }, time.Second, "no response for the over-cap claim")
	resp := over.first().GetClaimResponse()
	if resp == nil || resp.Code != codeUnavailable || resp.Decision != "" {
		t.Fatalf("resp = %+v, want empty decision and code UNAVAILABLE", resp)
	}
	if ledger.callCount() != maxInFlightClaims {
		t.Fatalf("ledger calls = %d, want exactly %d (the over-cap request must not call NATS)", ledger.callCount(), maxInFlightClaims)
	}

	close(block)
}

// blockingLedger never returns until block is closed, so a test can pin
// maxInFlightClaims goroutines in flight.
type blockingLedger struct {
	mu    sync.Mutex
	calls int
	block <-chan struct{}
}

func (b *blockingLedger) RequestWithContext(ctx context.Context, _ string, _ []byte, _ time.Duration) (*nats.Msg, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	select {
	case <-b.block:
	case <-ctx.Done():
	}
	return nil, errors.New("unblocked")
}

func (b *blockingLedger) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// --- 3.7: the cap releases per connection epoch ---

func TestClaimSlots_ReleaseIsPerEpoch(t *testing.T) {
	h := newClaimTestHandler(&fakeLedger{reply: &ledgerClaimResponse{Decision: "granted"}})

	for i := 0; i < 3; i++ {
		h.tryReserveClaimSlot("ns1", "ctrl1", 1)
	}
	if got := h.claimCounts[claimKey("ns1", "ctrl1", 1)]; got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}

	// The stream ends: OnDisconnect releases every pending claim of this
	// epoch.
	h.clearClaimSlots("ns1", "ctrl1", 1)
	if _, ok := h.claimCounts[claimKey("ns1", "ctrl1", 1)]; ok {
		t.Fatal("epoch 1's counter should be gone after clearClaimSlots")
	}

	// A new stream (epoch 2) starts with a fresh counter and can take a
	// full 32.
	for i := 0; i < maxInFlightClaims; i++ {
		if !h.tryReserveClaimSlot("ns1", "ctrl1", 2) {
			t.Fatalf("reservation %d on the new epoch should succeed", i)
		}
	}

	// A goroutine from the old epoch finishing late must not touch the new
	// epoch's counter, and can never push it negative.
	h.releaseClaimSlot("ns1", "ctrl1", 1)
	if got := h.claimCounts[claimKey("ns1", "ctrl1", 2)]; got != maxInFlightClaims {
		t.Fatalf("epoch 2 count = %d after a stale epoch-1 release, want unchanged %d", got, maxInFlightClaims)
	}
	if _, ok := h.claimCounts[claimKey("ns1", "ctrl1", 1)]; ok {
		t.Fatal("a release for an already-cleared epoch must not recreate its counter")
	}
}

// --- 11.4: the claim bridge resolves per job path, and fails closed on an
// unhosted tenant ---

func TestOnClaimRequest_ResolvesTenantFromJobPath(t *testing.T) {
	ledger := &fakeLedger{reply: &ledgerClaimResponse{Decision: "granted"}}
	h := newClaimTestHandler(ledger)
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"payments/build": {tenant: "payments", generation: 4},
	}}
	sender := &fakeSender{}

	req := &mitev1.ClaimRequest{RequestId: "req-1", RunId: "run-1", JobPath: "payments/build"}
	h.OnClaimRequest("cell1", "ns1", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no claim response sent")
	lastReq := ledger.lastRequest()
	if lastReq.Tenant != "payments" || lastReq.PlacementGeneration != 4 {
		t.Fatalf("ledger request tenant/generation = %q/%d, want payments/4", lastReq.Tenant, lastReq.PlacementGeneration)
	}
}

func TestOnClaimRequest_UnhostedTenantAnswersUnavailableWithNoLedgerCall(t *testing.T) {
	ledger := &fakeLedger{reply: &ledgerClaimResponse{Decision: "granted"}}
	h := newClaimTestHandler(ledger)
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"payments/build": {tenant: "payments", generation: 4},
	}}
	sender := &fakeSender{}

	req := &mitev1.ClaimRequest{RequestId: "req-2", RunId: "run-2", JobPath: "search/build"}
	h.OnClaimRequest("cell1", "ns1", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no claim response sent")
	resp := sender.first().GetClaimResponse()
	if resp == nil || resp.Code != codeUnavailable || resp.Decision != "" {
		t.Fatalf("resp = %+v, want empty decision and code UNAVAILABLE", resp)
	}
	if len(ledger.requests) != 0 {
		t.Fatalf("ledger requests = %d, want 0: an unhosted tenant must never reach the ledger", len(ledger.requests))
	}
}

// --- 11.3: the default single-tenant resolver's ResolveTenant still
// answers a controller with no cell role ---

func TestOnClaimRequest_NonCellControllerKeepsCompoundTenant(t *testing.T) {
	ledger := &fakeLedger{reply: &ledgerClaimResponse{Decision: "granted"}}
	h := newClaimTestHandler(ledger)

	sender := &fakeSender{}
	req := &mitev1.ClaimRequest{RequestId: "req-3", RunId: "run-3", JobPath: "some/job"}
	h.OnClaimRequest("ctrl1", "ns1", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no claim response sent")
	lastReq := ledger.lastRequest()
	if lastReq.Tenant != "ns1/ctrl1" || lastReq.PlacementGeneration != 0 {
		t.Fatalf("ledger request tenant/generation = %q/%d, want ns1/ctrl1/0", lastReq.Tenant, lastReq.PlacementGeneration)
	}
}

// TestOnClaimRequest_MoveIsServedStaleWithinTTLThenRefreshedAfterExpiry
// wires the real, cache-backed CellTenantResolver (not a stub) into the
// claim bridge and proves D14's rule for a move: the claim bridge simply
// forwards whatever the resolver's cache currently holds. Within the
// cache's TTL that is the tenant's old generation — the recorder's ledger
// claim fence (internal/recordstore/ledger.go's existingKeyDecision, which
// vetoes a claim whose placement_generation is lower than the stored one)
// is what rejects a stale generation, not the claim bridge. After the
// cache entry expires, the claim bridge forwards the refreshed value.
func TestOnClaimRequest_MoveIsServedStaleWithinTTLThenRefreshedAfterExpiry(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns1"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
	})
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: "cell1", CellNamespace: "ns1", Generation: 1},
		},
	})
	resolver := NewCellTenantResolver(store)

	ledger := &fakeLedger{reply: &ledgerClaimResponse{Decision: "granted"}}
	h := newClaimTestHandler(ledger)
	h.tenantResolver = resolver
	sender := &fakeSender{}

	req1 := &mitev1.ClaimRequest{RequestId: "req-1", RunId: "run-1", JobPath: "payments/build"}
	h.OnClaimRequest("cell1", "ns1", 1, req1, sender.send)
	waitFor(t, func() bool { return ledger.requestCount() == 1 }, time.Second, "no ledger request sent")
	if got := ledger.lastRequest(); got.Tenant != "payments" || got.PlacementGeneration != 1 {
		t.Fatalf("first claim tenant/generation = %q/%d, want payments/1", got.Tenant, got.PlacementGeneration)
	}

	// The tenant moves to generation 2 before the cache entry expires.
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cell: "cell1", CellNamespace: "ns1", Generation: 2},
		},
	})

	req2 := &mitev1.ClaimRequest{RequestId: "req-2", RunId: "run-2", JobPath: "payments/build"}
	h.OnClaimRequest("cell1", "ns1", 1, req2, sender.send)
	waitFor(t, func() bool { return ledger.requestCount() == 2 }, time.Second, "no second ledger request sent")
	if got := ledger.lastRequest(); got.Tenant != "payments" || got.PlacementGeneration != 1 {
		t.Fatalf("claim within the TTL = %q/%d, want the still-cached stale payments/1", got.Tenant, got.PlacementGeneration)
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

	req3 := &mitev1.ClaimRequest{RequestId: "req-3", RunId: "run-3", JobPath: "payments/build"}
	h.OnClaimRequest("cell1", "ns1", 1, req3, sender.send)
	waitFor(t, func() bool { return ledger.requestCount() == 3 }, time.Second, "no third ledger request sent")
	if got := ledger.lastRequest(); got.Tenant != "payments" || got.PlacementGeneration != 2 {
		t.Fatalf("claim after TTL expiry = %q/%d, want the refreshed payments/2", got.Tenant, got.PlacementGeneration)
	}
}

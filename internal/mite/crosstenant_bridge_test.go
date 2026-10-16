package mite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// fakeCrossTenantLedger implements ledgerRequester for the cross-tenant
// bridge. Every call is recorded so a test can assert the forwarded
// body's fields; replyRaw, when set, is returned verbatim so a test can
// assert the gateway copies a reply through without reparsing it.
type fakeCrossTenantLedger struct {
	replyRaw []byte
	err      error
	requests []map[string]any
}

func (f *fakeCrossTenantLedger) RequestWithContext(_ context.Context, _ string, data []byte, _ time.Duration) (*nats.Msg, error) {
	var req map[string]any
	_ = json.Unmarshal(data, &req)
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	return &nats.Msg{Data: f.replyRaw}, nil
}

func (f *fakeCrossTenantLedger) lastRequest() map[string]any {
	return f.requests[len(f.requests)-1]
}

func crossTenantRequestID(v uint64) *uint64 {
	return &v
}

// --- 5.1: the gateway overwrites the identity block ---

func TestOnCrossTenantRequest_OverwritesIdentityFields(t *testing.T) {
	ledger := &fakeCrossTenantLedger{replyRaw: []byte(`{"kind":"build","run_id":"run-1","state":"pending"}`)}
	h := newClaimTestHandler(ledger)
	h.tenantResolver = stubJobTenantResolver{byJobPath: map[string]stubTenantEntry{
		"team-a/build": {tenant: "team-a", generation: 7},
	}}
	sender := &fakeSender{}

	payload, _ := json.Marshal(map[string]any{
		"caller_job_path": "team-a/build",
		// A cell may claim to be any of these; the gateway must
		// overwrite every one from the stream identity and the
		// resolver, never trust the payload.
		"cluster":              "attacker-cluster",
		"namespace":            "attacker-ns",
		"cell":                 "attacker-cell",
		"caller_tenant":        "attacker-tenant",
		"placement_generation": 999,
		"target_tenant":        "team-b",
		"job_path":             "team-b/deploy",
		// The gateway must overwrite this too, so a caller cannot
		// backdate or forge its own request time.
		"requested_at": "2020-01-01T00:00:00Z",
	})
	req := &mitev1.CrossTenantRequest{
		RequestId:   crossTenantRequestID(1),
		Kind:        "build",
		PayloadJson: string(payload),
	}
	h.OnCrossTenantRequest("ci", "team-a", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no cross-tenant response sent")

	sent := ledger.lastRequest()
	if sent["cluster"] != "cluster1" || sent["namespace"] != "team-a" || sent["cell"] != "ci" {
		t.Fatalf("forwarded identity = %+v, want the stream identity, not the payload", sent)
	}
	if sent["caller_tenant"] != "team-a" {
		t.Fatalf("forwarded caller_tenant = %v, want the resolved tenant team-a", sent["caller_tenant"])
	}
	if gen, ok := sent["placement_generation"].(float64); !ok || gen != 7 {
		t.Fatalf("forwarded placement_generation = %v, want the resolved generation 7", sent["placement_generation"])
	}
	// A field the gateway does not overwrite passes through unchanged.
	if sent["target_tenant"] != "team-b" || sent["job_path"] != "team-b/deploy" {
		t.Fatalf("forwarded request fields = %+v, want the caller's own build fields preserved", sent)
	}
	requestedAt, ok := sent["requested_at"].(string)
	if !ok || requestedAt == "2020-01-01T00:00:00Z" {
		t.Fatalf("forwarded requested_at = %v, want the gateway's own clock, not the caller's value", sent["requested_at"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, requestedAt)
	if err != nil {
		t.Fatalf("forwarded requested_at = %q is not RFC 3339: %v", requestedAt, err)
	}
	if since := time.Since(parsed); since < 0 || since > time.Minute {
		t.Fatalf("forwarded requested_at = %q is not close to now", requestedAt)
	}

	resp := sender.first().GetCrossTenantResponse()
	if resp == nil {
		t.Fatal("sent message is not a CrossTenantResponse")
	}
	if resp.RequestId == nil || *resp.RequestId != 1 {
		t.Fatalf("request_id = %v, want echoed 1", resp.RequestId)
	}
	if resp.PayloadJson != string(ledger.replyRaw) {
		t.Fatalf("payload_json = %q, want the reply copied through verbatim", resp.PayloadJson)
	}
}

// --- 5.2: an unhosted caller job path fails closed ---

func TestOnCrossTenantRequest_UnhostedCallerAnswersUnavailableWithNoLedgerCall(t *testing.T) {
	ledger := &fakeCrossTenantLedger{}
	h := newClaimTestHandler(ledger)
	h.tenantResolver = stubJobTenantResolver{} // resolves nothing
	sender := &fakeSender{}

	payload, _ := json.Marshal(map[string]any{"caller_job_path": "unplaced/build"})
	req := &mitev1.CrossTenantRequest{RequestId: crossTenantRequestID(2), Kind: "status", PayloadJson: string(payload)}
	h.OnCrossTenantRequest("ci", "team-a", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no cross-tenant response sent")
	if len(ledger.requests) != 0 {
		t.Fatalf("ledger requests = %d, want 0 (an unhosted caller must never reach the ledger)", len(ledger.requests))
	}

	resp := sender.first().GetCrossTenantResponse()
	var errBody crossTenantErrorPayload
	if err := json.Unmarshal([]byte(resp.PayloadJson), &errBody); err != nil {
		t.Fatalf("payload_json unparsable: %v", err)
	}
	if errBody.Code != codeUnavailable {
		t.Fatalf("code = %q, want %q", errBody.Code, codeUnavailable)
	}
}

// --- unknown kind is rejected before the ledger is ever called ---

func TestOnCrossTenantRequest_UnknownKindAnswersErrorWithNoLedgerCall(t *testing.T) {
	ledger := &fakeCrossTenantLedger{}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	req := &mitev1.CrossTenantRequest{RequestId: crossTenantRequestID(3), Kind: "delete_everything", PayloadJson: "{}"}
	h.OnCrossTenantRequest("ci", "team-a", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no cross-tenant response sent")
	if len(ledger.requests) != 0 {
		t.Fatalf("ledger requests = %d, want 0 (an unknown kind must never reach the ledger)", len(ledger.requests))
	}
	resp := sender.first().GetCrossTenantResponse()
	var errBody crossTenantErrorPayload
	if err := json.Unmarshal([]byte(resp.PayloadJson), &errBody); err != nil {
		t.Fatalf("payload_json unparsable: %v", err)
	}
	if errBody.Code != codeInternal {
		t.Fatalf("code = %q, want %q", errBody.Code, codeInternal)
	}
}

// --- transport and decode failures answer UNAVAILABLE/INTERNAL, never a panic ---

func TestOnCrossTenantRequest_TransportErrorAnswersUnavailable(t *testing.T) {
	ledger := &fakeCrossTenantLedger{err: errors.New("no responders")}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	payload, _ := json.Marshal(map[string]any{"caller_job_path": "team-a/build"})
	req := &mitev1.CrossTenantRequest{RequestId: crossTenantRequestID(4), Kind: "status", PayloadJson: string(payload)}
	h.OnCrossTenantRequest("ci", "team-a", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no cross-tenant response sent")
	resp := sender.first().GetCrossTenantResponse()
	var errBody crossTenantErrorPayload
	_ = json.Unmarshal([]byte(resp.PayloadJson), &errBody)
	if errBody.Code != codeUnavailable {
		t.Fatalf("code = %q, want %q", errBody.Code, codeUnavailable)
	}
}

func TestOnCrossTenantRequest_MalformedPayloadAnswersInternal(t *testing.T) {
	ledger := &fakeCrossTenantLedger{}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	req := &mitev1.CrossTenantRequest{RequestId: crossTenantRequestID(5), Kind: "status", PayloadJson: "not json"}
	h.OnCrossTenantRequest("ci", "team-a", 1, req, sender.send)

	waitFor(t, func() bool { return sender.count() == 1 }, time.Second, "no cross-tenant response sent")
	if len(ledger.requests) != 0 {
		t.Fatalf("ledger requests = %d, want 0 (a malformed payload must never reach the ledger)", len(ledger.requests))
	}
	resp := sender.first().GetCrossTenantResponse()
	var errBody crossTenantErrorPayload
	_ = json.Unmarshal([]byte(resp.PayloadJson), &errBody)
	if errBody.Code != codeInternal {
		t.Fatalf("code = %q, want %q", errBody.Code, codeInternal)
	}
}

// --- 5.3: the 8 in-flight cap ---

// blockingCrossTenantLedger never returns until block is closed, so a
// test can pin crossTenantMaxInFlight goroutines in flight.
type blockingCrossTenantLedger struct {
	calls chan struct{}
	block <-chan struct{}
}

func (b *blockingCrossTenantLedger) RequestWithContext(ctx context.Context, _ string, _ []byte, _ time.Duration) (*nats.Msg, error) {
	b.calls <- struct{}{}
	select {
	case <-b.block:
	case <-ctx.Done():
	}
	return nil, errors.New("unblocked")
}

func TestOnCrossTenantRequest_CapRefusesWithNoLedgerCall(t *testing.T) {
	block := make(chan struct{})
	ledger := &blockingCrossTenantLedger{calls: make(chan struct{}, crossTenantMaxInFlight), block: block}
	h := newClaimTestHandler(ledger)
	sender := &fakeSender{}

	payload, _ := json.Marshal(map[string]any{"caller_job_path": "team-a/build"})
	for i := 0; i < crossTenantMaxInFlight; i++ {
		req := &mitev1.CrossTenantRequest{RequestId: crossTenantRequestID(uint64(i)), Kind: "status", PayloadJson: string(payload)}
		h.OnCrossTenantRequest("ci", "team-a", 1, req, sender.send)
	}
	for i := 0; i < crossTenantMaxInFlight; i++ {
		select {
		case <-ledger.calls:
		case <-time.After(time.Second):
			t.Fatalf("in-flight request %d never reached the ledger", i)
		}
	}

	over := &fakeSender{}
	overReq := &mitev1.CrossTenantRequest{RequestId: crossTenantRequestID(999), Kind: "status", PayloadJson: string(payload)}
	h.OnCrossTenantRequest("ci", "team-a", 1, overReq, over.send)

	waitFor(t, func() bool { return over.count() == 1 }, time.Second, "no response for the over-cap request")
	resp := over.first().GetCrossTenantResponse()
	var errBody crossTenantErrorPayload
	_ = json.Unmarshal([]byte(resp.PayloadJson), &errBody)
	if errBody.Code != codeUnavailable {
		t.Fatalf("code = %q, want %q (the over-cap request must not call NATS)", errBody.Code, codeUnavailable)
	}

	close(block)
}

// --- 5.3: a disconnect restores the full cap for a new stream ---

func TestCrossTenantSlots_ReleaseIsPerEpoch(t *testing.T) {
	h := newClaimTestHandler(&fakeCrossTenantLedger{})

	for i := 0; i < 3; i++ {
		h.tryReserveCrossTenantSlot("ns1", "ctrl1", 1)
	}
	if got := h.crossTenantCounts[crossTenantKey("ns1", "ctrl1", 1)]; got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}

	// The stream ends: OnDisconnect releases every pending cross-tenant
	// request of this epoch.
	h.clearCrossTenantSlots("ns1", "ctrl1", 1)
	if _, ok := h.crossTenantCounts[crossTenantKey("ns1", "ctrl1", 1)]; ok {
		t.Fatal("epoch 1's counter should be gone after clearCrossTenantSlots")
	}

	// A new stream (epoch 2) starts with a fresh counter and can take a
	// full crossTenantMaxInFlight.
	for i := 0; i < crossTenantMaxInFlight; i++ {
		if !h.tryReserveCrossTenantSlot("ns1", "ctrl1", 2) {
			t.Fatalf("reservation %d on the new epoch should succeed", i)
		}
	}

	// A goroutine from the old epoch finishing late must not touch the new
	// epoch's counter, and can never push it negative.
	h.releaseCrossTenantSlot("ns1", "ctrl1", 1)
	if got := h.crossTenantCounts[crossTenantKey("ns1", "ctrl1", 2)]; got != crossTenantMaxInFlight {
		t.Fatalf("epoch 2 count = %d after a stale epoch-1 release, want unchanged %d", got, crossTenantMaxInFlight)
	}
	if _, ok := h.crossTenantCounts[crossTenantKey("ns1", "ctrl1", 1)]; ok {
		t.Fatal("a release for an already-cleared epoch must not recreate its counter")
	}
}

// --- 5.4: the gateway's own budget (VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS) ---

func TestCrossTenantTimeoutFromEnv_DefaultWhenAbsent(t *testing.T) {
	t.Setenv(crossTenantGatewayTimeoutEnv, "")
	got := CrossTenantTimeoutFromEnv(crossTenantGatewayTimeoutEnv, defaultCrossTenantGatewayTimeout)
	if got != defaultCrossTenantGatewayTimeout {
		t.Fatalf("timeout = %v, want the default %v", got, defaultCrossTenantGatewayTimeout)
	}
}

func TestCrossTenantTimeoutFromEnv_OverrideChangesBudget(t *testing.T) {
	t.Setenv(crossTenantGatewayTimeoutEnv, "5000")
	got := CrossTenantTimeoutFromEnv(crossTenantGatewayTimeoutEnv, defaultCrossTenantGatewayTimeout)
	if got != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s", got)
	}
}

func TestCrossTenantTimeoutFromEnv_NonWholeNumberKeepsDefault(t *testing.T) {
	for _, v := range []string{"9000.5", "abc", "-1", "0"} {
		t.Setenv(crossTenantGatewayTimeoutEnv, v)
		got := CrossTenantTimeoutFromEnv(crossTenantGatewayTimeoutEnv, defaultCrossTenantGatewayTimeout)
		if got != defaultCrossTenantGatewayTimeout {
			t.Fatalf("value %q: timeout = %v, want the default %v", v, got, defaultCrossTenantGatewayTimeout)
		}
	}
}

func TestCrossTenantGatewayTimeout_WarnsWhenAtLeastMiteHopDefault(t *testing.T) {
	t.Setenv(crossTenantGatewayTimeoutEnv, "9000")
	var buf bytes.Buffer
	h := newClaimTestHandler(&fakeCrossTenantLedger{})
	h.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := h.crossTenantGatewayTimeout()
	if got != 9000*time.Millisecond {
		t.Fatalf("timeout = %v, want 9000ms", got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("log output = %q, want a WARNING", out)
	}
	if !strings.Contains(out, "VARROA_CROSSTENANT_GATEWAY_TIMEOUT_MS") {
		t.Fatalf("log output = %q, want it to name the misconfigured variable", out)
	}
}

func TestCrossTenantGatewayTimeout_NoWarningWhenShorterThanMiteHop(t *testing.T) {
	t.Setenv(crossTenantGatewayTimeoutEnv, "8500")
	var buf bytes.Buffer
	h := newClaimTestHandler(&fakeCrossTenantLedger{})
	h.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	h.crossTenantGatewayTimeout()
	if out := buf.String(); strings.Contains(out, "level=WARN") {
		t.Fatalf("log output = %q, want no WARNING for a budget shorter than the mite hop", out)
	}
}

package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// startFleetRunStream runs the fleet stream handler on its own goroutine and
// answers the recorder and a stop function that ends the stream.
func startFleetRunStream(t *testing.T, srv *Server) (*sseRecorder, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(contextWithClaims(context.Background(), resolverClaims))
	req := httptest.NewRequest(http.MethodGet, "/pipelines/runs/stream", nil).WithContext(ctx)
	rec := newSSERecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleFleetRunStream(rec, req)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fleet run stream did not stop after cancel")
		}
	}
	return rec, stop
}

func TestFleetRunStreamSubscribesEveryReadableKey(t *testing.T) {
	stub := &runFanoutStub{}
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay", "ops"), &fakeRecordReader{},
		pipelinesTenant("pay", true, "core", "ns", "c1"),
		pipelinesTenant("ops", true, "core", "ns", "c2"))
	srv.deps.RunFanout = stub

	rec, stop := startFleetRunStream(t, srv)
	defer stop()
	stub.waitForSubscription(t)

	wantKeys := []string{"core/ops", "core/pay"}
	got := stub.keysOf()
	if len(got) != len(wantKeys) || got[0] != wantKeys[0] || got[1] != wantKeys[1] {
		t.Fatalf("subscription keys = %v; want %v", got, wantKeys)
	}
	if stub.deliver(notification("run", "pay", "")) {
		t.Error("a notification with an empty job path passed the filter")
	}
	if stub.deliver(notification("run", "secret", "secret/a")) {
		t.Error("a notification for an unreadable tenant passed the filter")
	}
	if !stub.deliver(notification("run", "pay", "pay/a")) {
		t.Fatal("a notification for a readable job was refused")
	}
	rec.waitForFrame(t, `"tenant":"pay"`)
}

func TestFleetRunStreamNoTenant404(t *testing.T) {
	srv := fleetServer(t, pipelineGrantAuthorizer([]string{"read"}, "pay"), &fakeRecordReader{})
	srv.deps.RunFanout = &runFanoutStub{}
	req := httptest.NewRequest(http.MethodGet, "/pipelines/runs/stream", nil)
	req = req.WithContext(contextWithClaims(req.Context(), resolverClaims))
	rec := httptest.NewRecorder()
	srv.handleFleetRunStream(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
}

func TestStreamTicketPipelinesRunsScope(t *testing.T) {
	mint := func(store recordstore.Reader) int {
		srv := NewServer(&Dependencies{
			Authorizer:   NewAuthorizer(testResolver(nil, nil), false),
			TicketIssuer: newTicketIssuerForTest(t),
			RecordStore:  store,
			Logger:       slog.Default(),
		})
		req := httptest.NewRequest(http.MethodPost, "/stream/ticket", strings.NewReader(`{"scope":"pipelines-runs"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(authContext(resolverClaims))
		w := httptest.NewRecorder()
		srv.handleStreamTicket(w, req)
		return w.Code
	}
	if code := mint(&fakeRecordReader{}); code != http.StatusOK {
		t.Errorf("with a record store: status = %d; want 200", code)
	}
	if code := mint(nil); code != http.StatusServiceUnavailable {
		t.Errorf("without a record store: status = %d; want 503", code)
	}
}

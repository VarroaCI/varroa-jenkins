package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/pkg/client/stream"
)

// overrideSignalCtx replaces the package-level signalCtx with one that does
// NOT install OS signal handlers (which leak in tests). Returns a restore func.
func overrideSignalCtx() func() {
	orig := signalCtx
	signalCtx = func(ctx context.Context) (context.Context, context.CancelFunc) {
		// No signal.NotifyContext — use parent as-is.
		return context.WithCancel(ctx)
	}
	return func() { signalCtx = orig }
}

// ---------------------------------------------------------------------------
// events tests
// ---------------------------------------------------------------------------

func TestEvents_TableOutput(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/events") {
			t.Errorf("expected /events in path, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"event\":\"connected\",\"name\":\"ctrl-1\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"events", "controller", "team-a/ctrl-1", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEvents_JSONOutput(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"event\":\"connected\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"events", "controller", "team-a/ctrl-1", "--server", srv.URL, "-o", "json"})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// mite tests
// ---------------------------------------------------------------------------

func TestMite_TableOutput(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/mite/stream") {
			t.Errorf("expected /mite/stream in path, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "event: init\ndata: {\"name\":\"ctrl-1\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"mite", "team-a/ctrl-1", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMite_JSONOutput(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "event: init\ndata: {\"name\":\"ctrl-1\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"mite", "team-a/ctrl-1", "--server", srv.URL, "-o", "json"})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// logs -f tests
// ---------------------------------------------------------------------------

func TestLogsFollow_DedupeAfterReconnect(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/logs") || !strings.Contains(r.URL.RawQuery, "follow=true") {
			t.Errorf("expected /logs?follow=true, got %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		count := requestCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		if count == 1 {
			fmt.Fprintf(w, "data: {\"timestamp\":\"2024-01-15T10:00:01Z\",\"level\":\"INFO\",\"source\":\"mite\",\"message\":\"first\"}\n\n")
			fmt.Fprintf(w, "data: {\"timestamp\":\"2024-01-15T10:00:02Z\",\"level\":\"INFO\",\"source\":\"mite\",\"message\":\"second\"}\n\n")
			flusher.Flush()
			// Return closes connection — client reconnects
		} else {
			fmt.Fprintf(w, "data: {\"timestamp\":\"2024-01-15T10:00:02Z\",\"level\":\"INFO\",\"source\":\"mite\",\"message\":\"second\"}\n\n")
			fmt.Fprintf(w, "data: {\"timestamp\":\"2024-01-15T10:00:03Z\",\"level\":\"INFO\",\"source\":\"mite\",\"message\":\"third\"}\n\n")
			flusher.Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"logs", "team-a/ctrl-1", "--server", srv.URL, "-f"})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if requestCount.Load() < 2 {
		t.Errorf("expected at least 2 requests (reconnect), got %d", requestCount.Load())
	}
}

func TestLogsFollow_CtxCancelExits0(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"logs", "team-a/ctrl-1", "--server", srv.URL, "-f"})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("expected nil on cancel, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// stream.HTTPError tests
// ---------------------------------------------------------------------------

func TestEvents_TerminalError(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root.SetArgs([]string{"events", "controller", "team-a/ctrl-1", "--server", srv.URL})
	root.SetContext(ctx)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from 401, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("expected 'unauthorized' in error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ParseSSE integration
// ---------------------------------------------------------------------------

func TestParseSSE_EventStream(t *testing.T) {
	input := "event: test\ndata: {\"msg\":\"hello\"}\n\n"
	var events []stream.Event
	err := stream.ParseSSE(strings.NewReader(input), func(e stream.Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Name != "test" {
		t.Errorf("expected name 'test', got %q", events[0].Name)
	}
	var data map[string]any
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatalf("failed to decode data: %v", err)
	}
	if data["msg"] != "hello" {
		t.Errorf("expected msg 'hello', got %v", data["msg"])
	}
}

// ---------------------------------------------------------------------------
// events controller --cluster dev-cluster
// ---------------------------------------------------------------------------

func TestEvents_DevCluster(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/clusters/dev-cluster/controllers/team-a/ctrl-1/events") {
			t.Errorf("expected /clusters/dev-cluster/.../events, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"event\":\"connected\",\"name\":\"ctrl-1\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"events", "controller", "team-a/ctrl-1", "--cluster", "dev-cluster", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

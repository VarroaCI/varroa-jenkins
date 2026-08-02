package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// activity tests
// ---------------------------------------------------------------------------

func TestActivity_BackfillTable(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/activity") || strings.Contains(r.URL.Path, "/stream") {
			t.Errorf("expected /activity backfill, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"items": [
				{"timestamp":"2024-01-15T10:00:00Z","type":"Created","source":"operator","controller":"ctrl-1","namespace":"team-a","message":"controller created"}
			]
		}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	root.SetArgs([]string{"activity", "--server", srv.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestActivity_FollowStream(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/activity/stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "event: activity\ndata: {\"timestamp\":\"2024-01-15T10:00:01Z\",\"type\":\"Updated\",\"source\":\"operator\",\"controller\":\"ctrl-1\",\"namespace\":\"team-a\",\"message\":\"controller updated\"}\n\n")
			flusher.Flush()
			<-r.Context().Done()
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"items":[]}`)
		}
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

	root.SetArgs([]string{"activity", "--server", srv.URL, "-f"})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestActivity_ControllerFilter(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "controller=ctrl-1") {
			t.Errorf("expected controller=ctrl-1 in query, got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	root.SetArgs([]string{"activity", "--server", srv.URL, "--controller", "ctrl-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestActivity_ReconnectRefetch(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		if strings.Contains(r.URL.Path, "/activity/stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			if n <= 2 {
				flusher.Flush()
			} else {
				fmt.Fprintf(w, "event: activity\ndata: {\"timestamp\":\"2024-01-15T10:00:03Z\",\"type\":\"Updated\",\"source\":\"operator\",\"message\":\"new event\"}\n\n")
				flusher.Flush()
				<-r.Context().Done()
			}
		} else if strings.Contains(r.URL.Path, "/activity") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if n >= 3 {
				fmt.Fprintf(w, `{"items":[{"timestamp":"2024-01-15T10:00:02Z","type":"Created","source":"operator","message":"refetched"}]}`)
			} else {
				fmt.Fprintf(w, `{"items":[]}`)
			}
		}
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"activity", "--server", srv.URL, "-f"})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestActivity_NoFollowExitsCleanly(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	root.SetArgs([]string{"activity", "--server", srv.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

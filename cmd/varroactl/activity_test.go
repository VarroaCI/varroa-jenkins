package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/pkg/client/stream"
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
		if r.URL.Query().Has("cluster") {
			t.Errorf("expected no cluster parameter without --cluster, got %s", r.URL.RawQuery)
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

func TestActivity_ClusterFilterOnBackfillAndStream(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	var mu sync.Mutex
	seen := map[string]url.Values{}
	streamed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.URL.Query()
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/activity/stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			select {
			case streamed <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-streamed:
		case <-time.After(5 * time.Second):
		}
		cancel()
	}()

	root.SetArgs([]string{"activity", "--server", srv.URL, "-f", "--cluster", "member-b", "--controller", "ci"})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/api/v1/activity", "/api/v1/activity/stream"} {
		q, ok := seen[path]
		if !ok {
			t.Fatalf("no request reached %s", path)
		}
		if got := q.Get("cluster"); got != "member-b" {
			t.Errorf("%s: cluster = %q, want member-b", path, got)
		}
		if got := q.Get("controller"); got != "ci" {
			t.Errorf("%s: controller = %q, want ci", path, got)
		}
	}
}

func TestActivity_BackfillClusterOnly(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cluster"); got != "member-b" {
			t.Errorf("cluster = %q, want member-b (query %s)", got, r.URL.RawQuery)
		}
		if r.URL.Query().Has("controller") {
			t.Errorf("expected no controller parameter, got %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	root.SetArgs([]string{"activity", "--server", srv.URL, "--cluster", "member-b"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrintActivityStreamEvent_PrintsWhatTheServerSends(t *testing.T) {
	var lastPrinted atomic.Value
	data := []byte(`{"timestamp":"2024-01-15T10:00:01Z","type":"Updated","source":"operator","cluster":"member-b","controller":"ci","namespace":"team-a","message":"controller updated"}`)
	if err := printActivityStreamEvent(stream.Event{Name: "activity", Data: data}, &lastPrinted); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lastPrinted.Load() == nil {
		t.Fatal("server-filtered event was dropped by the client")
	}
}

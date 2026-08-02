package stream

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
// ParseSSE table-driven tests
// ---------------------------------------------------------------------------

func TestParseSSE_NamedEvent(t *testing.T) {
	input := "event: activity\ndata: {\"msg\":\"hello\"}\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Name != "activity" {
		t.Errorf("expected name 'activity', got %q", events[0].Name)
	}
	if string(events[0].Data) != `{"msg":"hello"}` {
		t.Errorf("expected data %q, got %q", `{"msg":"hello"}`, string(events[0].Data))
	}
}

func TestParseSSE_UnnamedEvent(t *testing.T) {
	input := "data: hello\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Name != "" {
		t.Errorf("expected empty name for unnamed event, got %q", events[0].Name)
	}
	if string(events[0].Data) != "hello" {
		t.Errorf("expected data 'hello', got %q", string(events[0].Data))
	}
}

func TestParseSSE_MultiLineData(t *testing.T) {
	input := "data: line1\ndata: line2\ndata: line3\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if string(events[0].Data) != "line1\nline2\nline3" {
		t.Errorf("expected joined data, got %q", string(events[0].Data))
	}
}

func TestParseSSE_CommentSkipped(t *testing.T) {
	input := ": keepalive\n: ping\ndata: payload\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if string(events[0].Data) != "payload" {
		t.Errorf("expected 'payload', got %q", string(events[0].Data))
	}
}

func TestParseSSE_IdRetryIgnored(t *testing.T) {
	input := "id: 42\nretry: 3000\ndata: hello\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if string(events[0].Data) != "hello" {
		t.Errorf("expected 'hello', got %q", string(events[0].Data))
	}
}

func TestParseSSE_EventNameResets(t *testing.T) {
	input := "event: first\ndata: msg1\n\nevent: second\ndata: msg2\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Name != "first" || string(events[0].Data) != "msg1" {
		t.Errorf("first event mismatch: %+v", events[0])
	}
	if events[1].Name != "second" || string(events[1].Data) != "msg2" {
		t.Errorf("second event mismatch: %+v", events[1])
	}
}

func TestParseSSE_DataLeadingSpacePreserved(t *testing.T) {
	// Per SSE spec, data:" hello" should strip only the first space -> " hello"
	// but data:"  hello" should strip only one space -> " hello"
	input := "data:  hello\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// TrimPrefix("data:", line) -> "  hello", then TrimPrefix(" ", "...") -> " hello"
	if string(events[0].Data) != " hello" {
		t.Errorf("expected ' hello' (single leading space preserved), got %q", string(events[0].Data))
	}
}

func TestParseSSE_HandlerErrorAborts(t *testing.T) {
	input := "data: first\n\ndata: second\n\n"
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		if string(e.Data) == "second" {
			return fmt.Errorf("abort")
		}
		return nil
	})
	if err == nil || err.Error() != "abort" {
		t.Fatalf("expected 'abort' error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// End-to-end httptest tests
// ---------------------------------------------------------------------------

func TestStream_Reconnect(t *testing.T) {
	// Server: connection 1 sends 2 frames and returns (closing conn).
	// Connection 2 (reconnect) sends "done" and holds open.
	var (
		requestCount atomic.Int32
		secondReqCh  = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		if count == 1 {
			fmt.Fprintf(w, "data: first\n\n")
			fmt.Fprintf(w, "data: second\n\n")
			flusher.Flush()
			// Return closes the connection naturally — client will see EOF
		} else {
			fmt.Fprintf(w, "data: done\n\n")
			flusher.Flush()
			close(secondReqCh)
			// Hold until context cancelled (test cancels when done received)
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var onConnectCalls []bool
	var mu int32
	var events []Event

	done := make(chan error, 1)
	go func() {
		done <- Stream(ctx, Config{
			URL:       srv.URL,
			Token:     "test",
			UserAgent: "test/1.0",
			Client:    srv.Client(),
			Backoff:   BackoffConfig{Initial: 5 * time.Millisecond, Max: 50 * time.Millisecond, Factor: 2},
			OnConnect: func(reconnected bool) {
				atomic.AddInt32(&mu, 1)
				onConnectCalls = append(onConnectCalls, reconnected)
			},
		}, func(e Event) error {
			events = append(events, e)
			if string(e.Data) == "done" {
				cancel()
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream to complete")
	}

	if len(events) < 3 {
		t.Fatalf("expected at least 3 events, got %d: %+v", len(events), events)
	}
	if atomic.LoadInt32(&mu) < 2 {
		t.Errorf("expected at least 2 OnConnect calls, got %d", atomic.LoadInt32(&mu))
	}
	// First call should be false (first connection), second should be true (reconnect)
	if len(onConnectCalls) >= 2 {
		if onConnectCalls[0] {
			t.Error("first OnConnect should be false (not a reconnect)")
		}
		if !onConnectCalls[1] {
			t.Error("second OnConnect should be true (reconnect)")
		}
	}
}

func TestStream_Terminal401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	ctx := context.Background()
	err := Stream(ctx, Config{
		URL:       srv.URL,
		Token:     "bad",
		UserAgent: "test/1.0",
		Client:    srv.Client(),
	}, func(e Event) error {
		return nil
	})

	if err == nil {
		t.Fatal("expected HTTPError, got nil")
	}
	var httpErr *HTTPError
	if !AsHTTPError(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != 401 {
		t.Errorf("expected status 401, got %d", httpErr.StatusCode)
	}
	if httpErr.Message != "unauthorized" {
		t.Errorf("expected message 'unauthorized', got %q", httpErr.Message)
	}
}

func TestStream_CtxCancelMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Hold connection open
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Stream(ctx, Config{
			URL:       srv.URL,
			Token:     "test",
			UserAgent: "test/1.0",
			Client:    srv.Client(),
		}, func(e Event) error {
			return nil
		})
	}()

	// Let connection establish
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on cancel, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream to cancel")
	}
}

func TestStream_HandlerErrorAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: boom\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Stream(ctx, Config{
		URL:       srv.URL,
		Token:     "test",
		UserAgent: "test/1.0",
		Client:    srv.Client(),
	}, func(e Event) error {
		return fmt.Errorf("handler error")
	})

	if err == nil || err.Error() != "handler error" {
		t.Fatalf("expected 'handler error', got %v", err)
	}
}

// TestParseSSE_EmptyInput tests that an empty stream produces no events.
func TestParseSSE_EmptyInput(t *testing.T) {
	var events []Event
	err := ParseSSE(strings.NewReader(""), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events from empty input, got %d", len(events))
	}
}

// TestParseSSE_MultipleBlankLines tests that multiple blank lines don't create events.
func TestParseSSE_MultipleBlankLines(t *testing.T) {
	input := "data: first\n\n\ndata: second\n\n"
	var events []Event
	err := ParseSSE(strings.NewReader(input), func(e Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

// TestStream_Terminal403 tests 403 is terminal (no retry).
func TestStream_Terminal403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":"forbidden"}`)
	}))
	defer srv.Close()

	ctx := context.Background()
	err := Stream(ctx, Config{
		URL:    srv.URL,
		Token:  "bad",
		Client: srv.Client(),
	}, func(e Event) error {
		return nil
	})

	var httpErr *HTTPError
	if !AsHTTPError(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %v", err)
	}
	if httpErr.StatusCode != 403 {
		t.Errorf("expected 403, got %d", httpErr.StatusCode)
	}
}

// TestStream_Non200Retryable tests that a 500 is retryable (not terminal).
func TestStream_Non200Retryable(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":"oops"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: ok\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Stream(ctx, Config{
			URL:       srv.URL,
			Token:     "test",
			UserAgent: "test/1.0",
			Client:    srv.Client(),
			Backoff:   BackoffConfig{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond, Factor: 2},
		}, func(e Event) error {
			if string(e.Data) == "ok" {
				cancel()
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	if count.Load() < 2 {
		t.Errorf("expected at least 2 requests (retry), got %d", count.Load())
	}
}

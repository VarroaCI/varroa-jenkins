package activity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRingBackfill(t *testing.T) {
	s := New(200)
	s.Append(Event{Type: "connected", Controller: "foo", Namespace: "team-a", Message: "connected"})
	s.Append(Event{Type: "connected", Controller: "bar", Namespace: "team-b", Message: "connected"})
	s.Append(Event{Type: "connected", Controller: "foo", Namespace: "team-a", Message: "heartbeat"})

	bf := NewRingBackfill(s)

	// Global feed: returns all events.
	events, err := bf.Recent(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Per-controller: scope "team-a/foo" should return only foo's events.
	events, err = bf.Recent(context.Background(), "team-a/foo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events for team-a/foo, got %d: %+v", len(events), events)
	}
	for _, e := range events {
		if e.Controller != "foo" {
			t.Errorf("expected Controller 'foo', got %q", e.Controller)
		}
	}

	// Limit.
	events, err = bf.Recent(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event with limit=1, got %d", len(events))
	}
}

func TestSeverityNormalizationAndBoundCursor(t *testing.T) {
	e := Event{Type: "retry-failed", Actor: " User@Example.com ", Result: "FAILURE"}
	e.Normalize()
	if e.Severity != "error" || e.Actor != "User@Example.com" {
		t.Fatalf("unexpected normalization: %#v", e)
	}
	e = Event{Type: "build", Result: "FAILURE"}
	e.Normalize()
	if e.Severity != "info" {
		t.Fatalf("Jenkins result changed severity: %s", e.Severity)
	}

	now := time.Now()
	store := New(10)
	store.Append(Event{Timestamp: now, Type: "one", Source: "mite"})
	store.Append(Event{Timestamp: now.Add(time.Second), Type: "two", Source: "mite"})
	q := Query{Limit: 1, Source: "mite"}
	page, err := NewRingBackfill(store).(Querier).Query(context.Background(), q)
	if err != nil || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("expected cursor page: %#v %v", page, err)
	}
	q.Cursor = page.NextCursor
	q.Source = "operator"
	_, err = NewRingBackfill(store).(Querier).Query(context.Background(), q)
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("expected bound cursor rejection, got %v", err)
	}
}

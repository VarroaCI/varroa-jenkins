package recordstore

import (
	"context"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// TestPgBackfillQuery_PredicateRejectsHalf proves HasMore stays accurate
// when the Go predicate rejects roughly half of a batch: the reader must
// keep pulling batches until it fills limit or the source is exhausted.
func TestPgBackfillQuery_PredicateRejectsHalf(t *testing.T) {
	t.Parallel()

	const n = 60
	base := time.Now().UTC().Truncate(time.Second)
	events := make([]activity.Event, n)
	for i := 0; i < n; i++ {
		ns := "ns-odd"
		if i%2 == 0 {
			ns = "ns-even"
		}
		events[i] = activity.Event{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Cluster:   "core", Namespace: ns, Controller: "ctrl", Source: "user", Type: "t",
		}
	}
	pool := seedActivityEvents(t, events)
	reader := NewReader(pool)
	bf := NewActivityBackfill(reader)

	authorize := func(e activity.Event) bool { return e.Namespace == "ns-even" }
	page, err := bf.Query(context.Background(), activity.Query{Cluster: "core", Limit: 20, Scope: activity.Scope{All: true}, Authorize: authorize})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 20 {
		t.Fatalf("got %d accepted items, want 20", len(page.Items))
	}
	for _, e := range page.Items {
		if e.Namespace != "ns-even" {
			t.Fatalf("rejected row %+v reached the response", e)
		}
	}
	// 30 ns-even rows exist total; 20 were returned, so more remain.
	if !page.HasMore {
		t.Fatal("expected HasMore true: 10 more ns-even rows exist")
	}

	// The next page must not repeat any row from the first.
	seen := make(map[string]bool, 20)
	for _, e := range page.Items {
		seen[activity.EventKey(e)] = true
	}
	q2 := activity.Query{Cluster: "core", Limit: 20, Scope: activity.Scope{All: true}, Authorize: authorize, Cursor: page.NextCursor}
	page2, err := bf.Query(context.Background(), q2)
	if err != nil {
		t.Fatalf("Query page 2: %v", err)
	}
	if len(page2.Items) != 10 {
		t.Fatalf("got %d accepted items on page 2, want the remaining 10", len(page2.Items))
	}
	for _, e := range page2.Items {
		if seen[activity.EventKey(e)] {
			t.Fatalf("row %+v repeated across pages", e)
		}
	}
	if page2.HasMore {
		t.Fatal("expected HasMore false: every ns-even row has now been returned")
	}
}

// TestPgBackfillQuery_TenRejectedBatchesReportsHasMore proves the
// ten-batch bound stops the loop and reports HasMore true even though no
// authorized match was found, per D4: "hasMore is therefore not exact."
func TestPgBackfillQuery_TenRejectedBatchesReportsHasMore(t *testing.T) {
	t.Parallel()

	// limit=5 means each batch reads 6 raw rows; 10 batches need at least
	// 60 rows so every batch is full (HasMore stays true through all ten).
	const n = 120
	base := time.Now().UTC().Truncate(time.Second)
	events := make([]activity.Event, n)
	for i := 0; i < n; i++ {
		events[i] = activity.Event{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Cluster:   "core", Namespace: "ns", Controller: "ctrl", Source: "user", Type: "t",
		}
	}
	pool := seedActivityEvents(t, events)
	reader := NewReader(pool)
	bf := NewActivityBackfill(reader)

	rejectAll := func(activity.Event) bool { return false }
	page, err := bf.Query(context.Background(), activity.Query{Cluster: "core", Limit: 5, Scope: activity.Scope{All: true}, Authorize: rejectAll})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("got %d items, want 0 — every row was rejected", len(page.Items))
	}
	if !page.HasMore {
		t.Fatal("expected HasMore true: the ten-batch bound stopped the loop, not the source")
	}
	if page.NextCursor == "" {
		t.Fatal("expected a cursor pointing at the last scanned candidate")
	}

	// The cursor must anchor on the last scanned candidate, not repeat any
	// of the (rejected) candidates the first call already scanned.
	page2, err := bf.Query(context.Background(), activity.Query{Cluster: "core", Limit: 5, Scope: activity.Scope{All: true}, Authorize: rejectAll, Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("Query page 2: %v", err)
	}
	if len(page2.Items) != 0 {
		t.Fatalf("got %d items on page 2, want 0", len(page2.Items))
	}
	if !page2.HasMore {
		t.Fatal("expected HasMore true on page 2 too: rows remain past the ten-batch bound")
	}
}

// TestPgBackfillQuery_ScopeAdmitsPredicateRejects proves a row the SQL
// scope admits, but the Go predicate rejects, never reaches the response
// — Authorize stays authoritative over the Scope pushdown (D5).
func TestPgBackfillQuery_ScopeAdmitsPredicateRejects(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	events := []activity.Event{
		{Timestamp: base, Cluster: "core", Namespace: "ns-a", Controller: "ctrl-x", Source: "user", Type: "t"},
		{Timestamp: base.Add(time.Second), Cluster: "core", Namespace: "ns-a", Controller: "ctrl-y", Source: "user", Type: "t"},
	}
	pool := seedActivityEvents(t, events)
	reader := NewReader(pool)
	bf := NewActivityBackfill(reader)

	// Scope admits the whole ns-a namespace (a sound superset), but the
	// predicate only actually authorizes ctrl-x.
	scope := activity.Scope{Namespaces: []string{"ns-a"}}
	authorize := func(e activity.Event) bool { return e.Controller == "ctrl-x" }

	page, err := bf.Query(context.Background(), activity.Query{Cluster: "core", Limit: 10, Scope: scope, Authorize: authorize})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Controller != "ctrl-x" {
		t.Fatalf("expected only ctrl-x, got %+v", page.Items)
	}
}

// TestPgBackfillRecent proves Recent scopes by cluster/namespace/controller
// and returns newest-first, matching the retired ring/JetStream backfills'
// contract.
func TestPgBackfillRecent(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	events := []activity.Event{
		{Timestamp: base, Cluster: "core", Namespace: "ns-a", Controller: "ctrl-a", Source: "user", Type: "t", Message: "first"},
		{Timestamp: base.Add(time.Second), Cluster: "core", Namespace: "ns-a", Controller: "ctrl-a", Source: "user", Type: "t", Message: "second"},
		{Timestamp: base.Add(2 * time.Second), Cluster: "core", Namespace: "ns-b", Controller: "ctrl-b", Source: "user", Type: "t", Message: "third"},
	}
	pool := seedActivityEvents(t, events)
	reader := NewReader(pool)
	bf := NewActivityBackfill(reader)

	got, err := bf.Recent(context.Background(), "core/ns-a/ctrl-a", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Message != "second" || got[1].Message != "first" {
		t.Fatalf("expected newest-first order, got %+v", got)
	}
}

// TestPgBackfillRetention proves Retention delegates to
// Reader.ActivityRetentionDays.
func TestPgBackfillRetention(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)
	reader := NewReader(pool, 30)
	bf := NewActivityBackfill(reader)

	mode, days := bf.Retention()
	if mode != "on" || days != 30 {
		t.Fatalf("Retention() = (%q, %d), want (\"on\", 30)", mode, days)
	}
}

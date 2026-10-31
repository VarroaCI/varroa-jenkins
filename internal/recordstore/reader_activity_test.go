package recordstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// seedActivityEvents inserts every event through the same ingest path the
// recorder uses (NewActivityIngester.Insert), computing each row's
// event_key with EventKey, and returns the pool for querying.
func seedActivityEvents(t *testing.T, events []activity.Event) *pgxpool.Pool {
	t.Helper()
	pool, _ := setupLedgerPool(t)
	ing := NewActivityIngester(pool)
	ctx := context.Background()
	for i := range events {
		events[i].Normalize()
		key := activity.EventKey(events[i])
		inserted, err := ing.Insert(ctx, events[i], key)
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
		if !inserted {
			t.Fatalf("event %d collided with an earlier event_key", i)
		}
	}
	return pool
}

// TestListActivity_FilterMapping covers each of the nine ActivityQuery
// filters from D4's mapping table, including the Controller field's two
// accepted forms.
func TestListActivity_FilterMapping(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	events := []activity.Event{
		{Timestamp: base, Cluster: "c1", Namespace: "ns1", Controller: "ctrl1", Source: "user", Severity: "info", Actor: "alice", Type: "login", Message: "alice logged in"},
		{Timestamp: base.Add(1 * time.Second), Cluster: "c2", Namespace: "ns2", Controller: "ctrl2", Source: "operator", Severity: "warning", Actor: "bob", Type: "reconcile.triggered", Message: "reconcile"},
		{Timestamp: base.Add(2 * time.Second), Cluster: "c1", Namespace: "", Controller: "", Source: "api", Severity: "error", Actor: "", Type: "settings.updated", Message: "global"},
	}
	pool := seedActivityEvents(t, events)
	r := NewReader(pool)
	ctx := context.Background()

	tests := []struct {
		name  string
		q     ActivityQuery
		wantN int
		check func(t *testing.T, items []activity.Event)
	}{
		{"Cluster", ActivityQuery{Cluster: "c1", Scope: activity.Scope{All: true}}, 2, nil},
		{"Controller bare name", ActivityQuery{Controller: "ctrl1", Scope: activity.Scope{All: true}}, 1, nil},
		{"Controller ns/name form", ActivityQuery{Controller: "ns1/ctrl1", Scope: activity.Scope{All: true}}, 1, nil},
		{"Namespace", ActivityQuery{Namespace: "ns2", Scope: activity.Scope{All: true}}, 1, nil},
		{"Source", ActivityQuery{Source: "operator", Scope: activity.Scope{All: true}}, 1, nil},
		{"Severity", ActivityQuery{Severity: "error", Scope: activity.Scope{All: true}}, 1, nil},
		{"Actor", ActivityQuery{Actor: "bob", Scope: activity.Scope{All: true}}, 1, nil},
		{"Type", ActivityQuery{Type: "login", Scope: activity.Scope{All: true}}, 1, nil},
		{"Start", ActivityQuery{Start: tPtr(base.Add(1 * time.Second)), End: tPtr(base.Add(10 * time.Second)), Scope: activity.Scope{All: true}}, 2, nil},
		{"End", ActivityQuery{Start: tPtr(base.Add(-10 * time.Second)), End: tPtr(base.Add(1 * time.Second)), Scope: activity.Scope{All: true}}, 1, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := r.ListActivity(ctx, tt.q)
			if err != nil {
				t.Fatalf("ListActivity: %v", err)
			}
			if len(page.Items) != tt.wantN {
				t.Fatalf("got %d items, want %d: %+v", len(page.Items), tt.wantN, page.Items)
			}
			if tt.check != nil {
				tt.check(t, page.Items)
			}
		})
	}
}

func tPtr(t time.Time) *time.Time { return &t }

// TestListActivity_ExcludeRoutineSignIns proves the SQL exclusion drops each
// routine local sign-in and keeps a break-glass sign-in, a failed sign-in,
// and every other event.
func TestListActivity_ExcludeRoutineSignIns(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	events := []activity.Event{
		{Timestamp: base, Cluster: "c1", Namespace: "ns", Controller: "ctrl", Source: "user", Type: "auth.local.login", Severity: "info", Message: "routine"},
		{Timestamp: base.Add(time.Second), Cluster: "c1", Namespace: "ns", Controller: "ctrl", Source: "user", Type: "auth.local.login", Severity: "warning", Message: "break-glass"},
		{Timestamp: base.Add(2 * time.Second), Cluster: "c1", Namespace: "ns", Controller: "ctrl", Source: "user", Type: "auth.local.login_failed", Severity: "warning", Message: "failed"},
		{Timestamp: base.Add(3 * time.Second), Cluster: "c1", Namespace: "ns", Controller: "ctrl", Source: "api", Type: "settings.updated", Severity: "info", Message: "settings"},
		{Timestamp: base.Add(4 * time.Second), Cluster: "c1", Namespace: "ns", Controller: "ctrl", Source: "user", Type: "auth.local.login", Severity: "error", Message: "error sign-in"},
	}
	pool := seedActivityEvents(t, events)
	r := NewReader(pool)
	ctx := context.Background()

	all, err := r.ListActivity(ctx, ActivityQuery{Scope: activity.Scope{All: true}})
	if err != nil {
		t.Fatalf("ListActivity without the flag: %v", err)
	}
	if len(all.Items) != 5 {
		t.Fatalf("without the flag: got %d items, want 5", len(all.Items))
	}

	page, err := r.ListActivity(ctx, ActivityQuery{ExcludeRoutineSignIns: true, Scope: activity.Scope{All: true}})
	if err != nil {
		t.Fatalf("ListActivity with the flag: %v", err)
	}
	if len(page.Items) != 4 {
		t.Fatalf("with the flag: got %d items, want 4: %+v", len(page.Items), page.Items)
	}
	for _, e := range page.Items {
		if e.Message == "routine" {
			t.Fatalf("routine sign-in reached the page: %+v", e)
		}
	}
}

// TestActivityFilterKey_ExcludeRoutineSignIns proves the cursor fingerprint
// holds the flag only when it is set. A cursor from the unflagged query must
// not resume the flagged one, and an unset flag leaves the key unchanged.
func TestActivityFilterKey_ExcludeRoutineSignIns(t *testing.T) {
	t.Parallel()

	unset := ActivityQuery{Scope: activity.Scope{All: true}}
	set := unset
	set.ExcludeRoutineSignIns = true

	if activityFilterKey(unset) == activityFilterKey(set) {
		t.Fatal("the flagged and unflagged queries share a cursor fingerprint")
	}
	// Nine empty filter fields, then the scope: true|false|[] for All.
	wantUnset := strings.Repeat("\x1f", 9) + "true|false|[]"
	if got := activityFilterKey(unset); got != wantUnset {
		t.Fatalf("unset flag changed the key: got %q, want %q", got, wantUnset)
	}
}

// TestListActivity_ScopePushdown proves the Scope field is pushed into
// SQL: a Namespaces-scoped query never returns a row outside it, and never
// returns a global (empty-controller) row unless Global is also set.
func TestListActivity_ScopePushdown(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	events := []activity.Event{
		{Timestamp: base, Cluster: "c1", Namespace: "ns-a", Controller: "ctrl-a", Source: "user", Type: "t"},
		{Timestamp: base.Add(time.Second), Cluster: "c1", Namespace: "ns-b", Controller: "ctrl-b", Source: "user", Type: "t"},
		{Timestamp: base.Add(2 * time.Second), Cluster: "c1", Namespace: "", Controller: "", Source: "user", Type: "t"},
	}
	pool := seedActivityEvents(t, events)
	r := NewReader(pool)
	ctx := context.Background()

	page, err := r.ListActivity(ctx, ActivityQuery{Scope: activity.Scope{Namespaces: []string{"ns-a"}}})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Namespace != "ns-a" {
		t.Fatalf("expected only the ns-a row, got %+v", page.Items)
	}

	page, err = r.ListActivity(ctx, ActivityQuery{Scope: activity.Scope{Namespaces: []string{"ns-a"}, Global: true}})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected the ns-a row plus the global row, got %d: %+v", len(page.Items), page.Items)
	}

	page, err = r.ListActivity(ctx, ActivityQuery{Scope: activity.Scope{}})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected the zero-value Scope to admit nothing, got %d: %+v", len(page.Items), page.Items)
	}
}

// TestListActivity_KeysetPaging_NoGapNoRepeat pages 300 seeded rows in
// both directions and asserts every row is returned exactly once.
func TestListActivity_KeysetPaging_NoGapNoRepeat(t *testing.T) {
	t.Parallel()

	const n = 300
	base := time.Now().UTC().Truncate(time.Second)
	events := make([]activity.Event, n)
	for i := 0; i < n; i++ {
		events[i] = activity.Event{
			Timestamp:  base.Add(time.Duration(i) * time.Second),
			Cluster:    "core",
			Namespace:  "ns",
			Controller: "ctrl",
			Source:     "user",
			Type:       "t",
			Message:    "seed",
		}
	}
	pool := seedActivityEvents(t, events)
	r := NewReader(pool)
	ctx := context.Background()

	// Backward (default, newest-first): page with a size that does not
	// evenly divide n, so the last page is partial.
	seen := make(map[string]int)
	q := ActivityQuery{Cluster: "core", Limit: 37, Scope: activity.Scope{All: true}}
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("too many pages; keyset paging is not terminating")
		}
		page, err := r.ListActivity(ctx, q)
		if err != nil {
			t.Fatalf("ListActivity backward page %d: %v", pages, err)
		}
		for _, e := range page.Items {
			seen[activity.EventKey(e)]++
		}
		if !page.HasMore {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("backward paging: saw %d distinct events, want %d", len(seen), n)
	}
	for k, c := range seen {
		if c != 1 {
			t.Fatalf("backward paging: event %s seen %d times, want 1", k, c)
		}
	}

	// Forward (explicit range covering every seeded row), oldest-first.
	seen = make(map[string]int)
	start := base.Add(-time.Hour)
	end := base.Add(time.Duration(n+1) * time.Second)
	q = ActivityQuery{Cluster: "core", Limit: 41, Start: &start, End: &end, Scope: activity.Scope{All: true}}
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("too many pages; keyset paging is not terminating")
		}
		page, err := r.ListActivity(ctx, q)
		if err != nil {
			t.Fatalf("ListActivity forward page %d: %v", pages, err)
		}
		for _, e := range page.Items {
			seen[activity.EventKey(e)]++
		}
		if !page.HasMore {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("forward paging: saw %d distinct events, want %d", len(seen), n)
	}
	for k, c := range seen {
		if c != 1 {
			t.Fatalf("forward paging: event %s seen %d times, want 1", k, c)
		}
	}
}

// TestListActivity_KeysetPaging_TiedTimestamps seeds several rows that
// share one ts and pages them with a limit small enough that the page
// boundary falls inside that tie group, in both directions. (ts,
// event_key) keyset paging must still return every row exactly once: the
// event_key tiebreaker, not ts alone, decides where a page cuts.
func TestListActivity_KeysetPaging_TiedTimestamps(t *testing.T) {
	t.Parallel()

	const n = 20
	base := time.Now().UTC().Truncate(time.Second)
	events := make([]activity.Event, n)
	for i := 0; i < n; i++ {
		events[i] = activity.Event{
			Timestamp:  base,
			Cluster:    "core",
			Namespace:  "ns",
			Controller: "ctrl",
			Source:     "user",
			Type:       "t",
			Message:    fmt.Sprintf("tie-%d", i),
		}
	}
	pool := seedActivityEvents(t, events)
	r := NewReader(pool)
	ctx := context.Background()

	// Backward (default, newest-first): every row shares one ts, so any
	// limit under n forces the boundary inside the tie group.
	seen := make(map[string]int)
	q := ActivityQuery{Cluster: "core", Limit: 7, Scope: activity.Scope{All: true}}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("too many pages; keyset paging is not terminating")
		}
		page, err := r.ListActivity(ctx, q)
		if err != nil {
			t.Fatalf("ListActivity backward page %d: %v", pages, err)
		}
		for _, e := range page.Items {
			seen[activity.EventKey(e)]++
		}
		if !page.HasMore {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("backward paging: saw %d distinct events, want %d", len(seen), n)
	}
	for k, c := range seen {
		if c != 1 {
			t.Fatalf("backward paging: event %s seen %d times, want 1", k, c)
		}
	}

	// Forward (explicit range covering the tied timestamp), oldest-first.
	seen = make(map[string]int)
	start := base.Add(-time.Second)
	end := base.Add(time.Second)
	q = ActivityQuery{Cluster: "core", Limit: 9, Start: &start, End: &end, Scope: activity.Scope{All: true}}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("too many pages; keyset paging is not terminating")
		}
		page, err := r.ListActivity(ctx, q)
		if err != nil {
			t.Fatalf("ListActivity forward page %d: %v", pages, err)
		}
		for _, e := range page.Items {
			seen[activity.EventKey(e)]++
		}
		if !page.HasMore {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("forward paging: saw %d distinct events, want %d", len(seen), n)
	}
	for k, c := range seen {
		if c != 1 {
			t.Fatalf("forward paging: event %s seen %d times, want 1", k, c)
		}
	}
}

// TestListActivity_CursorErrors covers the two cursor error sentinels
// D4 names: a filter mismatch and a cursor older than the retention
// window.
func TestListActivity_CursorErrors(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	events := []activity.Event{
		{Timestamp: base, Cluster: "c1", Namespace: "ns", Controller: "ctrl", Source: "user", Type: "t"},
	}
	pool := seedActivityEvents(t, events)
	ctx := context.Background()

	t.Run("filter mismatch", func(t *testing.T) {
		r := NewReader(pool)
		qA := ActivityQuery{Cluster: "c1", Limit: 1, Scope: activity.Scope{All: true}}
		page, err := r.ListActivity(ctx, qA)
		if err != nil {
			t.Fatalf("seed page: %v", err)
		}
		// Force a cursor to exist by requesting a page smaller than the
		// data: fabricate one from the returned item so this test does not
		// depend on HasMore with only one seeded row.
		cur := encodeActivityCursor(activityCursor{Direction: "backward", Filters: activityFilterKey(qA), TS: page.Items[0].Timestamp, EventKey: activity.EventKey(page.Items[0])})
		qB := ActivityQuery{Cluster: "c2", Limit: 1, Cursor: cur, Scope: activity.Scope{All: true}}
		if _, err := r.ListActivity(ctx, qB); !errors.Is(err, activity.ErrInvalidCursor) {
			t.Fatalf("got err = %v, want ErrInvalidCursor", err)
		}
	})

	t.Run("expired cursor", func(t *testing.T) {
		r := NewReader(pool, 1) // 1-day retention
		q := ActivityQuery{Cluster: "c1", Limit: 1, Scope: activity.Scope{All: true}}
		oldCursor := encodeActivityCursor(activityCursor{
			Direction: "backward", Filters: activityFilterKey(q),
			TS: time.Now().Add(-48 * time.Hour), EventKey: "does-not-matter",
		})
		q.Cursor = oldCursor
		if _, err := r.ListActivity(ctx, q); !errors.Is(err, activity.ErrCursorExpired) {
			t.Fatalf("got err = %v, want ErrCursorExpired", err)
		}
	})
}

// TestActivityRetentionDays proves ActivityRetentionDays reports the day
// count NewReader was constructed with, and still runs checkSchema.
func TestActivityRetentionDays(t *testing.T) {
	t.Parallel()

	pool, _ := setupLedgerPool(t)

	r := NewReader(pool, 30)
	days, err := r.ActivityRetentionDays(context.Background())
	if err != nil {
		t.Fatalf("ActivityRetentionDays: %v", err)
	}
	if days != 30 {
		t.Fatalf("days = %d, want 30", days)
	}

	rZero := NewReader(pool)
	days, err = rZero.ActivityRetentionDays(context.Background())
	if err != nil {
		t.Fatalf("ActivityRetentionDays: %v", err)
	}
	if days != 0 {
		t.Fatalf("days = %d, want 0 when NewReader is given none", days)
	}
}

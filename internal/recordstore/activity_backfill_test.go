package recordstore

import (
	"context"
	"errors"
	"fmt"
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

// TestPgBackfillQuery_ExcludeRoutineSignInsFillsLimit proves the exclusion
// reaches the SQL. Routine sign-ins newer than the operational events must
// not fill the page, so the page holds the operational events and no more.
func TestPgBackfillQuery_ExcludeRoutineSignInsFillsLimit(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	var events []activity.Event
	for i := 0; i < 5; i++ {
		events = append(events, activity.Event{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Cluster:   "core", Namespace: "ns", Controller: "ctrl", Source: "operator",
			Type: "controller.phase", Severity: "info", Message: fmt.Sprintf("operational %d", i),
		})
	}
	for i := 0; i < 40; i++ {
		events = append(events, activity.Event{
			Timestamp: base.Add(time.Duration(10+i) * time.Second),
			Cluster:   "core", Source: "user", Type: "auth.local.login", Severity: "info",
			Message: fmt.Sprintf("routine %d", i),
		})
	}
	pool := seedActivityEvents(t, events)
	bf := NewActivityBackfill(NewReader(pool))
	ctx := context.Background()

	page, err := bf.Query(ctx, activity.Query{Limit: 5, ExcludeRoutineSignIns: true, Scope: activity.Scope{All: true}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("got %d items, want the 5 operational events", len(page.Items))
	}
	if page.HasMore || page.NextCursor != "" {
		t.Fatalf("exact-size last page: HasMore %v, NextCursor %q; want no more history", page.HasMore, page.NextCursor)
	}
	for i, e := range page.Items {
		want := fmt.Sprintf("operational %d", 4-i)
		if e.Message != want {
			t.Fatalf("item %d = %q, want %q (newest first)", i, e.Message, want)
		}
	}
	// A paged read with a cursor returns the next operational events, not a repeat.
	first, err := bf.Query(ctx, activity.Query{Limit: 3, ExcludeRoutineSignIns: true, Scope: activity.Scope{All: true}})
	if err != nil {
		t.Fatalf("Query page 1: %v", err)
	}
	if len(first.Items) != 3 || !first.HasMore {
		t.Fatalf("page 1: got %d items, HasMore %v; want 3 and true", len(first.Items), first.HasMore)
	}
	second, err := bf.Query(ctx, activity.Query{Limit: 3, ExcludeRoutineSignIns: true, Scope: activity.Scope{All: true}, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("Query page 2: %v", err)
	}
	if len(second.Items) != 2 {
		t.Fatalf("page 2: got %d items, want the remaining 2", len(second.Items))
	}
	seen := map[string]bool{}
	for _, e := range append(first.Items, second.Items...) {
		if seen[e.Message] {
			t.Fatalf("event %q repeated across pages", e.Message)
		}
		seen[e.Message] = true
	}
}

// TestPgBackfillQuery_UnreachableStoreAnswersErrUnavailable proves a read that
// cannot reach the record store reaches the caller as ErrUnavailable. The BFF
// answers that error with 503 record_store_unreachable.
func TestPgBackfillQuery_UnreachableStoreAnswersErrUnavailable(t *testing.T) {
	bf := NewActivityBackfill(WithTimeout(unreachableReader(t), 200*time.Millisecond))

	_, err := bf.Query(context.Background(), activity.Query{Limit: 5, Scope: activity.Scope{All: true}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Query error = %v, want ErrUnavailable", err)
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

// pagedActivityReader serves ListActivity from pages in order and counts the
// calls. ActivityRetentionDays returns days and daysErr. Every other Reader
// method panics through the nil embedded Reader, which the tests never reach.
type pagedActivityReader struct {
	Reader
	pages   []ActivityPage
	calls   int
	days    int
	daysErr error
}

func (p *pagedActivityReader) ListActivity(_ context.Context, _ ActivityQuery) (ActivityPage, error) {
	p.calls++
	if p.calls > len(p.pages) {
		return ActivityPage{}, nil
	}
	return p.pages[p.calls-1], nil
}

func (p *pagedActivityReader) ActivityRetentionDays(context.Context) (int, error) {
	return p.days, p.daysErr
}

// TestPgBackfillQuery_RangeOutsideRetention proves a range start before the
// retention window answers ErrRangeOutsideRetention without a list call, and
// that a zero retention sets no bound.
func TestPgBackfillQuery_RangeOutsideRetention(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	cases := []struct {
		name    string
		days    int
		start   time.Time
		wantErr error
		calls   int
	}{
		{"start before window", 1, now.Add(-48 * time.Hour), activity.ErrRangeOutsideRetention, 0},
		{"start inside window", 1, now.Add(-time.Hour), nil, 1},
		{"zero days sets no bound", 0, now.Add(-48 * time.Hour), nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &pagedActivityReader{days: tc.days, pages: []ActivityPage{{}}}
			bf := NewActivityBackfill(reader)
			end := now.Add(time.Hour)
			start := tc.start
			_, err := bf.Query(context.Background(), activity.Query{Limit: 5, Start: &start, End: &end, Scope: activity.Scope{All: true}})
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Query error = %v, want nil", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("Query error = %v, want %v", err, tc.wantErr)
			}
			if reader.calls != tc.calls {
				t.Fatalf("ListActivity calls = %d, want %d", reader.calls, tc.calls)
			}
		})
	}
}

// TestPgBackfillQuery_RangeUnreachableStoreAnswersErrUnavailable proves a range
// read against a store that cannot be reached answers ErrUnavailable, so the
// BFF gives 503 and not a retention answer.
func TestPgBackfillQuery_RangeUnreachableStoreAnswersErrUnavailable(t *testing.T) {
	bf := NewActivityBackfill(WithTimeout(unreachableReader(t), 200*time.Millisecond))

	now := time.Now().UTC()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	_, err := bf.Query(context.Background(), activity.Query{Limit: 5, Start: &start, End: &end, Scope: activity.Scope{All: true}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Query error = %v, want ErrUnavailable", err)
	}
}

// TestPgBackfillQuery_HasMoreAtExactFill proves HasMore and NextCursor after
// accepted reaches limit. A stop with no later authorized row is the last page.
func TestPgBackfillQuery_HasMoreAtExactFill(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	mkPage := func(n int, hasMore bool) ActivityPage {
		items := make([]activity.Event, n)
		for i := range items {
			items[i] = activity.Event{
				Timestamp: base.Add(time.Duration(i) * time.Second),
				Cluster:   "core", Namespace: "ns", Controller: "ctrl", Source: "user", Type: "t",
				Message: fmt.Sprintf("row %d", i),
			}
		}
		return ActivityPage{Items: items, HasMore: hasMore}
	}
	accept := func(keep ...int) func(activity.Event) bool {
		return func(e activity.Event) bool {
			for _, k := range keep {
				if e.Message == fmt.Sprintf("row %d", k) {
					return true
				}
			}
			return false
		}
	}

	t.Run("exact size, no more rows", func(t *testing.T) {
		bf := NewActivityBackfill(&pagedActivityReader{pages: []ActivityPage{mkPage(3, false)}})
		page, err := bf.Query(context.Background(), activity.Query{Limit: 3, Scope: activity.Scope{All: true}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 3 || page.HasMore || page.NextCursor != "" {
			t.Fatalf("got %d items, HasMore %v, NextCursor %q; want 3, false, empty", len(page.Items), page.HasMore, page.NextCursor)
		}
	})

	t.Run("mid-batch fill, rest rejected, no more rows", func(t *testing.T) {
		bf := NewActivityBackfill(&pagedActivityReader{pages: []ActivityPage{mkPage(5, false)}})
		page, err := bf.Query(context.Background(), activity.Query{Limit: 3, Scope: activity.Scope{All: true}, Authorize: accept(0, 1, 2)})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 3 || page.HasMore || page.NextCursor != "" {
			t.Fatalf("got %d items, HasMore %v, NextCursor %q; want 3, false, empty", len(page.Items), page.HasMore, page.NextCursor)
		}
	})

	t.Run("mid-batch fill, later row accepted", func(t *testing.T) {
		bf := NewActivityBackfill(&pagedActivityReader{pages: []ActivityPage{mkPage(5, false)}})
		q := activity.Query{Limit: 3, Scope: activity.Scope{All: true}, Authorize: accept(0, 1, 2, 4)}
		page, err := bf.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !page.HasMore || page.NextCursor == "" {
			t.Fatalf("HasMore %v, NextCursor %q; want true and a cursor", page.HasMore, page.NextCursor)
		}
		cur, err := decodeActivityCursor(page.NextCursor, activityFilterKey(ActivityQuery{Scope: q.Scope}), "backward")
		if err != nil {
			t.Fatalf("decode cursor: %v", err)
		}
		third := mkPage(5, false).Items[2]
		if !cur.TS.Equal(third.Timestamp) || cur.EventKey != activity.EventKey(third) {
			t.Fatalf("cursor anchors on %v/%q, want the third accepted row %v/%q", cur.TS, cur.EventKey, third.Timestamp, activity.EventKey(third))
		}
	})

	t.Run("batch end fill, source reports more", func(t *testing.T) {
		bf := NewActivityBackfill(&pagedActivityReader{pages: []ActivityPage{mkPage(3, true)}})
		page, err := bf.Query(context.Background(), activity.Query{Limit: 3, Scope: activity.Scope{All: true}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !page.HasMore || page.NextCursor == "" {
			t.Fatalf("HasMore %v, NextCursor %q; want true and a cursor", page.HasMore, page.NextCursor)
		}
	})
}

// TestPgBackfillQuery_ExactSizeLastPageHasNoMore proves a page that holds
// every row of the store reports no more history. A paged read stops at the
// last row and carries no cursor.
func TestPgBackfillQuery_ExactSizeLastPageHasNoMore(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second)
	events := make([]activity.Event, 5)
	for i := range events {
		events[i] = activity.Event{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Cluster:   "core", Namespace: "ns", Controller: "ctrl", Source: "operator", Type: "t",
			Message: fmt.Sprintf("row %d", i),
		}
	}
	bf := NewActivityBackfill(NewReader(seedActivityEvents(t, events), 30))
	ctx := context.Background()
	all := activity.Scope{All: true}

	backward, err := bf.Query(ctx, activity.Query{Limit: 5, Scope: all})
	if err != nil {
		t.Fatalf("backward Query: %v", err)
	}
	if len(backward.Items) != 5 || backward.HasMore || backward.NextCursor != "" {
		t.Fatalf("backward: %d items, HasMore %v, NextCursor %q; want 5, false, empty", len(backward.Items), backward.HasMore, backward.NextCursor)
	}

	start, end := base.Add(-time.Minute), base.Add(time.Minute)
	forward, err := bf.Query(ctx, activity.Query{Limit: 5, Start: &start, End: &end, Scope: all})
	if err != nil {
		t.Fatalf("forward Query: %v", err)
	}
	if len(forward.Items) != 5 || forward.HasMore || forward.NextCursor != "" {
		t.Fatalf("forward: %d items, HasMore %v, NextCursor %q; want 5, false, empty", len(forward.Items), forward.HasMore, forward.NextCursor)
	}

	first, err := bf.Query(ctx, activity.Query{Limit: 4, Start: &start, End: &end, Scope: all})
	if err != nil {
		t.Fatalf("forward page 1: %v", err)
	}
	if len(first.Items) != 4 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("forward page 1: %d items, HasMore %v, NextCursor %q; want 4, true, cursor", len(first.Items), first.HasMore, first.NextCursor)
	}
	second, err := bf.Query(ctx, activity.Query{Limit: 4, Start: &start, End: &end, Scope: all, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("forward page 2: %v", err)
	}
	if len(second.Items) != 1 || second.HasMore || second.NextCursor != "" {
		t.Fatalf("forward page 2: %d items, HasMore %v, NextCursor %q; want 1, false, empty", len(second.Items), second.HasMore, second.NextCursor)
	}
}

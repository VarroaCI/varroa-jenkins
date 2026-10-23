package spend

import (
	"context"
	"testing"
	"time"
)

// recordUpserter records each (tenant, day) it is asked to write.
type recordUpserter struct {
	calls []string
}

func (r *recordUpserter) UpsertSpend(_ context.Context, tenant string, day time.Time) error {
	r.calls = append(r.calls, tenant+"@"+day.UTC().Format("2006-01-02"))
	return nil
}

// TestSpendAcrossUTCDayBoundary proves that the first pass of a new UTC day
// also refreshes the previous day, so a session that ends after midnight is
// counted on the day it started.
func TestSpendAcrossUTCDayBoundary(t *testing.T) {
	up := &recordUpserter{}
	r := &Rollover{Store: up}
	ctx := context.Background()

	late := time.Date(2026, 10, 18, 23, 58, 0, 0, time.UTC)
	if err := r.Tick(ctx, []string{"obj"}, late); err != nil {
		t.Fatal(err)
	}
	early := time.Date(2026, 10, 19, 0, 2, 0, 0, time.UTC)
	if err := r.Tick(ctx, []string{"obj"}, early); err != nil {
		t.Fatal(err)
	}
	again := time.Date(2026, 10, 19, 0, 30, 0, 0, time.UTC)
	if err := r.Tick(ctx, []string{"obj"}, again); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"obj@2026-10-17", // first pass of the process: the previous day, then today
		"obj@2026-10-18",
		"obj@2026-10-18", // first pass of day two: the previous day, then today
		"obj@2026-10-19",
		"obj@2026-10-19", // a later pass of day two
	}
	if len(up.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", up.calls, want)
	}
	for i := range want {
		if up.calls[i] != want[i] {
			t.Fatalf("call %d = %s, want %s (all: %v)", i, up.calls[i], want[i], up.calls)
		}
	}
}

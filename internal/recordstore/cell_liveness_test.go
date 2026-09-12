package recordstore

import (
	"testing"
	"time"
)

// TestCellLivenessGoneMovedReading asserts an old instance listed dead in
// dead_instances, with a last_seen_at older than the moved window, counts
// as gone.
func TestCellLivenessGoneMovedReading(t *testing.T) {
	now := time.Now()
	row := tenantPlacementLivenessRow{
		cellInstance:  "pod-new",
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: now.Add(-6 * time.Minute)}},
	}
	anchor := now.Add(-6 * time.Minute)
	if !cellLivenessGone(now, anchor, "pod-old", row, true) {
		t.Fatal("want gone: the old instance is listed dead past the moved window")
	}
}

// TestCellLivenessGoneChangedInstanceAloneIsNotAReading asserts a changed
// cell instance that never appears in dead_instances answers not gone,
// no matter how old the anchor is.
func TestCellLivenessGoneChangedInstanceAloneIsNotAReading(t *testing.T) {
	now := time.Now()
	row := tenantPlacementLivenessRow{cellInstance: "pod-new"}
	anchor := now.Add(-20 * time.Minute)
	if cellLivenessGone(now, anchor, "pod-old", row, true) {
		t.Fatal("want not gone: a changed placement alone is not a reading")
	}
}

// TestCellLivenessGoneFreshDeadEntryIsNotGoneYet asserts an instance
// listed dead with a last_seen_at inside the moved window is not gone
// yet.
func TestCellLivenessGoneFreshDeadEntryIsNotGoneYet(t *testing.T) {
	now := time.Now()
	row := tenantPlacementLivenessRow{
		cellInstance:  "pod-new",
		deadInstances: []TenantSyncDeadInstance{{Instance: "pod-old", LastSeenAt: now.Add(-10 * time.Second)}},
	}
	anchor := now.Add(-6 * time.Minute)
	if cellLivenessGone(now, anchor, "pod-old", row, true) {
		t.Fatal("want not gone: the dead-instance entry is only ten seconds old")
	}
}

// TestCellLivenessGoneMissingRowAnswersFalse asserts no tenant_placements
// row gives no reading.
func TestCellLivenessGoneMissingRowAnswersFalse(t *testing.T) {
	now := time.Now()
	if cellLivenessGone(now, now.Add(-time.Hour), "pod-old", tenantPlacementLivenessRow{}, false) {
		t.Fatal("want not gone: a missing placement row gives no reading")
	}
}

// TestCellLivenessGoneSilentReading asserts the current instance, with
// cell_alive false and a stale-enough cell_last_seen_at and a fresh
// cell_alive_at, counts as gone.
func TestCellLivenessGoneSilentReading(t *testing.T) {
	now := time.Now()
	lastSeen := now.Add(-20 * time.Minute)
	aliveAt := now.Add(-time.Minute)
	row := tenantPlacementLivenessRow{
		cellInstance:   "pod-a",
		cellAlive:      false,
		cellAliveAt:    &aliveAt,
		cellLastSeenAt: &lastSeen,
	}
	anchor := now.Add(-20 * time.Minute)
	if !cellLivenessGone(now, anchor, "pod-a", row, true) {
		t.Fatal("want gone: the current instance is reported not alive for the whole silent window")
	}
}

// TestCellLivenessGoneSilentReadingLiveCellIsNotGone asserts a current
// instance reported alive is never gone, however old the anchor is.
func TestCellLivenessGoneSilentReadingLiveCellIsNotGone(t *testing.T) {
	now := time.Now()
	aliveAt := now.Add(-time.Minute)
	row := tenantPlacementLivenessRow{cellInstance: "pod-a", cellAlive: true, cellAliveAt: &aliveAt}
	anchor := now.Add(-20 * time.Minute)
	if cellLivenessGone(now, anchor, "pod-a", row, true) {
		t.Fatal("want not gone: cell_alive is true")
	}
}

// TestCellLivenessGoneStaleCellAliveAtAnswersFalse asserts a
// cell_alive_at older than twice the announce interval counts the whole
// reading as absent, even with an old cell_last_seen_at.
func TestCellLivenessGoneStaleCellAliveAtAnswersFalse(t *testing.T) {
	now := time.Now()
	lastSeen := now.Add(-time.Hour)
	staleAliveAt := now.Add(-cellAliveStaleAfter - time.Minute)
	row := tenantPlacementLivenessRow{
		cellInstance:   "pod-a",
		cellAlive:      false,
		cellAliveAt:    &staleAliveAt,
		cellLastSeenAt: &lastSeen,
	}
	anchor := now.Add(-time.Hour)
	if cellLivenessGone(now, anchor, "pod-a", row, true) {
		t.Fatal("want not gone: a stale cell_alive_at counts as an absent signal")
	}
}

// TestCellLivenessGoneAnchorTooFreshHoldsOffTheSilentReading asserts a
// recent anchor never counts as gone, even with a confirmed-dead
// reading, because the silent window has not passed yet.
func TestCellLivenessGoneAnchorTooFreshHoldsOffTheSilentReading(t *testing.T) {
	now := time.Now()
	lastSeen := now.Add(-time.Hour)
	aliveAt := now.Add(-time.Minute)
	row := tenantPlacementLivenessRow{
		cellInstance:   "pod-a",
		cellAlive:      false,
		cellAliveAt:    &aliveAt,
		cellLastSeenAt: &lastSeen,
	}
	anchor := now.Add(-time.Minute)
	if cellLivenessGone(now, anchor, "pod-a", row, true) {
		t.Fatal("want not gone: the run's own heartbeat is only a minute old")
	}
}

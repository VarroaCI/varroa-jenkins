package mite

import "testing"

// The activity stream is a bounded audit log. Heartbeats and snapshots arrive
// once per mite per interval — persisting them as activity events floods the
// stream cap and evicts every real audit event, so only lifecycle transitions
// may map to activity messages.
func TestActivityMessageForBroodEvent(t *testing.T) {
	cases := []struct {
		event   string
		wantOK  bool
		wantMsg string
	}{
		{"connected", true, "mite 1.2.3 connected"},
		{"disconnected", true, "mite disconnected"},
		{"heartbeat", false, ""},
		{"snapshot", false, ""},
		{"unknown-future-event", false, ""},
	}
	for _, tc := range cases {
		msg, ok := activityMessageForBroodEvent(tc.event, "1.2.3")
		if ok != tc.wantOK || msg != tc.wantMsg {
			t.Errorf("activityMessageForBroodEvent(%q) = (%q, %v), want (%q, %v)",
				tc.event, msg, ok, tc.wantMsg, tc.wantOK)
		}
	}
}

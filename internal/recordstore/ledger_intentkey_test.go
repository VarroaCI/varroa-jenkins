package recordstore

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestParseIntentKey covers one valid and one invalid example per
// allocated scheme, the withdrawn webhook scheme, and the 512-byte cap.
func TestParseIntentKey(t *testing.T) {
	runID := uuid.NewString()

	tests := []struct {
		name    string
		key     string
		want    IntentScheme
		wantErr bool
	}{
		{"scm valid", "scm:acme/app:abc123def", IntentSchemeSCM, false},
		{"scm invalid empty revision", "scm:acme/app:", "", true},
		{"manual valid", "manual:" + uuid.NewString(), IntentSchemeManual, false},
		{"manual invalid empty uuid", "manual:", "", true},
		{"cron valid", "cron:acme/app:2024-01-01T00:00:00Z", IntentSchemeCron, false},
		{"cron invalid empty fire minute", "cron:acme/app:", "", true},
		{"retry valid", "retry:" + runID, IntentSchemeRetry, false},
		{"retry invalid empty run id", "retry:", "", true},
		{"upstream valid", "upstream:" + runID + ":3", IntentSchemeUpstream, false},
		{"upstream invalid empty seq", "upstream:" + runID + ":", "", true},
		{"api valid", "api:" + uuid.NewString(), IntentSchemeAPI, false},
		{"api invalid empty uuid", "api:", "", true},
		{"replay valid", "replay:0199aa11-2222-7333-8444-555566667777:0199bb22-3333-7444-8555-666677778888", IntentSchemeReplay, false},
		{"replay invalid empty source", "replay::x", "", true},
		{"replay invalid empty new run id", "replay:x:", "", true},
		{"replay invalid one segment", "replay:" + runID, "", true},
		{"restart valid", "restart:0199aa11-2222-7333-8444-555566667777:0199bb22-3333-7444-8555-666677778888", IntentSchemeRestart, false},
		{"restart invalid empty source", "restart::x", "", true},
		{"restart invalid one segment", "restart:" + runID, "", true},
		{"withdrawn webhook scheme is rejected", "webhook:github:abc", "", true},
		{"unallocated scheme is rejected", "poll:main:1", "", true},
		{"key with no scheme is rejected", "no-colon-here", "", true},
		{"key exceeding 512 bytes is rejected", "manual:" + strings.Repeat("a", 600), "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseIntentKey(tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseIntentKey(%q): want error, got none", tc.key)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIntentKey(%q): unexpected error: %v", tc.key, err)
			}
			if got != tc.want {
				t.Fatalf("ParseIntentKey(%q): want scheme %q, got %q", tc.key, tc.want, got)
			}
		})
	}
}

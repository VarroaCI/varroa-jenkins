package recordstore

import (
	"testing"
	"time"
)

// TestParseRetention covers task 6.1's acceptance list: the boundary
// values 1d and 3650d, a mid-range value, and the three malformed or
// out-of-range inputs.
func TestParseRetention(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		wantDay int
	}{
		{name: "one day", input: "1d", wantDay: 1},
		{name: "365 days", input: "365d", wantDay: 365},
		{name: "3650 days", input: "3650d", wantDay: 3650},
		{name: "zero days is out of range", input: "0d", wantErr: true},
		{name: "3651 days is out of range", input: "3651d", wantErr: true},
		{name: "missing day suffix", input: "90", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRetention(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRetention(%q) = %v, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRetention(%q) returned an unexpected error: %v", tc.input, err)
			}
			wantDuration := time.Duration(tc.wantDay) * 24 * time.Hour
			if got != wantDuration {
				t.Fatalf("ParseRetention(%q) = %v, want %v", tc.input, got, wantDuration)
			}
		})
	}
}

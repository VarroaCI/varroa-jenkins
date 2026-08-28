package activity

import (
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	tests := []struct {
		input        string
		wantMaxAge   time.Duration
		wantOff      bool
		wantFallback bool
	}{
		{input: "off", wantMaxAge: 0, wantOff: true, wantFallback: false},
		{input: "7d", wantMaxAge: 168 * time.Hour, wantOff: false, wantFallback: false},
		{input: "30d", wantMaxAge: 720 * time.Hour, wantOff: false, wantFallback: false},
		{input: "90d", wantMaxAge: 2160 * time.Hour, wantOff: false, wantFallback: false},
		{input: "", wantMaxAge: 168 * time.Hour, wantOff: false, wantFallback: true},
		{input: "garbage", wantMaxAge: 168 * time.Hour, wantOff: false, wantFallback: true},
		{input: "14d", wantMaxAge: 168 * time.Hour, wantOff: false, wantFallback: true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			maxAge, off, fallback := ParseRetention(tt.input)
			if maxAge != tt.wantMaxAge {
				t.Errorf("ParseRetention(%q) maxAge = %v, want %v", tt.input, maxAge, tt.wantMaxAge)
			}
			if off != tt.wantOff {
				t.Errorf("ParseRetention(%q) off = %v, want %v", tt.input, off, tt.wantOff)
			}
			if fallback != tt.wantFallback {
				t.Errorf("ParseRetention(%q) fallback = %v, want %v", tt.input, fallback, tt.wantFallback)
			}
		})
	}
}

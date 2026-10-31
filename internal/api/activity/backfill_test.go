package activity

import (
	"testing"
	"time"
)

func TestIsRoutineSignIn(t *testing.T) {
	tests := []struct {
		name string
		e    Event
		want bool
	}{
		{"informational local sign-in is routine", Event{Type: LocalSignInType, Severity: "info"}, true},
		{"local sign-in with warning is break-glass", Event{Type: LocalSignInType, Severity: "warning"}, false},
		{"local sign-in with error is not routine", Event{Type: LocalSignInType, Severity: "error"}, false},
		{"failed local sign-in is not routine", Event{Type: "auth.local.login_failed", Severity: "warning"}, false},
		{"other event is not routine", Event{Type: "controller.created", Severity: "info"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRoutineSignIn(tt.e); got != tt.want {
				t.Fatalf("IsRoutineSignIn(%+v) = %v, want %v", tt.e, got, tt.want)
			}
		})
	}
}

func TestMatches_ExcludeRoutineSignIns(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	events := map[string]Event{
		"routine sign-in":   {Timestamp: at, Type: LocalSignInType, Severity: "info", Source: "user"},
		"unset severity":    {Timestamp: at, Type: LocalSignInType, Source: "user"},
		"break-glass":       {Timestamp: at, Type: LocalSignInType, Severity: "warning", Source: "user"},
		"error sign-in":     {Timestamp: at, Type: LocalSignInType, Severity: "error", Source: "user"},
		"failed sign-in":    {Timestamp: at, Type: "auth.local.login_failed", Source: "user"},
		"other audit event": {Timestamp: at, Type: "settings.updated", Severity: "info", Source: "user"},
	}

	for name, e := range events {
		if !Matches(e, Query{}) {
			t.Errorf("%s: a query without the flag must match", name)
		}
	}

	q := Query{ExcludeRoutineSignIns: true}
	want := map[string]bool{
		"routine sign-in":   false,
		"unset severity":    false,
		"break-glass":       true,
		"error sign-in":     true,
		"failed sign-in":    true,
		"other audit event": true,
	}
	for name, e := range events {
		if got := Matches(e, q); got != want[name] {
			t.Errorf("%s: Matches with the flag = %v, want %v", name, got, want[name])
		}
	}
}

package mitev1

import "testing"

func TestCommandTypeLabel(t *testing.T) {
	cases := map[CommandType]string{
		CommandTypeTriggerBuild: "trigger_build",
		CommandTypeCancelBuild:  "cancel_build",
		CommandTypeUnspecified:  "unknown",
		CommandType(99):         "unknown",
	}
	for in, want := range cases {
		if got := in.Label(); got != want {
			t.Errorf("CommandType(%d).Label() = %q, want %q", in, got, want)
		}
	}
}

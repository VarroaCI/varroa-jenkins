package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
)

// TestIsOperatorTimeout checks which relay errors mean that the operator did
// not answer in time.
func TestIsOperatorTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"reply timeout", fmt.Errorf("forge pullrequest request: %w", nats.ErrTimeout), true},
		{"no responders", fmt.Errorf("forge issue request: %w", nats.ErrNoResponders), true},
		{"caller deadline", fmt.Errorf("forge issue request: %w", context.DeadlineExceeded), true},
		{"other error", errors.New("boom"), false},
		{"caller canceled", context.Canceled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOperatorTimeout(tc.err); got != tc.want {
				t.Fatalf("isOperatorTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

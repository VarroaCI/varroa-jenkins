package mite

import (
	"fmt"
	"strings"
	"testing"
)

// TestNewServer_RequiresStreamHandler asserts that a Server without a
// StreamHandler fails at construction, before it can accept a stream.
func TestNewServer_RequiresStreamHandler(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewServer accepted a nil StreamHandler")
		}
		if !strings.Contains(fmt.Sprint(r), "StreamHandler") {
			t.Fatalf("panic = %v, want a message that names the StreamHandler", r)
		}
	}()
	NewServer(nil, nil, nil)
}

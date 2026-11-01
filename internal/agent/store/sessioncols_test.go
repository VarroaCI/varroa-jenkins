package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// TestSessionColsCutsAtFirstMessageScan pins the SQL cut to the Go scan bound,
// so the store and TrimFirstMessage agree on how much text a first message reads.
func TestSessionColsCutsAtFirstMessageScan(t *testing.T) {
	t.Parallel()
	want := fmt.Sprintf("left(e.payload->>'text', %d)", agent.FirstMessageScan)
	if !strings.Contains(sessionCols, want) {
		t.Fatalf("sessionCols does not contain %q", want)
	}
}

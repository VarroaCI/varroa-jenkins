package blob

import (
	"strings"
	"testing"
)

func TestLogKey(t *testing.T) {
	got := LogKey("01996f3e-1e11-7c00-8f21-0242ac110002")
	want := "logs/01996f3e-1e11-7c00-8f21-0242ac110002"
	if got != want {
		t.Fatalf("LogKey: got %q, want %q", got, want)
	}
}

func TestLogIndexKey(t *testing.T) {
	got := LogIndexKey("01996f3e-1e11-7c00-8f21-0242ac110002")
	want := "logindex/01996f3e-1e11-7c00-8f21-0242ac110002"
	if got != want {
		t.Fatalf("LogIndexKey: got %q, want %q", got, want)
	}
}

func TestRunStateKey(t *testing.T) {
	got := RunStateKey("01996f3e-1e11-7c00-8f21-0242ac110002", 1737000000000, 4, "jobs/payments/jobs/api/builds/7/program.dat")
	want := "runstate/01996f3e-1e11-7c00-8f21-0242ac110002/1737000000000/4/jobs/payments/jobs/api/builds/7/program.dat"
	if got != want {
		t.Fatalf("RunStateKey: got %q, want %q", got, want)
	}
}

// TestRunStateKeyCarriesEpochAndGeneration proves the two properties the
// pipeline mesh needs from this key: two pushes of one file never share
// a key, and one prefix delete still covers every push.
//
// The generation separates two pushes of one process. A gateway rollout
// leaves two replicas that forward chunks of one process, so the epoch
// alone leaves those writes on one key.
func TestRunStateKeyCarriesEpochAndGeneration(t *testing.T) {
	const runID = "01996f3e-1e11-7c00-8f21-0242ac110002"
	const relPath = "jobs/payments/jobs/api/builds/7/program.dat"

	first := RunStateKey(runID, 1737000000000, 4, relPath)
	second := RunStateKey(runID, 1737000099999, 4, relPath)
	third := RunStateKey(runID, 1737000000000, 5, relPath)
	if first == second {
		t.Fatalf("two epochs share one key: %q", first)
	}
	if first == third {
		t.Fatalf("two generations share one key: %q", first)
	}

	prefix := RunStatePrefix(runID)
	for _, key := range []string{first, second, third} {
		if !strings.HasPrefix(key, prefix) {
			t.Fatalf("key %q is outside the prefix %q", key, prefix)
		}
	}
}

func TestRunStatePrefix(t *testing.T) {
	got := RunStatePrefix("01996f3e-1e11-7c00-8f21-0242ac110002")
	want := "runstate/01996f3e-1e11-7c00-8f21-0242ac110002/"
	if got != want {
		t.Fatalf("RunStatePrefix: got %q, want %q", got, want)
	}
}

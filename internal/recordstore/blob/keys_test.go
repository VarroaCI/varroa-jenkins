package blob

import "testing"

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

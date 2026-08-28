package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The operator remints the bootstrap Secret when the token expires while the
// pod is still initializing (long plugin pulls routinely outlive the 15-minute
// TTL), and kubelet refreshes the mounted file. A mite that caches the value
// it read at startup can never pick the remint up — register retries must
// observe the CURRENT file contents.
func TestReadBootstrapToken_ObservesFileChanges(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("stale-token\n"), 0600); err != nil {
		t.Fatal(err)
	}

	agent := NewAgent(Config{
		ControllerName: "test",
		Namespace:      "ns",
		BootstrapFile:  tokenFile,
	})

	got, err := agent.readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != "stale-token" {
		t.Fatalf("expected trimmed initial token, got %q", got)
	}

	// Simulate the operator's remint reaching the mounted volume.
	if err := os.WriteFile(tokenFile, []byte("reminted-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = agent.readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken after remint: %v", err)
	}
	if got != "reminted-token" {
		t.Fatalf("expected reminted token on re-read, got %q", got)
	}
}

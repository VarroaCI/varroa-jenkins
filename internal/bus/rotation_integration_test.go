package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// TestConnectSurvivesPasswordRotation rotates a user's password on the
// server and in the mounted file, then proves the client reconnects with
// the new credential instead of giving up after repeated auth errors.
func TestConnectSurvivesPasswordRotation(t *testing.T) {
	s, err := server.NewServer(&server.Options{
		Port:  -1,
		Users: []*server.User{{Username: "operator", Password: "old"}},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server did not start")
	}
	defer s.Shutdown()

	path := filepath.Join(t.TempDir(), "operator-password")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	conn, err := Connect(s.ClientURL(), Config{Username: "operator", PasswordFile: path})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	// Rotate server-side first (what the NATS config reloader does), then the
	// file (what kubelet does, later). In between the client must keep retrying.
	if err := s.ReloadOptions(&server.Options{
		Port:  -1,
		Users: []*server.User{{Username: "operator", Password: "new"}},
	}); err != nil {
		t.Fatalf("ReloadOptions: %v", err)
	}
	// Reloading changed users disconnects existing clients.
	waitFor(t, 5*time.Second, func() bool { return !conn.Connected() })

	// The client is now failing auth with the stale password. Give it more than
	// two attempts (ReconnectWait is 2s in Connect) so the give-up-after-two
	// behaviour would have closed the connection for good.
	time.Sleep(5 * time.Second)
	if conn.nc.IsClosed() {
		t.Fatal("connection closed permanently after auth errors; IgnoreAuthErrorAbort not effective")
	}

	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, conn.Connected)

	// Prove the reconnected session works end to end.
	sub, err := conn.nc.SubscribeSync("rotation.probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.nc.Publish("rotation.probe", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("publish after rotation: %v", err)
	}
}

// TestConnectWaitsForRotatedPasswordAtStartup covers the restart path: a pod
// that starts while its mounted Secret still holds the pre-rotation password
// must wait for the Secret to catch up, not exit and crash-loop.
func TestConnectWaitsForRotatedPasswordAtStartup(t *testing.T) {
	s, err := server.NewServer(&server.Options{
		Port:  -1,
		Users: []*server.User{{Username: "operator", Password: "new"}},
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server did not start")
	}
	defer s.Shutdown()

	// The file still holds the password the server has already rotated away
	// from, exactly as a freshly scheduled pod sees it.
	path := filepath.Join(t.TempDir(), "operator-password")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		conn *Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := Connect(s.ClientURL(), Config{
			Username:       "operator",
			PasswordFile:   path,
			StartupTimeout: 30 * time.Second,
		})
		done <- result{conn, err}
	}()

	// Connect must still be waiting rather than have returned an error.
	select {
	case r := <-done:
		if r.conn != nil {
			r.conn.Close()
		}
		t.Fatalf("Connect returned early (err=%v); it must wait for the credential to land", r.err)
	case <-time.After(2 * time.Second):
	}

	// kubelet syncs the rotated Secret.
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Connect: %v", r.err)
		}
		defer r.conn.Close()
		if !r.conn.Connected() {
			t.Fatal("Connect returned a Conn that is not connected")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Connect never returned after the password file caught up")
	}
}

// TestConnectStartupTimeout verifies a permanently unreachable bus still fails
// the process rather than hanging forever.
func TestConnectStartupTimeout(t *testing.T) {
	start := time.Now()
	// Port 1 on loopback refuses connections, so every dial attempt fails.
	conn, err := Connect("nats://127.0.0.1:1", Config{
		Username:       "operator",
		Password:       "irrelevant",
		StartupTimeout: 2 * time.Second,
	})
	if err == nil {
		conn.Close()
		t.Fatal("expected Connect to fail against an unreachable server")
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("Connect gave up after %s, before the startup budget elapsed", elapsed)
	}
	if !strings.Contains(err.Error(), "not connected after") {
		t.Fatalf("error should report the exhausted startup budget, got %v", err)
	}
}

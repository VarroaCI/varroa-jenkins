package bundle

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// deadlineReturnBound is the longest a git call may take to return after its
// context deadline. It is shorter than gitWaitDelay, so a pass proves that
// the process-group kill stopped git, not the wait-delay backstop.
const deadlineReturnBound = 4 * time.Second

// silentRemote is a TCP listener on 127.0.0.1 that accepts connections and
// never writes. An https git remote on it never completes the TLS handshake.
type silentRemote struct {
	url    string
	closed chan struct{}
}

func newSilentRemote(t *testing.T) *silentRemote {
	t.Helper()
	if !gitGroupKill {
		t.Skip("the deadline tests need the process-group kill, which this platform does not have")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	// Keep the test hermetic: no proxy and no user or system git config.
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	r := &silentRemote{
		url:    "https://" + ln.Addr().String() + "/repo.git",
		closed: make(chan struct{}, 16),
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						select {
						case r.closed <- struct{}{}:
						default:
						}
						return
					}
				}
			}()
		}
	}()
	// Cleanup closes the server side too, so that it does not wait for a
	// transport helper that a failed kill left running. The accept loop ends
	// before the sweep, so no wg.Add runs during wg.Wait and no connection
	// escapes the sweep.
	t.Cleanup(func() {
		_ = ln.Close()
		<-acceptDone
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return r
}

// assertPeerClosed checks that the git transport helper closed its
// connection, which proves that the kill reached the child process.
func (r *silentRemote) assertPeerClosed(t *testing.T) {
	t.Helper()
	select {
	case <-r.closed:
	case <-time.After(2 * time.Second):
		t.Error("the remote connection is still open: the git transport helper is still running")
	}
}

// assertStoppedAtDeadline checks that a git call returned soon after its
// deadline with an error that wraps context.DeadlineExceeded.
func assertStoppedAtDeadline(t *testing.T, err error, elapsed time.Duration) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not wrap context.DeadlineExceeded: %v", err)
	}
	if elapsed > deadlineReturnBound {
		t.Errorf("call returned after %v, want at most %v", elapsed, deadlineReturnBound)
	}
}

func TestClone_StopsAtContextDeadline(t *testing.T) {
	tests := []struct {
		name     string
		revision string
	}{
		// A branch name stops in ls-remote.
		{name: "branch", revision: "main"},
		// A pinned SHA skips ls-remote and stops in the bare fetch.
		{name: "pinned SHA", revision: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := newSilentRemote(t)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			start := time.Now()
			_, err := NewGitCloner().Clone(ctx, remote.url, tt.revision, filepath.Join(t.TempDir(), "clone"), nil)
			assertStoppedAtDeadline(t, err, time.Since(start))
			remote.assertPeerClosed(t)
		})
	}
}

func TestRemoteSHA_StopsAtContextDeadline(t *testing.T) {
	remote := newSilentRemote(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := NewGitCloner().RemoteSHA(ctx, remote.url, "main", nil)
	assertStoppedAtDeadline(t, err, time.Since(start))
	remote.assertPeerClosed(t)
}

func TestCloneCacheCheckout_StopsAtContextDeadline(t *testing.T) {
	tests := []struct {
		name     string
		revision string
	}{
		// A branch name stops in ls-remote.
		{name: "branch", revision: "main"},
		// A pinned SHA skips ls-remote and stops in the bare fetch.
		{name: "pinned SHA", revision: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := newSilentRemote(t)
			c := newCacheForTest(t, t.TempDir(), 10, 1000)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			start := time.Now()
			_, _, err := c.Checkout(ctx, remote.url, tt.revision, filepath.Join(t.TempDir(), "out"), nil)
			assertStoppedAtDeadline(t, err, time.Since(start))
			remote.assertPeerClosed(t)

			c.mu.Lock()
			n := len(c.locks)
			c.mu.Unlock()
			if n != 0 {
				t.Errorf("cache holds %d repo locks after the call, want 0", n)
			}
		})
	}
}

func TestCloneCacheCheckout_LockWaitHonorsContext(t *testing.T) {
	c := newCacheForTest(t, t.TempDir(), 10, 1000)
	bareURL, commit := newBareFixture(t)
	sha := commit("a.txt", "a")

	// Hold the repo lock, as a Checkout with a stuck fetch does.
	key := cacheKey(bareURL)
	held := c.perRepoLock(key)
	if !held.tryLock() {
		t.Fatal("repo lock is unexpectedly held")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := c.Checkout(ctx, bareURL, sha, filepath.Join(t.TempDir(), "out"), nil)
	assertStoppedAtDeadline(t, err, time.Since(start))
	if err != nil && !strings.Contains(err.Error(), "wait for git cache lock") {
		t.Errorf("error does not name the lock wait: %v", err)
	}

	held.unlock()
	c.releaseRepoLock(key)
	c.mu.Lock()
	n := len(c.locks)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("cache holds %d repo locks after both callers left, want 0", n)
	}

	// The lock is usable again after the waiter gave up.
	_, _, err = c.Checkout(context.Background(), bareURL, sha, filepath.Join(t.TempDir(), "out2"), nil)
	must(t, err)
}

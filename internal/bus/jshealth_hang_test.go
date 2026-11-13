package bus

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/varroaci/varroa-jenkins/internal/bus/bustest"
)

// hangConn reports one accepted connection. closed closes when the client
// closes its end.
type hangConn struct {
	closed <-chan struct{}
}

// hangingNATSListener accepts connections and sends INFO only after delay.
// It answers PING with PONG, so a dial that waits for the handshake succeeds
// late. Each accepted connection is reported on accepted, with a channel that
// closes when the client closes its end.
func hangingNATSListener(t *testing.T, delay time.Duration) (addr string, accepted chan hangConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted = make(chan hangConn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			closed := make(chan struct{})
			accepted <- hangConn{closed: closed}
			go func() {
				defer close(closed)
				time.Sleep(delay)
				_, _ = conn.Write([]byte(`INFO {"server_id":"hang","server_name":"hang","version":"2.14.7","proto":1,"max_payload":1048576}` + "\r\n"))
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 && strings.Contains(string(buf[:n]), "PING") {
						_, _ = conn.Write([]byte("PONG\r\n"))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), accepted
}

// hangEnv is a fake jsEnv whose probe connection dials through dial. The
// probe fails on the main connection and names another server, so each
// threshold reaches probeOther.
type hangEnv struct {
	*fakeJSEnv
	ctx     context.Context
	dial    func() (*nats.Conn, error)
	dialing atomic.Bool
}

func (e *hangEnv) probeOther() (string, string, error) {
	e.dialing.Store(true)
	return connJSEnv{ctx: e.ctx, c: &Conn{dialProbe: e.dial}}.probeOther()
}

// TestProbeOtherReturnsOnCancelAndClosesLateDial proves a probe dial that
// hangs does not hold the loop, and that a connection which completes late is
// closed.
func TestProbeOtherReturnsOnCancelAndClosesLateDial(t *testing.T) {
	addr, accepted := hangingNATSListener(t, 1500*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := connJSEnv{ctx: ctx, c: &Conn{dialProbe: func() (*nats.Conn, error) {
		return nats.Connect("nats://"+addr, nats.Timeout(10*time.Second), nats.NoReconnect())
	}}}

	done := make(chan error, 1)
	go func() {
		_, _, err := e.probeOther()
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probeOther error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("probeOther did not return within one second of cancel")
	}

	// The dial completes about 1.5 s later. The server side then sees the
	// client close the connection.
	var hc hangConn
	select {
	case hc = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("no connection reached the listener")
	}
	select {
	case <-hc.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("late connection was not closed within five seconds")
	}
}

// TestCloseReturnsWhileProbeDialHangs proves Close does not wait for a probe
// dial that has not returned.
func TestCloseReturnsWhileProbeDialHangs(t *testing.T) {
	addr, _ := hangingNATSListener(t, 1500*time.Millisecond)
	dial := func() (*nats.Conn, error) {
		return nats.Connect("nats://"+addr, nats.Timeout(10*time.Second), nats.NoReconnect())
	}
	c := &Conn{}
	ctx, cancel := context.WithCancel(context.Background())
	c.jsCancel = cancel
	c.jsDone = make(chan struct{})
	env := &hangEnv{fakeJSEnv: &fakeJSEnv{probeErr: errors.New("no responders"), otherID: "s2"}, ctx: ctx, dial: dial}
	go c.runJSHealth(ctx, 10*time.Millisecond, env)
	bustest.Eventually(t, "the loop enters the hanging probe dial", env.dialing.Load)

	start := time.Now()
	c.stopJSHealth()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stopJSHealth took %s while a probe dial hangs, want under one second", elapsed)
	}
}

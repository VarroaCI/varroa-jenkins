package bus

import (
	"errors"
	"slices"
	"testing"
)

// fakeJSEnv replaces the NATS connection. The server it reports is always
// "s1" with the name "nats-1".
type fakeJSEnv struct {
	disconnected bool
	probeErr     error
	otherID      string
	otherErr     error

	probes     int
	others     int
	reconnects int
}

func (f *fakeJSEnv) connected() bool { return !f.disconnected }

func (f *fakeJSEnv) server() (string, string) { return "s1", "nats-1" }

func (f *fakeJSEnv) probe() error {
	f.probes++
	return f.probeErr
}

func (f *fakeJSEnv) probeOther() (string, string, error) {
	f.others++
	return f.otherID, "nats-2", f.otherErr
}

func (f *fakeJSEnv) forceReconnect() error {
	f.reconnects++
	return nil
}

func newTestJSMonitor() *jsMonitor {
	return &jsMonitor{failures: JSProbeFailures}
}

func TestJSMonitorHealthyServerNeverProbesOther(t *testing.T) {
	env := &fakeJSEnv{}
	m := newTestJSMonitor()
	for range 10 {
		m.step(env)
	}
	if env.others != 0 || env.reconnects != 0 {
		t.Fatalf("others=%d reconnects=%d, want 0 and 0", env.others, env.reconnects)
	}
}

func TestJSMonitorMovesWhenAnotherServerAnswers(t *testing.T) {
	env := &fakeJSEnv{probeErr: errors.New("no responders"), otherID: "s2"}
	m := newTestJSMonitor()
	for range JSProbeFailures - 1 {
		if m.step(env) {
			t.Fatal("moved before the failure threshold")
		}
	}
	if env.others != 0 {
		t.Fatalf("others = %d before the threshold, want 0", env.others)
	}
	if !m.step(env) {
		t.Fatal("step did not move at the threshold")
	}
	if env.others != 1 || env.reconnects != 1 {
		t.Fatalf("others=%d reconnects=%d, want 1 and 1", env.others, env.reconnects)
	}
	// After a move the count is at failures-1, so the next failure checks again.
	if !m.step(env) {
		t.Fatal("second move did not happen")
	}
	if env.others != 2 || env.reconnects != 2 {
		t.Fatalf("others=%d reconnects=%d, want 2 and 2", env.others, env.reconnects)
	}
}

func TestJSMonitorKeepsConnectionWhenNoServerAnswers(t *testing.T) {
	env := &fakeJSEnv{probeErr: errors.New("no responders"), otherID: "s2", otherErr: errors.New("dial failed")}
	m := newTestJSMonitor()
	for range 3 * JSProbeFailures {
		m.step(env)
	}
	if env.others != 3 || env.reconnects != 0 {
		t.Fatalf("others=%d reconnects=%d, want 3 and 0", env.others, env.reconnects)
	}
}

func TestJSMonitorKeepsConnectionWhenDialFails(t *testing.T) {
	env := &fakeJSEnv{probeErr: errors.New("no responders"), otherID: "", otherErr: errors.New("dial failed")}
	m := newTestJSMonitor()
	for range 2 * JSProbeFailures {
		m.step(env)
	}
	if env.reconnects != 0 {
		t.Fatalf("reconnects = %d, want 0", env.reconnects)
	}
}

func TestJSMonitorKeepsConnectionOnSameServer(t *testing.T) {
	env := &fakeJSEnv{probeErr: errors.New("no responders"), otherID: "s1"}
	m := newTestJSMonitor()
	for range 2 * JSProbeFailures {
		m.step(env)
	}
	if env.reconnects != 0 {
		t.Fatalf("reconnects = %d, want 0", env.reconnects)
	}
}

func TestJSMonitorSkipsWhileDisconnected(t *testing.T) {
	env := &fakeJSEnv{disconnected: true, probeErr: errors.New("no responders"), otherID: "s2"}
	m := newTestJSMonitor()
	for range 5 {
		m.step(env)
	}
	if env.probes != 0 {
		t.Fatalf("probes = %d while disconnected, want 0", env.probes)
	}
	env.disconnected = false
	m.step(env)
	if env.others != 0 {
		t.Fatalf("others = %d after one failed step, want 0", env.others)
	}
}

func TestJSMonitorSuccessResetsCount(t *testing.T) {
	env := &fakeJSEnv{probeErr: errors.New("no responders"), otherID: "s2"}
	m := newTestJSMonitor()
	m.step(env)
	env.probeErr = nil
	m.step(env)
	env.probeErr = errors.New("no responders")
	m.step(env)
	if env.others != 0 {
		t.Fatalf("others = %d, want 0: a success must reset the count", env.others)
	}
}

func TestProbeServers(t *testing.T) {
	tests := []struct {
		name      string
		busURL    string
		connected string
		want      []string
	}{
		{"one entry", "nats://varroa-nats:4222", "nats://varroa-nats:4222", []string{"nats://varroa-nats:4222"}},
		{"connected first", "tls://a:30422,tls://b:30422", "tls://a:30422", []string{"tls://b:30422"}},
		{"connected last", "tls://a:30422,tls://b:30422", "tls://b:30422", []string{"tls://a:30422"}},
		{"three keep order", "tls://a:1,tls://b:1,tls://c:1", "tls://b:1", []string{"tls://a:1", "tls://c:1"}},
		{"default port", "tls://a,tls://b:4222", "tls://a:4222", []string{"tls://b:4222"}},
		{"no scheme", "a:4222,b:4222", "tls://a:4222", []string{"b:4222"}},
		{"spaces and slash", " tls://a:1/ , tls://b:1 ,", "tls://a:1", []string{"tls://b:1"}},
		{"not connected", "tls://a:1,tls://b:1", "", []string{"tls://a:1", "tls://b:1"}},
		{"only the connected host", "tls://a:1,tls://a:1", "tls://a:1", []string{"tls://a:1", "tls://a:1"}},
		{"empty", "", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeServers(tc.busURL, tc.connected); !slices.Equal(got, tc.want) {
				t.Fatalf("probeServers(%q, %q) = %q, want %q", tc.busURL, tc.connected, got, tc.want)
			}
		})
	}
}

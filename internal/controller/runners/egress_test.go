package runners

import (
	"context"
	"errors"
	"testing"
)

// stubResolver returns addrs unless err is set, and records whether it was
// called at all — proving an IP host never reaches the resolver.
func stubResolver(addrs []string, err error, called *bool) hostResolver {
	return func(_ context.Context, _ string) ([]string, error) {
		*called = true
		return addrs, err
	}
}

func withResolver(t *testing.T, r hostResolver) {
	t.Helper()
	orig := lookupHost
	lookupHost = r
	t.Cleanup(func() { lookupHost = orig })
}

// TestRunnerEgressPeers_IPHostSkipsResolver proves an already-parsable IP
// host needs no lookup: design D11's ipBlock case.
func TestRunnerEgressPeers_IPHostSkipsResolver(t *testing.T) {
	called := false
	withResolver(t, stubResolver(nil, errors.New("must not be called"), &called))

	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "10.20.0.5"

	peers := runnerEgressPeers(context.Background(), runner)
	if called {
		t.Fatal("resolver was called for an IP host")
	}
	if len(peers) != 1 || peers[0] != (RunnerEgressPeer{IP: "10.20.0.5", Port: defaultPort}) {
		t.Fatalf("peers = %+v, want one peer at 10.20.0.5:%d", peers, defaultPort)
	}
}

// TestRunnerEgressPeers_ExplicitPort proves a configured spec.port
// overrides the default.
func TestRunnerEgressPeers_ExplicitPort(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "10.20.0.5"
	port := 2222
	runner.Spec.Port = &port

	peers := runnerEgressPeers(context.Background(), runner)
	if len(peers) != 1 || peers[0].Port != 2222 {
		t.Fatalf("peers = %+v, want port 2222", peers)
	}
}

// TestRunnerEgressPeers_ResolvesOneAddress is task 6.2's first acceptance:
// an injected resolver answering with one address yields one peer.
func TestRunnerEgressPeers_ResolvesOneAddress(t *testing.T) {
	called := false
	withResolver(t, stubResolver([]string{"10.30.0.9"}, nil, &called))

	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "ci-runner.example.internal"

	peers := runnerEgressPeers(context.Background(), runner)
	if !called {
		t.Fatal("resolver was not called for a hostname")
	}
	if len(peers) != 1 || peers[0] != (RunnerEgressPeer{IP: "10.30.0.9", Port: defaultPort}) {
		t.Fatalf("peers = %+v, want one peer at 10.30.0.9:%d", peers, defaultPort)
	}
}

// TestRunnerEgressPeers_ResolvesSeveralAddresses is task 6.2's second
// acceptance: one peer per resolved address, sharing the runner's port.
func TestRunnerEgressPeers_ResolvesSeveralAddresses(t *testing.T) {
	called := false
	withResolver(t, stubResolver([]string{"10.30.0.9", "10.30.0.10", "10.30.0.11"}, nil, &called))

	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "ci-runner.example.internal"
	port := 22222
	runner.Spec.Port = &port

	peers := runnerEgressPeers(context.Background(), runner)
	if !called {
		t.Fatal("resolver was not called for a hostname")
	}
	want := []RunnerEgressPeer{
		{IP: "10.30.0.9", Port: 22222},
		{IP: "10.30.0.10", Port: 22222},
		{IP: "10.30.0.11", Port: 22222},
	}
	if len(peers) != len(want) {
		t.Fatalf("peers = %+v, want %+v", peers, want)
	}
	for i := range want {
		if peers[i] != want[i] {
			t.Errorf("peers[%d] = %+v, want %+v", i, peers[i], want[i])
		}
	}
}

// TestRunnerEgressPeers_ResolutionFailureYieldsNoPeer is task 6.2's third
// acceptance: a resolution failure fails closed for this one runner and
// produces no peer, never an error the caller must handle.
func TestRunnerEgressPeers_ResolutionFailureYieldsNoPeer(t *testing.T) {
	called := false
	withResolver(t, stubResolver(nil, errors.New("no such host"), &called))

	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "does-not-resolve.example.internal"

	peers := runnerEgressPeers(context.Background(), runner)
	if !called {
		t.Fatal("resolver was not called for a hostname")
	}
	if len(peers) != 0 {
		t.Fatalf("peers = %+v, want none on a resolution failure", peers)
	}
}

// TestRunnerEgressPeers_EmptyAnswerYieldsNoPeer proves a name that
// resolves to nothing (no error, zero addresses) also yields no rule.
func TestRunnerEgressPeers_EmptyAnswerYieldsNoPeer(t *testing.T) {
	called := false
	withResolver(t, stubResolver(nil, nil, &called))

	runner := sshRunner("win-1", "payments")
	runner.Spec.Host = "empty-answer.example.internal"

	peers := runnerEgressPeers(context.Background(), runner)
	if !called {
		t.Fatal("resolver was not called for a hostname")
	}
	if len(peers) != 0 {
		t.Fatalf("peers = %+v, want none when the name resolves to nothing", peers)
	}
}

// TestRunnerEgressPeers_InboundRunnerContributesNoPeer proves an inbound
// runner never opens a cell egress rule: it dials the agent door, not the
// cell's outbound network.
func TestRunnerEgressPeers_InboundRunnerContributesNoPeer(t *testing.T) {
	called := false
	withResolver(t, stubResolver([]string{"10.30.0.9"}, nil, &called))

	peers := runnerEgressPeers(context.Background(), inboundRunner())
	if called {
		t.Fatal("resolver was called for an inbound runner")
	}
	if len(peers) != 0 {
		t.Fatalf("peers = %+v, want none for an inbound runner", peers)
	}
}

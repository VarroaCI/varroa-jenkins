package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
)

// withPoolLibraryLookup replaces poolLibraryLookupHost for one test and
// restores it when the test ends.
func withPoolLibraryLookup(t *testing.T, fn func(ctx context.Context, host string) ([]string, error)) {
	t.Helper()
	orig := poolLibraryLookupHost
	poolLibraryLookupHost = fn
	t.Cleanup(func() { poolLibraryLookupHost = orig })
}

func TestPoolLibraryRemoteHostPort(t *testing.T) {
	tests := []struct {
		remote string
		host   string
		port   int
		wantOK bool
	}{
		{"https://github.com/o/r.git", "github.com", 443, true},
		{"https://user@git.example.com:8443/r.git", "git.example.com", 8443, true},
		{"ssh://git@host.example:2222/r.git", "host.example", 2222, true},
		{"ssh://git@host.example/r.git", "host.example", 22, true},
		{"git@github.com:o/r.git", "github.com", 22, true},
		{"https://[2001:db8::1]/r.git", "2001:db8::1", 443, true},
		{"https://10.0.0.7/r.git", "10.0.0.7", 443, true},
		{"https://${HOST}/r", "", 0, false},
		{"ssh://host:0/r", "", 0, false},
		{"ssh://host:99999/r", "", 0, false},
		{"https:///r", "", 0, false},
		{"notaurl", "", 0, false},
	}
	for _, tt := range tests {
		host, port, ok := poolLibraryRemoteHostPort(tt.remote)
		if ok != tt.wantOK || host != tt.host || port != tt.port {
			t.Errorf("poolLibraryRemoteHostPort(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tt.remote, host, port, ok, tt.host, tt.port, tt.wantOK)
		}
	}
}

func TestPoolLibraryEgressPeers_IPHostSkipsResolver(t *testing.T) {
	withPoolLibraryLookup(t, func(context.Context, string) ([]string, error) {
		t.Fatal("resolver must not be called for an IP literal host")
		return nil, nil
	})
	libs := []v1alpha1.CellPoolLibrary{{Name: "lib", Remote: "ssh://git@10.40.0.2:2222/lib.git"}}
	got := poolLibraryEgressPeers(context.Background(), libs)
	want := []runners.RunnerEgressPeer{{IP: "10.40.0.2", Port: 2222}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("peers = %+v, want %+v", got, want)
	}
}

func TestPoolLibraryEgressPeers_ResolvesSeveralAddresses(t *testing.T) {
	withPoolLibraryLookup(t, func(_ context.Context, host string) ([]string, error) {
		if host != "git.example" {
			t.Errorf("lookup host = %q, want git.example", host)
		}
		return []string{"10.30.0.9", "10.30.0.8"}, nil
	})
	libs := []v1alpha1.CellPoolLibrary{{Name: "lib", Remote: "https://git.example/lib.git"}}
	got := poolLibraryEgressPeers(context.Background(), libs)
	want := []runners.RunnerEgressPeer{{IP: "10.30.0.8", Port: 443}, {IP: "10.30.0.9", Port: 443}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("peers = %+v, want %+v (sorted)", got, want)
	}
}

func TestPoolLibraryEgressPeers_DeduplicatesSharedHost(t *testing.T) {
	withPoolLibraryLookup(t, func(context.Context, string) ([]string, error) {
		return []string{"10.30.0.9"}, nil
	})
	libs := []v1alpha1.CellPoolLibrary{
		{Name: "a", Remote: "ssh://git@git.example:2222/a.git"},
		{Name: "b", Remote: "ssh://git@git.example:2222/b.git"},
	}
	got := poolLibraryEgressPeers(context.Background(), libs)
	if len(got) != 1 || got[0] != (runners.RunnerEgressPeer{IP: "10.30.0.9", Port: 2222}) {
		t.Fatalf("peers = %+v, want one peer 10.30.0.9:2222", got)
	}
}

func TestPoolLibraryEgressPeers_LookupFailureSkipsOnlyThatLibrary(t *testing.T) {
	withPoolLibraryLookup(t, func(_ context.Context, host string) ([]string, error) {
		if host == "down.example" {
			return nil, errors.New("no such host")
		}
		return []string{"10.30.0.9"}, nil
	})
	libs := []v1alpha1.CellPoolLibrary{
		{Name: "down", Remote: "https://down.example/lib.git"},
		{Name: "up", Remote: "https://up.example/lib.git"},
	}
	got := poolLibraryEgressPeers(context.Background(), libs)
	if len(got) != 1 || got[0] != (runners.RunnerEgressPeer{IP: "10.30.0.9", Port: 443}) {
		t.Fatalf("peers = %+v, want only the resolvable library's peer", got)
	}
}

func TestPoolLibraryEgressPeers_SkipsUnparsableRemote(t *testing.T) {
	withPoolLibraryLookup(t, func(context.Context, string) ([]string, error) {
		t.Fatal("resolver must not be called for an unparsable remote")
		return nil, nil
	})
	libs := []v1alpha1.CellPoolLibrary{{Name: "bad", Remote: "https://${HOST}/lib.git"}}
	if got := poolLibraryEgressPeers(context.Background(), libs); len(got) != 0 {
		t.Fatalf("peers = %+v, want none", got)
	}
}

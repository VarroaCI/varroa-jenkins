package runners

import (
	"context"
	"net"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// hostResolver matches net.Resolver.LookupHost's signature. A test
// substitutes it so runnerEgressPeers needs no DNS access.
type hostResolver func(ctx context.Context, host string) ([]string, error)

// lookupHost is the resolver runnerEgressPeers calls to turn an ssh
// runner's hostname into one or more addresses (design D11). Tests replace
// it with a fixed answer.
var lookupHost hostResolver = net.DefaultResolver.LookupHost

// runnerEgressPeers resolves one runner into the cell NetworkPolicy egress
// peers design D11 requires. Only an ssh runner produces a peer: an
// inbound runner dials out through the agent door, not through a cell
// egress rule, so it contributes none.
//
// spec.host that already parses as an IP address needs no lookup and
// yields exactly one peer. Otherwise the name is resolved during this
// call, and one peer is written per resolved address. A resolution
// failure, or a name that resolves to nothing, yields no peer for this one
// runner: the failure closes for that runner, never for the whole policy.
// A resulting unreachable node shows up as Offline on the liveness
// probe's own next tick; this function writes no status.
func runnerEgressPeers(ctx context.Context, runner *v1alpha1.Runner) []RunnerEgressPeer {
	if runner.Spec.Launcher != launcherSSH {
		return nil
	}

	port := defaultPort
	if runner.Spec.Port != nil {
		port = *runner.Spec.Port
	}

	host := runner.Spec.Host
	if net.ParseIP(host) != nil {
		return []RunnerEgressPeer{{IP: host, Port: port}}
	}

	addrs, err := lookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	peers := make([]RunnerEgressPeer, 0, len(addrs))
	for _, addr := range addrs {
		peers = append(peers, RunnerEgressPeer{IP: addr, Port: port})
	}
	return peers
}

package runners

import (
	"context"
	"fmt"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// RunnerContribution is one cell's whole runner output for one desired-state
// assembly pass. The three parts travel together because the node list and
// the egress rules must describe the same runner set.
type RunnerContribution struct {
	// Nodes is one jenkins.nodes entry (design D3) per delivered runner
	// placed on this cell.
	Nodes []any
	// Credentials is one SYSTEM-scope global credential (design D5) per
	// delivered ssh runner placed on this cell. Filled in once the SSH
	// credential Secret projection reads the Team's agent namespace.
	Credentials []any
	// EgressPeers is one resolved network destination per delivered ssh
	// runner placed on this cell, for the cell NetworkPolicy egress builder
	// (design D11). A runner whose host does not resolve contributes none.
	EgressPeers []RunnerEgressPeer
}

// RunnerEgressPeer is one resolved address an ssh runner's cell must be
// able to dial. IP is a single resolved address, never a hostname: a
// NetworkPolicy egress peer accepts only a CIDR.
type RunnerEgressPeer struct {
	IP   string
	Port int
}

// BuildContribution reads every Runner and Team through backend on every
// call, keeps the runners whose owning Team is placed on cell, applies the
// same delivery rule the Runner ticker's tick steps 4 to 6 apply, and
// renders the delivered set through the shared renderer this package owns.
//
// It performs one fresh read per call. No cache is kept, because the
// Runner ticker is leader-elected while the Controller reconciler that
// calls this runs on every replica: a leader-filled cache would make a
// non-leader render an empty node list and delete the cell's runners. A
// deleted or moved runner is simply absent from the next call's read, so
// its node disappears with no bookkeeping.
func BuildContribution(ctx context.Context, backend crdstore.Backend, secrets SecretReader, cell *v1alpha1.Controller) (RunnerContribution, error) {
	teams, err := crdstore.List[v1alpha1.Team](ctx, backend, "", "")
	if err != nil {
		return RunnerContribution{}, fmt.Errorf("runners: list teams: %w", err)
	}
	runnerList, err := crdstore.List[v1alpha1.Runner](ctx, backend, "", "")
	if err != nil {
		return RunnerContribution{}, fmt.Errorf("runners: list runners: %w", err)
	}

	teamsByName := make(map[string]*v1alpha1.Team, len(teams))
	for _, t := range teams {
		teamsByName[t.Name] = t
	}

	var contrib RunnerContribution
	for _, runner := range runnerList {
		team := teamsByName[runner.Spec.Team]
		eval := evaluateRunner(runner, team)
		if !eval.Delivered {
			continue
		}
		if eval.Cell != cell.Name || eval.CellNamespace != cell.Namespace {
			continue
		}
		if runner.Spec.Launcher == launcherSSH {
			// Tick step 7: an ssh runner whose credential is missing or
			// malformed gets no node, no credential, and no egress peer.
			agentNamespace := ""
			if team != nil && team.Spec.Pipelines != nil {
				agentNamespace = team.Spec.Pipelines.AgentNamespace
			}
			cred, fault := readCredential(ctx, secrets, runner, agentNamespace)
			if fault != nil {
				continue
			}
			contrib.Credentials = append(contrib.Credentials, renderCredential(runner, cred))
		}
		contrib.Nodes = append(contrib.Nodes, renderNode(runner))
		contrib.EgressPeers = append(contrib.EgressPeers, runnerEgressPeers(ctx, runner)...)
	}
	return contrib, nil
}

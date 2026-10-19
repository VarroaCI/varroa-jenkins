// Package accept decides which TeamAgent objects run for a pipelines Team.
// It is the one rule that maps an object to its principal and its single
// accepted agent. The operator, the BFF, and the recorder call it, so they
// agree on who may act. The package imports only the API types.
package accept

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// PrincipalPrefix starts the subject of every Team agent. No other
// credential may carry it.
const PrincipalPrefix = "agent:"

// Credential constants of a Team agent token. The token package mints and
// the verifier checks with these values only.
const (
	TokenAudience = "varroa-team-agent"
	TokenIssuer   = "varroa-agent"
	TokenKeyID    = "team-agent-v1"
	TokenLifetime = 15 * time.Minute
	// MCPPath is the only path a Team agent token may reach.
	MCPPath = "/api/v1/mcp"
)

// Reasons that a verdict can carry. The values equal the condition reasons
// of the API package.
const (
	ReasonNamespaceNotOwned = v1alpha1.TeamAgentReasonNamespaceNotOwned
	ReasonDuplicate         = v1alpha1.TeamAgentReasonDuplicate
)

// Input is the state that Decide reads.
type Input struct {
	// TeamAgents lists every TeamAgent object seen by the caller.
	TeamAgents []v1alpha1.TeamAgent
	// Teams maps a Team name to the core-held Team object. A Team that is
	// absent here is handled by the release-namespace rule.
	Teams map[string]*v1alpha1.Team
	// ReleaseNamespace is the namespace of the core install.
	ReleaseNamespace string
}

// Verdict is the outcome for one TeamAgent object.
type Verdict struct {
	// Accepted is true when the object is the agent of its Team.
	Accepted bool
	// Reason is empty when Accepted is true. Otherwise it names why.
	Reason string
}

// Decide returns a verdict for every object in in. Decide ignores
// spec.enabled. A disabled object stays accepted, so its binding stays.
func Decide(in Input) map[types.NamespacedName]Verdict {
	out := make(map[types.NamespacedName]Verdict, len(in.TeamAgents))
	candidates := map[string][]*v1alpha1.TeamAgent{}
	for i := range in.TeamAgents {
		ta := &in.TeamAgents[i]
		key := types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}
		if !owns(in, ta) {
			out[key] = Verdict{Reason: ReasonNamespaceNotOwned}
			continue
		}
		team := ta.Spec.TeamRef.Name
		candidates[team] = append(candidates[team], ta)
	}
	for _, list := range candidates {
		slices.SortFunc(list, byAge)
		for i, ta := range list {
			key := types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}
			if i == 0 {
				out[key] = Verdict{Accepted: true}
				continue
			}
			out[key] = Verdict{Reason: ReasonDuplicate}
		}
	}
	return out
}

// owns reports whether the object's namespace may hold an agent of its Team.
func owns(in Input, ta *v1alpha1.TeamAgent) bool {
	if team, ok := in.Teams[ta.Spec.TeamRef.Name]; ok && team != nil {
		return slices.Contains(team.Spec.Namespaces, ta.Namespace)
	}
	return ta.Namespace == in.ReleaseNamespace
}

// byAge orders objects oldest first. A tie goes to the lower namespace/name.
func byAge(a, b *v1alpha1.TeamAgent) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return cmp.Compare(a.Name, b.Name)
}

// Principal returns the subject of a Team agent: agent:<ns>/<team>/<name>.
func Principal(ta *v1alpha1.TeamAgent) string {
	return PrincipalPrefix + ta.Namespace + "/" + ta.Spec.TeamRef.Name + "/" + ta.Name
}

// IsPrincipal reports whether a subject belongs to a Team agent.
func IsPrincipal(subject string) bool {
	return strings.HasPrefix(subject, PrincipalPrefix)
}

// Enabled reports whether the object is switched on. A nil value means on.
func Enabled(ta *v1alpha1.TeamAgent) bool {
	return ta.Spec.Enabled == nil || *ta.Spec.Enabled
}

// AcceptedFor returns the accepted object of a Team. The second result is
// false when no object of that Team is accepted.
func AcceptedFor(in Input, team string) (*v1alpha1.TeamAgent, bool) {
	verdicts := Decide(in)
	for i := range in.TeamAgents {
		ta := &in.TeamAgents[i]
		if ta.Spec.TeamRef.Name != team {
			continue
		}
		if v := verdicts[types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}]; v.Accepted {
			return ta, true
		}
	}
	return nil, false
}

// OutputKinds lists the outputs that are on, in the order note, issue,
// webhook.
func OutputKinds(o v1alpha1.TeamAgentOutputs) []string {
	var kinds []string
	if o.Notes {
		kinds = append(kinds, v1alpha1.TeamAgentOutputNote)
	}
	if o.Issues != nil {
		kinds = append(kinds, v1alpha1.TeamAgentOutputIssue)
	}
	if o.Webhook {
		kinds = append(kinds, v1alpha1.TeamAgentOutputWebhook)
	}
	return kinds
}

// OutputToolNames maps output kinds to the MCP tool names that serve them.
// An unknown kind is dropped.
func OutputToolNames(kinds []string) []string {
	var names []string
	for _, k := range kinds {
		switch k {
		case v1alpha1.TeamAgentOutputNote:
			names = append(names, "post_note")
		case v1alpha1.TeamAgentOutputIssue:
			names = append(names, "open_issue")
		case v1alpha1.TeamAgentOutputWebhook:
			names = append(names, "send_webhook")
		}
	}
	return names
}

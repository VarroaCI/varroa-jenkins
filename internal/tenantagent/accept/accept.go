// Package accept decides which TenantAgent objects run for a pipelines Tenant.
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

// PrincipalPrefix starts the subject of every Tenant agent. No other
// credential may carry it.
const PrincipalPrefix = "agent:"

// Credential constants of a Tenant agent token. The token package mints and
// the verifier checks with these values only.
const (
	TokenAudience = "varroa-tenant-agent"
	TokenIssuer   = "varroa-agent"
	TokenKeyID    = "tenant-agent-v1"
	TokenLifetime = 15 * time.Minute
	// MCPPath is the only path a Tenant agent token may reach.
	MCPPath = "/api/v1/mcp"
)

// Reasons that a verdict can carry. The values equal the condition reasons
// of the API package.
const (
	ReasonNamespaceNotOwned = v1alpha1.TenantAgentReasonNamespaceNotOwned
	ReasonDuplicate         = v1alpha1.TenantAgentReasonDuplicate
)

// Input is the state that Decide reads.
type Input struct {
	// TenantAgents lists every TenantAgent object seen by the caller.
	TenantAgents []v1alpha1.TenantAgent
	// Tenants maps a Tenant name to the core-held Tenant object. A Tenant that is
	// absent here is handled by the release-namespace rule.
	Tenants map[string]*v1alpha1.Tenant
	// ReleaseNamespace is the namespace of the core install.
	ReleaseNamespace string
}

// Verdict is the outcome for one TenantAgent object.
type Verdict struct {
	// Accepted is true when the object is the agent of its Tenant.
	Accepted bool
	// Reason is empty when Accepted is true. Otherwise it names why.
	Reason string
}

// Decide returns a verdict for every object in in. Decide ignores
// spec.enabled. A disabled object stays accepted, so its binding stays.
func Decide(in Input) map[types.NamespacedName]Verdict {
	out := make(map[types.NamespacedName]Verdict, len(in.TenantAgents))
	candidates := map[string][]*v1alpha1.TenantAgent{}
	for i := range in.TenantAgents {
		ta := &in.TenantAgents[i]
		key := types.NamespacedName{Namespace: ta.Namespace, Name: ta.Name}
		if !owns(in, ta) {
			out[key] = Verdict{Reason: ReasonNamespaceNotOwned}
			continue
		}
		tenant := ta.Spec.TenantRef.Name
		candidates[tenant] = append(candidates[tenant], ta)
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

// owns reports whether the object's namespace may hold an agent of its Tenant.
func owns(in Input, ta *v1alpha1.TenantAgent) bool {
	if tenant, ok := in.Tenants[ta.Spec.TenantRef.Name]; ok && tenant != nil {
		return slices.Contains(tenant.Spec.Namespaces, ta.Namespace)
	}
	return ta.Namespace == in.ReleaseNamespace
}

// byAge orders objects oldest first. A tie goes to the lower namespace/name.
func byAge(a, b *v1alpha1.TenantAgent) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return cmp.Compare(a.Name, b.Name)
}

// Principal returns the subject of a Tenant agent: agent:<ns>/<tenant>/<name>.
func Principal(ta *v1alpha1.TenantAgent) string {
	return PrincipalPrefix + ta.Namespace + "/" + ta.Spec.TenantRef.Name + "/" + ta.Name
}

// IsPrincipal reports whether a subject belongs to a Tenant agent.
func IsPrincipal(subject string) bool {
	return strings.HasPrefix(subject, PrincipalPrefix)
}

// Enabled reports whether the object is switched on. A nil value means on.
func Enabled(ta *v1alpha1.TenantAgent) bool {
	return ta.Spec.Enabled == nil || *ta.Spec.Enabled
}

// AcceptedFor returns the accepted object of a Tenant. The second result is
// false when no object of that Tenant is accepted.
func AcceptedFor(in Input, tenant string) (*v1alpha1.TenantAgent, bool) {
	verdicts := Decide(in)
	for i := range in.TenantAgents {
		ta := &in.TenantAgents[i]
		if ta.Spec.TenantRef.Name != tenant {
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
func OutputKinds(o v1alpha1.TenantAgentOutputs) []string {
	var kinds []string
	if o.Notes {
		kinds = append(kinds, v1alpha1.TenantAgentOutputNote)
	}
	if o.Issues != nil {
		kinds = append(kinds, v1alpha1.TenantAgentOutputIssue)
	}
	if o.Webhook {
		kinds = append(kinds, v1alpha1.TenantAgentOutputWebhook)
	}
	return kinds
}

// OutputToolNames maps output kinds to the MCP tool names that serve them.
// An unknown kind is dropped.
func OutputToolNames(kinds []string) []string {
	var names []string
	for _, k := range kinds {
		switch k {
		case v1alpha1.TenantAgentOutputNote:
			names = append(names, "post_note")
		case v1alpha1.TenantAgentOutputIssue:
			names = append(names, "open_issue")
		case v1alpha1.TenantAgentOutputWebhook:
			names = append(names, "send_webhook")
		}
	}
	return names
}

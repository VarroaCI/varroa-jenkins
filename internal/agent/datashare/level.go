// Package datashare decides how much of the platform's data a provider may
// receive. A session's level is the lowest of the user's level, the tenant's
// level, and the provider's ceiling. The package also filters tool results
// and hides the tools above the level.
package datashare

import (
	"encoding/json"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
)

// Rank orders the data-sharing levels: none 0, metadata 1, logs 2. An
// invalid level ranks 0.
func Rank(l agent.DataSharing) int {
	switch l {
	case agent.DataMetadata:
		return 1
	case agent.DataLogs:
		return 2
	}
	return 0
}

// Min returns the lowest of the levels. An invalid level counts as none.
// Min with no levels returns none.
func Min(levels ...agent.DataSharing) agent.DataSharing {
	if len(levels) == 0 {
		return agent.DataNone
	}
	lowest := levels[0]
	for _, l := range levels[1:] {
		if Rank(l) < Rank(lowest) {
			lowest = l
		}
	}
	if !lowest.Valid() {
		return agent.DataNone
	}
	return lowest
}

// Ceiling returns the highest level a provider may receive. An internal
// provider may receive logs. An external provider may receive the level
// external, and an invalid external level gives none.
func Ceiling(trust agent.ProviderTrust, external agent.DataSharing) agent.DataSharing {
	if trust == agent.TrustInternal {
		return agent.DataLogs
	}
	if !external.Valid() {
		return agent.DataNone
	}
	return external
}

// TenantLevel returns the level that a tenant grants. An empty tenant, or a
// tenant with no entry, grants logs. An invalid entry grants none.
func TenantLevel(levels map[string]agent.DataSharing, tenant string) agent.DataSharing {
	if tenant == "" {
		return agent.DataLogs
	}
	level, ok := levels[tenant]
	if !ok {
		return agent.DataLogs
	}
	if !level.Valid() {
		return agent.DataNone
	}
	return level
}

// Inputs are the values that decide a session's level.
type Inputs struct {
	User            agent.DataSharing
	Tenant          string
	TenantLevels    map[string]agent.DataSharing
	Trust           agent.ProviderTrust
	ExternalCeiling agent.DataSharing
}

// Effective returns the session's level. An empty user level counts as logs.
func Effective(in Inputs) agent.DataSharing {
	user := in.User
	if user == "" {
		user = agent.DataLogs
	}
	return Min(user, TenantLevel(in.TenantLevels, in.Tenant), Ceiling(in.Trust, in.ExternalCeiling))
}

// Visible returns the tools that the model may see at the level. Every
// non-read tool stays. A read tool needs the level of its data class.
func Visible(level agent.DataSharing, tools []agent.ToolDef) []agent.ToolDef {
	out := make([]agent.ToolDef, 0, len(tools))
	for _, t := range tools {
		if policy.Class(t) != agent.ClassRead {
			out = append(out, t)
			continue
		}
		// A metadata class needs metadata; logs and unclassified reads need logs.
		class, known := ClassOf(t.Name)
		metadata := known && (class == ClassMetadata || class == ClassMetadataFiltered)
		if (metadata && Rank(level) >= 1) || (!metadata && Rank(level) == 2) {
			out = append(out, t)
		}
	}
	return out
}

// Filter removes the fields that a level may not see from a tool result. A
// get_run result below logs loses digest.logTail. Other results, and results
// that are not JSON objects, come back unchanged.
func Filter(tool string, level agent.DataSharing, content string) string {
	if tool != "get_run" || Rank(level) >= 2 {
		return content
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return content
	}
	raw, ok := obj["digest"]
	if !ok {
		return content
	}
	var digest map[string]json.RawMessage
	if err := json.Unmarshal(raw, &digest); err != nil {
		return content
	}
	if _, ok := digest["logTail"]; !ok {
		return content
	}
	delete(digest, "logTail")
	cleaned, err := json.Marshal(digest)
	if err != nil {
		return content
	}
	obj["digest"] = cleaned
	out, err := json.Marshal(obj)
	if err != nil {
		return content
	}
	return string(out)
}

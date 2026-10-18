// Package factories maps each provider kind to its adapter factory. It lives
// apart from the provider package, because the adapters import the provider
// package.
package factories

import (
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/anthropic"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/openaicompat"
)

// DefaultFactories returns the adapter factory of every provider kind.
func DefaultFactories() map[agent.ProviderKind]provider.Factory {
	return map[agent.ProviderKind]provider.Factory{
		agent.ProviderAnthropic:        anthropic.Factory,
		agent.ProviderOpenAICompatible: openaicompat.Factory,
	}
}

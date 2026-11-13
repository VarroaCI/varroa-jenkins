package api

import (
	"log/slog"

	"github.com/varroaci/varroa-jenkins/internal/agent/sealedkey"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/assistantconfig"
)

// NewAssistantRuntime wraps the session store so every write publishes its
// notice. The caller sets HostFor before the first run.
func NewAssistantRuntime(live *assistantconfig.Live, st store.Store, sealer *sealedkey.Sealer, owner, cluster string, publish func(subject string, data []byte) error, logger *slog.Logger) *AssistantRuntime {
	return &AssistantRuntime{
		Live:    live,
		Store:   store.WithNotify(st, publish, logger),
		Sealer:  sealer,
		Owner:   owner,
		Cluster: cluster,
		Publish: publish,
		Logger:  logger,
	}
}

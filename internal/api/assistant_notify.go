package api

import (
	"encoding/json"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// assistantPublisher publishes one message on a bus subject.
type assistantPublisher func(subject string, data []byte) error

// publishControl publishes the control notice after a kill switch write or an
// unattended grant write. Each varroa-agent replica wakes its status ticker on it.
func publishControl(pub assistantPublisher, paused bool, by string, at time.Time) error {
	if pub == nil {
		return nil
	}
	body, err := json.Marshal(map[string]any{"paused": paused, "updated_by": by, "updated_at": at.UTC()})
	if err != nil {
		return err
	}
	return pub(bus.AssistantControlSubject, body)
}

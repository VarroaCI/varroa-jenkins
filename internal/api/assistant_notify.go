package api

import (
	"encoding/json"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// assistantPublisher publishes one message on a bus subject.
type assistantPublisher func(subject string, data []byte) error

// publishControl announces a change of the global kill switch.
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

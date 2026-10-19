package api

import (
	"encoding/json"
	"testing"
	"time"
)

type recordedPub struct {
	subjects []string
	bodies   [][]byte
}

func (r *recordedPub) publish(subject string, data []byte) error {
	r.subjects = append(r.subjects, subject)
	r.bodies = append(r.bodies, data)
	return nil
}

// TestPublishControl_Shape proves the kill-switch notice names the switch and
// its author only.
func TestPublishControl_Shape(t *testing.T) {
	pub := &recordedPub{}
	if err := publishControl(pub.publish, true, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	if pub.subjects[0] != "varroa.assistant.control" {
		t.Errorf("subject = %q", pub.subjects[0])
	}
	var body map[string]any
	if err := json.Unmarshal(pub.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if body["paused"] != true || body["updated_by"] != "admin" {
		t.Errorf("body = %v", body)
	}
}

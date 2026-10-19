package store

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
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

// TestNotify_ContentFree proves a write publishes exactly four keys, with no
// transcript text, on the session subject.
func TestNotify_ContentFree(t *testing.T) {
	ctx := context.Background()
	inner := NewMemory(Options{Now: time.Now})
	pub := &recordedPub{}
	st := WithNotify(inner, pub.publish, slog.New(slog.NewTextHandler(io.Discard, nil)))

	id, err := agent.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secret := "PRIVATE-TRANSCRIPT-TEXT"
	user, err := agent.NewEvent(agent.EventUser, agent.UserPayload{Text: secret})
	if err != nil {
		t.Fatal(err)
	}
	sess := agent.Session{
		ID: id, Kind: agent.KindAssistant, Principal: "u", Cluster: "core", Team: "",
		Trigger: agent.TriggerManual, Provider: "p", Model: "m",
		KeySource: agent.KeySourceChart, DataSharing: agent.DataLogs,
		State: agent.StateRunning, LeaseOwner: "owner-1",
	}
	if _, _, err := st.CreateSession(ctx, sess, []agent.EventInput{user}); err != nil {
		t.Fatal(err)
	}
	if len(pub.subjects) != 1 || pub.subjects[0] != "agentsessions.core._none" {
		t.Fatalf("subjects = %v, want [agentsessions.core._none]", pub.subjects)
	}
	var body map[string]any
	if err := json.Unmarshal(pub.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 4 {
		t.Errorf("notice has %d keys, want 4: %v", len(body), body)
	}
	for _, k := range []string{"session_id", "kind", "state", "seq"} {
		if _, ok := body[k]; !ok {
			t.Errorf("notice lacks %q", k)
		}
	}
	if strings.Contains(string(pub.bodies[0]), secret) {
		t.Errorf("notice carries transcript text: %s", pub.bodies[0])
	}
}

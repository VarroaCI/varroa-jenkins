package controller

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestReplayAckDecision(t *testing.T) {
	cases := []struct {
		status int
		want   replayAction
	}{
		{200, ackReplay},
		{201, ackReplay},
		{202, ackReplay},
		{299, ackReplay},
		{0, nakReplay},   // transport-level failure (no HTTP response)
		{429, nakReplay}, // rate limited
		{500, nakReplay},
		{503, nakReplay},
		{400, termReplay}, // bad request
		{401, termReplay}, // unauthorized (bad signature)
		{404, termReplay},
		{422, termReplay},
	}
	for _, tc := range cases {
		if got := replayAckDecision(tc.status); got != tc.want {
			t.Errorf("replayAckDecision(%d) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

func TestReplayWaiters_Correlation(t *testing.T) {
	w := &replayWaiters{chans: make(map[string]chan *mitev1.CommandResult)}

	ch := w.register("cmd-7")
	// A result for a different command_id must not be delivered here.
	w.deliver(&mitev1.CommandResult{CommandId: "cmd-other", HttpStatus: 500})
	select {
	case <-ch:
		t.Fatal("received a result for the wrong command_id")
	default:
	}

	// The matching result is delivered.
	w.deliver(&mitev1.CommandResult{CommandId: "cmd-7", HttpStatus: 200})
	select {
	case res := <-ch:
		if res.HttpStatus != 200 {
			t.Errorf("got status %d, want 200", res.HttpStatus)
		}
	default:
		t.Fatal("matching result was not delivered")
	}

	// After unregister, delivery is a no-op (and must not panic).
	w.unregister("cmd-7")
	w.deliver(&mitev1.CommandResult{CommandId: "cmd-7", HttpStatus: 200})
}

// TestWebhookEnvelope_MatchesBFFShape guards the operator's envelope decoder
// against the exact JSON the BFF writes (internal/api/handlers_hibernation.go).
func TestWebhookEnvelope_MatchesBFFShape(t *testing.T) {
	raw := `{
		"method": "POST",
		"path": "github-webhook/",
		"query": "",
		"headers": {"X-GitHub-Event": "push", "Content-Type": "application/json"},
		"bodyB64": "` + base64.StdEncoding.EncodeToString([]byte(`{"ref":"refs/heads/main"}`)) + `",
		"receivedAt": "2026-07-06T00:00:00Z",
		"cluster": "core",
		"namespace": "team-a",
		"controller": "demo"
	}`
	var env webhookEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Method != "POST" || env.Path != "github-webhook/" || env.Controller != "demo" || env.Namespace != "team-a" {
		t.Errorf("decoded envelope mismatch: %+v", env)
	}
	body, err := base64.StdEncoding.DecodeString(env.BodyB64)
	if err != nil || string(body) != `{"ref":"refs/heads/main"}` {
		t.Errorf("body decode = %q (err %v)", body, err)
	}
	if env.Headers["X-GitHub-Event"] != "push" {
		t.Errorf("headers not decoded: %+v", env.Headers)
	}
}

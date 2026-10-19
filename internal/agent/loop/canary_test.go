package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/openaicompat"
	"github.com/varroaci/varroa-jenkins/internal/agent/provider/providertest"
)

const canaryKey = "CANARY-PROV-7f3a91c2"

// TestCanaryKeyStaysInAdapter runs a full session through the real adapter
// against a scripted endpoint. The provider key may appear only in the
// Authorization header of the provider call. It must not reach the stored
// transcript, the log output, or the recorded request bodies.
func TestCanaryKeyStaysInAdapter(t *testing.T) {
	srv := providertest.New(providertest.Script{Turns: []providertest.Turn{
		{ToolCalls: []providertest.ScriptCall{{ID: "c1", Name: "get_run", Arguments: json.RawMessage(`{"run":"r1"}`)}}},
		{Text: "The stage failed."},
	}})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	f := newFixture(t)
	adapter := openaicompat.New(ts.URL+"/v1", ts.Client())
	var logs bytes.Buffer
	f.cfg.Provider = adapter
	f.cfg.Key = provider.NewKey([]byte(canaryKey))
	f.cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	f.cfg.Model = "varroa-smoke"

	if err := Run(context.Background(), f.cfg, TriggerStart); err != nil {
		t.Fatal(err)
	}
	if s := f.session(t); s.State != agent.StateCompleted {
		t.Fatalf("state = %s, want completed", s.State)
	}

	evs, err := f.st.Events(context.Background(), f.id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if strings.Contains(string(e.Payload), canaryKey) {
			t.Errorf("provider key in event %s: %s", e.Type, e.Payload)
		}
	}
	if strings.Contains(logs.String(), canaryKey) {
		t.Errorf("provider key in logs: %s", logs.String())
	}

	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("no provider request was recorded")
	}
	sawHeader := false
	for _, r := range reqs {
		if strings.Contains(string(r.Body), canaryKey) {
			t.Errorf("provider key in a request body")
		}
		if strings.Contains(r.Header.Get("Authorization"), canaryKey) {
			sawHeader = true
		}
	}
	if !sawHeader {
		t.Error("the key never reached the Authorization header")
	}
}

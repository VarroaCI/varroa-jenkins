package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent/provider/providertest"
)

// TestSelfTestTwoTurnsBothShapes starts the server on a loopback port and
// replays two turns in each shape.
func TestSelfTestTwoTurnsBothShapes(t *testing.T) {
	t.Parallel()
	script := providertest.Script{Turns: []providertest.Turn{
		{Text: "first"},
		{Text: "second", ToolCalls: []providertest.ScriptCall{{ID: "c", Name: "get_run", Arguments: []byte(`{"run":"r"}`)}}},
		{Text: "third"},
		{Text: "fourth"},
	}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = serve(l, providertest.New(script)) }()
	t.Cleanup(func() { _ = l.Close() })

	base := "http://" + l.Addr().String()
	want := []struct{ path, marker string }{
		{"/v1/messages", "message_stop"},
		{"/v1/messages", "message_stop"},
		{"/v1/chat/completions", "[DONE]"},
		{"/v1/chat/completions", "[DONE]"},
	}
	for i, w := range want {
		resp, err := http.Post(base+w.path, "application/json", strings.NewReader(`{"model":"fake"}`))
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), w.marker) {
			t.Fatalf("turn %d: status %d, body %q; want %s", i, resp.StatusCode, body, w.marker)
		}
	}
}

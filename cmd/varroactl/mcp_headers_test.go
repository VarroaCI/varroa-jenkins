package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// stateless2026 is the protocol version that needs the standard headers.
const stateless2026 = "2026-07-28"

// metaMessage builds a JSON-RPC message whose params carry the protocol
// version in _meta, as a 2026-07-28 client sends it.
func metaMessage(method, version string, params string) []byte {
	return []byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":%q,"params":{%s"_meta":{%q:%q}}}`,
		method, params, mcp.MetaKeyProtocolVersion, version))
}

func TestMCPProtocolHeaders(t *testing.T) {
	cases := []struct {
		name string
		msg  []byte
		want map[string]string
	}{
		{
			name: "2026-07-28 tools/call",
			msg:  metaMessage("tools/call", stateless2026, `"name":"get_me","arguments":{},`),
			want: map[string]string{
				"Mcp-Protocol-Version": stateless2026,
				"Mcp-Method":           "tools/call",
				"Mcp-Name":             "get_me",
			},
		},
		{
			name: "2026-07-28 server/discover",
			msg:  metaMessage("server/discover", stateless2026, ""),
			want: map[string]string{
				"Mcp-Protocol-Version": stateless2026,
				"Mcp-Method":           "server/discover",
			},
		},
		{
			name: "initialize",
			msg:  []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`),
		},
		{
			name: "_meta version 2025-11-25",
			msg:  metaMessage("tools/list", "2025-11-25", ""),
		},
		{
			name: "no params",
			msg:  []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
		},
		{
			name: "response",
			msg:  []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`),
		},
		{
			name: "text that is not JSON",
			msg:  []byte("not json"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpProtocolHeaders(tc.msg)
			if len(got) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("header %s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestMCP_StatelessMessageCarriesHeaders pins that the bridge forwards a
// 2026-07-28 message with the headers that the endpoint checks.
func TestMCP_StatelessMessageCarriesHeaders(t *testing.T) {
	version = "test"
	var (
		mu      sync.Mutex
		seen    int
		hdrErr  error
		gotVers string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &msg)
		mu.Lock()
		seen++
		hdrErr = mcp.ValidateStandardHeaders(r.Header.Get, r.Header.Get(mcp.HeaderProtocolVersion), mcp.MCPMethod(msg.Method), msg.Params)
		gotVers = r.Header.Get(mcp.HeaderProtocolVersion)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()

	stdinR, stdinW, _ := os.Pipe()
	stdoutR, stdoutW, _ := os.Pipe()
	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin = stdinR
	os.Stdout = stdoutW

	var output bytes.Buffer
	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		_, _ = io.Copy(&output, stdoutR)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		root := newRootCmd()
		root.SetArgs([]string{"mcp", "--server", srv.URL})
		root.SetContext(ctx)
		_ = root.Execute()
		os.Stdin = origStdin
		os.Stdout = origStdout
	}()

	msg := metaMessage("tools/call", stateless2026, `"name":"get_me","arguments":{},`)
	_, _ = fmt.Fprintln(stdinW, string(msg))
	time.Sleep(200 * time.Millisecond)

	_ = stdinW.Close()
	<-done
	_ = stdoutW.Close()
	readWg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if seen == 0 {
		t.Fatal("the fake endpoint saw no request")
	}
	if hdrErr != nil {
		t.Errorf("endpoint refuses the forwarded headers: %v", hdrErr)
	}
	if gotVers != stateless2026 {
		t.Errorf("Mcp-Protocol-Version = %q, want %q", gotVers, stateless2026)
	}
}

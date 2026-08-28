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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// MCP bridge tests — each test manages its own OS pipes
// ---------------------------------------------------------------------------

func TestMCP_JSONResponse(t *testing.T) {
	version = "test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"ok"}`)
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

	_, _ = fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	time.Sleep(200 * time.Millisecond)

	_ = stdinW.Close()
	<-done
	_ = stdoutW.Close()
	readWg.Wait()

	if !strings.Contains(output.String(), "jsonrpc") {
		t.Errorf("expected JSON-RPC response, got: %s", output.String())
	}
}

func TestMCP_SSEResponse(t *testing.T) {
	version = "test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":\"a\"}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":\"b\"}\n\n")
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

	_, _ = fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	time.Sleep(200 * time.Millisecond)

	_ = stdinW.Close()
	<-done
	_ = stdoutW.Close()
	readWg.Wait()

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected >=2 lines, got %d", len(lines))
	}
}

func TestMCP_202NoOutput(t *testing.T) {
	version = "test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
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

	_, _ = fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping"}`)
	time.Sleep(200 * time.Millisecond)

	_ = stdinW.Close()
	<-done
	_ = stdoutW.Close()
	readWg.Wait()

	if strings.TrimSpace(output.String()) != "" {
		t.Errorf("expected no output for 202, got: %q", output.String())
	}
}

func TestMCP_403ErrorWithID7(t *testing.T) {
	version = "test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"error":"forbidden"}`)
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

	_, _ = fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	time.Sleep(200 * time.Millisecond)

	_ = stdinW.Close()
	<-done
	_ = stdoutW.Close()
	readWg.Wait()

	if !strings.Contains(output.String(), `"id":7`) {
		t.Errorf("expected id:7 in error, got: %s", output.String())
	}
}

func TestMCP_NotificationErrorNullID(t *testing.T) {
	version = "test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"error":"forbidden"}`)
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

	_, _ = fmt.Fprintln(stdinW, `{"jsonrpc":"2.0","method":"ping"}`)
	time.Sleep(200 * time.Millisecond)

	_ = stdinW.Close()
	<-done
	_ = stdoutW.Close()
	readWg.Wait()

	if !strings.Contains(output.String(), `"id":null`) {
		t.Errorf("expected id:null for notification error, got: %s", output.String())
	}
}

func TestMCP_Concurrency50(t *testing.T) {
	version = "test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &probe)
		time.Sleep(time.Duration(len(body)%10) * 3 * time.Millisecond)
		if probe.ID != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"ok"}`, string(probe.ID))
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
		}
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

	var inputWg sync.WaitGroup
	for i := 0; i < 50; i++ {
		inputWg.Add(1)
		go func(id int) {
			defer inputWg.Done()
			if id%5 == 0 {
				_, _ = fmt.Fprintf(stdinW, `{"jsonrpc":"2.0","method":"ping_%d"}`+"\n", id)
			} else {
				_, _ = fmt.Fprintf(stdinW, `{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", id)
			}
		}(i)
	}
	inputWg.Wait()
	time.Sleep(1 * time.Second)

	_ = stdinW.Close()
	<-done
	_ = stdoutW.Close()
	readWg.Wait()

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")

	var jsonLines, jsonRPCLines atomic.Int32
	var lineWg sync.WaitGroup
	for _, line := range lines {
		line := strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lineWg.Add(1)
		go func(l string) {
			defer lineWg.Done()
			if json.Valid([]byte(l)) {
				jsonLines.Add(1)
			}
			if strings.Contains(l, "jsonrpc") {
				jsonRPCLines.Add(1)
			}
		}(line)
	}
	lineWg.Wait()

	nonEmpty := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}
	if int(jsonLines.Load()) != nonEmpty {
		t.Errorf("expected all %d lines valid JSON, got %d", nonEmpty, jsonLines.Load())
	}
	n := jsonRPCLines.Load()
	if n < 35 || n > 50 {
		t.Errorf("expected ~40 jsonrpc lines (50-10 notifications), got %d", n)
	}
}

func TestMCP_CtrlCInterrupt(t *testing.T) {
	version = "test"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	t.Log("testing Ctrl-C interrupt of MCP bridge")

	stdinR, stdinW, _ := os.Pipe()
	origStdin := os.Stdin
	os.Stdin = stdinR

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		root := newRootCmd()
		root.SetArgs([]string{"mcp", "--server", srv.URL})
		root.SetContext(ctx)
		_ = root.Execute()
		os.Stdin = origStdin
	}()

	_, _ = fmt.Fprintf(stdinW, `{"jsonrpc":"2.0","method":"ping"}`+"\n")
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)

	_ = stdinW.Close()
	<-done
}

// ---------------------------------------------------------------------------
// serve tests
// ---------------------------------------------------------------------------

func TestMCPServe_NonLoopbackRejected(t *testing.T) {
	version = "test"
	root := newRootCmd()
	root.SetArgs([]string{"mcp", "serve", "--listen", "0.0.0.0:0"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usage error for 0.0.0.0")
	}
	if !strings.Contains(err.Error(), "must resolve") {
		t.Errorf("expected 'must resolve' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// rpcError unit tests
// ---------------------------------------------------------------------------

func TestRPCError_WithID(t *testing.T) {
	orig := []byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	result := rpcError(orig, 403, "forbidden")

	var resp struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("rpcError produced invalid JSON: %v", err)
	}
	if string(resp.ID) != "7" {
		t.Errorf("expected id 7, got %s", string(resp.ID))
	}
	if resp.Error.Code != -32603 {
		t.Errorf("expected code -32603, got %d", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "forbidden") {
		t.Errorf("expected 'forbidden' in message, got %s", resp.Error.Message)
	}
}

func TestRPCError_Notification(t *testing.T) {
	orig := []byte(`{"jsonrpc":"2.0","method":"ping"}`)
	result := rpcError(orig, 403, "forbidden")

	var resp struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("rpcError produced invalid JSON: %v", err)
	}
	if string(resp.ID) != "null" {
		t.Errorf("expected id null for notification, got %s", string(resp.ID))
	}
}

// ---------------------------------------------------------------------------
// n1Message unit tests
// ---------------------------------------------------------------------------

func TestN1Message_Parsed(t *testing.T) {
	msg := n1Message([]byte(`{"error":"unauthorized"}`))
	if msg != "unauthorized" {
		t.Errorf("expected 'unauthorized', got %q", msg)
	}
}

func TestN1Message_Truncated(t *testing.T) {
	body := make([]byte, 600)
	for i := range body {
		body[i] = 'x'
	}
	msg := n1Message(body)
	if len(msg) > 520 {
		t.Errorf("expected truncated message <= 512, got %d", len(msg))
	}
}

// ---------------------------------------------------------------------------
// isLoopback tests
// ---------------------------------------------------------------------------

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"localhost", true},
		{"::1", true},
		{"0.0.0.0", false},
	}
	for _, tc := range cases {
		got := isLoopback(tc.host)
		if got != tc.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

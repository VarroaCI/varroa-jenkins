package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBuildAssistant_NoConfigIsOff proves an unset config leaves the assistant off.
func TestBuildAssistant_NoConfigIsOff(t *testing.T) {
	rt, err := buildAssistant(context.Background(), "", "", "core", "", nil, quietLogger())
	if err != nil || rt != nil {
		t.Fatalf("buildAssistant = %v, %v; want nil, nil", rt, err)
	}
}

// TestBuildAssistant_BadConfigIsOff proves a config that does not decode
// leaves the assistant off, and the BFF still starts.
func TestBuildAssistant_BadConfigIsOff(t *testing.T) {
	rt, err := buildAssistant(context.Background(), "{not json", "postgres://unused", "core", "", nil, quietLogger())
	if err != nil || rt != nil {
		t.Fatalf("buildAssistant = %v, %v; want nil, nil", rt, err)
	}
}

// TestBuildAssistant_NoDSNIsOff proves a valid config without a record store
// DSN leaves the assistant off.
func TestBuildAssistant_NoDSNIsOff(t *testing.T) {
	cfg := `{"providers":[],"externalDataSharing":"metadata","budgets":{"session":1,"userDay":1,"tenantDay":1,"steps":1},"approvalExpiry":"1h"}`
	rt, err := buildAssistant(context.Background(), cfg, "", "core", "", nil, quietLogger())
	if err != nil || rt != nil {
		t.Fatalf("buildAssistant = %v, %v; want nil, nil", rt, err)
	}
}

// silentRecordStoreDSN returns a DSN for a server that accepts TCP connections
// and never answers, with a one-second connect timeout.
func silentRecordStoreDSN(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	closed := false
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = c.Close()
				return
			}
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	return "postgres://u:p@" + ln.Addr().String() + "/db?sslmode=disable&connect_timeout=1"
}

// TestBuildAssistant_UnreachableStoreKeepsRuntime proves a record store that
// does not answer at startup leaves the assistant on. The store connects on
// its first call, so the start does not wait for the database.
func TestBuildAssistant_UnreachableStoreKeepsRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := `{"providers":[],"externalDataSharing":"metadata","budgets":{"session":1,"userDay":1,"tenantDay":1,"steps":1},"approvalExpiry":"1h"}`
	start := time.Now()
	rt, err := buildAssistant(ctx, cfg, silentRecordStoreDSN(t), "core", "", nil, quietLogger())
	if err != nil {
		t.Fatalf("buildAssistant error = %v, want nil", err)
	}
	if rt == nil {
		t.Fatal("buildAssistant returned no runtime for an unreachable store, want one")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("buildAssistant took %v, want under 2s", elapsed)
	}
}

// TestLogAssistantStoreStateStaysQuietWhenBFFStops proves that a BFF that stops
// during the startup check logs no store fault.
func TestLogAssistantStoreStateStaysQuietWhenBFFStops(t *testing.T) {
	st, err := store.Open(context.Background(), silentRecordStoreDSN(t), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	logAssistantStoreState(ctx, time.Second, st, slog.New(slog.NewTextHandler(&buf, nil)))
	if buf.Len() != 0 {
		t.Fatalf("log = %q, want none when the BFF stops", buf.String())
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/agent/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeAssistantDyn returns a dynamic client that holds no AssistantConfig object.
func fakeAssistantDyn() dynamic.Interface {
	gvr := v1alpha1.SchemeGroupVersion.WithResource("assistantconfigs")
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "AssistantConfigList"})
}

// TestBuildAssistant_DisabledIsOff proves that a BFF with the flag off builds no assistant.
func TestBuildAssistantOffWhenNotEnabled(t *testing.T) {
	rt, err := buildAssistant(context.Background(), false, "postgres://unused", "core", "", fakeAssistantDyn(), nil, quietLogger())
	if err != nil || rt != nil {
		t.Fatalf("buildAssistant = %v, %v; want nil, nil", rt, err)
	}
}

// TestBuildAssistant_NoDSNIsOff proves that an enabled assistant without a
// record store DSN stays off.
func TestBuildAssistant_NoDSNIsOff(t *testing.T) {
	rt, err := buildAssistant(context.Background(), true, "", "core", "", fakeAssistantDyn(), nil, quietLogger())
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
	start := time.Now()
	rt, err := buildAssistant(ctx, true, silentRecordStoreDSN(t), "core", "", fakeAssistantDyn(), nil, quietLogger())
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

// TestBuildAssistant_StoreCallsStopAtTheDeadline proves that a store call of
// the assistant ends at the per-call bound when the database never answers.
// The test shortens the bound, so it does not run in parallel.
func TestBuildAssistant_StoreCallsStopAtTheDeadline(t *testing.T) {
	saved := assistantStoreCallTimeout
	assistantStoreCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { assistantStoreCallTimeout = saved })

	// connect_timeout is raised so the call ends on the per-call bound, not on
	// the connect timeout.
	dsn := strings.Replace(silentRecordStoreDSN(t), "connect_timeout=1", "connect_timeout=30", 1)
	rt, err := buildAssistant(context.Background(), true, dsn, "core", "", fakeAssistantDyn(), nil, quietLogger())
	if err != nil || rt == nil {
		t.Fatalf("buildAssistant = %v, %v; want a runtime", rt, err)
	}
	start := time.Now()
	_, err = rt.Store.GetSettings(context.Background(), agent.ScopeGlobal)
	if !errors.Is(err, store.ErrUnavailable) {
		t.Fatalf("GetSettings error = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("GetSettings took %v, want under 2s", elapsed)
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

package store

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// silentStoreDSN returns a DSN for a server that accepts TCP connections and
// never answers. The DSN sets a one-second connect timeout.
func silentStoreDSN(t *testing.T) string {
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

// TestOpenUnreachableKeepsStore asserts that Open does not connect. A database
// that does not answer at startup leaves a usable store. Its calls answer
// ErrUnavailable within the connect timeout, and the schema gate stays closed.
func TestOpenUnreachableKeepsStore(t *testing.T) {
	t.Parallel()
	start := time.Now()
	p, err := Open(context.Background(), silentStoreDSN(t), Options{})
	if err != nil {
		t.Fatalf("Open against a silent server: %v", err)
	}
	t.Cleanup(p.Close)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Open took %v, want under 1s", elapsed)
	}

	ctx := context.Background()
	start = time.Now()
	if _, err := p.GetSettings(ctx, agent.ScopeGlobal); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetSettings error = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("GetSettings took %v, want under 5s", elapsed)
	}
	if err := p.CheckSchema(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CheckSchema error = %v, want ErrUnavailable", err)
	}
	if p.schemaOK.Load() {
		t.Fatal("schemaOK is true after a failed schema check")
	}
}

// TestCheckSchemaCallerCancelIsNotUnavailable asserts that a cancelled caller
// context is neither an unreachable store nor an old schema.
func TestCheckSchemaCallerCancelIsNotUnavailable(t *testing.T) {
	t.Parallel()
	p, err := Open(context.Background(), silentStoreDSN(t), Options{})
	if err != nil {
		t.Fatalf("Open against a silent server: %v", err)
	}
	t.Cleanup(p.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = p.CheckSchema(ctx)
	if err == nil {
		t.Fatal("CheckSchema with a cancelled context succeeded")
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrSchemaTooOld) {
		t.Fatalf("CheckSchema error = %v, want neither ErrUnavailable nor ErrSchemaTooOld", err)
	}
}

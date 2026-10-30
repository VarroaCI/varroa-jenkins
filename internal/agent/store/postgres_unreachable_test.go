package store

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestOpenUnreachableFailsFast checks that Open against a server that never
// answers returns an error within the default connect timeout, not the
// kernel's.
func TestOpenUnreachableFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
		}
	}()
	dsn := "postgres://u:p@" + ln.Addr().String() + "/db?sslmode=disable"
	start := time.Now()
	if _, err := Open(context.Background(), dsn, Options{}); err == nil {
		t.Fatal("Open against a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Open took %v, want under 10s", elapsed)
	}
}

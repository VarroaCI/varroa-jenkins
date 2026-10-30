package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// TestOpenRecordStoreReaderStartsWhenUnreachable checks that the BFF gets a
// reader, and returns at once, when the record store never answers at startup.
func TestOpenRecordStoreReaderStartsWhenUnreachable(t *testing.T) {
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

	saved := startupSchemaTimeout
	startupSchemaTimeout = 200 * time.Millisecond
	t.Cleanup(func() { startupSchemaTimeout = saved })

	dsn := "postgres://u:p@" + ln.Addr().String() + "/db?sslmode=disable&connect_timeout=30"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	reader, pool, err := openRecordStoreReader(context.Background(), dsn, 0, logger)
	if err != nil {
		t.Fatalf("openRecordStoreReader error = %v, want nil", err)
	}
	defer pool.Close()
	if reader == nil {
		t.Fatal("openRecordStoreReader returned a nil reader")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("openRecordStoreReader took %v, want under 5s", elapsed)
	}
}

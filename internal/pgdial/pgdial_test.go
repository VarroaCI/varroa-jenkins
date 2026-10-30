package pgdial

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const testDSN = "postgres://u:p@127.0.0.1:1/db?sslmode=disable"

func TestPoolConfigSetsDefaultConnectTimeout(t *testing.T) {
	cfg, err := PoolConfig(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.ConnectTimeout != DefaultConnectTimeout {
		t.Fatalf("ConnectTimeout = %v, want %v", cfg.ConnConfig.ConnectTimeout, DefaultConnectTimeout)
	}
}

func TestPoolConfigKeepsDSNConnectTimeout(t *testing.T) {
	cfg, err := PoolConfig(testDSN + "&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.ConnectTimeout != 2*time.Second {
		t.Fatalf("ConnectTimeout = %v, want 2s", cfg.ConnConfig.ConnectTimeout)
	}
}

func TestConnConfigSetsDefaultConnectTimeout(t *testing.T) {
	cfg, err := ConnConfig(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != DefaultConnectTimeout {
		t.Fatalf("ConnectTimeout = %v, want %v", cfg.ConnectTimeout, DefaultConnectTimeout)
	}
}

func TestApplyDefaultLeavesSetTimeout(t *testing.T) {
	c := &pgconn.Config{ConnectTimeout: 3 * time.Second}
	applyDefault(c)
	if c.ConnectTimeout != 3*time.Second {
		t.Fatalf("ConnectTimeout = %v, want 3s", c.ConnectTimeout)
	}
	c = &pgconn.Config{}
	applyDefault(c)
	if c.ConnectTimeout != DefaultConnectTimeout {
		t.Fatalf("ConnectTimeout = %v, want %v", c.ConnectTimeout, DefaultConnectTimeout)
	}
}

// silentListener accepts connections and never writes, so a client that
// waits for the server hello waits until its own timeout fires.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	return ln.Addr().String()
}

func TestNewPoolSilentServerFailsFast(t *testing.T) {
	addr := silentListener(t)
	dsn := "postgres://u:p@" + addr + "/db?sslmode=disable&connect_timeout=1"
	pool, err := NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err == nil {
		t.Fatal("Ping against a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Ping took %v, want under 2s", elapsed)
	}
}

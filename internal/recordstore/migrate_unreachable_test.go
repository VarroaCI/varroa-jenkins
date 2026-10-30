package recordstore

import (
	"context"
	"testing"
	"time"
)

// TestRunMigrationsUnreachableFailsFast checks that migrations against a
// silent database fail within the default connect timeout, not the kernel's.
func TestRunMigrationsUnreachableFailsFast(t *testing.T) {
	addr := unreachableListener(t)
	dsn := "postgres://u:p@" + addr + "/db?sslmode=disable"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := RunMigrations(ctx, dsn); err == nil {
		t.Fatal("RunMigrations against a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("RunMigrations took %v, want under 10s", elapsed)
	}
}

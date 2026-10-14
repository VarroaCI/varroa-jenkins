package main

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestRestartSourceRetentionFromEnvClamps(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Setenv("VARROA_RETENTION_RESTART_SOURCE", "")
	got, err := restartSourceRetentionFromEnv(365*24*time.Hour, logger)
	if err != nil || got != 7*24*time.Hour {
		t.Fatalf("default = %v, %v; want 7d", got, err)
	}

	t.Setenv("VARROA_RETENTION_RESTART_SOURCE", "7d")
	got, err = restartSourceRetentionFromEnv(5*24*time.Hour, logger)
	if err != nil || got != 5*24*time.Hour {
		t.Fatalf("7d over runs 5d = %v, %v; want the runs window 5d", got, err)
	}

	t.Setenv("VARROA_RETENTION_RESTART_SOURCE", "0d")
	if _, err := restartSourceRetentionFromEnv(365*24*time.Hour, logger); err == nil {
		t.Fatal("0d must not parse")
	}
}

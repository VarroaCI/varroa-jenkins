package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
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

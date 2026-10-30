package api

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestBusConfigBrood_CellLintRefusesLocalCluster asserts the relay refuses the
// local cluster before it sends anything on the bus, so the caller lints its
// own cells on its own resolver.
func TestBusConfigBrood_CellLintRefusesLocalCluster(t *testing.T) {
	f := &busConfigBrood{localCluster: "core"}
	_, err := f.CellLint(context.Background(), "core", bus.CellLintRequest{Cell: "cell-a", Namespace: "cells", Jenkinsfile: "pipeline {}"})
	var brood *BroodError
	if !errors.As(err, &brood) || brood.Code != bus.CodeInternal {
		t.Fatalf("err = %v, want a BroodError with code %q", err, bus.CodeInternal)
	}
}

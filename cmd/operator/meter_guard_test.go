package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// newTestMeterProvider returns a meter provider of the test with a manual
// reader. Pass the provider to the constructor that creates the
// instruments, and collect the values from the reader.
func newTestMeterProvider(t *testing.T) (metric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down test meter provider: %v", err)
		}
	})
	return mp, reader
}

// TestNoTestReplacesGlobalMeterProvider parses the test files of this
// package and fails on a SetMeterProvider call. A test gives each
// metrics constructor its own provider from newTestMeterProvider, so a
// parallel test never sees a provider that another test installs.
func TestNoTestReplacesGlobalMeterProvider(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("list test files: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, pos := range setMeterProviderCalls(file) {
			t.Errorf("%s replaces the global meter provider: pass a provider from newTestMeterProvider to the constructor", fset.Position(pos))
		}
	}
}

// setMeterProviderCalls returns the position of every call to a function
// or method named SetMeterProvider in file.
func setMeterProviderCalls(file *ast.File) []token.Pos {
	var calls []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "SetMeterProvider" {
				calls = append(calls, call.Pos())
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == "SetMeterProvider" {
				calls = append(calls, call.Pos())
			}
		}
		return true
	})
	return calls
}

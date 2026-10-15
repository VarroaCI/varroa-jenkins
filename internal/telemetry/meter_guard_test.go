package telemetry

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	otelImportPath      = "go.opentelemetry.io/otel"
	telemetryImportPath = "github.com/varroaci/varroa-jenkins/internal/telemetry"
)

// TestOnlyMainReadsGlobalMeterProvider fails when a non-test file outside
// a cmd/*/main.go reads the global meter provider through otel.Meter or
// otel.GetMeterProvider. A main reads the provider once and passes it to
// each constructor, so a test gives the constructor its own provider.
// cmd/recorder/run.go is the recorder's main body and reads it there.
func TestOnlyMainReadsGlobalMeterProvider(t *testing.T) {
	t.Parallel()

	walkModuleGoFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		if strings.HasSuffix(rel, "_test.go") || mainMayReadGlobalMeter(rel) {
			return
		}
		for _, pos := range packageCalls(file, otelImportPath, "Meter", "GetMeterProvider") {
			t.Errorf("%s reads the global meter provider: take a metric.MeterProvider in the constructor and let main pass it", fset.Position(pos))
		}
	})
}

// TestNoTestReplacesGlobalMeterProvider fails when a test file calls
// SetMeterProvider or telemetry.InitTelemetry. Both replace the global
// meter provider, so a parallel test can see a provider that another test
// installs. A test gives each constructor its own provider instead.
func TestNoTestReplacesGlobalMeterProvider(t *testing.T) {
	t.Parallel()

	walkModuleGoFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		if !strings.HasSuffix(rel, "_test.go") {
			return
		}
		for _, pos := range namedCalls(file, "SetMeterProvider") {
			t.Errorf("%s replaces the global meter provider: pass a test provider to the constructor", fset.Position(pos))
		}
		for _, pos := range packageCalls(file, telemetryImportPath, "InitTelemetry") {
			t.Errorf("%s calls telemetry.InitTelemetry, which replaces the global meter provider: use telemetry.NewMeterProvider", fset.Position(pos))
		}
		if file.Name.Name == "telemetry" {
			for _, pos := range localCalls(file, "InitTelemetry") {
				t.Errorf("%s calls InitTelemetry, which replaces the global meter provider: use NewMeterProvider", fset.Position(pos))
			}
		}
	})
}

// mainMayReadGlobalMeter reports whether rel is a file that may read the
// global meter provider: a cmd/<binary>/main.go, or cmd/recorder/run.go.
func mainMayReadGlobalMeter(rel string) bool {
	if rel == "cmd/recorder/run.go" {
		return true
	}
	parts := strings.Split(rel, "/")
	return len(parts) == 3 && parts[0] == "cmd" && parts[2] == "main.go"
}

// walkModuleGoFiles parses every Go file of the module and calls fn with
// the slash path relative to the module root. It skips hidden directories,
// testdata, node_modules, vendor, and nested modules.
func walkModuleGoFiles(t *testing.T, fn func(rel string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	fset := token.NewFileSet()
	var paths []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relative path of %s: %v", path, err)
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		fn(filepath.ToSlash(rel), fset, file)
	}
}

// importName returns the name that file binds to importPath, or "" when
// file does not import it. A blank or dot import binds no name.
func importName(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				return ""
			}
			return spec.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// packageCalls returns the position of every call to one of funcs in the
// package that file imports from importPath.
func packageCalls(file *ast.File, importPath string, funcs ...string) []token.Pos {
	name := importName(file, importPath)
	if name == "" {
		return nil
	}
	var calls []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != name {
			return true
		}
		for _, f := range funcs {
			if sel.Sel.Name == f {
				calls = append(calls, call.Pos())
			}
		}
		return true
	})
	return calls
}

// namedCalls returns the position of every call to a function or method
// named name in file.
func namedCalls(file *ast.File, name string) []token.Pos {
	var calls []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == name {
				calls = append(calls, call.Pos())
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == name {
				calls = append(calls, call.Pos())
			}
		}
		return true
	})
	return calls
}

// localCalls returns the position of every call to the unqualified
// function name in file.
func localCalls(file *ast.File, name string) []token.Pos {
	var calls []token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
			calls = append(calls, call.Pos())
		}
		return true
	})
	return calls
}

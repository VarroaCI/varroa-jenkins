package mcp

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every list_* tool must declare an outputSchema.
//
// This is the durable alternative to a hand-maintained coverage table: a new
// list tool fails here until it projects, and no shared file has to be edited
// to register it. It is the list-shape analogue of expectedToolKinds.
// Note what this does NOT check. The declared schemas pair a summary branch
// with an open {"type":"object"} verbose branch under anyOf, so almost any
// object validates against them — that is required (oneOf would reject every
// summary, since a summary satisfies both branches) but it means a schema can
// promise a field the code never emits and no test here will fail. That is the
// dead-`endpoint` defect this phase started by deleting.
//
// Only real data catches it: a field declared in the schema but absent from
// every object in a live listing is dead. The live re-measurement in the plan's
// Task 11 is what closes this, not a unit test.
func TestEveryListToolDeclaresOutputSchema(t *testing.T) {
	// list_jenkins_controllers proxies a Jenkins-side listing rather than CRs.
	// It is already ~61 bytes per item and is not a projection target.
	exempt := map[string]bool{"list_jenkins_controllers": true}

	var missing []string
	for _, tool := range liveTools(t) {
		if !strings.HasPrefix(tool.Name, "list_") || exempt[tool.Name] {
			continue
		}
		if len(tool.OutputSchema) == 0 {
			missing = append(missing, tool.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("list tools declare no outputSchema: %v\n"+
			"project each one as a summary-plus-verbose outputSchema pair "+
			"or add a justified exemption", missing)
	}
}

// resultJSON is the single point where api.SanitizeObject runs, which is what
// makes sanitization global across all 64 tools. A tool that marshals an object
// itself and returns NewToolResultText would ship an unsanitized CR with a
// green suite — the failure mode #467 already cost us once.
//
// The allowlist holds rendered argument expressions rather than tool names:
// matching the expression is precise and needs no walk back up to the enclosing
// registration.
func TestNoToolBypassesResultJSON(t *testing.T) {
	allowed := map[string]bool{
		"buf.String()": true, // get_controller_logs: raw pod logs, not an API object.
	}
	for _, file := range packageFiles(t) {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewToolResultText" || len(call.Args) == 0 {
				return true
			}
			if isLiteralish(call.Args[0]) {
				return true
			}
			var buf strings.Builder
			if err := printer.Fprint(&buf, fset, call.Args[0]); err != nil {
				t.Fatalf("render arg: %v", err)
			}
			if allowed[buf.String()] {
				return true
			}
			t.Errorf("%s:%d returns %s outside resultJSON; route it through "+
				"resultJSON so it is sanitized (or allowlist it with a reason)",
				file, fset.Position(call.Pos()).Line, buf.String())
			return true
		})
	}
}

// Accepting bare identifiers in isLiteralish leaves a hole: assigning a
// marshalled payload to a variable and returning that variable would pass.
// Tracing identifier bindings is not worth the complexity, so this guard closes
// the same bypass from the other end — a tool can only produce an unsanitized
// resource string by marshalling one.
//
// The check is scoped to a single function body rather than to a file. Plenty
// of code here marshals legitimately — resultJSON itself, the preflight error
// path, marshalUnmarshal, proxy.go's outbound JSON-RPC envelope, the schema
// builder, and the handlers that marshal a CR to hand to the brood. What none
// of them do is marshal *and* hand text straight back to the caller. Pairing
// the two within one function is the shape of an actual bypass, and scoping it
// this way needs no allowlist at all, so no infrastructure file has to be
// exempted and then silently stop being checked.
func TestNoToolMarshalsOutsideResultJSON(t *testing.T) {
	for _, file := range packageFiles(t) {
		base := filepath.Base(file)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, body := range functionBodies(parsed) {
			marshalPos, marshals := findMarshal(body)
			if !marshals || !returnsText(body) {
				continue
			}
			t.Errorf("%s:%d marshals JSON in the same function that returns "+
				"NewToolResultText; return the object and let resultJSON marshal "+
				"and sanitize it", base, fset.Position(marshalPos).Line)
		}
	}
}

// functionBodies returns every function body in the file — top-level
// declarations and function literals alike — each with nested literals removed,
// so a handler's body is judged on its own and not on its siblings'. Tool
// handlers are literals inside a register* function, so scoping to the
// declaration alone would smear every handler in a domain together.
func functionBodies(file *ast.File) []*ast.BlockStmt {
	var bodies []*ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncDecl:
			if v.Body != nil {
				bodies = append(bodies, v.Body)
			}
		case *ast.FuncLit:
			bodies = append(bodies, v.Body)
		}
		return true
	})
	return bodies
}

// inspectOwn walks a function body but does not descend into nested function
// literals, which are visited as bodies in their own right.
func inspectOwn(body *ast.BlockStmt, fn func(ast.Node) bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, nested := n.(*ast.FuncLit); nested && n != ast.Node(body) {
			return false
		}
		return fn(n)
	})
}

func findMarshal(body *ast.BlockStmt) (token.Pos, bool) {
	var pos token.Pos
	var found bool
	inspectOwn(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "json" {
			return true
		}
		if sel.Sel.Name == "Marshal" || sel.Sel.Name == "MarshalIndent" {
			if !found {
				pos, found = sel.Pos(), true
			}
		}
		return true
	})
	return pos, found
}

func returnsText(body *ast.BlockStmt) bool {
	var found bool
	inspectOwn(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "NewToolResultText" {
			found = true
		}
		return true
	})
	return found
}

// packageFiles lists every non-test Go file in this package.
//
// TestNoToolBypassesResultJSON consumes all of them, deliberately: proxy.go
// registers tools too, and a NewToolResultText guard blind to it would have a
// hole. TestNoToolMarshalsOutsideResultJSON narrows to tools_*.go for the
// reason given at its own loop.
func packageFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	files := make([]string, 0, len(all))
	for _, f := range all {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no package files found; the guards would vacuously pass")
	}
	return files
}

// isLiteralish reports whether e is a string literal, a plain identifier, a
// call to the strArg argument helper, or a concatenation of those — the shape
// of a human-readable status message rather than a marshalled payload.
//
// strArg must be accepted: sync_catalog_source legitimately returns
// "catalog source sync triggered for " + ns + "/" + strArg(args, "name"), and
// rejecting call expressions outright fails the guard on existing code.
func isLiteralish(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING
	case *ast.Ident:
		return true
	case *ast.BinaryExpr:
		return v.Op == token.ADD && isLiteralish(v.X) && isLiteralish(v.Y)
	case *ast.ParenExpr:
		return isLiteralish(v.X)
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		return ok && id.Name == "strArg"
	default:
		return false
	}
}

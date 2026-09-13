package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// varroa-cli delta: the cell-pool noun and the new top-level verb scale.
// CellPool is cluster-scoped, so every command refuses a namespace flag.
// ---------------------------------------------------------------------------

func cellPoolJSON(warm int) map[string]any {
	return map[string]any{
		"name": "lts-2-555",
		"spec": map[string]any{
			"profileRef": "lts-2-555", "warm": warm, "maxCells": 6, "maxTenantsPerCell": 10,
		},
		"status": map[string]any{
			"warmCells": warm, "placedCells": 1, "unreadyCells": 0, "freeTenantSlots": 9,
			"tenants": []string{"alpha", "beta"},
			"conditions": []map[string]any{
				{"type": "PoolReady", "status": "True", "reason": "Ready"},
			},
		},
	}
}

// TestCellPools_GetList covers "The cell-pool noun follows kubectl grammar"
// on the collection: the CLI calls GET /api/v1/cellpools.
func TestCellPools_GetList(t *testing.T) {
	testSetup(t)
	called := false
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/cellpools" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{cellPoolJSON(2)},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "cellpools"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("the CLI made no request")
	}
}

// TestCellPools_GetSingle covers the single-get form.
func TestCellPools_GetSingle(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/cellpools/lts-2-555" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cellPoolJSON(2))
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"get", "cellpool", "lts-2-555", "-o", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCellPools_Describe proves describe prints the tenant list and the
// PoolReady condition.
func TestCellPools_Describe(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cellpools/lts-2-555" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cellPoolJSON(2))
	})
	defer srv.Close()

	root := newRootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"describe", "cellpool", "lts-2-555"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := out.String()
	for _, want := range []string{"alpha", "beta", "PoolReady", "Ready"} {
		if !strings.Contains(text, want) {
			t.Errorf("describe output missing %q:\n%s", want, text)
		}
	}
}

// TestCellPools_NamespaceFlagRefused covers "A namespace flag on a cell pool
// is refused": the CLI exits with a usage error and makes no request.
func TestCellPools_NamespaceFlagRefused(t *testing.T) {
	for _, args := range [][]string{
		{"get", "cellpools", "-n", "team-a"},
		{"describe", "cellpool", "lts-2-555", "-n", "team-a"},
		{"scale", "cellpool", "lts-2-555", "--warm", "3", "-n", "team-a"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			testSetup(t)
			srv := testServer(t, func(http.ResponseWriter, *http.Request) {
				t.Error("the CLI must make no request")
			})
			defer srv.Close()

			root := newRootCmd()
			root.SetArgs(args)
			err := root.Execute()
			if err == nil {
				t.Fatal("expected a usage error")
			}
			var ue usageError
			if !errorsAs(err, &ue) {
				t.Fatalf("error = %T %v, want a usageError (exit code 2)", err, err)
			}
		})
	}
}

// TestCellPools_ScaleSendsOnlyTheNamedDials covers "Scaling sends only the
// named dials".
func TestCellPools_ScaleSendsOnlyTheNamedDials(t *testing.T) {
	testSetup(t)
	var body map[string]any
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/cellpools/lts-2-555" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cellPoolJSON(3))
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"scale", "cellpool", "lts-2-555", "--warm", "3"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("body carries no spec object: %v", body)
	}
	if got := spec["warm"]; got != float64(3) {
		t.Errorf("spec.warm = %v, want 3", got)
	}
	if _, ok := spec["maxCells"]; ok {
		t.Errorf("an unnamed dial must not be sent: %v", spec)
	}
	if len(spec) != 1 {
		t.Errorf("spec = %v, want warm alone", spec)
	}
}

// TestCellPools_ScaleSendsBothDials proves --max-cells travels with --warm.
func TestCellPools_ScaleSendsBothDials(t *testing.T) {
	testSetup(t)
	var body map[string]any
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cellPoolJSON(3))
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"scale", "cellpool", "lts-2-555", "--warm", "3", "--max-cells", "8"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	spec := body["spec"].(map[string]any)
	if spec["warm"] != float64(3) || spec["maxCells"] != float64(8) {
		t.Errorf("spec = %v, want warm 3 and maxCells 8", spec)
	}
}

// TestCellPools_ScaleWithNoFlagIsAUsageError covers "Scaling with no flag is
// a usage error", and the negative-value rule, both reported before the
// request.
func TestCellPools_ScaleRefusedBeforeTheRequest(t *testing.T) {
	for _, args := range [][]string{
		{"scale", "cellpool", "lts-2-555"},
		{"scale", "cellpool", "lts-2-555", "--warm", "-1"},
		{"scale", "cellpool", "lts-2-555", "--max-cells", "-2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			testSetup(t)
			srv := testServer(t, func(http.ResponseWriter, *http.Request) {
				t.Error("the CLI must make no request")
			})
			defer srv.Close()

			root := newRootCmd()
			root.SetArgs(args)
			err := root.Execute()
			if err == nil {
				t.Fatal("expected a usage error")
			}
			var ue usageError
			if !errorsAs(err, &ue) {
				t.Fatalf("error = %T %v, want a usageError (exit code 2)", err, err)
			}
		})
	}
}

// TestCellPools_ScalePermissionFailureSurfaces covers "A permission failure
// surfaces uniformly".
func TestCellPools_ScalePermissionFailureSurfaces(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "forbidden"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"scale", "cellpool", "lts-2-555", "--warm", "3"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error = %v, want the server's own message", err)
	}
}

// TestCellPools_CreateIsNotACLIOperation covers "Creating a pool is not a CLI
// operation": the noun registers under get and describe alone, so the command
// tree carries no create cellpool and no delete cellpool. A verb parent
// prints its own help for any unregistered noun, which is shared framework
// behaviour, so the assertion reads the command tree rather than an exit code.
func TestCellPools_CreateIsNotACLIOperation(t *testing.T) {
	testSetup(t)
	root := newRootCmd()

	present := map[string]bool{}
	for _, verb := range root.Commands() {
		for _, noun := range verb.Commands() {
			if noun.Name() == "cellpool" {
				present[verb.Name()] = true
			}
		}
	}
	for _, verb := range []string{"create", "delete", "edit", "patch"} {
		if present[verb] {
			t.Errorf("%s cellpool must not exist: the API serves no such route", verb)
		}
	}
	for _, verb := range []string{"get", "describe", "scale"} {
		if !present[verb] {
			t.Errorf("%s cellpool must exist", verb)
		}
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Varroa writes a Controller spec as the field manager "varroa-ui", so
// changing a field another manager owns is refused with a field conflict. The
// operator itself claims spec.powerState when it auto-hibernates a controller,
// which makes `power controller ... running` on a parked controller the common
// case rather than a corner one. Without --force the CLI has no way through.

// forceQueryProbe runs one varroactl invocation against a stub BFF and returns
// the raw ?force= value the request carried.
func forceQueryProbe(t *testing.T, args ...string) string {
	t.Helper()
	testSetup(t)
	var got string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("force")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "updated"})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return got
}

// The 409 body carries a conflicts array; rendering only its `error` leaves the
// user with a bare "field conflict" and no way to act on it.
func TestConflictErrorNamesFieldsAndForce(t *testing.T) {
	testSetup(t)
	srv := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "field conflict",
			"conflicts": []map[string]string{{
				"field":   ".spec.powerState",
				"manager": "varroa-operator",
				"message": `conflict with "varroa-operator"`,
			}},
		})
	})
	defer srv.Close()

	root := newRootCmd()
	root.SetArgs([]string{"power", "controller", "team-a/ctrl-1", "running"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected the 409 to surface as an error")
	}
	for _, want := range []string{".spec.powerState", "varroa-operator", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("conflict error does not mention %q: %v", want, err)
		}
	}
}

func TestForceFlagReachesPatchQuery(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"power", []string{"power", "controller", "team-a/ctrl-1", "running"}},
		{"patch", []string{"patch", "controller", "team-a/ctrl-1", "-p", `{"spec":{"version":"2.570"}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forceQueryProbe(t, tc.args...); got != "" {
				t.Errorf("force=%q on a plain call, want the query param absent", got)
			}
			if got := forceQueryProbe(t, append(tc.args, "--force")...); got != "true" {
				t.Errorf("force=%q with --force, want %q", got, "true")
			}
		})
	}
}

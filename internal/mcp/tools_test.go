package mcp

import (
	"math"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// Write tools decide "preserve" vs "clear" on key presence, so the second
// return is the contract: a non-bool value must report absent rather than
// silently reading as false and clearing a stored flag.
func TestBoolArg(t *testing.T) {
	args := map[string]any{"t": true, "f": false, "s": "true", "n": 1}
	for k, want := range map[string]bool{"t": true, "f": false, "s": false, "n": false, "missing": false} {
		if got, _ := boolArg(args, k); got != want {
			t.Errorf("boolArg(%q) = %v, want %v", k, got, want)
		}
	}
	if _, ok := boolArg(args, "missing"); ok {
		t.Error("boolArg must report ok=false for a missing key")
	}
	if _, ok := boolArg(args, "s"); ok {
		t.Error("boolArg must report ok=false for a non-bool value")
	}
}

// JSON numbers arrive as float64 through mcp-go, so the float64 case is the
// live path and the int case only covers hand-built argument maps. A
// fractional value is a caller error, not a value to truncate.
func TestIntArg(t *testing.T) {
	args := map[string]any{"f": float64(300), "i": 60, "s": "300", "frac": 60.5}
	if got, ok, err := intArg(args, "f"); err != nil || !ok || got != 300 {
		t.Errorf("intArg(f) = %d,%v,%v, want 300,true,nil", got, ok, err)
	}
	if got, ok, err := intArg(args, "i"); err != nil || !ok || got != 60 {
		t.Errorf("intArg(i) = %d,%v,%v, want 60,true,nil", got, ok, err)
	}
	if _, ok, err := intArg(args, "missing"); ok || err != nil {
		t.Errorf("intArg(missing) = ok=%v err=%v, want absent and no error", ok, err)
	}
	args["huge"] = 1e300
	args["nan"] = math.NaN()
	for _, k := range []string{"s", "frac", "huge", "nan"} {
		if _, ok, err := intArg(args, k); !ok || err == nil {
			t.Errorf("intArg(%q) must report present with an error, got ok=%v err=%v", k, ok, err)
		}
	}
}

func TestValidateCatalogSourceSpec_ReservedNameRejectsSource(t *testing.T) {
	reserved := v1alpha1.UpdateCenterCatalogSourceName
	if err := validateCatalogSourceSpec(reserved, &v1alpha1.CatalogSourceSpec{}); err != nil {
		t.Errorf("reserved source with no repoURL/ociRef must validate, got %v", err)
	}
	if err := validateCatalogSourceSpec(reserved, &v1alpha1.CatalogSourceSpec{RepoURL: "https://example.com/x.git"}); err == nil {
		t.Error("reserved source with repoURL must be rejected, matching the CRD rule")
	}
	if err := validateCatalogSourceSpec(reserved, &v1alpha1.CatalogSourceSpec{OCIRef: "ghcr.io/x/y:1"}); err == nil {
		t.Error("reserved source with ociRef must be rejected, matching the CRD rule")
	}
	if err := validateCatalogSourceSpec("plain", &v1alpha1.CatalogSourceSpec{}); err == nil {
		t.Error("ordinary source with no repoURL/ociRef must be rejected")
	}
}

func TestCheckSyncInterval(t *testing.T) {
	for _, v := range []int{0, 30, 300} {
		if err := checkSyncInterval(v); err != nil {
			t.Errorf("checkSyncInterval(%d) = %v, want nil", v, err)
		}
	}
	for _, v := range []int{1, 5, 29, 31536001, 10000000000} {
		if err := checkSyncInterval(v); err == nil {
			t.Errorf("checkSyncInterval(%d) must reject a value outside 30..31536000", v)
		}
	}
}

func TestValidateCatalogSourceSpec_RepoURLScheme(t *testing.T) {
	for _, u := range []string{"https://example.com/x.git", "ssh://git@example.com/x.git", "git@github.com:org/x.git"} {
		if err := validateCatalogSourceSpec("src", &v1alpha1.CatalogSourceSpec{RepoURL: u}); err != nil {
			t.Errorf("repoURL %q must validate, got %v", u, err)
		}
	}
	for _, u := range []string{"http://example.com/x.git", "file:///tmp/x", "ext::sh -c id"} {
		if err := validateCatalogSourceSpec("src", &v1alpha1.CatalogSourceSpec{RepoURL: u}); err == nil {
			t.Errorf("repoURL %q must be rejected, matching the CRD rule", u)
		}
	}
}

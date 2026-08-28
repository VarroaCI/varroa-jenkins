package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// resolveNSName tests
// ---------------------------------------------------------------------------

func TestResolveNSName_NSName(t *testing.T) {
	ns, name, err := resolveNSName("team-a/ctrl-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "team-a" {
		t.Errorf("expected ns=team-a, got %s", ns)
	}
	if name != "ctrl-1" {
		t.Errorf("expected name=ctrl-1, got %s", name)
	}
}

func TestResolveNSName_NameWithNFlag(t *testing.T) {
	ns, name, err := resolveNSName("ctrl-1", "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "team-a" {
		t.Errorf("expected ns=team-a, got %s", ns)
	}
	if name != "ctrl-1" {
		t.Errorf("expected name=ctrl-1, got %s", name)
	}
}

func TestResolveNSName_NameWithDefault(t *testing.T) {
	ns, name, err := resolveNSName("ctrl-1", "", "default-ns")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "default-ns" {
		t.Errorf("expected ns=default-ns, got %s", ns)
	}
	if name != "ctrl-1" {
		t.Errorf("expected name=ctrl-1, got %s", name)
	}
}

func TestResolveNSName_NSNameConflictWithNFlag(t *testing.T) {
	_, _, err := resolveNSName("team-a/ctrl-1", "team-b", "")
	if err == nil {
		t.Fatal("expected error for namespace conflict")
	}
	var ue usageError
	if !errorsAs(err, &ue) {
		t.Fatalf("expected usageError, got %T: %v", err, err)
	}
	if !strings.Contains(ue.Error(), "namespace conflict") {
		t.Errorf("expected 'namespace conflict' in error, got %v", ue.Error())
	}
}

func TestResolveNSName_MissingNamespace(t *testing.T) {
	_, _, err := resolveNSName("ctrl-1", "", "")
	if err == nil {
		t.Fatal("expected error for missing namespace")
	}
	var ue usageError
	if !errorsAs(err, &ue) {
		t.Fatalf("expected usageError, got %T: %v", err, err)
	}
	if !strings.Contains(ue.Error(), "namespace required") {
		t.Errorf("expected 'namespace required' in error, got %v", ue.Error())
	}
}

// ---------------------------------------------------------------------------
// resolveListNamespace tests
// ---------------------------------------------------------------------------

func TestResolveListNamespace_NFlag(t *testing.T) {
	ns := resolveListNamespace("team-a", false, "default")
	if ns != "team-a" {
		t.Errorf("expected team-a, got %s", ns)
	}
}

func TestResolveListNamespace_AFlag(t *testing.T) {
	ns := resolveListNamespace("", true, "default")
	if ns != "" {
		t.Errorf("expected empty for -A, got %s", ns)
	}
}

func TestResolveListNamespace_Neither(t *testing.T) {
	ns := resolveListNamespace("", false, "default-ns")
	if ns != "default-ns" {
		t.Errorf("expected default-ns, got %s", ns)
	}
}

func TestResolveListNamespace_NeitherNoDefault(t *testing.T) {
	ns := resolveListNamespace("", false, "")
	if ns != "" {
		t.Errorf("expected empty, got %s", ns)
	}
}

// ---------------------------------------------------------------------------
// resolveCluster tests
// ---------------------------------------------------------------------------

func TestResolveCluster_Flag(t *testing.T) {
	got := resolveCluster("dev-cluster", "staging")
	if got != "dev-cluster" {
		t.Errorf("expected dev-cluster, got %s", got)
	}
}

func TestResolveCluster_Default(t *testing.T) {
	got := resolveCluster("", "staging")
	if got != "staging" {
		t.Errorf("expected staging, got %s", got)
	}
}

func TestResolveCluster_Core(t *testing.T) {
	got := resolveCluster("", "")
	if got != "core" {
		t.Errorf("expected core, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// resolveListCluster tests
// ---------------------------------------------------------------------------

func TestResolveListCluster_FlagAndAllExclusive(t *testing.T) {
	_, err := resolveListCluster("dev-cluster", true, "staging")
	if err == nil {
		t.Fatal("expected error for mutually exclusive flags")
	}
	var ue usageError
	if !errorsAs(err, &ue) {
		t.Fatalf("expected usageError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected 'mutually exclusive' in error, got %v", err)
	}
}

func TestResolveListCluster_AllClusters(t *testing.T) {
	got, err := resolveListCluster("", true, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty for --all-clusters, got %s", got)
	}
}

func TestResolveListCluster_FlagOnly(t *testing.T) {
	got, err := resolveListCluster("dev-cluster", false, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if got != "dev-cluster" {
		t.Errorf("expected dev-cluster, got %s", got)
	}
}

func TestResolveListCluster_DefaultOnly(t *testing.T) {
	got, err := resolveListCluster("", false, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if got != "staging" {
		t.Errorf("expected staging, got %s", got)
	}
}

func TestResolveListCluster_NoFlagNoDefault(t *testing.T) {
	got, err := resolveListCluster("", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

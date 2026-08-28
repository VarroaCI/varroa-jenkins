package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// mintBroodOpName embeds the verb in the object name; every verb must yield a
// valid DNS-1123 name. executeGroovy is the only camelCase verb, so it regressed
// this (uppercase 'G' → apiserver rejects the create → BFF 500).
func TestMintBroodOpName_ValidDNS1123AllVerbs(t *testing.T) {
	verbs := []v1alpha1.BroodVerb{"restart", "reprovision", "reconcile", "stop", "start", "executeGroovy"}
	for _, v := range verbs {
		name := mintBroodOpName(v)
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Errorf("mintBroodOpName(%q) = %q is not a valid DNS-1123 name: %v", v, name, errs)
		}
	}
}

func knownCluster(name string) bool {
	known := map[string]bool{
		"core":        true,
		"dev-cluster": true,
		"staging":     true,
	}
	return known[name]
}

func TestValidateAndPartition_ClustersEmptyBehavesAsAbsent(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}}},
		Clusters:  []string{},
	}
	specs, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 1 || specs["core"].Targets.Names[0] != "ctrl-a" {
		t.Errorf("expected single core spec, got %v", specs)
	}
}

func TestValidateAndPartition_Dedup(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ns/ctrl-a"}}},
		Clusters:  []string{"core", "core"},
	}
	specs, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 1 {
		t.Errorf("expected 1 spec after dedup, got %d", len(specs))
	}
}

func TestValidateAndPartition_MixingQualifiedUnqualified(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"core/ns/ctrl-a", "ctrl-b"}}},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for mixing qualified and unqualified names")
	}
}

func TestValidateAndPartition_3TokenInTeamMode(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"core/ns/ctrl-a"}}},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for 3-token names in team mode")
	}
}

func TestValidateAndPartition_ClustersWithQualifiedNames(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"core/ns/ctrl-a"}}},
		Clusters:  []string{"core"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for clusters+qualified names")
	}
}

func TestValidateAndPartition_MultiEntryClustersWithUnqualifiedNames(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ns/ctrl-a"}}},
		Clusters:  []string{"core", "dev-cluster"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for multi-entry clusters with unqualified names")
	}
}

func TestValidateAndPartition_AllWithNames(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ns/ctrl-a"}}},
		Clusters:  []string{"all"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for all+names")
	}
}

func TestValidateAndPartition_TeamModeMultipleClusters(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}}},
		Clusters:  []string{"core", "dev-cluster"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for team mode with >1 cluster")
	}
}

func TestValidateAndPartition_UnknownCluster(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ns/ctrl-a"}}},
		Clusters:  []string{"unknown"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for unknown cluster")
	}
}

func TestValidateAndPartition_AllMixedWithExplicit(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}}},
		Clusters:  []string{"all", "core"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err == nil {
		t.Fatal("expected error for all mixed with explicit")
	}
}

func TestValidateAndPartition_HappyTeamSingleCluster(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}}},
	}
	specs, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 1 || specs["core"].Targets.Names[0] != "ctrl-a" {
		t.Errorf("expected core spec, got %v", specs)
	}
}

func TestValidateAndPartition_HappyOperatorSelectorMultiCluster(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}}},
		Clusters:  []string{"core", "dev-cluster"},
	}
	specs, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 2 {
		t.Errorf("expected 2 specs, got %d", len(specs))
	}
}

func TestValidateAndPartition_Partition3Token(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"core/team-a/ci", "dev-cluster/team-b/web"}}},
	}
	specs, err := validateAndPartition(req, "core", knownCluster, "operator-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}
	if len(specs["core"].Targets.Names) != 1 || specs["core"].Targets.Names[0] != "team-a/ci" {
		t.Errorf("core spec names: %v", specs["core"].Targets.Names)
	}
	if len(specs["dev-cluster"].Targets.Names) != 1 || specs["dev-cluster"].Targets.Names[0] != "team-b/web" {
		t.Errorf("dev-cluster spec names: %v", specs["dev-cluster"].Targets.Names)
	}
}

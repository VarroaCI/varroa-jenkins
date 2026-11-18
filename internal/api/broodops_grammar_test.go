package api

import (
	"errors"
	"reflect"
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

// allClusters lists the clusters that knownCluster knows.
func allClusters() ([]string, error) {
	return []string{"core", "dev-cluster", "staging"}, nil
}

func TestValidateAndPartition_ClustersEmptyBehavesAsAbsent(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}}},
		Clusters:  []string{},
	}
	specs, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
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
	specs, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
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
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
	if err == nil {
		t.Fatal("expected error for mixing qualified and unqualified names")
	}
}

func TestValidateAndPartition_3TokenInTenantMode(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"core/ns/ctrl-a"}}},
	}
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
	if err == nil {
		t.Fatal("expected error for 3-token names in tenant mode")
	}
}

func TestValidateAndPartition_ClustersWithQualifiedNames(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"core/ns/ctrl-a"}}},
		Clusters:  []string{"core"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
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
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
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
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
	if err == nil {
		t.Fatal("expected error for all+names")
	}
}

func TestValidateAndPartition_TenantModeMultipleClusters(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}}},
		Clusters:  []string{"core", "dev-cluster"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
	if err == nil {
		t.Fatal("expected error for tenant mode with >1 cluster")
	}
}

func TestValidateAndPartition_UnknownCluster(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ns/ctrl-a"}}},
		Clusters:  []string{"unknown"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
	if err == nil {
		t.Fatal("expected error for unknown cluster")
	}
}

// A cluster/ns/name entry must name a known cluster, like a clusters entry.
func TestValidateAndPartition_UnknownQualifierCluster(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		want  string
	}{
		{name: "unknown member", names: []string{"core/team-a/ci", "nope/team-b/web"}, want: `unknown cluster "nope"`},
		{name: "empty cluster token", names: []string{"/team-a/ci"}, want: `unknown cluster ""`},
		{name: "all sentinel", names: []string{"all/team-a/ci"}, want: `unknown cluster "all"`},
		{name: "empty namespace token", names: []string{"core//ci"}, want: `invalid name "core//ci": expected cluster/namespace/name`},
		{name: "empty name token", names: []string{"core/team-a/"}, want: `invalid name "core/team-a/": expected cluster/namespace/name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := broodCreateRequest{
				Namespace: "operator-ns",
				Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: tc.names}},
			}
			specs, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if specs != nil {
				t.Errorf("specs = %v, want nil", specs)
			}
		})
	}
}

func TestValidateAndPartition_AllMixedWithExplicit(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}}},
		Clusters:  []string{"all", "core"},
	}
	_, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
	if err == nil {
		t.Fatal("expected error for all mixed with explicit")
	}
}

func TestValidateAndPartition_HappyTenantSingleCluster(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "team-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}}},
	}
	specs, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
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
	specs, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
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
	specs, err := validateAndPartition(req, "core", knownCluster, allClusters, "operator-ns")
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

// knownWithMember knows the local cluster core and the member member-b.
func knownWithMember(name string) bool { return name == "core" || name == "member-b" }

// ["all"] expands to the local cluster and every membership entry. Each
// cluster gets the request spec unchanged.
func TestValidateAndPartition_AllExpandsToBrood(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}}},
		Clusters:  []string{"all", "all"},
	}
	all := func() ([]string, error) { return []string{"member-b", "core", "member-b"}, nil }
	specs, err := validateAndPartition(req, "core", knownWithMember, all, "operator-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := orderBroodClusters(specs, "core"); !reflect.DeepEqual(got, []string{"core", "member-b"}) {
		t.Fatalf("clusters = %v, want [core member-b]", got)
	}
	for c, spec := range specs {
		if !reflect.DeepEqual(spec, req.Spec) {
			t.Errorf("spec of %s = %+v, want the request spec", c, spec)
		}
	}
}

// The cluster list is read only for ["all"].
func TestValidateAndPartition_AllListReadOnlyForSentinel(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}}},
		Clusters:  []string{"core", "member-b"},
	}
	all := func() ([]string, error) {
		t.Fatal("cluster list read for an explicit clusters list")
		return nil, nil
	}
	if _, err := validateAndPartition(req, "core", knownWithMember, all, "operator-ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A failed cluster list read comes back as a broodClusterListError.
func TestValidateAndPartition_AllListError(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "operator-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}}},
		Clusters:  []string{"all"},
	}
	all := func() ([]string, error) { return nil, errors.New("list clusters: directory down") }
	specs, err := validateAndPartition(req, "core", knownWithMember, all, "operator-ns")
	var listErr *broodClusterListError
	if !errors.As(err, &listErr) || err.Error() != "list clusters: directory down" {
		t.Fatalf("err = %v, want a broodClusterListError", err)
	}
	if specs != nil {
		t.Errorf("specs = %v, want nil", specs)
	}
}

// A tenant namespace run targets one cluster, so ["all"] passes only while
// the brood holds the local cluster alone.
func TestValidateAndPartition_AllInTenantMode(t *testing.T) {
	req := broodCreateRequest{
		Namespace: "tenant-ns",
		Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Selector: &metav1.LabelSelector{}}},
		Clusters:  []string{"all"},
	}
	withMember := func() ([]string, error) { return []string{"core", "member-b"}, nil }
	if _, err := validateAndPartition(req, "core", knownWithMember, withMember, "operator-ns"); err == nil ||
		err.Error() != "tenant namespace mode supports at most one target cluster" {
		t.Fatalf("err = %v, want the tenant cluster refusal", err)
	}

	localOnly := func() ([]string, error) { return []string{"core"}, nil }
	specs, err := validateAndPartition(req, "core", knownWithMember, localOnly, "operator-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := orderBroodClusters(specs, "core"); !reflect.DeepEqual(got, []string{"core"}) {
		t.Errorf("clusters = %v, want [core]", got)
	}
}

// An empty clusters array with cluster/ns/name entries partitions like an
// absent clusters field.
func TestValidateAndPartition_EmptyClustersWithQualifiedNames(t *testing.T) {
	names := []string{"core/tenant-a/ci", "member-b/tenant-b/web"}
	for _, clusters := range [][]string{nil, {}} {
		req := broodCreateRequest{
			Namespace: "operator-ns",
			Spec:      v1alpha1.BroodOperationSpec{Targets: v1alpha1.BroodTargets{Names: names}},
			Clusters:  clusters,
		}
		specs, err := validateAndPartition(req, "core", knownWithMember, allClusters, "operator-ns")
		if err != nil {
			t.Fatalf("clusters %#v: unexpected error: %v", clusters, err)
		}
		if got := specs["core"].Targets.Names; !reflect.DeepEqual(got, []string{"tenant-a/ci"}) {
			t.Errorf("clusters %#v: core names = %v", clusters, got)
		}
		if got := specs["member-b"].Targets.Names; !reflect.DeepEqual(got, []string{"tenant-b/web"}) {
			t.Errorf("clusters %#v: member-b names = %v", clusters, got)
		}
	}
}

// The fan-out order puts the local cluster first and the others in name order.
func TestOrderBroodClusters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		specs []string
		want  []string
	}{
		{name: "local first", specs: []string{"member-c", "core", "member-b"}, want: []string{"core", "member-b", "member-c"}},
		{name: "no local", specs: []string{"member-c", "member-b"}, want: []string{"member-b", "member-c"}},
		{name: "local alone", specs: []string{"core"}, want: []string{"core"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := map[string]v1alpha1.BroodOperationSpec{}
			for _, c := range tc.specs {
				specs[c] = v1alpha1.BroodOperationSpec{}
			}
			if got := orderBroodClusters(specs, "core"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

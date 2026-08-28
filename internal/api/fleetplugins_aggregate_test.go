package api

import (
	"sort"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/pluginrange"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func nn(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "ns", Name: name}
}

func row(ns, name string, phase v1alpha1.ControllerPhase, hasInv bool, plugins []InstalledPlugin, env ClassifiedEnvelope, stale, degraded, truncated, optionalEdgesDropped, bootstrapApprox bool, source string) controllerRow {
	detailStale := checkEnvelope(env, env.Hash, env.Source, stale, degraded, truncated, optionalEdgesDropped, bootstrapApprox, env.DriftTruncated)
	var inv ControllerInventory
	if hasInv {
		inv = ControllerInventory{
			Plugins:     plugins,
			CollectedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC),
			Envelope:    env,
		}
	}
	return controllerRow{
		Cluster:              "local",
		Namespace:            ns,
		Name:                 name,
		Phase:                phase,
		Inv:                  inv,
		HasInv:               hasInv,
		Stale:                stale,
		Degraded:             degraded,
		Truncated:            truncated,
		OptionalEdgesDropped: optionalEdgesDropped,
		BootstrapApproximate: bootstrapApprox,
		Source:               source,
		Envelope:             env,
		DetailStale:          detailStale,
	}
}

func defaultEnv() ClassifiedEnvelope {
	return ClassifiedEnvelope{
		Hash:   "abc123",
		Source: "jenkins-api",
	}
}

// ---------------------------------------------------------------------------
// Tests: Rollup
// ---------------------------------------------------------------------------

func TestRollup_BasicCounts(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "ctrl-a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
				{Name: "workflow-api", Version: "2.0", Class: "dependency"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "ctrl-b", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.1.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, cov := Rollup(in, "", pluginrange.Expr{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// git-client should have controllerCount 2.
	gc := items[0]
	if gc.Name != "git-client" {
		// swap if sorted differently
		gc = items[1]
	}
	if gc.ControllerCount != 2 {
		t.Errorf("git-client controllerCount = %d, want 2", gc.ControllerCount)
	}
	if len(gc.Versions) != 2 {
		t.Errorf("git-client versions = %d, want 2", len(gc.Versions))
	}

	if cov.ControllersTotal != 2 {
		t.Errorf("controllersTotal = %d, want 2", cov.ControllersTotal)
	}
	if !cov.Complete {
		t.Error("complete should be true")
	}
}

func TestRollup_ClassBreakdownAcrossControllers(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "ctrl-a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "ctrl-b", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "unmanaged"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, _ := Rollup(in, "", pluginrange.Expr{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	gc := items[0]
	if len(gc.Classes) != 2 {
		t.Fatalf("expected 2 classes, got %d", len(gc.Classes))
	}
	// Bytewise ordering: declared < unmanaged.
	if gc.Classes[0].Class != "declared" {
		t.Errorf("classes[0] = %q, want declared", gc.Classes[0].Class)
	}
	if gc.Classes[0].ControllerCount != 1 {
		t.Errorf("declared count = %d, want 1", gc.Classes[0].ControllerCount)
	}
	if gc.Classes[1].Class != "unmanaged" {
		t.Errorf("classes[1] = %q, want unmanaged", gc.Classes[1].Class)
	}
	if gc.Classes[1].ControllerCount != 1 {
		t.Errorf("unmanaged count = %d, want 1", gc.Classes[1].ControllerCount)
	}
}

func TestRollup_ClassBytewiseOrdering(t *testing.T) {
	// unmanaged, declared, jenkins-supplied → declared, jenkins-supplied, unmanaged.
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "unmanaged"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "b", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "c", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "jenkins-supplied"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, _ := Rollup(in, "", pluginrange.Expr{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	classes := items[0].Classes
	if len(classes) != 3 {
		t.Fatalf("expected 3 classes, got %d", len(classes))
	}
	if classes[0].Class != "declared" {
		t.Errorf("classes[0] = %q, want declared", classes[0].Class)
	}
	if classes[1].Class != "jenkins-supplied" {
		t.Errorf("classes[1] = %q, want jenkins-supplied", classes[1].Class)
	}
	if classes[2].Class != "unmanaged" {
		t.Errorf("classes[2] = %q, want unmanaged", classes[2].Class)
	}
}

func TestRollup_QFilter(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "ctrl-a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
				{Name: "github-branch-source", Version: "1.0", Class: "declared"},
				{Name: "docker-workflow", Version: "2.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, _ := Rollup(in, "GIT", pluginrange.Expr{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items for q=GIT, got %d", len(items))
	}
	names := make(map[string]bool)
	for _, it := range items {
		names[it.Name] = true
	}
	if !names["git-client"] {
		t.Error("expected git-client")
	}
	if !names["github-branch-source"] {
		t.Error("expected github-branch-source")
	}
	if names["docker-workflow"] {
		t.Error("docker-workflow should not match GIT")
	}
}

func TestRollup_AffectedFilter(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "ctrl-a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "3.9.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "ctrl-b", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "ctrl-c", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.1.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	affected, _ := pluginrange.Parse("<=4.0.0")
	items, _ := Rollup(in, "", affected)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	gc := items[0]
	if gc.ControllerCount != 2 {
		t.Errorf("controllerCount with <=4.0.0 = %d, want 2", gc.ControllerCount)
	}
}

// ---------------------------------------------------------------------------
// Tests: Drill
// ---------------------------------------------------------------------------

func TestDrill_ExactNameMatching(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "ctrl-a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "ctrl-b", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client-api", Version: "1.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, _, _ := Drill(in, "git-client", pluginrange.Expr{})
	if len(items) != 1 {
		t.Fatalf("exact name match: expected 1 item, got %d", len(items))
	}
	if items[0].Controller != "ctrl-a" {
		t.Errorf("expected ctrl-a, got %s", items[0].Controller)
	}
}

func TestDrill_UnknownPluginEmptyAnswer(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "ctrl-a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, versions, _ := Drill(in, "does-not-exist", pluginrange.Expr{})
	if len(items) != 0 {
		t.Errorf("unknown plugin: expected 0 items, got %d", len(items))
	}
	if len(versions) != 0 {
		t.Errorf("unknown plugin: expected 0 versions, got %d", len(versions))
	}
}

// ---------------------------------------------------------------------------
// Tests: Version ordering determinism
// ---------------------------------------------------------------------------

func TestVersionOrdering_DeterministicAcrossShuffles(t *testing.T) {
	entries := []fleetPluginEntry{
		{name: "p", version: "4.1.0", class: "declared"},
		{name: "p", version: "4.0.0", class: "declared"},
		{name: "p", version: "3.9.0", class: "declared"},
	}

	// Collect the output for the original order.
	orig := versionHistogram(entries)

	// Shuffle and verify identical output.
	for trial := 0; trial < 5; trial++ {
		shuffled := make([]fleetPluginEntry, len(entries))
		copy(shuffled, entries)
		// Use a simple rotation to simulate different orders.
		for i := 0; i < len(shuffled); i++ {
			shifted := make([]fleetPluginEntry, len(shuffled))
			for j := range shuffled {
				shifted[j] = shuffled[(j+i)%len(shuffled)]
			}
			result := versionHistogram(shifted)
			if len(result) != len(orig) {
				t.Fatalf("shuffled trial %d: length %d != %d", trial, len(result), len(orig))
			}
			for k := range result {
				if result[k].Version != orig[k].Version || result[k].ControllerCount != orig[k].ControllerCount {
					t.Errorf("shuffled trial %d position %d: got %v, want %v", trial, k, result[k], orig[k])
				}
			}
		}
	}
}

func TestVersionOrdering_NewestFirst(t *testing.T) {
	entries := []fleetPluginEntry{
		{name: "p", version: "3.9.0", class: "declared"},
		{name: "p", version: "4.1.0", class: "declared"},
		{name: "p", version: "4.0.0", class: "declared"},
	}
	result := versionHistogram(entries)
	if len(result) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(result))
	}
	if result[0].Version != "4.1.0" {
		t.Errorf("first = %q, want 4.1.0", result[0].Version)
	}
	if result[1].Version != "4.0.0" {
		t.Errorf("second = %q, want 4.0.0", result[1].Version)
	}
	if result[2].Version != "3.9.0" {
		t.Errorf("third = %q, want 3.9.0", result[2].Version)
	}
}

func TestVersionOrdering_NonTransitiveTriple(t *testing.T) {
	// The triple "1", "1-sp", "1.0a1" — non-transitive under pluginver.Compare.
	entries := []fleetPluginEntry{
		{name: "p", version: "1", class: "declared"},
		{name: "p", version: "1-sp", class: "declared"},
		{name: "p", version: "1.0a1", class: "declared"},
	}
	orig := versionHistogram(entries)

	// Shuffle and verify identical output.
	shuffled := []fleetPluginEntry{
		{name: "p", version: "1.0a1", class: "declared"},
		{name: "p", version: "1", class: "declared"},
		{name: "p", version: "1-sp", class: "declared"},
	}
	result := versionHistogram(shuffled)
	if len(result) != len(orig) {
		t.Fatalf("length mismatch: %d != %d", len(result), len(orig))
	}
	for i := range result {
		if result[i].Version != orig[i].Version {
			t.Errorf("position %d: %q != %q", i, result[i].Version, orig[i].Version)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Coverage
// ---------------------------------------------------------------------------

func TestCoverage_Complete(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "b", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	_, cov := Rollup(in, "", pluginrange.Expr{})
	if !cov.Complete {
		t.Error("complete should be true")
	}
	if cov.ClustersNotCovered != 0 {
		t.Errorf("clustersNotCovered = %d, want 0", cov.ClustersNotCovered)
	}
	if cov.ControllersReporting != cov.ControllersTotal {
		t.Errorf("controllersReporting (%d) != controllersTotal (%d)", cov.ControllersReporting, cov.ControllersTotal)
	}
}

func TestCoverage_NotComplete(t *testing.T) {
	in := fleetInput{
		Rows:     []controllerRow{},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}, {Name: "remote", OK: false, Error: "v1 local only"}},
	}
	_, cov := Rollup(in, "", pluginrange.Expr{})
	if cov.Complete {
		t.Error("complete should be false with an unreachable cluster")
	}
	if cov.ClustersNotCovered != 1 {
		t.Errorf("clustersNotCovered = %d, want 1", cov.ClustersNotCovered)
	}
}

func TestCoverage_MissingControllers(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "connected", v1alpha1.ControllerPhaseConnected, false, nil, defaultEnv(), false, false, false, false, false, ""),
			row("ns1", "hibernated", v1alpha1.ControllerPhaseHibernated, false, nil, defaultEnv(), false, false, false, false, false, ""),
			row("ns1", "stopped", v1alpha1.ControllerPhaseStopped, false, nil, defaultEnv(), false, false, false, false, false, ""),
			row("ns1", "pending", v1alpha1.ControllerPhasePending, false, nil, defaultEnv(), false, false, false, false, false, ""),
			row("ns1", "empty", "", false, nil, defaultEnv(), false, false, false, false, false, ""),
			row("ns1", "reporting", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	_, cov := Rollup(in, "", pluginrange.Expr{})

	if cov.ControllersTotal != 6 {
		t.Errorf("controllersTotal = %d, want 6", cov.ControllersTotal)
	}
	if cov.ControllersReporting != 1 {
		t.Errorf("controllersReporting = %d, want 1", cov.ControllersReporting)
	}
	if len(cov.ControllersMissing) != 5 {
		t.Fatalf("controllersMissing = %d, want 5", len(cov.ControllersMissing))
	}

	// Verify reasons.
	wantReasons := map[string]string{
		"connected":  "never-reported",
		"hibernated": "hibernated",
		"stopped":    "stopped",
		"pending":    "not-connected",
		"empty":      "not-connected",
	}
	for _, m := range cov.ControllersMissing {
		if wantReasons[m.Name] != m.Reason {
			t.Errorf("%s reason = %q, want %q", m.Name, m.Reason, wantReasons[m.Name])
		}
	}
}

func TestCoverage_QualityFlags(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "declared"},
			}, defaultEnv(), true, true, true, true, true, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	_, cov := Rollup(in, "", pluginrange.Expr{})
	if cov.ControllersStale != 1 {
		t.Errorf("controllersStale = %d, want 1", cov.ControllersStale)
	}
	if cov.ControllersDegraded != 1 {
		t.Errorf("controllersDegraded = %d, want 1", cov.ControllersDegraded)
	}
	if cov.ControllersTruncated != 1 {
		t.Errorf("controllersTruncated = %d, want 1", cov.ControllersTruncated)
	}
}

func TestCoverage_DetailStaleCount(t *testing.T) {
	// Create an envelope mismatch.
	env := defaultEnv()
	mismatchEnv := defaultEnv()
	mismatchEnv.Stale = true // flag differs

	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "declared"},
			}, env, false, false, false, false, false, "jenkins-api"),
			// This row has a mismatched envelope.
			{
				Cluster:     "local",
				Namespace:   "ns1",
				Name:        "b",
				Phase:       v1alpha1.ControllerPhaseConnected,
				HasInv:      true,
				Inv:         ControllerInventory{Plugins: []InstalledPlugin{{Name: "p", Version: "1", Class: "declared"}}, CollectedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC), Envelope: mismatchEnv},
				Source:      "jenkins-api",
				Envelope:    mismatchEnv,
				DetailStale: checkEnvelope(mismatchEnv, env.Hash, env.Source, false, false, false, false, false, false),
			},
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	_, cov := Rollup(in, "", pluginrange.Expr{})
	if cov.ControllersDetailStale != 1 {
		t.Errorf("controllersDetailStale = %d, want 1", cov.ControllersDetailStale)
	}
}

// ---------------------------------------------------------------------------
// Tests: Coverage byte-identical with and without q/affected
// ---------------------------------------------------------------------------

func TestCoverage_UnaffectedByQ(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	_, covNoQ := Rollup(in, "", pluginrange.Expr{})
	_, covQ := Rollup(in, "GIT", pluginrange.Expr{})

	// Coverage should be identical.
	if !coverageEqual(covNoQ, covQ) {
		t.Errorf("coverage differs with q filter:\n  without: %+v\n  with:    %+v", covNoQ, covQ)
	}
}

func TestCoverage_UnaffectedByAffected(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.0.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
			row("ns1", "b", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "git-client", Version: "4.1.0", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	_, covNoAffected := Rollup(in, "", pluginrange.Expr{})
	affected, _ := pluginrange.Parse(">=4.1.0")
	_, covAffected := Rollup(in, "", affected)

	if !coverageEqual(covNoAffected, covAffected) {
		t.Errorf("coverage differs with affected filter:\n  without: %+v\n  with:    %+v", covNoAffected, covAffected)
	}
}

// ---------------------------------------------------------------------------
// Tests: Envelope cross-check
// ---------------------------------------------------------------------------

func TestCheckEnvelope_HashMismatch(t *testing.T) {
	env := ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"}
	if !checkEnvelope(env, "xyz", "jenkins-api", false, false, false, false, false, false) {
		t.Error("hash mismatch should set detailStale")
	}
}

func TestCheckEnvelope_SourceOnlyMismatch(t *testing.T) {
	// Source differs but hash is the same.
	env := ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"}
	if !checkEnvelope(env, "abc", "filesystem", false, false, false, false, false, false) {
		t.Error("source-only mismatch with unchanged hash should set detailStale")
	}
}

func TestCheckEnvelope_BooleanOnlyMismatch(t *testing.T) {
	// Hash is the same, only a boolean flag differs.
	env := ClassifiedEnvelope{Hash: "abc", Stale: true}
	if !checkEnvelope(env, "abc", "", false, false, false, false, false, false) {
		t.Error("boolean-only mismatch with unchanged hash should set detailStale")
	}
}

func TestCheckEnvelope_AllMatch(t *testing.T) {
	env := ClassifiedEnvelope{
		Hash:                 "abc",
		Source:               "jenkins-api",
		Stale:                true,
		Degraded:             false,
		Truncated:            true,
		OptionalEdgesDropped: false,
		BootstrapApproximate: true,
		DriftTruncated:       false,
	}
	if checkEnvelope(env, "abc", "jenkins-api", true, false, true, false, true, false) {
		t.Error("all fields match: detailStale should be false")
	}
}

func TestCheckEnvelope_EachFlagIndependent(t *testing.T) {
	// Verify that each of the six boolean flags can independently trigger detailStale.
	base := ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api"}
	baseStatus := []bool{false, false, false, false, false, false}

	tests := []struct {
		label string
		env   ClassifiedEnvelope
	}{
		{"stale", ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api", Stale: true}},
		{"degraded", ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api", Degraded: true}},
		{"truncated", ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api", Truncated: true}},
		{"optionalEdgesDropped", ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api", OptionalEdgesDropped: true}},
		{"bootstrapApproximate", ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api", BootstrapApproximate: true}},
		{"driftTruncated", ClassifiedEnvelope{Hash: "abc", Source: "jenkins-api", DriftTruncated: true}},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			if !checkEnvelope(tt.env, base.Hash, base.Source, baseStatus[0], baseStatus[1], baseStatus[2], baseStatus[3], baseStatus[4], baseStatus[5]) {
				t.Errorf("%s mismatch should set detailStale", tt.label)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: No quality flag excludes a row
// ---------------------------------------------------------------------------

func TestQualityFlagsNeverExclude(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1", Class: "declared"},
			}, defaultEnv(), true, true, true, true, true, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, cov := Rollup(in, "", pluginrange.Expr{})
	if len(items) != 1 {
		t.Fatal("row with all quality flags should still appear in rollup")
	}
	if items[0].ControllerCount != 1 {
		t.Errorf("controllerCount = %d, want 1", items[0].ControllerCount)
	}
	if cov.ControllersReporting != 1 {
		t.Error("controller should be counted as reporting")
	}
}

// ---------------------------------------------------------------------------
// Tests: reason
// ---------------------------------------------------------------------------

func TestReason(t *testing.T) {
	tests := []struct {
		phase v1alpha1.ControllerPhase
		want  string
	}{
		{v1alpha1.ControllerPhaseConnected, "never-reported"},
		{v1alpha1.ControllerPhaseHibernated, "hibernated"},
		{v1alpha1.ControllerPhaseStopped, "stopped"},
		{v1alpha1.ControllerPhasePending, "not-connected"},
		{v1alpha1.ControllerPhaseProvisioning, "not-connected"},
		{v1alpha1.ControllerPhaseRunning, "not-connected"},
		{v1alpha1.ControllerPhaseFailed, "not-connected"},
		{"", "not-connected"},
		{"FuturePhase", "not-connected"},
	}
	for _, tt := range tests {
		got := reason(tt.phase)
		if got != tt.want {
			t.Errorf("reason(%q) = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: DrillItem fields
// ---------------------------------------------------------------------------

func TestDrill_AllFieldsPropagated(t *testing.T) {
	env := ClassifiedEnvelope{
		Hash:                 "abc",
		Source:               "jenkins-api",
		Stale:                true,
		Degraded:             true,
		Truncated:            true,
		OptionalEdgesDropped: true,
		BootstrapApproximate: true,
		DriftTruncated:       true,
	}
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "ctrl", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "p", Version: "1.0", Class: "unmanaged"},
			}, env, true, true, true, true, true, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, _, _ := Drill(in, "p", pluginrange.Expr{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	d := items[0]
	if d.Cluster != "local" {
		t.Errorf("Cluster = %q", d.Cluster)
	}
	if d.Namespace != "ns1" {
		t.Errorf("Namespace = %q", d.Namespace)
	}
	if d.Controller != "ctrl" {
		t.Errorf("Controller = %q", d.Controller)
	}
	if d.Version != "1.0" {
		t.Errorf("Version = %q", d.Version)
	}
	if d.Class != "unmanaged" {
		t.Errorf("Class = %q", d.Class)
	}
	if d.Source != "jenkins-api" {
		t.Errorf("Source = %q", d.Source)
	}
	if d.CollectedAt != "2025-01-15T10:00:00Z" {
		t.Errorf("CollectedAt = %q", d.CollectedAt)
	}
	if d.DetailPath != "/api/v1/clusters/local/controllers/ns1/ctrl/plugins" {
		t.Errorf("DetailPath = %q", d.DetailPath)
	}
	if !d.Stale {
		t.Error("Stale should be true")
	}
	if !d.Degraded {
		t.Error("Degraded should be true")
	}
	if !d.Truncated {
		t.Error("Truncated should be true")
	}
	if !d.OptionalEdgesDropped {
		t.Error("OptionalEdgesDropped should be true")
	}
	if !d.BootstrapApproximate {
		t.Error("BootstrapApproximate should be true")
	}
}

// ---------------------------------------------------------------------------
// Tests: Empty edge cases
// ---------------------------------------------------------------------------

func TestRollup_EmptyFleet(t *testing.T) {
	in := fleetInput{
		Rows:     []controllerRow{},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, _ := Rollup(in, "", pluginrange.Expr{})
	if len(items) != 0 {
		t.Errorf("empty fleet: expected 0 items, got %d", len(items))
	}
}

func TestRollup_ControllerWithNoPlugins(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, cov := Rollup(in, "", pluginrange.Expr{})
	if len(items) != 0 {
		t.Errorf("controller with zero plugins: expected 0 items, got %d", len(items))
	}
	if cov.ControllersReporting != 1 {
		t.Errorf("zero-plugin controller should still count as reporting: got %d", cov.ControllersReporting)
	}
}

// ---------------------------------------------------------------------------
// Tests: sort determinism for missing controllers
// ---------------------------------------------------------------------------

func TestCoverage_MissingControllersSorted(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns-b", "z", v1alpha1.ControllerPhaseConnected, false, nil, defaultEnv(), false, false, false, false, false, ""),
			row("ns-a", "a", v1alpha1.ControllerPhaseConnected, false, nil, defaultEnv(), false, false, false, false, false, ""),
			row("ns-a", "b", v1alpha1.ControllerPhaseConnected, false, nil, defaultEnv(), false, false, false, false, false, ""),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	_, cov := Rollup(in, "", pluginrange.Expr{})

	// Verify sorted: ns-a/a, ns-a/b, ns-b/z.
	if len(cov.ControllersMissing) != 3 {
		t.Fatalf("expected 3 missing, got %d", len(cov.ControllersMissing))
	}
	order := [][2]string{
		{cov.ControllersMissing[0].Namespace, cov.ControllersMissing[0].Name},
		{cov.ControllersMissing[1].Namespace, cov.ControllersMissing[1].Name},
		{cov.ControllersMissing[2].Namespace, cov.ControllersMissing[2].Name},
	}
	expected := [][2]string{
		{"ns-a", "a"},
		{"ns-a", "b"},
		{"ns-b", "z"},
	}
	if !slicesEqual(order, expected) {
		t.Errorf("missing controllers order: %v, want %v", order, expected)
	}
}

// ---------------------------------------------------------------------------
// Tests: fakeFleetInventory
// ---------------------------------------------------------------------------

func TestFakeFleetInventory_AbsentVsEmpty(t *testing.T) {
	emptyInv := ControllerInventory{
		Plugins:     []InstalledPlugin{},
		CollectedAt: time.Now(),
		Envelope:    defaultEnv(),
	}
	f := newFakeFleetInventory(
		map[types.NamespacedName]ControllerInventory{
			nn("has-empty-inv"): emptyInv,
		},
		[]types.NamespacedName{nn("no-inv")},
	)

	keys := []types.NamespacedName{
		nn("has-empty-inv"),
		nn("no-inv"),
		nn("never-heard-of"),
	}

	result := f.List(keys)

	// has-empty-inv should be present.
	if _, ok := result[nn("has-empty-inv")]; !ok {
		t.Error("has-empty-inv should be present (observed empty inventory)")
	}
	// no-inv should be absent.
	if _, ok := result[nn("no-inv")]; ok {
		t.Error("no-inv should be absent (no observed inventory)")
	}
	// never-heard-of should be absent.
	if _, ok := result[nn("never-heard-of")]; ok {
		t.Error("never-heard-of should be absent")
	}
}

// ---------------------------------------------------------------------------
// Tests: Rollup item sorting
// ---------------------------------------------------------------------------

func TestRollup_ItemsSortedByName(t *testing.T) {
	in := fleetInput{
		Rows: []controllerRow{
			row("ns1", "a", v1alpha1.ControllerPhaseConnected, true, []InstalledPlugin{
				{Name: "zzz", Version: "1", Class: "declared"},
				{Name: "aaa", Version: "1", Class: "declared"},
			}, defaultEnv(), false, false, false, false, false, "jenkins-api"),
		},
		Statuses: []ClusterFanoutStatus{{Name: "local", OK: true}},
	}
	items, _ := Rollup(in, "", pluginrange.Expr{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Name != "aaa" || items[1].Name != "zzz" {
		t.Errorf("items not sorted by name: %v", []string{items[0].Name, items[1].Name})
	}
}

// Force sort import (used in coverage).
var _ = sort.Strings

// coverageEqual compares two Coverage values, handling uncomparable slices.
func coverageEqual(a, b Coverage) bool {
	if a.Complete != b.Complete ||
		a.ControllersTotal != b.ControllersTotal ||
		a.ControllersReporting != b.ControllersReporting ||
		a.ControllersStale != b.ControllersStale ||
		a.ControllersDegraded != b.ControllersDegraded ||
		a.ControllersTruncated != b.ControllersTruncated ||
		a.ControllersDetailStale != b.ControllersDetailStale ||
		a.ClustersNotCovered != b.ClustersNotCovered {
		return false
	}
	if len(a.ControllersMissing) != len(b.ControllersMissing) {
		return false
	}
	for i := range a.ControllersMissing {
		if a.ControllersMissing[i] != b.ControllersMissing[i] {
			return false
		}
	}
	return true
}

// slicesEqual compares two [][2]string slices.
func slicesEqual(a, b [][2]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

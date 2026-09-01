package bundle

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
)

func TestCheckPluginPins_Conflict(t *testing.T) {
	pluginsYAML := "plugins:\n  - artifactId: git\n    version: 1.0.0\n"
	set := []pluginlock.PluginEntry{{ArtifactID: "git", Version: "2.0.0"}}

	report, err := CheckPluginPins(pluginsYAML, set)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %+v", len(report.Conflicts), report.Conflicts)
	}
	got := report.Conflicts[0]
	if got.ArtifactID != "git" || got.BundleVersion != "1.0.0" || got.SetVersion != "2.0.0" {
		t.Fatalf("unexpected conflict entry: %+v", got)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("expected no missing entries, got %+v", report.Missing)
	}
}

func TestCheckPluginPins_Missing(t *testing.T) {
	pluginsYAML := "plugins:\n  - artifactId: workflow-cps\n    version: 1.0.0\n"
	set := []pluginlock.PluginEntry{{ArtifactID: "git", Version: "2.0.0"}}

	report, err := CheckPluginPins(pluginsYAML, set)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %+v", report.Conflicts)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("expected 1 missing entry, got %d: %+v", len(report.Missing), report.Missing)
	}
	got := report.Missing[0]
	if got.ArtifactID != "workflow-cps" || got.BundleVersion != "1.0.0" {
		t.Fatalf("unexpected missing entry: %+v", got)
	}
}

func TestCheckPluginPins_UnpinnedExcludedFromConflictButCheckedForMissing(t *testing.T) {
	set := []pluginlock.PluginEntry{{ArtifactID: "git", Version: "2.0.0"}}

	// Unpinned entry ("") that matches an artifact in the set: no conflict, no missing.
	report, err := CheckPluginPins("plugins:\n  - artifactId: git\n    version: \"\"\n", set)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("expected no conflicts for unpinned matching entry, got %+v", report.Conflicts)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("expected no missing for unpinned matching entry, got %+v", report.Missing)
	}

	// Unpinned entry ("latest") absent from the set: no conflict, but still missing.
	report, err = CheckPluginPins("plugins:\n  - artifactId: workflow-cps\n    version: latest\n", set)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("expected no conflicts for unpinned absent entry, got %+v", report.Conflicts)
	}
	if len(report.Missing) != 1 || report.Missing[0].ArtifactID != "workflow-cps" {
		t.Fatalf("expected workflow-cps reported missing, got %+v", report.Missing)
	}
}

func TestCheckPluginPins_MalformedYAML(t *testing.T) {
	set := []pluginlock.PluginEntry{{ArtifactID: "git", Version: "2.0.0"}}

	report, err := CheckPluginPins("plugins: [this is not", set)
	if err == nil {
		t.Fatal("expected an error for malformed plugins.yaml")
	}
	if len(report.Conflicts) != 0 || len(report.Missing) != 0 {
		t.Fatalf("expected zero-value report on error, got %+v", report)
	}
}

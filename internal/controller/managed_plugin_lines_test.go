package controller

import (
	"reflect"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
)

func cellJobDSLCoreSet() []pluginlock.PluginEntry {
	return []pluginlock.PluginEntry{
		{ArtifactID: "git", Version: "1"},
		{ArtifactID: "job-dsl", Version: "3732.x"},
		{ArtifactID: "kubernetes", Version: "2"},
	}
}

func TestManagedPluginLines_CellDropsJobDSL(t *testing.T) {
	cr := &v1alpha1.Controller{}
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	resolved := &bundle.MaterializedBundle{
		PluginsYAML: "plugins:\n- artifactId: job-dsl\n  version: \"3732.x\"\n- artifactId: slack\n  version: \"1\"\n",
	}

	got := managedPluginLines(cr, resolved, cellJobDSLCoreSet())
	want := []string{"git:1", "kubernetes:2", "slack:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managedPluginLines() = %v, want %v", got, want)
	}
}

func TestManagedPluginLines_CellDropsJobDSLFromPluginSpec(t *testing.T) {
	cr := &v1alpha1.Controller{}
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	cr.Spec.PluginSpec = &v1alpha1.PluginSpec{
		Entries: []v1alpha1.PluginEntry{
			{ArtifactId: "job-dsl", Version: "3732.x"},
			{ArtifactId: "slack", Version: "1"},
		},
	}

	got := managedPluginLines(cr, nil, cellJobDSLCoreSet())
	want := []string{"git:1", "kubernetes:2", "slack:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managedPluginLines() = %v, want %v", got, want)
	}
}

func TestCellInstalledSet_CellPinOfExcludedPluginIsNoConflict(t *testing.T) {
	resolved := &bundle.MaterializedBundle{
		PluginsYAML: "plugins:\n- artifactId: job-dsl\n  version: \"1\"\n",
	}

	cell := &v1alpha1.Controller{}
	cell.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	if conflict := pluginVersionConflict(cell, resolved, cellInstalledSet(cell, cellJobDSLCoreSet())); conflict != "" {
		t.Fatalf("cell pluginVersionConflict() = %q, want no conflict", conflict)
	}

	plain := &v1alpha1.Controller{}
	if conflict := pluginVersionConflict(plain, resolved, cellInstalledSet(plain, cellJobDSLCoreSet())); conflict == "" {
		t.Fatal("non-cell pluginVersionConflict() = no conflict, want a job-dsl version conflict")
	}
}

func TestManagedPluginLines_NonCellKeepsJobDSL(t *testing.T) {
	cr := &v1alpha1.Controller{}
	resolved := &bundle.MaterializedBundle{
		PluginsYAML: "plugins:\n- artifactId: job-dsl\n  version: \"3732.x\"\n- artifactId: slack\n  version: \"1\"\n",
	}

	got := managedPluginLines(cr, resolved, cellJobDSLCoreSet())
	want := []string{"git:1", "job-dsl:3732.x", "kubernetes:2", "slack:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managedPluginLines() = %v, want %v", got, want)
	}
}

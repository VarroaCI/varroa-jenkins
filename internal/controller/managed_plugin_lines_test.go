package controller

import (
	"reflect"
	"slices"
	"strings"
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

func TestManagedPluginLines_CellKeepsUtilityPins(t *testing.T) {
	cr := &v1alpha1.Controller{}
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	coreSet := append(cellJobDSLCoreSet(),
		pluginlock.PluginEntry{ArtifactID: "pipeline-utility-steps", Version: "3.810.va_7672d206740"},
		pluginlock.PluginEntry{ArtifactID: "ws-cleanup", Version: "0.49"},
		pluginlock.PluginEntry{ArtifactID: "ssh-agent", Version: "433.v9e73d67c3f74"},
		pluginlock.PluginEntry{ArtifactID: "timestamper", Version: "1.30"},
	)

	got := managedPluginLines(cr, nil, coreSet)
	want := []string{
		"git:1",
		"kubernetes:2",
		"pipeline-utility-steps:3.810.va_7672d206740",
		"ws-cleanup:0.49",
		"ssh-agent:433.v9e73d67c3f74",
		"timestamper:1.30",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managedPluginLines() = %v, want %v", got, want)
	}
}

// TestManagedPluginLines_KeepsQualityPins checks that the run quality pins
// reach both cells and classic controllers for each shipped core set.
func TestManagedPluginLines_KeepsQualityPins(t *testing.T) {
	cases := map[string]*v1alpha1.Controller{
		"cell":    {Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}}},
		"classic": {},
	}
	for name, cr := range cases {
		for _, v := range []string{"2.555", "2.570"} {
			t.Run(name+"/"+v, func(t *testing.T) {
				set, _ := pluginlock.Resolve(v)
				lines := managedPluginLines(cr, nil, set)
				for _, prefix := range []string{"coverage:", "warnings-ng:", "forensics-api:", "data-tables-api:", "analysis-model-api:"} {
					if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) }) {
						t.Errorf("managedPluginLines() has no line starting %q", prefix)
					}
				}
			})
		}
	}
}

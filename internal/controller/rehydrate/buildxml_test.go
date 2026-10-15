package rehydrate

import (
	"strings"
	"testing"
	"time"
)

func sampleRecord() Record {
	queued := time.UnixMilli(1_700_000_000_000)
	started := time.UnixMilli(1_700_000_001_000)
	duration := int64(45_000)
	return Record{
		BuildNumber:      7,
		QueuedAt:         &queued,
		StartedAt:        &started,
		DurationMs:       &duration,
		Result:           "SUCCESS",
		CauseDescription: "Started by user ada",
		Params: map[string]string{
			"BRANCH": "main",
			"SECRET": "s3cr3t",
		},
		MaskedParameterNames: []string{"SECRET"},
	}
}

// TestRenderBuildXML_Elements checks every element design.md D1's
// table names, one sub-test per element (task 5.1).
func TestRenderBuildXML_Elements(t *testing.T) {
	rec := sampleRecord()
	out, err := RenderBuildXML(rec, Options{}, "")
	if err != nil {
		t.Fatalf("RenderBuildXML: %v", err)
	}

	var doc flowBuildXML
	mustUnmarshal(t, out, &doc)

	t.Run("actions", func(t *testing.T) {
		if doc.Actions.ParametersAction == nil {
			t.Fatal("expected a ParametersAction for a run with an unmasked parameter")
		}
	})
	t.Run("queueId", func(t *testing.T) {
		if doc.QueueID != -1 {
			t.Errorf("queueId = %d, want -1 (no queue id in the record store)", doc.QueueID)
		}
	})
	t.Run("timestamp", func(t *testing.T) {
		if doc.Timestamp != rec.QueuedAt.UnixMilli() {
			t.Errorf("timestamp = %d, want %d", doc.Timestamp, rec.QueuedAt.UnixMilli())
		}
	})
	t.Run("startTime", func(t *testing.T) {
		if doc.StartTime != rec.StartedAt.UnixMilli() {
			t.Errorf("startTime = %d, want %d", doc.StartTime, rec.StartedAt.UnixMilli())
		}
	})
	t.Run("result", func(t *testing.T) {
		if doc.Result != "SUCCESS" {
			t.Errorf("result = %q, want SUCCESS", doc.Result)
		}
	})
	t.Run("description", func(t *testing.T) {
		if doc.Description != rec.CauseDescription {
			t.Errorf("description = %q, want %q", doc.Description, rec.CauseDescription)
		}
	})
	t.Run("duration", func(t *testing.T) {
		if doc.Duration != *rec.DurationMs {
			t.Errorf("duration = %d, want %d", doc.Duration, *rec.DurationMs)
		}
	})
	t.Run("charset", func(t *testing.T) {
		if doc.Charset != "UTF-8" {
			t.Errorf("charset = %q, want UTF-8", doc.Charset)
		}
	})
	t.Run("keepLog", func(t *testing.T) {
		if doc.KeepLog {
			t.Error("keepLog = true, want false")
		}
	})
	t.Run("artifactManager omitted by default", func(t *testing.T) {
		if doc.ArtifactManager != nil {
			t.Error("artifactManager present without HasArtifactManagerS3")
		}
	})
	t.Run("checkouts omitted with no change", func(t *testing.T) {
		if doc.Checkouts != nil {
			t.Error("checkouts present for a run with no recorded change")
		}
	})
	t.Run("completed", func(t *testing.T) {
		if !doc.Completed {
			t.Error("completed = false, want true")
		}
	})
	t.Run("no execution element", func(t *testing.T) {
		if strings.Contains(string(out), "<execution") {
			t.Error("document holds an <execution> element, which D1 forbids")
		}
	})
	t.Run("no culprits element", func(t *testing.T) {
		if strings.Contains(string(out), "<culprits") {
			t.Error("document holds a <culprits> element, which D1 forbids")
		}
	})
}

func TestRenderBuildXML_InterruptedMapsToAborted(t *testing.T) {
	rec := sampleRecord()
	rec.Result = "INTERRUPTED"
	out, err := RenderBuildXML(rec, Options{}, "")
	if err != nil {
		t.Fatalf("RenderBuildXML: %v", err)
	}
	var doc flowBuildXML
	mustUnmarshal(t, out, &doc)
	if doc.Result != "ABORTED" {
		t.Errorf("result = %q, want ABORTED (Jenkins Result has no INTERRUPTED value)", doc.Result)
	}
}

func TestRenderBuildXML_MaskedParameterHasNoValue(t *testing.T) {
	rec := sampleRecord()
	out, err := RenderBuildXML(rec, Options{}, "")
	if err != nil {
		t.Fatalf("RenderBuildXML: %v", err)
	}
	if strings.Contains(string(out), "s3cr3t") {
		t.Error("document carries the masked parameter's value")
	}
	if strings.Contains(string(out), "SECRET") {
		t.Error("document carries the masked parameter's name")
	}
}

func TestRenderBuildXML_ParameterDefinitionNamesRepeatsRenderedNames(t *testing.T) {
	rec := sampleRecord()
	rec.Params["EXTRA"] = "x"
	out, err := RenderBuildXML(rec, Options{}, "")
	if err != nil {
		t.Fatalf("RenderBuildXML: %v", err)
	}
	var doc flowBuildXML
	mustUnmarshal(t, out, &doc)
	pa := doc.Actions.ParametersAction
	if pa == nil {
		t.Fatal("expected a ParametersAction")
	}
	rendered := make([]string, 0, len(pa.Parameters))
	for _, p := range pa.Parameters {
		rendered = append(rendered, p.Name)
	}
	if len(pa.ParameterDefinitionNames) != len(rendered) {
		t.Fatalf("parameterDefinitionNames has %d names, want %d", len(pa.ParameterDefinitionNames), len(rendered))
	}
	for i, name := range rendered {
		if pa.ParameterDefinitionNames[i] != name {
			t.Errorf("parameterDefinitionNames[%d] = %q, want %q", i, pa.ParameterDefinitionNames[i], name)
		}
	}
}

// TestRenderBuildXML_ArtifactManager covers task 5.2's two branches.
func TestRenderBuildXML_ArtifactManager(t *testing.T) {
	rec := sampleRecord()

	t.Run("present when the profile pins artifact-manager-s3", func(t *testing.T) {
		out, err := RenderBuildXML(rec, Options{HasArtifactManagerS3: true}, "")
		if err != nil {
			t.Fatalf("RenderBuildXML: %v", err)
		}
		var doc flowBuildXML
		mustUnmarshal(t, out, &doc)
		if doc.ArtifactManager == nil {
			t.Fatal("expected an artifactManager element")
		}
		if doc.ArtifactManager.Class != artifactManagerClass {
			t.Errorf("artifactManager class = %q, want %q", doc.ArtifactManager.Class, artifactManagerClass)
		}
		if doc.ArtifactManager.Plugin != artifactManagerPlugin {
			t.Errorf("artifactManager plugin = %q, want %q", doc.ArtifactManager.Plugin, artifactManagerPlugin)
		}
		if doc.ArtifactManager.Provider.Class != s3BlobStoreClass {
			t.Errorf("provider class = %q, want %q", doc.ArtifactManager.Provider.Class, s3BlobStoreClass)
		}
	})

	t.Run("absent when the profile does not pin artifact-manager-s3", func(t *testing.T) {
		out, err := RenderBuildXML(rec, Options{HasArtifactManagerS3: false}, "")
		if err != nil {
			t.Fatalf("RenderBuildXML: %v", err)
		}
		var doc flowBuildXML
		mustUnmarshal(t, out, &doc)
		if doc.ArtifactManager != nil {
			t.Error("artifactManager present without HasArtifactManagerS3")
		}
	})
}

func TestRenderBuildXML_RequiresChangelogPathWhenChangesPresent(t *testing.T) {
	rec := sampleRecord()
	rec.Changes = []Change{{CommitID: "abc", AuthorID: "ada", Message: "fix", Timestamp: time.Now()}}
	if _, err := RenderBuildXML(rec, Options{}, ""); err == nil {
		t.Fatal("expected an error for changes with no changelog path")
	}
}

func TestRenderBuildXML_ChecksoutsNamesTheChangelogPath(t *testing.T) {
	rec := sampleRecord()
	rec.Changes = []Change{{CommitID: "abc", AuthorID: "ada", Message: "fix", Timestamp: time.Now()}}
	const path = "/var/jenkins_home/jobs/team/builds/7/changelog0.xml"
	out, err := RenderBuildXML(rec, Options{}, path)
	if err != nil {
		t.Fatalf("RenderBuildXML: %v", err)
	}
	var doc flowBuildXML
	mustUnmarshal(t, out, &doc)
	if doc.Checkouts == nil {
		t.Fatal("expected a checkouts element")
	}
	if doc.Checkouts.Checkout.ChangelogFile != path {
		t.Errorf("changelogFile = %q, want %q", doc.Checkouts.Checkout.ChangelogFile, path)
	}
	if doc.Checkouts.Checkout.SCM.Class != varroaRehydratedSCMClass {
		t.Errorf("scm class = %q, want %q", doc.Checkouts.Checkout.SCM.Class, varroaRehydratedSCMClass)
	}
}

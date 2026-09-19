package rehydrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
)

// TestRenderBuildXML_GoldenFixtures compares the rendered document
// against one golden build.xml per operator-seeded version profile
// (task 5.3). The golden files under testdata/ are this batch's own
// renderer output, not a live-Jenkins capture: design.md D1a states
// that a live capture (task 12.4) wins over the design when the two
// differ, and these fixtures exist to catch an unintended format
// change, not to assert live-Jenkins fidelity.
//
// Run with UPDATE_GOLDEN=1 to (re)write the fixtures from the current
// renderer output.
func TestRenderBuildXML_GoldenFixtures(t *testing.T) {
	seeds := pluginlock.Seeds()
	if len(seeds) == 0 {
		t.Fatal("no operator-seeded version profiles found")
	}

	queued := time.UnixMilli(1_700_000_000_000)
	started := time.UnixMilli(1_700_000_005_000)
	duration := int64(45_000)
	rec := Record{
		BuildNumber:      12,
		QueuedAt:         &queued,
		StartedAt:        &started,
		DurationMs:       &duration,
		Result:           "SUCCESS",
		CauseDescription: "Started by user ada",
		Params:           map[string]string{"BRANCH": "main"},
		Changes: []Change{
			{CommitID: "3f1c2ab", AuthorID: "ada", Message: "fix the queue lock", Timestamp: time.UnixMilli(1_700_000_000_000)},
		},
	}
	const jenkinsHome = "/var/jenkins_home"
	changelogPath := ChangelogAbsolutePath(jenkinsHome, "team/api", rec.BuildNumber)

	for _, seed := range seeds {
		t.Run(seed.Version, func(t *testing.T) {
			// pluginlock.SeedProfile.Plugins is the seed's raw
			// plugins.yaml content; a direct substring check avoids
			// depending on the version-resolution chain
			// coreSetForCr walks (profile ConfigMap -> pluginlock.Resolve),
			// which needs a live cluster and is out of this package's
			// scope.
			opts := Options{HasArtifactManagerS3: strings.Contains(seed.Plugins, "artifact-manager-s3")}

			got, err := RenderBuildXML(rec, opts, changelogPath)
			if err != nil {
				t.Fatalf("RenderBuildXML: %v", err)
			}

			goldenPath := filepath.Join("testdata", "build_"+strings.ReplaceAll(seed.Version, ".", "_")+".xml")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v", goldenPath, err)
			}
			if string(got) != string(want) {
				t.Errorf("rendered document differs from %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
			}
			// The store's source column has no rehydration meaning. The
			// rendered document must name no definition path, no library,
			// and no pod template.
			for _, field := range []string{"definition_path", "libraries", "pod_templates"} {
				if strings.Contains(string(got), field) {
					t.Errorf("rendered %s carries the source field %q", goldenPath, field)
				}
			}
		})
	}
}

package pipelinetemplate

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// pluginCorpusMirrorDir is the plugin's copy of the shared golden corpus,
// relative to this package's directory. D15 names one canonical corpus
// under this package, walked by the plugin and the browser from their own
// test suites; this test proves the plugin's copy stays byte-identical to
// it, so a corpus change here cannot silently drift out of parity with
// the mirror the Java validator's own tests actually walk.
const pluginCorpusMirrorDir = "../../../plugin/src/test/resources/org/jenkinsci/plugins/varroamiteauth/template/schema-corpus"

// listCorpusFiles returns every regular file under root, as paths
// relative to root, sorted.
func listCorpusFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

// TestPluginCorpusMirrorIsByteIdentical proves 🟡3 of review round 1: the
// plugin's copy of the shared schema corpus under
// plugin/src/test/resources/.../schema-corpus/ carries exactly the same
// files, with exactly the same bytes, as the canonical corpus this
// package owns. A future canonical change that does not also update the
// plugin's copy fails this test instead of silently breaking the
// three-validator parity claim.
func TestPluginCorpusMirrorIsByteIdentical(t *testing.T) {
	if _, err := os.Stat(pluginCorpusMirrorDir); err != nil {
		t.Fatalf("plugin corpus mirror not found at %s: %v", pluginCorpusMirrorDir, err)
	}

	canonicalFiles := listCorpusFiles(t, corpusDir)
	mirrorFiles := listCorpusFiles(t, pluginCorpusMirrorDir)

	canonicalSet := make(map[string]bool, len(canonicalFiles))
	for _, f := range canonicalFiles {
		canonicalSet[f] = true
	}
	mirrorSet := make(map[string]bool, len(mirrorFiles))
	for _, f := range mirrorFiles {
		mirrorSet[f] = true
	}

	for _, f := range canonicalFiles {
		if !mirrorSet[f] {
			t.Errorf("canonical corpus file %q has no plugin mirror copy", f)
		}
	}
	for _, f := range mirrorFiles {
		if !canonicalSet[f] {
			t.Errorf("plugin mirror file %q names no canonical corpus file", f)
		}
	}

	for _, f := range canonicalFiles {
		if !mirrorSet[f] {
			continue
		}
		canonical, err := os.ReadFile(filepath.Join(corpusDir, f))
		if err != nil {
			t.Errorf("read canonical %s: %v", f, err)
			continue
		}
		mirror, err := os.ReadFile(filepath.Join(pluginCorpusMirrorDir, f))
		if err != nil {
			t.Errorf("read mirror %s: %v", f, err)
			continue
		}
		if !bytes.Equal(canonical, mirror) {
			t.Errorf("%s: plugin mirror differs from the canonical corpus", f)
		}
	}
}

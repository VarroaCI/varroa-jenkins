package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResumeLogNextSeqFileMatchesThePlugin pins the file name across the
// language boundary. The plugin reads the file the mite writes, and a
// different name on one side keeps both sides' own tests green while
// every moved build starts its log at 0 again.
func TestResumeLogNextSeqFileMatchesThePlugin(t *testing.T) {
	src := filepath.Join("..", "..", "plugin", "src", "main", "java", "org", "jenkinsci", "plugins",
		"varroamiteauth", "VarroaLogStorageFactory.java")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read plugin source: %v", err)
	}
	want := `static final String LOG_NEXT_SEQ_FILE = "` + resumeLogNextSeqFile + `";`
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s does not declare %s", src, want)
	}
}

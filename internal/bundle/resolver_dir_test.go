package bundle

import (
	"context"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
)

func TestMaterialize_ReportsDirAndCommitSHA(t *testing.T) {
	t.Parallel()
	bareURL, commit := newBareFixture(t)
	_ = commit("bundle.yaml", `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
`)
	sha := commit("jenkins.yaml", "jenkins: {}")

	r := NewResolver(noop.NewMeterProvider(), t.TempDir())
	r.cloner.AllowLocalTransportForTest()

	cloneDir := filepath.Join(t.TempDir(), "clone")
	mat, err := r.Materialize(context.Background(), bareURL, ".", "main", cloneDir, nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if mat.CommitSHA != sha {
		t.Errorf("CommitSHA = %q, want %q", mat.CommitSHA, sha)
	}
	wantDir, err := filepath.EvalSymlinks(cloneDir)
	if err != nil {
		t.Fatalf("eval clone dir: %v", err)
	}
	gotDir, err := filepath.EvalSymlinks(mat.Dir)
	if err != nil {
		t.Fatalf("eval Dir %q: %v", mat.Dir, err)
	}
	if gotDir != wantDir {
		t.Errorf("Dir = %q, want %q", gotDir, wantDir)
	}
}

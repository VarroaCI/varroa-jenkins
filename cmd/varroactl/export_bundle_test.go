package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

const exportTestBundleYAML = `id: "test"
version: "1"
apiVersion: "1"
jcasc:
  - "jenkins.yaml"
`

// newExportBundleRepo builds a bare repository whose main branch holds two
// bundles, alpha and beta, and a README. It returns the file:// URL of the
// bare repository and the commit SHA of main.
func newExportBundleRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	bare := filepath.Join(dir, "bare.git")

	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Env = env
		out, err := c.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(work, "alpha"), 0o755))
	must(os.MkdirAll(filepath.Join(work, "beta"), 0o755))
	files := map[string]string{
		"README.md":          "readme\n",
		"alpha/bundle.yaml":  exportTestBundleYAML,
		"alpha/jenkins.yaml": "jenkins: {}\n",
		"beta/bundle.yaml":   exportTestBundleYAML,
		"beta/jenkins.yaml":  "jenkins: {}\n",
	}
	for name, body := range files {
		must(os.WriteFile(filepath.Join(work, name), []byte(body), 0o644))
	}

	run("-C", work, "init")
	run("-C", work, "symbolic-ref", "HEAD", "refs/heads/main")
	run("-C", work, "add", ".")
	run("-C", work, "commit", "-m", "fixture")
	head := run("-C", work, "rev-parse", "HEAD")
	run("clone", "--bare", work, bare)

	return "file://" + bare, head
}

// readExportLayer pulls the manifest under ref and returns the sorted names
// of the regular files in its bundle layer, plus the manifest itself.
func readExportLayer(t *testing.T, store oci.BlobStore, ref string) ([]string, oci.Manifest) {
	t.Helper()
	ctx := context.Background()
	m, err := store.Pull(ctx, ref)
	if err != nil {
		t.Fatalf("pull %q: %v", ref, err)
	}
	if len(m.Layers) != 1 {
		t.Fatalf("manifest %q has %d layers, want 1", ref, len(m.Layers))
	}
	rc, err := store.FetchBlob(ctx, m.Layers[0].Digest)
	if err != nil {
		t.Fatalf("fetch layer: %v", err)
	}
	defer func() { _ = rc.Close() }()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("gunzip layer: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			names = append(names, hdr.Name)
		}
	}
	sort.Strings(names)
	return names, m
}

func TestExportBundle_PacksOnlyPath(t *testing.T) {
	testSetup(t)

	repoURL, headSHA := newExportBundleRepo(t)

	prev := newExportBundleResolver
	newExportBundleResolver = func(workDir string) *bundle.Resolver {
		r := prev(workDir)
		r.Cloner().AllowLocalTransportForTest()
		return r
	}
	t.Cleanup(func() { newExportBundleResolver = prev })

	outDir := filepath.Join(t.TempDir(), "out")
	for _, p := range []string{"alpha", "beta"} {
		root := newRootCmd()
		root.SetArgs([]string{"export", "bundle",
			"--repo", repoURL,
			"--path", p,
			"--revision", "main",
			"--to", "dir://" + outDir,
			"-o", "json",
		})
		if err := root.Execute(); err != nil {
			t.Fatalf("export bundle --path %s: %v", p, err)
		}
	}

	store, err := oci.NewLayoutStore(outDir)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}

	refs := map[string]string{
		"alpha": exportBundleRef(repoURL, "alpha"),
		"beta":  exportBundleRef(repoURL, "beta"),
	}
	if refs["alpha"] == refs["beta"] {
		t.Fatalf("alpha and beta share ref %q", refs["alpha"])
	}

	for _, p := range []string{"alpha", "beta"} {
		ref := refs[p]
		if !strings.HasSuffix(ref, "-"+p) {
			t.Errorf("ref %q does not end with -%s", ref, p)
		}

		names, m := readExportLayer(t, store, ref)
		want := []string{"bundle.yaml", "jenkins.yaml"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Errorf("%s layer entries = %v, want %v", p, names, want)
		}

		if got := m.Annotations["dev.varroa.bundle.path"]; got != p {
			t.Errorf("%s bundle.path annotation = %q, want %q", p, got, p)
		}
		if got := m.Annotations["org.opencontainers.image.revision"]; got != headSHA {
			t.Errorf("%s revision annotation = %q, want %q", p, got, headSHA)
		}
		if got := m.Annotations["org.opencontainers.image.source"]; got != repoURL {
			t.Errorf("%s source annotation = %q, want %q", p, got, repoURL)
		}
	}
}

func TestExportBundleRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, repo, path, want string
	}{
		{"root of https repo", "https://github.com/o/r.git", ".", "github.com-o-r"},
		{"nested path with dot slash", "https://github.com/o/r.git", "./b/c/", "github.com-o-r-b-c"},
		{"https credentials are dropped", "https://user:tok@github.com/o/r.git", "b", "github.com-o-r-b"},
		{"scp-style remote", "git@github.com:o/r.git", "b", "github.com-o-r-b"},
		{"ssh URL", "ssh://git@host/o/r", ".", "host-o-r"},
		{"empty path is the root", "https://github.com/o/r", "", "github.com-o-r"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := exportBundleRef(tc.repo, tc.path)
			if got != tc.want {
				t.Errorf("exportBundleRef(%q, %q) = %q, want %q", tc.repo, tc.path, got, tc.want)
			}
			if strings.Contains(got, "tok") {
				t.Errorf("ref %q contains credentials", got)
			}
		})
	}
}

func TestStripRepoUserinfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"https://user:tok@github.com/o/r.git", "https://github.com/o/r.git"},
		{"https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"https://u:p%zz@github.com/o/r.git", "https://github.com/o/r.git"},
		{"https://u:pa#ss@github.com/o/r.git", "https://github.com/o/r.git"},
		{"https://u:p/w@github.com/o/r.git", "https://github.com/o/r.git"},
		{"https://u:tok@github.com/o/r.git?x=1", "https://github.com/o/r.git"},
		{"https://", "https://"},
		{"https://github.com/o/r.git?token=abc#frag", "https://github.com/o/r.git"},
	}
	for _, tc := range tests {
		if got := stripRepoUserinfo(tc.in); got != tc.want {
			t.Errorf("stripRepoUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

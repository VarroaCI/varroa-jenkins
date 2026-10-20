package challengecorpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCorpus writes manifest.yaml and creates each case directory listed in dirs.
func writeCorpus(t *testing.T, manifest string, dirs ...string) string {
	t.Helper()
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "manifest.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "cases"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, "cases", d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

const oneCase = `cases:
  - name: alpha
    tier: breadth
    status: supported
`

func TestLoad_Consistent(t *testing.T) {
	base := writeCorpus(t, oneCase, "breadth/alpha")
	mf, err := Load(base)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(mf.Cases) != 1 {
		t.Fatalf("got %d cases, want 1", len(mf.Cases))
	}
	if got, want := mf.Cases[0].Dir(base), filepath.Join(base, "cases", "breadth", "alpha"); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestLoad_OrphanDirectory(t *testing.T) {
	base := writeCorpus(t, oneCase, "breadth/alpha", "breadth/orphan")
	_, err := Load(base)
	if err == nil || !strings.Contains(err.Error(), "cases/breadth/orphan: case directory has no manifest.yaml entry") {
		t.Fatalf("Load error = %v, want orphan directory error", err)
	}
}

func TestLoad_MissingDirectory(t *testing.T) {
	base := writeCorpus(t, oneCase)
	_, err := Load(base)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Load error = %v, want missing directory error", err)
	}
}

func TestLoad_DuplicateEntry(t *testing.T) {
	manifest := oneCase + `  - name: alpha
    tier: breadth
    status: supported
`
	base := writeCorpus(t, manifest, "breadth/alpha")
	_, err := Load(base)
	if err == nil || !strings.Contains(err.Error(), "manifest entry breadth/alpha: duplicate entry") {
		t.Fatalf("Load error = %v, want duplicate entry error", err)
	}
}

func TestLoad_UnknownStatus(t *testing.T) {
	manifest := `cases:
  - name: alpha
    tier: breadth
    status: supportd
`
	base := writeCorpus(t, manifest, "breadth/alpha")
	_, err := Load(base)
	if err == nil || !strings.Contains(err.Error(), `unknown status "supportd"`) {
		t.Fatalf("Load error = %v, want unknown status error", err)
	}
}

func TestLoad_UnknownTierDirectory(t *testing.T) {
	base := writeCorpus(t, oneCase, "breadth/alpha", "extra/x")
	_, err := Load(base)
	if err == nil || !strings.Contains(err.Error(), "cases/extra: unknown tier directory") {
		t.Fatalf("Load error = %v, want unknown tier directory error", err)
	}
}

func TestLoad_ReportsAllProblems(t *testing.T) {
	base := writeCorpus(t, `cases:
  - name: gone
    tier: breadth
    status: supported
`, "breadth/orphan")
	_, err := Load(base)
	if err == nil {
		t.Fatal("Load returned nil error")
	}
	msg := err.Error()
	for _, want := range []string{
		"cases/breadth/orphan: case directory has no manifest.yaml entry",
		"manifest entry breadth/gone: case directory cases/breadth/gone does not exist",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestLoad_RealCorpus(t *testing.T) {
	mf, err := Load("../testdata/challenge")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(mf.Cases) == 0 {
		t.Fatal("no cases loaded from the real corpus")
	}
}

func TestLoad_NameNotOneComponent(t *testing.T) {
	manifest := `cases:
  - name: ../alpha
    tier: breadth
    status: supported
`
	base := writeCorpus(t, manifest, "breadth/alpha")
	_, err := Load(base)
	if err == nil || !strings.Contains(err.Error(), "name must be one directory component") {
		t.Fatalf("Load error = %v, want one-component error", err)
	}
}

func TestLoad_CaseIsFile(t *testing.T) {
	base := writeCorpus(t, oneCase, "breadth")
	if err := os.WriteFile(filepath.Join(base, "cases", "breadth", "alpha"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(base)
	if err == nil || !strings.Contains(err.Error(), "cases/breadth/alpha is not a directory") {
		t.Fatalf("Load error = %v, want not-a-directory error", err)
	}
}

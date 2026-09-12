package pipelinetemplate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// corpusDir is the shared golden corpus D15 describes. The plugin and the
// browser walk the same fixture files from their own test suites, so a
// change here changes the parity claim for all three.
const corpusDir = "testdata/schema-corpus"

// subsetCase is one manifest entry under schema-corpus/subset/. Message,
// when set, is the exact finding text CheckSchema emits for this case: the
// plugin's own corpus runner asserts the same exact text, so a reject case
// carries Message only when Go and the plugin agree on it byte for byte.
// A reject case whose finding text embeds a value the two validators
// render differently keeps Contains alone.
type subsetCase struct {
	ID       string `yaml:"id"`
	File     string `yaml:"file"`
	Verdict  string `yaml:"verdict"`
	Contains string `yaml:"contains"`
	Message  string `yaml:"message"`
}

// subsetManifest is schema-corpus/subset/manifest.yaml.
type subsetManifest struct {
	Cases []subsetCase `yaml:"cases"`
}

// valuesCase is one manifest entry under schema-corpus/values/. Message
// carries the same meaning as subsetCase.Message: the exact finding text
// ValidateValues emits, asserted only when Go and the plugin agree on it
// byte for byte. findingsFromValidationError renders every values finding
// from the schema library's typed ErrorKind into the text the plugin's
// hand-written validator already emits, so every reject case sets Message.
type valuesCase struct {
	ID             string         `yaml:"id"`
	Schema         string         `yaml:"schema"`
	Values         string         `yaml:"values"`
	Verdict        string         `yaml:"verdict"`
	Contains       string         `yaml:"contains"`
	Message        string         `yaml:"message"`
	ExpectDefaults map[string]any `yaml:"expectDefaults"`
}

// valuesManifest is schema-corpus/values/manifest.yaml.
type valuesManifest struct {
	Cases []valuesCase `yaml:"cases"`
}

func loadYAML[T any](t *testing.T, path string) T {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v T
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return v
}

func findingsText(findings []Finding) string {
	parts := make([]string, len(findings))
	for i, f := range findings {
		parts[i] = f.String()
	}
	return strings.Join(parts, "; ")
}

// TestSubsetCorpus proves every schema-corpus/subset/ case gets the verdict
// its manifest entry declares from CheckSchema, the Go-side subset and
// default checker.
func TestSubsetCorpus(t *testing.T) {
	dir := filepath.Join(corpusDir, "subset")
	manifest := loadYAML[subsetManifest](t, filepath.Join(dir, "manifest.yaml"))
	if len(manifest.Cases) == 0 {
		t.Fatal("subset manifest holds no cases")
	}
	for _, c := range manifest.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			schema := loadYAML[map[string]any](t, filepath.Join(dir, c.File))
			findings := CheckSchema(schema)
			text := findingsText(findings)
			switch c.Verdict {
			case "accept":
				if len(findings) != 0 {
					t.Errorf("expected accept, got findings: %s", text)
				}
			case "reject":
				if len(findings) == 0 {
					t.Errorf("expected reject, got none")
				} else if c.Message != "" {
					if text != c.Message {
						t.Errorf("expected the exact finding %q, got: %s", c.Message, text)
					}
				} else if c.Contains != "" && !strings.Contains(text, c.Contains) {
					t.Errorf("expected findings to mention %q, got: %s", c.Contains, text)
				}
			default:
				t.Fatalf("unknown verdict %q", c.Verdict)
			}
		})
	}
}

// TestValuesCorpus proves every schema-corpus/values/ case gets the verdict
// its manifest entry declares from ValidateValues, which every one of the
// three validators also runs against the same fixture files.
func TestValuesCorpus(t *testing.T) {
	dir := filepath.Join(corpusDir, "values")
	manifest := loadYAML[valuesManifest](t, filepath.Join(dir, "manifest.yaml"))
	if len(manifest.Cases) == 0 {
		t.Fatal("values manifest holds no cases")
	}
	for _, c := range manifest.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			schema := loadYAML[map[string]any](t, filepath.Join(dir, c.Schema))
			if findings := CheckSchema(schema); len(findings) != 0 {
				t.Fatalf("fixture schema is not itself subset-valid: %s", findingsText(findings))
			}
			values := loadYAML[map[string]any](t, filepath.Join(dir, c.Values))
			defaulted, findings, err := ValidateValues(schema, values)
			if err != nil {
				t.Fatalf("validate values: %v", err)
			}
			text := findingsText(findings)
			switch c.Verdict {
			case "accept":
				if len(findings) != 0 {
					t.Errorf("expected accept, got findings: %s", text)
				}
			case "reject":
				if len(findings) == 0 {
					t.Errorf("expected reject, got none")
				} else if c.Message != "" {
					if text != c.Message {
						t.Errorf("expected the exact finding %q, got: %s", c.Message, text)
					}
				} else if c.Contains != "" && !strings.Contains(text, c.Contains) {
					t.Errorf("expected findings to mention %q, got: %s", c.Contains, text)
				}
			default:
				t.Fatalf("unknown verdict %q", c.Verdict)
			}
			for k, want := range c.ExpectDefaults {
				got, ok := defaulted[k]
				if !ok {
					t.Errorf("expected defaulted value %q to be set", k)
					continue
				}
				if got != want {
					t.Errorf("expected defaulted value %q = %v, got %v", k, want, got)
				}
			}
		})
	}
}

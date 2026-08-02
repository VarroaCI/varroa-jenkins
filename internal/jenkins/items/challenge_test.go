package items

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "regenerate challenge goldens")

// challengeCase describes one case from manifest.yaml.
type challengeCase struct {
	Name   string `yaml:"name"`
	Tier   string `yaml:"tier"`
	Status string `yaml:"status"`
}

type challengeManifest struct {
	Cases []challengeCase `yaml:"cases"`
}

func TestChallenge(t *testing.T) {
	base := filepath.Join("testdata", "challenge")
	manifestData, err := os.ReadFile(filepath.Join(base, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var mf challengeManifest
	if err := yaml.Unmarshal(manifestData, &mf); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	for _, c := range mf.Cases {
		c := c
		t.Run(c.Tier+"/"+c.Name, func(t *testing.T) {
			caseDir := filepath.Join(base, "cases", c.Tier, c.Name)
			itemsData, err := os.ReadFile(filepath.Join(caseDir, "items.yaml"))
			if err != nil {
				t.Fatalf("read items.yaml: %v", err)
			}

			switch c.Status {
			case "supported":
				m, err := Parse(string(itemsData))
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				flat := m.Flatten()

				// Compute the expected golden set.
				expectedDir := filepath.Join(caseDir, "expected")
				flatPaths := make(map[string]bool)
				for _, ip := range flat {
					flatPaths[ip.Path] = true
				}

				if *update {
					// Write goldens and delete orphans.
					if err := os.MkdirAll(expectedDir, 0755); err != nil {
						t.Fatalf("mkdir expected: %v", err)
					}
					for _, ip := range flat {
						xml, err := GenerateConfigXML(ip.Item)
						if err != nil {
							t.Fatalf("GenerateConfigXML(%s): %v", ip.Path, err)
						}
						goldenPath := filepath.Join(expectedDir, pathToGolden(ip.Path))
						if err := os.WriteFile(goldenPath, []byte(xml), 0644); err != nil {
							t.Fatalf("write golden %s: %v", goldenPath, err)
						}
					}
					// Delete orphan goldens.
					entries, err := os.ReadDir(expectedDir)
					if err == nil {
						for _, e := range entries {
							if e.IsDir() {
								continue
							}
							if !strings.HasSuffix(e.Name(), ".config.xml") {
								continue
							}
							p := goldenToPath(e.Name())
							if !flatPaths[p] {
								if err := os.Remove(filepath.Join(expectedDir, e.Name())); err != nil {
									t.Errorf("remove orphan golden %s: %v", e.Name(), err)
								}
							}
						}
					}
				} else {
					// Assert golden set equals flattened set.
					entries, err := os.ReadDir(expectedDir)
					if os.IsNotExist(err) {
						t.Fatalf("expected/ directory missing for supported case %s", c.Name)
					}
					if err != nil {
						t.Fatalf("read expected/: %v", err)
					}
					goldenFiles := make(map[string]bool)
					for _, e := range entries {
						if e.IsDir() {
							continue
						}
						if !strings.HasSuffix(e.Name(), ".config.xml") {
							continue
						}
						p := goldenToPath(e.Name())
						goldenFiles[p] = true
					}
					for _, ip := range flat {
						if !goldenFiles[ip.Path] {
							t.Errorf("missing golden for flattened path %q", ip.Path)
						}
					}
					for gf := range goldenFiles {
						if !flatPaths[gf] {
							t.Errorf("orphan golden file for path %q (no longer flattened)", gf)
						}
					}

					// Compare each generated XML against its golden.
					for _, ip := range flat {
						xml, err := GenerateConfigXML(ip.Item)
						if err != nil {
							t.Errorf("GenerateConfigXML(%s): %v", ip.Path, err)
							continue
						}
						goldenPath := filepath.Join(expectedDir, pathToGolden(ip.Path))
						golden, err := os.ReadFile(goldenPath)
						if err != nil {
							t.Errorf("read golden %s: %v", goldenPath, err)
							continue
						}
						// Trailing-newline normalization.
						got := strings.TrimSuffix(xml, "\n")
						want := strings.TrimSuffix(string(golden), "\n")
						if got != want {
							t.Errorf("golden mismatch for %s", ip.Path)
						}
					}
				}

			case "known-gap":
				// Read contract files.
				errData, _ := os.ReadFile(filepath.Join(caseDir, "expect-error.txt"))
				absData, _ := os.ReadFile(filepath.Join(caseDir, "expect-absent.txt"))
				errSub := strings.TrimSpace(string(errData))
				absSub := strings.TrimSpace(string(absData))

				switch {
				case errSub != "" && absSub != "":
					t.Fatal("case has both expect-error.txt and expect-absent.txt")

				case errSub != "":
					// Error mode: try Validate() first, then GenerateConfigXML.
					m, parseErr := Parse(string(itemsData))
					if parseErr != nil {
						if !strings.Contains(parseErr.Error(), errSub) {
							t.Errorf("Parse/Validate error %q does not contain substring %q", parseErr.Error(), errSub)
						}
					} else if len(m.Items) > 0 {
						item := m.Items[0]
						_, genErr := GenerateConfigXML(item)
						if genErr == nil {
							t.Errorf("expected error containing %q but got nil", errSub)
						} else if !strings.Contains(genErr.Error(), errSub) {
							t.Errorf("GenerateConfigXML error %q does not contain substring %q", genErr.Error(), errSub)
						}
					} else {
						t.Errorf("expected error containing %q but Parse succeeded with no items", errSub)
					}

				case absSub != "":
					// Absence mode: generation must succeed and output must not contain substring.
					m, parseErr := Parse(string(itemsData))
					if parseErr != nil {
						t.Fatalf("Parse for absence-mode case: %v", parseErr)
					}
					flat := m.Flatten()
					if len(flat) == 0 {
						t.Fatal("no items flattened")
					}
					for _, ip := range flat {
						xml, err := GenerateConfigXML(ip.Item)
						if err != nil {
							t.Errorf("GenerateConfigXML(%s): %v", ip.Path, err)
							continue
						}
						if strings.Contains(xml, absSub) {
							t.Errorf("generated XML for %s contains prohibited substring %q", ip.Path, absSub)
						}
					}

				default:
					t.Fatal("known-gap case has neither expect-error.txt nor expect-absent.txt")
				}

			default:
				t.Fatalf("unknown status %q", c.Status)
			}
		})
	}
}

// pathToGolden converts a flattened path to a golden filename:
// "parent/child" → "parent__child.config.xml"
func pathToGolden(path string) string {
	return strings.ReplaceAll(path, "/", "__") + ".config.xml"
}

// goldenToPath reverses pathToGolden:
// "parent__child.config.xml" → "parent/child"
func goldenToPath(name string) string {
	s := strings.TrimSuffix(name, ".config.xml")
	return strings.ReplaceAll(s, "__", "/")
}

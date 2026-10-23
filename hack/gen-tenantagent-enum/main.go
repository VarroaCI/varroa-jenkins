// Command gen-tenantagent-enum writes the unattendedWrites enum of the TenantAgent
// CRD from the policy package. The enum marker is the single copy of the
// list in the CRD; policy.UnattendedEligible is the only source of truth.
//
// Run it through go generate in api/v1alpha1, which passes -file.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/agent/policy"
)

const (
	fieldLine  = "UnattendedWrites []string"
	markerName = "// +kubebuilder:validation:items:Enum="
	// searchDepth bounds the walk from the field up to its marker.
	searchDepth = 12
)

func main() {
	file := flag.String("file", "", "Go file that declares the UnattendedWrites field")
	flag.Parse()
	if *file == "" {
		fmt.Fprintln(os.Stderr, "gen-tenantagent-enum: -file is required")
		os.Exit(2)
	}
	if err := run(*file); err != nil {
		fmt.Fprintln(os.Stderr, "gen-tenantagent-enum:", err)
		os.Exit(1)
	}
}

// run rewrites the enum marker above the field and writes the file only when
// the content changes. It fails when the field or the marker is missing.
func run(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := rewrite(string(raw), strings.Join(policy.UnattendedEligible, ";"))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if out == string(raw) {
		return nil
	}
	return os.WriteFile(path, []byte(out), 0o644)
}

// rewrite replaces the value of the enum marker that sits directly above the
// UnattendedWrites field. It returns the new file text.
func rewrite(src, values string) (string, error) {
	lines := strings.Split(src, "\n")
	field := -1
	for i, l := range lines {
		if strings.Contains(l, fieldLine) {
			field = i
			break
		}
	}
	if field < 0 {
		return "", fmt.Errorf("field %q not found", fieldLine)
	}
	for i := field - 1; i >= 0 && i >= field-searchDepth; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, markerName) {
			indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
			lines[i] = indent + markerName + values
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", fmt.Errorf("enum marker above %q not found", fieldLine)
}

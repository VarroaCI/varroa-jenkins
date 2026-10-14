// Package lint runs policy rules over an items manifest. It is a seam
// separate from items.Parse: a non-cell controller must keep accepting
// every item shape it accepts today, so no rule runs there. The cell rule
// set runs in the operator's cell path and in the pipelines read.
package lint

import (
	"fmt"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// Finding is one policy violation found in a manifest.
type Finding struct {
	Rule    string // the rule id
	Path    string // the item path the finding is about
	Message string
}

// Rule checks one item against a policy and returns every finding.
type Rule interface {
	ID() string
	Check(item items.Item, path string) []Finding
}

// Run applies every rule to every item of the flattened manifest.
func Run(m *items.Manifest, rules []Rule) []Finding {
	flattened := m.Flatten()
	findings := make([]Finding, 0, len(flattened))
	for _, ip := range flattened {
		for _, r := range rules {
			findings = append(findings, r.Check(ip.Item, ip.Path)...)
		}
	}
	return findings
}

// ScriptRule is a Rule that also checks a Groovy pipeline body that no
// item holds, such as a pipeline template body.
type ScriptRule interface {
	Rule
	CheckScript(script, path string) []Finding
}

// RunScript applies each rule in rules that is a ScriptRule to script.
// A rule that is not a ScriptRule does not run. path names the script in
// each finding.
func RunScript(script, path string, rules []Rule) []Finding {
	var findings []Finding
	for _, r := range rules {
		sr, ok := r.(ScriptRule)
		if !ok {
			continue
		}
		findings = append(findings, sr.CheckScript(script, path)...)
	}
	return findings
}

// Summary renders findings as one line per finding, each naming its rule
// id, item path, and message, joined by "; ".
func Summary(findings []Finding) string {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, fmt.Sprintf("%s at %q: %s", f.Rule, f.Path, f.Message))
	}
	return strings.Join(lines, "; ")
}
